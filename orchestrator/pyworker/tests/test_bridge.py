"""Unit tests for the builder_worker bridge.

Run with:
    uv run --with pytest --with protobuf --with eth-utils --with eth-abi pytest tests/ -v

orchestrator-py must be installed (or on PYTHONPATH) for the eoatx test.
Install it with:
    pip install -e ../orchestrator-py
or:
    PYTHONPATH=../orchestrator-py/src python -m builder_worker
"""
from __future__ import annotations

import pytest

from builder_worker._proto import builder_pb2
from builder_worker.bridge import build


def test_bridge_unknown_verb() -> None:
    req = builder_pb2.BuildBatchRequest(id=1, verb="nope", start_idx=0, count=1)
    resp = build(req)
    assert resp.error.startswith("unknown verb")


def test_bridge_eoatx_returns_signables() -> None:
    """Requires orchestrator-py to be installed: pip install -e ../orchestrator-py"""
    # chain_id must be in LAB_ALLOWED_CHAIN_IDS (1337 or 31337) when using
    # the default lab private key — FacadeContext.__post_init__ enforces this.
    req = builder_pb2.BuildBatchRequest(
        id=1,
        verb="eoatx",
        start_idx=0,
        count=3,
        ctx=builder_pb2.FacadeCtxParams(
            base_address=b"\x01" * 20,
            revision=0,
            chain_id=1337,
            block_gas_limit=8_000_000_000,
            salt_cursor=0,
            address_stride=1 << 40,
        ),
    )
    resp = build(req)
    assert resp.error == "", f"unexpected error: {resp.error}"
    assert len(resp.signables) == 3
    for i, tx in enumerate(resp.signables):
        assert tx.nonce == i
        assert tx.gas == 21_000


def test_bridge_noop_returns_signables() -> None:
    """noop verb: self-transfer, no EELS dependency beyond orchestrator-py."""
    req = builder_pb2.BuildBatchRequest(
        id=2,
        verb="noop",
        start_idx=0,
        count=2,
        ctx=builder_pb2.FacadeCtxParams(
            base_address=b"\x00" * 20,
            revision=0,
            chain_id=1337,
            block_gas_limit=30_000_000,
            salt_cursor=0,
            address_stride=1 << 40,
        ),
    )
    resp = build(req)
    assert resp.error == "", f"unexpected error: {resp.error}"
    assert len(resp.signables) == 2
    for i, tx in enumerate(resp.signables):
        assert tx.nonce == i
        assert tx.gas == 21_000


def test_bridge_factorydeploytx_advances_salt() -> None:
    """salt_cursor must advance by count after factorydeploytx."""
    req = builder_pb2.BuildBatchRequest(
        id=3,
        verb="factorydeploytx",
        start_idx=0,
        count=3,
        ctx=builder_pb2.FacadeCtxParams(
            base_address=b"\x00" * 20,
            revision=0,
            chain_id=1337,
            block_gas_limit=30_000_000,
            salt_cursor=10,
            address_stride=1 << 40,
        ),
    )
    resp = build(req)
    assert resp.error == "", f"unexpected error: {resp.error}"
    assert resp.new_salt_cursor == 13  # started at 10, advanced 3 times


def test_bridge_deploytx_returns_contract_creates() -> None:
    """deploytx: to field must be empty (contract creation)."""
    req = builder_pb2.BuildBatchRequest(
        id=4,
        verb="deploytx",
        start_idx=0,
        count=2,
        ctx=builder_pb2.FacadeCtxParams(
            base_address=b"\x00" * 20,
            revision=0,
            chain_id=1337,
            block_gas_limit=30_000_000,
            salt_cursor=0,
            address_stride=1 << 40,
        ),
    )
    resp = build(req)
    assert resp.error == "", f"unexpected error: {resp.error}"
    assert len(resp.signables) == 2
    for tx in resp.signables:
        assert tx.to == b"", "deploytx must have empty 'to' (contract creation)"
        assert tx.gas == 300_000


def test_bridge_response_id_mirrors_request() -> None:
    req = builder_pb2.BuildBatchRequest(id=42, verb="noop", start_idx=5, count=1,
        ctx=builder_pb2.FacadeCtxParams(
            base_address=b"\x00" * 20,
            revision=0,
            chain_id=1337,
            block_gas_limit=30_000_000,
            salt_cursor=0,
            address_stride=1 << 40,
        ),
    )
    resp = build(req)
    assert resp.id == 42


def test_bridge_start_idx_offsets_nonce() -> None:
    """start_idx drives the nonce offset passed to verb builders."""
    req = builder_pb2.BuildBatchRequest(
        id=5,
        verb="noop",
        start_idx=100,
        count=2,
        ctx=builder_pb2.FacadeCtxParams(
            base_address=b"\x00" * 20,
            revision=0,
            chain_id=1337,
            block_gas_limit=30_000_000,
            salt_cursor=0,
            address_stride=1 << 40,
        ),
    )
    resp = build(req)
    assert resp.error == ""
    assert resp.signables[0].nonce == 100
    assert resp.signables[1].nonce == 101
