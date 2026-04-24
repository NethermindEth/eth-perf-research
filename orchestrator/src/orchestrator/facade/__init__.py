"""Facade: 12-verb registry + `dispatch` entry.

```python
txs = dispatch("eoatx", deadline_bytes=9_500_000, context=ctx)
```

The returned list's cumulative RLP byte length is ≤ `deadline_bytes` (except the
degenerate case where a single tx is already larger than the budget — forward
progress beats strict budget adherence).
"""
from __future__ import annotations

from typing import Callable

from .context import FacadeContext, SignedTransaction
from .verbs import (
    blob_combined,
    calltx,
    deploytx,
    eoatx,
    erc20_bloater,
    erc20tx,
    evm_fuzz,
    factorydeploytx,
    gasburnertx,
    storagerefundtx,
    storagespam,
    uniswap_swaps,
)


Adapter = Callable[[int, FacadeContext], list[SignedTransaction]]


VERBS: dict[str, Adapter] = {
    "eoatx": eoatx.adapter,
    "calltx": calltx.adapter,
    "deploytx": deploytx.adapter,
    "factorydeploytx": factorydeploytx.adapter,
    "storagespam": storagespam.adapter,
    "erc20_bloater": erc20_bloater.adapter,
    "erc20tx": erc20tx.adapter,
    "uniswap_swaps": uniswap_swaps.adapter,
    "storagerefundtx": storagerefundtx.adapter,
    "gasburnertx": gasburnertx.adapter,
    "blob_combined": blob_combined.adapter,
    "evm_fuzz": evm_fuzz.adapter,
}


class UnknownVerb(KeyError):
    """Raised by `dispatch` when the verb is not registered."""


def dispatch(
    verb: str, deadline_bytes: int, context: FacadeContext
) -> list[SignedTransaction]:
    try:
        adapter = VERBS[verb]
    except KeyError as exc:
        raise UnknownVerb(f"unknown facade verb: {verb!r}") from exc
    return adapter(deadline_bytes, context)


__all__ = [
    "VERBS",
    "dispatch",
    "FacadeContext",
    "SignedTransaction",
    "UnknownVerb",
]
