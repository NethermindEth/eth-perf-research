# v19: External Sidecar Architecture for State Composition

## Context

We spent 24h on v17/v18.x iterations of the in-process Nethermind StateComposition plugin. Every variant — channel-bounded tracker, fill_cache=false, lower MemoryHint, GCConserveMemory=9, parallelism 2/4/8 — hit the same structural memory wall at account-nodes ~1.7B on the bloatnet (1.87B accounts, 1.4 TB state). Cause: the .NET runtime + NM's archive-mode caches + shared block cache + per-worker accumulators compound to consume 78+ GB regardless of how we configure them.

Prototype A (external Go scanner) demonstrated the alternative: **154× lower memory (~512 MB peak), 5× higher throughput (~1.95M nodes/s)**. Scan complete in ~70 min vs in-process projected 5+ hours.

This document specifies the production architecture for moving ALL StateComposition functionality out of NM into a sidecar process.

## Design goals

1. **100% accuracy** on all metrics (codeBytesTotal, accountsTotal, storageSlotsTotal, contractsTotal, slot histogram, trie-node counts).
2. **Bounded memory** under 1 GB at 100× mainnet scale.
3. **No NM-side OOM risk** — sidecar runs in its own process tree.
4. **Live tracking** — orchestrator-visible metrics lag chain head by ≤5 s in steady state.
5. **Vanilla NM** — minimal residual plugin in NM (≤200 LOC), no tracker, no metrics, no RPC.
6. **Crash-safe** — sidecar restart in ≤60 s from durable state.
7. **Reusable** — same sidecar serves any future NM bloatnet, or with thin adapters any RocksDB-backed EL client.

## Component model

```
                                                                              
   +-------------------+         +----------------------+         +----------+
   |  Nethermind       |         |  v19 Sidecar         |         |  Orch    |
   |  (vanilla + tiny  |         |  (Go process)        |         |  (no     |
   |  diffs-writer     |         |                      |         |  change) |
   |  plugin)          |         |                      |         |          |
   +-------------------+         +----------------------+         +----------+
            |                              |                            |
            | imports blocks               | reads RocksDB              | RPC poll
            v                              v secondary mode             |
   +------------------------------+ +-------------------+               |
   |  /flat (FlatDb RocksDB)      | |  Tracker state    |               |
   |    StateNodes  (trie nodes)  | |  (in-memory +     |               |
   |    StorageNodes (trie nodes) | |   periodic blob)  |               |
   |    BlockDiffs (NEW)          | +-------------------+               |
   +------------------------------+           |                          |
            ^                                 v                          |
            | writes per-block         +--------------+                  |
            | (CodeHashChange[]+       |  JSON-RPC    | <----------------+
            |  SlotCountChange[])       |  :9001       |
            |                          +--------------+

```

## NM-side residual (Phase 2)

A minimal `Nethermind.StateDiffsWriter` plugin replaces `Nethermind.StateComposition` entirely. Responsibilities:

- Subscribe to `IBlockProcessor.BlockProcessed`.
- For each block: run the existing `TrieDiffWalker` against (parent_root, new_root).
- Encode the resulting `CodeHashChange[]` and `SlotCountChange[]` to RLP (use existing decoders).
- Persist to a new `BlockDiffs` CF, key = `BE(block_number)` (8 bytes), value = RLP-encoded record.
- That's it. No tracker, no metrics, no RPC, no bootstrap.

Estimated size: ~200 LOC + tests. Compare to current SC plugin's ~6,000 LOC.

## Sidecar binary (Phase 1)

### Modes (selectable via `--mode`)

| Mode | Purpose | Lifecycle |
|---|---|---|
| `bootstrap` | One-shot full scan | Reads FlatDb, emits snapshot.json, exits. Used to prime a fresh sidecar. |
| `tail` | Long-running incremental tracker | Loads last snapshot, tails BlockDiffs CF, updates tracker, periodically re-snapshots. |
| `serve` | JSON-RPC frontend | HTTP server exposing `statecomp_*` methods. Reads tracker from shared in-memory state. |
| `all` (default) | Bootstrap-if-needed + tail + serve | Production mode. Single process. |

### State machine

```
   STARTING
       |
       v
   +------------------+
   | Snapshot exists?  |
   +------------------+
       | yes                | no
       v                    v
   LOAD_SNAPSHOT        BOOTSTRAP_SCAN
       |                    |
       v                    v
   CATCHING_UP <------------+
       |
       | tail from last_block to chain_head
       v
   LIVE_TAILING
       |
       | periodic snapshot every 60s
       | crash → restart → STARTING
```

### Internal modules

- `pkg/db/` — RocksDB secondary-mode wrappers, CF iterators with `fill_cache=false` + readahead.
- `pkg/rlp/` — RLP encode/decode (use `github.com/ethereum/go-ethereum/rlp`).
- `pkg/tracker/` — codehash refcount + slot count maps, in-memory backed by mmap'd sorted-array files for spill.
- `pkg/scanner/` — bootstrap mode (Prototype A).
- `pkg/tailer/` — reads BlockDiffs CF sequentially, applies to tracker.
- `pkg/rpc/` — minimal JSON-RPC server (use `gorilla/rpc/json` or net/rpc).
- `pkg/snapshot/` — binary blob writer/reader for the tracker (zstd compressed).
- `cmd/sidecar/` — entry point.

### Memory budget

| Component | Budget |
|---|---|
| RocksDB block cache | 128 MB |
| Tracker hot tier | 300 MB (200M codehashes × ~16 B/entry packed) |
| Tracker spill (mmap'd) | 0 RAM (kernel pages on demand) |
| Tailer ring buffer | 64 MB |
| RPC server | 16 MB |
| Go runtime overhead | ~50 MB |
| **Total** | **~560 MB hard ceiling** |

At 100× mainnet scale: increase tracker spill (disk, not RAM). RAM ceiling unchanged.

### JSON-RPC compatibility

Sidecar exposes the same methods as the current `Statecomp` RPC module:

- `statecomp_lite` → returns the same `CumulativeTrieStats` JSON shape.
- `statecomp_get` → full structure with histograms.
- `statecomp_topN` → top-N contracts (sourced from histogram).
- `statecomp_health` → NEW: sidecar's own lag-from-head + last-snapshot-age + memory usage.

Listens on `0.0.0.0:9001`. Orchestrator config flips `--sensor-rpc-url=http://nethermind-bloatnet:8545` → `http://nethermind-bloatnet-sidecar:9001`.

## Phases

### Phase 0: Verify bootstrap (in flight, ~25 min remaining)

The current v19 scanner finishes scanning the bloatnet's FlatDb. Output: `snapshot.json` with all v18-equivalent metrics. **Gating criterion: JSON values are plausible (accounts 1.5-2.0B, codeBytesTotal > 0).**

### Phase 1: Production sidecar (1-2 days)

Extend Prototype A's scanner.go into a long-running daemon:

1. Refactor `scanner.go` → modular `cmd/sidecar/` with the modules listed above.
2. Add `tail` mode: read BlockDiffs CF; decode CodeHashChange/SlotCountChange; apply to tracker.
3. Add `serve` mode: JSON-RPC HTTP server.
4. Add `snapshot` writer: tracker → zstd blob every 60s; load on startup.
5. Add `--mode=all` orchestration that runs bootstrap → tail → serve.
6. Cross-compile for linux/amd64; ship as static binary.
7. Docker image: alpine base + binary (~50 MB image).
8. Test plan: synthetic block stream, validate metric drift = 0.

### Phase 2: NM diffs-writer plugin (1 day)

Build `Nethermind.StateDiffsWriter` plugin (replaces `Nethermind.StateComposition` over time):

1. New project `src/Nethermind/Nethermind.StateDiffsWriter/`.
2. Hook `IBlockProcessor.BlockProcessed`.
3. Compute diff via existing `TrieDiffWalker`.
4. RLP-encode `(CodeHashChange[], SlotCountChange[])`.
5. Persist to new `BlockDiffs` CF (8-byte BE block number key).
6. Add `Pruning.BlockDiffsKeepBlocks` config (default: keep last 1M blocks, auto-prune older).
7. Tests: 100 synthetic blocks, RLP roundtrip, prune correctness.
8. Build into NM image alongside the current SC plugin (both active during Phase 3).

### Phase 3: Parallel validation (1 week soak)

Run both old SC plugin AND new sidecar simultaneously on the bloatnet:

1. Deploy NM with both plugins (current SC + new diffs-writer).
2. Deploy sidecar pointing at the same FlatDb.
3. Every minute: poll both `statecomp_lite` endpoints, diff values, log to `validation-trace.jsonl`.
4. Alert if any metric drifts >0 (bit-equal accuracy required).
5. Run for 7 days under continuous bloating.

### Phase 4: Cutover (1 day)

1. Remove `Nethermind.StateComposition` from NM image build.
2. Update orchestrator config to point at sidecar's RPC port.
3. Tag a final release of the consolidated WIP branch.

## Failure modes + mitigations

| Failure | Detection | Mitigation |
|---|---|---|
| Sidecar crashes mid-tail | `statecomp_health` returns 500 | systemd restart, resume from last snapshot |
| Sidecar lags chain head >60s | `health.lag_blocks > N` | Orchestrator backs off bloating; alert |
| NM crashes (separate concern) | `docker inspect` | Existing watchdog handles this; sidecar continues serving stale-but-consistent state |
| FlatDb corruption | RocksDB returns ErrCorruption | Sidecar logs + exits; restart triggers fresh bootstrap |
| BlockDiffs CF lag (NM imports blocks faster than diffs-writer keeps up) | NM-side metric `block_diffs.queue_depth` | Throttle block processing (back-pressure); should never trip in practice |
| Bootstrap takes longer than chain advance | block_number in snapshot < chain head at finish | Sidecar tails forward from snapshot block automatically |
| Snapshot file corruption | zstd checksum fails | Fall back to bootstrap (~70 min penalty); alert |
| Disk full (tracker spill files) | Sidecar exits with EIO | Operator increases /var/lib/sidecar size; restart |

## Acceptance criteria

1. **Memory ceiling**: sidecar RSS ≤1 GB across a 7-day soak at 5× state.
2. **Accuracy**: bit-equal metric outputs vs current SC plugin across 7 days.
3. **Lag**: 99th percentile chain-head-to-sidecar lag ≤5 s under continuous bloating.
4. **Bootstrap time**: ≤90 min for the bloatnet's 1.4 TB FlatDb.
5. **Crash recovery**: from cold restart to LIVE_TAILING in ≤60 s.
6. **Operational simplicity**: single docker-compose service line; no manual coordination with NM.

## Out of scope (this iteration)

- Cross-client adapters (Geth/Reth/Erigon) — same Go binary structure could support them, but actual implementation deferred.
- Verkle-state metrics — separate ticket.
- Multi-machine deployment (sidecar on separate VM from NM via NFS) — works in principle, not tested.
- HA / leader election among multiple sidecars — single-instance is sufficient for bloatnet.
