#!/usr/bin/env -S uv run --script
# /// script
# dependencies = []
# requires-python = ">=3.10"
# ///
"""Summarize a directory of ablation runs into one readable table.

Reads ``ablation-runs/<name>/state/{run-manifest.json, orchestrator.journal.jsonl}``
for every name and prints one row per ablation:

  name              batches  stop_reason          total_kB  Δacc  Δsto  Δcod  F-cov  verbs

Usage:
  python3 scripts/summarize_ablations.py [ablation_root]
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

TARGET_TOTAL = 10_485_760
TARGET_SHARES = {"accounts": 0.141, "storage": 0.817, "code": 0.043}
AXES = ("accounts", "storage", "code")


def hex_int(v: object) -> int:
    if isinstance(v, int):
        return v
    s = str(v)
    return int(s, 16) if s.startswith(("0x", "0X")) else int(s)


def summarize_one(state_dir: Path) -> dict:
    manifest = json.loads((state_dir / "run-manifest.json").read_text())
    session = manifest["sessions"][0]
    journal_lines = (state_dir / "orchestrator.journal.jsonl").read_text().splitlines()
    last = json.loads(journal_lines[-1])
    snapshot = last["observability"]["statecomp_snapshot"]
    if snapshot is None:
        # Fall back to the second-to-last record.
        for line in reversed(journal_lines[:-1]):
            rec = json.loads(line)
            if rec["observability"]["statecomp_snapshot"] is not None:
                snapshot = rec["observability"]["statecomp_snapshot"]
                break
    ts = snapshot["trieStats"]
    acc = hex_int(ts["accountTrieBytes"])
    sto = hex_int(ts["storageTrieBytes"])
    cod = hex_int(ts["codeBytesTotal"])
    total = acc + sto + cod
    if total == 0:
        return {"empty": True}
    shares = {
        "accounts": acc / total,
        "storage": sto / total,
        "code": cod / total,
    }
    deltas = {a: (shares[a] - TARGET_SHARES[a]) * 100.0 for a in AXES}

    # F-coverage: count verbs with any non-zero F entry across the run.
    seen_F: set[str] = set()
    verb_counts: dict[str, int] = {}
    for line in journal_lines:
        rec = json.loads(line)
        verb_counts[rec["replay_core"]["verb"]] = (
            verb_counts.get(rec["replay_core"]["verb"], 0) + 1
        )
        cf = rec["observability"]["coeffs_after"]
        for v, axes in cf.items():
            if any(abs(float(x)) > 1e-9 for x in axes.values()):
                seen_F.add(v)

    return {
        "batches": session["last_batch_id"],
        "stop_reason": session["stop_reason"][:40],
        "total_kB": total / 1024,
        "delta_pp": deltas,
        "max_pp": max(abs(d) for d in deltas.values()),
        "f_coverage": len(seen_F),
        "verbs": verb_counts,
    }


def main() -> None:
    root = Path(sys.argv[1] if len(sys.argv) > 1 else "ablation-runs")
    runs = sorted(p for p in root.iterdir() if p.is_dir())
    if not runs:
        sys.exit(f"no ablations found under {root}")

    rows = []
    for r in runs:
        try:
            s = summarize_one(r / "state")
            s["name"] = r.name
            rows.append(s)
        except Exception as exc:
            print(f"  {r.name}: ERROR — {exc}")

    print()
    print(
        f"{'name':<14} {'batches':>7} {'stop':<24} {'total_kB':>10} "
        f"{'Δacc':>7} {'Δsto':>7} {'Δcod':>7} {'max_pp':>7} {'F_cov':>6} {'verbs':>5}"
    )
    print("-" * 105)
    for r in rows:
        if r.get("empty"):
            print(f"{r.get('name', '?'):<14}  EMPTY")
            continue
        print(
            f"{r['name']:<14} {r['batches']:>7} {r['stop_reason']:<24} "
            f"{r['total_kB']:>10.1f} "
            f"{r['delta_pp']['accounts']:>+7.2f} "
            f"{r['delta_pp']['storage']:>+7.2f} "
            f"{r['delta_pp']['code']:>+7.2f} "
            f"{r['max_pp']:>7.2f} "
            f"{r['f_coverage']:>6} "
            f"{len(r['verbs']):>5}"
        )

    print()
    print("verb selection per run:")
    for r in rows:
        if r.get("empty"):
            continue
        verbs = sorted(r["verbs"].items(), key=lambda kv: -kv[1])
        s = ", ".join(f"{v}={n}" for v, n in verbs[:5])
        print(f"  {r['name']:<14} {s}")


if __name__ == "__main__":
    main()
