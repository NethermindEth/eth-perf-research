#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.12"
# dependencies = [
#   "httpx>=0.27",
#   "pyjwt>=2.8",
#   "rlp>=4.0",
# ]
# ///
"""Replay state/payloads.rlp onto a running geth Engine API endpoint.

Reads the orchestrator's append-only ExecutionPayloadV3 stream and feeds each
record to geth via `engine_newPayloadV3` + `engine_forkchoiceUpdatedV3`. Stops
on the first non-VALID response. Reports per-block status and prints final
state-root for cross-client comparison with Nethermind.

Usage::

    scripts/replay_on_geth.py \\
        --payloads state/payloads.rlp \\
        --jwt state-geth/jwt.hex \\
        --auth-url http://127.0.0.1:8651 \\
        --public-url http://127.0.0.1:8645
"""

from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

import httpx
import jwt as pyjwt
import rlp

# Make the orchestrator package importable when invoked from repo root.
_REPO_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_REPO_ROOT / "src"))

from orchestrator.payloads import PayloadStreamReader  # noqa: E402

ZERO_HASH = "0x" + "00" * 32


def _hex(b: bytes, *, length: int | None = None) -> str:
    h = b.hex()
    if length is not None and len(h) < length * 2:
        h = h.zfill(length * 2)
    return "0x" + h


def _hex_int(b: bytes) -> str:
    """Encode an RLP-decoded big-endian unsigned integer as quantity hex."""
    n = int.from_bytes(b, "big") if b else 0
    return hex(n)


def _hex_data(b: bytes) -> str:
    """Encode bytes as a Data hex string (always even length, lowercase)."""
    return "0x" + b.hex()


def _decode_payload(body: bytes) -> dict:
    """Decode one length-prefixed RLP record into the Engine-API JSON shape."""
    parts = rlp.decode(body)
    (
        parent_hash,
        fee_recipient,
        state_root,
        receipts_root,
        logs_bloom,
        prev_randao,
        block_number,
        gas_limit,
        gas_used,
        timestamp,
        extra_data,
        base_fee_per_gas,
        block_hash,
        transactions,
        withdrawals,
        blob_gas_used,
        excess_blob_gas,
    ) = parts

    return {
        "parentHash": _hex_data(parent_hash),
        "feeRecipient": _hex_data(fee_recipient),
        "stateRoot": _hex_data(state_root),
        "receiptsRoot": _hex_data(receipts_root),
        "logsBloom": _hex_data(logs_bloom),
        "prevRandao": _hex_data(prev_randao),
        "blockNumber": _hex_int(block_number),
        "gasLimit": _hex_int(gas_limit),
        "gasUsed": _hex_int(gas_used),
        "timestamp": _hex_int(timestamp),
        "extraData": _hex_data(extra_data),
        "baseFeePerGas": _hex_int(base_fee_per_gas),
        "blockHash": _hex_data(block_hash),
        "transactions": [_hex_data(tx) for tx in transactions],
        "withdrawals": [_decode_withdrawal(w) for w in withdrawals],
        "blobGasUsed": _hex_int(blob_gas_used),
        "excessBlobGas": _hex_int(excess_blob_gas),
    }


def _decode_withdrawal(w: bytes | list) -> dict:
    """Best-effort decode of a withdrawal entry; format may vary."""
    if isinstance(w, bytes):
        # We never see this in our recording (withdrawals=[]) but keep a sane fallback.
        return {"raw": _hex_data(w)}
    index, validator_index, address, amount = w
    return {
        "index": _hex_int(index),
        "validatorIndex": _hex_int(validator_index),
        "address": _hex_data(address),
        "amount": _hex_int(amount),
    }


def _bearer(jwt_path: Path) -> str:
    secret_hex = jwt_path.read_text(encoding="utf-8").strip()
    secret = bytes.fromhex(secret_hex)
    token = pyjwt.encode({"iat": int(time.time())}, secret, algorithm="HS256")
    if isinstance(token, bytes):
        token = token.decode("ascii")
    return token


def _rpc(client: httpx.Client, url: str, method: str, params: list, *, jwt_path: Path | None = None) -> dict:
    headers = {"content-type": "application/json"}
    if jwt_path is not None:
        headers["authorization"] = f"Bearer {_bearer(jwt_path)}"
    resp = client.post(
        url,
        headers=headers,
        json={"jsonrpc": "2.0", "method": method, "params": params, "id": 1},
        timeout=120.0,
    )
    resp.raise_for_status()
    body = resp.json()
    if "error" in body:
        raise RuntimeError(f"{method} returned error: {body['error']}")
    return body["result"]


def _summarise_block(client: httpx.Client, public_url: str, block_ref: str) -> dict:
    blk = _rpc(client, public_url, "eth_getBlockByNumber", [block_ref, False])
    if blk is None:
        return {"available": False}
    return {
        "available": True,
        "number": blk["number"],
        "hash": blk["hash"],
        "stateRoot": blk["stateRoot"],
        "gasUsed": blk["gasUsed"],
        "txCount": len(blk["transactions"]),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--payloads", default="state/payloads.rlp", type=Path)
    parser.add_argument("--jwt", default="state-geth/jwt.hex", type=Path)
    parser.add_argument("--auth-url", default="http://127.0.0.1:8651")
    parser.add_argument("--public-url", default="http://127.0.0.1:8645")
    parser.add_argument(
        "--nm-url",
        default="http://127.0.0.1:8545",
        help="Nethermind public RPC for cross-client comparison",
    )
    parser.add_argument(
        "--max",
        type=int,
        default=None,
        help="Cap the number of payloads to push (debug aid).",
    )
    args = parser.parse_args()

    if not args.payloads.exists():
        print(f"missing payloads file: {args.payloads}", file=sys.stderr)
        return 2
    if not args.jwt.exists():
        print(f"missing jwt file: {args.jwt}", file=sys.stderr)
        return 2

    accepted = 0
    rejected = 0
    last_valid_hash: str | None = None
    last_block_number: int | None = None
    fail_reason: dict | None = None

    with httpx.Client() as client:
        # Sanity-check geth liveness via the public port.
        try:
            chain_id = _rpc(client, args.public_url, "eth_chainId", [])
            print(f"[geth] chain_id={chain_id}")
        except Exception as exc:  # noqa: BLE001
            print(f"failed to reach geth public port {args.public_url}: {exc}", file=sys.stderr)
            return 3

        genesis = _summarise_block(client, args.public_url, "0x0")
        print(f"[geth] genesis: {genesis}")

        for index, body in enumerate(PayloadStreamReader(args.payloads)):
            if args.max is not None and index >= args.max:
                break
            payload = _decode_payload(body)
            block_number = int(payload["blockNumber"], 16)
            block_hash = payload["blockHash"]

            # Genesis activates Prague (and Osaka) at timestamp 0, so geth
            # demands newPayloadV4 (post-Prague) for these payloads. The
            # 4th arg is execution_requests (deposits/withdrawals/consolidations);
            # `testing_commitBlockV1` produced no requests during recording, so
            # an empty list is the right replay value.
            params = [
                payload,
                [],  # expectedBlobVersionedHashes
                ZERO_HASH,  # parentBeaconBlockRoot — orchestrator zeroed it on commit
                [],  # executionRequests — empty (post-Prague)
            ]
            try:
                np_result = _rpc(client, args.auth_url, "engine_newPayloadV4", params, jwt_path=args.jwt)
            except Exception as exc:  # noqa: BLE001
                fail_reason = {"phase": "newPayloadV4", "error": str(exc), "block": block_number}
                print(f"[block {block_number}] newPayloadV4 transport error: {exc}")
                break
            status = np_result.get("status")
            if status != "VALID":
                rejected += 1
                fail_reason = {
                    "phase": "newPayloadV4",
                    "block": block_number,
                    "block_hash": block_hash,
                    "result": np_result,
                }
                print(
                    f"[block {block_number}] newPayloadV4 status={status} "
                    f"latestValidHash={np_result.get('latestValidHash')!r} "
                    f"err={np_result.get('validationError')!r}"
                )
                break

            # Advance fork choice so the head moves and subsequent payloads chain on.
            try:
                fcu = _rpc(
                    client,
                    args.auth_url,
                    "engine_forkchoiceUpdatedV3",
                    [
                        {
                            "headBlockHash": block_hash,
                            "safeBlockHash": block_hash,
                            "finalizedBlockHash": block_hash,
                        },
                        None,
                    ],
                    jwt_path=args.jwt,
                )
            except Exception as exc:  # noqa: BLE001
                fail_reason = {"phase": "fcuV3", "error": str(exc), "block": block_number}
                print(f"[block {block_number}] fcuV3 transport error: {exc}")
                break
            fcu_status = fcu.get("payloadStatus", {}).get("status")
            if fcu_status != "VALID":
                rejected += 1
                fail_reason = {
                    "phase": "fcuV3",
                    "block": block_number,
                    "block_hash": block_hash,
                    "result": fcu,
                }
                print(f"[block {block_number}] fcuV3 status={fcu_status} body={fcu}")
                break

            accepted += 1
            last_valid_hash = block_hash
            last_block_number = block_number
            if accepted == 1 or accepted % 25 == 0:
                print(f"[block {block_number}] accepted ({accepted} total)")

        print()
        print("=== replay summary ===")
        print(f"  payloads accepted: {accepted}")
        print(f"  payloads rejected: {rejected}")
        if last_block_number is not None:
            print(f"  last accepted block: #{last_block_number} hash={last_valid_hash}")
        if fail_reason is not None:
            print(f"  fail_reason: {fail_reason}")

        # Cross-client comparison.
        print()
        print("=== cross-client final-state comparison ===")
        head_geth = _summarise_block(client, args.public_url, "latest")
        print(f"  geth latest:      {head_geth}")
        for tag in ("0x0", "latest"):
            try:
                nm = _summarise_block(client, args.nm_url, tag)
                print(f"  nethermind {tag:>6}: {nm}")
            except Exception as exc:  # noqa: BLE001
                print(f"  nethermind {tag}: unreachable ({exc})")

    return 0 if fail_reason is None else 1


if __name__ == "__main__":
    raise SystemExit(main())
