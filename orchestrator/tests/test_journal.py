"""Journal writer/reader tests: chain-hash, fsync, resume, schema."""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path

import jsonschema
import pytest

from orchestrator.journal import (
    ChainHashMismatch,
    JournalReader,
    JournalSchemaError,
    JournalWriter,
    Observability,
    Record,
    ReplayCore,
    load_schema,
    serialize_replay_core,
    validate_record_dict,
)


_MISSING: object = object()


def _make_record(batch_id: int, *, snapshot: object = _MISSING, status: str = "ok") -> Record:
    snap = {"trieStats": {}, "blockNumber": 0} if snapshot is _MISSING else snapshot
    return Record(
        session_id=1,
        resumed_from_batch=None,
        ts_iso=f"2026-04-24T00:00:{batch_id:02d}Z",
        batch_id=batch_id,
        replay_core=ReplayCore(
            verb="eoatx",
            deadline_bytes=9_500_000,
            start_address=f"0x{batch_id:040x}",
            end_address=f"0x{batch_id + 1:040x}",
            status=status,
            block_hash=f"0x{'bb' * 32}",
            block_number=100 + batch_id,
        ),
        observability=Observability(
            observed_flat_bytes=1234,
            coeffs_before={"eoatx": {"accounts": 94.2}},
            coeffs_after={"eoatx": {"accounts": 95.1}},
            sigma_innov={"eoatx": {"accounts": 4.2}},
            alpha_current=0.087,
            innovation_ratio=0.024,
            residual_norm=124300.0,
            statecomp_snapshot=snap,  # type: ignore[arg-type]
        ),
    )


def test_chain_hash_covers_replay_core_only(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(3):
            w.append(_make_record(i))

    lines = [json.loads(line) for line in journal.read_text().splitlines()]
    # Re-derive record-3 chain hash from records 1 and 2.
    prev = "0" * 64
    for rec_dict in lines:
        rc = rec_dict["replay_core"]
        stored = rc["chain_hash"]
        tmp = dict(rc)
        tmp.pop("chain_hash")
        rc_bytes = json.dumps(tmp, sort_keys=True, separators=(",", ":")).encode("utf-8")
        expected = hashlib.sha256(prev.encode("ascii") + rc_bytes).hexdigest()
        assert stored == expected
        prev = stored


def test_chain_hash_breaks_on_tamper(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(3):
            w.append(_make_record(i))

    # Tamper: rewrite the middle record's deadline_bytes without recomputing chain hash.
    lines = journal.read_text().splitlines()
    bad = json.loads(lines[1])
    bad["replay_core"]["deadline_bytes"] = 12345
    lines[1] = json.dumps(bad, separators=(",", ":"))
    journal.write_text("\n".join(lines) + "\n")

    with pytest.raises(ChainHashMismatch):
        JournalReader(journal).verify_chain()


def test_observability_excluded_from_chain_hash(tmp_path: Path) -> None:
    journal_a = tmp_path / "a.jsonl"
    journal_b = tmp_path / "b.jsonl"
    record_a = _make_record(0)
    record_b = _make_record(0)
    record_b.observability.alpha_current = 999.0
    record_b.observability.residual_norm = -42.0
    with JournalWriter(journal_a) as wa:
        wa.append(record_a)
    with JournalWriter(journal_b) as wb:
        wb.append(record_b)
    assert record_a.replay_core.chain_hash == record_b.replay_core.chain_hash


def test_fsync_called_per_append(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[int] = []
    real_fsync = os.fsync
    monkeypatch.setattr(os, "fsync", lambda fd: calls.append(fd) or real_fsync(fd))
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(5):
            w.append(_make_record(i))
    assert len(calls) == 5


def test_reader_tail_returns_last_record(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(10):
            w.append(_make_record(i))
    tail = JournalReader(journal).tail()
    assert tail is not None
    assert tail.batch_id == 9


def test_resume_continues_chain(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        w.append(_make_record(0))
        first_hash = w.prev_chain_hash
    # Open a new writer on the same file — chain must continue.
    with JournalWriter(journal) as w:
        assert w.prev_chain_hash == first_hash
        w.append(_make_record(1))
    JournalReader(journal).verify_chain()  # full chain still valid end-to-end


def test_schema_round_trip(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        w.append(_make_record(0))
    schema = load_schema()
    for line in journal.read_text().splitlines():
        jsonschema.validate(instance=json.loads(line), schema=schema)


def test_statecomp_snapshot_null_preserves_hash(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    record = _make_record(0, snapshot=None, status="sensor_wait_timeout")
    with JournalWriter(journal) as w:
        w.append(record)
    raw = json.loads(journal.read_text().strip())
    assert raw["observability"]["statecomp_snapshot"] is None
    JournalReader(journal).verify_chain()


def test_unknown_status_rejected_by_schema(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    bad = _make_record(0)
    object.__setattr__(bad.replay_core, "status", "garbage")
    with JournalWriter(journal) as w:
        w.append(bad)
    schema = load_schema()
    line = json.loads(journal.read_text().strip())
    with pytest.raises(jsonschema.ValidationError):
        jsonschema.validate(instance=line, schema=schema)


def test_reader_rejects_oversized_address(tmp_path: Path) -> None:
    """Crafted >40-hex address would DoS replay.int() — schema must reject."""
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        w.append(_make_record(0))
    lines = journal.read_text().splitlines()
    body = json.loads(lines[0])
    body["replay_core"]["start_address"] = "0x" + ("f" * 1_000_000)
    lines[0] = json.dumps(body, separators=(",", ":"))
    journal.write_text("\n".join(lines) + "\n")

    with pytest.raises(JournalSchemaError) as excinfo:
        list(JournalReader(journal))
    assert excinfo.value.batch_id == 0


def test_reader_rejects_malformed_json(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    journal.write_text('{"schema": 1, "incomplete":\n')
    with pytest.raises(JournalSchemaError):
        list(JournalReader(journal))


def test_reader_rejects_missing_required_field(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        w.append(_make_record(0))
    body = json.loads(journal.read_text().strip())
    del body["replay_core"]["block_hash"]
    journal.write_text(json.dumps(body, separators=(",", ":")) + "\n")
    with pytest.raises(JournalSchemaError):
        list(JournalReader(journal))


def test_validate_record_dict_rejects_bad_status() -> None:
    from orchestrator.journal import JournalSchemaError

    bad = {
        "schema": 1,
        "session_id": 1,
        "ts_iso": "2026-04-24T00:00:00Z",
        "batch_id": 0,
        "replay_core": {
            "verb": "eoatx",
            "deadline_bytes": 1000,
            "start_address": "0x00",
            "end_address": "0x01",
            "status": "garbage",
            "block_hash": "0x" + "bb" * 32,
            "block_number": 100,
            "chain_hash": "0" * 64,
        },
        "observability": {
            "observed_flat_bytes": 0,
            "coeffs_before": {},
            "coeffs_after": {},
            "sigma_innov": {},
            "alpha_current": 0.0,
            "innovation_ratio": 0.0,
            "residual_norm": 0.0,
            "statecomp_snapshot": None,
        },
    }
    with pytest.raises(JournalSchemaError):
        validate_record_dict(bad)


def test_reverse_seek_finds_last_chain_hash(tmp_path: Path) -> None:
    """H7: reopening a writer on a big journal must not scan the whole file."""
    from orchestrator.journal import _read_last_chain_hash

    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(20):
            w.append(_make_record(i))
        expected = w.prev_chain_hash

    assert _read_last_chain_hash(journal) == expected


def test_reverse_seek_tolerates_missing_trailing_newline(tmp_path: Path) -> None:
    from orchestrator.journal import _read_last_chain_hash

    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        w.append(_make_record(0))
        w.append(_make_record(1))
        expected = w.prev_chain_hash
    # Strip the trailing newline — simulates a writer that didn't flush a final \n.
    raw = journal.read_bytes().rstrip(b"\n")
    journal.write_bytes(raw)
    assert _read_last_chain_hash(journal) == expected


def test_verify_chain_from_checkpoint(tmp_path: Path) -> None:
    """H7: verify_chain_from lets resume verify only the suffix past a checkpoint."""
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(5):
            w.append(_make_record(i))
    reader = JournalReader(journal)
    records = list(reader)
    checkpoint = records[2]
    # Should succeed: start verification from record 2's chain_hash, batch_id=3 onwards.
    reader.verify_chain_from(checkpoint.replay_core.chain_hash, min_batch_id=3)


def test_verify_chain_from_detects_tamper_in_suffix(tmp_path: Path) -> None:
    journal = tmp_path / "j.jsonl"
    with JournalWriter(journal) as w:
        for i in range(5):
            w.append(_make_record(i))
    lines = journal.read_text().splitlines()
    body = json.loads(lines[4])
    body["replay_core"]["deadline_bytes"] = 12345
    lines[4] = json.dumps(body, separators=(",", ":"))
    journal.write_text("\n".join(lines) + "\n")
    reader = JournalReader(journal)
    records = list(reader)
    with pytest.raises(ChainHashMismatch):
        reader.verify_chain_from(
            records[2].replay_core.chain_hash, min_batch_id=3
        )


def test_serialize_replay_core_excludes_chain_hash() -> None:
    rc = ReplayCore(
        verb="eoatx",
        deadline_bytes=100,
        start_address="0x00",
        end_address="0x01",
        status="ok",
        block_hash="0xab",
        block_number=1,
        chain_hash="deadbeef",
    )
    payload = serialize_replay_core(rc).decode()
    assert "chain_hash" not in payload
