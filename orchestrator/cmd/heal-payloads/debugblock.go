package main

import (
	"context"
	"encoding/json"
	"fmt"

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
	return blockToPayload(&blk)
}

func blockToPayload(blk *types.Block) (*payloads.ExecutionPayloadV3, error) {
	h := blk.Header()
	txs := make([][]byte, len(blk.Transactions()))
	for i, tx := range blk.Transactions() {
		enc, err := tx.MarshalBinary()
		if err != nil {
			return nil, fmt.Errorf("heal: marshal tx %d in block %d: %w", i, h.Number.Uint64(), err)
		}
		txs[i] = enc
	}
	bloom := h.Bloom
	var blobGasUsed, excessBlobGas uint64
	if h.BlobGasUsed != nil {
		blobGasUsed = *h.BlobGasUsed
	}
	if h.ExcessBlobGas != nil {
		excessBlobGas = *h.ExcessBlobGas
	}
	return &payloads.ExecutionPayloadV3{
		ParentHash:    h.ParentHash,
		FeeRecipient:  h.Coinbase,
		StateRoot:     h.Root,
		ReceiptsRoot:  h.ReceiptHash,
		LogsBloom:     bloom[:],
		Random:        h.MixDigest,
		Number:        h.Number.Uint64(),
		GasLimit:      h.GasLimit,
		GasUsed:       h.GasUsed,
		Timestamp:     h.Time,
		ExtraData:     h.Extra,
		BaseFeePerGas: h.BaseFee,
		BlockHash:     blk.Hash(),
		Transactions:  txs,
		Withdrawals:   blk.Withdrawals(),
		BlobGasUsed:   &blobGasUsed,
		ExcessBlobGas: &excessBlobGas,
	}, nil
}
