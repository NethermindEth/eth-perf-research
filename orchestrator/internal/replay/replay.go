// Package replay submits a recorded stream of ExecutionPayloadV3s to an EL
// client via the Engine API and verifies the final state root against the
// run manifest.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

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

	// The recording path commits blocks via testing_commitBlockV1 with
	// ProcessingOptions.NoValidation, which accepted blocks whose timestamp
	// equals the parent's. The normal Engine API (engine_newPayloadV4, used
	// here) requires strictly-increasing timestamps, so we re-stamp duplicate
	// timestamps to prev+1. Bumping a timestamp (or relinking the parent hash
	// after an upstream bump) changes the block hash, so relink recomputes and
	// rewrites BlockHash to keep the chain hash-linked. The state root is
	// timestamp-independent for this workload, so the final-state-root check
	// downstream still holds.
	var prevHash *common.Hash
	var prevTs uint64

	var count int
	for {
		p, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("replay: read payload %d: %w", count, err)
		}

		if err := relink(p, prevHash, prevTs); err != nil {
			return fmt.Errorf("replay: relink payload %d: %w", count, err)
		}

		if err := d.submitPayload(ctx, p); err != nil {
			return err
		}

		h := p.BlockHash
		prevHash = &h
		prevTs = p.Timestamp

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

// relink re-stamps and re-links p so the replayed chain has strictly-increasing
// timestamps and hash-linked parents, recomputing the block hash when either
// field changes.
//
// The recorded payloads come from the NoValidation testing_commitBlockV1 path,
// which accepted equal consecutive timestamps; engine_newPayloadV4 rejects
// those. Bumping the timestamp (or relinking the parent hash after an upstream
// bump) invalidates the stored BlockHash, so we recompute it with
// ExecutableDataToBlockNoHash (which does NOT validate against the stale stored
// hash). The recompute uses the same empty beaconRoot / empty executionRequests
// the driver submits, so the derived hash matches what the EL will compute.
func relink(p *payloads.ExecutionPayloadV3, prevHash *common.Hash, prevTs uint64) error {
	if prevHash == nil {
		return nil
	}

	needBump := p.Timestamp <= prevTs
	if needBump {
		p.Timestamp = prevTs + 1
	}
	needRelink := p.ParentHash != *prevHash
	if needRelink {
		p.ParentHash = *prevHash
	}
	if !needBump && !needRelink {
		return nil
	}

	zeroHash := common.Hash{}
	blk, err := engine.ExecutableDataToBlockNoHash(*p, nil, &zeroHash, [][]byte{})
	if err != nil {
		return err
	}
	p.BlockHash = blk.Hash()
	return nil
}

func (d *Driver) submitPayload(ctx context.Context, p *payloads.ExecutionPayloadV3) error {
	// We submit empty blobVersionedHashes and a zero parentBeaconBlockRoot, which
	// is correct only for the synthetic non-blob bloating workload. Fail loudly
	// rather than replay a blob-carrying payload with mismatched (empty) hashes.
	if p.BlobGasUsed != nil && *p.BlobGasUsed != 0 {
		return fmt.Errorf("replay: block %s carries blobs (blobGasUsed=%d); blob replay is unsupported",
			p.BlockHash.Hex(), *p.BlobGasUsed)
	}

	var status engine.PayloadStatusV1
	err := d.Client.Call(ctx, "engine_newPayloadV4", []any{
		p,                 // ExecutionPayloadV3 == engine.ExecutableData
		[]hexutil.Bytes{}, // blobVersionedHashes
		common.Hash{},     // parentBeaconBlockRoot (zero)
		[]hexutil.Bytes{}, // executionRequests
	}, &status)
	if err != nil {
		return fmt.Errorf("replay: engine_newPayloadV4 block %s: %w", p.BlockHash.Hex(), err)
	}
	if status.Status != "VALID" {
		return fmt.Errorf("%w: block %s newPayload status=%s",
			ErrReplayInvalid, p.BlockHash.Hex(), status.Status)
	}
	if status.LatestValidHash == nil || *status.LatestValidHash != p.BlockHash {
		return fmt.Errorf("%w: block %s newPayload latestValidHash=%v",
			ErrReplayInvalid, p.BlockHash.Hex(), status.LatestValidHash)
	}

	fcs := engine.ForkchoiceStateV1{
		HeadBlockHash:      p.BlockHash,
		SafeBlockHash:      p.BlockHash,
		FinalizedBlockHash: p.BlockHash,
	}
	var fcuResp engine.ForkChoiceResponse
	if err := d.Client.Call(ctx, "engine_forkchoiceUpdatedV3", []any{fcs, nil}, &fcuResp); err != nil {
		return fmt.Errorf("replay: engine_forkchoiceUpdatedV3 block %s: %w", p.BlockHash.Hex(), err)
	}
	if fcuResp.PayloadStatus.Status != "VALID" {
		return fmt.Errorf("%w: block %s forkchoiceUpdated status=%s",
			ErrReplayInvalid, p.BlockHash.Hex(), fcuResp.PayloadStatus.Status)
	}

	return nil
}

// equalHex compares two 0x-prefixed hex strings case-insensitively after
// stripping the prefix.
func equalHex(a, b string) bool {
	return strings.EqualFold(
		strings.TrimPrefix(a, "0x"),
		strings.TrimPrefix(b, "0x"),
	)
}
