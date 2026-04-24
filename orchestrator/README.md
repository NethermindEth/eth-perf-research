# orchestrator

Python package that drives a Nethermind node's state composition toward a mainnet-faithful target
via closed-loop feedback.

One process, one loop, two artifacts per run:

- `state/orchestrator.journal.jsonl` — crash-resilient JSONL journal (chain-hashed `replay_core`).
- `state/payloads.rlp` — append-only `ExecutionPayloadV3` stream for cross-client reproduction.

See [`final-design-v3.md`](../.omc/co-design/simplify-20260422/final-design-v3.md) for the
normative specification.

## Quick start

```bash
uv sync
uv run orchestrator --target-yaml target.yaml --state-dir ./state --rpc-url http://localhost:8545
```

Resume mode is auto-detected: if `state/orchestrator.journal.jsonl` exists, the orchestrator
verifies head + composition hash + chain hash and continues. Mismatch → refuses with a diagnostic.

Replay a completed run:

```bash
uv run orchestrator --replay ./state/orchestrator.journal.jsonl --rpc-url http://localhost:8545
```

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
