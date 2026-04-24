"""Crash-resilient JSONL journal with chain-hashed `replay_core`.

Each batch appends one `Record` to `orchestrator.journal.jsonl`. The `replay_core` sub-object
is serialized deterministically and sha256-chained (`chain_hash = sha256(prev || rc_bytes)`).
`observability` is excluded from the chain hash by design — it is diagnostic data that may
differ between identical replay-valid runs (timestamps, innovation coefficients, etc.).

`os.fsync` is called on every append: lost-record recovery relies on fsync durability.
"""
from __future__ import annotations

import dataclasses
import hashlib
import json
import os
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any, Iterator


SCHEMA_VERSION = 1
_CHAIN_HASH_GENESIS = "0" * 64


class ChainHashMismatch(Exception):
    """Raised when `verify_chain` detects a tampered record."""


@dataclass
class ReplayCore:
    verb: str
    deadline_bytes: int
    start_address: str  # hex string "0x…"
    end_address: str
    status: str  # ok | aborted | controller_instability | sensor_wait_timeout
    block_hash: str
    block_number: int
    chain_hash: str = ""  # set by the writer


@dataclass
class Observability:
    observed_flat_bytes: int = 0
    coeffs_before: dict[str, dict[str, float]] = field(default_factory=dict)
    coeffs_after: dict[str, dict[str, float]] = field(default_factory=dict)
    sigma_innov: dict[str, dict[str, float]] = field(default_factory=dict)
    alpha_current: float = 0.0
    innovation_ratio: float = 0.0
    residual_norm: float = 0.0
    # Null (not absent) on sensor_wait_timeout — preserves chain-hash determinism.
    statecomp_snapshot: dict[str, Any] | None = None


@dataclass
class Record:
    session_id: int
    resumed_from_batch: int | None
    ts_iso: str
    batch_id: int
    replay_core: ReplayCore
    observability: Observability
    schema: int = SCHEMA_VERSION


def serialize_replay_core(rc: ReplayCore) -> bytes:
    """Deterministic JSON serialization used as the chain-hash preimage.

    `chain_hash` is excluded from the preimage (it is the output of this hashing step).
    """
    body = asdict(rc)
    body.pop("chain_hash", None)
    return json.dumps(body, sort_keys=True, separators=(",", ":")).encode("utf-8")


def _record_to_dict(record: Record) -> dict[str, Any]:
    d = asdict(record)
    # Preserve null (not absent) for statecomp_snapshot.
    if record.observability.statecomp_snapshot is None:
        d["observability"]["statecomp_snapshot"] = None
    return d


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
        new_hash = hashlib.sha256(
            self._prev_chain_hash.encode("ascii") + rc_bytes
        ).hexdigest()
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
        last = None
        with self.path.open("r", encoding="utf-8") as f:
            for raw in f:
                raw = raw.strip()
                if raw:
                    last = raw
        if last is None:
            return _CHAIN_HASH_GENESIS
        return json.loads(last)["replay_core"]["chain_hash"]


class JournalReader:
    """Iterate JSONL records; re-verify chain-hash end-to-end."""

    def __init__(self, path: Path | str) -> None:
        self.path = Path(path)

    def __iter__(self) -> Iterator[Record]:
        with self.path.open("r", encoding="utf-8") as f:
            for raw in f:
                raw = raw.strip()
                if not raw:
                    continue
                yield _dict_to_record(json.loads(raw))

    def tail(self) -> Record | None:
        last: Record | None = None
        for record in self:
            last = record
        return last

    def verify_chain(self) -> None:
        prev = _CHAIN_HASH_GENESIS
        for record in self:
            rc_bytes = serialize_replay_core(record.replay_core)
            expected = hashlib.sha256(prev.encode("ascii") + rc_bytes).hexdigest()
            if expected != record.replay_core.chain_hash:
                raise ChainHashMismatch(
                    f"batch {record.batch_id}: chain_hash mismatch "
                    f"(expected {expected}, stored {record.replay_core.chain_hash})"
                )
            prev = record.replay_core.chain_hash


def _dict_to_record(d: dict[str, Any]) -> Record:
    rc = ReplayCore(**d["replay_core"])
    obs = Observability(**d["observability"])
    return Record(
        schema=d.get("schema", SCHEMA_VERSION),
        session_id=d["session_id"],
        resumed_from_batch=d.get("resumed_from_batch"),
        ts_iso=d["ts_iso"],
        batch_id=d["batch_id"],
        replay_core=rc,
        observability=obs,
    )


def load_schema() -> dict[str, Any]:
    """Return the JSON Schema for a journal record (cached read)."""
    schema_path = Path(__file__).parent / "schemas" / "journal_v1.json"
    return json.loads(schema_path.read_text(encoding="utf-8"))
