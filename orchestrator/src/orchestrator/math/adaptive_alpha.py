"""Huber-saturated innovation update with tanh-scheduled learning rate.

See final-design-v3.md §2.4 for the exact formula. Per-axis, per-verb.
"""
from __future__ import annotations

import math
from dataclasses import dataclass


# Design-v3 defaults (§4).
A_MIN = 0.02
A_MAX = 0.30
C = 0.08
K = 25.0
SIGMA_FLOOR = 1.0
EPS = 1000.0
SIGMA_EWMA_DECAY = 0.9
ALPHA_EWMA_DECAY = 0.7


@dataclass
class AlphaUpdate:
    f_new: float
    sigma_new: float
    alpha_new: float
    raw_innovation: float
    saturated_innovation: float
    abs_ratio: float


def update_coeff(
    f_hat: float,
    observed: float,
    sigma: float,
    alpha: float,
    *,
    a_min: float = A_MIN,
    a_max: float = A_MAX,
    c: float = C,
    k: float = K,
    sigma_floor: float = SIGMA_FLOOR,
    eps: float = EPS,
) -> AlphaUpdate:
    """Apply the adaptive-α innovation update for a single (axis, verb) coefficient."""
    if sigma_floor <= 0.0:
        raise ValueError("sigma_floor must be > 0 to avoid division by zero in tanh arg")
    for label, value in (("f_hat", f_hat), ("observed", observed), ("sigma", sigma), ("alpha", alpha)):
        if not math.isfinite(value):
            raise ValueError(f"update_coeff({label}) must be finite, got {value}")
    raw = observed - f_hat
    denom = max(sigma, sigma_floor)
    sat = sigma * math.tanh(raw / denom)
    sigma_new = SIGMA_EWMA_DECAY * sigma + (1.0 - SIGMA_EWMA_DECAY) * abs(raw)
    abs_ratio = abs(sat) / max(abs(f_hat), eps)
    alpha_target = a_min + (a_max - a_min) / 2.0 * (1.0 + math.tanh(k * (abs_ratio - c)))
    alpha_new = ALPHA_EWMA_DECAY * alpha + (1.0 - ALPHA_EWMA_DECAY) * alpha_target
    f_new = f_hat + alpha_new * sat
    return AlphaUpdate(
        f_new=f_new,
        sigma_new=sigma_new,
        alpha_new=alpha_new,
        raw_innovation=raw,
        saturated_innovation=sat,
        abs_ratio=abs_ratio,
    )
