"""Controller — picks the next batch's scenario mix via QP + Michelot + adaptive α.

Lifecycle:
    1. `probe` seeds `ControllerState.F` from measured observations.
    2. `pick_next_batch(observation, target)` computes a `BatchPlan`.
    3. `apply_observation(observation, plan)` updates F, σ, α in-place.

Overshoot detection uses a rolling window: fires ``ControllerInstability`` when
at least ``OVERSHOOT_WINDOW_TRIPS`` of the last ``OVERSHOOT_WINDOW`` batches (after
the grace period) had a residual-norm ratio exceeding ``OVERSHOOT_THRESHOLD``. A
single clean batch no longer resets the counter — previously that let a perfect
sawtooth oscillation run forever.
"""

from __future__ import annotations

from collections import deque
from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

import numpy as np

from .math.adaptive_alpha import A_MIN, update_coeff
from .math.michelot import project_simplex
from .reference_f import AXES, ReferenceF
from .sensor import StateObservation
from .target import TargetConfig

if TYPE_CHECKING:
    from .journal import Observability


OVERSHOOT_THRESHOLD = 0.20
OVERSHOOT_WINDOW = 6
OVERSHOOT_WINDOW_TRIPS = 4
OVERSHOOT_GRACE_BATCHES = 5
_RESIDUAL_NORM_FLOOR = 1024.0


class ControllerInstability(Exception):
    """Raised when the overshoot trigger fires; run should abort."""


@dataclass(frozen=True, slots=True)
class BatchPlan:
    verb: str
    deadline_bytes: int
    mix: dict[str, float]  # full simplex snapshot for observability


@dataclass
class ControllerState:
    F: dict[str, dict[str, float]]
    sigma: dict[str, dict[str, float]]
    alpha: dict[str, dict[str, float]] = field(default_factory=dict)
    batch_id: int = 0
    overshoot_window: deque[bool] = field(default_factory=lambda: deque(maxlen=OVERSHOOT_WINDOW))
    last_observation: StateObservation | None = None
    reference_f_version: str = ""

    def alpha_mean(self) -> float:
        """Scalar summary for the journal's legacy ``alpha_current`` field."""
        all_vals = [v for per in self.alpha.values() for v in per.values()]
        return sum(all_vals) / len(all_vals) if all_vals else A_MIN


def init_state(
    reference_f: ReferenceF,
    qp_scenarios: Iterable[str],
) -> ControllerState:
    """Initialize F from REFERENCE_F for the QP scenarios; σ starts at SIGMA_FLOOR."""
    scenarios = list(qp_scenarios)
    F: dict[str, dict[str, float]] = {}
    sigma: dict[str, dict[str, float]] = {}
    alpha: dict[str, dict[str, float]] = {}
    for verb in scenarios:
        F[verb] = dict(reference_f.per_scenario(verb))
        sigma[verb] = dict.fromkeys(AXES, 1.0)
        alpha[verb] = dict.fromkeys(AXES, A_MIN)
    return ControllerState(
        F=F,
        sigma=sigma,
        alpha=alpha,
        batch_id=0,
        reference_f_version=reference_f.version,
    )


def rehydrate_state(
    reference_f: ReferenceF,
    qp_scenarios: Iterable[str],
    tail_observability: Observability,
    last_batch_id: int,
) -> ControllerState:
    """Rebuild controller state from the last journal record's observability block.

    Design §C.3 step 6 requires F/σ/α to be reconstructed from the tail, not re-seeded
    from REFERENCE_F — otherwise every resume is a cold start and multi-session journals
    diverge from an uninterrupted run's ``final_state_root`` (spec §C.1).

    Per-(verb, axis) α is stored in ``Observability.alpha_state``; on legacy journals
    where that field is absent we broadcast the scalar ``alpha_current`` to every slot.
    """
    scenarios = list(qp_scenarios)
    state = init_state(reference_f, scenarios)
    scalar_alpha = (
        float(tail_observability.alpha_current) if tail_observability.alpha_current else A_MIN
    )
    alpha_state = getattr(tail_observability, "alpha_state", {}) or {}
    for verb in scenarios:
        coeffs = tail_observability.coeffs_after.get(verb)
        if coeffs is not None and all(axis in coeffs for axis in AXES):
            state.F[verb] = {axis: float(coeffs[axis]) for axis in AXES}
        sigma = tail_observability.sigma_innov.get(verb)
        if sigma is not None and all(axis in sigma for axis in AXES):
            state.sigma[verb] = {axis: float(sigma[axis]) for axis in AXES}
        per_verb = alpha_state.get(verb)
        if per_verb and all(axis in per_verb for axis in AXES):
            state.alpha[verb] = {axis: float(per_verb[axis]) for axis in AXES}
        else:
            state.alpha[verb] = dict.fromkeys(AXES, scalar_alpha)
    state.batch_id = last_batch_id + 1
    return state


class Controller:
    def __init__(self, state: ControllerState) -> None:
        self.state = state

    @property
    def F(self) -> dict[str, dict[str, float]]:
        return self.state.F

    def pick_next_batch(self, observation: StateObservation, target: TargetConfig) -> BatchPlan:
        """Project the desired scenario mix onto the simplex and pick the top verb."""
        verbs = list(target.qp_scenarios)
        residual = self._residual(observation, target)  # shape (3,)
        f_matrix = self._f_matrix(verbs)  # shape (3, n)
        x = np.full(len(verbs), 1.0 / len(verbs))  # warm start
        grad = 2.0 * f_matrix.T @ (f_matrix @ x - residual)
        # Normalize gradient so eta has a consistent effect regardless of residual magnitude.
        grad_norm = float(np.linalg.norm(grad))
        if grad_norm > 1e-12:
            grad = grad / grad_norm
        x_proj = project_simplex(x - target.projection_eta * grad)
        top_index = int(np.argmax(x_proj))
        top_verb = verbs[top_index]
        weight = float(x_proj[top_index])
        deadline = max(1, int(target.total_batch_bytes * weight))
        mix = {v: float(w) for v, w in zip(verbs, x_proj, strict=False)}
        return BatchPlan(verb=top_verb, deadline_bytes=deadline, mix=mix)

    def apply_observation(
        self,
        pre: StateObservation,
        post: StateObservation,
        plan: BatchPlan,
        *,
        tx_count: int = 1,
    ) -> dict[str, Any]:
        """Apply the adaptive-α update; returns a diagnostic dict for the journal.

        `tx_count` scales F (bytes per tx) to batch-total bytes for both the per-axis
        coefficient update (F is updated with observed/tx_count) and the overshoot check.
        """
        if tx_count <= 0:
            raise ValueError("tx_count must be positive")
        observed = {
            "accounts": float(post.account_bytes - pre.account_bytes),
            "storage": float(post.storage_bytes - pre.storage_bytes),
            "code": float(post.code_bytes - pre.code_bytes),
        }
        verb = plan.verb
        coeffs_before = dict(self.state.F[verb])
        max_abs_ratio = 0.0
        for axis in AXES:
            # Per-(verb, axis) α per design §2.4: "rule applied per-column of F".
            result = update_coeff(
                self.state.F[verb][axis],
                observed[axis] / tx_count,
                self.state.sigma[verb][axis],
                self.state.alpha[verb][axis],
            )
            self.state.F[verb][axis] = result.f_new
            self.state.sigma[verb][axis] = result.sigma_new
            self.state.alpha[verb][axis] = result.alpha_new
            max_abs_ratio = max(max_abs_ratio, result.abs_ratio)
        # L2 norm-ratio of the residual vector; matches design §4 semantics.
        commanded_vec = np.array(
            [self.state.F[verb][axis] * tx_count for axis in AXES], dtype=np.float64
        )
        obs_vec = np.array([observed[axis] for axis in AXES], dtype=np.float64)
        residual_norm = float(np.linalg.norm(obs_vec - commanded_vec))
        self._check_overshoot(residual_norm, commanded_vec)
        self.state.batch_id += 1
        self.state.last_observation = post
        return {
            "observed_flat_bytes": int(sum(observed.values())),
            "coeffs_before": {verb: coeffs_before},
            "coeffs_after": {verb: dict(self.state.F[verb])},
            "sigma_innov": {verb: dict(self.state.sigma[verb])},
            "alpha_current": self.state.alpha_mean(),
            "alpha_state": {verb: dict(self.state.alpha[verb])},
            "innovation_ratio": max_abs_ratio,
            "residual_norm": residual_norm,
        }

    def _residual(self, observation: StateObservation, target: TargetConfig) -> np.ndarray:
        current = np.array(
            [observation.account_bytes, observation.storage_bytes, observation.code_bytes],
            dtype=np.float64,
        )
        desired = np.array(
            [target.byte_target(a) for a in AXES],
            dtype=np.float64,
        )
        return desired - current

    def _f_matrix(self, verbs: list[str]) -> np.ndarray:
        rows = []
        for axis in AXES:
            rows.append([self.state.F[v][axis] for v in verbs])
        return np.array(rows, dtype=np.float64)

    def _check_overshoot(self, residual_norm: float, commanded: np.ndarray) -> None:
        """Windowed overshoot detector (H3).

        A single clean batch no longer resets the trip counter — a sawtooth
        oscillation with pattern [bad, bad, ok, bad, bad, ok, …] will saturate
        the window after ~9 batches and fire. The detector is gated by the
        grace period so probe-era transients don't trip it.
        """
        if self.state.batch_id < OVERSHOOT_GRACE_BATCHES:
            return
        denom = max(float(np.linalg.norm(commanded)), _RESIDUAL_NORM_FLOOR)
        ratio = residual_norm / denom
        tripped = ratio > OVERSHOOT_THRESHOLD
        self.state.overshoot_window.append(tripped)
        trips = sum(self.state.overshoot_window)
        if len(self.state.overshoot_window) == OVERSHOOT_WINDOW and trips >= OVERSHOOT_WINDOW_TRIPS:
            raise ControllerInstability(
                f"overshoot: {trips}/{OVERSHOOT_WINDOW} recent batches over "
                f"threshold={OVERSHOOT_THRESHOLD} (latest ratio {ratio:.3f})"
            )
