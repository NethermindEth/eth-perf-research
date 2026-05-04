"""Replay mode — reproduces a recorded run without controller/sensor.

Exit codes (design §C.2):
    0 — success
    1 — chain-hash mismatch
    2 — block-hash mismatch
    3 — state-root mismatch vs manifest
    4 — facade dispatch error (unknown verb, cursor drift, invalid manifest, …)
    5 — missing / invalid manifest (required for context reconstruction)
"""

from __future__ import annotations

from pathlib import Path

import httpx

from .facade import FacadeContext, UnknownVerb, dispatch
from .journal import ChainHashMismatch, JournalReader, JournalSchemaError, Record
from .manifest import Manifest, TargetSnapshot
from .rpc import RpcClient


class ReplayError(RuntimeError):
    """Raised when the manifest's ``target_history`` cannot reconstruct a record."""

EXIT_OK = 0
EXIT_CHAIN_HASH = 1
EXIT_BLOCK_HASH = 2
EXIT_STATE_ROOT = 3
EXIT_FACADE = 4
EXIT_MANIFEST = 5


def replay(
    journal_path: Path | str,
    rpc_url: str,
    *,
    manifest_path: Path | str,
    rpc: RpcClient | None = None,
    deploy_private_key: bytes | None = None,
) -> int:
    """Return exit code per the module docstring legend.

    The manifest is **required**: it holds the ``ReplayContext`` (base_address,
    revision, chain_id, gas_limit, signer fingerprint) needed to reconstruct the
    ``FacadeContext`` used by the live run. Without it we cannot reproduce the
    same tx/block hashes. ``deploy_private_key`` defaults to the lab key; replay
    verifies its fingerprint against the manifest and refuses on mismatch.
    """
    journal_path = Path(journal_path)

    try:
        manifest = Manifest.read(manifest_path)
    except (FileNotFoundError, ValueError, TypeError):
        return EXIT_MANIFEST
    if manifest.replay_context is None:
        return EXIT_MANIFEST

    reader = JournalReader(journal_path)
    try:
        reader.verify_chain()
    except ChainHashMismatch:
        return EXIT_CHAIN_HASH
    except JournalSchemaError:
        return EXIT_FACADE

    own_rpc = rpc is None
    rpc = rpc or RpcClient(rpc_url)

    # Per-batch target lookup: each journaled record's ``target_sha256``
    # must resolve into ``manifest.target_history``. Build the index once.
    target_index = _index_target_history(manifest.target_history)

    try:
        first = next(iter(reader), None)
        if first is None:
            return EXIT_OK  # empty journal is trivially valid

        ctx = _context_from_manifest(manifest, deploy_private_key)
        if ctx is None:
            return EXIT_MANIFEST

        for record in reader:
            try:
                _resolve_target_for_record(record, target_index)
            except ReplayError:
                # Manifest's target_history doesn't contain the sha referenced
                # by this batch — surface as a facade error so operators see
                # exit code 4 (corrupt or pruned manifest).
                return EXIT_FACADE
            exit_code = _replay_one_record(record, ctx, rpc)
            if exit_code != EXIT_OK:
                return exit_code

        if manifest.final_state_root is not None:
            latest = rpc.eth_get_block_by_number("latest")
            if latest.get("stateRoot") != manifest.final_state_root:
                return EXIT_STATE_ROOT
    finally:
        if own_rpc:
            rpc.close()

    return EXIT_OK


def _index_target_history(
    target_history: list[TargetSnapshot],
) -> dict[str, TargetSnapshot]:
    """Build sha256 → snapshot index. Empty manifests yield an empty index."""
    return {snap.sha256: snap for snap in target_history}


def _resolve_target_for_record(
    record: Record, target_index: dict[str, TargetSnapshot]
) -> TargetSnapshot | None:
    """Look up the target snapshot referenced by ``record.replay_core.target_sha256``.

    Returns the snapshot, or ``None`` when ``target_sha256`` is empty
    (treated as "use whatever was in the manifest"). Raises
    ``ReplayError`` when the sha is non-empty but missing from the index —
    that signals a corrupt or truncated manifest and the operator must
    archive state/.
    """
    sha = record.replay_core.target_sha256
    if not sha:
        return None
    snap = target_index.get(sha)
    if snap is None:
        raise ReplayError(
            f"target_sha256 {sha[:8]}… referenced by batch {record.batch_id} "
            f"not in manifest.target_history; corrupt manifest"
        )
    return snap


def _replay_one_record(record: Record, ctx: FacadeContext, rpc: RpcClient) -> int:
    """Replay a single journal record against the live RPC.

    Returns ``EXIT_OK`` on success, or one of the specific exit codes on the first
    mismatch. ``block_hash`` is verified regardless of ``status`` so a tampered
    journal with ``status="sensor_wait_timeout"`` cannot bypass block identity.
    """
    rc = record.replay_core
    ctx.address_cursor = int(rc.start_address, 16)
    try:
        txs = dispatch(rc.verb, rc.deadline_bytes, ctx)
    except (UnknownVerb, KeyError):
        return EXIT_FACADE

    if ctx.address_cursor != int(rc.end_address, 16):
        return EXIT_FACADE

    try:
        # Re-supply the journaled block timestamp; the EL folds it into the
        # block hash, so any drift here breaks replay-equivalence.
        block_hash = rpc.testing_commit_block_v1(
            [tx.rlp for tx in txs], timestamp_unix=rc.block_timestamp
        )
    except (httpx.HTTPError, ConnectionError, TimeoutError, RuntimeError):
        return EXIT_FACADE
    if block_hash != rc.block_hash:
        return EXIT_BLOCK_HASH
    return EXIT_OK


def _context_from_manifest(
    manifest: Manifest, deploy_private_key: bytes | None
) -> FacadeContext | None:
    """Reconstruct the exact FacadeContext the live run used, or None on mismatch."""
    replay_ctx = manifest.replay_context
    if replay_ctx is None:
        return None
    try:
        base_addr = bytes.fromhex(replay_ctx.base_address.removeprefix("0x")).rjust(20, b"\x00")
    except ValueError:
        return None
    key = deploy_private_key if deploy_private_key is not None else b"\x11" * 32
    # Manifests that recorded ``block_gas_limit=0`` fall back to ``gas_limit``
    # to match the live-run convention in ``build_facade_context``.
    block_gas_limit = (
        replay_ctx.block_gas_limit
        if replay_ctx.block_gas_limit > 0
        else replay_ctx.gas_limit
    )
    ctx = FacadeContext(
        base_address=base_addr,
        revision=replay_ctx.revision,
        chain_id=replay_ctx.chain_id,
        gas_limit=replay_ctx.gas_limit,
        block_gas_limit=block_gas_limit,
        deploy_private_key=key,
        address_stride=replay_ctx.address_stride,
    )
    if ctx.deploy_pubkey_sha256() != replay_ctx.deploy_pubkey_sha256:
        return None
    return ctx
