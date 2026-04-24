"""Replay mode — reproduces a recorded run without controller/sensor.

Exit codes (design §C.2):
    0 — success
    1 — chain-hash mismatch
    2 — block-hash mismatch
    3 — state-root mismatch vs manifest
    4 — facade dispatch error (unknown verb, cursor drift, etc.)
"""
from __future__ import annotations

import json
from pathlib import Path

import httpx

from .facade import FacadeContext, UnknownVerb, dispatch
from .journal import ChainHashMismatch, JournalReader, JournalSchemaError
from .rpc import RpcClient


EXIT_OK = 0
EXIT_CHAIN_HASH = 1
EXIT_BLOCK_HASH = 2
EXIT_STATE_ROOT = 3
EXIT_FACADE = 4


class _BlockHashMismatch(Exception):
    pass


class _CursorDrift(Exception):
    pass


def replay(
    journal_path: Path | str,
    rpc_url: str,
    *,
    manifest_path: Path | str | None = None,
    rpc: RpcClient | None = None,
) -> int:
    """Return exit code per §C.2."""
    journal_path = Path(journal_path)

    reader = JournalReader(journal_path)
    try:
        reader.verify_chain()
    except ChainHashMismatch:
        return EXIT_CHAIN_HASH
    except JournalSchemaError:
        # Schema violation in input journal is a facade-level corruption from replay's POV.
        return EXIT_FACADE

    manifest_body: dict | None = None
    if manifest_path:
        manifest_body = json.loads(Path(manifest_path).read_text(encoding="utf-8"))

    own_rpc = rpc is None
    rpc = rpc or RpcClient(rpc_url)

    try:
        # Reconstruct the facade context from the first record's start_address.
        first = next(iter(reader), None)
        if first is None:
            return EXIT_OK  # empty journal is trivially valid

        first_start = int(first.replay_core.start_address, 16)
        ctx = FacadeContext(
            base_address=b"\x00" * 20,
            revision=0,
            address_cursor=first_start,
        )

        for record in reader:
            rc = record.replay_core
            ctx.address_cursor = int(rc.start_address, 16)
            try:
                txs = dispatch(rc.verb, rc.deadline_bytes, ctx)
            except (UnknownVerb, KeyError):
                return EXIT_FACADE

            end_after = int(rc.end_address, 16)
            if ctx.address_cursor != end_after:
                return EXIT_FACADE

            # H4: verify block_hash regardless of status. A tampered journal with
            # status="sensor_wait_timeout" must not bypass block identity.
            try:
                block_hash = rpc.testing_commit_block_v1([tx.rlp for tx in txs])
            except (httpx.HTTPError, ConnectionError, TimeoutError, RuntimeError):
                return EXIT_FACADE
            if block_hash != rc.block_hash:
                return EXIT_BLOCK_HASH

        if manifest_body and manifest_body.get("final_state_root") is not None:
            latest = rpc.eth_get_block_by_number("latest")
            if latest.get("stateRoot") != manifest_body["final_state_root"]:
                return EXIT_STATE_ROOT
    finally:
        if own_rpc:
            rpc.close()

    return EXIT_OK
