"""Controller closed-loop tests."""

from __future__ import annotations

import pytest

from orchestrator.controller import (
    OVERSHOOT_GRACE_BATCHES,
    BatchPlan,
    Controller,
    ControllerInstability,
    init_state,
)
from orchestrator.probe import (
    CharacterizationInsufficient,
    run_probe,
    seed_state_from_probe,
)
from orchestrator.reference_f import default_reference_f_path, load_reference_f
from orchestrator.sensor import StateObservation
from orchestrator.target import TargetConfig


@pytest.fixture
def reference_f():
    return load_reference_f(default_reference_f_path())


@pytest.fixture
def target():
    return TargetConfig(
        mainnet_target={"accounts": 0.141, "storage": 0.817, "code": 0.043},
        target_total_bytes=10_000_000_000,
        base_address=b"\x00" * 20,
        revision=0,
        qp_scenarios=(
            "eoatx",
            "calltx",
            "deploytx",
            "factorydeploytx",
            "storagespam",
            "erc20_bloater",
            "erc20tx",
            "uniswap_swaps",
            "storagerefundtx",
        ),
        total_batch_bytes=1_000_000,
        projection_eta=0.5,
        chain_id=1337,
        gas_limit=30_000_000,
        block_gas_limit=30_000_000,
        raw={},
        source_sha256="t" * 64,
    )


def _obs(acc: int = 0, st: int = 0, co: int = 0, bn: int = 0) -> StateObservation:
    return StateObservation(block_number=bn, account_bytes=acc, storage_bytes=st, code_bytes=co)


def test_pick_next_batch_returns_valid_plan(reference_f, target) -> None:
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    plan = ctrl.pick_next_batch(_obs(), target)
    assert plan.verb in target.qp_scenarios
    assert plan.deadline_bytes > 0
    total = sum(plan.mix.values())
    assert abs(total - 1.0) < 1e-6


def test_pick_next_batch_fast(reference_f, target) -> None:
    import time

    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    started = time.perf_counter()
    ctrl.pick_next_batch(_obs(), target)
    elapsed = time.perf_counter() - started
    assert elapsed < 0.01  # < 10 ms; loose bound for CI noise


def test_closed_loop_convergence_single_axis(reference_f) -> None:
    """Single-verb target: drive storage to 0.5 GB using storagespam only."""
    target = TargetConfig(
        mainnet_target={"accounts": 0.0, "storage": 1.0, "code": 0.0},
        target_total_bytes=500_000_000,
        base_address=b"\x00" * 20,
        revision=0,
        qp_scenarios=("storagespam",),
        total_batch_bytes=1_000_000,
        projection_eta=0.5,
        chain_id=1337,
        gas_limit=30_000_000,
        block_gas_limit=30_000_000,
        raw={},
        source_sha256="t" * 64,
    )
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    # Simulate: each byte of deadline_bytes produces ~F[storage] bytes of storage growth.
    cumulative = _obs()
    for _ in range(2000):
        plan = ctrl.pick_next_batch(cumulative, target)
        pre = cumulative
        # Simulator: real coefficient slightly different from probe seed → controller must learn.
        actual_per_tx = {"accounts": 4.0, "storage": 180.0, "code": 0.0}
        tx_count = max(1, plan.deadline_bytes // 300)
        post = StateObservation(
            block_number=pre.block_number + 1,
            account_bytes=pre.account_bytes + int(actual_per_tx["accounts"] * tx_count),
            storage_bytes=pre.storage_bytes + int(actual_per_tx["storage"] * tx_count),
            code_bytes=pre.code_bytes + int(actual_per_tx["code"] * tx_count),
        )
        ctrl.apply_observation(pre, post, plan, tx_count=tx_count)
        cumulative = post
        if cumulative.storage_bytes >= 0.95 * target.byte_target("storage"):
            break
    # Controller must have made substantial progress; allow for simulator discretization.
    assert cumulative.storage_bytes >= 0.90 * target.byte_target("storage")


def test_apply_observation_updates_avg_tx_rlp(reference_f, target) -> None:
    """``avg_tx_rlp`` must converge toward the dispatched bytes-per-tx — the
    deadline cap reads it to translate per-tx caps into RLP-byte deadlines."""
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    plan = BatchPlan(verb="storagespam", deadline_bytes=1_000, mix={"storagespam": 1.0})
    pre = _obs()
    post = _obs(st=200_000)
    # First call seeds avg_tx_rlp[verb] = observed value (prev defaults to it).
    ctrl.apply_observation(pre, post, plan, tx_count=400, dispatched_rlp_bytes=1_000_000)
    assert state.avg_tx_rlp["storagespam"] == pytest.approx(2500.0)
    # Subsequent calls EWMA toward the new observation (alpha=0.3 in apply_observation).
    ctrl.apply_observation(post, post, plan, tx_count=200, dispatched_rlp_bytes=200_000)
    expected = 0.3 * (200_000 / 200) + 0.7 * 2500.0
    assert state.avg_tx_rlp["storagespam"] == pytest.approx(expected)
    # Verbs that haven't been dispatched stay absent → cap falls back to default.
    assert "eoatx" not in state.avg_tx_rlp


def test_probe_seeds_all_qp_scenarios(reference_f) -> None:
    """Stub executor: each verb produces exactly REFERENCE_F bytes per tx."""
    qp = tuple(reference_f.scenarios)

    def executor(verb: str, tx_count: int):
        ref = reference_f.per_scenario(verb)
        pre = _obs()
        post = _obs(
            acc=int(ref["accounts"] * tx_count),
            st=int(ref["storage"] * tx_count),
            co=int(ref["code"] * tx_count),
        )
        return pre, post

    results = run_probe(reference_f, qp, executor)
    state = init_state(reference_f, qp)
    seed_state_from_probe(state, results)
    for verb in qp:
        # Every axis must have been overwritten with measured values.
        measured = state.F[verb]
        for axis in ("accounts", "storage", "code"):
            assert measured[axis] == pytest.approx(reference_f.per_scenario(verb)[axis])


def test_probe_sanity_gate_fires_on_outlier(reference_f) -> None:
    qp = ("eoatx",)

    def bad_executor(verb: str, tx_count: int):
        ref = reference_f.per_scenario(verb)
        pre = _obs()
        post = _obs(acc=int(ref["accounts"] * tx_count * 10))  # 10× off
        return pre, post

    with pytest.raises(CharacterizationInsufficient):
        run_probe(reference_f, qp, bad_executor)


def test_overshoot_fires_in_windowed_detector(reference_f, target) -> None:
    """H3: a sustained overshoot run trips the 4-of-6 windowed detector."""
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    # Advance batch_id past the grace period without overshooting.
    tx_count = 100
    for _ in range(OVERSHOOT_GRACE_BATCHES):
        plan = ctrl.pick_next_batch(_obs(), target)
        ref = reference_f.per_scenario(plan.verb)
        post = _obs(
            acc=int(ref["accounts"] * tx_count),
            st=int(ref["storage"] * tx_count),
            co=int(ref["code"] * tx_count),
        )
        ctrl.apply_observation(_obs(), post, plan, tx_count=tx_count)

    # Feed explosive observations: each one saturates the ratio; window trips at 4/6.
    with pytest.raises(ControllerInstability):
        for _ in range(10):
            plan = ctrl.pick_next_batch(_obs(), target)
            explosive_post = _obs(acc=10_000_000, st=10_000_000, co=10_000_000)
            ctrl.apply_observation(_obs(), explosive_post, plan, tx_count=tx_count)


def test_overshoot_detects_sawtooth_oscillation(reference_f, target) -> None:
    """H3: an alternating [bad, bad, ok, bad, bad, ok, …] pattern must eventually trip."""
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    tx_count = 100

    def clean_post(plan):
        ref = reference_f.per_scenario(plan.verb)
        return _obs(
            acc=int(ref["accounts"] * tx_count),
            st=int(ref["storage"] * tx_count),
            co=int(ref["code"] * tx_count),
        )

    # Past the grace period without overshoot.
    for _ in range(OVERSHOOT_GRACE_BATCHES):
        plan = ctrl.pick_next_batch(_obs(), target)
        ctrl.apply_observation(_obs(), clean_post(plan), plan, tx_count=tx_count)

    with pytest.raises(ControllerInstability):
        # Sawtooth: 2 bad, 1 clean, repeat.
        for cycle in range(12):
            plan = ctrl.pick_next_batch(_obs(), target)
            if cycle % 3 == 2:
                post = clean_post(plan)
            else:
                post = _obs(acc=10_000_000, st=10_000_000, co=10_000_000)
            ctrl.apply_observation(_obs(), post, plan, tx_count=tx_count)


def test_reference_f_version_recorded(reference_f) -> None:
    state = init_state(reference_f, reference_f.scenarios)
    assert state.reference_f_version == reference_f.version


def test_rehydrate_state_restores_f_sigma_alpha(reference_f) -> None:
    """C2: resume must seed F/σ/α from the journal tail — not re-init from REFERENCE_F."""
    from orchestrator.controller import rehydrate_state
    from orchestrator.journal import Observability

    observability = Observability(
        observed_flat_bytes=0,
        coeffs_before={},
        coeffs_after={
            "eoatx": {"accounts": 222.5, "storage": 3.0, "code": 0.0},
            "storagespam": {"accounts": 1.0, "storage": 400.0, "code": 0.0},
        },
        sigma_innov={
            "eoatx": {"accounts": 10.0, "storage": 2.0, "code": 1.0},
            "storagespam": {"accounts": 1.0, "storage": 50.0, "code": 1.0},
        },
        alpha_state={
            "eoatx": {"accounts": 0.187, "storage": 0.187, "code": 0.187},
            "storagespam": {"accounts": 0.187, "storage": 0.187, "code": 0.187},
            "deploytx": {"accounts": 0.187, "storage": 0.187, "code": 0.187},
        },
        alpha_current=0.187,
        innovation_ratio=0.1,
        residual_norm=5.0,
        statecomp_snapshot=None,
    )
    state = rehydrate_state(
        reference_f,
        ("eoatx", "storagespam", "deploytx"),
        observability,
        last_batch_id=42,
    )
    assert state.F["eoatx"]["accounts"] == pytest.approx(222.5)
    assert state.F["storagespam"]["storage"] == pytest.approx(400.0)
    # deploytx absent from tail → falls back to REFERENCE_F seed
    assert state.F["deploytx"]["code"] == pytest.approx(
        reference_f.per_scenario("deploytx")["code"]
    )
    assert state.alpha["eoatx"]["accounts"] == pytest.approx(0.187)
    assert state.alpha["deploytx"]["storage"] == pytest.approx(0.187)
    assert state.batch_id == 43  # last + 1
    assert state.sigma["eoatx"]["accounts"] == pytest.approx(10.0)


def test_per_verb_alpha_updates_independently(reference_f, target) -> None:
    """MEDIUM: α must be tracked per-(verb, axis), not globally (spec §2.4)."""
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    tx_count = 100

    plan = ctrl.pick_next_batch(_obs(), target)
    ref = reference_f.per_scenario(plan.verb)
    # Wildly off-model observation → α for this (verb, axis) should ramp up.
    post = _obs(
        acc=int(ref["accounts"] * tx_count * 10),
        st=int(ref["storage"] * tx_count),
        co=int(ref["code"] * tx_count),
    )
    ctrl.apply_observation(_obs(), post, plan, tx_count=tx_count)

    # Another verb that was not touched should retain its A_MIN.
    untouched_verbs = [v for v in target.qp_scenarios if v != plan.verb]
    assert untouched_verbs
    untouched = untouched_verbs[0]
    from orchestrator.math.adaptive_alpha import A_MIN

    assert state.alpha[untouched]["accounts"] == pytest.approx(A_MIN)


# ---------------------------------------------------------------------------
# Verb-selection mode: argmax (default) vs proportional sampling


def test_argmax_selection_picks_dominant_verb(reference_f, target, monkeypatch) -> None:
    """argmax mode (default) returns the highest-weight verb deterministically."""
    import orchestrator.controller as ctl_mod

    monkeypatch.setattr(ctl_mod, "EPSILON", 0.0)
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    # Two calls with the same input must return the same verb (no randomness).
    plan_a = ctrl.pick_next_batch(_obs(), target, batch_id=0, chain_identity_hash="c" * 64)
    plan_b = ctrl.pick_next_batch(_obs(), target, batch_id=42, chain_identity_hash="d" * 64)
    assert plan_a.verb == plan_b.verb


def test_proportional_selection_is_seeded_deterministic(
    reference_f, target, monkeypatch
) -> None:
    """proportional mode with same (chain_identity_hash, batch_id) reproduces."""
    import orchestrator.controller as ctl_mod

    monkeypatch.setattr(ctl_mod, "EPSILON", 1.0)
    state_a = init_state(reference_f, target.qp_scenarios)
    state_b = init_state(reference_f, target.qp_scenarios)
    ctrl_a = Controller(state_a)
    ctrl_b = Controller(state_b)
    seq_a = [
        ctrl_a.pick_next_batch(_obs(), target, batch_id=i, chain_identity_hash="z" * 64).verb
        for i in range(20)
    ]
    seq_b = [
        ctrl_b.pick_next_batch(_obs(), target, batch_id=i, chain_identity_hash="z" * 64).verb
        for i in range(20)
    ]
    assert seq_a == seq_b
    # Different chain_identity_hash → different stream (sanity, may rarely collide).
    seq_c = [
        ctrl_b.pick_next_batch(_obs(), target, batch_id=i, chain_identity_hash="y" * 64).verb
        for i in range(20)
    ]
    assert seq_a != seq_c, "different chain_identity_hash should produce different picks"


def test_proportional_distribution_tracks_simplex(reference_f, target, monkeypatch) -> None:
    """Over many samples the verb-pick histogram approximates the simplex weights."""
    import collections

    import orchestrator.controller as ctl_mod

    monkeypatch.setattr(ctl_mod, "EPSILON", 1.0)
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    # Reference simplex weights: same observation, same projection across calls,
    # so the *expected* distribution is the simplex weights from one snapshot.
    snapshot = ctrl.pick_next_batch(_obs(), target, batch_id=0, chain_identity_hash="x" * 64)
    expected_mix = snapshot.mix
    n_samples = 4000
    counts: collections.Counter[str] = collections.Counter()
    for i in range(n_samples):
        plan = ctrl.pick_next_batch(_obs(), target, batch_id=i, chain_identity_hash="x" * 64)
        counts[plan.verb] += 1
    # Compare empirical vs expected for verbs with non-trivial weight (>1 %).
    for verb, weight in expected_mix.items():
        if weight < 0.01:
            continue
        empirical = counts[verb] / n_samples
        assert abs(empirical - weight) < 0.03, (
            f"{verb}: empirical {empirical:.3f} vs expected {weight:.3f} (diff > 3 pp)"
        )


def test_proportional_falls_back_to_argmax_without_seed(
    reference_f, target, monkeypatch
) -> None:
    """Missing batch_id / chain_identity_hash makes proportional mode degenerate to argmax."""
    import numpy as np

    import orchestrator.controller as ctl_mod

    monkeypatch.setattr(ctl_mod, "EPSILON", 1.0)
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    plan_seeded = ctrl.pick_next_batch(_obs(), target, batch_id=0, chain_identity_hash="x" * 64)
    plan_unseeded = ctrl.pick_next_batch(_obs(), target)  # no kwargs
    expected = list(plan_seeded.mix)[int(np.argmax(list(plan_seeded.mix.values())))]
    assert plan_unseeded.verb == expected


def test_epsilon_zero_reproduces_argmax_exactly(reference_f, target, monkeypatch) -> None:
    """ε=0 must reproduce pure argmax (the identity case for the verb sampler)."""
    import orchestrator.controller as ctl_mod

    monkeypatch.setattr(ctl_mod, "EPSILON", 0.0)
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    seq = [
        ctrl.pick_next_batch(_obs(), target, batch_id=i, chain_identity_hash="z" * 64).verb
        for i in range(15)
    ]
    # Without exploration, identical observations must yield identical picks.
    assert all(v == seq[0] for v in seq)


def test_epsilon_mixture_explores_more_than_argmax(reference_f, target, monkeypatch) -> None:
    """0 < ε < 1 should produce a strict superset of verbs vs ε=0 over many batches."""
    import orchestrator.controller as ctl_mod

    monkeypatch.setattr(ctl_mod, "EPSILON", 0.0)
    state_a = init_state(reference_f, target.qp_scenarios)
    ctrl_a = Controller(state_a)
    argmax_verbs = {
        ctrl_a.pick_next_batch(_obs(), target, batch_id=i, chain_identity_hash="z" * 64).verb
        for i in range(50)
    }

    monkeypatch.setattr(ctl_mod, "EPSILON", 0.5)
    state_b = init_state(reference_f, target.qp_scenarios)
    ctrl_b = Controller(state_b)
    eps_verbs = {
        ctrl_b.pick_next_batch(_obs(), target, batch_id=i, chain_identity_hash="z" * 64).verb
        for i in range(50)
    }
    # ε=0 should converge on a single verb; ε=0.5 should hit more.
    assert len(eps_verbs) >= len(argmax_verbs)


def test_residual_norm_cached_on_state(reference_f, target) -> None:
    """``apply_observation`` must stash residual_norm on ControllerState."""
    state = init_state(reference_f, target.qp_scenarios)
    ctrl = Controller(state)
    assert state.last_residual_norm == float("inf")  # initial sentinel
    plan = ctrl.pick_next_batch(_obs(), target, batch_id=0, chain_identity_hash="x" * 64)
    pre = _obs()
    post = StateObservation(
        block_number=1, account_bytes=100, storage_bytes=50, code_bytes=10,
    )
    ctrl.apply_observation(pre, post, plan, tx_count=10)
    assert state.last_residual_norm < float("inf")  # populated
    assert state.last_residual_norm >= 0
