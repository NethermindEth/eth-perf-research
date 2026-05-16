package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/metrics"
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
	metricsReg   *metrics.Registry
}

// batchResult is the outcome of one iteration's work.
type batchResult struct {
	committed   bool
	txCount     int
	rlpBytes    uint64
	blockHeader *rpc.BlockHeader
	postObs     *controller.Observation
}

// dispatched carries the unsigned state produced by dispatchBatch into
// commitBatch.
//
// All cursor values that buildRecord needs are snapshotted here so commit
// reads a stable view independent of any later facadeCtx mutation.
//
// pre is the observation Pick consumed for this batch; the committer threads
// it into Apply as the batch's pre-commit baseline. timing carries the
// pick/build/sign phase durations measured in the planner goroutine; the
// committer fills the remaining phases before emitting it.
type dispatched struct {
	batchID    uint64
	plan       *controller.BatchPlan
	addrBefore uint64
	addrAfter  uint64
	saltBefore uint64
	saltAfter  uint64
	res        *facade.Result // signed RLPs, hashes, cursors-after
	pre        *controller.Observation
	timing     phaseTimings
}

// dispatchBatch performs Pick + Dispatch and reserves the nonce/salt ranges.
//
// Returns nil if Pick yields no plan or Dispatch yields zero txs. The single
// planner produces batches strictly in order, so no commit-ordering key is
// needed; a dispatch error is simply propagated for the caller to skip.
//
// Runs in the planner goroutine. The pick phase reads controller State under
// the State mutex (Apply, in the committer goroutine, holds the same mutex);
// build+sign are pure CPU and run lock-free. pick/build/sign durations are
// recorded into the returned dispatched.timing.
func dispatchBatch(ctx context.Context, d *batchDeps, batchID uint64, currentObs *controller.Observation) (*dispatched, error) {
	pickStart := time.Now()
	d.state.LockState()
	plan := d.state.Pick(currentObs, d.target, defaultTotalBatchBytes, d.facadeCtx.LoadBlockGasLimit())
	d.state.UnlockState()
	pickDur := time.Since(pickStart)
	if plan == nil {
		return nil, nil
	}

	// Reserve a contiguous range of exactly plan.NMaxTxs nonce/salt slots.
	// We always reserve plan.NMaxTxs slots even though Dispatch may refuse
	// the batch (e.g. zero-tx response) — wasted slots are abandoned (salt
	// domain is 2^64; nonce holes are converted into a hard error by the
	// nonce-hole assertion below).
	want := uint64(plan.NMaxTxs)
	if want == 0 {
		want = 1
	}
	addrBefore := d.facadeCtx.ReserveAddresses(want)
	saltBefore := d.facadeCtx.ReserveSalts(want)

	res, err := d.dispatcher.Dispatch(ctx, facade.DispatchInput{
		Plan:       plan,
		StartNonce: addrBefore,
		StartSalt:  saltBefore,
		NumNonces:  want,
	}, d.facadeCtx)
	if err != nil {
		// The planner skips this batch and continues; reserved nonces leak,
		// but the chain's next batch reconciles them.
		return nil, fmt.Errorf("lifecycle: dispatch: %w", err)
	}
	if res == nil || len(res.SignedRLP) == 0 {
		return nil, nil
	}

	// Refuse to send a batch with nonce holes. If trimSignables dropped tail
	// txs, the reservation contains unused nonces that would stall the chain
	// (gap-resistant mempools reject the next batch's first tx). Fail the batch
	// loudly so the operator notices; the planner skips it and continues.
	if uint64(res.TxCount) != want {
		return nil, fmt.Errorf("lifecycle: nonce-range hole: reserved %d, signed %d (verb=%s, deadline=%d)",
			want, res.TxCount, plan.Verb, plan.DeadlineBytes)
	}

	return &dispatched{
		batchID:    batchID,
		plan:       plan,
		addrBefore: addrBefore,
		addrAfter:  res.NewCursor,
		saltBefore: saltBefore,
		saltAfter:  res.NewSalt,
		res:        res,
		pre:        currentObs,
		timing: phaseTimings{
			pick:  pickDur,
			build: res.BuildDuration,
			sign:  res.SignDuration,
		},
	}, nil
}

// commitBatch consumes a dispatched batch: writes the pending sidecar, commits
// via testing_commitBlockV1, fetches the block, appends the execution payload,
// polls the sensor for forward progress, runs controller.Apply, journals the
// record, then clears the pending sidecar. Returns the post-commit observation
// and residual snapshot baked into a batchResult.
//
// Runs in the committer goroutine. The commit/sensor/apply/journal phase
// durations are recorded into db.timing (the planner already filled
// pick/build/sign). The Apply call holds the State mutex so it does not race
// with the planner's concurrent Pick. pre is db.pre — the observation Pick
// consumed — threaded in as the batch's pre-commit baseline.
func commitBatch(ctx context.Context, d *batchDeps, db *dispatched) (*batchResult, error) {
	pre := db.pre
	pending := &orchpb.PendingBatch{
		BatchId:          db.batchID,
		Verb:             db.plan.Verb,
		StartAddress:     db.addrBefore,
		Count:            uint64(db.res.TxCount),
		TargetSha256:     d.targetDigest,
		SessionId:        d.sessionID,
		SaltCursorBefore: db.saltBefore,
		SignedTxHashes:   db.res.TxRLPHashes,
		TsIso:            time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := journal.WritePending(d.pendingPath, pending); err != nil {
		return nil, fmt.Errorf("lifecycle: write pending: %w", err)
	}

	commitStart := time.Now()
	blockTS := uint64(time.Now().Unix())
	blockHash, err := d.rpc.TestingCommitBlockV1(ctx, db.res.SignedRLP, blockTS)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: testing_commitBlockV1: %w", err)
	}

	block, err := d.rpc.BlockByHash(ctx, blockHash, false)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: BlockByHash %s: %w", blockHash.Hex(), err)
	}
	db.timing.commit = time.Since(commitStart)

	// Statecomp plugin batches diffs (~30k blocks per baseline rotation), so the
	// per-batch sensor blockNumber won't advance. Do a single poll — accept
	// whatever's there, never block. The controller's F-update tolerates stale
	// observations; new data lands when the plugin rotates.
	sensorStart := time.Now()
	snap, err := waitForValidSensor(ctx, d.sensor)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: sensor poll: %w", err)
	}
	db.timing.sensor = time.Since(sensorStart)
	post := &controller.Observation{
		AccountTrieBytes: snap.AccountTrieBytes,
		StorageTrieBytes: snap.StorageTrieBytes,
		CodeBytesTotal:   snap.CodeBytesTotal,
		BlockNumber:      snap.BlockNumber,
	}

	applyStart := time.Now()
	d.state.LockState()
	residual, err := d.state.Apply(pre, post, db.plan, db.res.TxCount, db.res.RLPBytes, block.GasUsed)
	d.state.UnlockState()
	if err != nil {
		return nil, fmt.Errorf("lifecycle: controller.Apply: %w", err)
	}
	db.timing.apply = time.Since(applyStart)

	journalStart := time.Now()
	payload := buildExecutionPayloadV3(block, db.res.SignedRLP)
	if err := d.pw.Append(payload); err != nil {
		return nil, fmt.Errorf("lifecycle: append payload: %w", err)
	}
	rec := buildRecord(d, db.batchID, db.plan, db.res, block, blockTS, db.saltBefore, db.saltAfter, snap, residual, db.addrBefore)
	if _, err := d.jw.Append(rec); err != nil {
		return nil, fmt.Errorf("lifecycle: journal append: %w", err)
	}
	if err := journal.ClearPending(d.pendingPath); err != nil {
		return nil, fmt.Errorf("lifecycle: clear pending: %w", err)
	}
	db.timing.journal = time.Since(journalStart)

	db.timing.observe(d.metricsReg, db.batchID, db.res.TxCount)

	// Update Prometheus metrics after a successful commit.
	if d.metricsReg != nil {
		m := d.metricsReg
		m.JournalRecordsTotal.Inc()
		m.JournalGasUsedTotal.Add(float64(block.GasUsed))
		m.JournalVerbTotal.WithLabelValues(db.plan.Verb).Inc()
		m.JournalStatusTotal.WithLabelValues("ok").Inc()
		m.BatchID.Set(float64(db.batchID))
		m.BatchGasUsed.Set(float64(block.GasUsed))
		m.BlockNumber.Set(float64(block.Number))
		m.DeadlineBytes.Set(float64(db.plan.DeadlineBytes))
		m.ResidualNorm.Set(residual.L2Norm)
		m.ObservedFlatBytes.WithLabelValues("accounts").Set(float64(post.AccountTrieBytes))
		m.ObservedFlatBytes.WithLabelValues("storage").Set(float64(post.StorageTrieBytes))
		m.ObservedFlatBytes.WithLabelValues("code").Set(float64(post.CodeBytesTotal))
		for verb, w := range db.plan.Mix {
			m.MixSimplex.WithLabelValues(verb).Set(w)
		}
		for verb, row := range d.state.AlphaSnapshot() {
			for ax, v := range row {
				m.AlphaCurrent.WithLabelValues(verb, string(ax)).Set(v)
			}
		}
		for verb, row := range d.state.SigmaSnapshot() {
			for ax, v := range row {
				m.AlphaState.WithLabelValues(verb, string(ax)).Set(v)
			}
		}
	}

	return &batchResult{
		committed:   true,
		txCount:     db.res.TxCount,
		rlpBytes:    db.res.RLPBytes,
		blockHeader: block,
		postObs:     post,
	}, nil
}

// buildRecord assembles the on-disk Record for one committed batch.
//
// All facadeCtx cursor reads come from the dispatched snapshot
// (saltBefore/saltAfter, addrBefore) so this is unaffected by any later
// facadeCtx advance for the next batch.
func buildRecord(
	d *batchDeps,
	batchID uint64,
	plan *controller.BatchPlan,
	res *facade.Result,
	block *rpc.BlockHeader,
	blockTS uint64,
	saltBefore uint64,
	saltAfter uint64,
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
			SaltCursorAfter:  saltAfter,
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
