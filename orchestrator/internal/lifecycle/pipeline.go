package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/target"
)

// The planner idle backoff, dispatch-skip-streak halt and rejection-streak
// halt were package-level constants here; they now live in
// config.RunConfig.Run and are read off cfg.Run (the resolved RunConfig
// carried on Config). config.Defaults() holds the historical values.

// pipeline holds the state shared between the planner and committer goroutines
// of the depth-1 build-ahead loop.
//
// ready is the depth-1 hand-off channel: the planner sends prepared batches,
// the committer receives them. The buffer of one lets build+sign of batch N+1
// overlap commit+sensor+apply+journal of batch N. The planner is the sole
// sender and closes the channel when it stops producing; the committer drains
// until close.
type pipeline struct {
	ready        chan *dispatched
	startBatchID uint64

	// obsMu guards obs and last. The committer writes both after a successful
	// commit; the planner reads obs for its termination predicates and as the
	// Pick input. A one-batch lag here is by design.
	obsMu sync.Mutex
	obs   *controller.Observation
	last  *rpc.BlockHeader

	// termMu guards reason; first writer wins so the genuine cause is kept.
	termMu sync.Mutex
	reason string
}

// setTerm records the first termination reason. Subsequent calls are ignored.
func (p *pipeline) setTerm(reason string) {
	p.termMu.Lock()
	if p.reason == "" {
		p.reason = reason
	}
	p.termMu.Unlock()
}

// termReason returns the recorded termination reason (empty if none).
func (p *pipeline) termReason() string {
	p.termMu.Lock()
	defer p.termMu.Unlock()
	return p.reason
}

// loadObs returns the latest committed observation.
func (p *pipeline) loadObs() *controller.Observation {
	p.obsMu.Lock()
	defer p.obsMu.Unlock()
	return p.obs
}

// storeObs records the post-commit observation and last block.
func (p *pipeline) storeObs(obs *controller.Observation, block *rpc.BlockHeader) {
	p.obsMu.Lock()
	p.obs = obs
	p.last = block
	p.obsMu.Unlock()
}

// lastBlock returns the most recently committed block header (or nil).
func (p *pipeline) lastBlock() *rpc.BlockHeader {
	p.obsMu.Lock()
	defer p.obsMu.Unlock()
	return p.last
}

// planner is the build-ahead goroutine: it Picks, builds and signs batches in
// strict batchID order and sends each prepared batch on p.ready. It owns the
// batchID counter and the nonce/salt cursor advance (single planner ⇒ no
// cursor race). It stops — closing p.ready — on cancellation or a terminal
// predicate (instability, target reached, batch limit, dispatch-skip streak),
// cancelling loopCtx so the committer also winds down.
func (p *pipeline) planner(
	loopCtx context.Context,
	cancel context.CancelFunc,
	cfg Config,
	deps *batchDeps,
	ctrlTarget **controller.Target,
	targetCh chan *target.Target,
	mf *manifest.Manifest,
) {
	defer close(p.ready)

	batchID := p.startBatchID
	consecutiveDispatchSkips := 0

	for {
		// 1. Respect cancellation, then drain target reloads.
		select {
		case <-loopCtx.Done():
			p.setTerm(terminationSignal)
			return
		default:
		}
		select {
		case newT := <-targetCh:
			_, newCtrl, _ := refreshTarget(newT)
			*ctrlTarget = newCtrl
			deps.target = newCtrl
			deps.targetDigest = targetSha256Bytes(newT.SHA256)
			mf.TargetHistory = append(mf.TargetHistory, manifest.TargetEntry{
				AppliedAtBatch: int(batchID),
				AppliedAtISO:   time.Now().UTC().Format(time.RFC3339Nano),
				TargetSHA256:   newT.SHA256,
				Shares:         cloneShares(newT.Shares),
				TotalBytes:     newT.TotalBytes,
			})
			slog.Info("lifecycle: target reloaded", "sha", newT.SHA256[:8], "total_bytes", newT.TotalBytes)
		default:
		}

		// 2. Termination predicates — checked BEFORE claiming a batchID so we
		// never burn an id on a batch we won't dispatch. obs lags by up to one
		// batch (the committer may not have folded in the latest commit yet);
		// that is acceptable for these coarse stop conditions.
		obs := p.loadObs()
		ck := cfg.Run.Control
		if deps.state.HasInstability(ck.OvershootThreshold, ck.OvershootWindow, ck.OvershootMaxTrips, ck.OvershootGrace) {
			p.setTerm(terminationOvershoot)
			cancel()
			return
		}
		if reachedTarget(obs, *ctrlTarget) {
			p.setTerm(terminationTargetMet)
			cancel()
			return
		}
		if cfg.MaxBatches > 0 && batchID >= uint64(cfg.MaxBatches) {
			p.setTerm(terminationBatchLimit)
			cancel()
			return
		}

		// 3. Pick + build + sign.
		db, err := dispatchBatch(loopCtx, deps, batchID, obs)
		if err != nil {
			if loopCtx.Err() != nil {
				p.setTerm(terminationSignal)
				return
			}
			// A dispatch/build error (gas cap exceeded, oversized batch,
			// builder failure, nonce hole) is batch-local and recoverable:
			// SKIP the batch and continue planning. Only a sustained streak
			// halts so a single bad batch does not kill controller learning.
			consecutiveDispatchSkips++
			slog.Warn("lifecycle: dispatch failed, skipping batch",
				"batch_id", batchID, "streak", consecutiveDispatchSkips, "err", err)
			if consecutiveDispatchSkips >= cfg.Run.Run.DispatchSkipStreakHalt {
				p.setTerm(fmt.Sprintf("error: %d consecutive dispatch failures (last: %s)",
					consecutiveDispatchSkips, err.Error()))
				cancel()
				return
			}
			batchID++
			continue
		}
		consecutiveDispatchSkips = 0
		if db == nil {
			// Pick yielded no plan — back off briefly. batchID is not advanced;
			// the next iteration retries the same id.
			select {
			case <-loopCtx.Done():
				p.setTerm(terminationSignal)
				return
			case <-time.After(time.Duration(cfg.Run.Run.IdleBackoffMS) * time.Millisecond):
			}
			continue
		}

		// 4. Hand the prepared batch to the committer. The depth-1 buffer lets
		// the next iteration's build+sign overlap the committer's work.
		select {
		case <-loopCtx.Done():
			p.setTerm(terminationSignal)
			return
		case p.ready <- db:
		}
		batchID++
	}
}

// committer is the commit goroutine: it receives prepared batches from p.ready,
// commits each via testing_commitBlockV1, polls the sensor, runs controller
// Apply and journals the record. It updates the shared observation the planner
// reads. It runs until p.ready is closed (planner stopped) and drained; a fatal
// commit error or a full-rejection streak cancels loopCtx so the planner also
// stops.
func (p *pipeline) committer(loopCtx context.Context, cancel context.CancelFunc, deps *batchDeps) {
	consecutiveFullRejections := 0

	for db := range p.ready {
		res, err := commitBatch(loopCtx, deps, db)
		if err != nil {
			if loopCtx.Err() != nil {
				p.setTerm(terminationSignal)
				cancel()
				continue
			}
			// A transport error means NM is unreachable (restart / redeploy),
			// not a logical failure. Enter the reconnect-wait loop instead of
			// terminating; the controller State, journal and address/nonce
			// cursor stay in memory, so on reconnect we just move to the next
			// batch. Only a reconnect timeout terminates the run.
			if isTransportError(err) {
				slog.Warn("lifecycle: commit hit transport error — NM unreachable",
					"batch_id", db.batchID, "err", err)
				rcRun := deps.cfg.Run
				if rerr := awaitReconnect(loopCtx, deps.rpc,
					time.Duration(rcRun.ReconnectMaxWaitS)*time.Second,
					time.Duration(rcRun.ReconnectBackoffInitialMS)*time.Millisecond,
					time.Duration(rcRun.ReconnectBackoffMaxMS)*time.Millisecond); rerr != nil {
					if loopCtx.Err() != nil {
						p.setTerm(terminationSignal)
					} else {
						p.setTerm(terminationNMUnreachable)
					}
					cancel()
					continue
				}
				continue
			}
			if isPartialAcceptanceErr(err) {
				expected, included := parsePartialAcceptance(err)
				slog.Warn("lifecycle: commit partial-acceptance, skipping batch",
					"batch_id", db.batchID,
					"expected", expected,
					"included", included,
					"verb", db.plan.Verb,
				)
				if included == 0 {
					consecutiveFullRejections++
					if consecutiveFullRejections >= deps.cfg.Run.RejectionStreakHalt {
						p.setTerm(fmt.Sprintf("error: %d consecutive batches fully rejected (verb=%s); chain state likely missing dependencies",
							consecutiveFullRejections, db.plan.Verb))
						cancel()
						continue
					}
				} else {
					consecutiveFullRejections = 0
				}
				continue
			}
			slog.Error("lifecycle: commit failed", "batch_id", db.batchID, "err", err)
			p.setTerm("error: " + err.Error())
			cancel()
			continue
		}
		if res != nil && res.committed {
			p.storeObs(res.postObs, res.blockHeader)
			consecutiveFullRejections = 0
		}
	}
}
