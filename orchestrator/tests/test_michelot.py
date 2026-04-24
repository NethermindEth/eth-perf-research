"""Michelot projection tests."""
from __future__ import annotations

import numpy as np
import pytest

from orchestrator.math.michelot import project_simplex


def test_n1_returns_1() -> None:
    assert project_simplex(np.array([0.5])).tolist() == [1.0]


def test_already_on_simplex() -> None:
    x = np.array([0.2, 0.3, 0.5])
    out = project_simplex(x)
    np.testing.assert_allclose(out, x, atol=1e-12)


def test_negative_coordinates_clipped() -> None:
    # Design doc §A.1 worked example: project [0.8, 0.5, -0.2, 0.4]
    x = np.array([0.8, 0.5, -0.2, 0.4])
    out = project_simplex(x)
    assert abs(out.sum() - 1.0) < 1e-9
    assert (out >= 0).all()
    # Negative input coordinate must be zero in output.
    assert out[2] == 0.0


@pytest.mark.parametrize("seed", list(range(20)))
def test_random_vectors_land_on_simplex(seed: int) -> None:
    rng = np.random.default_rng(seed)
    for n in (1, 2, 5, 9, 12, 16):
        x = rng.standard_normal(n) * 10
        out = project_simplex(x)
        assert abs(out.sum() - 1.0) < 1e-9
        assert (out >= -1e-12).all()


def test_uniform_vector_stays_uniform() -> None:
    out = project_simplex(np.full(5, 0.2))
    np.testing.assert_allclose(out, np.full(5, 0.2), atol=1e-12)
