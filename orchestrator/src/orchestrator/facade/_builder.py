"""Internal tx builder used by all 12 verb adapters.

This version replaces the per-tx ``Account.sign_transaction`` call with a
batched out-of-process Go signer (``FastSigner``). The Go side fans out across
goroutines, so a single :py:meth:`pack_until_deadline` call signs hundreds of
thousands of transactions per second on a multi-core host.

Public contract unchanged: ``pack_until_deadline(deadline_bytes, context,
build_one, kind, *, gas_budget=None) -> list[SignedTransaction]``. Adapters
upstream see identical behavior (same RLP bytes, same metadata fields, same
forward-progress / over-budget rules).
"""

from __future__ import annotations

import os
from collections.abc import Callable
from typing import Any

from .context import FacadeContext, SignedTransaction
from .fast_signer import FastSigner


# One FastSigner subprocess per signer key, kept alive for the orchestrator's
# lifetime. Keyed by the deploy private key so multi-key future-proofing is
# possible; in practice there is exactly one entry.
_signer_cache: dict[bytes, FastSigner] = {}


def _get_signer(context: FacadeContext) -> FastSigner:
    key = context.deploy_private_key
    signer = _signer_cache.get(key)
    if signer is None:
        signer = FastSigner(
            private_key=key,
            signer_binary=os.environ.get("TX_SIGNER_BINARY", "/signer/tx-signer"),
        )
        _signer_cache[key] = signer
    return signer


def _normalize(signable: dict[str, Any], base: dict[str, Any]) -> dict[str, Any]:
    """Merge per-tx fields with the shared base + convert hex strings to bytes.

    Mirrors the old ``_sign``'s normalization step so the FastSigner consumes
    the same shape as ``eth_account.Account.sign_transaction`` did.
    """
    full = {**base, **signable}
    to_val = full.get("to")
    if isinstance(to_val, str):
        full["to"] = (
            bytes.fromhex(to_val[2:]) if to_val.startswith("0x")
            else (bytes.fromhex(to_val) if to_val else None)
        )
    data_val = full.get("data")
    if isinstance(data_val, str):
        full["data"] = (
            bytes.fromhex(data_val[2:]) if data_val.startswith("0x")
            else (bytes.fromhex(data_val) if data_val else b"")
        )
    return full


def _estimate_signed_size(signable: dict[str, Any]) -> int:
    """Conservative estimate of the type-2 RLP signed size for budget packing.

    Overhead breakdown:
      - 1 byte type prefix
      - ~70 bytes for the RLP frame + scalar fields (chainId, nonces, fees, gas, value, sig)
      - 21 bytes for the 20-byte ``to`` + length tag (or 1 byte for empty `to`)
      - ``len(data)`` + a few bytes for RLP-prefix
      - ~70 bytes per AccessTuple (20-byte address + ~50 bytes per 32-byte storage key)
    Slightly over-estimates so the deadline-bytes budget never gets exceeded.
    """
    data = signable.get("data") or b""
    data_len = len(data) if isinstance(data, (bytes, bytearray)) else (
        len(data) // 2 - 1 if isinstance(data, str) and data.startswith("0x") else 0
    )
    access_list = signable.get("accessList") or []
    al_bytes = 0
    for entry in access_list:
        al_bytes += 22
        al_bytes += 33 * len(entry.get("storageKeys", []))
    return 1 + 90 + 22 + data_len + al_bytes + 16


def pack_until_deadline(
    deadline_bytes: int,
    context: FacadeContext,
    build_one: Callable[[int, FacadeContext], tuple[dict[str, Any], dict[str, Any]]],
    kind: str,
    *,
    gas_budget: int | None = None,
) -> list[SignedTransaction]:
    """Build a batch of signable templates, then sign them all in one Go call.

    Behavior matches the previous serial implementation: at least one tx is
    always returned (forward progress), the ``gas_budget`` is capped at 95% of
    the supplied value, and the loop is bounded at 200,000 to defeat absurdly
    large deadlines in tests.
    """
    signables: list[tuple[dict[str, Any], dict[str, Any], int]] = []
    accumulated_est_bytes = 0
    accumulated_expected_gas = 0
    gas_ceiling = int(gas_budget * 0.95) if gas_budget is not None else None
    factor = context.verb_gas_factors.get(kind, 1.0)
    starting_cursor = context.address_cursor

    while True:
        signable, diag = build_one(context.address_cursor, context)
        tx_gas_limit = int(signable.get("gas", 0))
        tx_expected_gas = max(int(tx_gas_limit * factor), 21_000)
        est_size = _estimate_signed_size(signable)

        if signables and accumulated_est_bytes + est_size > deadline_bytes:
            break
        if (
            gas_ceiling is not None
            and signables
            and accumulated_expected_gas + tx_expected_gas > gas_ceiling
        ):
            break

        signables.append((signable, diag, tx_gas_limit))
        accumulated_est_bytes += est_size
        accumulated_expected_gas += tx_expected_gas
        context.address_cursor += 1

        if accumulated_est_bytes >= deadline_bytes:
            break
        if gas_ceiling is not None and accumulated_expected_gas >= gas_ceiling:
            break
        # Cap batch size to keep NM block production fast; partial-acceptance
        # creates cursor drift, so prefer many small batches over one huge one.
        if context.address_cursor - starting_cursor >= int(
            os.environ.get("FAST_SIGNER_MAX_TXS_PER_BATCH", "1500")
        ):
            break
        if context.address_cursor - starting_cursor > 200_000:
            break

    if not signables:
        return []

    base = context.base_tx_fields()
    normalized = [_normalize(s, base) for s, _, _ in signables]
    raws = _get_signer(context).sign_batch(normalized)

    return [
        SignedTransaction(rlp=raw, kind=kind, fields=diag, gas_limit=tx_gas)
        for raw, (_, diag, tx_gas) in zip(raws, signables)
    ]
