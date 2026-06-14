#!/usr/bin/env python3
"""Byte-for-byte equivalence test: FastSigner output must match eth_account.

Run from the tx-signer/ dir AFTER `make linux` (or `make build` for local macOS dev).
Builds a corpus of 10k random EIP-1559 type-2 txs and signs each two ways.

  uv run --with eth_account --with protobuf python3 test_equivalence.py
"""

from __future__ import annotations

import os
import random
import secrets
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# Make `orchestrator._proto` importable without dragging in the rest of the
# orchestrator package (which depends on eels_spamoor_builders, not in this env).
import types as _types
_orch_pkg = _types.ModuleType("orchestrator")
_orch_pkg.__path__ = [str(ROOT / "orchestrator" / "src" / "orchestrator")]
sys.modules["orchestrator"] = _orch_pkg
_proto_pkg = _types.ModuleType("orchestrator._proto")
_proto_pkg.__path__ = [str(ROOT / "orchestrator" / "src" / "orchestrator" / "_proto")]
sys.modules["orchestrator._proto"] = _proto_pkg

from eth_account import Account
from eth_utils import to_checksum_address

# Import FastSigner directly without going through orchestrator.facade.__init__.
import importlib.util as _u
_spec = _u.spec_from_file_location(
    "fast_signer", ROOT / "orchestrator" / "src" / "orchestrator" / "facade" / "fast_signer.py"
)
_mod = _u.module_from_spec(_spec)
_spec.loader.exec_module(_mod)
FastSigner = _mod.FastSigner

CHAIN_ID = 1
PRIV_HEX = "bcdf20249abf0ed6d944c0288fad489e33f66b3960d9e6229c1cd214ed3bbe31"
PRIV = bytes.fromhex(PRIV_HEX)


def random_tx(rng: random.Random, nonce: int) -> dict:
    """Build a random EIP-1559 tx dict that exercises common edges."""
    is_create = rng.random() < 0.1   # 10% contract creates (empty `to`)
    to = "" if is_create else to_checksum_address("0x" + bytes(rng.randbytes(20)).hex())
    data_len = rng.choice([0, 0, 4, 32, 100, 256])
    data = "0x" + bytes(rng.randbytes(data_len)).hex() if data_len else "0x"
    value = rng.choice([0, 0, 1, 10**18, rng.randint(0, 10**24)])
    gas = rng.choice([21000, 40_000, 100_000, 400_000, 2_000_000])

    n_access = rng.choice([0, 0, 0, 1, 3, 10])
    access = []
    for _ in range(n_access):
        access.append({
            "address": to_checksum_address("0x" + bytes(rng.randbytes(20)).hex()),
            "storageKeys": [
                "0x" + bytes(rng.randbytes(32)).hex()
                for _ in range(rng.randint(0, 5))
            ],
        })

    return {
        "type": 2,
        "chainId": CHAIN_ID,
        "nonce": nonce,
        "maxPriorityFeePerGas": rng.randint(1, 10**11),
        "maxFeePerGas": rng.randint(10**11, 10**13),
        "gas": gas,
        "to": to,
        "value": value,
        "data": data,
        "accessList": access,
    }


def main() -> int:
    binary = os.environ.get("TX_SIGNER_BINARY", "./tx-signer")
    if not os.path.exists(binary):
        print(f"signer binary not found at {binary} — run `make build` first", file=sys.stderr)
        return 2

    N = int(os.environ.get("EQUIV_N", "10000"))
    rng = random.Random(secrets.randbits(64))

    print(f"generating {N} random type-2 templates ...")
    txs = [random_tx(rng, nonce=i) for i in range(N)]

    print("signing with eth_account (reference) ...")
    t0 = time.perf_counter()
    expected = []
    for t in txs:
        signed = Account.sign_transaction(t, PRIV)
        expected.append(bytes(signed.raw_transaction))
    t_eth = time.perf_counter() - t0
    print(f"  eth_account: {N} txs in {t_eth:.2f}s = {N / t_eth:,.0f} tx/s")

    print("signing with FastSigner (subject) ...")
    signer = FastSigner(PRIV, signer_binary=binary)
    t0 = time.perf_counter()
    actual = signer.sign_batch(txs)
    t_fast = time.perf_counter() - t0
    print(f"  FastSigner: {N} txs in {t_fast:.2f}s = {N / t_fast:,.0f} tx/s")
    print(f"  speedup: {t_eth / t_fast:.1f}x")

    mismatches = []
    for i, (e, a) in enumerate(zip(expected, actual)):
        if e != a:
            mismatches.append(i)
            if len(mismatches) <= 5:
                print(f"  MISMATCH @ idx {i}:")
                print(f"    tx:       {txs[i]}")
                print(f"    eth      : {e.hex()}")
                print(f"    fast     : {a.hex()}")

    if mismatches:
        print(f"\nFAIL: {len(mismatches)}/{N} signed bytes differ")
        return 1

    print(f"\nPASS: all {N} signed bytes match byte-for-byte")
    return 0


if __name__ == "__main__":
    sys.exit(main())
