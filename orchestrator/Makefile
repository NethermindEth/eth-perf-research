SHELL := /bin/bash
PROTOC ?= protoc
PB_DIR := internal
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w

.PHONY: all proto build linux test test-race lint tidy clean

all: proto build

proto:
	mkdir -p internal/orchpb
	$(PROTOC) --go_out=internal/orchpb --go_opt=paths=source_relative \
		-I=proto proto/builder.proto proto/journal.proto proto/txsigner.proto

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
