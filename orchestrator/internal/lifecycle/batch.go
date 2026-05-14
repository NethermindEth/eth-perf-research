package lifecycle

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
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
//
// reserveMu serialises the (seqID, nonce, salt) reservation triple so that
// seqID monotonicity exactly matches nonce-range monotonicity. Without the
// mutex, two atomic.Add calls on separate counters can interleave (planner A
// claims seqID=5 then is preempted; planner B claims seqID=6 and races ahead
// to reserve a lower nonce). seqCounter is the dense, gap-free counter used
// for commit-ordering.
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
	reserveMu    *sync.Mutex
	seqCounter   *atomic.Uint64
}

// batchResult is the outcome of one iteration's work.
type batchResult struct {
	committed   bool
	txCount     int
	rlpBytes    uint64
	blockHeader *rpc.BlockHeader
	postObs     *controller.Observation
}

// updateFeePolicy refreshes the facade context's EIP-1559 fee policy and
// block gas limit from the latest block. Safe for concurrent callers: stores
// happen through atomic accessors (SetFeePolicy / BlockGasLimit is updated
// only on the commit goroutine — see note below).
//
// Concurrency note: BlockGasLimit is plain uint64 and could race when several
// planner goroutines call updateFeePolicy at once. The values written are
// identical for the same head block, so the race is benign (same-value writes),
// but to keep the race detector happy and avoid relying on Intel guarantees
// the lifecycle invokes updateFeePolicy only from the commit goroutine when
// running with cfg.Planners>1. Currently every planner calls it
// pre-dispatch; same-block multi-write tolerated.
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
	fctx.SetFeePolicy(maxFee, tip)
	if head.GasLimit > 0 {
		fctx.SetBlockGasLimit(head.GasLimit)
	}
	return head, nil
}

// dispatched carries the unsigned state produced by dispatchBatch into
// commitBatch. It is the unit of work flowing between the pipeline goroutines.
//
// All cursor values that buildRecord needs are snapshotted here so commit
// never reads back from facadeCtx (which the planner is concurrently mutating
// for the next batch).
//
// seqID is a strictly monotonic, gap-free counter assigned at the same atomic
// step as the nonce-range reservation. It guarantees that the commit goroutine
// can replay batches in nonce order (= chain-accepted order) regardless of the
// arbitrary order in which planner goroutines finish their work. `skip` marks
// a sentinel record produced when a planner reserved a seqID but then failed
// to dispatch (e.g. dispatch error). Commit treats skip=true as "advance the
// next-seq counter, do not commit". Without it, a missing seqID would block
// the commit goroutine forever.
type dispatched struct {
	seqID      uint64
	skip       bool
	batchID    uint64
	plan       *controller.BatchPlan
	addrBefore uint64
	addrAfter  uint64
	saltBefore uint64
	saltAfter  uint64
	res        *facade.Result // signed RLPs, hashes, cursors-after
}

// dispatchBatch performs Pick + fee update + Dispatch. Atomically reserves the
// (seqID, nonce, salt) triple from a single critical section so multiple
// planner goroutines may execute concurrently without nonce collisions AND
// without seqID/nonce-order skew.
//
// Returns nil if Pick yields no plan or Dispatch yields zero txs. Returns a
// non-nil seqID via the returned dispatched so the caller can emit a `skip`
// sentinel if the dispatch later errors out.
func dispatchBatch(ctx context.Context, d *batchDeps, batchID uint64, currentObs *controller.Observation) (*dispatched, error) {
	plan := d.state.Pick(currentObs, d.target, defaultTotalBatchBytes, d.facadeCtx.LoadBlockGasLimit())
	if plan == nil {
		return nil, nil
	}

	if _, err := updateFeePolicy(ctx, d.rpc, d.facadeCtx); err != nil {
		return nil, err
	}

	// Atomic reservation: pin disjoint nonce and salt ranges + assign a
	// monotone seqID inside one critical section. The seqID is the ordering
	// key the commit goroutine uses to replay batches in nonce order.
	//
	// We always reserve plan.NMaxTxs slots even though Dispatch may refuse
	// the batch (e.g. zero-tx response) — wasted slots are abandoned (salt
	// domain is 2^64; nonce holes are converted into a hard error by the
	// nonce-hole assertion below).
	want := uint64(plan.NMaxTxs)
	if want == 0 {
		want = 1
	}
	d.reserveMu.Lock()
	seqID := d.seqCounter.Add(1) - 1
	addrBefore := d.facadeCtx.ReserveAddresses(want)
	saltBefore := d.facadeCtx.ReserveSalts(want)
	d.reserveMu.Unlock()

	res, err := d.dispatcher.Dispatch(ctx, facade.DispatchInput{
		Plan:       plan,
		StartNonce: addrBefore,
		StartSalt:  saltBefore,
		NumNonces:  want,
	}, d.facadeCtx)
	if err != nil {
		// seqID was already claimed under the reservation mutex. Return a
		// skip sentinel so the commit goroutine can advance past this seqID
		// without waiting forever. Reserved nonces leak (the chain will
		// reject the next batch with a "nonce too high" error) — the planner
		// is expected to setTerm and shut the run down.
		return &dispatched{seqID: seqID, skip: true}, fmt.Errorf("lifecycle: dispatch: %w", err)
	}
	if res == nil || len(res.SignedRLP) == 0 {
		// Same rationale as above — surface a skip so the queue drains.
		return &dispatched{seqID: seqID, skip: true}, nil
	}

	// Refuse to send a batch with nonce holes. If trimSignables dropped tail
	// txs, the reservation contains unused nonces that would stall the chain
	// (gap-resistant mempools reject the next batch's first tx). Fail the batch
	// loudly so the operator notices; the planner will give up and shut down.
	if uint64(res.TxCount) != want {
		return &dispatched{seqID: seqID, skip: true}, fmt.Errorf("lifecycle: nonce-range hole: reserved %d, signed %d (verb=%s, deadline=%d)",
			want, res.TxCount, plan.Verb, plan.DeadlineBytes)
	}

	return &dispatched{
		seqID:      seqID,
		batchID:    batchID,
		plan:       plan,
		addrBefore: addrBefore,
		addrAfter:  res.NewCursor,
		saltBefore: saltBefore,
		saltAfter:  res.NewSalt,
		res:        res,
	}, nil
}

// commitBatch consumes a dispatched batch: writes the pending sidecar, commits
// via testing_commitBlockV1, fetches the block, appends the execution payload,
// polls the sensor for forward progress, runs controller.Apply, journals the
// record, then clears the pending sidecar. Returns the post-commit observation
// and residual snapshot baked into a batchResult.
func commitBatch(ctx context.Context, d *batchDeps, db *dispatched, pre *controller.Observation) (*batchResult, error) {
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

	blockTS := uint64(time.Now().Unix())
	blockHash, err := d.rpc.TestingCommitBlockV1(ctx, db.res.SignedRLP, blockTS)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: testing_commitBlockV1: %w", err)
	}

	block, err := d.rpc.BlockByHash(ctx, blockHash, false)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: BlockByHash %s: %w", blockHash.Hex(), err)
	}

	payload := buildExecutionPayloadV3(block, db.res.SignedRLP)
	if err := d.pw.Append(payload); err != nil {
		return nil, fmt.Errorf("lifecycle: append payload: %w", err)
	}

	// Wait for the sensor to advance past the previous observation. The new
	// snapshot is what the controller consumes; we no longer need a stale-tolerate
	// workaround because ReadAfter returns on first forward-progress poll.
	preBN := uint64(0)
	if pre != nil {
		preBN = pre.BlockNumber
	}
	// Statecomp plugin batches diffs (~30k blocks per baseline rotation), so the
	// per-batch sensor blockNumber won't advance. Do a single poll — accept
	// whatever's there, never block. The controller's F-update tolerates stale
	// observations; new data lands when the plugin rotates.
	snap, err := waitForValidSensor(ctx, d.sensor)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: sensor poll: %w", err)
	}
	_ = preBN
	post := &controller.Observation{
		AccountTrieBytes: snap.AccountTrieBytes,
		StorageTrieBytes: snap.StorageTrieBytes,
		CodeBytesTotal:   snap.CodeBytesTotal,
		BlockNumber:      snap.BlockNumber,
	}

	residual, err := d.state.Apply(pre, post, db.plan, db.res.TxCount, db.res.RLPBytes)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: controller.Apply: %w", err)
	}

	rec := buildRecord(d, db.batchID, db.plan, db.res, block, blockTS, db.saltBefore, db.saltAfter, snap, residual, db.addrBefore)
	if _, err := d.jw.Append(rec); err != nil {
		return nil, fmt.Errorf("lifecycle: journal append: %w", err)
	}
	if err := journal.ClearPending(d.pendingPath); err != nil {
		return nil, fmt.Errorf("lifecycle: clear pending: %w", err)
	}

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

// runOneBatch is the sequential composition of dispatchBatch + commitBatch.
// It is kept as a fallback path; the pipelined runLoop calls dispatchBatch and
// commitBatch on separate goroutines.
func runOneBatch(ctx context.Context, d *batchDeps, batchID uint64, currentObs *controller.Observation) (*batchResult, error) {
	db, err := dispatchBatch(ctx, d, batchID, currentObs)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return &batchResult{}, nil
	}
	return commitBatch(ctx, d, db, currentObs)
}

// buildRecord assembles the on-disk Record for one committed batch.
//
// All facadeCtx cursor reads come from the dispatched snapshot
// (saltBefore/saltAfter, addrBefore) so this is safe to call while the planner
// goroutine has already advanced facadeCtx for the next batch.
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
