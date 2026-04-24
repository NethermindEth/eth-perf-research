"""Run manifest — records environment, session log, and final state root."""
from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any


MANIFEST_SCHEMA = 1


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


@dataclass
class Manifest:
    run_id: str
    target_yaml_sha256: str
    base_address: str
    revision: int
    genesis_sha256: str
    composition_hash: str
    reference_f_version: str
    plugin_git_sha: str
    nethermind_commit_sha: str
    dotnet_runtime_major: int
    cpu_arch: str
    replay_context: ReplayContext | None = None
    sessions: list[Session] = field(default_factory=list)
    journal_sha256: str = ""
    final_state_root: str | None = None
    manifest_signature: str | None = None
    manifest_signer_pubkey: str | None = None
    schema: int = MANIFEST_SCHEMA

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def write(self, path: Path | str) -> None:
        Path(path).write_text(
            json.dumps(self.to_dict(), indent=2, sort_keys=True),
            encoding="utf-8",
        )

    @classmethod
    def read(cls, path: Path | str) -> "Manifest":
        body = json.loads(Path(path).read_text(encoding="utf-8"))
        sessions = [Session(**s) for s in body.pop("sessions", [])]
        replay_raw = body.pop("replay_context", None)
        replay_ctx = ReplayContext(**replay_raw) if replay_raw else None
        return cls(sessions=sessions, replay_context=replay_ctx, **body)


def compute_composition_hash(
    target_sha256: str,
    env: EnvInfo,
    replay_context: ReplayContext | None = None,
) -> str:
    """sha256 over every knob that could change final_state_root across runs.

    Order is fixed; joining with ``|`` avoids concatenation ambiguity. The replay
    context is included so two runs with identical target.yaml but different
    ``base_address`` / ``revision`` / ``chain_id`` / signer key produce different
    composition hashes — which is what we want for resume refusal and cross-run
    provenance.
    """
    parts = [
        target_sha256,
        env.genesis_sha256,
        env.plugin_git_sha,
        env.nethermind_commit_sha,
        str(env.dotnet_runtime_major),
        env.cpu_arch,
    ]
    if replay_context is not None:
        parts.extend(
            [
                replay_context.base_address,
                str(replay_context.revision),
                str(replay_context.chain_id),
                str(replay_context.gas_limit),
                str(replay_context.address_stride),
                replay_context.deploy_pubkey_sha256,
            ]
        )
    return hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()


def compute_journal_sha256(journal_path: Path | str) -> str:
    """sha256 over the concatenation of every record's `replay_core` bytes.

    Matches design §7's "hash over all replay_core bytes" so replay can re-derive
    and match without re-reading observability fields.
    """
    from .journal import serialize_replay_core, JournalReader

    digest = hashlib.sha256()
    for record in JournalReader(journal_path):
        digest.update(serialize_replay_core(record.replay_core))
    return digest.hexdigest()
