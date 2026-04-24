"""Contract creation (CREATE) — grows both `accountsTotal` and code bytes."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


# A minimal runtime that just returns (PUSH1 0x00 PUSH1 0x00 RETURN).
# Wrapped in constructor: PUSH1 len PUSH1 offset PUSH1 0x00 CODECOPY PUSH1 len PUSH1 0x00 RETURN
_RUNTIME = bytes.fromhex("60006000f3")
_INIT = bytes.fromhex("6005600c60003960056000f3") + _RUNTIME


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        signable = {
            "type": 2,
            "nonce": index,
            "to": None,
            "value": 0,
            "gas": 300_000,
            "data": _INIT,
        }
        return signable, {"is_deploy": True, "init_len": len(_INIT)}

    return pack_until_deadline(deadline_bytes, context, build_one, "deploytx")
