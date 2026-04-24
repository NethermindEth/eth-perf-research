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
    with a single C-level dict merge. Over a multi-day run at ~100 txs/batch
    this is measurable (review H8 + P1 / TRIZ Prior Action).
    """
    full = {**context.base_tx_fields(), **tx_fields}
    signed = context.account.sign_transaction(full)
    return bytes(signed.raw_transaction)


def pack_until_deadline(
    deadline_bytes: int,
    context: FacadeContext,
    build_one: Callable[[int, FacadeContext], tuple[dict[str, Any], dict[str, Any]]],
    kind: str,
) -> list[SignedTransaction]:
    """Generic build-and-pack loop.

    `build_one(index, ctx) -> (signable_fields, diagnostic_fields)`.

    The first returned tuple feeds `Account.sign_transaction`; the second is attached to
    the `SignedTransaction.fields` for downstream introspection (tests, journaling, etc.)
    without re-decoding the RLP.

    Returns at least one tx even if the first tx exceeds the deadline — forward progress
    is more important than strict budget adherence when the budget is tiny.
    """
    out: list[SignedTransaction] = []
    accumulated = 0
    starting_cursor = context.address_cursor
    while True:
        signable, diag = build_one(context.address_cursor, context)
        raw = _sign(signable, context)
        tx_size = len(raw)
        if out and accumulated + tx_size > deadline_bytes:
            break
        out.append(SignedTransaction(rlp=raw, kind=kind, fields=diag))
        accumulated += tx_size
        context.address_cursor += 1
        # Safety: if deadline is 0 and we've emitted one tx, stop.
        if accumulated >= deadline_bytes:
            break
        # Safety: bound the loop to avoid runaway on absurdly-large deadlines in tests.
        if context.address_cursor - starting_cursor > 200_000:
            break
    return out
