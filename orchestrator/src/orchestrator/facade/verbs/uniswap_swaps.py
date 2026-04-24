"""Uniswap-style swap — mixed storage effect (pool + balance slots)."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


_SWAP_SELECTOR = bytes.fromhex("128acb08")  # uniswapV3 `swap(...)` prefix


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        pool = ctx.derive_address(1)
        data = _SWAP_SELECTOR + (index).to_bytes(32, "big") + (1).to_bytes(32, "big")
        signable = {
            "type": 2,
            "nonce": index,
            "to": pool,
            "value": 0,
            "gas": 250_000,
            "data": data,
        }
        return signable, {"swap_pool": "0x" + pool.hex()}

    return pack_until_deadline(deadline_bytes, context, build_one, "uniswap_swaps")
