"""Manifest writer + composition-hash tests."""
from __future__ import annotations

import json
from pathlib import Path

from orchestrator.manifest import (
    EnvInfo,
    Manifest,
    Session,
    compute_composition_hash,
    compute_journal_sha256,
)
from orchestrator.journal import (
    JournalWriter,
    Observability,
    Record,
    ReplayCore,
)


def _env() -> EnvInfo:
    return EnvInfo(
        genesis_sha256="g" * 64,
        plugin_git_sha="p" * 40,
        nethermind_commit_sha="n" * 40,
        dotnet_runtime_major=10,
        cpu_arch="x86_64",
    )


def test_composition_hash_is_deterministic() -> None:
    env = _env()
    a = compute_composition_hash("t" * 64, env)
    b = compute_composition_hash("t" * 64, env)
    assert a == b
    assert len(a) == 64


def test_composition_hash_changes_when_any_input_changes() -> None:
    env = _env()
    baseline = compute_composition_hash("t" * 64, env)
    # Different target.
    assert compute_composition_hash("u" * 64, env) != baseline
    # Different plugin sha.
    env2 = EnvInfo(**{**env.__dict__, "plugin_git_sha": "q" * 40})
    assert compute_composition_hash("t" * 64, env2) != baseline


def test_manifest_roundtrip(tmp_path: Path) -> None:
    m = Manifest(
        run_id="abc",
        target_yaml_sha256="t" * 64,
        base_address="0x" + "00" * 20,
        revision=0,
        genesis_sha256="g" * 64,
        composition_hash="c" * 64,
        reference_f_version="2026.04.23",
        plugin_git_sha="p" * 40,
        nethermind_commit_sha="n" * 40,
        dotnet_runtime_major=10,
        cpu_arch="x86_64",
        sessions=[Session(session_id=1, started_at="s", stopped_at="e", stop_reason="manual")],
        journal_sha256="j" * 64,
    )
    out = tmp_path / "run-manifest.json"
    m.write(out)
    body = json.loads(out.read_text())
    assert body["schema"] == 1
    assert body["run_id"] == "abc"

    reloaded = Manifest.read(out)
    assert reloaded.run_id == "abc"
    assert reloaded.sessions[0].session_id == 1


def test_compute_journal_sha256(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(3):
            w.append(
                Record(
                    session_id=1,
                    resumed_from_batch=None,
                    ts_iso="2026-04-24T00:00:00Z",
                    batch_id=i,
                    replay_core=ReplayCore(
                        verb="eoatx",
                        deadline_bytes=1000,
                        start_address="0x" + (i).to_bytes(20, "big").hex(),
                        end_address="0x" + (i + 1).to_bytes(20, "big").hex(),
                        status="ok",
                        block_hash="0x" + ("bb" * 32),
                        block_number=100 + i,
                    ),
                    observability=Observability(statecomp_snapshot={"x": i}),
                )
            )
    sha = compute_journal_sha256(journal)
    assert len(sha) == 64
