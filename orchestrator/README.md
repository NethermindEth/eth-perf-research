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
