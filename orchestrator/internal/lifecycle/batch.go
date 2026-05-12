package lifecycle

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
)

// defaultPriorityTipWei is the fixed 1 gwei suggested priority tip for the
// orchestrator's signed txs. Matches the Python lifecycle.
const defaultPriorityTipWei = 1_000_000_000

// batchDeps groups the live subsystems threaded through batch execution.
type batchDeps struct {
	rpc          *rpc.Client
	sensor       *sensor.Sensor
	dispatcher   *facade.Dispatcher
	jw           *journal.Writer
	pw           *payloads.Writer
	state        *controller.State
	facadeCtx    *facade.Context
	pendingPath  string
	sessionID    string
	resumedFrom  uint64
	target       *controller.Target
	targetDigest []byte
}

// batchResult is the outcome of one iteration's work.
type batchResult struct {
	committed   bool
	txCount     int
	rlpBytes    uint64
	blockHeader *rpc.BlockHeader
	postObs     *controller.Observation
}

// updateFeePolicy refreshes ctx.MaxFeePerGas / MaxPriorityFeePerGas from the
// latest block's baseFeePerGas. Returns the latest block.
func updateFeePolicy(ctx context.Context, rpcCli *rpc.Client, fctx *facade.Context) (*rpc.BlockHeader, error) {
	head, err := rpcCli.BlockByNumber(ctx, -1)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: head for fee policy: %w", err)
	}
	baseFee := head.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	tip := big.NewInt(defaultPriorityTipWei)
	// max_fee = baseFee*2 + tip (matches Python heuristic).
	maxFee := new(big.Int).Mul(baseFee, big.NewInt(2))
	maxFee.Add(maxFee, tip)
	fctx.MaxFeePerGas = maxFee
	fctx.MaxPriorityFeePerGas = tip
	if head.GasLimit > 0 {
		fctx.BlockGasLimit = head.GasLimit
	}
	return head, nil
}

// runOneBatch executes a single iteration of the main loop.
func runOneBatch(ctx context.Context, d *batchDeps, batchID uint64, currentObs *controller.Observation) (*batchResult, error) {
	plan := d.state.Pick(currentObs, d.target, defaultTotalBatchBytes)
	if plan == nil {
		return &batchResult{}, nil
	}

	if _, err := updateFeePolicy(ctx, d.rpc, d.facadeCtx); err != nil {
		return nil, err
	}

	saltBefore := d.facadeCtx.SaltCursor
	addrBefore := d.facadeCtx.AddressCursor

	res, err := d.dispatcher.Dispatch(ctx, plan, d.facadeCtx)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: dispatch: %w", err)
	}
	if res == nil || len(res.SignedRLP) == 0 {
		return &batchResult{}, nil
	}

	// Sidecar BEFORE commit so a crash leaves a recoverable marker.
	pending := &orchpb.PendingBatch{
		BatchId:          batchID,
		Verb:             plan.Verb,
		StartAddress:     addrBefore,
		Count:            uint64(res.TxCount),
		TargetSha256:     d.targetDigest,
		SessionId:        d.sessionID,
		SaltCursorBefore: saltBefore,
		SignedTxHashes:   res.TxRLPHashes,
		TsIso:            time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := journal.WritePending(d.pendingPath, pending); err != nil {
		return nil, fmt.Errorf("lifecycle: write pending: %w", err)
	}

	pre := currentObs
	blockTS := uint64(time.Now().Unix())

	blockHash, err := d.rpc.TestingCommitBlockV1(ctx, res.SignedRLP, blockTS)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: testing_commitBlockV1: %w", err)
	}

	block, err := d.rpc.BlockByHash(ctx, blockHash, true)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: BlockByHash %s: %w", blockHash.Hex(), err)
	}

	payload := buildExecutionPayloadV3(block, res.SignedRLP)
	if err := d.pw.Append(payload); err != nil {
		return nil, fmt.Errorf("lifecycle: append payload: %w", err)
	}

	snap, err := d.sensor.Read(ctx, block.Number)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: sensor read block=%d: %w", block.Number, err)
	}
	post := &controller.Observation{
		AccountTrieBytes: snap.AccountTrieBytes,
		StorageTrieBytes: snap.StorageTrieBytes,
		CodeBytesTotal:   snap.CodeBytesTotal,
		BlockNumber:      snap.BlockNumber,
	}

	residual, err := d.state.Apply(pre, post, plan, res.TxCount, res.RLPBytes)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: controller.Apply: %w", err)
	}

	rec := buildRecord(d, batchID, plan, res, block, blockTS, saltBefore, snap, residual, addrBefore)
	if _, err := d.jw.Append(rec); err != nil {
		return nil, fmt.Errorf("lifecycle: journal append: %w", err)
	}
	if err := journal.ClearPending(d.pendingPath); err != nil {
		return nil, fmt.Errorf("lifecycle: clear pending: %w", err)
	}

	return &batchResult{
		committed:   true,
		txCount:     res.TxCount,
		rlpBytes:    res.RLPBytes,
		blockHeader: block,
		postObs:     post,
	}, nil
}

// buildRecord assembles the on-disk Record for one committed batch.
func buildRecord(
	d *batchDeps,
	batchID uint64,
	plan *controller.BatchPlan,
	res *facade.Result,
	block *rpc.BlockHeader,
	blockTS uint64,
	saltBefore uint64,
	snap *sensor.Snapshot,
	residual *controller.ResidualSnapshot,
	addrBefore uint64,
) *orchpb.Record {
	addrAfter := addrBefore + uint64(res.TxCount)
	mix := make(map[string]float64, len(plan.Mix))
	for k, v := range plan.Mix {
		mix[k] = v
	}

	return &orchpb.Record{
		SessionId:        d.sessionID,
		ResumedFromBatch: d.resumedFrom,
		TsIso:            time.Now().UTC().Format(time.RFC3339Nano),
		BatchId:          batchID,
		ReplayCore: &orchpb.ReplayCore{
			Schema:           1,
			Verb:             plan.Verb,
			DeadlineBytes:    uint64(plan.DeadlineBytes),
			StartAddress:     encodeAddr(addrBefore),
			EndAddress:       encodeAddr(addrAfter),
			Status:           "ok",
			BlockHash:        block.Hash.Bytes(),
			BlockNumber:      block.Number,
			BlockTimestamp:   blockTS,
			TargetSha256:     d.targetDigest,
			SaltCursorBefore: saltBefore,
			SaltCursorAfter:  d.facadeCtx.SaltCursor,
		},
		Observability: &orchpb.Observability{
			ObservedFlat:       snap.Raw,
			StatecompSnapshot:  snap.Raw,
			ResidualNorm:       residual.L2Norm,
			GasUsed:            block.GasUsed,
			VerbGasFactors:     cloneFloatMap(d.facadeCtx.VerbGasFactors),
			CoeffsAfter:        flatFromAxisMap(d.state.F),
			AlphaCurrent:       flatFromAxisMap(d.state.Alpha),
			SigmaInnov:         flatFromAxisMap(d.state.Sigma),
			MixSimplex:         mix,
			Epsilon:            d.state.Epsilon,
			TxCount:            uint32(res.TxCount),
			DispatchedRlpBytes: res.RLPBytes,
		},
	}
}

// encodeAddr serialises an address-cursor into 8 big-endian bytes for the
// ReplayCore start_address / end_address fields.
func encodeAddr(cursor uint64) []byte {
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = byte(cursor & 0xff)
		cursor >>= 8
	}
	return out
}

func cloneFloatMap(in map[string]float64) map[string]float64 {
	if in == nil {
		return map[string]float64{}
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
