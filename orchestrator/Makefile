SHELL := /bin/bash
PROTOC ?= protoc
PB_DIR := internal
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w

# Sidecar build defaults — Linux production uses CGO+librocksdb (Docker);
# local dev uses the nogrocksdb stub so Mac developers without librocksdb-dev
# can run the unit tests.
SIDECAR_TAGS ?= nogrocksdb
SIDECAR_BIN          := bin/sidecar
SIDECAR_BIN_LINUX    := bin/sidecar-linux-amd64
SIDECAR_BIN_CGO      := bin/sidecar-cgo

.PHONY: all proto build linux test test-race lint tidy clean \
        build-sidecar build-sidecar-linux build-sidecar-cgo \
        test-sidecar vet-sidecar docker-sidecar

all: proto build build-sidecar

proto:
	mkdir -p internal/orchpb
	$(PROTOC) --go_out=internal/orchpb --go_opt=paths=source_relative \
		-I=proto proto/journal.proto proto/txsigner.proto

build: proto
	go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o bin/orchestrator ./cmd/orchestrator

linux: proto
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build $(GOFLAGS) -ldflags='$(LDFLAGS) -extldflags=-static' \
		-o bin/orchestrator.linux-amd64 ./cmd/orchestrator

test: proto
	go test ./...

test-race: proto
	go test -race ./...

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/ proto/*.pb.go internal/*/pb

# --- sidecar targets -------------------------------------------------------

build-sidecar: ## Build the local sidecar binary (stub backend; no CGO required).
	mkdir -p bin
	CGO_ENABLED=0 go build $(GOFLAGS) -tags $(SIDECAR_TAGS) -ldflags='$(LDFLAGS)' -o $(SIDECAR_BIN) ./cmd/sidecar

build-sidecar-linux: ## Cross-compile a Linux/amd64 sidecar binary (stub backend; Docker does the full grocksdb image).
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build $(GOFLAGS) -tags $(SIDECAR_TAGS) -ldflags='$(LDFLAGS)' -o $(SIDECAR_BIN_LINUX) ./cmd/sidecar

build-sidecar-cgo: ## Build the full grocksdb-backed sidecar binary (requires librocksdb-dev).
	mkdir -p bin
	CGO_ENABLED=1 go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(SIDECAR_BIN_CGO) ./cmd/sidecar

test-sidecar: ## Run sidecar unit + integration tests with the stub backend.
	go test -tags $(SIDECAR_TAGS) -race -count=1 ./pkg/sidecar/... ./test/sidecar/... ./cmd/sidecar/...

vet-sidecar:
	go vet -tags $(SIDECAR_TAGS) ./pkg/sidecar/... ./test/sidecar/... ./cmd/sidecar/...

docker-sidecar: ## Build the production sidecar Docker image (alpine + binary).
	docker build -t eth-perf-research/sidecar:dev -f Dockerfile.sidecar .
