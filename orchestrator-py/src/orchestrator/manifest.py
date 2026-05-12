"""Run manifest — records environment, session log, and final state root."""

from __future__ import annotations

import contextlib
import dataclasses
import hashlib
import json
import os
import tempfile
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

MANIFEST_SCHEMA = 2


@dataclass
class EnvInfo:
    genesis_sha256: str
    plugin_git_sha: str
    nethermind_commit_sha: str
    dotnet_runtime_major: int
    cpu_arch: str


@dataclass
class Session:
    session_id: int
    started_at: str
    stopped_at: str | None = None
    last_batch_id: int | None = None
    stop_reason: str = "manual"


@dataclass
class ReplayContext:
    """Tx-signing identity persisted in the manifest so replay is reproducible.

    Without these fields in the manifest, ``orchestrator --replay`` cannot
    reconstruct the ``FacadeContext`` used for the live run — tx hashes, block
    hashes, and final state root all differ when address derivation or chain_id
    change. Design §C.1 requires identical ``final_state_root`` across replays.
    """

    base_address: str
    revision: int
    chain_id: int
    gas_limit: int
    address_stride: int
    deploy_pubkey_sha256: str
    # ``block_gas_limit`` shifts the dispatcher's tx-count threshold. Two runs
    # with identical chain identity but different ``block_gas_limit`` produce
    # different tx sets and different block hashes, so it is part of the
    # chain_identity_hash preimage.
    block_gas_limit: int = 0


@dataclass
class TargetSnapshot:
    """A single ``target.yaml`` snapshot recorded into the manifest.

    ``sha256`` is the canonical sha256 of the raw YAML bytes (matches
    ``TargetConfig.source_sha256``). ``ts_iso`` is the wall-clock time the
    snapshot was first observed by the run. ``body`` is the parsed YAML —
    sufficient for replay to re-construct the exact target shape, without
    preserving comments. The manifest grows by one snapshot every time the
    live watcher detects a sha256 change.
    """

    sha256: str
    ts_iso: str
    body: dict[str, Any]


@dataclass
class Manifest:
    run_id: str
    base_address: str
    revision: int
    genesis_sha256: str
    chain_identity_hash: str
    reference_f_version: str
    plugin_git_sha: str
    nethermind_commit_sha: str
    dotnet_runtime_major: int
    cpu_arch: str
    replay_context: ReplayContext | None = None
    sessions: list[Session] = field(default_factory=list)
    # Per-batch target reload (schema v2): every distinct ``target.yaml`` shape
    # observed during the run is recorded here. ``replay_core.target_sha256``
    # references one of these entries by sha256 so replay can rebuild the
    # exact target each batch was dispatched against.
    target_history: list[TargetSnapshot] = field(default_factory=list)
    journal_sha256: str = ""
    # Checkpoint for incremental chain verification on resume. Successive
    # runs verify only the suffix past ``last_checkpoint_batch_id``.
    last_chain_hash_checkpoint: str = ""
    last_checkpoint_batch_id: int = -1
    final_state_root: str | None = None
    schema: int = MANIFEST_SCHEMA

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def write(self, path: Path | str) -> None:
        """Atomically replace the manifest file (write tmp + os.replace + fsync).

        Live target reload mutates ``target_history`` between commits; if a crash
        landed mid-write the next run could read a half-flushed manifest and
        crash before resume even starts. Writing through a temp file and
        ``os.replace`` keeps the on-disk manifest at either the prior version
        or the new version, never partial.
        """
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        body = json.dumps(self.to_dict(), indent=2, sort_keys=True)
        # Use mkstemp so the temp file lives in the same directory (same FS) as
        # the destination — required for atomic os.replace across all platforms.
        fd, tmp_name = tempfile.mkstemp(
            dir=str(path.parent), prefix=path.name + ".", suffix=".tmp"
        )
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                f.write(body)
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp_name, path)  # noqa: PTH105
        except Exception:
            with contextlib.suppress(FileNotFoundError):
                Path(tmp_name).unlink()
            raise

    @classmethod
    def read(cls, path: Path | str) -> Manifest:
        body = json.loads(Path(path).read_text(encoding="utf-8"))
        manifest_schema = body.get("schema")
        if manifest_schema is not None and int(manifest_schema) < MANIFEST_SCHEMA:
            raise ValueError(
                f"manifest schema={manifest_schema} predates per-batch target "
                f"reload (current schema={MANIFEST_SCHEMA}). Archive state/ to "
                f"start fresh, or downgrade orchestrator."
            )
        sessions = [Session(**s) for s in body.pop("sessions", [])]
        replay_raw = body.pop("replay_context", None)
        replay_ctx = ReplayContext(**replay_raw) if replay_raw else None
        target_history_raw = body.pop("target_history", [])
        target_history = [TargetSnapshot(**snap) for snap in target_history_raw]
        # Filter to known fields so a future manifest with extra keys still
        # loads. Unknown keys are ignored; missing required keys raise from
        # ``cls(...)`` as usual.
        known = {f.name for f in dataclasses.fields(cls)}
        filtered = {k: v for k, v in body.items() if k in known}
        return cls(
            sessions=sessions,
            replay_context=replay_ctx,
            target_history=target_history,
            **filtered,
        )


def compute_chain_identity_hash(
    env: EnvInfo,
    replay_context: ReplayContext | None = None,
) -> str:
    """sha256 over the run's chain identity. Resume gate.

    Preimage: ``genesis || chain_id || deploy_pubkey || plugin || nethermind ||
    runtime || arch || schema_version``, plus ``block_gas_limit`` when the
    ``replay_context`` is supplied (the dispatcher's tx-count behaviour for
    ``(verb, deadline_bytes)`` depends on it).

    Crucially, this hash does NOT include ``target.yaml`` — the target shape is
    a per-batch input that can change mid-run (see ``manifest.target_history``).
    Two runs with the same chain identity but different target.yaml files MUST
    produce the same ``chain_identity_hash`` so the second one can resume the
    first.
    """
    parts: list[str] = [
        env.genesis_sha256,
        env.plugin_git_sha,
        env.nethermind_commit_sha,
        str(env.dotnet_runtime_major),
        env.cpu_arch,
        f"schema={MANIFEST_SCHEMA}",
    ]
    if replay_context is not None:
        parts.extend(
            [
                str(replay_context.chain_id),
                replay_context.deploy_pubkey_sha256,
                str(replay_context.block_gas_limit),
            ]
        )
    return hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()


def replay_context_matches(a: ReplayContext | None, b: ReplayContext | None) -> bool:
    """Resume-time compatibility check for two run contexts.

    Returns ``True`` iff every field that affects tx/block hashes is identical.

    - Both None → compatible.
    - Both set → require structural equality.
    - Exactly one set → incompatible; caller must raise ``ResumeRefused``.
    """
    if a is None and b is None:
        return True
    if a is None or b is None:
        return False
    return a == b


def compute_journal_sha256(journal_path: Path | str) -> str:
    """sha256 over the concatenation of every record's `replay_core` bytes."""
    from .journal import JournalReader, serialize_replay_core

    digest = hashlib.sha256()
    for record in JournalReader(journal_path):
        digest.update(serialize_replay_core(record.replay_core))
    return digest.hexdigest()
