"""Michelot (1986) projection onto the unit probability simplex.

Given `x ∈ R^n`, returns `x* ∈ Δ = {x ≥ 0, 1ᵀx = 1}` minimizing `‖x − x*‖₂`.
Complexity: O(n log n).

Reference: final-design-v3.md §A.1 and math-explained.md §5.
"""

from __future__ import annotations

import numpy as np


def project_simplex(x: np.ndarray) -> np.ndarray:
    """Project a vector onto the unit simplex. Returns a new array.

    Raises ``ValueError`` on NaN/Inf input — corrupt controller inputs must not
    silently produce a numerically-plausible simplex point. The general formula
    handles ``n == 1`` correctly on its own (it collapses to ``[1.0]``), so the
    special case is dropped to keep one code path under test.
    """
    x = np.asarray(x, dtype=np.float64).ravel()
    n = x.size
    if n == 0:
        return x
    if not np.all(np.isfinite(x)):
        raise ValueError("project_simplex requires finite inputs; got NaN/Inf")
    sorted_desc = np.sort(x)[::-1]
    cumulative = np.cumsum(sorted_desc)
    # Find the largest k where sorted_desc[k] + (1 - cumulative[k]) / (k+1) > 0.
    indices = np.arange(1, n + 1)
    threshold_candidates = sorted_desc + (1.0 - cumulative) / indices
    k = np.where(threshold_candidates > 0)[0]
    if k.size == 0:
        # Numerical edge case: put all mass on the largest coordinate.
        out = np.zeros_like(x)
        out[int(np.argmax(x))] = 1.0
        return out
    rho = k[-1]
    tau = (cumulative[rho] - 1.0) / (rho + 1)
    return np.maximum(x - tau, 0.0)
