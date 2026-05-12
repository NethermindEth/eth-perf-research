"""Shared facade context: cursors + chain metadata passed to every adapter."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from typing import Any

from eth_account import Account
from eth_account.signers.local import LocalAccount

# A deterministic, well-known key used by tests and by lab runs. Because its bytes
# are checked into this repository, it must only ever sign transactions on
# lab/devnet chain ids. We enforce that with an *allowlist* (not a blocklist):
# adding a new L2 testnet to Ethereum must not silently expose the lab key.
_LAB_PRIVATE_KEY = b"\x11" * 32
LAB_ALLOWED_CHAIN_IDS = frozenset({1337, 31337})
_ADDRESS_SPACE = 1 << 160


@dataclass(slots=True)
class SignedTransaction:
    """Wrapped signed tx — carries both RLP bytes (for size/commit) and metadata.

    Adapters return these; the orchestrator takes `rlp` for `testing_commitBlockV1` and
    reads `fields` for diagnostics. `kind` records the verb that produced the tx so
    downstream tooling can filter without re-decoding. ``slots=True`` keeps the
    per-tx footprint small on the hot loop (100s of instances per batch).
    """

    rlp: bytes
    kind: str
    fields: dict[str, Any] = field(default_factory=dict)
    gas_limit: int = 0


@dataclass
class FacadeContext:
    """Mutable cursor state carried across batches.

    Addresses are derived sequentially as `base_address + (revision * stride + address_cursor)`,
    big-endian-packed into 20 bytes. Salts (for CREATE2) share the same monotonic cursor but
    are tracked separately so deploy and factory-deploy verbs don't collide.

    ``deploy_private_key`` defaults to the lab test key; production use requires the
    caller to pass an explicit key or accept a refusal if ``chain_id`` is mainnet-like.
    """

    base_address: bytes
    revision: int
    address_cursor: int = 0
    salt_cursor: int = 0
    genesis_sha256: bytes = b""
    chain_id: int = 1337
    gas_limit: int = 30_000_000
    # Block-level gas ceiling used by the dispatcher to cap cumulative gas
    # across a single batch (so e.g. 200×storagespam at 2 M gas/tx can't
    # request 400 M gas against a 30 M block). Sourced from
    # ``target.yaml:block_gas_limit`` if present; falls back to the per-tx
    # ``gas_limit``. Read in ``pack_until_deadline`` (not signed onto txs).
    block_gas_limit: int = 30_000_000
    # Cache of the last block's timestamp so commit_and_journal can floor at
    # ``parent.timestamp + 1``. The Engine API enforces strictly increasing
    # timestamps; rapid commits (probe phase fires 7 blocks within one second)
    # would otherwise share a wall-clock timestamp and be rejected by
    # geth/besu/reth on cross-client replay.
    last_block_timestamp: int = 0
    # Per-verb EWMA of (block.gas_used / sum(tx.gas_limit)). Populated by
    # ``lifecycle.update_verb_gas_factor``; consumed by ``pack_until_deadline``.
    # Defaults to 1.0 per verb until the first commit feeds back a real value.
    verb_gas_factors: dict[str, float] = field(default_factory=dict)
    # Sliding window of recent block-apply latencies in seconds. Drives the
    # back-pressure check (`lifecycle.under_back_pressure`) — when NM's pruner
    # falls behind, blocks take longer; the orchestrator inserts noop batches
    # to give the pruner room to catch up.
    block_apply_latencies: list[float] = field(default_factory=list)
    # `repr=False` so `repr(ctx)` never leaks the private key into logs/tracebacks.
    deploy_private_key: bytes = field(default=_LAB_PRIVATE_KEY, repr=False)
    address_stride: int = 1 << 40
    # Cached LocalAccount reused across every signing call — avoids re-derivation
    # on the hot tx-generation path. Populated lazily on first `account` access.
    _account: LocalAccount | None = field(default=None, repr=False, compare=False)

    @property
    def account(self) -> LocalAccount:
        """Cached LocalAccount for this context's deploy key."""
        acct = self._account
        if acct is None:
            acct = Account.from_key(self.deploy_private_key)
            # Field is assigned via object.__setattr__ to work under frozen=True too.
            object.__setattr__(self, "_account", acct)
        return acct

    def __post_init__(self) -> None:
        if (
            self.deploy_private_key == _LAB_PRIVATE_KEY
            and self.chain_id not in LAB_ALLOWED_CHAIN_IDS
        ):
            raise ValueError(
                f"refusing to construct FacadeContext with the well-known lab key on "
                f"chain_id={self.chain_id}; allowed only for "
                f"{sorted(LAB_ALLOWED_CHAIN_IDS)}. Supply an explicit deploy_private_key."
            )
        if self.revision < 0 or self.address_stride <= 0:
            raise ValueError("revision must be >= 0 and address_stride > 0")
        # Guard against silent OverflowError in derive_address under pathological config.
        max_index_room = _ADDRESS_SPACE - self.revision * self.address_stride
        if max_index_room <= 0:
            raise ValueError(
                f"revision={self.revision} × stride={self.address_stride} "
                f"exceeds the 20-byte address space"
            )

    def derive_address(self, index: int) -> bytes:
        """Return the 20-byte address for the given logical index in this revision."""
        if index < 0:
            raise ValueError("address index must be non-negative")
        base_int = int.from_bytes(self.base_address, "big") if self.base_address else 0
        addr_int = base_int + self.revision * self.address_stride + index
        if addr_int >= _ADDRESS_SPACE:
            raise ValueError(f"address derivation overflows 20 bytes at index {index}")
        return addr_int.to_bytes(20, "big")

    def deploy_pubkey_sha256(self) -> str:
        """Stable fingerprint of the signer without leaking the private key."""
        return hashlib.sha256(self.deploy_private_key).hexdigest()

    def base_tx_fields(self) -> dict[str, Any]:
        """Pre-bound EIP-1559 tx fields shared by every signed tx in the run.

        Returned dict is safe to mutate-by-copy in the caller (``{**base, **fields}``)
        — avoids ~4 setdefault probes per tx × ~100 txs per batch × tens of thousands
        of batches (TRIZ Prior Action / review P1).
        """
        return {
            "type": 2,
            "chainId": self.chain_id,
            "maxFeePerGas": 2_000_000_000,
            "maxPriorityFeePerGas": 1_000_000_000,
            "accessList": [],
        }
