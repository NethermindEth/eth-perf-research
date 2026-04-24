"""Facade registry + per-verb tests."""

from __future__ import annotations

import pytest
import rlp

from orchestrator.facade import VERBS, UnknownVerb, dispatch
from orchestrator.facade.context import FacadeContext
from orchestrator.facade.verbs import SSTORE_TO_ZERO_MARKER

EXPECTED_VERBS = {
    "eoatx",
    "calltx",
    "deploytx",
    "factorydeploytx",
    "storagespam",
    "erc20_bloater",
    "erc20tx",
    "uniswap_swaps",
    "storagerefundtx",
    "gasburnertx",
    "blob_combined",
    "evm_fuzz",
}


def _ctx() -> FacadeContext:
    return FacadeContext(
        base_address=(0x10_00_00).to_bytes(20, "big"),
        revision=0,
        address_cursor=0,
        chain_id=1337,
        deploy_private_key=b"\x42" * 32,
    )


def test_registry_has_all_12_verbs() -> None:
    assert set(VERBS.keys()) == EXPECTED_VERBS
    assert len(VERBS) == 12


@pytest.mark.parametrize("verb", sorted(EXPECTED_VERBS))
def test_every_verb_respects_deadline(verb: str) -> None:
    ctx = _ctx()
    txs = dispatch(verb, deadline_bytes=50_000, context=ctx)
    assert txs, f"{verb}: dispatch returned zero txs"
    total = sum(len(tx.rlp) for tx in txs)
    # Budget is respected once we have ≥2 txs (first tx always emitted for progress).
    if len(txs) > 1:
        assert total <= 50_000, f"{verb}: cumulative size {total} > 50000"


def test_eoatx_advances_address_cursor() -> None:
    ctx = _ctx()
    dispatch("eoatx", deadline_bytes=20_000, context=ctx)
    first_cursor = ctx.address_cursor
    dispatch("eoatx", deadline_bytes=20_000, context=ctx)
    assert ctx.address_cursor > first_cursor


def test_deploytx_produces_contract_creation() -> None:
    ctx = _ctx()
    txs = dispatch("deploytx", deadline_bytes=50_000, context=ctx)
    # Decode the outer envelope: type-2 txs are prefixed with 0x02, rest is RLP.
    first = txs[0]
    assert first.rlp[0] == 0x02, "deploytx must be EIP-1559"
    assert first.fields.get("is_deploy") is True
    decoded = rlp.decode(first.rlp[1:])
    # EIP-1559 field order:
    # [chainId, nonce, maxPrio, maxFee, gas, to, value, data, accessList, v, r, s]
    to_field = decoded[5]
    assert to_field == b"", "deploy tx has empty `to`"


def test_factorydeploytx_advances_salt_cursor() -> None:
    ctx = _ctx()
    dispatch("factorydeploytx", deadline_bytes=10_000, context=ctx)
    assert ctx.salt_cursor > 0


def test_storagerefundtx_encodes_SSTORE_to_zero_marker() -> None:
    ctx = _ctx()
    txs = dispatch("storagerefundtx", deadline_bytes=20_000, context=ctx)
    assert txs
    # Tx data contains the SSTORE→0 marker (32-byte zero value preceded by 0x55).
    decoded = rlp.decode(txs[0].rlp[1:])
    data = decoded[7]
    assert SSTORE_TO_ZERO_MARKER in data


def test_unknown_verb_raises() -> None:
    with pytest.raises(UnknownVerb):
        dispatch("no_such_verb", 10_000, _ctx())


def test_calltx_touches_existing_accounts_only() -> None:
    ctx = _ctx()
    txs = dispatch("calltx", deadline_bytes=15_000, context=ctx)
    assert txs
    for tx in txs:
        assert tx.fields.get("call_touch_only") is True


def test_refuses_lab_key_on_sepolia() -> None:
    """H-1: lab key is only allowed on {1337, 31337}; all testnets/mainnets must refuse."""
    with pytest.raises(ValueError, match="refusing to construct"):
        FacadeContext(base_address=b"\x00" * 20, revision=0, chain_id=11155111)


def test_refuses_lab_key_on_holesky() -> None:
    with pytest.raises(ValueError, match="refusing to construct"):
        FacadeContext(base_address=b"\x00" * 20, revision=0, chain_id=17000)


def test_refuses_lab_key_on_mainnet() -> None:
    with pytest.raises(ValueError, match="refusing to construct"):
        FacadeContext(base_address=b"\x00" * 20, revision=0, chain_id=1)


def test_accepts_lab_key_on_allowed_chain_ids() -> None:
    for chain_id in (1337, 31337):
        ctx = FacadeContext(base_address=b"\x00" * 20, revision=0, chain_id=chain_id)
        assert ctx.chain_id == chain_id


def test_context_repr_does_not_leak_private_key() -> None:
    """L-3: repr must not expose deploy_private_key."""
    ctx = FacadeContext(base_address=b"\x00" * 20, revision=0)
    r = repr(ctx)
    assert "\\x11" not in r
    assert ctx.deploy_private_key.hex() not in r


def test_every_verb_returns_signed_tx() -> None:
    ctx = _ctx()
    for verb in EXPECTED_VERBS:
        txs = dispatch(verb, deadline_bytes=15_000, context=ctx)
        assert txs, f"{verb}: zero txs"
        assert isinstance(txs[0].rlp, (bytes, bytearray))
        assert len(txs[0].rlp) > 0
