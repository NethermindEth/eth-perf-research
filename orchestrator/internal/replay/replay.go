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
	"time"

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
	// MemGate, when non-nil, paces dispatch so the EL is never driven past its
	// state-flush throughput — preventing the diff-layer OOM (see MemGate).
	MemGate *MemGate
	// lastHead is the hash of the most recently applied block. The mem gate's
	// Flush nudge re-asserts forkchoice to it while paused. Set and read on the
	// single Replay goroutine (the gate runs synchronously between blocks).
	lastHead common.Hash
	// Follow, when true, tails a payloads file a live bloat run is still appending
	// to: instead of stopping at EOF, the reader waits FollowPoll for more frames.
	// Lets the replay stream blocks as they are produced, decoupled via the file so
	// the replay's pace never backpressures the bloat. Runs until ctx is cancelled.
	Follow     bool
	FollowPoll time.Duration
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
	// Resume support: if the EL already has blocks (a prior run persisted up to
	// some head before stopping — e.g. after an OOM-kill), payloads at or below
	// that head are skipped. Re-submitting them is wasted work and, worse, a
	// forkchoice to a block far below the EL head is rejected as "Too deep
	// reorg". With a fresh EL (head 0) nothing is skipped.
	// The EL head must be detected reliably: skipping the wrong number of applied
	// blocks would either re-submit known blocks (too-deep-reorg rejection) or
	// skip un-applied ones (a gap). Fail loudly rather than guess.
	head, err := d.headBlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("replay: detect EL head: %w", err)
	}

	// While the gate is paused, re-assert forkchoice to the last applied head so
	// the EL keeps persisting its dirty state buffer (no new blocks arrive to
	// trigger it otherwise). No-op until the first block is submitted.
	if d.MemGate != nil {
		d.MemGate.Flush = func(ctx context.Context) error {
			if (d.lastHead == common.Hash{}) {
				return nil
			}
			return d.forceFlushHead(ctx)
		}
	}

	var prevHash *common.Hash
	var prevTs uint64
	var read, submitted, skipped int
	for {
		var p *payloads.ExecutionPayloadV3
		var err error
		if d.Follow {
			// Tail the file a live bloat run is appending to; blocks for more frames.
			p, err = r.NextFollow(ctx, d.FollowPoll)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				slog.Info("replay follow: stopped", "submitted", submitted)
				return nil
			}
		} else {
			p, err = r.Next()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("replay: read payload %d: %w", read, err)
		}
		read++

		if err := relink(p, prevHash, prevTs); err != nil {
			return fmt.Errorf("replay: relink payload %d: %w", read, err)
		}
		// Track linkage for every payload (including skipped ones) so the relink
		// chain is byte-identical to the original run and the resume block's
		// parent hash matches what the EL already stored.
		h := p.BlockHash
		prevHash = &h
		prevTs = p.Timestamp

		if p.Number <= head {
			skipped++
			if skipped%50000 == 0 {
				slog.Info("replay fast-forward over applied blocks", "skipped", skipped, "el_head", head)
			}
			continue
		}

		if err := d.submitPayload(ctx, p); err != nil {
			return err
		}
		submitted++
		d.lastHead = p.BlockHash

		if d.MemGate != nil && d.MemGate.CheckEvery > 0 && submitted%d.MemGate.CheckEvery == 0 {
			if err := d.MemGate.Wait(ctx); err != nil {
				return fmt.Errorf("replay: mem gate at block %d: %w", p.Number, err)
			}
		}
		if submitted%100 == 0 {
			slog.Info("replay progress", "submitted", submitted, "skipped", skipped, "block", p.Number)
		}
	}

	slog.Info("replay complete", "submitted", submitted, "skipped", skipped)

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

// headBlockNumber returns the EL's current head block number via eth_blockNumber,
// used to skip already-applied payloads on resume.
func (d *Driver) headBlockNumber(ctx context.Context) (uint64, error) {
	var hexNum string
	if err := d.Client.Call(ctx, "eth_blockNumber", []any{}, &hexNum); err != nil {
		return 0, err
	}
	return hexutil.DecodeUint64(hexNum)
}

// forceFlushHead re-issues forkchoiceUpdated for the last applied head with no
// payload attributes. Re-asserting the canonical/finalized head drives the EL's
// state-persist pipeline forward while the mem gate is paused and no new blocks
// are arriving, so its dirty diff-layer/journal buffer drains instead of merely
// stalling. The head already exists, so a transport error is surfaced (worth a
// log) but a non-VALID status is not treated as fatal.
func (d *Driver) forceFlushHead(ctx context.Context) error {
	fcs := engine.ForkchoiceStateV1{
		HeadBlockHash:      d.lastHead,
		SafeBlockHash:      d.lastHead,
		FinalizedBlockHash: d.lastHead,
	}
	var fcuResp engine.ForkChoiceResponse
	if err := d.Client.Call(ctx, "engine_forkchoiceUpdatedV3", []any{fcs, nil}, &fcuResp); err != nil {
		return fmt.Errorf("replay: flush-nudge forkchoiceUpdated head %s: %w", d.lastHead.Hex(), err)
	}
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

	// newPayload is idempotent, and a busy EL legitimately times out on it: geth
	// caps engine-API payload insertion at ~8s while it is still importing the
	// previous heavy block (-32002 "request timed out"), and a client-side HTTP
	// timeout means the same thing. Retry with a wait instead of dying — the
	// import completes and the resubmission succeeds.
	var status engine.PayloadStatusV1
	var err error
	for attempt := 0; ; attempt++ {
		err = d.Client.Call(ctx, "engine_newPayloadV4", []any{
			p,                 // ExecutionPayloadV3 == engine.ExecutableData
			[]hexutil.Bytes{}, // blobVersionedHashes
			common.Hash{},     // parentBeaconBlockRoot (zero)
			[]hexutil.Bytes{}, // executionRequests
		}, &status)
		if err == nil || !isTimeout(err) || attempt >= 60 {
			break
		}
		slog.Warn("replay: newPayload timed out, EL busy — retrying", "block", p.BlockHash.Hex(), "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
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
	for attempt := 0; ; attempt++ {
		err = d.Client.Call(ctx, "engine_forkchoiceUpdatedV3", []any{fcs, nil}, &fcuResp)
		if err == nil || !isTimeout(err) || attempt >= 60 {
			break
		}
		slog.Warn("replay: forkchoiceUpdated timed out, EL busy — retrying", "block", p.BlockHash.Hex(), "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("replay: engine_forkchoiceUpdatedV3 block %s: %w", p.BlockHash.Hex(), err)
	}
	if fcuResp.PayloadStatus.Status != "VALID" {
		return fmt.Errorf("%w: block %s forkchoiceUpdated status=%s",
			ErrReplayInvalid, p.BlockHash.Hex(), fcuResp.PayloadStatus.Status)
	}

	return nil
}

// isTimeout reports whether err is a busy-EL timeout worth retrying: the engine
// API's -32002 "request timed out" or a client-side HTTP deadline.
func isTimeout(err error) bool {
	s := err.Error()
	return strings.Contains(s, "request timed out") ||
		strings.Contains(s, "Client.Timeout") ||
		strings.Contains(s, "context deadline exceeded")
}

// equalHex compares two 0x-prefixed hex strings case-insensitively after
// stripping the prefix.
func equalHex(a, b string) bool {
	return strings.EqualFold(
		strings.TrimPrefix(a, "0x"),
		strings.TrimPrefix(b, "0x"),
	)
}
