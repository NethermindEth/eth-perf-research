# Manual Review Guide — `orchestrator` (branch `adaptive-orch-v5`)

Read in this order. It is bottom-up: each package is reviewed **before** the
packages that import it, so by the time you reach the glue (`lifecycle`) and the
entrypoints you already understand every type they touch. Two binaries live
here — review the **orchestrator** stack fully first (Stages 1–7), then the
**sidecar** stack (Stage 8), then the standalone CLIs (Stage 9), then tests
(Stage 10).

Confirmed dependency facts that fix the order:
- `controller` imports only `config`, `mathx`, `referencef` (pure foundations).
- `facade` imports `controller`, `signer`, `verbs`, `orchpb`.
- `lifecycle` imports nearly everything — review it last among libraries.
- `sidecar/tracker` imports `sidecar/rlp` — rlp before tracker.

---

## Stage 0 — Orientation (no code yet)
1. `README.md` — the architecture box and the two-binary split. Anchor everything to this diagram.
2. `docs/v19-sidecar-architecture.md` — why the in-process NM plugin became an external Go sidecar.
3. `go.mod` — dependency surface, Go version (1.26).
4. `Makefile` + `Dockerfile` + `Dockerfile.sidecar` — how each binary is built and what build tags exist (`nogrocksdb` stub matters for the sidecar).

## Stage 1 — Contracts & foundations (no internal deps)
Review these first; everything downstream is typed against them.
1. `proto/journal.proto`, `proto/txsigner.proto` — the wire/persisted schemas.
2. `internal/orchpb/` — generated/host protobuf types for the journal (1.1k LOC; skim generated code, read hand-written helpers).
3. `internal/config/config.go` then `internal/config/load.go` — every tunable (epsilon, alphas, gates, target wiring). **This is the single most important file for understanding behavior.** Note defaults and validation.
4. `internal/target/` — target.yaml model: shares, total_bytes, `reachedTarget` thresholds. Verify the stop condition math.
5. `internal/mathx/` — numeric primitives (clamping, vectors). Pure, testable.
6. `internal/referencef/` — reference-F matrix (verb→axis growth priors). Feeds the controller.

## Stage 2 — The control core (`internal/controller/`)
This is the heart of the adaptive loop and where the historical bugs lived
(calltx fixation, tolerance compounding). Read in this sub-order:
1. `state.go` — controller state shape and lifecycle.
2. `verbstats.go` — per-verb running stats (the alphas / F-rows).
3. `baseline.go` — baseline/reference handling.
4. `pick.go` — **the ε-greedy verb selection.** Trace: F-row → gradient → xProj weight → maxNTxs cap → post-floor selectFrom → which step can zero a verb. This is the function that must steer toward the under-target axis.
5. `apply.go` — applying observed deltas back into state (closes the loop).
6. `instability.go` — instability/guard logic.
Then the tests as a correctness spec: `pick_test.go`, `controller_test.go`, `concurrent_snapshot_test.go`.

## Stage 3 — The action path (build & submit blocks)
1. `internal/verbs/` — 19 files, native Go tx builders (eoatx, deploytx, factorydeploytx, gasburnertx, calltx, …). Review the registry/dispatch file first, then one builder per family. Check gas accounting and what each verb actually grows (accounts vs code).
2. `internal/signer/` — in-process tx signing (+ `txsigner.proto`).
3. `internal/rpc/` — JSON-RPC client to Nethermind (`testing_commitBlockV1`, engine calls, JWT). Check timeout/retry and error surfacing.
4. `internal/facade/` — `Dispatcher`: ties controller picks → verbs → signer → rpc. The seam where a pick becomes a committed block.

## Stage 4 — The feedback path
1. `internal/sensor/` — polls the sidecar's `statecomp_lite`, produces the control signal. Check the staleness/lag handling and `waitSensorForBlock` semantics.

## Stage 5 — Persistence & safety
1. `internal/journal/` — protobuf binlog + BLAKE3 chain hash. Verify append/replay integrity and the hash chaining.
2. `internal/payloads/` — `payloads.rlp` reader/writer (4-byte LE length prefix + ExecutionPayloadV3 RLP). The format the verify/heal/replay CLIs depend on.
3. `internal/manifest/` — run-manifest.json.
4. `internal/lock/` — single-instance lock.
5. `internal/metrics/` — Prometheus exposition (the `:9101` verb metrics you monitor).

## Stage 6 — Orchestration glue (`internal/lifecycle/`)
Largest package; imports all of the above. Read in execution order, not alphabetical:
1. `run.go` — top-level run loop wiring (constructs deps, owns `lastBlockTS atomic.Uint64`).
2. `startup.go` — startup sequencing.
3. `bootstrap.go` — empty-journal / fresh-head bootstrap (and `nextBlockTS` strictly-increasing timestamp helper).
4. `registry.go` — component registry.
5. `eligibility.go` — verb eligibility gating.
6. `feepolicy.go` — fee/basefee policy for packed txs.
7. `timing.go`, `helpers.go` — block timing (`nextBlockTS`), misc.
8. `batch.go` — the per-batch commit loop (also uses `nextBlockTS`).
9. `pipeline.go` — pipelined commit throughput.
10. `reconnect.go` — NM reconnect/resilience.
Tests as the behavioral spec: `lifecycle_test.go`, `run_test.go`, `integration_test.go`, `blockts_test.go`, `eligibility_test.go`, `feepolicy_test.go`, `reconnect_test.go`, `registry_test.go`.

## Stage 7 — Orchestrator entrypoint
1. `cmd/orchestrator/` — cobra wiring: `run`, `replay`, `verify-journal` subcommands. Confirm flags map cleanly onto `internal/config`.
2. `internal/replay/` — payload replay transform (timestamp relink + BlockHash recompute). Reviewed here because `cmd/orchestrator` exposes it and it shares the payload format from Stage 5.

---

## Stage 8 — Sidecar stack (`pkg/sidecar/` + `cmd/sidecar/`)
Separate binary; bottom-up again.
1. `pkg/sidecar/config/` — sidecar tunables (datadir, codestore dir, ports).
2. `pkg/sidecar/db/` — RocksDB secondary-mode access (note the `nogrocksdb` build tag stub).
3. `pkg/sidecar/rlp/` — account/trie RLP decoding (tracker depends on this).
4. `pkg/sidecar/scanner/` — FlatDb bootstrap scan (the long initial pass).
5. `pkg/sidecar/tracker/` — 1.9k LOC, the in-memory tier-1 tracker + byte counters. The correctness center of the sidecar; read carefully alongside its tests.
6. `pkg/sidecar/tailer/` — live-tail of `BlockDiffs` after bootstrap.
7. `pkg/sidecar/snapshot/` — snapshot.bin serialize/restore (the no-rescan-on-restart path).
8. `pkg/sidecar/state/` — aggregate state shape.
9. `pkg/sidecar/rpc/` — serves `statecomp_*` JSON-RPC (the orchestrator's sensor reads this).
10. `pkg/sidecar/promexport/` — Prometheus `:9099` metrics (account/storage/code bytes you monitor).
11. `cmd/sidecar/` — entrypoint wiring all of the above.

## Stage 9 — Standalone tooling CLIs
Independent of the live loop; safe to review last. They share `internal/payloads`.
1. `cmd/verify-payloads/` — gap / duplicate / non-monotonic / **ts-non-increasing** checks (read-only).
2. `cmd/heal-payloads/` — 1.4k LOC repair tool. Highest-risk standalone code (it writes); review its safety/backup behavior closely.
3. `cmd/statescan/` — node-type + depth state scan.

## Stage 10 — Cross-cutting test sweep
1. `test/` — top-level integration tests (595 LOC).
2. Re-skim every `*_test.go` flagged above as the executable spec for each unit.
3. Confirm `make test` is race-clean and that the controller/lifecycle timestamp tests (`blockts_test.go`) actually assert strict monotonicity.

---

### Review checklist to carry through every stage
- **Immutability**: new objects over in-place mutation (esp. controller state snapshots).
- **Error handling**: no swallowed errors; NM/RPC failures surface and are counted.
- **Boundaries**: config validated at load; external RPC/RLP data never trusted raw.
- **Concurrency**: `lastBlockTS` CAS, controller snapshot under concurrency, sidecar tailer vs. snapshot writer.
- **The two known traps**: (1) verb selection must not fixate on a near-no-op verb and starve the under-target axis; (2) block timestamps must be strictly increasing (RLP/consensus validity).
