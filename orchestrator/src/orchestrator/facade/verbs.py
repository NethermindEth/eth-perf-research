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

# Well-known byte constants reused by a couple of verbs.
_DEPLOY_RUNTIME = bytes.fromhex("60006000f3")
_DEPLOY_INIT = bytes.fromhex("6005600c60003960056000f3") + _DEPLOY_RUNTIME

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


def _deploy_data(_ctx: FacadeContext, _idx: int) -> bytes:
    return _DEPLOY_INIT


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


def _deploy_diag(_ctx: FacadeContext, _idx: int) -> dict[str, Any]:
    return {"is_deploy": True, "init_len": len(_DEPLOY_INIT)}


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
)


def build_adapter(spec: VerbSpec) -> Callable[[int, FacadeContext], list[SignedTransaction]]:
    """Factory: return the standard adapter closure for a verb spec."""

    def adapter(deadline_bytes: int, context: FacadeContext) -> list[SignedTransaction]:
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

        return pack_until_deadline(deadline_bytes, context, build_one, spec.name)

    adapter.__name__ = f"{spec.name}_adapter"
    return adapter
