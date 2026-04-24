"""RpcClient tests: JWT validation, engine-port guard, error handling, SSRF guard."""
from __future__ import annotations

import os
from pathlib import Path

import httpx
import pytest

from orchestrator.rpc import JwtConfigError, RpcClient, RpcError, UnsafeRpcUrl


class _Transport(httpx.BaseTransport):
    def __init__(self, body: dict, *, status: int = 200) -> None:
        self.body = body
        self.status = status
        self.captured_auth: str | None = None

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.captured_auth = request.headers.get("authorization")
        return httpx.Response(self.status, json=self.body)


def _client(transport: _Transport) -> httpx.Client:
    return httpx.Client(transport=transport)


def _write_jwt(path: Path, value: str, *, mode: int = 0o600) -> Path:
    path.write_text(value)
    os.chmod(path, mode)
    return path


def test_requires_jwt_on_engine_port(tmp_path: Path) -> None:
    with pytest.raises(JwtConfigError, match="engine-protected"):
        RpcClient("http://nethermind:8551")


def test_accepts_engine_port_with_jwt(tmp_path: Path) -> None:
    jwt = _write_jwt(tmp_path / "jwt.hex", "a" * 64)
    rpc = RpcClient("http://nethermind:8551", jwt_path=jwt)
    assert rpc is not None
    rpc.close()


def test_rejects_short_jwt(tmp_path: Path) -> None:
    jwt = _write_jwt(tmp_path / "jwt.hex", "short")
    with pytest.raises(JwtConfigError, match="64 hex"):
        RpcClient("http://nethermind:8551", jwt_path=jwt)


def test_rejects_non_hex_jwt(tmp_path: Path) -> None:
    jwt = _write_jwt(tmp_path / "jwt.hex", "z" * 64)
    with pytest.raises(JwtConfigError):
        RpcClient("http://nethermind:8551", jwt_path=jwt)


def test_rejects_world_readable_jwt(tmp_path: Path) -> None:
    """M-3: a 0o644 JWT is functionally a leaked JWT — refuse at load."""
    jwt = _write_jwt(tmp_path / "jwt.hex", "a" * 64, mode=0o644)
    with pytest.raises(JwtConfigError, match="lax permissions"):
        RpcClient("http://nethermind:8551", jwt_path=jwt)


def test_rejects_group_readable_jwt(tmp_path: Path) -> None:
    jwt = _write_jwt(tmp_path / "jwt.hex", "a" * 64, mode=0o640)
    with pytest.raises(JwtConfigError, match="lax permissions"):
        RpcClient("http://nethermind:8551", jwt_path=jwt)


def test_jwt_sent_as_bearer(tmp_path: Path) -> None:
    jwt = _write_jwt(tmp_path / "jwt.hex", "a" * 64)
    transport = _Transport({"jsonrpc": "2.0", "id": 1, "result": {"number": "0x0"}})
    rpc = RpcClient(
        "http://nethermind:8551", jwt_path=jwt, client=_client(transport)
    )
    rpc.eth_get_block_by_number("latest")
    assert transport.captured_auth == "Bearer " + "a" * 64


def test_no_auth_header_on_public_port(tmp_path: Path) -> None:
    transport = _Transport({"jsonrpc": "2.0", "id": 1, "result": {"number": "0x0"}})
    rpc = RpcClient("http://nethermind:8545", client=_client(transport))
    rpc.eth_get_block_by_number("latest")
    assert transport.captured_auth is None


def test_rejects_non_http_scheme(tmp_path: Path) -> None:
    with pytest.raises(ValueError, match="http"):
        RpcClient("ftp://nethermind:8545")


def test_rejects_loopback_url() -> None:
    """M-2: loopback is always refused (SSRF defense-in-depth)."""
    with pytest.raises(UnsafeRpcUrl, match="loopback"):
        RpcClient("http://127.0.0.1:8545")


def test_rejects_link_local_url() -> None:
    """Refuse link-local (AWS metadata service sits at 169.254.169.254)."""
    with pytest.raises(UnsafeRpcUrl, match="link-local"):
        RpcClient("http://169.254.169.254:8545")


def test_rpc_error_does_not_leak_server_body_in_str() -> None:
    transport = _Transport(
        {"jsonrpc": "2.0", "id": 1, "error": {"code": -32000, "message": "sensitive data"}}
    )
    rpc = RpcClient("http://nethermind:8545", client=_client(transport))
    with pytest.raises(RpcError) as excinfo:
        rpc.eth_get_block_by_number("latest")
    assert "sensitive data" not in str(excinfo.value)
    assert excinfo.value.server_message == "sensitive data"
    assert excinfo.value.code == -32000


def test_rpc_error_log_redacts_server_message(
    caplog: pytest.LogCaptureFixture,
) -> None:
    """M-1: raw server body must not leak into structured logs."""
    import logging

    caplog.set_level(logging.WARNING, logger="orchestrator.rpc")
    transport = _Transport(
        {"jsonrpc": "2.0", "id": 1, "error": {"code": -32000, "message": "SECRET_LEAK_TOKEN"}}
    )
    rpc = RpcClient("http://nethermind:8545", client=_client(transport))
    with pytest.raises(RpcError):
        rpc.eth_get_block_by_number("latest")
    combined = " ".join(r.getMessage() for r in caplog.records)
    assert "SECRET_LEAK_TOKEN" not in combined
    assert "msg_sha16=" in combined
