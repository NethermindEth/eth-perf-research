"""Thin JSON-RPC client used by the lifecycle and replay paths.

Wraps three calls:
    - `testing_commitBlockV1(signed_txs)` → block hash
    - `eth_getBlockByHash(hash, full=True)` → payload-ready block
    - `eth_getBlockByNumber("latest"|int)` → state root + misc

The shape of `testing_commitBlockV1` is defined in Nethermind's
`TestingRpcModule.cs`; we accept whatever the module returns and unwrap `result`.
"""
from __future__ import annotations

from pathlib import Path
from typing import Any

import httpx


class RpcClient:
    def __init__(
        self,
        rpc_url: str,
        jwt_path: Path | str | None = None,
        *,
        client: httpx.Client | None = None,
    ) -> None:
        self._rpc_url = rpc_url
        self._jwt_path = Path(jwt_path) if jwt_path is not None else None
        self._client = client if client is not None else httpx.Client(timeout=30.0)
        self._owns_client = client is None
        self._request_id = 0

    def close(self) -> None:
        if self._owns_client:
            self._client.close()

    def __enter__(self) -> RpcClient:
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()

    def testing_commit_block_v1(self, signed_txs_rlp: list[bytes]) -> str:
        """Submit a batch of signed txs; returns the committed block's hash (hex)."""
        hex_txs = ["0x" + raw.hex() for raw in signed_txs_rlp]
        return self._call("testing_commitBlockV1", [hex_txs])

    def eth_get_block_by_hash(self, block_hash: str, *, full: bool = True) -> dict[str, Any]:
        return self._call("eth_getBlockByHash", [block_hash, full])

    def eth_get_block_by_number(
        self, number: str | int = "latest", *, full: bool = False
    ) -> dict[str, Any]:
        tag = number if isinstance(number, str) else hex(number)
        return self._call("eth_getBlockByNumber", [tag, full])

    def _call(self, method: str, params: list[Any]) -> Any:
        self._request_id += 1
        payload = {
            "jsonrpc": "2.0",
            "id": self._request_id,
            "method": method,
            "params": params,
        }
        headers = {"content-type": "application/json"}
        if self._jwt_path is not None and self._jwt_path.exists():
            headers["authorization"] = f"Bearer {self._jwt_path.read_text().strip()}"
        response = self._client.post(self._rpc_url, json=payload, headers=headers)
        response.raise_for_status()
        body = response.json()
        if "error" in body:
            raise RuntimeError(f"rpc error for {method}: {body['error']}")
        return body["result"]
