"""Run manifest — records environment, session log, and final state root."""

from __future__ import annotations

import dataclasses
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
    # ``block_gas_limit`` shifts the dispatcher's tx-count threshold for the
    # same ``(verb, deadline_bytes)`` pair (heavy-gas verbs cap earlier). Two
    # runs with identical ``target.yaml`` SHA but different ``block_gas_limit``
    # produce different tx sets and different block hashes, so it must be part
    # of the composition_hash preimage. Defaulted to 0 for legacy-manifest
    # back-compat (older runs round-trip into a sentinel that
    # ``replay_context_matches`` will refuse against any concrete value).
    block_gas_limit: int = 0


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
    # H7: checkpoint for incremental chain verification on resume. Successive
    # runs verify only the suffix past ``last_checkpoint_batch_id``.
    last_chain_hash_checkpoint: str = ""
    last_checkpoint_batch_id: int = -1
    final_state_root: str | None = None
    # Fields for Ed25519 manifest signing (design §13) will land when signing
    # lands — removed pre-feature to avoid shipping a "signature: null" slot
    # that could be mistaken for a valid unsigned manifest (review YAGNI /
    # security M-MANIFEST-SIG-THEATRE).
    schema: int = MANIFEST_SCHEMA

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def write(self, path: Path | str) -> None:
        Path(path).write_text(
            json.dumps(self.to_dict(), indent=2, sort_keys=True),
            encoding="utf-8",
        )

    @classmethod
    def read(cls, path: Path | str) -> Manifest:
        body = json.loads(Path(path).read_text(encoding="utf-8"))
        sessions = [Session(**s) for s in body.pop("sessions", [])]
        replay_raw = body.pop("replay_context", None)
        replay_ctx = ReplayContext(**replay_raw) if replay_raw else None
        # Filter to known fields so newer manifests with extra keys don't crash
        # older readers. Unknown keys are ignored; missing required keys will
        # raise from ``cls(...)`` as usual.
        known = {f.name for f in dataclasses.fields(cls)}
        filtered = {k: v for k, v in body.items() if k in known}
        return cls(sessions=sessions, replay_context=replay_ctx, **filtered)


def compute_composition_hash(
    target_sha256: str,
    env: EnvInfo,
    replay_context: ReplayContext | None = None,
) -> str:
    """sha256 matching the exact preimage defined in design §7.

    Preimage: ``target_yaml || genesis || plugin || nethermind || runtime || arch``,
    plus ``block_gas_limit`` from ``replay_context`` (when supplied) since the
    dispatcher's tx-count behaviour for ``(verb, deadline_bytes)`` depends on
    it. Two runs with the same ``target.yaml`` SHA but different
    ``block_gas_limit`` produce different tx sets and would silently bypass
    ``ResumeRefused`` without this term.

    The signer / chain identity stays in ``Manifest.replay_context`` and is
    compared structurally on resume (see ``replay_context_matches``). Pre-widen
    journals serialize ``replay_context=None`` here so existing
    composition_hash values still match and old runs continue to resume.
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
        parts.append(str(replay_context.block_gas_limit))
    return hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()


def replay_context_matches(a: ReplayContext | None, b: ReplayContext | None) -> bool:
    """Resume-time compatibility check for two run contexts.

    Returns ``True`` iff every field that affects tx/block hashes is identical.

    Tightened from round-2: the old "legacy-None is compatible with anything"
    shim was an unauthenticated bypass — a manifest with ``replay_context: null``
    could be dropped alongside a valid journal and silently skip the signer+chain
    gate (review C2 / skeptic F-1 / security M-REPLAY-NULL-SKIP). Now:

    - Both None → compatible (pre-ReplayContext journals replayed by old code).
    - Both set → require structural equality.
    - Exactly one set → incompatible; caller must raise ``ResumeRefused``.
    """
    if a is None and b is None:
        return True
    if a is None or b is None:
        return False
    return a == b


def compute_journal_sha256(journal_path: Path | str) -> str:
    """sha256 over the concatenation of every record's `replay_core` bytes.

    Matches design §7's "hash over all replay_core bytes" so replay can re-derive
    and match without re-reading observability fields.
    """
    from .journal import JournalReader, serialize_replay_core

    digest = hashlib.sha256()
    for record in JournalReader(journal_path):
        digest.update(serialize_replay_core(record.replay_core))
    return digest.hexdigest()
