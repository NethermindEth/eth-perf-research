// Package replay submits a recorded stream of ExecutionPayloadV3s to an EL
// client via the Engine API and verifies the final state root against the
// run manifest.
package replay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

var (
	// ErrReplayMismatch is returned when all payloads were accepted but the
	// final state root differs from manifest.FinalStateRoot.
	ErrReplayMismatch = errors.New("replay: final state root mismatch")

	// ErrReplayInvalid is returned when the Engine API rejects a payload
	// (payloadStatus.status != "VALID").
	ErrReplayInvalid = errors.New("replay: payload rejected by client")
)

// Driver holds the dependencies needed to replay a payload stream.
type Driver struct {
	Client   *rpc.Client
	Manifest *manifest.Manifest
}

// Replay reads payloads from payloadsPath and submits them to the EL client
// via engine_newPayloadV4 + engine_forkchoiceUpdatedV3. It returns:
//   - nil on success with matching state root
//   - ErrReplayMismatch when all payloads applied but state root differs
//   - ErrReplayInvalid when a payload is rejected
//   - a wrapped I/O or RPC error for other failures
func (d *Driver) Replay(ctx context.Context, payloadsPath string) error {
	r, err := payloads.OpenReader(payloadsPath)
	if err != nil {
		return fmt.Errorf("replay: open payloads: %w", err)
	}
	defer r.Close()

	var count int
	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("replay: read payload %d: %w", count, err)
		}

		if err := d.submitPayload(ctx, p); err != nil {
			return err
		}

		count++
		if count%100 == 0 {
			slog.Info("replay progress", "submitted", count)
		}
	}

	slog.Info("replay complete", "total_payloads", count)

	if d.Manifest.FinalStateRoot == "" {
		return nil
	}

	var blockResult map[string]json.RawMessage
	if err := d.Client.Call(ctx, "eth_getBlockByNumber", []any{"latest", false}, &blockResult); err != nil {
		return fmt.Errorf("replay: eth_getBlockByNumber: %w", err)
	}

	var stateRootRaw string
	if raw, ok := blockResult["stateRoot"]; ok {
		if err := json.Unmarshal(raw, &stateRootRaw); err != nil {
			return fmt.Errorf("replay: parse stateRoot: %w", err)
		}
	}

	if !equalHex(stateRootRaw, d.Manifest.FinalStateRoot) {
		slog.Error("state root mismatch",
			"got", stateRootRaw,
			"want", d.Manifest.FinalStateRoot,
		)
		return ErrReplayMismatch
	}

	return nil
}

// submitPayload sends engine_newPayloadV4 then engine_forkchoiceUpdatedV3 for p.
func (d *Driver) submitPayload(ctx context.Context, p *payloads.ExecutionPayloadV3) error {
	blockHashHex := "0x" + hex.EncodeToString(p.BlockHash[:])

	// engine_newPayloadV4
	wp := toWirePayload(p)
	var newPayloadResp payloadStatusResponse
	err := d.Client.Call(ctx, "engine_newPayloadV4", []any{
		wp,
		[]string{},                      // blobVersionedHashes
		"0x" + strings.Repeat("00", 32), // parentBeaconBlockRoot
		[]string{},                      // executionRequests
	}, &newPayloadResp)
	if err != nil {
		return fmt.Errorf("replay: engine_newPayloadV4 block %s: %w", blockHashHex, err)
	}
	if newPayloadResp.Status != "VALID" {
		return fmt.Errorf("%w: block %s newPayload status=%s",
			ErrReplayInvalid, blockHashHex, newPayloadResp.Status)
	}
	if !equalHex(newPayloadResp.LatestValidHash, blockHashHex) {
		return fmt.Errorf("%w: block %s newPayload latestValidHash=%s",
			ErrReplayInvalid, blockHashHex, newPayloadResp.LatestValidHash)
	}

	// engine_forkchoiceUpdatedV3
	fcs := forkchoiceState{
		HeadBlockHash:      blockHashHex,
		SafeBlockHash:      blockHashHex,
		FinalizedBlockHash: blockHashHex,
	}
	var fcuResp forkchoiceUpdatedResponse
	if err := d.Client.Call(ctx, "engine_forkchoiceUpdatedV3", []any{fcs, nil}, &fcuResp); err != nil {
		return fmt.Errorf("replay: engine_forkchoiceUpdatedV3 block %s: %w", blockHashHex, err)
	}
	if fcuResp.PayloadStatus.Status != "VALID" {
		return fmt.Errorf("%w: block %s forkchoiceUpdated status=%s",
			ErrReplayInvalid, blockHashHex, fcuResp.PayloadStatus.Status)
	}

	return nil
}

// --- Engine API wire types ---

// wirePayload is the JSON shape sent to engine_newPayloadV4.
type wirePayload struct {
	ParentHash    string   `json:"parentHash"`
	FeeRecipient  string   `json:"feeRecipient"`
	StateRoot     string   `json:"stateRoot"`
	ReceiptsRoot  string   `json:"receiptsRoot"`
	LogsBloom     string   `json:"logsBloom"`
	PrevRandao    string   `json:"prevRandao"`
	BlockNumber   string   `json:"blockNumber"`
	GasLimit      string   `json:"gasLimit"`
	GasUsed       string   `json:"gasUsed"`
	Timestamp     string   `json:"timestamp"`
	ExtraData     string   `json:"extraData"`
	BaseFeePerGas string   `json:"baseFeePerGas"`
	BlockHash     string   `json:"blockHash"`
	Transactions  []string `json:"transactions"`
	Withdrawals   []any    `json:"withdrawals"`
	BlobGasUsed   string   `json:"blobGasUsed"`
	ExcessBlobGas string   `json:"excessBlobGas"`
}

type payloadStatusResponse struct {
	Status          string `json:"status"`
	LatestValidHash string `json:"latestValidHash"`
	ValidationError any    `json:"validationError"`
}

type forkchoiceState struct {
	HeadBlockHash      string `json:"headBlockHash"`
	SafeBlockHash      string `json:"safeBlockHash"`
	FinalizedBlockHash string `json:"finalizedBlockHash"`
}

type forkchoiceUpdatedResponse struct {
	PayloadStatus payloadStatusResponse `json:"payloadStatus"`
	PayloadID     *string               `json:"payloadId"`
}

// toWirePayload converts an ExecutionPayloadV3 to its Engine API JSON form.
func toWirePayload(p *payloads.ExecutionPayloadV3) *wirePayload {
	txs := make([]string, len(p.Transactions))
	for i, tx := range p.Transactions {
		txs[i] = "0x" + hex.EncodeToString(tx)
	}

	var withdrawals []any
	if p.Withdrawals != nil {
		withdrawals = make([]any, len(p.Withdrawals))
		for i, w := range p.Withdrawals {
			withdrawals[i] = map[string]string{
				"index":          toHexUint(w.Index),
				"validatorIndex": toHexUint(w.Validator),
				"address":        "0x" + hex.EncodeToString(w.Address[:]),
				"amount":         toHexUint(w.Amount),
			}
		}
	} else {
		withdrawals = []any{}
	}

	return &wirePayload{
		ParentHash:    "0x" + hex.EncodeToString(p.ParentHash[:]),
		FeeRecipient:  "0x" + hex.EncodeToString(p.FeeRecipient[:]),
		StateRoot:     "0x" + hex.EncodeToString(p.StateRoot[:]),
		ReceiptsRoot:  "0x" + hex.EncodeToString(p.ReceiptsRoot[:]),
		LogsBloom:     "0x" + hex.EncodeToString(p.LogsBloom[:]),
		PrevRandao:    "0x" + hex.EncodeToString(p.PrevRandao[:]),
		BlockNumber:   toHexUint(p.BlockNumber),
		GasLimit:      toHexUint(p.GasLimit),
		GasUsed:       toHexUint(p.GasUsed),
		Timestamp:     toHexUint(p.Timestamp),
		ExtraData:     "0x" + hex.EncodeToString(p.ExtraData),
		BaseFeePerGas: toHexBigInt(p.BaseFeePerGas),
		BlockHash:     "0x" + hex.EncodeToString(p.BlockHash[:]),
		Transactions:  txs,
		Withdrawals:   withdrawals,
		BlobGasUsed:   toHexUint(p.BlobGasUsed),
		ExcessBlobGas: toHexUint(p.ExcessBlobGas),
	}
}

// toHexUint encodes u as a 0x-prefixed minimal-zero hex string.
// Zero encodes as "0x0".
func toHexUint(u uint64) string {
	if u == 0 {
		return "0x0"
	}
	return fmt.Sprintf("0x%x", u)
}

// toHexBigInt encodes b as a 0x-prefixed hex string. Nil or zero → "0x0".
func toHexBigInt(b *big.Int) string {
	if b == nil || b.Sign() == 0 {
		return "0x0"
	}
	return "0x" + b.Text(16)
}

// equalHex compares two 0x-prefixed hex strings case-insensitively after
// stripping the prefix.
func equalHex(a, b string) bool {
	return strings.EqualFold(
		strings.TrimPrefix(a, "0x"),
		strings.TrimPrefix(b, "0x"),
	)
}
