"""Sensor client tests with a fake httpx transport."""
from __future__ import annotations

import time

import httpx
import pytest

from orchestrator.sensor import SensorClient, SensorWaitTimeout, StateObservation


def _report(block_number: int, *, account: int = 10, storage: int = 20, code: int = 30) -> dict:
    return {
        "trieStats": {
            "accountsTotal": 1,
            "accountTrieBytes": account,
            "storageTrieBytes": storage,
            "codeBytesTotal": code,
        },
        "blockNumber": block_number,
        "diffsSinceBaseline": 0,
    }


class _FakeTransport(httpx.BaseTransport):
    """Serves `statecomp_get` from a scripted queue of block numbers."""

    def __init__(self, block_numbers: list[int], *, account: int = 10, storage: int = 20, code: int = 30):
        self.block_numbers = list(block_numbers)
        self.calls = 0
        self._account = account
        self._storage = storage
        self._code = code

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.calls += 1
        bn = self.block_numbers.pop(0) if self.block_numbers else self.block_numbers[-1]
        body = {
            "jsonrpc": "2.0",
            "id": 1,
            "result": _report(bn, account=self._account, storage=self._storage, code=self._code),
        }
        return httpx.Response(200, json=body)


def _sensor(transport: _FakeTransport) -> SensorClient:
    client = httpx.Client(transport=transport)
    return SensorClient("http://fake/rpc", client=client)


def test_returns_immediately_when_block_caught_up() -> None:
    transport = _FakeTransport([100])
    with _sensor(transport) as sensor:
        obs = sensor.read(expected_block=100)
    assert obs.block_number == 100
    assert transport.calls == 1


def test_polls_until_block_catches_up() -> None:
    transport = _FakeTransport([99, 99, 100])
    with _sensor(transport) as sensor:
        obs = sensor.read(expected_block=100, timeout_s=2.0)
    assert obs.block_number == 100
    assert transport.calls == 3


def test_timeout_raises() -> None:
    transport = _FakeTransport([99] * 10)
    started = time.monotonic()
    with _sensor(transport) as sensor:
        with pytest.raises(SensorWaitTimeout) as excinfo:
            sensor.read(expected_block=100, timeout_s=0.3)
    elapsed = time.monotonic() - started
    assert excinfo.value.expected_block == 100
    assert excinfo.value.last_seen_block == 99
    assert 0.2 <= elapsed < 1.5


def test_extracts_three_counters() -> None:
    transport = _FakeTransport([42], account=111, storage=222, code=333)
    with _sensor(transport) as sensor:
        obs: StateObservation = sensor.read()
    assert (obs.account_bytes, obs.storage_bytes, obs.code_bytes) == (111, 222, 333)


def test_raw_preserves_full_response() -> None:
    transport = _FakeTransport([42])
    with _sensor(transport) as sensor:
        obs = sensor.read()
    assert obs.raw["blockNumber"] == 42
    assert obs.raw["trieStats"]["accountsTotal"] == 1
    assert obs.raw["diffsSinceBaseline"] == 0


def test_read_without_expected_block_returns_first_response() -> None:
    transport = _FakeTransport([55])
    with _sensor(transport) as sensor:
        obs = sensor.read()
    assert obs.block_number == 55
    assert transport.calls == 1


def test_default_timeout_is_five_seconds() -> None:
    assert SensorClient.DEFAULT_TIMEOUT_S == 5.0


def test_poll_interval_is_100ms() -> None:
    assert SensorClient.POLL_INTERVAL_S == 0.1
