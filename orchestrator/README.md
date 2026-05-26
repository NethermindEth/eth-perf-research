# orchestrator (Go)

This repo contains two co-located Go binaries that together drive the v19
state-composition bloating loop against Nethermind:

| Binary | Role |
|--------|------|
| `orchestrator` (`./cmd/orchestrator`) | Bloating feedback-loop driver. Closed-loop control over `testing_commitBlockV1`, packs EELS-based spamoor scenarios into blocks until the target state composition is reached. |
| `sidecar` (`./cmd/sidecar`) | v19 external state-composition tracker. Owns every responsibility the in-process `Nethermind.StateComposition` plugin used to carry: scans the FlatDb in RocksDB secondary mode, maintains an in-memory tier-1 tracker, applies `BlockDiffs`, and serves `statecomp_*` JSON-RPC. |

The orchestrator polls the sidecar's `statecomp_lite` endpoint (same shape as
`Nethermind.StateComposition.Data.StateCompositionLite`) for the control
signal; the sidecar runs alongside Nethermind, sharing FlatDb in
RocksDB-secondary mode.

## Architecture

```
┌─ orchestrator (Go) ──────────────────────────────────────────────┐
│                                                                  │
│  controller ─▶ facade.Dispatcher ─▶ signer (in-process)          │
│      ▲              │                    │                       │
│      │              ▼                    ▼                       │
│      │     ┌─ verbs ────────┐    testing_commitBlockV1           │
│      │     │ native Go tx   │           │                        │
│      │     │ builders       │           ▼                        │
│      │     └────────────────┘      Nethermind                    │
│      │                                  │                        │
│      └────────────── sensor (statecomp_lite) ──◀── sidecar (Go)  │
│                                                       │          │
│  journal.bin (protobuf binlog + BLAKE3 chain hash)    ▼          │
│  payloads.rlp (4-B-LE-prefixed ExecutionPayloadV3 RLP)  Nethermind│
│  run-manifest.json                                      FlatDb   │
└──────────────────────────────────────────────────────────────────┘
```

## Build

Orchestrator:

```
make build       # local binary at bin/orchestrator
make linux       # static Linux/amd64 binary at bin/orchestrator.linux-amd64
make test        # all tests, race detector clean
```

Sidecar (local dev defaults to the `nogrocksdb` stub so Macs without
librocksdb-dev can run the unit + integration tests):

```
make build-sidecar          # bin/sidecar (stub backend, no CGO)
make build-sidecar-linux    # bin/sidecar-linux-amd64 (stub backend; production uses Docker)
make build-sidecar-cgo      # bin/sidecar-cgo (full grocksdb; requires librocksdb-dev)
make test-sidecar           # race-clean stub-backed test run (28 tests)
make docker-sidecar         # Debian-slim image with librocksdb9 + the binary
```

`make all` builds both.

## Run

Orchestrator:

```
ORCH_DEPLOY_PRIVATE_KEY=0x... \
bin/orchestrator run \
  --rpc-url http://nethermind:8545 \
  --state-dir /var/orch/state \
  --target-yaml /etc/orch/target.yaml \
  --genesis-sha256 <hex>
```

Sidecar (production mode, alongside Nethermind):

```
bin/sidecar \
  --mode=all \
  --db /mnt/nm/flat \
  --code-db /mnt/nm/code \
  --snapshot-dir /var/lib/sidecar \
  --dedup-dir /mnt/scratch/codehash-dedup    # roomy volume; ~700 GB at 10× state
```

## Subcommands

### orchestrator

- `run` — closed-loop controller against Nethermind.
- `replay --payloads payloads.rlp --manifest run-manifest.json --rpc-url <engine-api>` — cross-client replay via Engine API.
- `verify-journal --path journal.bin` — re-derive the chain hash and print the final hash + record count.

### sidecar

One binary, four modes:

| Mode | Purpose |
|------|---------|
| `bootstrap` | One-shot full scan of `/flat` → `snapshot.bin`, exit. |
| `tail` | Long-running; restores the last snapshot, applies `BlockDiffs`, periodically re-snapshots. |
| `serve` | Read-only JSON-RPC server over an existing snapshot. |
| `all` | Bootstrap-if-needed + tail + serve. Production default. |

Three RPC methods, listening on `:9001` by default:

- `statecomp_lite` — tier-1 counters + lag (matches `StateCompositionLite`).
- `statecomp_get` — full report incl. tier-2 byte/depth counters and histogram.
- `statecomp_health` — sidecar-specific liveness: lag, snapshot age, RSS.

## Layout

```
cmd/
  orchestrator/         cobra entry + subcommand wiring for the bloating driver
  sidecar/              entry point + mode dispatch for the state-composition tracker
  heal-payloads/        replay-side payload healer
internal/                orchestrator-only packages (lifecycle, controller, verbs, …)
pkg/sidecar/            sidecar packages — co-located so future code can share types
  config/               YAML/CLI configuration
  db/                   RocksDB secondary-mode wrappers (+ stub for `-tags nogrocksdb`)
  rlp/                  AccountDecoder + BlockDiffRecord encode/decode
  tracker/              code-refcount + slot-count maps, 32 hash-routed shards
  scanner/              bootstrap-mode scanner + streaming codehash dedup (RocksDB-backed,
                        constant-memory replacement for the v18-experiments OOM fix)
  tailer/               BlockDiffs CF reader, applies records to the tracker
  rpc/                  JSON-RPC HTTP server, schema-compatible with the C# plugin
  snapshot/             zstd blob writer + reader, two-file atomic publish
  state/                cross-module phase enum
test/
  sidecar/              end-to-end integration + memory regression tests
docs/
  v19-sidecar-architecture.md   full design rationale
proto/                  .proto sources
Dockerfile              orchestrator production image (distroless)
Dockerfile.sidecar      sidecar production image (debian-slim + librocksdb9)
```

## Sidecar memory model

The streaming codehash dedup keeps the bootstrap scanner's RSS bounded by a
temporary RocksDB regardless of unique-codehash cardinality (replaces the
v18.x bucket-sort / in-memory loadUniqueSorted that OOMed at bloatnet scale).

| Component | Budget |
|-----------|--------|
| RocksDB FlatDb block cache | 128 MB |
| Dedup RocksDB write buffer | 1 GB (256 MB × 4) |
| Dedup block cache | 128 MB |
| Tracker hot tier (32 shards) | 300 MB |
| Mmap'd spill (kernel-paged) | 0 RAM |
| Tailer ring buffer | 64 MB |
| RPC server | 16 MB |
| Go runtime overhead | ~50 MB |
| **Total ceiling** | **~1.7 GB** |

Disk ceiling: the temp dedup DB ≈ 1.5× the codehash spill (LSM overhead).
At 10× state that's ~700 GB — point `--dedup-dir` at a roomy volume.

## See also

- The v19 architecture doc at `docs/v19-sidecar-architecture.md`
- The planning document at `~/.claude/plans/let-s-create-a-structure-serialized-popcorn.md`
- The legacy Python orchestrator at `../orchestrator-py/` (deleted on cutover)
