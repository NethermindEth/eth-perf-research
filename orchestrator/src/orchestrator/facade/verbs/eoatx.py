"""EOA-to-EOA value transfer — grows `accountsTotal`."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        to = ctx.derive_address(index)
        signable = {
            "type": 2,
            "nonce": index,
            "to": to,
            "value": 1,
            "gas": 21_000,
            "data": b"",
        }
        return signable, {"to": "0x" + to.hex(), "value": 1}

    return pack_until_deadline(deadline_bytes, context, build_one, "eoatx")
