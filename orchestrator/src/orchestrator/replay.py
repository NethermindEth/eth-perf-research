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
from .manifest import Manifest
from .rpc import RpcClient

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
    """Return exit code per §C.2.

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

    try:
        first = next(iter(reader), None)
        if first is None:
            return EXIT_OK  # empty journal is trivially valid

        ctx = _context_from_manifest(manifest, deploy_private_key)
        if ctx is None:
            return EXIT_MANIFEST

        for record in reader:
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


def _replay_one_record(record: Record, ctx: FacadeContext, rpc: RpcClient) -> int:
    """Replay a single journal record against the live RPC.

    Returns ``EXIT_OK`` on success, or one of the specific exit codes on the first
    mismatch. H4: ``block_hash`` is verified regardless of ``status`` so a tampered
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
        # block hash, so any drift here breaks §C.1 replay-equivalence.
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
    # Pre-block_gas_limit manifests serialized 0; FacadeContext defaults its
    # ``block_gas_limit`` to ``gas_limit`` to match the live-run convention in
    # ``build_facade_context``. This keeps replay reproducing the same
    # gas-aware dispatch decisions for legacy journals too.
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
