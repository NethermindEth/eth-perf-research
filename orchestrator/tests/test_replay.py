"""Replay mode tests — exercises exit codes 0–5."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import pytest

from orchestrator.facade import FacadeContext, dispatch
from orchestrator.journal import (
    JournalWriter,
    Observability,
    Record,
    ReplayCore,
)
from orchestrator.lifecycle import build_replay_context
from orchestrator.manifest import Manifest
from orchestrator.replay import (
    EXIT_BLOCK_HASH,
    EXIT_CHAIN_HASH,
    EXIT_FACADE,
    EXIT_MANIFEST,
    EXIT_OK,
    EXIT_STATE_ROOT,
    replay,
)


class _StubRpc:
    """Stub: deterministic block hash from sha256(concat(signed txs)); static state root."""

    def __init__(self) -> None:
        self._call_idx = 0

    def testing_commit_block_v1(self, signed_txs_rlp: list[bytes]) -> str:
        digest = hashlib.sha256(b"".join(signed_txs_rlp)).hexdigest()
        self._call_idx += 1
        return "0x" + digest

    def eth_get_block_by_hash(self, block_hash: str, full: bool = True) -> dict[str, Any]:
        return {"hash": block_hash}

    def eth_get_block_by_number(self, number: str | int = "latest", full: bool = False) -> dict[str, Any]:
        return {"stateRoot": "0x" + "aa" * 32}

    def close(self) -> None: ...


def _seed_journal(
    tmp_path: Path, *, verb: str = "eoatx", n: int = 3, state_root: str | None = None
) -> tuple[Path, Path]:
    """Generate a valid journal + manifest pair that replay can consume."""
    ctx = FacadeContext(base_address=b"\x00" * 20, revision=0)
    rpc = _StubRpc()
    journal = tmp_path / "orchestrator.journal.jsonl"
    manifest_path = tmp_path / "run-manifest.json"
    with JournalWriter(journal) as w:
        for i in range(n):
            start = ctx.address_cursor
            txs = dispatch(verb, deadline_bytes=5_000, context=ctx)
            end = ctx.address_cursor
            block_hash = rpc.testing_commit_block_v1([tx.rlp for tx in txs])
            w.append(
                Record(
                    session_id=1,
                    resumed_from_batch=None,
                    ts_iso="2026-04-24T00:00:00Z",
                    batch_id=i,
                    replay_core=ReplayCore(
                        verb=verb,
                        deadline_bytes=5_000,
                        start_address="0x" + start.to_bytes(20, "big").hex(),
                        end_address="0x" + end.to_bytes(20, "big").hex(),
                        status="ok",
                        block_hash=block_hash,
                        block_number=100 + i,
                    ),
                    observability=Observability(statecomp_snapshot={"x": i}),
                )
            )
    manifest = Manifest(
        run_id="r",
        target_yaml_sha256="t" * 64,
        base_address="0x" + ctx.base_address.hex(),
        revision=0,
        genesis_sha256="g" * 64,
        composition_hash="c" * 64,
        reference_f_version="2026.04.23",
        plugin_git_sha="p" * 40,
        nethermind_commit_sha="n" * 40,
        dotnet_runtime_major=10,
        cpu_arch="x86_64",
        replay_context=build_replay_context(
            FacadeContext(base_address=b"\x00" * 20, revision=0)
        ),
        final_state_root=state_root,
    )
    manifest.write(manifest_path)
    return journal, manifest_path


def _recompute_chain_hash(lines: list[str], idx: int) -> list[str]:
    """Re-chain from `idx` onwards after in-place replay_core edits so we don't exit 1."""
    prev = "0" * 64 if idx == 0 else json.loads(lines[idx - 1])["replay_core"]["chain_hash"]
    for i in range(idx, len(lines)):
        body = json.loads(lines[i])
        rc = dict(body["replay_core"])
        rc.pop("chain_hash")
        rc_bytes = json.dumps(rc, sort_keys=True, separators=(",", ":")).encode()
        body["replay_core"]["chain_hash"] = hashlib.sha256(prev.encode() + rc_bytes).hexdigest()
        lines[i] = json.dumps(body, separators=(",", ":"))
        prev = body["replay_core"]["chain_hash"]
    return lines


def test_replay_happy_path(tmp_path: Path) -> None:
    journal, manifest = _seed_journal(tmp_path)
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_OK


def test_replay_still_verifies_block_hash_when_status_is_sensor_wait_timeout(
    tmp_path: Path,
) -> None:
    """H4: tampered journal with status=sensor_wait_timeout must not bypass block-hash check."""
    journal, manifest = _seed_journal(tmp_path, n=2)
    lines = journal.read_text().splitlines()
    body = json.loads(lines[1])
    body["replay_core"]["status"] = "sensor_wait_timeout"
    body["replay_core"]["block_hash"] = "0x" + "aa" * 32  # forged
    lines[1] = json.dumps(body, separators=(",", ":"))
    lines = _recompute_chain_hash(lines, 1)
    journal.write_text("\n".join(lines) + "\n")

    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_BLOCK_HASH


def test_replay_chain_hash_mismatch_exits_1(tmp_path: Path) -> None:
    journal, manifest = _seed_journal(tmp_path)
    lines = journal.read_text().splitlines()
    mutated = json.loads(lines[1])
    mutated["replay_core"]["deadline_bytes"] = 999_999
    lines[1] = json.dumps(mutated, separators=(",", ":"))
    journal.write_text("\n".join(lines) + "\n")

    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_CHAIN_HASH


def test_replay_block_hash_mismatch_exits_2(tmp_path: Path) -> None:
    journal, manifest = _seed_journal(tmp_path)

    class _BadHash(_StubRpc):
        def testing_commit_block_v1(self, signed_txs_rlp):
            return "0x" + "ff" * 32

    rc = replay(journal, "http://stub", rpc=_BadHash(), manifest_path=manifest)
    assert rc == EXIT_BLOCK_HASH


def test_replay_state_root_mismatch_exits_3(tmp_path: Path) -> None:
    journal, manifest = _seed_journal(tmp_path, state_root="0x" + "bb" * 32)
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_STATE_ROOT


def test_replay_state_root_match_exits_0(tmp_path: Path) -> None:
    journal, manifest = _seed_journal(tmp_path, state_root="0x" + "aa" * 32)
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_OK


def test_replay_unknown_verb_exits_4(tmp_path: Path) -> None:
    # Seed a valid manifest so we get past EXIT_MANIFEST.
    _, manifest = _seed_journal(tmp_path, n=1)
    journal = tmp_path / "orchestrator.journal.jsonl"
    journal.unlink()
    with JournalWriter(journal) as w:
        w.append(
            Record(
                session_id=1,
                resumed_from_batch=None,
                ts_iso="2026-04-24T00:00:00Z",
                batch_id=0,
                replay_core=ReplayCore(
                    verb="garbage_verb",
                    deadline_bytes=1000,
                    start_address="0x" + (0).to_bytes(20, "big").hex(),
                    end_address="0x" + (1).to_bytes(20, "big").hex(),
                    status="ok",
                    block_hash="0x" + "bb" * 32,
                    block_number=100,
                ),
                observability=Observability(statecomp_snapshot={}),
            )
        )
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_FACADE


def test_replay_ignores_observability(tmp_path: Path) -> None:
    journal, manifest = _seed_journal(tmp_path)
    lines = journal.read_text().splitlines()
    rewritten = []
    for line in lines:
        body = json.loads(line)
        body["observability"] = {
            "observed_flat_bytes": 0,
            "coeffs_before": {},
            "coeffs_after": {},
            "sigma_innov": {},
            "alpha_current": 0.0,
            "innovation_ratio": 0.0,
            "residual_norm": 0.0,
            "statecomp_snapshot": None,
        }
        rewritten.append(json.dumps(body, separators=(",", ":")))
    journal.write_text("\n".join(rewritten) + "\n")

    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_OK


def test_replay_empty_journal_exits_0(tmp_path: Path) -> None:
    _, manifest = _seed_journal(tmp_path, n=1)
    journal = tmp_path / "empty.jsonl"
    journal.write_text("")
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_OK


def test_replay_cursor_drift_exits_4(tmp_path: Path) -> None:
    journal, manifest = _seed_journal(tmp_path)
    lines = journal.read_text().splitlines()
    body = json.loads(lines[1])
    body["replay_core"]["end_address"] = "0x" + (99999).to_bytes(20, "big").hex()
    lines[1] = json.dumps(body, separators=(",", ":"))
    lines = _recompute_chain_hash(lines, 1)
    journal.write_text("\n".join(lines) + "\n")
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_FACADE


def test_replay_missing_manifest_exits_5(tmp_path: Path) -> None:
    """C1: replay without the manifest cannot reconstruct the facade context."""
    journal, _ = _seed_journal(tmp_path)
    rc = replay(
        journal,
        "http://stub",
        rpc=_StubRpc(),
        manifest_path=tmp_path / "does_not_exist.json",
    )
    assert rc == EXIT_MANIFEST


def test_replay_refuses_on_signer_fingerprint_mismatch(tmp_path: Path) -> None:
    """H1: a different signing key must not masquerade as the manifest's signer."""
    journal, manifest = _seed_journal(tmp_path, n=1)
    rc = replay(
        journal,
        "http://stub",
        rpc=_StubRpc(),
        manifest_path=manifest,
        deploy_private_key=b"\x22" * 32,  # wrong key
    )
    assert rc == EXIT_MANIFEST
