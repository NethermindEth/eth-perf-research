"""Internal tx builder used by all 12 verb adapters.

This is a deliberate seam: it keeps adapter signatures uniform and replaces the EELS
scenario dispatch with equivalent, well-formed signed transactions. When the EELS port
(`execution-specs feat/spamoor-to-est`) becomes importable, each adapter can swap this
builder for the upstream helper with no change to the facade's public contract.

Every adapter obeys the deadline-bytes budget by packing until the next tx would push
the cumulative RLP size over the limit — at least one tx is always returned to ensure
forward progress.
"""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from .context import FacadeContext, SignedTransaction


def _sign(tx_fields: dict[str, Any], context: FacadeContext) -> bytes:
    """Sign an EIP-1559 tx via the cached LocalAccount on the context.

    Reusing ``context.account`` avoids per-call key derivation; merging with a
    pre-bound ``base_tx_fields()`` template replaces four ``setdefault`` probes
    with a single C-level dict merge.

    EELS builders return ``to``/``data`` as hex strings; the canonical signing
    path here uses ``bytes``/``bytes | None``. Normalize both so callers can
    pass either shape without forcing the verb layer to convert.
    """
    full = {**context.base_tx_fields(), **tx_fields}
    to_val = full.get("to")
    if isinstance(to_val, str):
        full["to"] = bytes.fromhex(to_val[2:]) if to_val.startswith("0x") else (
            bytes.fromhex(to_val) if to_val else None
        )
    data_val = full.get("data")
    if isinstance(data_val, str):
        full["data"] = bytes.fromhex(data_val[2:]) if data_val.startswith("0x") else (
            bytes.fromhex(data_val) if data_val else b""
        )
    signed = context.account.sign_transaction(full)
    return bytes(signed.raw_transaction)


def pack_until_deadline(
    deadline_bytes: int,
    context: FacadeContext,
    build_one: Callable[[int, FacadeContext], tuple[dict[str, Any], dict[str, Any]]],
    kind: str,
    *,
    gas_budget: int | None = None,
) -> list[SignedTransaction]:
    """Generic build-and-pack loop.

    `build_one(index, ctx) -> (signable_fields, diagnostic_fields)`.

    The first returned tuple feeds `Account.sign_transaction`; the second is attached to
    the `SignedTransaction.fields` for downstream introspection (tests, journaling, etc.)
    without re-decoding the RLP.

    Returns at least one tx even if the first tx exceeds the deadline — forward progress
    is more important than strict budget adherence when the budget is tiny.

    With ``gas_budget``, also stops once cumulative expected gas-used would
    exceed ``0.95 * gas_budget``. Expected-used = ``signable["gas"] *
    context.verb_gas_factors[kind]``, floored at the 21 K intrinsic so a
    near-zero factor can't pack an empty block.
    """
    out: list[SignedTransaction] = []
    accumulated = 0
    accumulated_expected_gas = 0
    gas_ceiling = int(gas_budget * 0.95) if gas_budget is not None else None
    factor = context.verb_gas_factors.get(kind, 1.0)
    starting_cursor = context.address_cursor
    while True:
        signable, diag = build_one(context.address_cursor, context)
        raw = _sign(signable, context)
        tx_size = len(raw)
        tx_gas_limit = int(signable.get("gas", 0))
        tx_expected_gas = max(int(tx_gas_limit * factor), 21_000)
        if out and accumulated + tx_size > deadline_bytes:
            break
        if (
            gas_ceiling is not None
            and out
            and accumulated_expected_gas + tx_expected_gas > gas_ceiling
        ):
            break
        out.append(
            SignedTransaction(rlp=raw, kind=kind, fields=diag, gas_limit=tx_gas_limit)
        )
        accumulated += tx_size
        accumulated_expected_gas += tx_expected_gas
        context.address_cursor += 1
        # Safety: if deadline is 0 and we've emitted one tx, stop.
        if accumulated >= deadline_bytes:
            break
        if gas_ceiling is not None and accumulated_expected_gas >= gas_ceiling:
            break
        # Safety: bound the loop to avoid runaway on absurdly-large deadlines in tests.
        if context.address_cursor - starting_cursor > 200_000:
            break
    return out
