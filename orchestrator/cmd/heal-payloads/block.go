package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// rpcBlockFetcher is the production BlockFetcher that talks to Nethermind
// over JSON-RPC via internal/rpc.Client.
type rpcBlockFetcher struct {
	client *rpc.Client
}

func newRPCBlockFetcher(client *rpc.Client) *rpcBlockFetcher {
	return &rpcBlockFetcher{client: client}
}

// wireBlock is the full eth_getBlockByNumber view we need to rebuild a payload.
type wireBlock struct {
	Number           string          `json:"number"`
	Hash             string          `json:"hash"`
	ParentHash       string          `json:"parentHash"`
	Miner            string          `json:"miner"`
	StateRoot        string          `json:"stateRoot"`
	ReceiptsRoot     string          `json:"receiptsRoot"`
	LogsBloom        string          `json:"logsBloom"`
	MixHash          string          `json:"mixHash"`
	GasLimit         string          `json:"gasLimit"`
	GasUsed          string          `json:"gasUsed"`
	Timestamp        string          `json:"timestamp"`
	ExtraData        string          `json:"extraData"`
	BaseFeePerGas    string          `json:"baseFeePerGas"`
	BlobGasUsed      string          `json:"blobGasUsed"`
	ExcessBlobGas    string          `json:"excessBlobGas"`
	Transactions     []string        `json:"transactions"`
	Withdrawals      json.RawMessage `json:"withdrawals"`
}

type wireWithdrawal struct {
	Index          string `json:"index"`
	ValidatorIndex string `json:"validatorIndex"`
	Address        string `json:"address"`
	Amount         string `json:"amount"`
}

func (f *rpcBlockFetcher) HeadBlock(ctx context.Context) (uint64, error) {
	var raw string
	if err := f.client.Call(ctx, "eth_blockNumber", nil, &raw); err != nil {
		return 0, fmt.Errorf("heal: eth_blockNumber: %w", err)
	}
	n, err := parseHexUint64(raw)
	if err != nil {
		return 0, fmt.Errorf("heal: parse head: %w", err)
	}
	return n, nil
}

func (f *rpcBlockFetcher) PayloadAt(ctx context.Context, blockNumber uint64) (*payloads.ExecutionPayloadV3, error) {
	tag := fmt.Sprintf("0x%x", blockNumber)
	var raw json.RawMessage
	if err := f.client.Call(ctx, "eth_getBlockByNumber", []any{tag, false}, &raw); err != nil {
		return nil, fmt.Errorf("heal: eth_getBlockByNumber(%d): %w", blockNumber, err)
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("heal: block %d not found", blockNumber)
	}

	var wb wireBlock
	if err := json.Unmarshal(raw, &wb); err != nil {
		return nil, fmt.Errorf("heal: decode block %d: %w", blockNumber, err)
	}

	txs := make([][]byte, len(wb.Transactions))
	for i, hHex := range wb.Transactions {
		var rawTx string
		if err := f.client.Call(ctx, "eth_getRawTransactionByHash", []any{hHex}, &rawTx); err != nil {
			return nil, fmt.Errorf("heal: eth_getRawTransactionByHash(%s): %w", hHex, err)
		}
		b, err := decodeHexBytes(rawTx)
		if err != nil {
			return nil, fmt.Errorf("heal: decode raw tx %s: %w", hHex, err)
		}
		txs[i] = b
	}

	return wireBlockToPayload(&wb, txs)
}

// wireBlockToPayload converts a decoded RPC block + materialised raw txs into
// a canonical ExecutionPayloadV3. Pure function; no I/O.
func wireBlockToPayload(wb *wireBlock, txs [][]byte) (*payloads.ExecutionPayloadV3, error) {
	number, err := parseHexUint64(wb.Number)
	if err != nil {
		return nil, fmt.Errorf("heal: block.number: %w", err)
	}
	gasLimit, err := parseHexUint64(wb.GasLimit)
	if err != nil {
		return nil, fmt.Errorf("heal: block.gasLimit: %w", err)
	}
	gasUsed, err := parseHexUint64(wb.GasUsed)
	if err != nil {
		return nil, fmt.Errorf("heal: block.gasUsed: %w", err)
	}
	ts, err := parseHexUint64(wb.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("heal: block.timestamp: %w", err)
	}
	baseFee := new(big.Int)
	if wb.BaseFeePerGas != "" {
		b, err := parseHexBigInt(wb.BaseFeePerGas)
		if err != nil {
			return nil, fmt.Errorf("heal: block.baseFeePerGas: %w", err)
		}
		baseFee = b
	}
	var blobGasUsed uint64
	if wb.BlobGasUsed != "" {
		blobGasUsed, err = parseHexUint64(wb.BlobGasUsed)
		if err != nil {
			return nil, fmt.Errorf("heal: block.blobGasUsed: %w", err)
		}
	}
	var excessBlobGas uint64
	if wb.ExcessBlobGas != "" {
		excessBlobGas, err = parseHexUint64(wb.ExcessBlobGas)
		if err != nil {
			return nil, fmt.Errorf("heal: block.excessBlobGas: %w", err)
		}
	}
	extra, err := decodeHexBytes(wb.ExtraData)
	if err != nil {
		return nil, fmt.Errorf("heal: block.extraData: %w", err)
	}
	bloom, err := decodeHexBytes(wb.LogsBloom)
	if err != nil {
		return nil, fmt.Errorf("heal: block.logsBloom: %w", err)
	}
	if len(bloom) != 256 {
		return nil, fmt.Errorf("heal: logsBloom: expected 256 bytes, got %d", len(bloom))
	}

	withdrawals, err := decodeWithdrawals(wb.Withdrawals)
	if err != nil {
		return nil, fmt.Errorf("heal: block.withdrawals: %w", err)
	}

	p := &payloads.ExecutionPayloadV3{
		ParentHash:    common.HexToHash(wb.ParentHash),
		FeeRecipient:  common.HexToAddress(wb.Miner),
		StateRoot:     common.HexToHash(wb.StateRoot),
		ReceiptsRoot:  common.HexToHash(wb.ReceiptsRoot),
		PrevRandao:    common.HexToHash(wb.MixHash),
		BlockNumber:   number,
		GasLimit:      gasLimit,
		GasUsed:       gasUsed,
		Timestamp:     ts,
		ExtraData:     extra,
		BaseFeePerGas: baseFee,
		BlockHash:     common.HexToHash(wb.Hash),
		Transactions:  txs,
		Withdrawals:   withdrawals,
		BlobGasUsed:   blobGasUsed,
		ExcessBlobGas: excessBlobGas,
	}
	copy(p.LogsBloom[:], bloom)
	return p, nil
}

func decodeWithdrawals(raw json.RawMessage) ([]*types.Withdrawal, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var wws []wireWithdrawal
	if err := json.Unmarshal(raw, &wws); err != nil {
		return nil, err
	}
	out := make([]*types.Withdrawal, len(wws))
	for i, w := range wws {
		idx, err := parseHexUint64(w.Index)
		if err != nil {
			return nil, fmt.Errorf("withdrawal[%d].index: %w", i, err)
		}
		val, err := parseHexUint64(w.ValidatorIndex)
		if err != nil {
			return nil, fmt.Errorf("withdrawal[%d].validatorIndex: %w", i, err)
		}
		amt, err := parseHexUint64(w.Amount)
		if err != nil {
			return nil, fmt.Errorf("withdrawal[%d].amount: %w", i, err)
		}
		out[i] = &types.Withdrawal{
			Index:     idx,
			Validator: val,
			Address:   common.HexToAddress(w.Address),
			Amount:    amt,
		}
	}
	return out, nil
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

func parseHexBigInt(s string) (*big.Int, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return new(big.Int), nil
	}
	b := new(big.Int)
	if _, ok := b.SetString(s, 16); !ok {
		return nil, fmt.Errorf("parseHexBigInt %q: invalid hex", s)
	}
	return b, nil
}

func decodeHexBytes(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return []byte{}, nil
	}
	return hex.DecodeString(s)
}
