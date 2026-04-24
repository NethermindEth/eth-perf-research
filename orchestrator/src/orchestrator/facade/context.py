"""Shared facade context: cursors + chain metadata passed to every adapter."""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class SignedTransaction:
    """Wrapped signed tx — carries both RLP bytes (for size/commit) and metadata.

    Adapters return these; the orchestrator takes `rlp` for `testing_commitBlockV1` and
    reads `fields` for diagnostics. `kind` records the verb that produced the tx so
    downstream tooling can filter without re-decoding.
    """

    rlp: bytes
    kind: str
    fields: dict[str, Any] = field(default_factory=dict)


@dataclass
class FacadeContext:
    """Mutable cursor state carried across batches.

    Addresses are derived sequentially as `base_address + (revision * stride + address_cursor)`,
    big-endian-packed into 20 bytes. Salts (for CREATE2) share the same monotonic cursor but
    are tracked separately so deploy and factory-deploy verbs don't collide.
    """

    base_address: bytes
    revision: int
    address_cursor: int = 0
    salt_cursor: int = 0
    genesis_sha256: bytes = b""
    chain_id: int = 1337
    gas_limit: int = 30_000_000
    deploy_private_key: bytes = b"\x11" * 32
    address_stride: int = 1 << 40  # per-revision address bucket, large enough to avoid collision

    def derive_address(self, index: int) -> bytes:
        """Return the 20-byte address for the given logical index in this revision."""
        base_int = int.from_bytes(self.base_address, "big") if self.base_address else 0
        addr_int = base_int + self.revision * self.address_stride + index
        return addr_int.to_bytes(20, "big")

    def next_address(self) -> bytes:
        addr = self.derive_address(self.address_cursor)
        self.address_cursor += 1
        return addr

    def next_salt(self) -> bytes:
        salt = self.salt_cursor.to_bytes(32, "big")
        self.salt_cursor += 1
        return salt
