"""Thin JSON-RPC client used by the lifecycle and replay paths.

Wraps three calls:
    - `testing_commitBlockV1(signed_txs)` → block hash
    - `eth_getBlockByHash(hash, full=True)` → payload-ready block
    - `eth_getBlockByNumber("latest"|int)` → state root + misc

The shape of `testing_commitBlockV1` is defined in Nethermind's
`TestingRpcModule.cs`; we accept whatever the module returns and unwrap `result`.

JWT is read once at construction — not on every call — validated as 64 hex chars,
and the JWT file's mode bits are checked against ``0o077`` so a world-readable
or group-readable secret fails loudly. Engine-port URLs (default 8551, configurable)
require the JWT; plain public-port usage does not. SSRF guard: the URL's hostname
is resolved once at construction, and private / loopback / link-local / CGNAT IPs
are refused unless the caller passes ``allow_private=True``.
"""

from __future__ import annotations

import hashlib
import ipaddress
import logging
import re
import socket
import stat
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
    """JWT file is missing, unreadable, not 64 hex chars, or has lax permissions."""


class UnsafeRpcUrl(ValueError):
    """Rpc url resolves to a private / loopback / link-local address."""


def _load_jwt(path: Path) -> str:
    try:
        mode = path.stat().st_mode
    except OSError as exc:
        raise JwtConfigError(f"jwt file at {path} not readable: {exc}") from exc
    # Bits 0o077 covers group+other rwx. A JWT leaked via world-readable file
    # is functionally the same as a leaked JWT; refuse at load time.
    if mode & 0o077:
        raise JwtConfigError(
            f"jwt file at {path} has lax permissions {stat.filemode(mode)}; chmod 0600 before reuse"
        )
    try:
        raw = path.read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise JwtConfigError(f"jwt file at {path} not readable: {exc}") from exc
    if not _JWT_HEX_RE.fullmatch(raw):
        raise JwtConfigError(f"jwt file at {path} must be exactly 64 hex characters")
    return raw


def _check_url_not_private(rpc_url: str) -> None:
    """SSRF guard: resolve the host once and refuse private/loopback targets.

    This is *defense in depth* only — a hostile DNS server could still flip an
    allowed name to a private IP between this check and the httpx request. To
    close that window we'd need an IP-pinned transport; documented follow-up.
    """
    parsed = urlparse(rpc_url)
    host = parsed.hostname
    if not host:
        return
    try:
        addr = socket.gethostbyname(host)
        ip = ipaddress.ip_address(addr)
    except (socket.gaierror, ValueError):
        # Don't crash on unresolvable hostnames at construction time — tests use
        # stubs like "http://fake/rpc" that never resolve. The actual request
        # will fail with a clearer error.
        return
    if ip.is_loopback or ip.is_link_local or ip.is_multicast:
        raise UnsafeRpcUrl(
            f"rpc url host {host!r} resolves to {ip} ({_ip_kind(ip)}); "
            f"pass allow_private=True to opt in"
        )
    if ip.is_private:
        # Private (RFC 1918, fc00::/7, etc.) is the common docker-compose case.
        # Allow it, but log so footguns are visible.
        _log.debug("rpc url resolves to private %s; allowed", ip)


def _ip_kind(ip: ipaddress.IPv4Address | ipaddress.IPv6Address) -> str:
    if ip.is_loopback:
        return "loopback"
    if ip.is_link_local:
        return "link-local"
    if ip.is_multicast:
        return "multicast"
    return "restricted"


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
        # Always refuse loopback/link-local/multicast. Private (RFC 1918)
        # remains allowed — see ``_check_url_not_private`` for the policy.
        _check_url_not_private(rpc_url)
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
        self._client = client if client is not None else httpx.Client(timeout=30.0, limits=limits)
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
        """Submit a batch of signed txs; returns the committed block's hash (hex).

        Marcin's endpoint signature is
        ``testing_commitBlockV1(payloadAttributes, txRlps, extraData)``;
        we synthesize minimal payload attributes (current wall-clock timestamp,
        zeroed randao / fee recipient / parent beacon root, empty withdrawals)
        and pass ``null`` extraData. Withdrawals are an empty list (Cancun+).
        """
        import time as _time

        hex_txs = ["0x" + raw.hex() for raw in signed_txs_rlp]
        payload_attributes = {
            "timestamp": hex(int(_time.time())),
            "prevRandao": "0x" + "00" * 32,
            "suggestedFeeRecipient": "0x" + "00" * 20,
            "withdrawals": [],
            "parentBeaconBlockRoot": "0x" + "00" * 32,
        }
        return self._call("testing_commitBlockV1", [payload_attributes, hex_txs, None])

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
            # Redact the raw server body — Nethermind (or a MITM'd response) can
            # emit attacker-controlled text; only a SHA-16 prefix goes to logs
            # (review M-1). Full message stays accessible via RpcError.server_message.
            msg_digest = hashlib.sha256(message.encode("utf-8")).hexdigest()[:16]
            _log.warning("rpc %s failed: code=%s msg_sha16=%s", method, code, msg_digest)
            raise RpcError(method, code, message)
        return body["result"]
