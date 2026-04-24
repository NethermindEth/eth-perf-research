"""TargetConfig loader tests."""
from __future__ import annotations

from pathlib import Path

import pytest

from orchestrator.target import load_target


def _write_target(tmp_path: Path, body: str) -> Path:
    p = tmp_path / "target.yaml"
    p.write_text(body)
    return p


def test_load_target_default_fields(tmp_path: Path) -> None:
    path = _write_target(
        tmp_path,
        """
mainnet_target:
  accounts: 0.141
  storage: 0.817
  code: 0.042
target_total_bytes: 1000000000000
base_address: "0x1000"
revision: 0
""",
    )
    cfg = load_target(path)
    assert cfg.target_total_bytes == 1_000_000_000_000
    assert cfg.mainnet_target["accounts"] == pytest.approx(0.141)
    assert cfg.source_sha256  # populated
    assert len(cfg.qp_scenarios) == 9


def test_load_target_rejects_unnormalized_mainnet(tmp_path: Path) -> None:
    path = _write_target(
        tmp_path,
        """
mainnet_target:
  accounts: 0.5
  storage: 0.5
  code: 0.5
target_total_bytes: 1000
base_address: "0x1000"
""",
    )
    with pytest.raises(ValueError):
        load_target(path)


def test_byte_target_computes_axis_share(tmp_path: Path) -> None:
    path = _write_target(
        tmp_path,
        """
mainnet_target:
  accounts: 0.1
  storage: 0.8
  code: 0.1
target_total_bytes: 1000000
base_address: "0x1000"
""",
    )
    cfg = load_target(path)
    assert cfg.byte_target("storage") == pytest.approx(800_000)
