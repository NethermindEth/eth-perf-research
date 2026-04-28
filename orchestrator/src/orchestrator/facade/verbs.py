"""Data-driven verb registry — one spec per scenario (replaces 12 near-identical modules).

Every verb follows the same shape: derive a ``to`` address, build calldata, pick a gas
limit, and tag the resulting tx with a diagnostic dict. A ``VerbSpec`` table captures
those four bits; the adapter factory in ``__init__`` binds them into the standard
``(deadline_bytes, context) -> list[SignedTransaction]`` signature.

When the upstream EELS port (``execution-specs feat/spamoor-to-est``) becomes
pip-installable each entry's ``make_data`` / ``make_to`` is a drop-in replacement for the
upstream helper with no change to the registry's shape.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

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

_FACTORY_CALL_PREFIX = b"\xff" * 4
_ERC20_TRANSFER_SELECTOR = bytes.fromhex("a9059cbb")  # keccak("transfer(address,uint256)")[:4]
_STORAGESPAM_SELECTOR = bytes.fromhex("8c8e4f53")
_SWAP_SELECTOR = bytes.fromhex("128acb08")
_CLEAR_SELECTOR = bytes.fromhex("45b5a4a0")
_BURN_SELECTOR = bytes.fromhex("8b9a4f53")

# Marker sequence used by storagerefundtx to make "SSTORE→0" intent visible in tests
# without having to disassemble EVM bytecode. Kept exported for back-compat.
SSTORE_TO_ZERO_MARKER = bytes.fromhex("55" + "00" * 32)


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


# ---------------------------------------------------------------------------
# Helpers used by multiple verbs. Defined as lambdas below for locality.


def _eoatx_to(ctx: FacadeContext, idx: int) -> bytes:
    return ctx.derive_address(idx)


def _self_to(ctx: FacadeContext, _idx: int) -> bytes:
    """Self-transfer recipient — used by the no-op verb.

    Returns the deploy account's own 20-byte address. Sending value=0 to
    self with no calldata costs only the 21 K intrinsic gas, touches only
    the sender's nonce, and produces ~zero state-trie growth (encoding a
    larger nonce can shift a leaf by one byte but typically doesn't). This
    gives the controller a true mathematical "do nothing" verb so the
    simplex projector has a zero in its action space.
    """
    addr_hex = ctx.account.address.lower().removeprefix("0x")
    return bytes.fromhex(addr_hex)


def _calltx_to(ctx: FacadeContext, idx: int) -> bytes:
    # Touch a previously-created address (idx-1) so we don't create new state.
    return ctx.derive_address(max(0, idx - 1))


def _deploy_to(_ctx: FacadeContext, _idx: int) -> None:
    return None  # CREATE tx — `to` must be absent.


def _zero_to(ctx: FacadeContext, _idx: int) -> bytes:
    return ctx.derive_address(0)


def _pool_to(ctx: FacadeContext, _idx: int) -> bytes:
    return ctx.derive_address(1)


def _noop_data(_ctx: FacadeContext, _idx: int) -> bytes:
    return b""


def _calltx_data(_ctx: FacadeContext, _idx: int) -> bytes:
    return b"\x00" * 4  # bare 4-byte selector


def _deploy_data(_ctx: FacadeContext, idx: int) -> bytes:
    return _build_unique_storage_burner_init(idx)


def _factory_data(ctx: FacadeContext, _idx: int) -> bytes:
    return _FACTORY_CALL_PREFIX + ctx.next_salt()


def _storagespam_data(_ctx: FacadeContext, idx: int) -> bytes:
    return _STORAGESPAM_SELECTOR + idx.to_bytes(32, "big") + (16).to_bytes(32, "big")


def _erc20_bloater_data(ctx: FacadeContext, idx: int) -> bytes:
    recipient = ctx.derive_address(idx + 1_000_000)
    return _ERC20_TRANSFER_SELECTOR + b"\x00" * 12 + recipient + (1).to_bytes(32, "big")


def _erc20tx_data(ctx: FacadeContext, idx: int) -> bytes:
    recipient = ctx.derive_address((idx % 64) + 1)
    return _ERC20_TRANSFER_SELECTOR + b"\x00" * 12 + recipient + (1).to_bytes(32, "big")


def _swap_data(_ctx: FacadeContext, idx: int) -> bytes:
    return _SWAP_SELECTOR + idx.to_bytes(32, "big") + (1).to_bytes(32, "big")


def _storagerefund_data(_ctx: FacadeContext, idx: int) -> bytes:
    return _CLEAR_SELECTOR + idx.to_bytes(32, "big") + SSTORE_TO_ZERO_MARKER


def _gasburner_data(_ctx: FacadeContext, _idx: int) -> bytes:
    return _BURN_SELECTOR + (100_000).to_bytes(32, "big")


def _blob_data(_ctx: FacadeContext, _idx: int) -> bytes:
    return b"\xff" * 32


def _fuzz_data(_ctx: FacadeContext, idx: int) -> bytes:
    return ((idx * 0x9E3779B97F4A7C15) & ((1 << 256) - 1)).to_bytes(32, "big")


def _erc20_bloater_diag(ctx: FacadeContext, idx: int) -> dict[str, Any]:
    recipient = ctx.derive_address(idx + 1_000_000)
    return {"erc20_recipient": "0x" + recipient.hex()}


def _erc20tx_diag(ctx: FacadeContext, idx: int) -> dict[str, Any]:
    recipient = ctx.derive_address((idx % 64) + 1)
    return {"erc20_recipient": "0x" + recipient.hex(), "churn_only": True}


def _calltx_diag(ctx: FacadeContext, idx: int) -> dict[str, Any]:
    to = ctx.derive_address(max(0, idx - 1))
    return {"to": "0x" + to.hex(), "call_touch_only": True}


def _eoatx_diag(ctx: FacadeContext, idx: int) -> dict[str, Any]:
    to = ctx.derive_address(idx)
    return {"to": "0x" + to.hex(), "value": 1}


def _deploy_diag(_ctx: FacadeContext, idx: int) -> dict[str, Any]:
    return {"is_deploy": True, "init_len": len(_build_unique_storage_burner_init(idx))}


def _factory_diag(ctx: FacadeContext, _idx: int) -> dict[str, Any]:
    # `next_salt` has already advanced by the time diag runs; reconstruct the prior.
    prior_salt = (ctx.salt_cursor - 1).to_bytes(32, "big") if ctx.salt_cursor > 0 else b"\x00" * 32
    return {"is_factory_deploy": True, "salt": prior_salt.hex()}


def _swap_diag(ctx: FacadeContext, _idx: int) -> dict[str, Any]:
    return {"swap_pool": "0x" + ctx.derive_address(1).hex()}


def _storagerefund_diag(_ctx: FacadeContext, idx: int) -> dict[str, Any]:
    return {"refund_start_slot": idx}


def _storagespam_diag(_ctx: FacadeContext, idx: int) -> dict[str, Any]:
    return {"slots_written": 16, "start_slot": idx}


def _blob_diag(_ctx: FacadeContext, _idx: int) -> dict[str, Any]:
    return {"blob_tx_placeholder": True, "blob_count": 3}


def _fuzz_diag(_ctx: FacadeContext, idx: int) -> dict[str, Any]:
    return {"fuzz_seed": idx}


def _gasburner_diag(_ctx: FacadeContext, _idx: int) -> dict[str, Any]:
    return {"compute_only": True}


# Tuple (not list) so ``VERB_SPECS.append(...)`` can't silently extend the registry
# at runtime. Treat the set of verbs as a compile-time property of the package.
VERB_SPECS: tuple[VerbSpec, ...] = (
    VerbSpec("eoatx", 21_000, _eoatx_to, _noop_data, _eoatx_diag, value=1),
    VerbSpec("calltx", 40_000, _calltx_to, _calltx_data, _calltx_diag),
    VerbSpec("deploytx", 300_000, _deploy_to, _deploy_data, _deploy_diag),
    VerbSpec(
        "factorydeploytx",
        400_000,
        _zero_to,
        _factory_data,
        _factory_diag,
        advance_salt=True,
    ),
    VerbSpec("storagespam", 2_000_000, _zero_to, _storagespam_data, _storagespam_diag),
    VerbSpec("erc20_bloater", 80_000, _zero_to, _erc20_bloater_data, _erc20_bloater_diag),
    VerbSpec("erc20tx", 60_000, _zero_to, _erc20tx_data, _erc20tx_diag),
    VerbSpec("uniswap_swaps", 250_000, _pool_to, _swap_data, _swap_diag),
    VerbSpec("storagerefundtx", 80_000, _zero_to, _storagerefund_data, _storagerefund_diag),
    VerbSpec("gasburnertx", 1_500_000, _zero_to, _gasburner_data, _gasburner_diag),
    VerbSpec("blob_combined", 200_000, _zero_to, _blob_data, _blob_diag),
    VerbSpec("evm_fuzz", 300_000, _zero_to, _fuzz_data, _fuzz_diag),
    # No-op: self-transfer with value=0, intrinsic gas only, ~zero state delta.
    # Gives the controller a "wait" move whose F vector is essentially (0, 0, 0)
    # so the simplex projector can sit on the boundary of the simplex once we
    # approach target. Combined with ORCH_RESIDUAL_STOP_THRESHOLD this is what
    # makes "stop early when there's nothing useful to do" work.
    VerbSpec("noop", 21_000, _self_to, _noop_data),
)


def build_adapter(spec: VerbSpec) -> Callable[..., list[SignedTransaction]]:
    """Factory: return the standard adapter closure for a verb spec."""

    def adapter(
        deadline_bytes: int,
        context: FacadeContext,
        *,
        gas_budget: int | None = None,
    ) -> list[SignedTransaction]:
        def build_one(index: int, ctx: FacadeContext) -> tuple[dict[str, Any], dict[str, Any]]:
            to = spec.make_to(ctx, index)
            data = spec.make_data(ctx, index)
            signable = {
                "type": 2,
                "nonce": index,
                "to": to,
                "value": spec.value,
                "gas": spec.gas,
                "data": data,
            }
            diag = spec.extra_diag(ctx, index)
            return signable, diag

        return pack_until_deadline(
            deadline_bytes, context, build_one, spec.name, gas_budget=gas_budget,
        )

    adapter.__name__ = f"{spec.name}_adapter"
    return adapter
