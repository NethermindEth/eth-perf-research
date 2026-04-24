"""Adaptive-α update tests."""
from __future__ import annotations

import pytest

from orchestrator.math.adaptive_alpha import (
    A_MAX,
    A_MIN,
    SIGMA_FLOOR,
    update_coeff,
)


def test_alpha_rises_on_disagreement() -> None:
    # Large innovation relative to F => abs_ratio > 0.12 => alpha_target near A_MAX.
    f_hat = 100.0
    sigma = 20.0
    result = update_coeff(f_hat, observed=200.0, sigma=sigma, alpha=A_MIN)
    assert result.alpha_new > A_MIN


def test_alpha_falls_on_agreement() -> None:
    alpha = 0.15
    for _ in range(50):
        r = update_coeff(100.0, observed=100.0, sigma=1.0, alpha=alpha)
        alpha = r.alpha_new
    assert alpha < 0.10


def test_huber_clips_outlier() -> None:
    # Observed 100× bigger than f_hat — raw innovation huge, but saturated innov capped at ~σ.
    f_hat = 100.0
    sigma = 10.0
    result = update_coeff(f_hat, observed=10_000.0, sigma=sigma, alpha=A_MIN)
    # sat = sigma * tanh(big) ≈ sigma (cap), so f_new - f_hat ≈ alpha * sigma.
    assert abs(result.saturated_innovation) <= sigma + 1e-9
    assert abs(result.f_new - f_hat) <= A_MAX * sigma + 1e-6


def test_update_is_pure() -> None:
    # Calling with the same inputs gives the same output (no hidden state).
    a = update_coeff(100.0, 110.0, 5.0, 0.1)
    b = update_coeff(100.0, 110.0, 5.0, 0.1)
    assert a == b


def test_sigma_floor_honored_on_small_sigma() -> None:
    # With sigma=0, denom falls back to SIGMA_FLOOR; tanh arg is raw/SIGMA_FLOOR.
    result = update_coeff(100.0, 101.0, 0.0, 0.1)
    assert result.sigma_new > 0  # running EWMA lifts it off zero
    assert abs(result.saturated_innovation) < SIGMA_FLOOR  # sigma=0 pins output at 0


def test_rejects_zero_sigma_floor() -> None:
    with pytest.raises(ValueError, match="sigma_floor"):
        update_coeff(100.0, 101.0, 5.0, 0.1, sigma_floor=0.0)


def test_rejects_nan_observed() -> None:
    with pytest.raises(ValueError, match="observed"):
        update_coeff(100.0, float("nan"), 5.0, 0.1)


def test_rejects_inf_f_hat() -> None:
    with pytest.raises(ValueError, match="f_hat"):
        update_coeff(float("inf"), 100.0, 5.0, 0.1)
