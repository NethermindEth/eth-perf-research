"""Lifecycle tests: auto-detect startup, resume guard, main loop control flow."""
from __future__ import annotations

from pathlib import Path

import httpx
import pytest

from orchestrator.facade import FacadeContext
from orchestrator.journal import (
    JournalWriter,
    Observability,
    Record,
    ReplayCore,
)
from orchestrator.lifecycle import (
    LifecycleDeps,
    ResumeRefused,
    StartupMode,
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


def _record(batch_id: int, *, block_number: int = 100) -> Record:
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
        observability=Observability(statecomp_snapshot={"blockNumber": block_number}),
    )
