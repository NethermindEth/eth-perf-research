"""`target.yaml` loader.

Defines the mainnet-composition target, byte budget, and QP verb set. `composition_hash`
is assembled later in `manifest.py` — this loader just produces a `TargetConfig`.
"""
from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml


DEFAULT_QP_SCENARIOS = (
    "eoatx",
    "calltx",
    "deploytx",
    "factorydeploytx",
    "storagespam",
    "erc20_bloater",
    "erc20tx",
    "uniswap_swaps",
    "storagerefundtx",
)

DEFAULT_MAINNET_TARGET = {"accounts": 0.141, "storage": 0.817, "code": 0.043}


@dataclass(frozen=True)
class TargetConfig:
    mainnet_target: dict[str, float]
    target_total_bytes: int
    base_address: bytes
    revision: int
    qp_scenarios: tuple[str, ...] = DEFAULT_QP_SCENARIOS
    reference_f_path: Path | None = None
    total_batch_bytes: int = 10_000_000
    projection_eta: float = 0.5
    raw: dict[str, Any] = field(default_factory=dict)
    source_sha256: str = ""

    def byte_target(self, axis: str) -> float:
        return self.mainnet_target[axis] * self.target_total_bytes


def load_target(path: Path | str) -> TargetConfig:
    path = Path(path)
    raw_bytes = path.read_bytes()
    body = yaml.safe_load(raw_bytes) or {}
    mainnet = body.get("mainnet_target") or DEFAULT_MAINNET_TARGET
    _validate_mainnet_target(mainnet)
    base_addr_hex = body.get("base_address", "0x1000")
    base_addr = bytes.fromhex(base_addr_hex.removeprefix("0x")).rjust(20, b"\x00")
    return TargetConfig(
        mainnet_target={k: float(v) for k, v in mainnet.items()},
        target_total_bytes=int(body.get("target_total_bytes", 1_000_000_000_000)),
        base_address=base_addr,
        revision=int(body.get("revision", 0)),
        qp_scenarios=tuple(body.get("qp_scenarios", DEFAULT_QP_SCENARIOS)),
        reference_f_path=Path(body["reference_f_path"]) if body.get("reference_f_path") else None,
        total_batch_bytes=int(body.get("total_batch_bytes", 10_000_000)),
        projection_eta=float(body.get("projection_eta", 0.5)),
        raw=body,
        source_sha256=hashlib.sha256(raw_bytes).hexdigest(),
    )


def _validate_mainnet_target(mainnet: dict[str, float]) -> None:
    required = {"accounts", "storage", "code"}
    missing = required - set(mainnet.keys())
    if missing:
        raise ValueError(f"mainnet_target missing axes: {sorted(missing)}")
    total = sum(float(mainnet[a]) for a in required)
    if abs(total - 1.0) > 1e-6:
        raise ValueError(f"mainnet_target axes must sum to 1.0 (got {total:.6f})")
