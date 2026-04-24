"""Lifecycle tests: auto-detect startup, resume guard, main loop control flow."""
from __future__ import annotations

from pathlib import Path

import httpx
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
from orchestrator.manifest import EnvInfo, Manifest, compute_composition_hash
from orchestrator.rpc import RpcClient
from orchestrator.sensor import SensorClient
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
        total_batch_bytes=500_000,
        source_sha256="t" * 64,
    )


def test_resolve_fresh_when_no_journal(tmp_path: Path) -> None:
    decision = resolve_startup_mode(tmp_path, composition_hash="abc", head_block=None)
    assert decision.mode is StartupMode.FRESH


def test_resolve_resume_when_valid(tmp_path: Path) -> None:
    target = _target()
    env = _env()
    comp_hash = compute_composition_hash(target.source_sha256, env)

    # Seed: a journal record + manifest matching composition_hash.
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0))
    manifest = Manifest(
        run_id="r",
        target_yaml_sha256=target.source_sha256,
        base_address="0x" + target.base_address.hex(),
        revision=0,
        genesis_sha256=env.genesis_sha256,
        composition_hash=comp_hash,
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
        target_yaml_sha256=target.source_sha256,
        base_address="0x" + target.base_address.hex(),
        revision=0,
        genesis_sha256=env.genesis_sha256,
        composition_hash="WRONG",
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
    target = _target()
    env = _env()
    comp = compute_composition_hash(target.source_sha256, env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    with pytest.raises(ResumeRefused):
        resolve_startup_mode(tmp_path, composition_hash=comp, head_block=105)


def test_reconcile_pending_clears_when_head_matches_tail(tmp_path: Path) -> None:
    """C3: crash between pending and commit — block never made it on-chain."""
    env = _env()
    target = _target()
    comp = compute_composition_hash(target.source_sha256, env)
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
        ),
    )
    decision = resolve_startup_mode(tmp_path, composition_hash=comp, head_block=100)
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


def test_reconcile_refuses_on_tx_set_mismatch(tmp_path: Path) -> None:
    """C-RECONCILE-TRUST: a misreported head block must NOT be silently trusted."""
    env = _env()
    target = _target()
    comp = compute_composition_hash(target.source_sha256, env)
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
            composition_hash=comp,
            head_block=101,
            rpc=_AutoMinedEmpty(),
            facade_ctx=ctx,
        )


def test_state_dir_lock_refuses_second_holder(tmp_path: Path) -> None:
    """C-NO-LOCK: two orchestrators on the same state dir must not both start."""
    with _state_dir_lock(tmp_path):
        with pytest.raises(RunAlreadyActive):
            with _state_dir_lock(tmp_path):
                pass  # unreachable


def test_reconcile_pending_synthesizes_record_when_head_advanced(tmp_path: Path) -> None:
    """C3: crash between commit and journal append — replay missing record from pending."""
    env = _env()
    target = _target()
    comp = compute_composition_hash(target.source_sha256, env)
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
        ),
    )

    class _Rpc:
        def eth_get_block_by_number(self, number, full=False):
            return {"hash": "0x" + "cc" * 32, "stateRoot": "0x00"}

        def close(self) -> None: ...

    decision = resolve_startup_mode(
        tmp_path, composition_hash=comp, head_block=101, rpc=_Rpc()
    )
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
    target = _target()
    comp = compute_composition_hash(target.source_sha256, env)
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
        ),
    )

    class _Rpc:
        def eth_get_block_by_number(self, number, full=False):
            return {"hash": "0x" + "cc" * 32}

        def close(self) -> None: ...

    resolve_startup_mode(tmp_path, composition_hash=comp, head_block=101, rpc=_Rpc())
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
            assert not stop.requested
            assert signal.getsignal(signal.SIGINT) is not sentinel
        # After the context exits, the previous handlers must be back in place.
        assert signal.getsignal(signal.SIGINT) is sentinel
        assert signal.getsignal(signal.SIGTERM) is sentinel
    finally:
        signal.signal(signal.SIGINT, prior_sigint)
        signal.signal(signal.SIGTERM, prior_sigterm)


def test_refuse_when_head_is_unknown(tmp_path: Path) -> None:
    """H2: RPC unreachable must not silently allow resume."""
    target = _target()
    env = _env()
    comp = compute_composition_hash(target.source_sha256, env)
    journal = tmp_path / "orchestrator.journal.jsonl"
    with JournalWriter(journal) as w:
        w.append(_record(batch_id=0, block_number=100))
    with pytest.raises(ResumeRefused, match="head unknown"):
        resolve_startup_mode(tmp_path, composition_hash=comp, head_block=None)


def test_run_with_max_batches_writes_manifest(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
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

        def testing_commit_block_v1(self, txs):
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

        def close(self) -> None: ...

    deps = LifecycleDeps(sensor=_StubSensor(), rpc=_StubRpc(), probe_executor=None)
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
        ),
        observability=observability
        or Observability(statecomp_snapshot={"blockNumber": block_number}),
    )
