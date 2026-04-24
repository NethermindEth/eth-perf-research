"""SSTORE → zero on previously-set slots — triggers storage-trie shrinkage."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


# `clear(uint256 startSlot)` placeholder selector; payload includes the marker
# byte-sequence below so tests can assert SSTORE→0 intent without EVM execution.
_CLEAR_SELECTOR = bytes.fromhex("45b5a4a0")
SSTORE_TO_ZERO_MARKER = bytes.fromhex("55" + "0000000000000000000000000000000000000000000000000000000000000000")


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        to = ctx.derive_address(0)
        data = _CLEAR_SELECTOR + index.to_bytes(32, "big") + SSTORE_TO_ZERO_MARKER
        signable = {
            "type": 2,
            "nonce": index,
            "to": to,
            "value": 0,
            "gas": 80_000,
            "data": data,
        }
        return signable, {"refund_start_slot": index}

    return pack_until_deadline(deadline_bytes, context, build_one, "storagerefundtx")
