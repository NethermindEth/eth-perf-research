"""Append-only `ExecutionPayloadV3` stream for cross-client reproduction.

Format: one record per commit. Each record is length-prefixed (4-byte big-endian unsigned
integer giving RLP byte length) followed by the RLP-encoded payload. fsync after each write.

The RLP layout is the canonical `ExecutionPayloadV3` encoding used across all EL clients —
`geth import`, `besu blocks import`, `erigon import`, `reth import` all accept it.
"""

from __future__ import annotations

import os
import struct
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import cast

import rlp


@dataclass
class ExecutionPayloadV3:
    """Minimal ExecutionPayloadV3 carrier.

    Fields follow the Engine API spec. `encode_rlp` hands the whole list to `rlp.encode`
    using the block's canonical field order; fields we don't need at construction time
    (e.g. `blob_gas_used`, `excess_blob_gas`) default to zero / empty.
    """

    parent_hash: bytes
    fee_recipient: bytes
    state_root: bytes
    receipts_root: bytes
    logs_bloom: bytes
    prev_randao: bytes
    block_number: int
    gas_limit: int
    gas_used: int
    timestamp: int
    extra_data: bytes
    base_fee_per_gas: int
    block_hash: bytes
    transactions: list[bytes] = field(default_factory=list)
    withdrawals: list[bytes] = field(default_factory=list)
    blob_gas_used: int = 0
    excess_blob_gas: int = 0

    def encode_rlp(self) -> bytes:
        # `rlp.encode` handles ints/bytes/lists directly; wrap each top-level field.
        body = [
            self.parent_hash,
            self.fee_recipient,
            self.state_root,
            self.receipts_root,
            self.logs_bloom,
            self.prev_randao,
            self.block_number,
            self.gas_limit,
            self.gas_used,
            self.timestamp,
            self.extra_data,
            self.base_fee_per_gas,
            self.block_hash,
            self.transactions,
            self.withdrawals,
            self.blob_gas_used,
            self.excess_blob_gas,
        ]
        return cast(bytes, rlp.encode(body))


_LEN_STRUCT = struct.Struct(">I")


class PayloadStreamWriter:
    """Length-prefixed binary stream, fsync per append."""

    def __init__(self, path: Path | str) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fd = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)

    def append(self, payload: ExecutionPayloadV3) -> None:
        body = payload.encode_rlp()
        os.write(self._fd, _LEN_STRUCT.pack(len(body)))
        os.write(self._fd, body)
        os.fsync(self._fd)

    def close(self) -> None:
        os.close(self._fd)

    def __enter__(self) -> PayloadStreamWriter:
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()


class PayloadStreamReader:
    """Iterate raw RLP-encoded payloads from the stream (length-prefixed)."""

    def __init__(self, path: Path | str) -> None:
        self.path = Path(path)

    def __iter__(self) -> Iterator[bytes]:
        with self.path.open("rb") as f:
            while True:
                header = f.read(_LEN_STRUCT.size)
                if not header:
                    return
                if len(header) != _LEN_STRUCT.size:
                    raise ValueError("truncated payload stream: header")
                (length,) = _LEN_STRUCT.unpack(header)
                body = f.read(length)
                if len(body) != length:
                    raise ValueError("truncated payload stream: body")
                yield body
