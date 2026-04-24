"""`target.yaml` loader.

Defines the mainnet-composition target, byte budget, and QP verb set. `composition_hash`
is assembled later in `manifest.py` — this loader just produces a `TargetConfig`.

Every YAML field is required — the orchestrator refuses to run on a silent default because
silent defaults hide configuration drift across runs. An operator who's serious about the
reproducibility contract (same target.yaml → same composition_hash → same journal identity)
should have to state each choice explicitly.
"""

from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import yaml

_BASE_ADDRESS_RE = re.compile(r"^0x[0-9a-fA-F]{1,40}$")


@dataclass(frozen=True)
class TargetConfig:
    mainnet_target: dict[str, float]
    target_total_bytes: int
    base_address: bytes
    revision: int
    qp_scenarios: tuple[str, ...]
    total_batch_bytes: int
    projection_eta: float
    raw: dict[str, Any]
    source_sha256: str
    reference_f_path: Path | None = None

    def byte_target(self, axis: str) -> float:
        return self.mainnet_target[axis] * self.target_total_bytes


def load_target(path: Path | str) -> TargetConfig:
    path = Path(path)
    raw_bytes = path.read_bytes()
    body = yaml.safe_load(raw_bytes) or {}

    mainnet = _require(body, "mainnet_target", dict)
    _validate_mainnet_target(mainnet)
    base_addr = _parse_base_address(_require(body, "base_address", str))
    revision = int(_require(body, "revision", int))
    if revision < 0 or revision >= (1 << 80):
        raise ValueError(f"revision out of range: {revision}")

    qp_scenarios_raw = _require(body, "qp_scenarios", list)
    if not qp_scenarios_raw:
        raise ValueError("qp_scenarios must be a non-empty list")

    return TargetConfig(
        mainnet_target={k: float(v) for k, v in mainnet.items()},
        target_total_bytes=int(_require(body, "target_total_bytes", int)),
        base_address=base_addr,
        revision=revision,
        qp_scenarios=tuple(str(v) for v in qp_scenarios_raw),
        total_batch_bytes=int(_require(body, "total_batch_bytes", int)),
        projection_eta=float(_require(body, "projection_eta", (int, float))),
        reference_f_path=(
            Path(body["reference_f_path"]) if body.get("reference_f_path") is not None else None
        ),
        raw=body,
        source_sha256=hashlib.sha256(raw_bytes).hexdigest(),
    )


def _require(body: dict[str, Any], key: str, expected: type | tuple[type, ...]) -> Any:
    """Pull ``key`` from ``body`` or raise ``ValueError``. Enforces the top-level YAML type.

    Missing-vs-wrong-type distinction matters: ``revision: "zero"`` and a missing
    ``revision:`` field fail for different reasons and the operator should see which.
    """
    if key not in body:
        raise ValueError(f"target.yaml missing required field: {key!r}")
    value = body[key]
    if not isinstance(value, expected):
        expected_name = (
            expected.__name__
            if isinstance(expected, type)
            else "/".join(t.__name__ for t in expected)
        )
        raise ValueError(
            f"target.yaml field {key!r} must be {expected_name}, got {type(value).__name__}"
        )
    return value


def _parse_base_address(value: Any) -> bytes:
    """Parse ``0x``-prefixed hex into a 20-byte big-endian address. Rejects oversize input."""
    if not isinstance(value, str) or not _BASE_ADDRESS_RE.fullmatch(value):
        raise ValueError(f"base_address must match ^0x[0-9a-fA-F]{{1,40}}$, got {value!r}")
    try:
        raw = bytes.fromhex(value.removeprefix("0x"))
    except ValueError as exc:
        raise ValueError(f"base_address hex parse failed: {exc}") from exc
    return raw.rjust(20, b"\x00")


def _validate_mainnet_target(mainnet: dict[str, float]) -> None:
    required = {"accounts", "storage", "code"}
    missing = required - set(mainnet.keys())
    if missing:
        raise ValueError(f"mainnet_target missing axes: {sorted(missing)}")
    total = sum(float(mainnet[a]) for a in required)
    # Spec §B.3 fractions (0.141/0.817/0.043) sum to 1.001 due to rounding in the
    # Paradigm 2024 report; accept any rounding noise up to 1 pp. Larger skews are
    # genuine misconfiguration and still raise.
    if abs(total - 1.0) > 0.01:
        raise ValueError(f"mainnet_target axes must sum to 1.0 (got {total:.6f})")
