"""Pin the EELS-spamoor placeholder bytecodes preallocated in lab-genesis.json.

A future genesis edit must not silently change these — every EELS scenario in
``facade/verbs.py`` targets one of these addresses with ``reuse_contract=True``,
so the runtime bytecode must keep its shape across edits.

Tests are deliberately strict: the exact ``code`` value is pinned. Update both
this test and ``state-geth/genesis.json`` together when changing a placeholder.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

# (lowercase address -> expected code; ``None`` means no ``code`` field).
EXPECTED_PREALLOCS: dict[str, str | None] = {
    "1111111111111111111111111111111111111111": "0x00",
    "2222222222222222222222222222222222222222": "0x00",
    "3333333333333333333333333333333333333333": "0x5b600056",
    "4444444444444444444444444444444444444444": "0x602060005260206000F3",
    "5555555555555555555555555555555555555555": "0x6020600052602060015260206000F3",
    "6666666666666666666666666666666666666666": "0x6020600052602060015260206000F3",
    "7777777777777777777777777777777777777777": None,  # EOA — balance only.
    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "0x6020600052602060015260206000F3",
    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb": (
        "0x6000546001018080556001018080556001018080556001018080556001018080"
        "556001018080556001018080556001018080556001018080556001018080556001"
        "018080556001018080556001018080556001018080556001018080556001018080"
        "558060005500"
    ),
    "dddddddddddddddddddddddddddddddddddddddd": "0x6020600052602060015260206000F3",
    "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee": (
        "0x6000546001018080556001018080556001018080556001018080556001018080"
        "556001018080556001018080556001018080556001018080556001018080556001"
        "018080556001018080556001018080556001018080556001018080556001018080"
        "558060005500"
    ),
}

LAB_GENESIS = (
    Path(__file__).resolve().parent.parent / "lab-genesis.json"
)


def _normalize_addr(key: str) -> str:
    return key.removeprefix("0x").lower()


def _load_alloc(path: Path, accounts_key: str) -> dict[str, dict]:
    """Index alloc entries by lowercase, no-0x address."""
    raw = json.loads(path.read_text())
    accounts = raw[accounts_key]
    return {_normalize_addr(k): v for k, v in accounts.items()}


@pytest.mark.skipif(not LAB_GENESIS.exists(), reason="lab-genesis.json not committed; pair with NM repo")
@pytest.mark.parametrize("addr,expected_code", EXPECTED_PREALLOCS.items())
def test_lab_genesis_prealloc(addr: str, expected_code: str | None) -> None:
    accounts = _load_alloc(LAB_GENESIS, "accounts")
    entry = accounts.get(addr)
    assert entry is not None, f"{addr}: missing from lab-genesis.json"
    if expected_code is None:
        assert "code" not in entry, f"{addr}: expected EOA but found code"
        # Sanity: account exists in the trie.
        balance = entry.get("balance", "0")
        assert balance not in ("0", "0x0"), (
            f"{addr}: EOA recipient must have non-zero balance to exist in trie"
        )
    else:
        actual = entry.get("code", "")
        assert actual.lower() == expected_code.lower(), (
            f"{addr}: code mismatch\n  actual:   {actual}\n  expected: {expected_code}"
        )


