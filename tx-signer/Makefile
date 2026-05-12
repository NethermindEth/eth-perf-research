PROTOC      ?= protoc
ORCH_PROTO  := ../orchestrator/src/orchestrator/_proto

.PHONY: proto build linux test clean

# Generate Go bindings (under ./pb) and Python bindings (under the orchestrator package).
proto:
	$(PROTOC) -I=proto \
	          --go_out=pb --go_opt=paths=source_relative \
	          --python_out=$(ORCH_PROTO) \
	          --pyi_out=$(ORCH_PROTO) \
	          proto/txsigner.proto
	# Generated Go file lands at pb/txsigner.pb.go (path matches proto basename).
	# Generated Python lands at $(ORCH_PROTO)/txsigner_pb2.py + .pyi.
	# Make the Python output a proper subpackage.
	touch $(ORCH_PROTO)/__init__.py

build:
	go build -o tx-signer ./

# Static Linux/amd64 binary for the orchestrator container.
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	  go build -trimpath -ldflags='-s -w -extldflags=-static' -o tx-signer.linux-amd64 ./

test:
	go test ./...

clean:
	rm -f tx-signer tx-signer.linux-amd64
	rm -f pb/*.pb.go
	rm -f $(ORCH_PROTO)/txsigner_pb2.py $(ORCH_PROTO)/txsigner_pb2.pyi
