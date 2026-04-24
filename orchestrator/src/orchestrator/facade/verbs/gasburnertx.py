"""Compute-only burn — no state effect, used to model pure gas consumption."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


_BURN_SELECTOR = bytes.fromhex("8b9a4f53")


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        to = ctx.derive_address(0)
        data = _BURN_SELECTOR + (100_000).to_bytes(32, "big")
        signable = {
            "type": 2,
            "nonce": index,
            "to": to,
            "value": 0,
            "gas": 1_500_000,
            "data": data,
        }
        return signable, {"compute_only": True}

    return pack_until_deadline(deadline_bytes, context, build_one, "gasburnertx")
