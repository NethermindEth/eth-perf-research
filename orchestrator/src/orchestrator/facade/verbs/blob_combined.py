"""EIP-4844 blob-carrying tx — exercises blob state path.

Note: full blob tx encoding requires kzg blob commitments; for orchestrator unit-test
coverage we emit a placeholder type-2 tx with `blob_count` diagnostic set, because the
plugin's state-composition counters do not include blob sidecars. The EELS port's
`blob_combined` scenario swaps in the real type-3 encoding.
"""
from __future__ import annotations

from .._builder import pack_until_deadline
from ..context import FacadeContext, SignedTransaction


def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    def build_one(index: int, ctx: FacadeContext):
        to = ctx.derive_address(0)
        signable = {
            "type": 2,
            "nonce": index,
            "to": to,
            "value": 0,
            "gas": 200_000,
            "data": b"\xff" * 32,  # proxy blob reference
        }
        return signable, {"blob_tx_placeholder": True, "blob_count": 3}

    return pack_until_deadline(deadline_bytes, context, build_one, "blob_combined")
