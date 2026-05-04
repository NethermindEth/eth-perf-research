# orchestrator

Python package that drives a Nethermind node's state composition toward a mainnet-faithful target
via closed-loop feedback.

One process, one loop, two artifacts per run:

- `state/orchestrator.journal.jsonl` — crash-resilient JSONL journal (chain-hashed `replay_core`).
- `state/payloads.rlp` — append-only `ExecutionPayloadV3` stream for cross-client reproduction.

See [`final-design-v3.md`](../.omc/co-design/simplify-20260422/final-design-v3.md) for the
normative specification.

## Quick start

The canonical invocation is via Docker Compose, which injects all required env vars:

```bash
docker compose up orchestrator
```

For direct invocation (see `uv run orchestrator --help` for the full list of required flags):

```bash
uv sync
uv run orchestrator \
  --rpc-url http://localhost:8545 \
  --state-dir ./state \
  --target-yaml target.yaml \
  --genesis-sha256 <hex-sha256-of-genesis.json> \
  --plugin-git-sha <git-sha> \
  --nethermind-commit-sha <git-sha> \
  --dotnet-runtime-major 8
```

Resume mode is auto-detected: if `state/orchestrator.journal.jsonl` exists, the orchestrator
verifies head + composition hash + chain hash and continues. Mismatch → refuses with a diagnostic.

Replay a completed run:

```bash
uv run orchestrator --replay ./state/orchestrator.journal.jsonl --rpc-url http://localhost:8545
```

## Cross-client verification

`payloads.rlp` is an append-only stream of canonical `ExecutionPayloadV3` blocks. Any
EL client that speaks the Engine API can replay the stream and the resulting `stateRoot`
on the final block must match the `final_state_root` recorded in `run-manifest.json`.

To verify a completed run on Geth (or any other EL):

1. Boot a fresh archive node against the lab genesis (`state-geth/genesis.json`).
2. For each payload in `payloads.rlp`, send `engine_newPayloadV4` followed by
   `engine_forkchoiceUpdatedV3` pinning the new head.
3. Read `eth_getBlockByNumber("latest")` and compare its `stateRoot` with
   `manifest.final_state_root`.

Bit-exact match on every payload is the design's normative claim — the journal +
payload stream is a deterministic byte-level reproduction recipe regardless of which
EL produced it.

A reference replay harness for Geth lives outside the PR scope; see the local demo
under `eth-perf-research/dagu/` for an executable example.

## Docker

```bash
docker compose up -d       # starts Nethermind + orchestrator with an ephemeral JWT
docker compose down        # shreds JWT, stops containers
```

## Tests

```bash
uv sync --all-extras
uv run pytest
```
