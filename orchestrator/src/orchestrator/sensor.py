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
from pathlib import Path
from typing import Any

import httpx


class SensorWaitTimeout(Exception):
    """Raised when `blockNumber` fails to catch up to `expected_block` within the deadline."""

    def __init__(self, expected_block: int, last_seen_block: int) -> None:
        super().__init__(
            f"sensor: expected block {expected_block}, last seen {last_seen_block}"
        )
        self.expected_block = expected_block
        self.last_seen_block = last_seen_block


@dataclass(frozen=True)
class StateObservation:
    block_number: int
    account_bytes: int
    storage_bytes: int
    code_bytes: int
    raw: dict[str, Any] = field(default_factory=dict)


class SensorClient:
    """Polls `statecomp_get` over JSON-RPC and extracts three byte counters.

    `statecomp_get` lives on the public RPC port and needs no JWT. `jwt_path` is accepted
    for parity with engine-port clients and is read lazily on the first request if present.
    """

    POLL_INTERVAL_S = 0.1
    DEFAULT_TIMEOUT_S = 5.0

    def __init__(
        self,
        rpc_url: str,
        jwt_path: Path | str | None = None,
        *,
        client: httpx.Client | None = None,
    ) -> None:
        self._rpc_url = rpc_url
        self._jwt_path = Path(jwt_path) if jwt_path is not None else None
        self._client = client if client is not None else httpx.Client(timeout=10.0)
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
            resp = self._rpc("statecomp_get")
            last_seen = int(resp["blockNumber"])
            if expected_block is None or last_seen >= expected_block:
                ts = resp["trieStats"]
                return StateObservation(
                    block_number=last_seen,
                    account_bytes=int(ts["accountTrieBytes"]),
                    storage_bytes=int(ts["storageTrieBytes"]),
                    code_bytes=int(ts["codeBytesTotal"]),
                    raw=resp,
                )
            if time.monotonic() > deadline:
                raise SensorWaitTimeout(expected_block, last_seen)
            time.sleep(self.POLL_INTERVAL_S)

    def _rpc(self, method: str, params: list[Any] | None = None) -> dict[str, Any]:
        self._request_id += 1
        payload = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": method,
            "params": params or [],
        }
        headers: dict[str, str] = {"content-type": "application/json"}
        if self._jwt_path is not None and self._jwt_path.exists():
            headers["authorization"] = f"Bearer {self._jwt_path.read_text().strip()}"
        response = self._client.post(self._rpc_url, json=payload, headers=headers)
        response.raise_for_status()
        body = response.json()
        if "error" in body:
            raise RuntimeError(f"rpc error for {method}: {body['error']}")
        return body["result"]
