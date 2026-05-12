"""Facade registry + per-verb tests."""

from __future__ import annotations

import pytest
import rlp

from orchestrator.facade import VERBS, UnknownVerb, dispatch
from orchestrator.facade.context import FacadeContext
from orchestrator.facade.verbs import SPAMOOR_PLACEHOLDERS, SSTORE_TO_ZERO_MARKER

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
    "noop",
}


def _ctx() -> FacadeContext:
    return FacadeContext(
        base_address=(0x10_00_00).to_bytes(20, "big"),
        revision=0,
        address_cursor=0,
        chain_id=1337,
        deploy_private_key=b"\x42" * 32,
    )


def test_registry_has_all_13_verbs() -> None:
    """12 original scenarios + the no-op (self-transfer, mathematical zero)."""
    assert set(VERBS.keys()) == EXPECTED_VERBS
    assert len(VERBS) == 13


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


def test_storagerefundtx_uses_eels_execute_selector() -> None:
    """storagerefundtx now dispatches the EELS ``execute(uint256)`` selector
    ``0xfe0d94c1`` against the prealloc'd placeholder address, with
    ``slots_per_call=0`` so the trailing 32-byte zero word still contains the
    legacy SSTORE→0 marker for downstream tooling that greps the RLP.
    """
    ctx = _ctx()
    txs = dispatch("storagerefundtx", deadline_bytes=20_000, context=ctx)
    assert txs
    decoded = rlp.decode(txs[0].rlp[1:])
    data = decoded[7]
    # EELS' execute(uint256) selector — keccak("execute(uint256)")[:4]
    assert data[:4] == bytes.fromhex("fe0d94c1")
    # 32-byte zero arg (slots_per_call=0) keeps the SSTORE→0 marker inside.
    assert SSTORE_TO_ZERO_MARKER[1:] in data  # 32 zero bytes are present


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


def test_gas_aware_dispatcher_caps_storagespam(monkeypatch) -> None:
    """When ORCH_GAS_AWARE_DISPATCH=1 and block_gas_limit is small, the
    dispatcher returns ≤ ⌊0.95·block / per_tx_gas⌋ txs even if the byte
    budget would allow more."""
    # Re-import facade with the env flag set so the module-level constant
    # picks it up. Reload to flip GAS_AWARE_DISPATCH.
    monkeypatch.setenv("ORCH_GAS_AWARE_DISPATCH", "1")
    import importlib

    import orchestrator.facade as facade_mod

    facade_mod = importlib.reload(facade_mod)

    ctx = FacadeContext(
        base_address=(0x10_00_00).to_bytes(20, "big"),
        revision=0,
        chain_id=1337,
        deploy_private_key=b"\x42" * 32,
        block_gas_limit=30_000_000,
    )
    # storagespam.gas = 2_000_000 → cap = floor(30M*0.95 / 2M) = 14
    txs = facade_mod.dispatch("storagespam", deadline_bytes=10_000_000, context=ctx)
    assert 1 <= len(txs) <= 14, f"got {len(txs)} txs, expected ≤14 under gas cap"


def test_noop_verb_self_transfer() -> None:
    """noop verb must produce a value=0 self-transfer (sender == recipient)."""
    ctx = _ctx()
    txs = dispatch("noop", deadline_bytes=5_000, context=ctx)
    assert txs, "noop: zero txs"
    for tx in txs:
        assert tx.kind == "noop"


# ---------------------------------------------------------------------------
# Per-verb EELS dispatch tests. Each asserts:
#   1. The signed tx is type-2 (EIP-1559).
#   2. The ``to`` field in the decoded RLP matches ``SPAMOOR_PLACEHOLDERS[v]``.
#   3. The leading 4 bytes of calldata match the selector EELS produces.
# ---------------------------------------------------------------------------

# selector → bytes(keccak(sig)[:4]) for each EELS-backed verb.
EELS_SELECTORS: dict[str, bytes | None] = {
    "calltx": bytes.fromhex("00000000"),  # opaque 4-byte selector
    "factorydeploytx": bytes.fromhex("4c8c9ea1"),  # deploy(bytes32,bytes)
    "gasburnertx": None,  # 4-byte big-endian txIdx, value-dependent
    "storagespam": bytes.fromhex("fed72935"),  # setRandomForGas(uint256,uint256)
    "erc20_bloater": bytes.fromhex("c1926de5"),  # bloatStorage(uint256,uint256)
    "erc20tx": bytes.fromhex("9d0f7cba"),  # transferMint(address,uint256)
    "uniswap_swaps": None,  # variant-dependent: 38ed1739 / 7ff36ab5 / 18cbafe5
    "storagerefundtx": bytes.fromhex("fe0d94c1"),  # execute(uint256)
}


def _decoded_to_field(rlp_bytes: bytes) -> bytes:
    """Return the ``to`` field from a signed type-2 tx RLP."""
    decoded = rlp.decode(rlp_bytes[1:])
    # EIP-1559 layout:
    # [chainId, nonce, maxPrio, maxFee, gas, to, value, data, accessList, v, r, s]
    return decoded[5]


def _decoded_data_field(rlp_bytes: bytes) -> bytes:
    decoded = rlp.decode(rlp_bytes[1:])
    return decoded[7]


@pytest.mark.parametrize("verb", sorted(SPAMOOR_PLACEHOLDERS))
def test_eels_dispatched_to_matches_placeholder(verb: str) -> None:
    ctx = _ctx()
    txs = dispatch(verb, deadline_bytes=5_000, context=ctx)
    assert txs, f"{verb}: zero txs"
    first = txs[0]
    # Type-2 envelope.
    assert first.rlp[0] == 0x02, f"{verb}: not EIP-1559"
    expected_to = bytes.fromhex(SPAMOOR_PLACEHOLDERS[verb][2:])
    assert _decoded_to_field(first.rlp) == expected_to, (
        f"{verb}: to mismatch — expected {expected_to.hex()}"
    )


@pytest.mark.parametrize(
    "verb,selector",
    [(v, s) for v, s in EELS_SELECTORS.items() if s is not None],
)
def test_eels_dispatched_selector_matches(verb: str, selector: bytes) -> None:
    ctx = _ctx()
    txs = dispatch(verb, deadline_bytes=5_000, context=ctx)
    assert txs
    data = _decoded_data_field(txs[0].rlp)
    assert data[: len(selector)] == selector, (
        f"{verb}: leading selector {data[: len(selector)].hex()} != {selector.hex()}"
    )


def test_uniswap_swaps_uses_one_of_three_router_selectors() -> None:
    """Uniswap router calls cycle through three selectors; assert the first
    tx leads with one of them."""
    ctx = _ctx()
    txs = dispatch("uniswap_swaps", deadline_bytes=5_000, context=ctx)
    assert txs
    data = _decoded_data_field(txs[0].rlp)
    accepted = (
        bytes.fromhex("38ed1739"),  # swapExactTokensForTokens
        bytes.fromhex("7ff36ab5"),  # swapExactETHForTokens
        bytes.fromhex("18cbafe5"),  # swapExactTokensForETH
    )
    assert data[:4] in accepted, f"unexpected uniswap selector {data[:4].hex()}"


def test_eoatx_targets_fresh_derived_addresses() -> None:
    """We override EELS' burn-address default with ``ctx.derive_address(idx)``
    so every eoatx writes a fresh leaf into the account trie. Without this
    the controller can't grow the accounts axis (every tx would update the
    same 0x0 leaf in storage, not create a new account)."""
    ctx = _ctx()
    txs = dispatch("eoatx", deadline_bytes=5_000, context=ctx)
    assert txs
    seen: set[bytes] = set()
    for tx in txs:
        to = _decoded_to_field(tx.rlp)
        assert to != b"\x00" * 20, "eoatx must not target the burn address"
        assert to not in seen, "eoatx recipients must be unique per tx"
        seen.add(to)
        assert tx.fields.get("fresh_account") is True


def test_factorydeploytx_skips_factory_deployment() -> None:
    """With ``factory_address`` non-empty, EELS skips the leading deploy tx;
    every emitted tx already targets the placeholder factory."""
    ctx = _ctx()
    txs = dispatch("factorydeploytx", deadline_bytes=20_000, context=ctx)
    assert txs
    factory = bytes.fromhex(SPAMOOR_PLACEHOLDERS["factorydeploytx"][2:])
    for tx in txs:
        assert _decoded_to_field(tx.rlp) == factory, (
            "factorydeploytx must never emit a CREATE tx in reuse mode"
        )


def test_gasburnertx_skips_leading_deploy() -> None:
    """EELS' gasburner builder always emits ``[deploy, exec, exec, ...]``;
    the verb adapter must slice off the deploy and only emit the exec."""
    ctx = _ctx()
    txs = dispatch("gasburnertx", deadline_bytes=5_000, context=ctx)
    assert txs
    target = bytes.fromhex(SPAMOOR_PLACEHOLDERS["gasburnertx"][2:])
    for tx in txs:
        # `to` must equal the placeholder, never the empty CREATE addr.
        assert _decoded_to_field(tx.rlp) == target
