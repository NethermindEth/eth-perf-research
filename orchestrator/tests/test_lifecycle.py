"""Lifecycle tests: auto-detect startup, resume guard, main loop control flow."""

from __future__ import annotations

from pathlib import Path

import pytest

from orchestrator.facade import FacadeContext
from orchestrator.journal import (
    JournalReader,
    JournalWriter,
    Observability,
    PendingBatch,
    Record,
    ReplayCore,
    write_pending,
)
from orchestrator.lifecycle import (
    LifecycleDeps,
    ResumeRefused,
    RunAlreadyActive,
    StartupMode,
    _state_dir_lock,
    resolve_startup_mode,
    run,
)
from orchestrator.manifest import EnvInfo, Manifest, compute_chain_identity_hash
from orchestrator.target import TargetConfig


def _env() -> EnvInfo:
    return EnvInfo(
        genesis_sha256="g" * 64,
        plugin_git_sha="p" * 40,
        nethermind_commit_sha="n" * 40,
        dotnet_runtime_major=10,
        cpu_arch="x86_64",
    )


def _target() -> TargetConfig:
    return TargetConfig(
        mainnet_target={"accounts": 0.141, "storage": 0.817, "code": 0.042},
        target_total_bytes=1_000_000_000,
        base_address=(0x10_00).to_bytes(20, "big"),
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
        total_batch_bytes=500_000,
        projection_eta=0.5,
        raw={},
        source_sha256="a" * 64,
    )


def test_resolve_fresh_when_no_journal(tmp_path: Path) -> None:
    decision = resolve_startup_mode(tmp_path, composition_hash="abc", head_block=None)
    assert decision.mode is StartupMode.FRESH


def test_resolve_resume_when_valid(tmp_path: Path) -> None:
    target = _target()
    env = _env()
    comp_hash = compute_chain_identity_hash(env)

    # Seed: a journal record + manifest matching composition_hash.
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0))
    manifest = Manifest(
        run_id="r",
        base_address="0x" + target.base_address.hex(),
        revision=0,
        genesis_sha256=env.genesis_sha256,
        chain_identity_hash=comp_hash,
        reference_f_version="2026.04.23",
        plugin_git_sha=env.plugin_git_sha,
        nethermind_commit_sha=env.nethermind_commit_sha,
        dotnet_runtime_major=env.dotnet_runtime_major,
        cpu_arch=env.cpu_arch,
    )
    manifest.write(tmp_path / "run-manifest.json")

    decision = resolve_startup_mode(tmp_path, composition_hash=comp_hash, head_block=100)
    assert decision.mode is StartupMode.RESUME


def test_refuse_when_composition_hash_mismatch(tmp_path: Path) -> None:
    target = _target()
    env = _env()
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0))
    manifest = Manifest(
        run_id="r",
        base_address="0x" + target.base_address.hex(),
        revision=0,
        genesis_sha256=env.genesis_sha256,
        chain_identity_hash="WRONG",
        reference_f_version="2026.04.23",
        plugin_git_sha=env.plugin_git_sha,
        nethermind_commit_sha=env.nethermind_commit_sha,
        dotnet_runtime_major=env.dotnet_runtime_major,
        cpu_arch=env.cpu_arch,
    )
    manifest.write(tmp_path / "run-manifest.json")
    with pytest.raises(ResumeRefused):
        resolve_startup_mode(tmp_path, composition_hash="RIGHT", head_block=100)


def test_refuse_when_head_drifted(tmp_path: Path) -> None:
    _target()
    env = _env()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    with pytest.raises(ResumeRefused):
        resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=105)


def test_reconcile_pending_clears_when_head_matches_tail(tmp_path: Path) -> None:
    """C3: crash between pending and commit — block never made it on-chain."""
    env = _env()
    _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=1,
            resumed_from_batch=None,
            batch_id=1,
            verb="eoatx",
            deadline_bytes=1000,
            start_address="0x" + (1).to_bytes(20, "big").hex(),
            end_address="0x" + (2).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash=comp,
            target_sha256="a" * 64,
        ),
    )
    decision = resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=100)
    assert decision.mode is StartupMode.RESUME
    assert not (tmp_path / "orchestrator.journal.pending").exists()


def test_refuse_when_journal_empty_but_chain_head_nonzero(tmp_path: Path) -> None:
    """C-EMPTY-JOURNAL: a truncated/missing journal + live chain must not fresh-start."""
    with pytest.raises(ResumeRefused, match="journal empty but Nethermind head"):
        resolve_startup_mode(tmp_path, composition_hash="c" * 64, head_block=42)


def test_refuse_when_journal_empty_but_pending_present(tmp_path: Path) -> None:
    """C-EMPTY-JOURNAL: a sidecar with an empty journal is ambiguous — refuse."""
    from orchestrator.journal import PendingBatch as _PB

    write_pending(
        tmp_path,
        _PB(
            session_id=1,
            resumed_from_batch=None,
            batch_id=0,
            verb="eoatx",
            deadline_bytes=1_000,
            start_address="0x" + (0).to_bytes(20, "big").hex(),
            end_address="0x" + (1).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=0,
        ),
    )
    with pytest.raises(ResumeRefused, match="state ambiguous"):
        resolve_startup_mode(tmp_path, composition_hash="c" * 64, head_block=0)


def test_reconcile_refuses_on_tx_hash_mismatch(tmp_path: Path) -> None:
    """C1: equal-count but different-signed tx set must be refused (TRIZ H1 / skeptic F-5)."""
    env = _env()
    target = _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=1,
            resumed_from_batch=None,
            batch_id=1,
            verb="eoatx",
            deadline_bytes=50_000,
            start_address="0x" + (0).to_bytes(20, "big").hex(),
            end_address="0x" + (0).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash=comp,
            target_sha256="a" * 64,
        ),
    )

    # Compute the real tx count for this (verb, deadline, ctx) so the count
    # gate passes and the hash identity gate is what actually fires.
    from orchestrator.facade import dispatch as _dispatch

    probe = FacadeContext(base_address=target.base_address, revision=target.revision)
    real_count = len(_dispatch("eoatx", 50_000, probe))

    class _WrongHashRpc:
        def eth_get_block_by_number(self, number, full=False):
            return {
                "hash": "0x" + "dd" * 32,
                "transactions": [{"hash": "0x" + "ff" * 32} for _ in range(real_count)],
            }

        def close(self) -> None: ...

    ctx = FacadeContext(base_address=target.base_address, revision=target.revision)
    with pytest.raises(ResumeRefused, match=r"tx .* hash mismatch"):
        resolve_startup_mode(
            tmp_path,
            chain_identity_hash=comp,
            head_block=101,
            rpc=_WrongHashRpc(),
            facade_ctx=ctx,
        )


def test_reconcile_refuses_on_tx_set_mismatch(tmp_path: Path) -> None:
    """C-RECONCILE-TRUST: a misreported head block must NOT be silently trusted."""
    env = _env()
    target = _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=1,
            resumed_from_batch=None,
            batch_id=1,
            verb="eoatx",
            deadline_bytes=50_000,
            start_address="0x" + (0).to_bytes(20, "big").hex(),
            end_address="0x" + (0).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash=comp,
            target_sha256="a" * 64,
        ),
    )

    class _AutoMinedEmpty:
        def eth_get_block_by_number(self, number, full=False):
            # Simulate Nethermind auto-mining an empty block between commit and resume.
            return {"hash": "0x" + "dd" * 32, "transactions": []}

        def close(self) -> None: ...

    ctx = FacadeContext(base_address=target.base_address, revision=target.revision)
    with pytest.raises(ResumeRefused, match="reconcile: head block has 0 txs"):
        resolve_startup_mode(
            tmp_path,
            chain_identity_hash=comp,
            head_block=101,
            rpc=_AutoMinedEmpty(),
            facade_ctx=ctx,
        )


def test_pending_composition_hash_mismatch_refused(tmp_path: Path) -> None:
    """H-PENDING-CH: sidecar from a different composition must not be trusted."""
    env = _env()
    _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=1,
            resumed_from_batch=None,
            batch_id=1,
            verb="eoatx",
            deadline_bytes=1000,
            start_address="0x" + (1).to_bytes(20, "big").hex(),
            end_address="0x" + (2).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash="d" * 64,  # DIFFERENT chain identity
        ),
    )
    with pytest.raises(ResumeRefused, match="chain_identity_hash"):
        resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=100)


def test_resume_refuses_on_journal_schema_violation(tmp_path: Path) -> None:
    """H-SCHEMA: journal schema errors on resume must map to ResumeRefused."""
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    # Truncate the last line mid-JSON to simulate a partial-write scenario.
    raw = journal.read_bytes()
    journal.write_bytes(raw[:-10])  # drop newline + trailing bytes
    with pytest.raises(ResumeRefused, match="schema violation"):
        resolve_startup_mode(tmp_path, composition_hash="c" * 64, head_block=100)


def test_state_dir_lock_refuses_second_holder(tmp_path: Path) -> None:
    """C-NO-LOCK: two orchestrators on the same state dir must not both start."""
    with _state_dir_lock(tmp_path), pytest.raises(RunAlreadyActive), _state_dir_lock(tmp_path):
        pass  # unreachable


def test_reconcile_synthesized_record_uses_new_session_id(tmp_path: Path) -> None:
    """Skeptic F-3: synthesized record must use the NEW session's id, not the crashed one."""
    env = _env()
    _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    # Prior session_id = 5; synthesized reconcile record should be stamped with 6.
    prior_tail = _record(batch_id=0, block_number=100)
    object.__setattr__(prior_tail, "session_id", 5)
    with JournalWriter(journal) as w:
        w.append(prior_tail)
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=5,  # pending belongs to the crashed session
            resumed_from_batch=None,
            batch_id=1,
            verb="eoatx",
            deadline_bytes=1000,
            start_address="0x" + (1).to_bytes(20, "big").hex(),
            end_address="0x" + (2).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash=comp,
            target_sha256="a" * 64,
        ),
    )

    class _Rpc:
        def eth_get_block_by_number(self, number, full=False):
            return {"hash": "0x" + "cc" * 32, "transactions": []}

        def close(self) -> None: ...

    # Force resume_session_id=6 (next session).
    resolve_startup_mode(
        tmp_path,
        chain_identity_hash=comp,
        head_block=101,
        rpc=_Rpc(),
        resume_session_id=6,
    )
    records = list(JournalReader(journal))
    assert records[1].session_id == 6, "reconciled record must carry the new session_id"


def test_reconcile_pending_synthesizes_record_when_head_advanced(tmp_path: Path) -> None:
    """C3: crash between commit and journal append — replay missing record from pending."""
    env = _env()
    _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=1,
            resumed_from_batch=None,
            batch_id=1,
            verb="eoatx",
            deadline_bytes=1000,
            start_address="0x" + (1).to_bytes(20, "big").hex(),
            end_address="0x" + (2).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash=comp,
            target_sha256="a" * 64,
        ),
    )

    class _Rpc:
        def eth_get_block_by_number(self, number, full=False):
            return {"hash": "0x" + "cc" * 32, "stateRoot": "0x00"}

        def close(self) -> None: ...

    decision = resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=101, rpc=_Rpc())
    assert decision.mode is StartupMode.RESUME
    records = list(JournalReader(journal))
    assert len(records) == 2
    assert records[1].batch_id == 1
    assert records[1].replay_core.block_hash == "0x" + "cc" * 32
    assert records[1].replay_core.status == "reconciled_unobserved"
    assert not (tmp_path / "orchestrator.journal.pending").exists()


def test_reconcile_preserves_tail_observability(tmp_path: Path) -> None:
    """C-OBS: synthesized reconcile record must carry forward tail's F/σ/α."""
    env = _env()
    _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    rich_obs = Observability(
        coeffs_before={"eoatx": {"accounts": 100.0, "storage": 0.0, "code": 0.0}},
        coeffs_after={"eoatx": {"accounts": 155.5, "storage": 1.5, "code": 0.0}},
        alpha_state={"eoatx": {"accounts": 0.17, "storage": 0.05, "code": 0.02}},
        sigma_innov={"eoatx": {"accounts": 8.0, "storage": 1.0, "code": 1.0}},
        alpha_current=0.08,
        innovation_ratio=0.05,
        residual_norm=2.0,
        statecomp_snapshot={"blockNumber": 100},
    )
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100, observability=rich_obs))
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=1,
            resumed_from_batch=None,
            batch_id=1,
            verb="eoatx",
            deadline_bytes=1000,
            start_address="0x" + (1).to_bytes(20, "big").hex(),
            end_address="0x" + (2).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash=comp,
            target_sha256="a" * 64,
        ),
    )

    class _Rpc:
        def eth_get_block_by_number(self, number, full=False):
            return {"hash": "0x" + "cc" * 32}

        def close(self) -> None: ...

    resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=101, rpc=_Rpc())
    records = list(JournalReader(journal))
    synthesized = records[1]
    # F, σ, α all carried forward from the tail — next resume must not cold-start.
    assert synthesized.observability.coeffs_after == rich_obs.coeffs_after
    assert synthesized.observability.alpha_state == rich_obs.alpha_state
    assert synthesized.observability.sigma_innov == rich_obs.sigma_innov


def test_signal_handlers_restore_previous(tmp_path: Path) -> None:
    """H6: entering/exiting the signal-handler context must not leak onto the process."""
    import signal

    from orchestrator.lifecycle import _signal_handlers

    sentinel_called = {"count": 0}

    def sentinel(*_: object) -> None:
        sentinel_called["count"] += 1

    prior_sigint = signal.signal(signal.SIGINT, sentinel)
    prior_sigterm = signal.signal(signal.SIGTERM, sentinel)
    try:
        with _signal_handlers() as stop:
            # Inside the context, our flag is what handles signals.
            assert not stop.is_set()
            assert signal.getsignal(signal.SIGINT) is not sentinel
        # After the context exits, the previous handlers must be back in place.
        assert signal.getsignal(signal.SIGINT) is sentinel
        assert signal.getsignal(signal.SIGTERM) is sentinel
    finally:
        signal.signal(signal.SIGINT, prior_sigint)
        signal.signal(signal.SIGTERM, prior_sigterm)


def test_refuse_when_head_regresses(tmp_path: Path) -> None:
    """Spec §1 reorg-free invariant violation — head < tail.block_number."""
    env = _env()
    _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    with pytest.raises(ResumeRefused, match="§1"):
        resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=50)


def test_pending_batch_id_equal_to_tail_treats_as_already_journaled(tmp_path: Path) -> None:
    """Skeptic F-9: if clear_pending failed after successful journal append, recover gracefully."""
    env = _env()
    _target()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=5, block_number=105))
    # Stale pending: tail and pending share the same batch_id, meaning the
    # append succeeded on the prior run but the sidecar was never cleared.
    write_pending(
        tmp_path,
        PendingBatch(
            session_id=1,
            resumed_from_batch=None,
            batch_id=5,
            verb="eoatx",
            deadline_bytes=1000,
            start_address="0x" + (1).to_bytes(20, "big").hex(),
            end_address="0x" + (2).to_bytes(20, "big").hex(),
            ts_iso="2026-04-24T00:01:00Z",
            pre_block_number=100,
            chain_identity_hash=comp,
            target_sha256="a" * 64,
        ),
    )
    decision = resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=105)
    assert decision.mode is StartupMode.RESUME
    assert not (tmp_path / "orchestrator.journal.pending").exists()


def test_refuse_when_head_is_unknown(tmp_path: Path) -> None:
    """H2: RPC unreachable must not silently allow resume."""
    _target()
    env = _env()
    comp = compute_chain_identity_hash(env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    with pytest.raises(ResumeRefused, match="head unknown"):
        resolve_startup_mode(tmp_path, chain_identity_hash=comp, head_block=None)


def test_run_with_max_batches_writes_manifest(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    target = _target()
    env = _env()

    # Stub sensor: always returns block_number incremented by one.
    class _StubSensor:
        def __init__(self) -> None:
            self._block = 0

        def read(self, expected_block=None, timeout_s=5.0):
            from orchestrator.sensor import StateObservation

            if expected_block is not None:
                self._block = expected_block
            else:
                self._block += 1
            return StateObservation(
                block_number=self._block,
                account_bytes=self._block * 1000,
                storage_bytes=self._block * 5000,
                code_bytes=self._block * 100,
                raw={"blockNumber": self._block},
            )

        def close(self) -> None: ...

    class _StubRpc:
        def __init__(self) -> None:
            self._block = 0

        def testing_commit_block_v1(self, txs, *, timestamp_unix):
            self._block += 1
            return "0x" + self._block.to_bytes(32, "big").hex()

        def eth_get_block_by_hash(self, block_hash, full=True):
            return {
                "parentHash": "0x" + b"\x00".hex() * 32,
                "miner": "0x" + b"\x00".hex() * 20,
                "stateRoot": "0x" + b"\x01".hex() * 32,
                "receiptsRoot": "0x" + b"\x00".hex() * 32,
                "logsBloom": "0x" + b"\x00".hex() * 256,
                "mixHash": "0x" + b"\x00".hex() * 32,
                "number": hex(self._block),
                "gasLimit": "0x1c9c380",
                "gasUsed": "0x5208",
                "timestamp": hex(1700000000 + self._block),
                "extraData": "0x",
                "baseFeePerGas": "0x3b9aca00",
                "hash": block_hash,
            }

        def eth_get_block_by_number(self, number="latest", full=False):
            return {
                "number": hex(max(self._block, 0)),
                "stateRoot": "0x" + b"\x01".hex() * 32,
            }

        def eth_get_transaction_count(self, address, block="latest"):
            # Fresh runs start at cursor=0; nonce reconcile only fires on
            # resume, so the value here doesn't matter for the fresh-start path.
            return 0

        def close(self) -> None: ...

    # Round-3 C4 wires a default probe executor on FRESH runs; inject a stub
    # that produces REFERENCE_F-matching numbers so the §B.2 sanity gate passes
    # under the synthetic sensor.
    from orchestrator.reference_f import default_reference_f_path, load_reference_f

    ref_f = load_reference_f(default_reference_f_path())

    def _stub_probe(verb: str, tx_count: int):
        from orchestrator.sensor import StateObservation

        ref = ref_f.per_scenario(verb)
        pre = StateObservation(block_number=0, account_bytes=0, storage_bytes=0, code_bytes=0)
        post = StateObservation(
            block_number=1,
            account_bytes=int(ref["accounts"] * tx_count),
            storage_bytes=int(ref["storage"] * tx_count),
            code_bytes=int(ref["code"] * tx_count),
        )
        return pre, post

    deps = LifecycleDeps(
        sensor=_StubSensor(),
        rpc=_StubRpc(),
        probe_executor=_stub_probe,
    )
    manifest_path = run(
        target=target,
        state_dir=tmp_path,
        rpc_url="http://stub",
        env=env,
        max_batches=3,
        deps=deps,
    )

    assert manifest_path.exists()
    manifest = Manifest.read(manifest_path)
    assert len(manifest.sessions) == 1
    assert manifest.sessions[0].session_id == 1

    journal = tmp_path / "orchestrator.journal.jsonl"
    assert journal.exists()
    lines = [ln for ln in journal.read_text().splitlines() if ln.strip()]
    assert len(lines) == 3


def test_resume_refused_when_nonce_mismatches_cursor(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Resume must refuse if eth_getTransactionCount disagrees with the rehydrated cursor.

    A mid-batch crash that left partial txs on-chain would leave the cursor
    behind the EOA's nonce; resuming would re-sign already-mined nonces. The
    guard fires before the main loop opens its journal/payload writers.
    """
    from orchestrator.lifecycle import LifecycleDeps

    target = _target()
    env = _env()

    # Seed: the journal has cursor=1 (one batch dispatched a single tx),
    # but the on-chain nonce will be 5 → guard must raise.
    journal = tmp_path / "orchestrator.journal.jsonl"
    rec = _record(batch_id=0, block_number=100)
    object.__setattr__(rec.replay_core, "end_address", "0x" + (1).to_bytes(20, "big").hex())
    with JournalWriter(journal) as w:
        w.append(rec)

    # Build a manifest that matches the current run config so we get past
    # composition_hash + replay_context gates.
    from orchestrator.lifecycle import build_facade_context, build_replay_context
    from orchestrator.manifest import Manifest, compute_chain_identity_hash

    fctx = build_facade_context(target)
    rctx = build_replay_context(fctx)
    comp = compute_chain_identity_hash(env, rctx)
    manifest = Manifest(
        run_id="r",
        base_address="0x" + target.base_address.hex(),
        revision=0,
        genesis_sha256=env.genesis_sha256,
        chain_identity_hash=comp,
        reference_f_version="2026.04.23",
        plugin_git_sha=env.plugin_git_sha,
        nethermind_commit_sha=env.nethermind_commit_sha,
        dotnet_runtime_major=env.dotnet_runtime_major,
        cpu_arch=env.cpu_arch,
        replay_context=rctx,
    )
    manifest.write(tmp_path / "run-manifest.json")

    class _MismatchedRpc:
        def testing_commit_block_v1(self, txs, *, timestamp_unix):
            return "0x" + "00" * 32

        def eth_get_block_by_hash(self, block_hash, full=True):
            return {}

        def eth_get_block_by_number(self, number="latest", full=False):
            # Make resume go down the resume path: head == tail.block_number.
            return {"number": hex(100), "stateRoot": "0x" + "01" * 32}

        def eth_get_transaction_count(self, address, block="latest"):
            # Cursor is 1 after rehydration; chain reports 5 → mismatch.
            return 5

        def close(self) -> None: ...

    class _Sensor:
        def read(self, expected_block=None, timeout_s=5.0):
            from orchestrator.sensor import StateObservation

            return StateObservation(
                block_number=expected_block or 100,
                account_bytes=0,
                storage_bytes=0,
                code_bytes=0,
                raw={"blockNumber": expected_block or 100},
            )

        def close(self) -> None: ...

    deps = LifecycleDeps(sensor=_Sensor(), rpc=_MismatchedRpc())
    with pytest.raises(ResumeRefused, match=r"cursor.*chain"):
        run(target=target, state_dir=tmp_path, rpc_url="http://stub", env=env, deps=deps)


def test_legacy_manifest_resume_bypass_refused(tmp_path: Path) -> None:
    """A pre-ReplayContext manifest must NOT silently let a new signer resume.

    Without this guard, an attacker (or operator with a stale state dir) could
    drop a manifest with ``replay_context: null`` next to a non-empty journal
    and resume under a different signer / chain_id. The signer + chain identity
    gate would be skipped because ``replay_context_matches(None, current)``
    returns False but only when current is non-None — yet the prior code only
    invoked ``replay_context_matches`` if ``replay_context is not None``,
    creating an "if both sides present" check that this gate must close.
    """
    target = _target()
    env = _env()
    from orchestrator.lifecycle import build_facade_context, build_replay_context
    from orchestrator.manifest import compute_chain_identity_hash

    fctx = build_facade_context(target)
    rctx = build_replay_context(fctx)
    comp = compute_chain_identity_hash(env, rctx)

    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    legacy = Manifest(
        run_id="r",
        base_address="0x" + target.base_address.hex(),
        revision=0,
        genesis_sha256=env.genesis_sha256,
        chain_identity_hash=comp,
        reference_f_version="2026.04.23",
        plugin_git_sha=env.plugin_git_sha,
        nethermind_commit_sha=env.nethermind_commit_sha,
        dotnet_runtime_major=env.dotnet_runtime_major,
        cpu_arch=env.cpu_arch,
        replay_context=None,  # legacy run, pre-ReplayContext
    )
    legacy.write(tmp_path / "run-manifest.json")

    with pytest.raises(ResumeRefused, match="legacy manifest"):
        resolve_startup_mode(
            tmp_path,
            chain_identity_hash=comp,
            head_block=100,
            replay_context=rctx,
        )


class _ProbeStubSensor:
    """Sensor that grows account_bytes per call so the probe sanity gate passes.

    Each ``read()`` call increments ``block_number`` by one and adds the verb's
    REFERENCE_F per-tx bytes × tx_count to the cumulative byte counters. The
    probe fires per-verb back-to-back, so this stub only needs to grow on
    matched (pre, post) pairs — the lifecycle's pre-loop ``sensor.read()`` call
    sees whichever counters were last set.
    """

    def __init__(self, ref_f, tx_count: int) -> None:
        self._block = 0
        self._account = 0
        self._storage = 0
        self._code = 0
        self._ref_f = ref_f
        self._tx_count = tx_count
        self._verbs_iter: list[str] = []

    def queue(self, verbs: list[str]) -> None:
        self._verbs_iter = list(verbs)

    def read(self, expected_block=None, timeout_s=5.0):
        from orchestrator.sensor import StateObservation

        if expected_block is not None:
            # post-commit read: bump the counters by the next queued verb's
            # REFERENCE_F so the probe sanity gate passes.
            self._block = expected_block
            if self._verbs_iter:
                verb = self._verbs_iter.pop(0)
                ref = self._ref_f.per_scenario(verb)
                self._account += int(ref["accounts"] * self._tx_count)
                self._storage += int(ref["storage"] * self._tx_count)
                self._code += int(ref["code"] * self._tx_count)
        else:
            # pre-commit read or top-of-loop read: don't advance the block.
            pass
        return StateObservation(
            block_number=self._block,
            account_bytes=self._account,
            storage_bytes=self._storage,
            code_bytes=self._code,
            raw={"blockNumber": self._block},
        )

    def close(self) -> None: ...


class _ProbeStubRpc:
    """Tracks committed blocks; returns minimal-but-valid block dicts."""

    def __init__(self) -> None:
        self._block = 0

    def testing_commit_block_v1(self, txs, *, timestamp_unix):
        self._block += 1
        return "0x" + self._block.to_bytes(32, "big").hex()

    def eth_get_block_by_hash(self, block_hash, full=True):
        return {
            "parentHash": "0x" + b"\x00".hex() * 32,
            "miner": "0x" + b"\x00".hex() * 20,
            "stateRoot": "0x" + b"\x01".hex() * 32,
            "receiptsRoot": "0x" + b"\x00".hex() * 32,
            "logsBloom": "0x" + b"\x00".hex() * 256,
            "mixHash": "0x" + b"\x00".hex() * 32,
            "number": hex(self._block),
            "gasLimit": "0x1c9c380",
            "gasUsed": "0x5208",
            "timestamp": hex(1700000000 + self._block),
            "extraData": "0x",
            "baseFeePerGas": "0x3b9aca00",
            "hash": block_hash,
        }

    def eth_get_block_by_number(self, number="latest", full=False):
        return {
            "number": hex(max(self._block, 0)),
            "stateRoot": "0x" + b"\x01".hex() * 32,
        }

    def eth_get_transaction_count(self, address, block="latest"):
        return 0

    def close(self) -> None: ...


def _three_verb_target() -> TargetConfig:
    """Tiny 3-verb target so probe + QP records are easy to count."""
    return TargetConfig(
        mainnet_target={"accounts": 0.141, "storage": 0.817, "code": 0.042},
        target_total_bytes=10_000_000_000,
        base_address=(0x10_00).to_bytes(20, "big"),
        revision=0,
        qp_scenarios=("eoatx", "calltx", "deploytx"),
        total_batch_bytes=500_000,
        projection_eta=0.5,
        raw={},
        source_sha256="a" * 64,
    )


def test_probe_batches_are_journaled(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """§B.2: probe blocks must land in the journal at batch_ids 0..n-1 with payloads."""
    from orchestrator.payloads import PayloadStreamReader
    from orchestrator.probe import PROBE_TX_COUNT
    from orchestrator.reference_f import default_reference_f_path, load_reference_f

    target = _three_verb_target()
    env = _env()
    ref_f = load_reference_f(default_reference_f_path())

    sensor = _ProbeStubSensor(ref_f, PROBE_TX_COUNT)
    sensor.queue(list(target.qp_scenarios))

    deps = LifecycleDeps(
        sensor=sensor,
        rpc=_ProbeStubRpc(),
        probe_executor=None,  # use the default journaling executor
    )
    run(
        target=target,
        state_dir=tmp_path,
        rpc_url="http://stub",
        env=env,
        max_batches=0,  # zero QP batches — only probes journal
        deps=deps,
    )

    journal = tmp_path / "orchestrator.journal.jsonl"
    records = list(JournalReader(journal))
    n_probes = len(target.qp_scenarios)
    assert len(records) == n_probes, f"expected {n_probes} probe records, got {len(records)}"
    for i, rec in enumerate(records):
        assert rec.batch_id == i
        assert rec.replay_core.verb == target.qp_scenarios[i]
        assert rec.replay_core.status == "ok"

    # Payload stream must mirror the journal: one ExecutionPayloadV3 per probe.
    payload_path = tmp_path / "payloads.rlp"
    payloads = list(PayloadStreamReader(payload_path))
    assert len(payloads) == n_probes


def test_probe_then_qp_batch_id_continuity(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """QP loop's first batch_id must equal n_probes (continuity across probe/QP)."""
    from orchestrator.probe import PROBE_TX_COUNT
    from orchestrator.reference_f import default_reference_f_path, load_reference_f

    target = _three_verb_target()
    env = _env()
    ref_f = load_reference_f(default_reference_f_path())

    sensor = _ProbeStubSensor(ref_f, PROBE_TX_COUNT)
    sensor.queue(list(target.qp_scenarios))

    deps = LifecycleDeps(
        sensor=sensor,
        rpc=_ProbeStubRpc(),
        probe_executor=None,
    )
    run(
        target=target,
        state_dir=tmp_path,
        rpc_url="http://stub",
        env=env,
        max_batches=1,  # one QP batch after the probe
        deps=deps,
    )

    journal = tmp_path / "orchestrator.journal.jsonl"
    records = list(JournalReader(journal))
    n_probes = len(target.qp_scenarios)
    assert len(records) == n_probes + 1
    # batch_ids must be contiguous 0..n_probes
    assert [r.batch_id for r in records] == list(range(n_probes + 1))
    # The QP batch's batch_id == n_probes
    assert records[n_probes].batch_id == n_probes


_TARGET_A_YAML = b"""\
mainnet_target:
  accounts: 0.141
  storage:  0.817
  code:     0.042
target_total_bytes: 1000000000
base_address: "0x1000"
revision: 0
qp_scenarios:
  - eoatx
  - calltx
  - deploytx
total_batch_bytes: 500000
projection_eta: 0.5
"""

_TARGET_B_YAML = b"""\
mainnet_target:
  accounts: 0.5
  storage:  0.4
  code:     0.1
target_total_bytes: 5000000000
base_address: "0x1000"
revision: 0
qp_scenarios:
  - eoatx
  - calltx
  - deploytx
total_batch_bytes: 500000
projection_eta: 0.5
"""


def _write_target_yaml(path: Path, body: bytes) -> None:
    """Write target.yaml and bump its mtime so the watcher detects the edit.

    `os.utime` ensures the watcher's mtime-cache invalidates even when the
    write happens within the same nanosecond as the previous one.
    """
    import os as _os

    path.write_bytes(body)
    # Bump mtime forward by 1 second to guarantee the watcher sees the change.
    stat = path.stat()
    _os.utime(path, ns=(stat.st_atime_ns + 1_000_000_000, stat.st_mtime_ns + 1_000_000_000))


def test_live_target_reload_picks_up_new_shape(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Editing target.yaml mid-run flips the journal's target_sha256 boundary."""
    from orchestrator.target import LiveTargetWatcher, load_target

    target_yaml = tmp_path / "target.yaml"
    target_yaml.write_bytes(_TARGET_A_YAML)
    target_a = load_target(target_yaml)
    sha_a = target_a.sha256

    # Compute target B's sha so the test can verify the boundary moved.
    sha_b_yaml_target = parse_target_sha(_TARGET_B_YAML)

    env = _env()
    state_dir = tmp_path / "state"
    state_dir.mkdir()

    flip_at_batch = {"value": 4}
    completed = {"count": 0}

    class _Sensor:
        def __init__(self) -> None:
            self._block = 0

        def read(self, expected_block=None, timeout_s=5.0):
            from orchestrator.sensor import StateObservation

            if expected_block is not None:
                self._block = expected_block
            else:
                self._block += 1
            return StateObservation(
                block_number=self._block,
                account_bytes=self._block * 100,
                storage_bytes=self._block * 100,
                code_bytes=self._block * 100,
                raw={"blockNumber": self._block},
            )

        def close(self) -> None: ...

    class _Rpc:
        def __init__(self) -> None:
            self._block = 0

        def testing_commit_block_v1(self, txs, *, timestamp_unix):
            self._block += 1
            # When a target swap is due, edit target.yaml between batches so
            # the watcher's next current() picks up the new shape.
            completed["count"] += 1
            if completed["count"] == flip_at_batch["value"]:
                _write_target_yaml(target_yaml, _TARGET_B_YAML)
            return "0x" + self._block.to_bytes(32, "big").hex()

        def eth_get_block_by_hash(self, block_hash, full=True):
            return {
                "parentHash": "0x" + b"\x00".hex() * 32,
                "miner": "0x" + b"\x00".hex() * 20,
                "stateRoot": "0x" + b"\x01".hex() * 32,
                "receiptsRoot": "0x" + b"\x00".hex() * 32,
                "logsBloom": "0x" + b"\x00".hex() * 256,
                "mixHash": "0x" + b"\x00".hex() * 32,
                "number": hex(self._block),
                "gasLimit": "0x1c9c380",
                "gasUsed": "0x5208",
                "timestamp": hex(1_700_000_000 + self._block),
                "extraData": "0x",
                "baseFeePerGas": "0x3b9aca00",
                "hash": block_hash,
            }

        def eth_get_block_by_number(self, number="latest", full=False):
            return {
                "number": hex(max(self._block, 0)),
                "stateRoot": "0x" + b"\x01".hex() * 32,
            }

        def eth_get_transaction_count(self, address, block="latest"):
            return 0

        def close(self) -> None: ...

    from orchestrator.reference_f import default_reference_f_path, load_reference_f

    ref_f = load_reference_f(default_reference_f_path())

    def _stub_probe(verb: str, tx_count: int):
        from orchestrator.sensor import StateObservation

        ref = ref_f.per_scenario(verb)
        pre = StateObservation(block_number=0, account_bytes=0, storage_bytes=0, code_bytes=0)
        post = StateObservation(
            block_number=1,
            account_bytes=int(ref["accounts"] * tx_count),
            storage_bytes=int(ref["storage"] * tx_count),
            code_bytes=int(ref["code"] * tx_count),
        )
        return pre, post

    deps = LifecycleDeps(sensor=_Sensor(), rpc=_Rpc(), probe_executor=_stub_probe)
    watcher = LiveTargetWatcher(target_yaml, initial=target_a)
    run(
        target=target_a,
        target_watcher=watcher,
        state_dir=state_dir,
        rpc_url="http://stub",
        env=env,
        max_batches=8,
        deps=deps,
    )

    journal = state_dir / "orchestrator.journal.jsonl"
    records = list(JournalReader(journal))
    # Probe records (3 verbs) seed first, then 8 QP batches → 11 total.
    qp_records = [r for r in records if r.batch_id >= 3]
    sha_at_batch = {r.batch_id: r.replay_core.target_sha256 for r in qp_records}
    # Early QP batches must reference target A.
    assert any(s == sha_a for s in sha_at_batch.values()), (
        f"no batches referenced target A sha {sha_a[:8]}"
    )
    # Later QP batches must reference target B.
    assert any(s == sha_b_yaml_target for s in sha_at_batch.values()), (
        f"no batches referenced target B sha {sha_b_yaml_target[:8]}"
    )


def parse_target_sha(yaml_bytes: bytes) -> str:
    """Compute the sha256 a watcher would assign to a YAML body."""
    from orchestrator.target import parse_target

    return parse_target(yaml_bytes).sha256


def test_target_history_records_all_snapshots(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Both target A and target B end up in manifest.target_history in order."""
    from orchestrator.target import LiveTargetWatcher, load_target

    target_yaml = tmp_path / "target.yaml"
    target_yaml.write_bytes(_TARGET_A_YAML)
    target_a = load_target(target_yaml)
    sha_b = parse_target_sha(_TARGET_B_YAML)

    env = _env()
    state_dir = tmp_path / "state"
    state_dir.mkdir()

    flip_at = {"value": 3}
    completed = {"count": 0}

    class _Sensor:
        def __init__(self) -> None:
            self._block = 0

        def read(self, expected_block=None, timeout_s=5.0):
            from orchestrator.sensor import StateObservation

            if expected_block is not None:
                self._block = expected_block
            else:
                self._block += 1
            return StateObservation(
                block_number=self._block, account_bytes=0, storage_bytes=0, code_bytes=0,
                raw={"blockNumber": self._block},
            )

        def close(self) -> None: ...

    class _Rpc:
        def __init__(self) -> None:
            self._block = 0

        def testing_commit_block_v1(self, txs, *, timestamp_unix):
            self._block += 1
            completed["count"] += 1
            if completed["count"] == flip_at["value"]:
                _write_target_yaml(target_yaml, _TARGET_B_YAML)
            return "0x" + self._block.to_bytes(32, "big").hex()

        def eth_get_block_by_hash(self, block_hash, full=True):
            return {
                "parentHash": "0x" + b"\x00".hex() * 32,
                "miner": "0x" + b"\x00".hex() * 20,
                "stateRoot": "0x" + b"\x01".hex() * 32,
                "receiptsRoot": "0x" + b"\x00".hex() * 32,
                "logsBloom": "0x" + b"\x00".hex() * 256,
                "mixHash": "0x" + b"\x00".hex() * 32,
                "number": hex(self._block),
                "gasLimit": "0x1c9c380",
                "gasUsed": "0x5208",
                "timestamp": hex(1_700_000_000 + self._block),
                "extraData": "0x",
                "baseFeePerGas": "0x3b9aca00",
                "hash": block_hash,
            }

        def eth_get_block_by_number(self, number="latest", full=False):
            return {
                "number": hex(max(self._block, 0)),
                "stateRoot": "0x" + b"\x01".hex() * 32,
            }

        def eth_get_transaction_count(self, address, block="latest"):
            return 0

        def close(self) -> None: ...

    from orchestrator.reference_f import default_reference_f_path, load_reference_f

    ref_f = load_reference_f(default_reference_f_path())

    def _stub_probe(verb: str, tx_count: int):
        from orchestrator.sensor import StateObservation

        ref = ref_f.per_scenario(verb)
        return (
            StateObservation(block_number=0, account_bytes=0, storage_bytes=0, code_bytes=0),
            StateObservation(
                block_number=1,
                account_bytes=int(ref["accounts"] * tx_count),
                storage_bytes=int(ref["storage"] * tx_count),
                code_bytes=int(ref["code"] * tx_count),
            ),
        )

    deps = LifecycleDeps(sensor=_Sensor(), rpc=_Rpc(), probe_executor=_stub_probe)
    watcher = LiveTargetWatcher(target_yaml, initial=target_a)
    manifest_path = run(
        target=target_a,
        target_watcher=watcher,
        state_dir=state_dir,
        rpc_url="http://stub",
        env=env,
        max_batches=6,
        deps=deps,
    )
    manifest = Manifest.read(manifest_path)
    history_shas = [snap.sha256 for snap in manifest.target_history]
    assert target_a.sha256 in history_shas, "boot target not in history"
    assert sha_b in history_shas, "edited target not in history"
    # Order: A first, B appended on the swap.
    assert history_shas.index(target_a.sha256) < history_shas.index(sha_b)


def test_resume_with_changed_target_yaml(tmp_path: Path) -> None:
    """A run with target B that shares the chain identity of a prior run with
    target A must resume cleanly; the new run uses target B's sha for new batches.
    """
    from orchestrator.target import LiveTargetWatcher, load_target

    target_yaml = tmp_path / "target.yaml"
    target_yaml.write_bytes(_TARGET_A_YAML)
    target_a = load_target(target_yaml)
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    env = _env()

    class _Sensor:
        def __init__(self) -> None:
            self._block = 0

        def read(self, expected_block=None, timeout_s=5.0):
            from orchestrator.sensor import StateObservation

            if expected_block is not None:
                self._block = expected_block
            else:
                self._block += 1
            return StateObservation(
                block_number=self._block, account_bytes=0, storage_bytes=0, code_bytes=0,
                raw={"blockNumber": self._block},
            )

        def close(self) -> None: ...

    class _Rpc:
        def __init__(self) -> None:
            self._block = 0

        def testing_commit_block_v1(self, txs, *, timestamp_unix):
            self._block += 1
            return "0x" + self._block.to_bytes(32, "big").hex()

        def eth_get_block_by_hash(self, block_hash, full=True):
            return {
                "parentHash": "0x" + b"\x00".hex() * 32,
                "miner": "0x" + b"\x00".hex() * 20,
                "stateRoot": "0x" + b"\x01".hex() * 32,
                "receiptsRoot": "0x" + b"\x00".hex() * 32,
                "logsBloom": "0x" + b"\x00".hex() * 256,
                "mixHash": "0x" + b"\x00".hex() * 32,
                "number": hex(self._block),
                "gasLimit": "0x1c9c380",
                "gasUsed": "0x5208",
                "timestamp": hex(1_700_000_000 + self._block),
                "extraData": "0x",
                "baseFeePerGas": "0x3b9aca00",
                "hash": block_hash,
            }

        def eth_get_block_by_number(self, number="latest", full=False):
            return {"number": hex(max(self._block, 0)), "stateRoot": "0x01" * 32}

        def eth_get_transaction_count(self, address, block="latest"):
            # Cursor is fresh-start 0 + n probes' txs; the simple probe stub
            # increments cursor by 0 per probe (no real txs sent), so on resume
            # the cursor matches "0" exactly.
            return 0

        def close(self) -> None: ...

    from orchestrator.reference_f import default_reference_f_path, load_reference_f

    ref_f = load_reference_f(default_reference_f_path())

    def _stub_probe(verb: str, tx_count: int):
        from orchestrator.sensor import StateObservation

        ref = ref_f.per_scenario(verb)
        return (
            StateObservation(block_number=0, account_bytes=0, storage_bytes=0, code_bytes=0),
            StateObservation(
                block_number=1,
                account_bytes=int(ref["accounts"] * tx_count),
                storage_bytes=int(ref["storage"] * tx_count),
                code_bytes=int(ref["code"] * tx_count),
            ),
        )

    # First run with target A. Stop after a few batches (max_batches).
    rpc = _Rpc()
    deps = LifecycleDeps(
        sensor=_Sensor(),
        rpc=rpc,
        probe_executor=_stub_probe,
    )
    watcher = LiveTargetWatcher(target_yaml, initial=target_a)
    run(
        target=target_a,
        target_watcher=watcher,
        state_dir=state_dir,
        rpc_url="http://stub",
        env=env,
        max_batches=3,
        deps=deps,
    )

    # Capture the journal-tail block number so the second-run RPC reports a
    # head that matches (otherwise the head < tail check refuses resume).
    tail_record = JournalReader(state_dir / "orchestrator.journal.jsonl").tail()
    assert tail_record is not None
    tail_block = tail_record.replay_core.block_number

    # Second run: same chain, different target.yaml shape.
    target_yaml.write_bytes(_TARGET_B_YAML)
    target_b = load_target(target_yaml)

    end_cursor = int(tail_record.replay_core.end_address, 16)

    class _ResumedRpc(_Rpc):
        def __init__(self, start_block: int, nonce: int) -> None:
            super().__init__()
            self._block = start_block
            self._nonce = nonce

        def eth_get_transaction_count(self, address, block="latest"):
            return self._nonce

    deps2 = LifecycleDeps(
        sensor=_Sensor(),
        rpc=_ResumedRpc(tail_block, end_cursor),
        probe_executor=_stub_probe,
    )
    watcher2 = LiveTargetWatcher(target_yaml, initial=target_b)
    # Must NOT raise ResumeRefused — chain_identity_hash unchanged.
    run(
        target=target_b,
        target_watcher=watcher2,
        state_dir=state_dir,
        rpc_url="http://stub",
        env=env,
        max_batches=2,
        deps=deps2,
    )
    journal = state_dir / "orchestrator.journal.jsonl"
    records = list(JournalReader(journal))
    # Some records reference A (from session 1) and some reference B (session 2).
    shas = {r.replay_core.target_sha256 for r in records}
    assert target_a.sha256 in shas
    assert target_b.sha256 in shas


def test_target_reached_idle_timeout(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """When the active target is satisfied and no edit arrives, the run shuts
    down with stop_reason='target_reached_idle' after the idle timeout."""
    from orchestrator import lifecycle as lc_mod
    from orchestrator.target import LiveTargetWatcher, load_target

    target_yaml = tmp_path / "target.yaml"
    target_yaml.write_bytes(_TARGET_A_YAML)
    target_a = load_target(target_yaml)
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    env = _env()

    # Force the target to be already satisfied: bump byte counters so
    # _target_reached returns True immediately.
    huge = target_a.target_total_bytes  # any axis × this passes the threshold

    class _Sensor:
        def __init__(self) -> None:
            self._block = 0

        def read(self, expected_block=None, timeout_s=5.0):
            from orchestrator.sensor import StateObservation

            if expected_block is not None:
                self._block = expected_block
            else:
                self._block += 1
            return StateObservation(
                block_number=self._block,
                account_bytes=huge,
                storage_bytes=huge,
                code_bytes=huge,
                raw={"blockNumber": self._block},
            )

        def close(self) -> None: ...

    class _Rpc:
        def __init__(self) -> None:
            self._block = 0

        def testing_commit_block_v1(self, txs, *, timestamp_unix):
            self._block += 1
            return "0x" + self._block.to_bytes(32, "big").hex()

        def eth_get_block_by_hash(self, block_hash, full=True):
            return {
                "parentHash": "0x" + b"\x00".hex() * 32,
                "miner": "0x" + b"\x00".hex() * 20,
                "stateRoot": "0x" + b"\x01".hex() * 32,
                "receiptsRoot": "0x" + b"\x00".hex() * 32,
                "logsBloom": "0x" + b"\x00".hex() * 256,
                "mixHash": "0x" + b"\x00".hex() * 32,
                "number": hex(self._block),
                "gasLimit": "0x1c9c380",
                "gasUsed": "0x5208",
                "timestamp": hex(1_700_000_000 + self._block),
                "extraData": "0x",
                "baseFeePerGas": "0x3b9aca00",
                "hash": block_hash,
            }

        def eth_get_block_by_number(self, number="latest", full=False):
            return {"number": hex(max(self._block, 0)), "stateRoot": "0x01" * 32}

        def eth_get_transaction_count(self, address, block="latest"):
            return 0

        def close(self) -> None: ...

    from orchestrator.reference_f import default_reference_f_path, load_reference_f

    ref_f = load_reference_f(default_reference_f_path())

    def _stub_probe(verb: str, tx_count: int):
        from orchestrator.sensor import StateObservation

        ref = ref_f.per_scenario(verb)
        return (
            StateObservation(block_number=0, account_bytes=0, storage_bytes=0, code_bytes=0),
            StateObservation(
                block_number=1,
                account_bytes=int(ref["accounts"] * tx_count),
                storage_bytes=int(ref["storage"] * tx_count),
                code_bytes=int(ref["code"] * tx_count),
            ),
        )

    # Crank the idle timeout down to 1 s with a 0.1 s poll interval so the
    # test runs in well under a second of real time.
    monkeypatch.setattr(lc_mod, "TARGET_REACHED_IDLE_TIMEOUT_SEC", 0.5)
    monkeypatch.setattr(lc_mod, "TARGET_REACHED_POLL_INTERVAL_SEC", 0.05)

    deps = LifecycleDeps(sensor=_Sensor(), rpc=_Rpc(), probe_executor=_stub_probe)
    watcher = LiveTargetWatcher(target_yaml, initial=target_a)
    manifest_path = run(
        target=target_a,
        target_watcher=watcher,
        state_dir=state_dir,
        rpc_url="http://stub",
        env=env,
        max_batches=20,
        deps=deps,
    )
    manifest = Manifest.read(manifest_path)
    # Last session's stop_reason is the idle-timeout sentinel.
    assert manifest.sessions[-1].stop_reason == "target_reached_idle"


def _record(
    batch_id: int,
    *,
    block_number: int = 100,
    observability: Observability | None = None,
) -> Record:
    return Record(
        session_id=1,
        resumed_from_batch=None,
        ts_iso="2026-04-24T00:00:00Z",
        batch_id=batch_id,
        replay_core=ReplayCore(
            verb="eoatx",
            deadline_bytes=1_000,
            start_address="0x" + (0x1000).to_bytes(20, "big").hex(),
            end_address="0x" + (0x1001).to_bytes(20, "big").hex(),
            status="ok",
            block_hash="0x" + ("bb" * 32),
            block_number=block_number,
            block_timestamp=1_700_000_000 + batch_id,
            target_sha256="a" * 64,
        ),
        observability=observability
        or Observability(statecomp_snapshot={"blockNumber": block_number}),
    )
