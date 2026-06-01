package lifecycle

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/config"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/metrics"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
)

// batchDeps groups the live subsystems threaded through batch execution. cfg
// is the resolved RunConfig — dispatchBatch reads the batch-byte budget off it.
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
	cfg          config.RunConfig
	// lastBlockTS is a shared monotonic counter ensuring committed blocks have
	// strictly-increasing timestamps. testing_commitBlockV1 runs with
	// NoValidation and accepts ts == parent.ts, but Geth and the normal Engine
	// API require ts > parent.ts; without this, ~3 blocks/sec at 1s resolution
	// share a timestamp and the recorded payloads become un-replayable. It is a
	// POINTER so the bootstrap deps and the main loop deps share one counter.
	lastBlockTS *atomic.Uint64
}

// nextBlockTS returns a strictly-increasing block timestamp. It uses wall-clock
// seconds when those advance past the last emitted value, and bumps by +1 only
// to break a tie within the same wall-clock second.
func nextBlockTS(last *atomic.Uint64) uint64 {
	now := uint64(time.Now().Unix())
	for {
		prev := last.Load()
		ts := now
		if ts <= prev {
			ts = prev + 1
		}
		if last.CompareAndSwap(prev, ts) {
			return ts
		}
	}
}

// batchResult is the outcome of one iteration's work.
type batchResult struct {
	committed   bool
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
	// Pick receives the hard byte CAP (historically the defaultTotalBatchBytes
	// const, 7680 KiB) as its deadline_bytes ceiling — not the soft
	// total_batch_bytes budget. Behaviour matches the pre-RunConfig const.
	plan := d.state.Pick(currentObs, d.target, d.cfg.Run.TotalBatchBytesCap, d.facadeCtx.LoadBlockGasLimit())
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
	blockTS := nextBlockTS(d.lastBlockTS)
	blockHash, err := d.rpc.TestingCommitBlockV1(ctx, db.res.SignedRLP, blockTS)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: testing_commitBlockV1: %w", err)
	}

	block, err := d.rpc.BlockByHash(ctx, blockHash, false)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: BlockByHash %s: %w", blockHash.Hex(), err)
	}
	db.timing.commit = time.Since(commitStart)

	// Sensor-paced: block until the StateComposition trie-diff has folded THIS
	// committed block into the sensor. The loop must hold fresh sensor data for
	// block N before it Picks N+1, so the per-block trie-diff can never fall
	// behind. If a baseline rescan is in progress the sensor stalls;
	// waitSensorForBlock waits it out rather than racing ahead.
	sensorStart := time.Now()
	snap, err := waitSensorForBlock(ctx, d.sensor, block.Number,
		time.Duration(d.cfg.Run.SensorLogIntervalS)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: sensor wait: %w", err)
	}
	db.timing.sensor = time.Since(sensorStart)
	post := &controller.Observation{
		AccountTrieBytes: snap.AccountTrieBytes,
		StorageTrieBytes: snap.StorageTrieBytes,
		CodeBytesTotal:   snap.CodeBytesTotal,
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
				m.AlphaCurrent.WithLabelValues(verb, ax.String()).Set(v)
			}
		}
		for verb, row := range d.state.SigmaSnapshot() {
			for ax, v := range row {
				m.AlphaState.WithLabelValues(verb, ax.String()).Set(v)
			}
		}
	}

	return &batchResult{
		committed:   true,
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
			CoeffsAfter:        flatFromAxisMap(d.state.FSnapshot()),
			AlphaCurrent:       flatFromAxisMap(d.state.AlphaSnapshot()),
			SigmaInnov:         flatFromAxisMap(d.state.SigmaSnapshot()),
			MixSimplex:         mix,
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
