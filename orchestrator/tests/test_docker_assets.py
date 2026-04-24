"""Static checks on Docker assets — run without a Docker daemon."""

from __future__ import annotations

from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent


def test_dockerfile_non_root_uid() -> None:
    body = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    assert "--uid 10000" in body
    assert "USER orchestrator" in body


def test_dockerfile_is_multi_stage() -> None:
    body = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    assert body.count("FROM ") >= 2


def test_dockerfile_entrypoint() -> None:
    body = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    assert 'ENTRYPOINT ["orchestrator"]' in body


def test_compose_has_jwt_init_service() -> None:
    compose = yaml.safe_load((ROOT / "docker-compose.yml").read_text())
    assert "jwt-init" in compose["services"]
    jwt_svc = compose["services"]["jwt-init"]
    assert any(v.get("target") == "/run/jwt" for v in jwt_svc["volumes"])


def test_compose_nethermind_healthcheck_present() -> None:
    compose = yaml.safe_load((ROOT / "docker-compose.yml").read_text())
    neth = compose["services"]["nethermind"]
    assert "healthcheck" in neth
    assert neth["read_only"] is True


def test_compose_orchestrator_depends_on_healthy_nethermind() -> None:
    compose = yaml.safe_load((ROOT / "docker-compose.yml").read_text())
    dep = compose["services"]["orchestrator"]["depends_on"]
    assert dep["nethermind"]["condition"] == "service_healthy"


def test_compose_tmpfs_jwt_shared() -> None:
    """security L-COMPOSE: /run/jwt must be a SHARED named volume, not per-service tmpfs."""
    compose = yaml.safe_load((ROOT / "docker-compose.yml").read_text())
    # Named volume must be declared with a tmpfs backend.
    assert "jwt" in compose.get("volumes", {})
    for svc in ("nethermind", "orchestrator", "jwt-init"):
        volumes = compose["services"][svc]["volumes"]
        jwt_mount = next((v for v in volumes if v.get("target") == "/run/jwt"), None)
        assert jwt_mount is not None, f"{svc}: no /run/jwt mount"
        # Reject the old per-service `type: tmpfs` isolation (gen-jwt would write
        # into its own tmpfs and nethermind would see nothing).
        assert jwt_mount.get("type") == "volume", f"{svc}: /run/jwt must share the named volume"
        assert jwt_mount.get("source") == "jwt", f"{svc}: /run/jwt source must be named 'jwt'"


def test_dockerignore_excludes_state_and_tests() -> None:
    body = (ROOT / ".dockerignore").read_text()
    assert "tests/" in body
    assert "state/" in body
    assert ".venv/" in body
    assert ".git/" in body


def test_gen_jwt_script_exists_and_executable() -> None:
    gen = ROOT / "scripts" / "gen-jwt.sh"
    shred = ROOT / "scripts" / "shred-jwt.sh"
    assert gen.exists() and gen.stat().st_mode & 0o111
    assert shred.exists() and shred.stat().st_mode & 0o111


def test_gen_jwt_writes_600_permissions() -> None:
    body = (ROOT / "scripts" / "gen-jwt.sh").read_text()
    assert "chmod 600" in body
