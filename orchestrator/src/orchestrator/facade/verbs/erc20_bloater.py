"""ERC20 `transfer` to fresh recipients — grows storage via `balances[newAddr] = amt`."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


_ERC20_TRANSFER = bytes.fromhex("a9059cbb")  # keccak("transfer(address,uint256)")[:4]


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        token = ctx.derive_address(0)
        recipient = ctx.derive_address(index + 1_000_000)
        data = _ERC20_TRANSFER + b"\x00" * 12 + recipient + (1).to_bytes(32, "big")
        signable = {
            "type": 2,
            "nonce": index,
            "to": token,
            "value": 0,
            "gas": 80_000,
            "data": data,
        }
        return signable, {"erc20_recipient": "0x" + recipient.hex()}

    return pack_until_deadline(deadline_bytes, context, build_one, "erc20_bloater")
