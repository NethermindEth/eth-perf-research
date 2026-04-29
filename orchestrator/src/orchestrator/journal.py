"""Crash-resilient JSONL journal with chain-hashed `replay_core`.

Each batch appends one `Record` to `orchestrator.journal.jsonl`. The `replay_core` sub-object
is serialized deterministically and sha256-chained (`chain_hash = sha256(prev || rc_bytes)`).
`observability` is excluded from the chain hash by design — it is diagnostic data that may
differ between identical replay-valid runs (timestamps, innovation coefficients, etc.).

`os.fsync` is called on every append: lost-record recovery relies on fsync durability.
"""

from __future__ import annotations

import contextlib
import functools
import hashlib
import json
import os
from collections.abc import Iterator
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import jsonschema

SCHEMA_VERSION = 2
_CHAIN_HASH_GENESIS = "0" * 64


class ChainHashMismatch(Exception):
    """Raised when `verify_chain` detects a tampered record."""


class JournalSchemaError(ValueError):
    """Raised when a journal line fails schema validation.

    Carries the offending batch_id (if present) so operators can locate the row.
    """

    def __init__(self, message: str, batch_id: int | None = None) -> None:
        super().__init__(message)
        self.batch_id = batch_id


@dataclass
class ReplayCore:
    verb: str
    deadline_bytes: int
    start_address: str  # hex string "0x…"
    end_address: str
    status: (
        str  # ok | aborted | controller_instability | sensor_wait_timeout | reconciled_unobserved
    )
    block_hash: str
    block_number: int
    # Unix-epoch seconds passed to ``testing_commitBlockV1`` as
    # ``payloadAttributes.timestamp``. The EL folds this into the committed
    # block's header hash, so replay must re-supply the same value to satisfy
    # §C.1 replay-equivalence. Chain-hash-preimage critical.
    block_timestamp: int = 0
    # sha256 of the ``target.yaml`` bytes that produced this batch. Per-batch
    # so live target reload can flip the target shape between consecutive
    # records without breaking the chain. Replay looks the value up in
    # ``manifest.target_history`` to reconstruct the exact target. Required by
    # schema v2 — chain-hash-preimage critical so two runs with different
    # targets at the same batch_id produce different chain_hashes.
    target_sha256: str = ""
    chain_hash: str = ""  # set by the writer


@dataclass
class Observability:
    observed_flat_bytes: int = 0
    coeffs_before: dict[str, dict[str, float]] = field(default_factory=dict)
    coeffs_after: dict[str, dict[str, float]] = field(default_factory=dict)
    # Per-(verb, axis) α — authoritative for resume state reconstruction.
    # ``alpha_current`` is a display-only scalar mean of this field, kept for
    # back-compat with plotters / dashboards that pre-date per-verb α. On write,
    # both are populated consistently; on resume, ``alpha_state`` wins.
    alpha_state: dict[str, dict[str, float]] = field(default_factory=dict)
    sigma_innov: dict[str, dict[str, float]] = field(default_factory=dict)
    alpha_current: float = 0.0  # display-only: mean of alpha_state.values()
    innovation_ratio: float = 0.0
    residual_norm: float = 0.0
    # Null (not absent) on sensor_wait_timeout — preserves chain-hash determinism.
    statecomp_snapshot: dict[str, Any] | None = None
    # Diagnostic-only knobs that produced this batch's pick. ``mix_simplex``
    # is the full simplex projection; ``epsilon`` is the explore probability
    # in effect; ``overshoot_penalty`` is the QP overshoot λ. All written into
    # Observability (not ReplayCore) so the chain-hash stays deterministic
    # regardless of these knob values. ``None`` on records produced before
    # the field existed (back-compat for older journals).
    mix_simplex: dict[str, float] | None = None
    epsilon: float | None = None
    overshoot_penalty: float | None = None


@dataclass
class Record:
    session_id: int
    resumed_from_batch: int | None
    ts_iso: str
    batch_id: int
    replay_core: ReplayCore
    observability: Observability
    schema: int = SCHEMA_VERSION


def _replay_core_to_jsonable(rc: ReplayCore) -> dict[str, Any]:
    """Flat dataclass-to-dict without recursion. ``ReplayCore`` has only scalars."""
    return {
        "verb": rc.verb,
        "deadline_bytes": rc.deadline_bytes,
        "start_address": rc.start_address,
        "end_address": rc.end_address,
        "status": rc.status,
        "block_hash": rc.block_hash,
        "block_number": rc.block_number,
        "block_timestamp": rc.block_timestamp,
        "target_sha256": rc.target_sha256,
        "chain_hash": rc.chain_hash,
    }


def _observability_to_jsonable(obs: Observability) -> dict[str, Any]:
    """Hand-rolled serializer — shares references to ``statecomp_snapshot``.

    ``asdict`` would deep-copy the snapshot dict (hundreds of keys on a real
    sensor response); since the record is immediately JSON-encoded and the
    snapshot is never mutated after capture, reference-sharing is safe and
    10-30× faster per batch (TRIZ Prior Action / review P3).
    """
    return {
        "observed_flat_bytes": obs.observed_flat_bytes,
        "coeffs_before": obs.coeffs_before,
        "coeffs_after": obs.coeffs_after,
        "alpha_state": obs.alpha_state,
        "sigma_innov": obs.sigma_innov,
        "alpha_current": obs.alpha_current,
        "innovation_ratio": obs.innovation_ratio,
        "residual_norm": obs.residual_norm,
        "statecomp_snapshot": obs.statecomp_snapshot,  # None preserved as JSON null
        "mix_simplex": obs.mix_simplex,
        "epsilon": obs.epsilon,
        "overshoot_penalty": obs.overshoot_penalty,
    }


def serialize_replay_core(rc: ReplayCore) -> bytes:
    """Deterministic JSON serialization used as the chain-hash preimage.

    ``chain_hash`` is excluded from the preimage (it is the output of this step).
    Uses the hand-rolled ``_replay_core_to_jsonable`` instead of ``asdict`` for
    consistency with the write path and to avoid the recursive copy.
    """
    body = _replay_core_to_jsonable(rc)
    body.pop("chain_hash", None)
    return json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")


def _record_to_dict(record: Record) -> dict[str, Any]:
    """Serialize a Record without recursive asdict copies.

    On the per-batch hot path this is the difference between linear scaling and
    constant-time scaling wrt ``statecomp_snapshot`` size. Chain-hash determinism
    is preserved because ``serialize_replay_core`` uses the same primitive and
    ``sort_keys=True``.
    """
    return {
        "schema": record.schema,
        "session_id": record.session_id,
        "resumed_from_batch": record.resumed_from_batch,
        "ts_iso": record.ts_iso,
        "batch_id": record.batch_id,
        "replay_core": _replay_core_to_jsonable(record.replay_core),
        "observability": _observability_to_jsonable(record.observability),
    }


class JournalWriter:
    """Append-only JSONL writer with fsync-per-record and chain-hash tracking."""

    def __init__(self, path: Path | str) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._prev_chain_hash = self._load_last_chain_hash()
        flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
        self._fd = os.open(self.path, flags, 0o644)

    @property
    def prev_chain_hash(self) -> str:
        return self._prev_chain_hash

    def append(self, record: Record) -> str:
        """Compute chain hash, write the record, fsync. Returns the new chain hash."""
        rc_bytes = serialize_replay_core(record.replay_core)
        new_hash = hashlib.sha256(self._prev_chain_hash.encode("ascii") + rc_bytes).hexdigest()
        # Mutate the record in-place so downstream code (manifest writer) sees the hash.
        record.replay_core.chain_hash = new_hash
        line = json.dumps(_record_to_dict(record), separators=(",", ":")) + "\n"
        os.write(self._fd, line.encode("utf-8"))
        os.fsync(self._fd)
        self._prev_chain_hash = new_hash
        return new_hash

    def close(self) -> None:
        os.close(self._fd)

    def __enter__(self) -> JournalWriter:
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()

    def _load_last_chain_hash(self) -> str:
        if not self.path.exists() or self.path.stat().st_size == 0:
            return _CHAIN_HASH_GENESIS
        return _read_last_chain_hash(self.path)


def _read_last_line_bytes(path: Path) -> bytes | None:
    """Return the last newline-delimited line of ``path`` in constant time.

    Reverse-seeks in 4 KiB chunks, finds the last internal ``\\n``, returns the
    bytes after it. Returns None for empty files. Tolerates a missing trailing
    newline — the final line is returned whether or not it ends in ``\\n``.
    """
    size = path.stat().st_size
    if size == 0:
        return None
    chunk_size = 4096
    buffer = bytearray()
    with path.open("rb") as f:
        pos = size
        while pos > 0:
            step = min(chunk_size, pos)
            pos -= step
            f.seek(pos)
            chunk = f.read(step)
            buffer[:0] = chunk
            stripped = bytes(buffer).rstrip(b"\n")
            nl = stripped.rfind(b"\n")
            if nl != -1:
                return stripped[nl + 1 :]
    stripped = bytes(buffer).strip()
    return stripped if stripped else None


def _read_last_chain_hash(path: Path) -> str:
    """Reverse-seek the last record and extract its chain_hash.

    Constant time regardless of journal length. A partially-written last line
    (incomplete JSON) is treated as corruption and raises ``JournalSchemaError``
    so the resume path refuses rather than silently returning a stale prior hash.
    """
    last = _read_last_line_bytes(path)
    if last is None:
        return _CHAIN_HASH_GENESIS
    try:
        return json.loads(last.decode("utf-8"))["replay_core"]["chain_hash"]
    except (json.JSONDecodeError, UnicodeDecodeError, KeyError) as exc:
        raise JournalSchemaError(
            f"trailing partial record in {path}; archive state/ and restart"
        ) from exc


class JournalReader:
    """Iterate JSONL records; re-verify chain-hash end-to-end."""

    def __init__(self, path: Path | str) -> None:
        self.path = Path(path)

    def __iter__(self) -> Iterator[Record]:
        validator = _journal_validator()
        with self.path.open("r", encoding="utf-8") as f:
            for line_no, raw in enumerate(f, start=1):
                raw = raw.strip()
                if not raw:
                    continue
                try:
                    body = json.loads(raw)
                except json.JSONDecodeError as exc:
                    raise JournalSchemaError(f"line {line_no}: malformed JSON ({exc.msg})") from exc
                # Materialize only the first error — ``sorted(iter_errors(...))``
                # walks the full schema twice per record. On a 50k-record journal
                # this is 2-3× the overall validation cost.
                first_err = next(iter(validator.iter_errors(body)), None)
                if first_err is not None:
                    raise JournalSchemaError(
                        f"line {line_no}: schema violation at {list(first_err.path)}: "
                        f"{first_err.message}",
                        batch_id=body.get("batch_id") if isinstance(body, dict) else None,
                    )
                yield _dict_to_record(body)

    def tail(self) -> Record | None:
        """Return the last record in the journal, O(1) via reverse-seek.

        Validates the last line against the schema — a partial or tampered last
        record raises ``JournalSchemaError`` just like ``__iter__`` would. Does
        NOT re-verify the chain; callers must call ``verify_chain*`` separately.
        """
        if not self.path.exists():
            return None
        last = _read_last_line_bytes(self.path)
        if last is None:
            return None
        try:
            body = json.loads(last.decode("utf-8"))
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise JournalSchemaError(
                f"trailing partial record in {self.path}; archive state/ and restart"
            ) from exc
        validate_record_dict(body)
        return _dict_to_record(body)

    def verify_chain(self) -> None:
        self.verify_chain_from(_CHAIN_HASH_GENESIS, min_batch_id=0)

    def verify_chain_from(self, prev_hash: str, min_batch_id: int) -> None:
        """Verify only the suffix of the chain past ``min_batch_id``.

        Callers that previously checkpointed a trusted (prev_hash, batch_id) pair
        (e.g. manifest.last_chain_hash_checkpoint) pass them here so resume cost
        stays O(tail_size) instead of O(journal_size) — review H7.
        """
        prev = prev_hash
        for record in self:
            if record.batch_id < min_batch_id:
                continue
            rc_bytes = serialize_replay_core(record.replay_core)
            expected = hashlib.sha256(prev.encode("ascii") + rc_bytes).hexdigest()
            if expected != record.replay_core.chain_hash:
                raise ChainHashMismatch(
                    f"batch {record.batch_id}: chain_hash mismatch "
                    f"(expected {expected}, stored {record.replay_core.chain_hash})"
                )
            prev = record.replay_core.chain_hash


def _dict_to_record(d: dict[str, Any]) -> Record:
    record_schema = d.get("schema", SCHEMA_VERSION)
    if record_schema != SCHEMA_VERSION:
        raise JournalSchemaError(
            f"schema={record_schema} found; this build requires schema={SCHEMA_VERSION} "
            f"(target_sha256 is now per-batch). Archive state/ to start fresh.",
            batch_id=d.get("batch_id") if isinstance(d, dict) else None,
        )
    rc = ReplayCore(**d["replay_core"])
    obs = Observability(**d["observability"])
    return Record(
        schema=record_schema,
        session_id=d["session_id"],
        resumed_from_batch=d.get("resumed_from_batch"),
        ts_iso=d["ts_iso"],
        batch_id=d["batch_id"],
        replay_core=rc,
        observability=obs,
    )


PENDING_FILENAME = "orchestrator.journal.pending"


@dataclass
class PendingBatch:
    """Intent record written BEFORE `testing_commit_block_v1`.

    If the orchestrator crashes between commit and journal append, the resume path
    sees this sidecar and reconciles with Nethermind's head to synthesize the
    missing record — closing the atomicity gap flagged by review C3.

    ``chain_identity_hash`` ties the sidecar to the exact chain identity that
    wrote it. An operator who changes the chain (genesis, signer, runtime)
    between ``compose down`` and ``compose up`` would get a mismatched hash and
    the reconcile refuses rather than replaying under the wrong code path.

    ``target_sha256`` records the target.yaml shape that was active when the
    sidecar was written; a mid-run target swap doesn't gate reconcile (target
    is per-batch now), but the value is journaled into the synthesized record
    so replay can look it up in ``manifest.target_history``.
    """

    session_id: int
    resumed_from_batch: int | None
    batch_id: int
    verb: str
    deadline_bytes: int
    start_address: str
    end_address: str
    ts_iso: str
    pre_block_number: int
    chain_identity_hash: str = ""
    target_sha256: str = ""


def write_pending(state_dir: Path | str, pending: PendingBatch) -> None:
    """Atomically write the pending-batch sidecar with fsync.

    Uses ``O_NOFOLLOW | O_EXCL`` on the tmp file so a pre-planted symlink cannot
    redirect the write (security M-PENDING-SYMLINK). The tmp is unlinked and
    re-created each call to keep O_EXCL meaningful.
    """
    path = Path(state_dir) / PENDING_FILENAME
    tmp = path.with_suffix(".tmp")
    with contextlib.suppress(FileNotFoundError):
        tmp.unlink()
    data = (json.dumps(asdict(pending), separators=(",", ":")) + "\n").encode("utf-8")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(tmp, flags, 0o600)
    try:
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)
    # `os.replace` keeps the atomic-rename semantics we want; `Path.replace` wraps
    # it and loses nothing but the linter would flag it (PTH105).
    os.replace(tmp, path)  # noqa: PTH105
    dir_fd = os.open(str(Path(state_dir)), os.O_RDONLY)
    try:
        os.fsync(dir_fd)
    finally:
        os.close(dir_fd)


def read_pending(state_dir: Path | str) -> PendingBatch | None:
    path = Path(state_dir) / PENDING_FILENAME
    if not path.exists() or path.stat().st_size == 0:
        return None
    body = json.loads(path.read_text(encoding="utf-8"))
    return PendingBatch(**body)


def clear_pending(state_dir: Path | str) -> None:
    path = Path(state_dir) / PENDING_FILENAME
    if path.exists():
        path.unlink()


@functools.cache
def load_schema() -> dict[str, Any]:
    """Return the JSON Schema for a journal record (cached read).

    Schema bumped to v2 in the live-target-reload work: ``replay_core`` now
    carries ``target_sha256`` (per-batch reference into ``manifest.target_history``).
    Pre-v2 records are rejected by ``_dict_to_record`` with an explicit message.
    """
    schema_path = Path(__file__).parent / "schemas" / "journal_v2.json"
    return json.loads(schema_path.read_text(encoding="utf-8"))


@functools.cache
def _journal_validator() -> jsonschema.Draft202012Validator:
    """Compiled validator — reused across all journal reads."""
    return jsonschema.Draft202012Validator(load_schema())


def validate_record_dict(body: dict[str, Any]) -> None:
    """Public entry — validate a single journal record dict or raise ``JournalSchemaError``."""
    validator = _journal_validator()
    first = next(iter(validator.iter_errors(body)), None)
    if first is None:
        return
    raise JournalSchemaError(
        f"schema violation at {list(first.path)}: {first.message}",
        batch_id=body.get("batch_id") if isinstance(body, dict) else None,
    )
