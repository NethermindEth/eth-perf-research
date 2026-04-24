"""Controller closed-loop tests."""

from __future__ import annotations

import pytest

from orchestrator.controller import (
    OVERSHOOT_GRACE_BATCHES,
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
        total_batch_bytes=1_000_000,
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
    # α is now per-(verb, axis); legacy scalar alpha_current broadcasts to every slot.
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
