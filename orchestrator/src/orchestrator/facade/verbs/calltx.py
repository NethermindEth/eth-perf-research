"""Plain CALL to existing accounts — touches but doesn't expand state."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        # Call a previously-created address (touch-only).
        target_index = max(0, index - 1)
        to = ctx.derive_address(target_index)
        signable = {
            "type": 2,
            "nonce": index,
            "to": to,
            "value": 0,
            "gas": 40_000,
            "data": b"\x00" * 4,  # 4-byte selector, no args
        }
        return signable, {"to": "0x" + to.hex(), "call_touch_only": True}

    return pack_until_deadline(deadline_bytes, context, build_one, "calltx")
