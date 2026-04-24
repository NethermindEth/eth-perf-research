"""Factory CREATE2 deployment — salted child deploys, grows code + accounts."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


_FACTORY_CALL_PREFIX = b"\xff" * 4  # placeholder selector for create2 factory call


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        factory = ctx.derive_address(0)  # all factory calls go to a single deployed factory
        salt = ctx.next_salt()
        signable = {
            "type": 2,
            "nonce": index,
            "to": factory,
            "value": 0,
            "gas": 400_000,
            "data": _FACTORY_CALL_PREFIX + salt,
        }
        return signable, {"is_factory_deploy": True, "salt": salt.hex()}

    return pack_until_deadline(deadline_bytes, context, build_one, "factorydeploytx")
