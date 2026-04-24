"""Payload stream round-trip test."""
from __future__ import annotations

import os
from pathlib import Path

import pytest
import rlp

from orchestrator.payloads import (
    ExecutionPayloadV3,
    PayloadStreamReader,
    PayloadStreamWriter,
)


def _make_payload(block_number: int) -> ExecutionPayloadV3:
    return ExecutionPayloadV3(
        parent_hash=b"\x00" * 32,
        fee_recipient=b"\x00" * 20,
        state_root=(block_number).to_bytes(32, "big"),
        receipts_root=b"\x00" * 32,
        logs_bloom=b"\x00" * 256,
        prev_randao=b"\x00" * 32,
        block_number=block_number,
        gas_limit=30_000_000,
        gas_used=21_000,
        timestamp=1700000000 + block_number,
        extra_data=b"",
        base_fee_per_gas=1_000_000_000,
        block_hash=(block_number).to_bytes(32, "big"),
        transactions=[b"\x01\x02\x03"],
        withdrawals=[],
    )


def test_payload_stream_round_trip(tmp_path: Path) -> None:
    stream = tmp_path / "payloads.rlp"
    with PayloadStreamWriter(stream) as w:
        for i in range(5):
            w.append(_make_payload(i))

    blobs = list(PayloadStreamReader(stream))
    assert len(blobs) == 5
    for i, blob in enumerate(blobs):
        decoded = rlp.decode(blob)
        # block_number is at index 6 in the ExecutionPayloadV3 layout.
        assert int.from_bytes(decoded[6], "big") == i


def test_payload_stream_fsync_per_append(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    calls: list[int] = []
    real_fsync = os.fsync
    monkeypatch.setattr(os, "fsync", lambda fd: calls.append(fd) or real_fsync(fd))
    stream = tmp_path / "payloads.rlp"
    with PayloadStreamWriter(stream) as w:
        for i in range(4):
            w.append(_make_payload(i))
    assert len(calls) == 4


def test_payload_stream_reader_rejects_truncated(tmp_path: Path) -> None:
    stream = tmp_path / "payloads.rlp"
    with PayloadStreamWriter(stream) as w:
        w.append(_make_payload(0))
    # Corrupt: truncate to mid-body.
    raw = stream.read_bytes()
    stream.write_bytes(raw[: len(raw) - 5])
    with pytest.raises(ValueError):
        list(PayloadStreamReader(stream))
