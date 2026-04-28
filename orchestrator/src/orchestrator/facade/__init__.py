"""Facade: 12-verb data-driven registry + `dispatch` entry.

```python
txs = dispatch("eoatx", deadline_bytes=9_500_000, context=ctx)
```

The returned list's cumulative RLP byte length is ≤ ``deadline_bytes`` (except the
degenerate case where a single tx is already larger than the budget — forward
progress beats strict budget adherence). When ``ORCH_GAS_AWARE_DISPATCH=1`` the
dispatcher additionally caps cumulative gas at ``0.95 * context.block_gas_limit``
so a heavy-gas verb (storagespam at 2 M gas/tx) can't request more gas than
the block can hold.
"""

from __future__ import annotations

import os
from collections.abc import Callable

from .context import FacadeContext, SignedTransaction
from .verbs import VERB_SPECS, build_adapter

Adapter = Callable[..., list[SignedTransaction]]

# Env-gated so legacy runs see no behaviour change. Set "1" or "true" to opt in.
GAS_AWARE_DISPATCH = os.environ.get("ORCH_GAS_AWARE_DISPATCH", "0").lower() in (
    "1",
    "true",
    "yes",
    "on",
)


VERBS: dict[str, Adapter] = {spec.name: build_adapter(spec) for spec in VERB_SPECS}


class UnknownVerb(KeyError):
    """Raised by `dispatch` when the verb is not registered."""


def dispatch(verb: str, deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    try:
        adapter = VERBS[verb]
    except KeyError as exc:
        raise UnknownVerb(f"unknown facade verb: {verb!r}") from exc
    gas_budget = context.block_gas_limit if GAS_AWARE_DISPATCH else None
    return adapter(deadline_bytes, context, gas_budget=gas_budget)


__all__ = [
    "VERBS",
    "VERB_SPECS",
    "FacadeContext",
    "SignedTransaction",
    "UnknownVerb",
    "dispatch",
]
