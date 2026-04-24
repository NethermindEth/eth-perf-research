"""TargetConfig loader tests."""

from __future__ import annotations

from pathlib import Path

import pytest

from orchestrator.target import load_target

_MINIMAL_VALID_YAML = """
mainnet_target:
  accounts: 0.141
  storage: 0.817
  code: 0.042
target_total_bytes: 1000000000000
base_address: "0x1000"
revision: 0
qp_scenarios:
  - eoatx
  - calltx
  - deploytx
  - factorydeploytx
  - storagespam
  - erc20_bloater
  - erc20tx
  - uniswap_swaps
  - storagerefundtx
total_batch_bytes: 10000000
projection_eta: 0.5
"""


def _write_target(tmp_path: Path, body: str) -> Path:
    p = tmp_path / "target.yaml"
    p.write_text(body)
    return p


def _write_valid_target(tmp_path: Path, **overrides: str) -> Path:
    """Write a minimal valid target.yaml, optionally replacing specific lines.

    ``overrides`` values are spliced in as-is for lines whose key matches; pass
    ``revision="revision: -1"`` to override the revision line, for example.
    """
    lines: list[str] = [str(ln) for ln in _MINIMAL_VALID_YAML.strip().splitlines()]
    for key, replacement in overrides.items():
        for i, line in enumerate(lines):
            if line.startswith(f"{key}:"):
                lines[i] = replacement
                break
    return _write_target(tmp_path, "\n".join(lines) + "\n")


def test_load_target_happy_path(tmp_path: Path) -> None:
    path = _write_target(tmp_path, _MINIMAL_VALID_YAML)
    cfg = load_target(path)
    assert cfg.target_total_bytes == 1_000_000_000_000
    assert cfg.mainnet_target["accounts"] == pytest.approx(0.141)
    assert cfg.source_sha256  # populated
    assert len(cfg.qp_scenarios) == 9
    assert cfg.total_batch_bytes == 10_000_000
    assert cfg.projection_eta == pytest.approx(0.5)


@pytest.mark.parametrize(
    "missing_key",
    [
        "mainnet_target",
        "target_total_bytes",
        "base_address",
        "revision",
        "qp_scenarios",
        "total_batch_bytes",
        "projection_eta",
    ],
)
def test_load_target_refuses_when_required_field_missing(tmp_path: Path, missing_key: str) -> None:
    """Every top-level field is required — silent defaults are a reproducibility hazard."""
    lines = _MINIMAL_VALID_YAML.strip().splitlines()
    # For dict-typed fields (mainnet_target), also drop the indented child lines.
    filtered: list[str] = []
    dropping_block = False
    for line in lines:
        stripped = line.lstrip()
        is_child = line.startswith(" ") or line.startswith("-") or line.startswith("\t")
        if dropping_block and is_child:
            continue
        dropping_block = False
        if stripped.startswith(f"{missing_key}:"):
            dropping_block = True
            continue
        filtered.append(line)
    path = _write_target(tmp_path, "\n".join(filtered) + "\n")
    with pytest.raises(ValueError, match=missing_key):
        load_target(path)


def test_load_target_refuses_when_qp_scenarios_empty(tmp_path: Path) -> None:
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
qp_scenarios: []
total_batch_bytes: 10000000
projection_eta: 0.5
""",
    )
    with pytest.raises(ValueError, match="qp_scenarios"):
        load_target(path)


def test_load_target_rejects_oversized_base_address(tmp_path: Path) -> None:
    path = _write_valid_target(
        tmp_path, base_address=f'base_address: "0x{"f" * 60}"'
    )
    with pytest.raises(ValueError, match="base_address"):
        load_target(path)


def test_load_target_rejects_non_hex_base_address(tmp_path: Path) -> None:
    path = _write_valid_target(tmp_path, base_address='base_address: "not-hex"')
    with pytest.raises(ValueError, match="base_address"):
        load_target(path)


def test_load_target_rejects_negative_revision(tmp_path: Path) -> None:
    path = _write_valid_target(tmp_path, revision="revision: -1")
    with pytest.raises(ValueError, match="revision"):
        load_target(path)


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
revision: 0
qp_scenarios:
  - eoatx
total_batch_bytes: 1000
projection_eta: 0.5
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
revision: 0
qp_scenarios:
  - eoatx
total_batch_bytes: 1000
projection_eta: 0.5
""",
    )
    cfg = load_target(path)
    assert cfg.byte_target("storage") == pytest.approx(800_000)
