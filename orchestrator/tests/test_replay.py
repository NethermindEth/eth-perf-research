"""Replay mode tests — exercises exit codes 0–4.

Uses a stub RPC that returns deterministic block hashes so replay can re-commit
without a real Nethermind.
"""
from __future__ import annotations

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
from orchestrator.replay import (
    EXIT_BLOCK_HASH,
    EXIT_CHAIN_HASH,
    EXIT_FACADE,
    EXIT_OK,
    EXIT_STATE_ROOT,
    replay,
)


class _StubRpc:
    def __init__(self, *, hash_by_batch: dict[int, str] | None = None) -> None:
        self._call_idx = 0
        self._hash_by_batch = hash_by_batch or {}

    def testing_commit_block_v1(self, signed_txs_rlp: list[bytes]) -> str:
        import hashlib

        digest = hashlib.sha256(b"".join(signed_txs_rlp)).hexdigest()
        self._call_idx += 1
        return "0x" + digest

    def eth_get_block_by_hash(self, block_hash: str, full: bool = True) -> dict[str, Any]:
        return {"hash": block_hash}

    def eth_get_block_by_number(self, number="latest", full=False) -> dict[str, Any]:
        return {"stateRoot": "0x" + "aa" * 32}

    def close(self) -> None: ...


def _seed_journal(tmp_path: Path, *, verb: str = "eoatx", n: int = 3) -> tuple[Path, list[str]]:
    """Generate a valid journal: dispatch each batch, record the RPC-produced block hash."""
    ctx = FacadeContext(base_address=b"\x00" * 20, revision=0)
    rpc = _StubRpc()
    journal = tmp_path / "orchestrator.journal.jsonl"
    block_hashes: list[str] = []
    with JournalWriter(journal) as w:
        for i in range(n):
            start = ctx.address_cursor
            txs = dispatch(verb, deadline_bytes=5_000, context=ctx)
            end = ctx.address_cursor
            block_hash = rpc.testing_commit_block_v1([tx.rlp for tx in txs])
            block_hashes.append(block_hash)
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
    return journal, block_hashes


def test_replay_happy_path(tmp_path: Path) -> None:
    journal, _ = _seed_journal(tmp_path)
    # Fresh stub RPC — deterministic hashes will match.
    rc = replay(journal, "http://stub", rpc=_StubRpc())
    assert rc == EXIT_OK


def test_replay_still_verifies_block_hash_when_status_is_sensor_wait_timeout(
    tmp_path: Path,
) -> None:
    """H4: a tampered journal with status=sensor_wait_timeout must not bypass block-hash check."""
    import hashlib

    journal, _ = _seed_journal(tmp_path, n=2)
    lines = journal.read_text().splitlines()
    body = json.loads(lines[1])
    body["replay_core"]["status"] = "sensor_wait_timeout"
    body["replay_core"]["block_hash"] = "0x" + "aa" * 32  # forged
    # Recompute chain hash so we don't short-circuit on EXIT_CHAIN_HASH.
    prev = json.loads(lines[0])["replay_core"]["chain_hash"]
    rc = dict(body["replay_core"])
    rc.pop("chain_hash")
    rc_bytes = json.dumps(rc, sort_keys=True, separators=(",", ":")).encode()
    body["replay_core"]["chain_hash"] = hashlib.sha256(prev.encode() + rc_bytes).hexdigest()
    lines[1] = json.dumps(body, separators=(",", ":"))
    journal.write_text("\n".join(lines) + "\n")

    rc_exit = replay(journal, "http://stub", rpc=_StubRpc())
    assert rc_exit == EXIT_BLOCK_HASH


def test_replay_chain_hash_mismatch_exits_1(tmp_path: Path) -> None:
    journal, _ = _seed_journal(tmp_path)
    lines = journal.read_text().splitlines()
    mutated = json.loads(lines[1])
    mutated["replay_core"]["deadline_bytes"] = 999_999
    lines[1] = json.dumps(mutated, separators=(",", ":"))
    journal.write_text("\n".join(lines) + "\n")

    rc = replay(journal, "http://stub", rpc=_StubRpc())
    assert rc == EXIT_CHAIN_HASH


def test_replay_block_hash_mismatch_exits_2(tmp_path: Path) -> None:
    journal, _ = _seed_journal(tmp_path)

    class _BadHash(_StubRpc):
        def testing_commit_block_v1(self, signed_txs_rlp):
            return "0x" + "ff" * 32  # always the same bogus hash

    rc = replay(journal, "http://stub", rpc=_BadHash())
    assert rc == EXIT_BLOCK_HASH


def test_replay_state_root_mismatch_exits_3(tmp_path: Path) -> None:
    journal, _ = _seed_journal(tmp_path)
    manifest = tmp_path / "run-manifest.json"
    manifest.write_text(
        json.dumps({"final_state_root": "0x" + "bb" * 32}), encoding="utf-8"
    )
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_STATE_ROOT


def test_replay_state_root_match_exits_0(tmp_path: Path) -> None:
    journal, _ = _seed_journal(tmp_path)
    manifest = tmp_path / "run-manifest.json"
    manifest.write_text(
        json.dumps({"final_state_root": "0x" + "aa" * 32}), encoding="utf-8"
    )
    rc = replay(journal, "http://stub", rpc=_StubRpc(), manifest_path=manifest)
    assert rc == EXIT_OK


def test_replay_unknown_verb_exits_4(tmp_path: Path) -> None:
    journal = tmp_path / "orchestrator.journal.jsonl"
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
    rc = replay(journal, "http://stub", rpc=_StubRpc())
    assert rc == EXIT_FACADE


def test_replay_ignores_observability(tmp_path: Path) -> None:
    journal, _ = _seed_journal(tmp_path)
    # Zero out observability fields via line-by-line rewrite.
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

    rc = replay(journal, "http://stub", rpc=_StubRpc())
    assert rc == EXIT_OK


def test_replay_empty_journal_exits_0(tmp_path: Path) -> None:
    journal = tmp_path / "empty.jsonl"
    journal.write_text("")
    rc = replay(journal, "http://stub", rpc=_StubRpc())
    assert rc == EXIT_OK


def test_replay_cursor_drift_exits_4(tmp_path: Path) -> None:
    journal, _ = _seed_journal(tmp_path)
    lines = journal.read_text().splitlines()
    body = json.loads(lines[1])
    # Tamper end_address so it no longer matches the adapter's expected cursor.
    body["replay_core"]["end_address"] = "0x" + (99999).to_bytes(20, "big").hex()
    # Recompute chain hash so we don't fall into EXIT_CHAIN_HASH first.
    import hashlib
    prev = json.loads(lines[0])["replay_core"]["chain_hash"]
    rc_for_hash = dict(body["replay_core"])
    rc_for_hash.pop("chain_hash")
    rc_bytes = json.dumps(rc_for_hash, sort_keys=True, separators=(",", ":")).encode()
    body["replay_core"]["chain_hash"] = hashlib.sha256(prev.encode() + rc_bytes).hexdigest()
    lines[1] = json.dumps(body, separators=(",", ":"))
    # Record 2 depends on record 1's chain_hash — recompute all subsequent hashes too.
    chain = body["replay_core"]["chain_hash"]
    if len(lines) > 2:
        body2 = json.loads(lines[2])
        rc2 = dict(body2["replay_core"])
        rc2.pop("chain_hash")
        rc2_bytes = json.dumps(rc2, sort_keys=True, separators=(",", ":")).encode()
        body2["replay_core"]["chain_hash"] = hashlib.sha256(chain.encode() + rc2_bytes).hexdigest()
        lines[2] = json.dumps(body2, separators=(",", ":"))
    journal.write_text("\n".join(lines) + "\n")
    rc = replay(journal, "http://stub", rpc=_StubRpc())
    assert rc == EXIT_FACADE
