# tx-signer — out-of-process EIP-1559 signer for the orchestrator

A Go binary that signs Ethereum EIP-1559 (type-2) transactions on behalf of the
Python orchestrator, driven over stdio with a Protobuf wire protocol.

The Python orchestrator's hot path (`facade/_builder.py`) is sig-CPU-bound at
~3,000 tx/s single-core because `eth_account.Account.sign_transaction` adds >90%
Python overhead per transaction. This binary uses
`github.com/ethereum/go-ethereum/crypto.Sign` and fans signing out across
goroutines (one per core), sustaining **>70,000 tx/s on a laptop** and
**>200,000 tx/s on the 64-core bloatnet VM** for batches of ~10k templates.

## Wire protocol

```
[4-byte big-endian length][protobuf SignRequest]   →
                                                  ←  [4-byte big-endian length][protobuf SignResponse]
```

Schema in [`proto/txsigner.proto`](proto/txsigner.proto). Each request bundles
N tx templates; the response returns N signed RLP bytes in the same order.

## Build

```bash
make proto    # regenerate Go + Python bindings (commit both)
make build    # local binary (./tx-signer)
make linux    # static Linux/amd64 binary for the orchestrator container
```

The Linux binary is what gets mounted into the orchestrator container at
`/signer/tx-signer`.

## Run standalone

```bash
SIGNER_PRIVATE_KEY=0xbcdf2024... ./tx-signer
# (stdin/stdout speak the framed protobuf protocol; not meant for humans)
```

## Verify correctness

```bash
make build
TX_SIGNER_BINARY=./tx-signer EQUIV_N=10000 \
  uv run --with eth_account --with eth_utils --with protobuf python3 test_equivalence.py
```

Generates N random type-2 templates and asserts the signed RLP matches
`eth_account.Account.sign_transaction` byte-for-byte. Currently:

- 10,000 templates: byte-for-byte match, **274× faster than eth_account** on macOS.

## Configuration knobs

- `SIGNER_PRIVATE_KEY` — required, hex (with or without `0x` prefix).
- `FAST_SIGNER_MAX_TXS_PER_BATCH` (read by the Python wrapper) — caps batch
  size to avoid building huge blocks that get partially rejected by Nethermind.
  Default 1500.
- `TX_SIGNER_BINARY` (read by `fast_signer.py`) — path to the binary
  (defaults to `/signer/tx-signer`).
