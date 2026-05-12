# builder-worker

Subprocess shim that hosts orchestrator-py verb builders for the Go orchestrator.
The Go `builderpool` spawns this process and speaks length-prefixed protobuf over stdio.

## Wire protocol

```
stdin:  [4-byte BE length][protobuf BuildBatchRequest]
stdout: [4-byte BE length][protobuf BuildBatchResponse]
```

## Setup

```sh
# 1. Generate protobuf bindings (requires protoc on PATH)
make proto

# 2. Install orchestrator-py as an editable dep, then run normally
pip install -e ../orchestrator-py
python -m builder_worker
```

## Tests

```sh
make test
```

`make test` uses `uv run` and installs `orchestrator-py` via its absolute file URI so
no manual `pip install` is needed for the test run.

If you prefer a plain `pytest` invocation:

```sh
pip install -e ../orchestrator-py
pip install protobuf pytest
pytest tests/ -v
```

## Protobuf descriptor sharing

`builder_pb2` imports `txsigner_pb2` from `orchestrator._proto` (the copy shipped
with orchestrator-py) rather than re-registering it locally. This prevents
`duplicate file name txsigner.proto` errors in the protobuf descriptor pool when
both packages are loaded in the same process.

`make proto` handles this automatically via a `sed` rewrite of the generated import.
