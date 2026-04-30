"""Data-driven verb registry — one spec per scenario.

Each EELS-backed verb routes through ``spamoor_builders.build_<verb>_transactions``
with ``count=1`` and ``reuse_contract=True`` (or an equivalent ``contract_address``
override) so every emitted tx targets the address pre-funded in lab-genesis.
The orchestrator's signing path (``_builder._sign`` + ``context.base_tx_fields``)
strips the EELS-supplied chain/fee fields and re-injects the canonical lab values.

``noop`` keeps a local self-transfer (no EELS analog; used by the controller's
residual-stop logic). ``blob_combined`` keeps a local stub: type-3 broadcast needs
KZG sidecars that the orchestrator's ``_sign`` path doesn't currently produce —
deferred to a follow-up.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

import eels_spamoor_builders as sb
from ._builder import pack_until_deadline
from .context import FacadeContext, SignedTransaction

# Storage-burner runtime: SLOAD slot 0 (counter), then 16 unrolled SSTOREs at
# (counter+1..counter+16), then SSTORE slot 0 = counter+16, STOP. 104 bytes.
# Mirrors the contract prefunded at derive_address(0) in the lab chainspec
# so a deployed contract is functionally identical (and call-compatible
# with the storage-spam selectors).
_STORAGE_BURNER_RUNTIME = bytes.fromhex(
    "6000546001018080556001018080556001018080556001018080556001018080"
    "556001018080556001018080556001018080556001018080556001018080556001"
    "018080556001018080556001018080556001018080556001018080556001018080"
    "558060005500"
)


def _build_unique_storage_burner_init(idx: int) -> bytes:
    """Return CREATE-tx initcode that deploys a per-idx unique storage burner.

    Prefixed with ``PUSH32 idx; POP`` so each deploy emits a distinct codehash.
    The plugin's ``codeBytesTotal`` is deduplicated by hash, so without a unique
    tag every deploytx would collapse onto a single hash and the code axis
    wouldn't grow proportionally.
    """
    unique_tag = b"\x7f" + idx.to_bytes(32, "big") + b"\x50"  # PUSH32 idx; POP
    runtime = unique_tag + _STORAGE_BURNER_RUNTIME
    length = len(runtime)
    if length > 0xFFFF:
        raise ValueError(f"runtime too large for PUSH2 length: {length}")
    # PUSH2 length; PUSH1 14 (= len(init_prefix)); PUSH1 0; CODECOPY;
    # PUSH2 length; PUSH1 0; RETURN
    init_prefix = bytes([
        0x61, (length >> 8) & 0xFF, length & 0xFF,
        0x60, 0x0E,
        0x60, 0x00,
        0x39,
        0x61, (length >> 8) & 0xFF, length & 0xFF,
        0x60, 0x00,
        0xF3,
    ])
    assert len(init_prefix) == 14
    return init_prefix + runtime


# Marker sequence used by storagerefundtx to make "SSTORE→0" intent visible in
# tests without having to disassemble EVM bytecode. Kept exported for
# back-compat: 0x55 (SSTORE opcode) followed by 32 zero bytes.
# EELS' storagerefundtx selector ``fe0d94c1`` + 32-byte zero slot count produces
# the same ``55 || 0x00*32`` byte sequence inside calldata, so the marker
# remains discoverable in the signed RLP.
SSTORE_TO_ZERO_MARKER = bytes.fromhex("55" + "00" * 32)


# ----------------------------------------------------------------------------
# Placeholder addresses pre-deployed in lab-genesis.json. Each EELS builder
# targets one of these when invoked with ``reuse_contract=True`` (or, for
# factorydeploytx, with a non-empty ``factory_address``).
# ----------------------------------------------------------------------------

SPAMOOR_PLACEHOLDERS: dict[str, str] = {
    "calltx": "0x1111111111111111111111111111111111111111",
    "factorydeploytx": "0x2222222222222222222222222222222222222222",
    "gasburnertx": "0x3333333333333333333333333333333333333333",
    "uniswap_swaps": "0x4444444444444444444444444444444444444444",
    "erc20tx": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "storagespam": "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "erc20_bloater": "0xdddddddddddddddddddddddddddddddddddddddd",
    "storagerefundtx": "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
}


MakeTo = Callable[[FacadeContext, int], bytes | None]
MakeData = Callable[[FacadeContext, int], bytes]
ExtraDiag = Callable[[FacadeContext, int], dict[str, Any]]


@dataclass(frozen=True)
class VerbSpec:
    """All the per-verb variation collapsed into a data record."""

    name: str
    gas: int
    make_to: MakeTo
    make_data: MakeData
    extra_diag: ExtraDiag = field(default=lambda _ctx, _idx: {})
    advance_salt: bool = False
    value: int = 0


# ----------------------------------------------------------------------------
# Local helpers retained for ``noop`` (and tests).
# ----------------------------------------------------------------------------


def _self_to(ctx: FacadeContext, _idx: int) -> bytes:
    """Self-transfer recipient — used by the no-op verb."""
    addr_hex = ctx.account.address.lower().removeprefix("0x")
    return bytes.fromhex(addr_hex)


def _noop_data(_ctx: FacadeContext, _idx: int) -> bytes:
    return b""


def _blob_to(_ctx: FacadeContext, _idx: int) -> bytes:
    return bytes.fromhex("1000000000000000000000000000000000000000")


def _blob_data(_ctx: FacadeContext, _idx: int) -> bytes:
    # Stub payload until type-3 + KZG sidecars are wired through ``_sign``.
    return b"\xff" * 32


# ----------------------------------------------------------------------------
# EELS-backed builders. Each returns ``(signable, diag)`` for ``build_one``.
# We always invoke EELS with ``count=1`` and the placeholder target so a single
# call yields exactly one execution tx; the dispatch loop in
# ``pack_until_deadline`` already drives multiple calls in sequence.
# ----------------------------------------------------------------------------


_BASE_FIELDS_TO_STRIP = ("chainId", "maxFeePerGas", "maxPriorityFeePerGas", "type", "accessList")


def _to_signable(eels_tx: dict[str, Any], idx: int, gas: int) -> dict[str, Any]:
    """Drop fields the orchestrator's ``base_tx_fields`` already injects.

    EELS' builders pin ``chainId=1`` and a builder-internal fee schedule;
    the orchestrator overrides both via ``FacadeContext.base_tx_fields()``.
    Rather than mutate the EELS dict in place, copy + filter so a future
    re-sync of ``spamoor_builders.py`` doesn't surprise us.
    """
    out: dict[str, Any] = {
        k: v for k, v in eels_tx.items() if k not in _BASE_FIELDS_TO_STRIP
    }
    out["nonce"] = idx
    out["gas"] = gas
    return out


def _eoatx_build(ctx: FacadeContext, idx: int) -> tuple[dict[str, Any], dict[str, Any]]:
    """Value-1 EOA transfer to a fresh derived address (creates one new account per tx).

    EELS' ``build_eoatx_transactions`` hard-codes ``to=0x0…0`` (burn address) which
    matches Spamoor's CLI default. That makes the tx mechanically valid but it
    doesn't grow the account-trie axis the controller needs to balance against
    storage + code. We keep the EELS calldata/fee shape and just override the
    recipient with ``ctx.derive_address(idx)`` so each tx writes a fresh leaf.
    """
    txs = sb.build_eoatx_transactions(count=1, throughput=1.0, amount=1)
    signable = _to_signable(txs[0], idx, gas=21_000)
    fresh = ctx.derive_address(idx)
    signable["to"] = "0x" + fresh.hex()
    signable["value"] = 1
    return signable, {"to": signable["to"], "value": 1, "fresh_account": True}


def _calltx_build(_ctx: FacadeContext, idx: int) -> tuple[dict[str, Any], dict[str, Any]]:
    target = SPAMOOR_PLACEHOLDERS["calltx"]
    txs = sb.build_calltx_transactions(
        count=1,
        throughput=1.0,
        contract_address=target,
        call_data="0x00000000",
        gas_limit=40_000,
    )
    signable = _to_signable(txs[0], idx, gas=40_000)
    return signable, {"to": target, "call_touch_only": True}


def _deploytx_build(_ctx: FacadeContext, idx: int) -> tuple[dict[str, Any], dict[str, Any]]:
    """Deploy a per-idx unique storage-burner so codehashes don't dedupe.

    EELS' ``build_deploytx_transactions`` cycles a fixed bytecode list, which
    would collapse to a single codehash across the run. We keep the local
    unique-init builder so the plugin's ``codeBytesTotal`` axis grows.
    """
    init_code = _build_unique_storage_burner_init(idx)
    signable = {
        "type": 2,
        "nonce": idx,
        "to": None,
        "value": 0,
        "gas": 300_000,
        "data": init_code,
    }
    return signable, {"is_deploy": True, "init_len": len(init_code)}


def _factorydeploytx_build(
    ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    factory = SPAMOOR_PLACEHOLDERS["factorydeploytx"]
    salt = ctx.salt_cursor
    # ``factory_address`` non-empty → builder skips the leading deploy tx.
    txs = sb.build_factorydeploytx_transactions(
        count=1,
        init_code="0x6001600055",
        start_salt=salt,
        factory_address=factory,
        gas_limit=400_000,
    )
    signable = _to_signable(txs[0], idx, gas=400_000)
    ctx.salt_cursor += 1
    return signable, {
        "is_factory_deploy": True,
        "salt": salt.to_bytes(32, "big").hex(),
    }


def _storagespam_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    target = SPAMOOR_PLACEHOLDERS["storagespam"]
    txs = sb.build_storagespam_transactions(
        count=1,
        gas_units_to_burn=1_950_000,
        reuse_contract=True,
        contract_address=target,
    )
    signable = _to_signable(txs[0], idx, gas=2_000_000)
    return signable, {"slots_written": 16, "start_slot": idx}


def _erc20_bloater_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    target = SPAMOOR_PLACEHOLDERS["erc20_bloater"]
    txs = sb.build_erc20_bloater_transactions(
        count=1,
        addresses_per_tx=370,
        start_address_index=1 + idx * 370,
        gas_limit=80_000,
        contract_address=target,
    )
    signable = _to_signable(txs[0], idx, gas=80_000)
    return signable, {"erc20_recipient": target}


def _erc20tx_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    target = SPAMOOR_PLACEHOLDERS["erc20tx"]
    txs = sb.build_erc20tx_transactions(
        count=1,
        contract_address=target,
        gas_limit=60_000,
    )
    signable = _to_signable(txs[0], idx, gas=60_000)
    return signable, {"erc20_recipient": target, "churn_only": True}


def _uniswap_swaps_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    router = SPAMOOR_PLACEHOLDERS["uniswap_swaps"]
    txs = sb.build_uniswap_swaps_transactions(
        count=1,
        gas_limit=250_000,
        router_address=router,
    )
    signable = _to_signable(txs[0], idx, gas=250_000)
    return signable, {"swap_pool": router}


def _storagerefundtx_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    target = SPAMOOR_PLACEHOLDERS["storagerefundtx"]
    txs = sb.build_storagerefundtx_transactions(
        count=1,
        slots_per_call=0,  # produces zero word -> SSTORE→0 marker survives
        contract_address=target,
        gas_limit=80_000,
    )
    signable = _to_signable(txs[0], idx, gas=80_000)
    return signable, {"refund_start_slot": idx}


def _gasburnertx_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    """EELS' gasburner builder always emits a leading deploy tx; slice it off.

    With ``contract_address`` set, the second tx targets the placeholder.
    We use ``count=1`` so the result is exactly ``[deploy, exec]`` and we
    keep the exec.
    """
    target = SPAMOOR_PLACEHOLDERS["gasburnertx"]
    txs = sb.build_gasburnertx_transactions(
        count=1,
        gas_units_to_burn=1_500_000,
        contract_address=target,
    )
    # txs == [deploy_tx, exec_tx]; we want the exec (idx 1).
    exec_tx = txs[1]
    signable = _to_signable(exec_tx, idx, gas=1_500_000)
    return signable, {"compute_only": True}


def _evm_fuzz_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    txs = sb.build_evm_fuzz_transactions(
        count=1,
        gas_limit=300_000,
        tx_id_offset=idx,
    )
    signable = _to_signable(txs[0], idx, gas=300_000)
    return signable, {"fuzz_seed": idx}


def _blob_combined_build(
    _ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Local type-2 stub: real EELS blob_combined produces type-3 txs which
    require KZG sidecars not yet handled by the orchestrator's ``_sign``.
    """
    signable = {
        "type": 2,
        "nonce": idx,
        "to": _blob_to(_ctx, idx),
        "value": 0,
        "gas": 200_000,
        "data": _blob_data(_ctx, idx),
    }
    return signable, {"blob_tx_placeholder": True, "blob_count": 3}


def _noop_build(
    ctx: FacadeContext, idx: int
) -> tuple[dict[str, Any], dict[str, Any]]:
    signable = {
        "type": 2,
        "nonce": idx,
        "to": _self_to(ctx, idx),
        "value": 0,
        "gas": 21_000,
        "data": _noop_data(ctx, idx),
    }
    return signable, {}


# Map verb name -> builder. Each builder returns ``(signable, diag)``.
_VERB_BUILDERS: dict[str, Callable[[FacadeContext, int], tuple[dict[str, Any], dict[str, Any]]]] = {
    "eoatx": _eoatx_build,
    "calltx": _calltx_build,
    "deploytx": _deploytx_build,
    "factorydeploytx": _factorydeploytx_build,
    "storagespam": _storagespam_build,
    "erc20_bloater": _erc20_bloater_build,
    "erc20tx": _erc20tx_build,
    "uniswap_swaps": _uniswap_swaps_build,
    "storagerefundtx": _storagerefundtx_build,
    "gasburnertx": _gasburnertx_build,
    "evm_fuzz": _evm_fuzz_build,
    "blob_combined": _blob_combined_build,
    "noop": _noop_build,
}


# Per-verb gas hints used by the dispatcher's gas-aware cap. Must match the
# ``gas`` value the corresponding builder writes into ``signable``.
_VERB_GAS: dict[str, int] = {
    "eoatx": 21_000,
    "calltx": 40_000,
    "deploytx": 300_000,
    "factorydeploytx": 400_000,
    "storagespam": 2_000_000,
    "erc20_bloater": 80_000,
    "erc20tx": 60_000,
    "uniswap_swaps": 250_000,
    "storagerefundtx": 80_000,
    "gasburnertx": 1_500_000,
    "evm_fuzz": 300_000,
    "blob_combined": 200_000,
    "noop": 21_000,
}


# Tuple (not list) so ``VERB_SPECS.append(...)`` can't silently extend the registry
# at runtime. Treat the set of verbs as a compile-time property of the package.
VERB_SPECS: tuple[VerbSpec, ...] = tuple(
    VerbSpec(
        name=name,
        gas=_VERB_GAS[name],
        make_to=lambda _c, _i: None,  # unused; build_one drives EELS directly
        make_data=lambda _c, _i: b"",  # unused; ditto
        advance_salt=(name == "factorydeploytx"),
    )
    for name in (
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
    )
)


def build_adapter(spec: VerbSpec) -> Callable[..., list[SignedTransaction]]:
    """Factory: return the standard adapter closure for a verb spec."""
    builder = _VERB_BUILDERS[spec.name]

    def adapter(
        deadline_bytes: int,
        context: FacadeContext,
        *,
        gas_budget: int | None = None,
    ) -> list[SignedTransaction]:
        def build_one(index: int, ctx: FacadeContext) -> tuple[dict[str, Any], dict[str, Any]]:
            return builder(ctx, index)

        return pack_until_deadline(
            deadline_bytes, context, build_one, spec.name, gas_budget=gas_budget,
        )

    adapter.__name__ = f"{spec.name}_adapter"
    return adapter
