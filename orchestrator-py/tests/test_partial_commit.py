"""Partial-commit handling: testing_commitBlockV1 returns -32000 with
"expected N transactions but only M were included" when the EL accepted only
a prefix of the batch. The orchestrator must journal a record covering the
landed prefix, advance address_cursor by that count, and continue.
"""

from __future__ import annotations

import pytest

from orchestrator.lifecycle import _parse_partial_inclusion
from orchestrator.rpc import RpcError


@pytest.mark.parametrize(
    "message,expected",
    [
        ("expected 4065 transactions but only 3922 were included", 3922),
        ("expected 100 but only 80 were included", 80),
        ("only 1 of 2 included", 1),
    ],
)
def test_parse_partial_inclusion_extracts_landed_count(message: str, expected: int) -> None:
    exc = RpcError(method="testing_commitBlockV1", code=-32000, message=message)
    assert _parse_partial_inclusion(exc) == expected


@pytest.mark.parametrize(
    "code,message",
    [
        (-32603, "expected 100 but only 80 were included"),  # wrong code
        (-32000, "transaction underpriced"),  # not a partial-inclusion
        (-32000, "block reverted"),
    ],
)
def test_parse_partial_inclusion_rejects_unrelated_errors(code: int, message: str) -> None:
    exc = RpcError(method="testing_commitBlockV1", code=code, message=message)
    assert _parse_partial_inclusion(exc) is None


def test_parse_partial_inclusion_handles_zero_included() -> None:
    # "only 0 included" means the whole batch bounced. Caller treats None and 0
    # the same way (re-raise) so the cursor isn't silently advanced by zero.
    exc = RpcError(method="testing_commitBlockV1", code=-32000, message="only 0 of 100 were included")
    assert _parse_partial_inclusion(exc) == 0
