"""REFERENCE_F loader — version-stamped seed coefficients (bytes per tx, per axis)."""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path

AXES = ("accounts", "storage", "code")


@dataclass(frozen=True)
class ReferenceF:
    version: str
    tolerance: float
    coefficients: dict[str, dict[str, float]]
    source_path: Path

    def per_scenario(self, verb: str) -> dict[str, float]:
        # Verbs not in the reference dataset (e.g. ``noop`` — added after the
        # JSON was minted) seed at zero on every axis. Zero F is honest for
        # scenarios with no a priori expectation; the adaptive-α update will
        # populate real values once the verb is observed.
        return self.coefficients.get(verb, dict.fromkeys(AXES, 0.0))

    @property
    def scenarios(self) -> list[str]:
        return sorted(self.coefficients.keys())


def load_reference_f(path: Path | str) -> ReferenceF:
    path = Path(path)
    body = json.loads(path.read_text(encoding="utf-8"))
    coeffs = body["coefficients"]
    for verb, axes in coeffs.items():
        missing = set(AXES) - set(axes.keys())
        if missing:
            raise ValueError(f"REFERENCE_F[{verb}] missing axes: {sorted(missing)}")
    return ReferenceF(
        version=body["version"],
        tolerance=float(body.get("tolerance", 0.30)),
        coefficients={v: {a: float(axes[a]) for a in AXES} for v, axes in coeffs.items()},
        source_path=path,
    )


def default_reference_f_path() -> Path:
    """Return the bundled REFERENCE_F JSON — used when no explicit path is given."""
    root = Path(__file__).resolve().parent.parent.parent  # orchestrator/
    return root / "reference_f" / "REFERENCE_F-2026.04.23.json"
