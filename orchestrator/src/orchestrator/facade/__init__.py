"""Facade: 12-verb data-driven registry + `dispatch` entry.

```python
txs = dispatch("eoatx", deadline_bytes=9_500_000, context=ctx)
```

The returned list's cumulative RLP byte length is ≤ ``deadline_bytes`` (except the
degenerate case where a single tx is already larger than the budget — forward
progress beats strict budget adherence).
"""

from __future__ import annotations

from collections.abc import Callable

from .context import FacadeContext, SignedTransaction
from .verbs import VERB_SPECS, build_adapter

Adapter = Callable[[int, FacadeContext], list[SignedTransaction]]


VERBS: dict[str, Adapter] = {spec.name: build_adapter(spec) for spec in VERB_SPECS}


class UnknownVerb(KeyError):
    """Raised by `dispatch` when the verb is not registered."""


def dispatch(verb: str, deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
    try:
        adapter = VERBS[verb]
    except KeyError as exc:
        raise UnknownVerb(f"unknown facade verb: {verb!r}") from exc
    return adapter(deadline_bytes, context)


__all__ = [
    "VERBS",
    "VERB_SPECS",
    "FacadeContext",
    "SignedTransaction",
    "UnknownVerb",
    "dispatch",
]
