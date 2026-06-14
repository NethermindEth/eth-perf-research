package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// debugBlockFetcher materialises payloads via debug_getRawBlock, which returns
// the whole consensus-encoded block in a single call. For bloated blocks (~7200
// txs each) this replaces ~7200 eth_getRawTransactionByHash round-trips per
// block with one, making a full-range rebuild tractable.
type debugBlockFetcher struct {
	client *rpc.Client
}

func newDebugBlockFetcher(client *rpc.Client) *debugBlockFetcher {
	return &debugBlockFetcher{client: client}
}

func (f *debugBlockFetcher) HeadBlock(ctx context.Context) (uint64, error) {
	var raw string
	if err := f.client.Call(ctx, "eth_blockNumber", nil, &raw); err != nil {
		return 0, fmt.Errorf("heal: eth_blockNumber: %w", err)
	}
	return parseHexUint64(raw)
}

func (f *debugBlockFetcher) BlockHashAt(ctx context.Context, blockNumber uint64) (common.Hash, error) {
	tag := fmt.Sprintf("0x%x", blockNumber)
	var raw json.RawMessage
	if err := f.client.Call(ctx, "eth_getBlockByNumber", []any{tag, false}, &raw); err != nil {
		return common.Hash{}, fmt.Errorf("heal: eth_getBlockByNumber(%d): %w", blockNumber, err)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return common.Hash{}, fmt.Errorf("heal: block %d not found", blockNumber)
	}
	var wb struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(raw, &wb); err != nil {
		return common.Hash{}, fmt.Errorf("heal: decode block %d hash: %w", blockNumber, err)
	}
	return common.HexToHash(wb.Hash), nil
}

func (f *debugBlockFetcher) PayloadAt(ctx context.Context, blockNumber uint64) (*payloads.ExecutionPayloadV3, error) {
	tag := fmt.Sprintf("0x%x", blockNumber)
	var raw string
	if err := f.client.Call(ctx, "debug_getRawBlock", []any{tag}, &raw); err != nil {
		return nil, fmt.Errorf("heal: debug_getRawBlock(%d): %w", blockNumber, err)
	}
	b, err := decodeHexBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("heal: decode raw block %d: %w", blockNumber, err)
	}
	var blk types.Block
	if err := rlp.DecodeBytes(b, &blk); err != nil {
		return nil, fmt.Errorf("heal: rlp-decode block %d: %w", blockNumber, err)
	}
	return engine.BlockToExecutableData(&blk, nil, nil, nil).ExecutionPayload, nil
}

func parseHexUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return 0, nil
	}
	var v uint64
	if _, err := fmt.Sscanf(s, "%x", &v); err != nil {
		return 0, fmt.Errorf("parseHexUint64 %q: %w", s, err)
	}
	return v, nil
}

func decodeHexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return []byte{}, nil
	}
	return hex.DecodeString(s)
}
