# PR #77 simplification analysis

Deep simplification research on the Go orchestrator (PR NethermindEth/eth-perf-research#77,
+27,331 LOC, 157 files). Goal: remove a lot of code without losing critical behavior.
Findings from a 5-track parallel analysis (verbs, sidecar, controller+lifecycle,
io+rpc+cmd, external best-practice web research).

**Headline:** ~1,500–2,000 LOC removable at low/medium risk, plus ~300–400 behind two
judgment calls. The architecture is sound; the fat is duplication, dead/test-only code,
hand-rolled encoders go-ethereum already provides, and a config-overlay pattern a
one-liner replaces.

## Tier 1 — safe wins (do first)

1. **Delete `cmd/heal-payloads/block.go` (~263 LOC, low risk).** Route the `heal` command
   through the `debug_getRawBlock` fetcher `cohere` already uses in production (#149).
   Removes the hand-rolled `wireBlock`/`wireWithdrawal`/hex-parser machinery. Verify:
   `heal` output frames byte-identical vs current binary on a small payloads.rlp.
2. **Config: `yaml.Unmarshal` onto a defaulted struct (~150–170 LOC, low-med).** Drop the
   `controlYAML`/`costYAML`/`runYAML` pointer structs, `applyYAML`, and `setF/setI/...`
   helpers. `sigs.k8s.io/yaml` (already a dep) leaves absent struct fields untouched when
   unmarshaling onto `Defaults()`. Keep `validate()`. The existing config_test overlay
   tests are the equivalence gate. Also delete the stale "environment-variable overrides"
   precedence claim in config.go/load.go docs — Load never reads env.
3. **Table-driven verb builder (~185 LOC, low).** Collapse 5–7 near-identical call-verb
   files (calltx, storagespam, erc20_bloater, storagerefundtx, gasburnertx, ...) into one
   `callSpec` table + generic builder. eoatx/deploytx stay bespoke. Gate: byte-exact
   `TestVerbsGolden`.
4. **Merge `heal` into `cohere` (~216 LOC, med).** `cohere` is a strict superset (gap-fill +
   tail-fill + reorg relink + hash-linkage verify). Drop heal.go + the `heal` CLI mode;
   default `--from` to the first frame in the input.
5. **Drop dead/undeployable verbs (~190 LOC, low-med).** `storagerefundtx` is not in the
   live ORCH_VERBS set; `uniswap_swaps` (the heaviest verb, 141 LOC) targets a router that
   is deliberately never in `contractCatalog`, so its calldata is never sent on bloatnet.
   Confirm scope, then build-tag-gate (reachable via explicit ORCH_VERBS) or delete.
6. **`engine.BlockToExecutableData` (~30–40 LOC, low).** Replaces the two hand-rolled
   `blockToPayload`/`wireBlockToPayload` converters — go-ethereum's canonical inverse of
   the `ExecutableDataToBlock` already used in replay.go.
7. **Sidecar dead-path deletes (~130 LOC, low).** `rlp.fromWire` + `DecodeBlockDiffStream`
   (zero callers), tracker `ApplyCodeChange`/`applyCodeRemove`/`applyCodeAdd` (test-only;
   retarget the 4 tests to the live `ApplyBlockDiff` batched path), dedup the two `hex32`
   copies, factor a shared `iterate32ByteKeys`.
8. **Stdlib/generics sweep (~60 LOC, low).** `min`/`max` builtins replace
   `clampMin`/`clampMax`; one `finite()` helper folds ~6 duplicated NaN/Inf checks;
   `slices`/`maps`; delete the dead `LookaheadDepth` config knob (never read — runLoop
   hardcodes depth-1).

## Tier 2 — high value, needs a decision/benchmark

- **Collapse the depth-1 pipeline → sequential loop (~230–300 LOC, med-high).** The loop is
  already sensor-paced (commitBatch blocks on the trie-diff fold); the pipeline only
  overlaps build+sign (cheap CPU) with the ~3 s sensor wait. Deleting it also removes ALL
  goroutine/`pickApplyMu`/atomic-float-snapshot race surface (a net correctness win) and
  the concurrent_snapshot test. BUT commit 04da4eb re-added the pipeline deliberately for
  throughput — re-benchmark batches/min on the live rig before merging. Largest single cut
  if the regression is negligible.
- **Journal: drop the BLAKE3 chain-hash (~60–80 LOC + the lukechampine.com/blake3 dep, med).**
  It gates no control flow today (resume reads only the block number; recovery uses the
  pending-batch gob). Whole-file `JournalSHA256` covers the realistic corruption mode.
  Tamper-evidence is gold-plating for an internal artifact — needs lead sign-off.
- **Minimize crash-resume (~80–120 LOC, med).** Reduce to nonce-rederive (load-bearing,
  stays) + cold-start from reference-F; drop the per-cell coefficient hydration whose
  elaborate corruption guard defends an already-fixed divergence bug. Trade-off: early-batch
  re-exploration after a restart.

## Do NOT touch (load-bearing — critical loss)

- Sidecar **snapshot codec** (fixed-width zstd streaming → no-rescan/no-OOM at 25M entries).
- The **3 CodeStore variants + MultiGet/BatchPut** (disk path mandatory at 1.4B codehashes;
  the interface polymorphism is what lets tests run on the mem store).
- **`decodeUint64Lenient`** + the `Split`-based forward-compat RLP decoder (guard against
  silently dropping byte deltas in [1,127] and drifting the tracker until a full rescan).
- **32-way sharding** (O(32) snapshot/aggregate vs O(25M)); the tracker uses a plain Go map,
  not a custom hash map.
- **nogrocksdb build-tag stubs** (the only thing that makes the package buildable/testable
  without cgo+rocksdb).
- Don't adopt koanf/viper or a bandit library — plain `yaml.Unmarshal`-onto-defaults and the
  bespoke ε-greedy are the right minimal call here. DO lean harder on go-ethereum
  `beacon/engine` + `ethclient`/`rpc.Client` (correctness + LOC).

## Stale-comment corrections (fix regardless — the PR describes machinery not in the code)

- No "projected-gradient / simplex / R1 entropy floor / R2 anti-windup" exists. `Pick` is a
  greedy argmax + ε-greedy. Delete those comments (state.go/pick.go).
- Config docs claim an env-override precedence layer; Load never reads env.
- payloads.go package doc: the length prefix is 4-byte BIG-endian (one comment says BE,
  package doc omits it).

## External best-practice references (web research)

- Config: `sigs.k8s.io/yaml` unmarshal-onto-defaults (minimal) or koanf (if env+flags
  precedence merging is wanted; avoid viper — key-lowercasing, dep bloat). Pair with
  go-playground/validator if richer validation is needed.
- Engine API: go-ethereum `beacon/engine` (ExecutableData, ForkchoiceStateV1, PayloadStatusV1,
  Block↔Payload helpers) + `rpc.Client.CallContext` + `ethclient`/`gethclient`.
- Bandit/control: no mature general Go controls lib; bespoke is the norm. stitchfix/mab is
  selection-only and stale (v0.1.0, 2021) — not worth a dep here.
- Snapshot persistence: gob (Go-only, schema-robust) or pebble `Checkpoint()` if incremental;
  BLAKE3 chain-hash is standard only for adversarial logs.
- Generics/stdlib: `slices`/`maps`, range-over-int, range-over-func iterators collapse most
  hand-rolled helpers. Keep config as a struct — functional options are the wrong tool for
  startup config.
