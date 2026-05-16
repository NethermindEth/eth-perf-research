# orchestrator (Go)

Bloating feedback-loop orchestrator for Nethermind. Drives `testing_commitBlockV1` in a closed control loop, packing EELS-based spamoor scenarios into blocks until the target state composition is reached.

This is a Go rewrite of the Python orchestrator (now at `../orchestrator-py/`). The EELS verb builders have been ported to native in-process Go (`internal/verbs/`); transaction building is a plain loop with no subprocesses.

## Architecture

```
┌─ Go orchestrator ──────────────────────────────────────────────┐
│                                                                 │
│  controller ─▶ facade.Dispatcher ─▶ signer (in-process)         │
│      ▲              │                    │                      │
│      │              ▼                    ▼                      │
│      │     ┌─ verbs ────────┐    testing_commitBlockV1          │
│      │     │ native Go tx   │           │                       │
│      │     │ builders       │           ▼                       │
│      │     └────────────────┘      Nethermind                   │
│      │                                  │                       │
│      └────────────── sensor (statecomp_get)                     │
│                                                                 │
│  journal.bin (protobuf binlog + BLAKE3 chain hash)              │
│  payloads.rlp (4-B-LE-prefixed ExecutionPayloadV3 RLP)          │
│  run-manifest.json                                              │
└────────────────────────────────────────────────────────────────┘
```

## Build

```
make build       # local binary at bin/orchestrator
make linux       # static Linux/amd64 binary at bin/orchestrator.linux-amd64
make test        # all tests, race detector clean
```

## Run

```
ORCH_DEPLOY_PRIVATE_KEY=0x... \
bin/orchestrator run \
  --rpc-url http://nethermind:8545 \
  --state-dir /var/orch/state \
  --target-yaml /etc/orch/target.yaml \
  --genesis-sha256 <hex>
```

## Subcommands

- `run` — closed-loop controller against Nethermind.
- `replay --payloads payloads.rlp --manifest run-manifest.json --rpc-url <engine-api>` — cross-client replay via Engine API.
- `verify-journal --path journal.bin` — re-derive the chain hash and print the final hash + record count.

## Layout

```
cmd/orchestrator/         cobra entry + subcommand wiring
internal/
  lifecycle/              main loop, startup decision, batch pipeline
  controller/             pick_next_batch + apply_observation
  mathx/                  Michelot simplex + tanh-saturated α
  facade/                 dispatcher: native verb builder → signer
  verbs/                  native Go transaction builders (13 verbs)
  signer/                 in-process ECDSA via go-ethereum
  rpc/                    JSON-RPC client + JWT round-tripper
  sensor/                 statecomp_get poller
  journal/                protobuf binlog + BLAKE3 chain hash
  payloads/               ExecutionPayloadV3 RLP stream
  manifest/               run-manifest.json
  target/                 YAML loader + fsnotify watcher
  referencef/             REFERENCE_F.yaml loader
  probe/                  F-matrix probe
  replay/                 Engine API cross-client driver
  lock/                   POSIX flock(2) state-dir lock
  orchpb/                 generated protobuf bindings
proto/                    .proto sources
```

## See also

- The planning document at `~/.claude/plans/let-s-create-a-structure-serialized-popcorn.md`
- The legacy Python implementation at `../orchestrator-py/` (will be deleted on cutover)
