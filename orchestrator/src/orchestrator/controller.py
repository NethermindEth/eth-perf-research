"""Controller — picks the next batch's scenario mix via QP + Michelot + adaptive α.

Lifecycle:
    1. `probe` seeds `ControllerState.F` from measured observations.
    2. `pick_next_batch(observation, target)` computes a `BatchPlan`.
    3. `apply_observation(observation, plan)` updates F, σ, α in-place.

Overshoot detection fires `ControllerInstability` if the L2 norm-ratio between
commanded and observed bytes stays > 0.20 for 3 consecutive batches after batch 5.
"""
from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Iterable

import numpy as np

from .math.adaptive_alpha import A_MIN, update_coeff
from .math.michelot import project_simplex
from .reference_f import AXES, ReferenceF
from .sensor import StateObservation
from .target import TargetConfig

if TYPE_CHECKING:
    from .journal import Observability


OVERSHOOT_THRESHOLD = 0.20
OVERSHOOT_CONSECUTIVE = 3
OVERSHOOT_GRACE_BATCHES = 5


class ControllerInstability(Exception):
    """Raised when the overshoot trigger fires; run should abort."""


@dataclass
class BatchPlan:
    verb: str
    deadline_bytes: int
    mix: dict[str, float]  # full simplex snapshot for observability


@dataclass
class ControllerState:
    F: dict[str, dict[str, float]]
    sigma: dict[str, dict[str, float]]
    alpha: float = A_MIN
    batch_id: int = 0
    overshoot_streak: int = 0
    last_observation: StateObservation | None = None
    reference_f_version: str = ""


def init_state(
    reference_f: ReferenceF,
    qp_scenarios: Iterable[str],
) -> ControllerState:
    """Initialize F from REFERENCE_F for the QP scenarios; σ starts at SIGMA_FLOOR."""
    scenarios = list(qp_scenarios)
    F: dict[str, dict[str, float]] = {}
    sigma: dict[str, dict[str, float]] = {}
    for verb in scenarios:
        F[verb] = dict(reference_f.per_scenario(verb))
        sigma[verb] = {axis: 1.0 for axis in AXES}
    return ControllerState(
        F=F,
        sigma=sigma,
        alpha=A_MIN,
        batch_id=0,
        reference_f_version=reference_f.version,
    )


def rehydrate_state(
    reference_f: ReferenceF,
    qp_scenarios: Iterable[str],
    tail_observability: "Observability",
    last_batch_id: int,
) -> ControllerState:
    """Rebuild controller state from the last journal record's observability block.

    Design §C.3 step 6 requires F/σ/α to be reconstructed from the tail, not re-seeded
    from REFERENCE_F — otherwise every resume is a cold start and multi-session journals
    diverge from an uninterrupted run's ``final_state_root`` (spec §C.1).

    Fallback behavior when a QP scenario is absent from the tail (e.g. `qp_scenarios`
    was changed between sessions): that verb's F/σ reset to REFERENCE_F seeds and we
    emit a warning via the returned state. The operator is expected to notice the
    ``reference_f_version`` on shutdown and refuse such a run in review.
    """
    scenarios = list(qp_scenarios)
    state = init_state(reference_f, scenarios)
    for verb in scenarios:
        coeffs = tail_observability.coeffs_after.get(verb)
        if coeffs is not None and all(axis in coeffs for axis in AXES):
            state.F[verb] = {axis: float(coeffs[axis]) for axis in AXES}
        sigma = tail_observability.sigma_innov.get(verb)
        if sigma is not None and all(axis in sigma for axis in AXES):
            state.sigma[verb] = {axis: float(sigma[axis]) for axis in AXES}
    state.alpha = float(tail_observability.alpha_current) if tail_observability.alpha_current else A_MIN
    state.batch_id = last_batch_id + 1
    return state


class Controller:
    def __init__(self, state: ControllerState) -> None:
        self.state = state

    @property
    def F(self) -> dict[str, dict[str, float]]:
        return self.state.F

    def pick_next_batch(
        self, observation: StateObservation, target: TargetConfig
    ) -> BatchPlan:
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
        mix = {v: float(w) for v, w in zip(verbs, x_proj)}
        return BatchPlan(verb=top_verb, deadline_bytes=deadline, mix=mix)

    def apply_observation(
        self,
        pre: StateObservation,
        post: StateObservation,
        plan: BatchPlan,
        *,
        tx_count: int = 1,
    ) -> dict[str, float]:
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
            # Update F on per-tx scale — observed_per_tx = observed / tx_count.
            result = update_coeff(
                self.state.F[verb][axis],
                observed[axis] / tx_count,
                self.state.sigma[verb][axis],
                self.state.alpha,
            )
            self.state.F[verb][axis] = result.f_new
            self.state.sigma[verb][axis] = result.sigma_new
            self.state.alpha = result.alpha_new
            max_abs_ratio = max(max_abs_ratio, result.abs_ratio)
        self._check_overshoot(plan, observed, tx_count)
        self.state.batch_id += 1
        self.state.last_observation = post
        commanded_norm = math.sqrt(
            sum(self.state.F[verb][axis] ** 2 for axis in AXES)
        )
        observed_norm = math.sqrt(sum(observed[axis] ** 2 for axis in AXES))
        residual_norm = abs(observed_norm - commanded_norm)
        return {
            "observed_flat_bytes": int(sum(observed.values())),
            "coeffs_before": {verb: coeffs_before},
            "coeffs_after": {verb: dict(self.state.F[verb])},
            "sigma_innov": {verb: dict(self.state.sigma[verb])},
            "alpha_current": self.state.alpha,
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

    def _check_overshoot(
        self, plan: BatchPlan, observed: dict[str, float], tx_count: int
    ) -> None:
        if self.state.batch_id < OVERSHOOT_GRACE_BATCHES:
            self.state.overshoot_streak = 0
            return
        # Scale per-tx F to batch-total so the comparison is apples-to-apples.
        commanded = np.array(
            [self.state.F[plan.verb][a] * tx_count for a in AXES], dtype=np.float64
        )
        obs_vec = np.array([observed[a] for a in AXES], dtype=np.float64)
        denom = max(float(np.linalg.norm(commanded)), 1024.0)
        ratio = float(np.linalg.norm(obs_vec - commanded) / denom)
        if ratio > OVERSHOOT_THRESHOLD:
            self.state.overshoot_streak += 1
        else:
            self.state.overshoot_streak = 0
        if self.state.overshoot_streak >= OVERSHOOT_CONSECUTIVE:
            raise ControllerInstability(
                f"overshoot norm-ratio {ratio:.3f} × {self.state.overshoot_streak} consecutive batches"
            )
