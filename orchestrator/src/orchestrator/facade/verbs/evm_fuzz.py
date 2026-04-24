"""EVM fuzz scenario — mixed-effect random opcode sequences for correctness exercise."""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        to = ctx.derive_address(0)
        # Deterministic pseudo-random payload seeded by index — every byte varies between calls.
        payload = ((index * 0x9E3779B97F4A7C15) & ((1 << 256) - 1)).to_bytes(32, "big")
        signable = {
            "type": 2,
            "nonce": index,
            "to": to,
            "value": 0,
            "gas": 300_000,
            "data": payload,
        }
        return signable, {"fuzz_seed": index}

    return pack_until_deadline(deadline_bytes, context, build_one, "evm_fuzz")
