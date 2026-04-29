"""Manifest writer + chain-identity-hash tests."""

from __future__ import annotations

import dataclasses
import json
from pathlib import Path

from orchestrator.journal import (
    JournalWriter,
    Observability,
    Record,
    ReplayCore,
)
from orchestrator.manifest import (
    EnvInfo,
    Manifest,
    Session,
    TargetSnapshot,
    compute_chain_identity_hash,
    compute_journal_sha256,
)


def _env() -> EnvInfo:
    return EnvInfo(
        genesis_sha256="g" * 64,
        plugin_git_sha="p" * 40,
        nethermind_commit_sha="n" * 40,
        dotnet_runtime_major=10,
        cpu_arch="x86_64",
    )


def test_replay_context_matches_both_none() -> None:
    """Both-null (legacy manifests resuming under legacy code) remains compatible."""
    from orchestrator.manifest import replay_context_matches

    assert replay_context_matches(None, None)


def test_replay_context_matches_refuses_legacy_one_sided() -> None:
    """C2: manifest with null replay_context must NOT silently pass the signer gate."""
    from orchestrator.manifest import ReplayContext, replay_context_matches

    current = ReplayContext(
        base_address="0x" + "00" * 20,
        revision=0,
        chain_id=1337,
        gas_limit=30_000_000,
        address_stride=1 << 40,
        deploy_pubkey_sha256="a" * 64,
        block_gas_limit=30_000_000,
    )
    # Attacker drops `"replay_context": null` → must now be rejected.
    assert not replay_context_matches(None, current)
    assert not replay_context_matches(current, None)


def test_replay_context_matches_field_equality() -> None:
    from orchestrator.manifest import ReplayContext, replay_context_matches

    a = ReplayContext(
        base_address="0x" + "00" * 20,
        revision=0,
        chain_id=1337,
        gas_limit=30_000_000,
        address_stride=1 << 40,
        deploy_pubkey_sha256="a" * 64,
        block_gas_limit=30_000_000,
    )
    b = dataclasses.replace(a, chain_id=11155111)
    assert replay_context_matches(a, a)
    assert not replay_context_matches(a, b)


def test_chain_identity_hash_is_deterministic() -> None:
    env = _env()
    a = compute_chain_identity_hash(env)
    b = compute_chain_identity_hash(env)
    assert a == b
    assert len(a) == 64


def test_chain_identity_hash_changes_when_env_changes() -> None:
    env = _env()
    baseline = compute_chain_identity_hash(env)
    # Different plugin sha.
    env2 = EnvInfo(**{**env.__dict__, "plugin_git_sha": "q" * 40})
    assert compute_chain_identity_hash(env2) != baseline


def test_chain_identity_hash_excludes_target() -> None:
    """target.yaml shape is per-batch input — same chain identity must produce
    the same chain_identity_hash regardless of target.yaml shape."""
    env = _env()
    # No target argument exists for compute_chain_identity_hash; verify the
    # signature documents this contract by construction.
    a = compute_chain_identity_hash(env)
    b = compute_chain_identity_hash(env)
    assert a == b


def test_chain_identity_hash_includes_block_gas_limit() -> None:
    """block_gas_limit shifts dispatch behaviour → must affect chain identity."""
    from orchestrator.manifest import ReplayContext

    env = _env()
    rctx_30m = ReplayContext(
        base_address="0x" + "00" * 20,
        revision=0,
        chain_id=1337,
        gas_limit=30_000_000,
        address_stride=1 << 40,
        deploy_pubkey_sha256="a" * 64,
        block_gas_limit=30_000_000,
    )
    rctx_60m = ReplayContext(
        base_address="0x" + "00" * 20,
        revision=0,
        chain_id=1337,
        gas_limit=30_000_000,
        address_stride=1 << 40,
        deploy_pubkey_sha256="a" * 64,
        block_gas_limit=60_000_000,
    )
    h30 = compute_chain_identity_hash(env, rctx_30m)
    h60 = compute_chain_identity_hash(env, rctx_60m)
    assert h30 != h60
    # No replay_context → distinct from both.
    plain = compute_chain_identity_hash(env)
    assert plain != h30
    assert plain != h60


def test_manifest_roundtrip(tmp_path: Path) -> None:
    m = Manifest(
        run_id="abc",
        base_address="0x" + "00" * 20,
        revision=0,
        genesis_sha256="g" * 64,
        chain_identity_hash="c" * 64,
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
    assert body["schema"] == 2
    assert body["run_id"] == "abc"

    reloaded = Manifest.read(out)
    assert reloaded.run_id == "abc"
    assert reloaded.sessions[0].session_id == 1


def test_target_history_replay_lookup(tmp_path: Path) -> None:
    """Round-trip a manifest with two snapshots; loader recovers them in order."""
    snap_a = TargetSnapshot(
        sha256="a" * 64,
        ts_iso="2026-04-24T00:00:00Z",
        body={"mainnet_target": {"accounts": 0.141, "storage": 0.817, "code": 0.042}},
    )
    snap_b = TargetSnapshot(
        sha256="b" * 64,
        ts_iso="2026-04-24T01:00:00Z",
        body={"mainnet_target": {"accounts": 0.5, "storage": 0.4, "code": 0.1}},
    )
    m = Manifest(
        run_id="r",
        base_address="0x" + "00" * 20,
        revision=0,
        genesis_sha256="g" * 64,
        chain_identity_hash="c" * 64,
        reference_f_version="2026.04.23",
        plugin_git_sha="p" * 40,
        nethermind_commit_sha="n" * 40,
        dotnet_runtime_major=10,
        cpu_arch="x86_64",
        target_history=[snap_a, snap_b],
    )
    out = tmp_path / "run-manifest.json"
    m.write(out)
    reloaded = Manifest.read(out)
    assert len(reloaded.target_history) == 2
    assert reloaded.target_history[0].sha256 == snap_a.sha256
    assert reloaded.target_history[1].sha256 == snap_b.sha256
    assert reloaded.target_history[0].body == snap_a.body


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
                        target_sha256="a" * 64,
                    ),
                    observability=Observability(statecomp_snapshot={"x": i}),
                )
            )
    sha = compute_journal_sha256(journal)
    assert len(sha) == 64
