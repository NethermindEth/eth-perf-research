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

import hashlib
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


import os as _os

# Overshoot detector knobs — env-tunable so demo / smoke runs can relax the
# safety net without recompiling. Production defaults are unchanged.
OVERSHOOT_THRESHOLD = float(_os.environ.get("ORCH_OVERSHOOT_THRESHOLD", "0.20"))
OVERSHOOT_WINDOW = int(_os.environ.get("ORCH_OVERSHOOT_WINDOW", "6"))
OVERSHOOT_WINDOW_TRIPS = int(_os.environ.get("ORCH_OVERSHOOT_WINDOW_TRIPS", "4"))
OVERSHOOT_GRACE_BATCHES = int(_os.environ.get("ORCH_OVERSHOOT_GRACE_BATCHES", "5"))
_RESIDUAL_NORM_FLOOR = 1024.0

# Verb-selection mixture. ``ORCH_EPSILON`` (default ``0.0``) is the probability
# that the next verb is *sampled* from the simplex weights instead of taken
# greedily as the argmax. ``0`` reproduces the original argmax mode; ``1``
# reproduces full proportional sampling; ``0.1`` gives mostly-greedy with
# occasional exploration ("ε-greedy" with sampling-from-projection rather
# than uniform). The RNG is seeded per-batch from (chain_identity_hash,
# target_sha256, batch_id) so forward runs of the same chain + target
# reproduce the same pick sequence; a different target.yaml shape explores
# a different verb stream while the chain identity stays stable.
EPSILON = float(_os.environ.get("ORCH_EPSILON", "0.0"))
if not 0.0 <= EPSILON <= 1.0:
    raise ValueError(f"ORCH_EPSILON must be in [0, 1], got {EPSILON}")

# Overshoot penalty (λ) for the QP gradient. When > 0, the projection's
# gradient is augmented with ``λ · 2·F · max(0, F·x·N - residual)`` so mixes
# whose predicted single-step delta would push any axis past its target are
# pulled back. Default 0 (disabled, identical to today's projector).
OVERSHOOT_PENALTY = float(_os.environ.get("ORCH_OVERSHOOT_PENALTY", "0.0"))
if OVERSHOOT_PENALTY < 0:
    raise ValueError(f"ORCH_OVERSHOOT_PENALTY must be ≥ 0, got {OVERSHOOT_PENALTY}")


class ControllerInstability(Exception):
    """Raised when the overshoot trigger fires; run should abort."""


@dataclass(frozen=True, slots=True)
class BatchPlan:
    verb: str
    deadline_bytes: int
    mix: dict[str, float]  # full simplex snapshot for observability


_DEFAULT_AVG_TX_RLP = 1500.0


@dataclass
class ControllerState:
    F: dict[str, dict[str, float]]
    sigma: dict[str, dict[str, float]]
    alpha: dict[str, dict[str, float]] = field(default_factory=dict)
    batch_id: int = 0
    overshoot_window: deque[bool] = field(default_factory=lambda: deque(maxlen=OVERSHOOT_WINDOW))
    last_observation: StateObservation | None = None
    reference_f_version: str = ""
    # Latest residual_norm cached after each apply_observation so the
    # lifecycle's soft-stop (ORCH_RESIDUAL_STOP_THRESHOLD) can read it
    # without recomputing. Inf until the first observation.
    last_residual_norm: float = float("inf")
    avg_tx_rlp: dict[str, float] = field(default_factory=dict)

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


def _select_verb_index(
    x_proj: np.ndarray,
    batch_id: int | None,
    chain_identity_hash: str | None,
    target_sha256: str | None = None,
) -> int:
    """Pick a verb index from the simplex projection.

    With probability ``1 - EPSILON`` take ``argmax(x_proj)`` (greedy exploit);
    otherwise sample from ``x_proj`` (weighted explore). The RNG is seeded
    from ``(chain_identity_hash, target_sha256, batch_id)`` so two forward
    runs of the same chain + target reproduce the same picks; a different
    ``target.yaml`` shape explores a different verb stream while the chain
    identity stays stable. Falls back to pure argmax if any seed input is
    missing, the projection produced a degenerate distribution, or
    ``EPSILON`` is exactly 0.
    """
    if EPSILON <= 0.0 or batch_id is None or chain_identity_hash is None:
        return int(np.argmax(x_proj))
    total = float(x_proj.sum())
    if total <= 0.0 or not np.isfinite(total):
        return int(np.argmax(x_proj))
    seed_input = f"{chain_identity_hash}:{target_sha256 or ''}:{batch_id}"
    seed = int.from_bytes(
        hashlib.sha256(seed_input.encode()).digest()[:8],
        "big",
    )
    rng = np.random.default_rng(seed)
    if rng.random() >= EPSILON:
        return int(np.argmax(x_proj))
    p = x_proj / total
    return int(rng.choice(len(p), p=p))


class Controller:
    def __init__(self, state: ControllerState) -> None:
        self.state = state

    @property
    def F(self) -> dict[str, dict[str, float]]:
        return self.state.F

    def pick_next_batch(
        self,
        observation: StateObservation,
        target: TargetConfig,
        *,
        batch_id: int | None = None,
        chain_identity_hash: str | None = None,
        composition_hash: str | None = None,
    ) -> BatchPlan:
        """Project the desired scenario mix onto the simplex and pick a verb.

        Greedy by default (argmax of the projected mix). When ``EPSILON > 0``
        a fraction ``ε`` of batches are sampled from the projected mix
        instead — keeps F estimates fresh across the registry. The RNG is
        seeded from ``(chain_identity_hash, target.sha256, batch_id)`` for
        reproducibility — same chain + same target ⇒ same picks; a target
        swap shifts the explore stream while keeping the chain stable.

        ``composition_hash`` is accepted for back-compat with pre-v2 callers
        (it is forwarded as ``chain_identity_hash`` if the latter is None).
        """
        if chain_identity_hash is None and composition_hash is not None:
            chain_identity_hash = composition_hash
        verbs = list(target.qp_scenarios)
        residual = self._residual(observation, target)
        f_matrix = self._f_matrix(verbs)
        x = np.full(len(verbs), 1.0 / len(verbs))
        current_arr = np.array(
            [observation.account_bytes, observation.storage_bytes, observation.code_bytes],
            dtype=np.float64,
        )
        target_arr = np.array([target.byte_target(a) for a in AXES], dtype=np.float64)
        cum = float(current_arr.sum())
        # State is monotone non-decreasing: at endgame (cum past target_total
        # with ≥1 axis still under absolute target) clip the negative residual
        # components so the under-served axis isn't drowned out in the gradient.
        endgame = cum >= float(target.target_total_bytes) and (current_arr < target_arr).any()
        residual_for_grad = np.maximum(0.0, residual) if endgame else residual
        grad = 2.0 * f_matrix.T @ (f_matrix @ x - residual_for_grad)
        if OVERSHOOT_PENALTY > 0.0:
            n_full = max(1.0, float(target.total_batch_bytes))
            predicted_delta = (f_matrix @ x) * n_full
            overshoot = np.maximum(0.0, predicted_delta - residual)
            grad = grad + OVERSHOOT_PENALTY * 2.0 * f_matrix.T @ overshoot * n_full
        grad_norm = float(np.linalg.norm(grad))
        if grad_norm > 1e-12:
            grad = grad / grad_norm
        x_proj = project_simplex(x - target.projection_eta * grad)
        # Per-axis trajectory cap. n_txs is bounded by headroom / per-tx
        # *excess over the proportional rate*; under-emitters don't constrain
        # — they just move the line closer in our favor. Without this cap a
        # single full-budget eoatx batch (F_a=160, p_a·F_sum≈14) blows the
        # accounts line by 10× even when the terminal target is far away.
        progress = min(cum / max(float(target.target_total_bytes), 1.0), 1.0)
        desired_arr = progress * target_arr
        p_arr = np.array([target.mainnet_target[a] for a in AXES], dtype=np.float64)
        # Floor tolerance: p=0 axes would otherwise force 1-tx batches.
        tolerance = np.maximum(p_arr, 0.005) * float(target.total_batch_bytes)
        headroom = np.maximum(0.0, desired_arr - current_arr) + tolerance
        max_n_txs = np.full(len(verbs), np.inf)
        for i, v in enumerate(verbs):
            f_row = np.array([self.state.F[v][a] for a in AXES], dtype=np.float64)
            f_sum = float(f_row.sum())
            if f_sum <= 0:
                continue
            excess = f_row - p_arr * f_sum
            over = excess > 0
            if over.any():
                max_n_txs[i] = float(np.min(headroom[over] / excess[over]))
        feasible = max_n_txs >= 1.0
        select_from = x_proj * feasible if feasible.any() else x_proj
        s = float(select_from.sum())
        if s > 0:
            select_from = select_from / s
        top_index = _select_verb_index(
            select_from,
            batch_id,
            chain_identity_hash,
            target.source_sha256 if isinstance(target, TargetConfig) else None,
        )
        top_verb = verbs[top_index]
        avg_rlp = self.state.avg_tx_rlp.get(top_verb, _DEFAULT_AVG_TX_RLP)
        n_safe = max_n_txs[top_index]
        if np.isfinite(n_safe):
            safe_deadline = int(n_safe * avg_rlp)
            deadline = max(1, min(int(target.total_batch_bytes), safe_deadline))
        else:
            deadline = max(1, int(target.total_batch_bytes))
        mix = {v: float(w) for v, w in zip(verbs, x_proj, strict=False)}
        return BatchPlan(verb=top_verb, deadline_bytes=deadline, mix=mix)

    def apply_observation(
        self,
        pre: StateObservation,
        post: StateObservation,
        plan: BatchPlan,
        *,
        tx_count: int = 1,
        dispatched_rlp_bytes: int | None = None,
    ) -> dict[str, Any]:
        """Apply the adaptive-α update; returns a diagnostic dict for the journal.

        `tx_count` scales F (bytes per tx) to batch-total bytes for both the per-axis
        coefficient update (F is updated with observed/tx_count) and the overshoot check.
        ``dispatched_rlp_bytes`` feeds the EWMA the next batch's deadline cap reads
        from ``state.avg_tx_rlp[verb]``.
        """
        if tx_count <= 0:
            raise ValueError("tx_count must be positive")
        if dispatched_rlp_bytes is not None and dispatched_rlp_bytes > 0:
            observed_avg = dispatched_rlp_bytes / tx_count
            prev = self.state.avg_tx_rlp.get(plan.verb, observed_avg)
            self.state.avg_tx_rlp[plan.verb] = 0.3 * observed_avg + 0.7 * prev
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
        self.state.last_residual_norm = residual_norm
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
        cum = float(current.sum())
        progress = min(cum / max(float(target.target_total_bytes), 1.0), 1.0)
        desired = np.array(
            [progress * target.byte_target(a) for a in AXES],
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
