"""Thin JSON-RPC client used by the lifecycle and replay paths.

Wraps three calls:
    - `testing_commitBlockV1(signed_txs)` → block hash
    - `eth_getBlockByHash(hash, full=True)` → payload-ready block
    - `eth_getBlockByNumber("latest"|int)` → state root + misc

The shape of `testing_commitBlockV1` is defined in Nethermind's
`TestingRpcModule.cs`; we accept whatever the module returns and unwrap `result`.

JWT is read once at construction — not on every call — and validated as 64 hex chars.
Engine-port URLs (default 8551, configurable) require the JWT; plain public-port usage
does not. SSRF guard blocks link-local / loopback-only when an explicit url schema does
not match the allowlist passed to ``from_url``.
"""
from __future__ import annotations

import logging
import re
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import httpx


_JWT_HEX_RE = re.compile(r"^[0-9a-fA-F]{64}$")
_ENGINE_PORTS = frozenset({8551, 8552})  # default JWT-protected ports
_log = logging.getLogger(__name__)


class RpcError(RuntimeError):
    """JSON-RPC server returned an error envelope. Raw body not embedded in str."""

    def __init__(self, method: str, code: int | None, message: str) -> None:
        super().__init__(f"rpc error for {method}: code={code}")
        self.method = method
        self.code = code
        self.server_message = message  # available for logs / debuggers, not in __str__


class JwtConfigError(ValueError):
    """JWT file is missing, unreadable, or not 64 hex chars."""


def _load_jwt(path: Path) -> str:
    try:
        raw = path.read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise JwtConfigError(f"jwt file at {path} not readable: {exc}") from exc
    if not _JWT_HEX_RE.fullmatch(raw):
        raise JwtConfigError(
            f"jwt file at {path} must be exactly 64 hex characters"
        )
    return raw


class RpcClient:
    def __init__(
        self,
        rpc_url: str,
        jwt_path: Path | str | None = None,
        *,
        client: httpx.Client | None = None,
        require_jwt_for_engine_port: bool = True,
    ) -> None:
        parsed = urlparse(rpc_url)
        if parsed.scheme not in ("http", "https"):
            raise ValueError(f"rpc url must be http(s), got {rpc_url!r}")
        self._rpc_url = rpc_url
        self._bearer: str | None = None
        if jwt_path is not None:
            self._bearer = _load_jwt(Path(jwt_path))
        elif require_jwt_for_engine_port and parsed.port in _ENGINE_PORTS:
            raise JwtConfigError(
                f"rpc url port {parsed.port} is engine-protected; "
                f"supply jwt_path or set require_jwt_for_engine_port=False"
            )
        limits = httpx.Limits(max_keepalive_connections=4, keepalive_expiry=600.0)
        self._client = (
            client if client is not None else httpx.Client(timeout=30.0, limits=limits)
        )
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
        if self._bearer is not None:
            headers["authorization"] = f"Bearer {self._bearer}"
        response = self._client.post(self._rpc_url, json=payload, headers=headers)
        response.raise_for_status()
        body = response.json()
        if "error" in body:
            err = body["error"] or {}
            code = err.get("code") if isinstance(err, dict) else None
            message = err.get("message", "") if isinstance(err, dict) else str(err)
            _log.warning("rpc %s failed: code=%s message=%s", method, code, message)
            raise RpcError(method, code, message)
        return body["result"]
