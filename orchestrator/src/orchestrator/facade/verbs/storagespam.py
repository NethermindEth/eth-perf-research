"""Sparse SSTORE of non-zero values — maximises storage trie growth per gas."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


_STORAGESPAM_SELECTOR = bytes.fromhex("8c8e4f53")  # `storeMany(uint256,uint256)` placeholder


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        to = ctx.derive_address(0)
        data = _STORAGESPAM_SELECTOR + index.to_bytes(32, "big") + (16).to_bytes(32, "big")
        signable = {
            "type": 2,
            "nonce": index,
            "to": to,
            "value": 0,
            "gas": 2_000_000,
            "data": data,
        }
        return signable, {"slots_written": 16, "start_slot": index}

    return pack_until_deadline(deadline_bytes, context, build_one, "storagespam")
