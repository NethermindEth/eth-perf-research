"""ERC20 transfers between existing holders — small storage churn."""
from __future__ import annotations

from .._builder import pack_until_deadline
from .erc20_bloater import _ERC20_TRANSFER
from ..context import FacadeContext, SignedTransaction


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        token = ctx.derive_address(0)
        # Recipient bounces between a small existing pool → no fresh storage slot.
        recipient = ctx.derive_address((index % 64) + 1)
        data = _ERC20_TRANSFER + b"\x00" * 12 + recipient + (1).to_bytes(32, "big")
        signable = {
            "type": 2,
            "nonce": index,
            "to": token,
            "value": 0,
            "gas": 60_000,
            "data": data,
        }
        return signable, {"erc20_recipient": "0x" + recipient.hex(), "churn_only": True}

    return pack_until_deadline(deadline_bytes, context, build_one, "erc20tx")
