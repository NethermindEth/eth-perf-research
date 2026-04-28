"""Sensor client for Nethermind's `statecomp_get` RPC.

The plugin on master returns the `StateCompositionReport` shape (camelCase JSON):

    {
      "trieStats": {
        "accountTrieBytes": 18000000000,
        "storageTrieBytes": 72000000000,
        "codeBytesTotal":   12500000000,
        ...
      },
      "blockNumber": 19012346,
      ...
    }

Only three byte counters feed the controller; the rest travels into the journal via `raw`.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any

import httpx


def _parse_int(v: Any) -> int:
    """Accept hex (``"0x..."``) or decimal (string or int). Nethermind serializes
    quantities as hex; older fixtures sometimes use decimal."""
    if isinstance(v, int):
        return v
    s = str(v)
    return int(s, 16) if s.startswith(("0x", "0X")) else int(s)


class SensorWaitTimeout(Exception):
    """Raised when `blockNumber` fails to catch up to `expected_block` within the deadline."""

    def __init__(self, expected_block: int, last_seen_block: int) -> None:
        super().__init__(f"sensor: expected block {expected_block}, last seen {last_seen_block}")
        self.expected_block = expected_block
        self.last_seen_block = last_seen_block


@dataclass(frozen=True, slots=True)
class StateObservation:
    block_number: int
    account_bytes: int
    storage_bytes: int
    code_bytes: int
    raw: dict[str, Any] = field(default_factory=dict)


class SensorClient:
    """Polls `statecomp_get` over JSON-RPC and extracts three byte counters.

    ``statecomp_get`` lives on the public RPC port and needs no JWT, so the sensor
    client deliberately does not accept a ``jwt_path``. Engine-port (JWT-protected)
    access is handled by ``orchestrator.rpc.RpcClient`` instead.
    """

    POLL_INTERVAL_S = 0.1
    DEFAULT_TIMEOUT_S = 5.0

    def __init__(
        self,
        rpc_url: str,
        *,
        client: httpx.Client | None = None,
    ) -> None:
        self._rpc_url = rpc_url
        limits = httpx.Limits(max_keepalive_connections=2, keepalive_expiry=600.0)
        self._client = client if client is not None else httpx.Client(timeout=10.0, limits=limits)
        self._owns_client = client is None
        self._request_id = 0

    def close(self) -> None:
        if self._owns_client:
            self._client.close()

    def __enter__(self) -> SensorClient:
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()

    def read(
        self,
        expected_block: int | None = None,
        timeout_s: float = DEFAULT_TIMEOUT_S,
    ) -> StateObservation:
        deadline = time.monotonic() + timeout_s
        last_seen = -1
        while True:
            # A single stuck RPC call must not eat the whole 5 s window. Cap
            # the per-call timeout at the lesser of (remaining-deadline, 2 s)
            # so we still get at least one poll attempt before SensorWaitTimeout.
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise SensorWaitTimeout(expected_block or 0, last_seen)
            call_timeout = min(remaining, 2.0)
            try:
                resp = self._rpc("statecomp_get", request_timeout=call_timeout)
            except httpx.TimeoutException:
                # The call itself timed out; convert to SensorWaitTimeout if
                # the outer deadline is also exceeded, otherwise loop and retry.
                if time.monotonic() >= deadline:
                    raise SensorWaitTimeout(expected_block or 0, last_seen) from None
                continue
            last_seen = _parse_int(resp["blockNumber"])
            if expected_block is None or last_seen >= expected_block:
                ts = resp["trieStats"]
                return StateObservation(
                    block_number=last_seen,
                    account_bytes=_parse_int(ts["accountTrieBytes"]),
                    storage_bytes=_parse_int(ts["storageTrieBytes"]),
                    code_bytes=_parse_int(ts["codeBytesTotal"]),
                    raw=resp,
                )
            if time.monotonic() > deadline:
                raise SensorWaitTimeout(expected_block, last_seen)
            time.sleep(self.POLL_INTERVAL_S)

    def _rpc(
        self,
        method: str,
        params: list[Any] | None = None,
        *,
        request_timeout: float | None = None,
    ) -> dict[str, Any]:
        self._request_id += 1
        payload = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": method,
            "params": params or [],
        }
        headers = {"content-type": "application/json"}
        # Per-call timeout overrides the client-default 10 s so the sensor
        # never blocks past the outer ``read()`` deadline.
        kwargs: dict[str, Any] = {"json": payload, "headers": headers}
        if request_timeout is not None:
            kwargs["timeout"] = request_timeout
        response = self._client.post(self._rpc_url, **kwargs)
        response.raise_for_status()
        body = response.json()
        if "error" in body:
            err = body["error"] or {}
            code = err.get("code") if isinstance(err, dict) else None
            raise RuntimeError(f"sensor rpc {method} failed: code={code}")
        return body["result"]
