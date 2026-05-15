// Package lifecycle wires every subsystem into the orchestrator's main loop.
//
// It owns the run-level invariants:
//   - exclusive state-dir lock,
//   - chain identity computation,
//   - fresh-vs-resume decision,
//   - journal/payloads/manifest persistence,
//   - shutdown / termination accounting.
//
// The hot loop lives in runLoop and delegates per-iteration work to runOneBatch.
package lifecycle

import (
	"container/heap"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/builderpool"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/facade"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/healthd"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/journal"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/lock"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/manifest"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/metrics"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mode"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/signer"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/target"
)

// batchQueue is a min-heap over *dispatched ordered by seqID. The commit
// goroutine uses it to replay batches in nonce-correct order regardless of
// the (out-of-order) completion order of the planner goroutines.
type batchQueue []*dispatched

func (q batchQueue) Len() int           { return len(q) }
func (q batchQueue) Less(i, j int) bool { return q[i].seqID < q[j].seqID }
func (q batchQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *batchQueue) Push(x any)        { *q = append(*q, x.(*dispatched)) }
func (q *batchQueue) Pop() any {
	old := *q
	n := len(old)
	x := old[n-1]
	*q = old[:n-1]
	return x
}

// Config is the top-level configuration for the lifecycle run.
type Config struct {
	RPCURL              string
	JWTPath             string
	StateDir            string
	TargetYAMLPath      string
	GenesisSHA256       string
	ReferenceFPath      string
	ManifestPath        string
	MaxBatches          int
	// Planners is the number of parallel planner goroutines sharing one master
	// signer via atomic nonce reservation. Default 4. Set to 1 for the old
	// sequential pipeline behaviour.
	Planners            int
	EnableProbe         bool
	DeployPrivateKey    string
	BuilderWorkerCmd    []string
	BuilderWorkers      int
	Epsilon             float64
	TotalBatchBytes     int
	SensorPollInterval  time.Duration
	SensorDeadline      time.Duration
	Verbs               []string
	PluginGitSHA        string
	NethermindCommitSHA string
	DotnetRuntimeMajor  string
	MetricsAddr         string // TCP address for the Prometheus /metrics endpoint (default ":9101")
}

const (
	defaultEpsilon         = 0.5
	defaultTotalBatchBytes = 5 * 1024 * 1024 // stay below NM's BlockProductionMaxTxKilobytes=7.75 MiB once per-tx RLP overhead is counted
	defaultAddressStride   = uint64(1) << 40
	terminationOvershoot   = "overshoot"
	terminationTargetMet   = "target_reached"
	terminationBatchLimit  = "batch_limit"
	terminationSignal      = "signal"
	terminationProbeFailed = "probe_failed"
	overshootWindowSize    = 5
	overshootMaxTrips      = 3
	overshootGrace         = 5
	overshootThreshold     = 0.20
)

// Run executes the orchestrator's main loop. It blocks until the context is
// cancelled, the target is reached, or an unrecoverable error occurs.
func Run(ctx context.Context, cfg Config) error {
	cfg = withDefaults(cfg)

	// 1. Acquire lock.
	lk, err := lock.Acquire(cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() {
		if err := lk.Release(); err != nil {
			slog.Warn("lifecycle: lock release", "err", err)
		}
	}()

	// 2. Load target + reference-F.
	rawTarget, ctrlTarget, err := loadTarget(cfg.TargetYAMLPath)
	if err != nil {
		return err
	}
	refF, err := loadReferenceF(cfg.ReferenceFPath)
	if err != nil {
		return err
	}

	// 3. Chain identity.
	chainIdentity, err := computeChainIdentity(cfg.GenesisSHA256, rawTarget.SHA256)
	if err != nil {
		return err
	}

	// 4. RPC client + chain id + head.
	rpcOpts := []rpc.Option{}
	if cfg.JWTPath != "" {
		rpcOpts = append(rpcOpts, rpc.WithJWTFile(cfg.JWTPath))
	}
	if raw := os.Getenv("ORCH_RPC_TIMEOUT_S"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			rpcOpts = append(rpcOpts, rpc.WithTimeout(time.Duration(v)*time.Second))
		}
	} else {
		rpcOpts = append(rpcOpts, rpc.WithTimeout(120*time.Second))
	}
	rpcCli, err := rpc.NewClient(cfg.RPCURL, rpcOpts...)
	if err != nil {
		return fmt.Errorf("lifecycle: rpc client: %w", err)
	}
	chainID, err := rpcCli.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: chain id: %w", err)
	}
	head, err := rpcCli.BlockByNumber(ctx, -1)
	if err != nil {
		return fmt.Errorf("lifecycle: head: %w", err)
	}
	slog.Info("lifecycle: connected to el client",
		"chain_id", chainID,
		"head_block", head.Number,
		"gas_limit", head.GasLimit,
	)

	// 5. Decide fresh vs resume.
	decision, err := resolveStartupMode(ctx, cfg.StateDir, rpcCli)
	if err != nil {
		return err
	}
	slog.Info("lifecycle: startup mode resolved", "mode", decision.Mode.String(), "resumed_from", decision.ResumedFromBatch)

	if decision.Mode == modeResume {
		if err := reconcilePending(ctx, rpcCli, decision); err != nil {
			return err
		}
	}

	// 6. Build signer.
	deployKey := cfg.DeployPrivateKey
	if deployKey == "" {
		deployKey = os.Getenv("ORCH_DEPLOY_PRIVATE_KEY")
	}
	if deployKey == "" {
		return errors.New("lifecycle: deploy private key not set (--deploy-private-key or $ORCH_DEPLOY_PRIVATE_KEY)")
	}
	signr, err := signer.New(deployKey)
	if err != nil {
		return fmt.Errorf("lifecycle: signer: %w", err)
	}

	// 7a. Metrics registry + HTTP server.
	metricsReg := metrics.New()
	metricsReg.Epsilon.Set(cfg.Epsilon)
	metricsSrv := &http.Server{
		Addr:    cfg.MetricsAddr,
		Handler: metricsReg.Handler(),
	}
	go func() {
		slog.Info("lifecycle: metrics listening", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Warn("lifecycle: metrics server error", "err", err)
		}
	}()
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := metricsSrv.Shutdown(shutCtx); err != nil {
			slog.Warn("lifecycle: metrics server shutdown", "err", err)
		}
		slog.Info("lifecycle: metrics server stopped")
	}()

	// 7. Open builder pool, sensor, journal writer, payloads writer.
	pool, err := builderpool.Open(ctx, builderpool.Config{
		Cmd:     cfg.BuilderWorkerCmd,
		Workers: cfg.BuilderWorkers,
	})
	if err != nil {
		return fmt.Errorf("lifecycle: builder pool: %w", err)
	}
	defer pool.Close()

	sensorOpts := []sensor.SensorOption{}
	if cfg.SensorPollInterval > 0 {
		sensorOpts = append(sensorOpts, sensor.WithPollInterval(cfg.SensorPollInterval))
	}
	if cfg.SensorDeadline > 0 {
		sensorOpts = append(sensorOpts, sensor.WithDeadline(cfg.SensorDeadline))
	}
	sens := sensor.New(rpcCli, sensorOpts...)

	jw, err := journal.OpenWriter(decision.JournalPath)
	if err != nil {
		return fmt.Errorf("lifecycle: journal writer: %w", err)
	}
	defer jw.Close()

	pw, err := payloads.OpenWriter(decision.PayloadsPath)
	if err != nil {
		return fmt.Errorf("lifecycle: payloads writer: %w", err)
	}
	defer pw.Close()

	// 8. Build controller state, hydrating from tail on resume.
	state := controller.NewState(cfg.Verbs, refF, chainIdentity, cfg.Epsilon)
	if decision.Mode == modeResume && decision.TailRecord != nil {
		hydrateStateFromTail(state, decision.TailRecord)
	}

	// 9. Facade context. Base address from $ORCH_BASE_ADDRESS (hex), else zeros.
	// Initial nonce from $ORCH_INITIAL_ADDRESS_CURSOR override, else queried from RPC.
	facadeCtx := buildFacadeContext(rawTarget, chainID, head.GasLimit)
	if hex := strings.TrimPrefix(os.Getenv("ORCH_BASE_ADDRESS"), "0x"); hex != "" {
		b, err := decodeBaseAddress(hex)
		if err != nil {
			return fmt.Errorf("lifecycle: ORCH_BASE_ADDRESS: %w", err)
		}
		facadeCtx.BaseAddress = b
	}
	if raw := os.Getenv("ORCH_INITIAL_ADDRESS_CURSOR"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("lifecycle: ORCH_INITIAL_ADDRESS_CURSOR: %w", err)
		}
		facadeCtx.AddressCursor.Store(v)
	} else {
		// The chain's master-signer nonce is authoritative for the address
		// cursor and survives EL-client crashes / reorgs. Derive it from the
		// chain for both fresh and resume — a resumed run after an EL restart
		// must pick up the rolled-back nonce, not the (stale) journal cursor.
		n, err := rpcCli.TransactionCount(ctx, signr.Address())
		if err != nil {
			return fmt.Errorf("lifecycle: query master nonce: %w", err)
		}
		facadeCtx.AddressCursor.Store(n)
		slog.Info("lifecycle: master nonce primed from RPC",
			"address", signr.Address().Hex(), "nonce", n, "mode", decision.Mode.String())
	}

	// 10. Initial observation from sensor. Loop until the plugin returns valid
	// trie stats — during a bootstrap scan, state-comp returns all zeros. The
	// cycle is sensor → scenario → bloat → sensor, so a blind start (zero stats)
	// would feed garbage into the controller. Wait for the plugin instead.
	initialSnap, err := waitForValidSensor(ctx, sens)
	if err != nil {
		return fmt.Errorf("lifecycle: initial sensor read: %w", err)
	}
	currentObs := &controller.Observation{
		AccountTrieBytes: initialSnap.AccountTrieBytes,
		StorageTrieBytes: initialSnap.StorageTrieBytes,
		CodeBytesTotal:   initialSnap.CodeBytesTotal,
		BlockNumber:      initialSnap.BlockNumber,
	}

	// 10a. Seed initial ObservedFlatBytes gauges.
	metricsReg.ObservedFlatBytes.WithLabelValues("accounts").Set(float64(currentObs.AccountTrieBytes))
	metricsReg.ObservedFlatBytes.WithLabelValues("storage").Set(float64(currentObs.StorageTrieBytes))
	metricsReg.ObservedFlatBytes.WithLabelValues("code").Set(float64(currentObs.CodeBytesTotal))

	// 11. Manifest.
	sessionID := newSessionID()
	metricsReg.SessionID.WithLabelValues(sessionID).Set(1)
	manifestPath := cfg.ManifestPath
	if manifestPath == "" {
		manifestPath = cfg.StateDir + "/run-manifest.json"
	}
	mf := freshManifest(cfg, rawTarget, chainIdentity, sessionID)
	if err := mf.Save(manifestPath); err != nil {
		return fmt.Errorf("lifecycle: initial manifest save: %w", err)
	}

	// 12. Target watcher.
	targetCh := make(chan *target.Target, 1)
	watcher, err := target.NewWatcher(ctx, cfg.TargetYAMLPath, func(t *target.Target) {
		select {
		case targetCh <- t:
		default:
		}
	})
	if err != nil {
		slog.Warn("lifecycle: target watcher disabled", "err", err)
	} else {
		defer watcher.Close()
	}

	// 13. Prime fee policy once synchronously so the first batch has a valid
	// max-fee/tip pair before any planner runs, then start the background
	// refresher (single RPC per tick, independent of planner count).
	if _, err := refreshFeePolicy(ctx, rpcCli, facadeCtx); err != nil {
		return fmt.Errorf("lifecycle: prime fee policy: %w", err)
	}
	feeCtx, feeCancel := context.WithCancel(ctx)
	var feeWG sync.WaitGroup
	feeWG.Add(1)
	go func() {
		defer feeWG.Done()
		if err := runFeePolicyLoop(feeCtx, rpcCli, facadeCtx, defaultFeePolicyInterval); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("lifecycle: fee policy loop exited", "err", err)
		}
	}()
	defer func() {
		feeCancel()
		feeWG.Wait()
	}()

	// 14. Mode controller + healthd. Healthd polls chain head vs sensor head
	// at 500ms and requests throttle/halt transitions when the sensor lags.
	// Step 7 will move both into an errgroup.
	modeCtl := mode.NewModeController()
	hd := healthd.New(rpcCli, sens, modeCtl)
	healthCtx, healthCancel := context.WithCancel(ctx)
	var healthWG sync.WaitGroup
	healthWG.Add(1)
	go func() {
		defer healthWG.Done()
		if err := hd.Run(healthCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("lifecycle: healthd exited", "err", err)
		}
	}()
	defer func() {
		healthCancel()
		healthWG.Wait()
	}()
	_ = modeCtl // mode is consumed in a later step; declared here so healthd has a target

	// 15. Hot loop.
	dispatcher := facade.New(pool, signr)
	deps := &batchDeps{
		rpc:          rpcCli,
		sensor:       sens,
		dispatcher:   dispatcher,
		jw:           jw,
		pw:           pw,
		state:        state,
		facadeCtx:    facadeCtx,
		pendingPath:  decision.PendingPath,
		sessionID:    sessionID,
		resumedFrom:  decision.ResumedFromBatch,
		target:       ctrlTarget,
		targetDigest: targetSha256Bytes(rawTarget.SHA256),
		metricsReg:   metricsReg,
		reserveMu:    &sync.Mutex{},
		seqCounter:   &atomic.Uint64{},
	}

	termination, finalBlock := runLoop(ctx, cfg, deps, &ctrlTarget, currentObs, targetCh, rawTarget, mf)

	// 16. Finalise manifest.
	finishISO := time.Now().UTC().Format(time.RFC3339Nano)
	mf.FinishedAtISO = finishISO
	mf.Terminated = termination
	mf.BatchCount = int(state.BatchID)
	tailHash := jw.CurrentChainHash()
	mf.LastChainHashCheckpoint = hexBytes(tailHash[:])
	if finalBlock != nil {
		mf.FinalStateRoot = finalBlock.StateRoot.Hex()
	}
	if err := mf.Save(manifestPath); err != nil {
		slog.Error("lifecycle: final manifest save", "err", err)
		return err
	}

	slog.Info("lifecycle: run complete",
		"termination", termination,
		"batches", state.BatchID,
	)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func withDefaults(cfg Config) Config {
	if cfg.Epsilon <= 0 {
		cfg.Epsilon = defaultEpsilon
	}
	if cfg.TotalBatchBytes <= 0 {
		cfg.TotalBatchBytes = defaultTotalBatchBytes
	}
	if len(cfg.BuilderWorkerCmd) == 0 {
		cfg.BuilderWorkerCmd = []string{"python", "-m", "builder_worker"}
	}
	if len(cfg.Verbs) == 0 {
		cfg.Verbs = defaultVerbs()
	}
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = ":9101"
	}
	// ORCH_PLANNERS env var overrides the flag/struct value when set.
	if raw := os.Getenv("ORCH_PLANNERS"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			cfg.Planners = v
		}
	}
	if cfg.Planners <= 0 {
		cfg.Planners = 4
	}
	return cfg
}

func defaultVerbs() []string {
	return []string{
		"eoatx", "calltx", "deploytx", "factorydeploytx",
		"storagespam", "erc20_bloater", "erc20tx", "uniswap_swaps",
		"storagerefundtx", "gasburnertx", "evm_fuzz", "noop",
	}
}

// decodeBaseAddress parses a 20-byte hex string (0x optional, padded left with zeros).
func decodeBaseAddress(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "0x")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(raw) > 20 {
		return nil, fmt.Errorf("base address > 20 bytes (got %d)", len(raw))
	}
	out := make([]byte, 20)
	copy(out[20-len(raw):], raw)
	return out, nil
}

// buildFacadeContext creates a fresh facade.Context for this run.
// AddressCursor / SaltCursor / BlockGasLimit are zero-valued atomics; callers
// must use the typed Set / Reserve methods to seed/advance them.
func buildFacadeContext(t *target.Target, chainID, gasLimit uint64) *facade.Context {
	c := &facade.Context{
		BaseAddress:    make([]byte, 20),
		Revision:       0,
		AddressStride:  defaultAddressStride,
		ChainID:        chainID,
		GasLimit:       gasLimit,
		VerbGasFactors: map[string]float64{},
	}
	c.SetBlockGasLimit(gasLimit)
	return c
}

// runLoop drives the dispatch+commit pipeline until a termination condition
// fires. Returns the termination reason and the last committed block (or nil).
//
// Topology:
//   - N PLANNER goroutines (cfg.Planners, default 4): each grabs a unique
//     batchID via atomic increment, picks a plan, reserves nonce+salt ranges
//     atomically from the shared facadeCtx, dispatches, and sends *dispatched
//     into the pipe. All planners share one master signer.
//   - 1 COMMIT goroutine: drains the pipe, calls commitBatch, updates the
//     shared observation pointer + lastBlock. Drains pipe to completion even
//     on shutdown so no dispatched batch is orphaned.
//   - 1 PIPE-CLOSER goroutine: waits for all planners to exit then close(pipe)
//     so the commit goroutine sees EOF.
//
// Only planner 0 services the targetCh reload — the other planners pick up
// the new target via the shared *ctrlTarget on their next iteration.
//
// The pipe capacity n+1 keeps backpressure tight while letting planners hand
// off work without serialising on the commit goroutine.
func runLoop(
	ctx context.Context,
	cfg Config,
	deps *batchDeps,
	ctrlTarget **controller.Target,
	currentObs *controller.Observation,
	targetCh chan *target.Target,
	rawTarget *target.Target,
	mf *manifest.Manifest,
) (string, *rpc.BlockHeader) {
	startBatchID := deps.resumedFrom
	if startBatchID > 0 {
		startBatchID++
	}

	// Shared state.
	obsPtr := &atomic.Pointer[controller.Observation]{}
	obsPtr.Store(currentObs)

	lastBlockPtr := &atomic.Pointer[rpc.BlockHeader]{}

	var (
		termOnce   sync.Once
		termReason atomic.Pointer[string]
	)
	setTerm := func(reason string) {
		termOnce.Do(func() {
			r := reason
			termReason.Store(&r)
		})
	}

	n := cfg.Planners
	if n <= 0 {
		n = 4
	}

	pipe := make(chan *dispatched, n+1)

	// Shared batch-id allocator. Each planner does globalBatchID.Add(1)-1 to
	// claim a unique id. Note that with N>1 planners, batch ids may commit
	// out of strict numerical order; the commit goroutine still serialises
	// the journal append (single writer).
	var globalBatchID atomic.Uint64
	globalBatchID.Store(startBatchID)

	plannerLoop := func(idx int) {
		idleBackoff := 50 * time.Millisecond
		for {
			// Early-exit when another planner already tripped a terminator.
			if termReason.Load() != nil {
				return
			}

			// 1. Drain target reloads (planner 0 only) and respect cancellation.
			select {
			case <-ctx.Done():
				setTerm(terminationSignal)
				return
			default:
			}
			if idx == 0 {
				select {
				case newT := <-targetCh:
					_, newCtrl, _ := refreshTarget(newT)
					*ctrlTarget = newCtrl
					deps.target = newCtrl
					deps.targetDigest = targetSha256Bytes(newT.SHA256)
					mf.TargetHistory = append(mf.TargetHistory, manifest.TargetEntry{
						AppliedAtBatch: int(globalBatchID.Load()),
						AppliedAtISO:   time.Now().UTC().Format(time.RFC3339Nano),
						TargetSHA256:   newT.SHA256,
						Shares:         cloneShares(newT.Shares),
						TotalBytes:     newT.TotalBytes,
					})
					slog.Info("lifecycle: target reloaded", "sha", newT.SHA256[:8], "total_bytes", newT.TotalBytes)
				default:
				}
			}

			// 2. Termination predicates — checked BEFORE claiming a batchID so
			// we never burn an id on a batch we won't dispatch.
			threshold := overshootThreshold
			if env := os.Getenv("ORCH_OVERSHOOT_THRESHOLD"); env != "" {
				if v, err := strconv.ParseFloat(env, 64); err == nil {
					threshold = v
				}
			}
			if deps.state.HasInstability(threshold, overshootWindowSize, overshootMaxTrips, overshootGrace) {
				setTerm(terminationOvershoot)
				return
			}
			obsSnapshot := obsPtr.Load()
			if reachedTarget(obsSnapshot, *ctrlTarget) {
				setTerm(terminationTargetMet)
				return
			}
			if cfg.MaxBatches > 0 && globalBatchID.Load() >= uint64(cfg.MaxBatches) {
				setTerm(terminationBatchLimit)
				return
			}

			// 3. Claim a batchID and dispatch.
			batchID := globalBatchID.Add(1) - 1
			// Re-check MaxBatches: a racing planner may have just claimed the
			// last legal id. Drop the over-budget claim instead of dispatching.
			if cfg.MaxBatches > 0 && batchID >= uint64(cfg.MaxBatches) {
				setTerm(terminationBatchLimit)
				return
			}

			db, err := dispatchBatch(ctx, deps, batchID, obsSnapshot)
			if err != nil {
				// dispatchBatch may return a non-nil `db` even with an error
				// (a skip sentinel marking the seqID it already claimed). If
				// so, send it so the commit-ordering heap can advance past
				// this seqID before the run shuts down.
				if db != nil && db.skip {
					select {
					case <-ctx.Done():
					case pipe <- db:
					}
				}
				if ctx.Err() != nil {
					setTerm(terminationSignal)
					return
				}
				slog.Error("lifecycle: dispatch failed", "planner", idx, "batch_id", batchID, "err", err)
				setTerm("error: " + err.Error())
				return
			}
			if db == nil {
				// Pick yielded no plan (no seqID claimed yet) — back off
				// briefly. The batchID we claimed is wasted; the commit
				// goroutine never sees it, so journal numbering will have
				// a small hole (acceptable — the journal carries explicit
				// BatchId field).
				select {
				case <-ctx.Done():
					setTerm(terminationSignal)
					return
				case <-time.After(idleBackoff):
				}
				continue
			}

			// 4. Send to commit goroutine. This includes skip sentinels (db.skip
			// == true) so the heap-based commit ordering can drain past
			// reserved-but-unused seqIDs.
			select {
			case <-ctx.Done():
				setTerm(terminationSignal)
				return
			case pipe <- db:
			}
		}
	}

	var plannerWG sync.WaitGroup
	plannerWG.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer plannerWG.Done()
			plannerLoop(idx)
		}(i)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	// Pipe-closer: waits for every planner to exit, then closes the pipe so
	// the commit goroutine can drain to EOF.
	go func() {
		plannerWG.Wait()
		close(pipe)
	}()

	// COMMIT goroutine. Drains pipe to completion even on shutdown so we never
	// orphan a dispatched batch (which would leave a stale pending sidecar).
	//
	// Out-of-order arrival is the norm with N>1 planners: planner A may finish
	// its build+sign before planner B even though A holds a later seqID. The
	// heap (`q`) re-orders by seqID so we commit in strict nonce-monotonic
	// order. `nextSeq` is the seqID we expect next; the commit loop dequeues
	// only when the heap top matches it.
	//
	// Skip sentinels (db.skip == true) carry a seqID but no payload; they
	// represent planner failures after reservation. We advance nextSeq past
	// them without calling commitBatch.
	const rejectionStreakHalt = 5
	go func() {
		defer wg.Done()
		var q batchQueue
		var nextSeq uint64 = 0
		var consecutiveFullRejections int

		drain := func(useCtx context.Context) {
			for q.Len() > 0 && q[0].seqID == nextSeq {
				top := heap.Pop(&q).(*dispatched)
				nextSeq++
				if top.skip {
					continue
				}
				if useCtx.Err() != nil {
					continue
				}
				pre := obsPtr.Load()
				res, err := commitBatch(useCtx, deps, top, pre)
				if err != nil {
					if useCtx.Err() != nil {
						continue
					}
					if isPartialAcceptanceErr(err) {
						expected, included := parsePartialAcceptance(err)
						slog.Warn("lifecycle: commit partial-acceptance, skipping batch",
							"batch_id", top.batchID,
							"seq_id", top.seqID,
							"expected", expected,
							"included", included,
							"verb", top.plan.Verb,
						)
						if included == 0 {
							consecutiveFullRejections++
							if consecutiveFullRejections >= rejectionStreakHalt {
								setTerm(fmt.Sprintf("error: %d consecutive batches fully rejected (verb=%s); chain state likely missing dependencies",
									consecutiveFullRejections, top.plan.Verb))
								continue
							}
						} else {
							consecutiveFullRejections = 0
						}
						continue
					}
					slog.Error("lifecycle: commit failed", "batch_id", top.batchID, "seq_id", top.seqID, "err", err)
					setTerm("error: " + err.Error())
					continue
				}
				if res != nil && res.committed {
					obsPtr.Store(res.postObs)
					lastBlockPtr.Store(res.blockHeader)
					consecutiveFullRejections = 0
				}
			}
		}

		for db := range pipe {
			heap.Push(&q, db)
			drain(ctx)
		}

		// After pipe is closed: drain residual entries. If a planner died
		// holding a seqID without sending a sentinel (shouldn't happen, but
		// defensively) the heap top may not match nextSeq — in that case we
		// force-drain by advancing nextSeq to the heap top, which preserves
		// the relative order of the remaining batches.
		for q.Len() > 0 {
			if q[0].seqID > nextSeq {
				nextSeq = q[0].seqID
			}
			drain(ctx)
		}
	}()

	wg.Wait()

	// Drain residual pending sidecar in case commit was interrupted mid-flight
	// (commitBatch normally clears it; on cancel-during-commit we may have left
	// a stale marker). Best-effort: errors are logged but don't fail the run.
	if err := journal.ClearPending(deps.pendingPath); err != nil {
		slog.Warn("lifecycle: clear pending on shutdown", "err", err)
	}

	reason := terminationSignal
	if r := termReason.Load(); r != nil {
		reason = *r
	}
	return reason, lastBlockPtr.Load()
}

// refreshTarget reloads the target and returns the controller projection.
func refreshTarget(t *target.Target) (*target.Target, *controller.Target, error) {
	if t == nil {
		return nil, nil, errors.New("lifecycle: nil target")
	}
	return t, shareTargetFromTarget(t.Shares, t.TotalBytes, t.SHA256), nil
}

// isPartialAcceptanceErr matches NM's testing_commitBlockV1 partial-acceptance
// error pattern, where the block builder included fewer txs than the
// orchestrator submitted. Form: "expected N transactions but only M were included".
func isPartialAcceptanceErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "transactions but only")
}

// parsePartialAcceptance pulls the expected/included counts out of NM's
// partial-acceptance error string. Returns zeros if the pattern is unexpected.
// Format: "expected <expected> transactions but only <included> were included".
func parsePartialAcceptance(err error) (expected, included uint64) {
	if err == nil {
		return 0, 0
	}
	msg := err.Error()
	const sep = "transactions but only"
	idx := strings.Index(msg, sep)
	if idx < 0 {
		return 0, 0
	}
	// Walk left to extract the integer before " transactions but only".
	left := strings.TrimRight(msg[:idx], " ")
	if li := strings.LastIndex(left, " "); li >= 0 {
		left = left[li+1:]
	}
	if v, perr := strconv.ParseUint(left, 10, 64); perr == nil {
		expected = v
	}
	// Walk right to extract the integer after "transactions but only ".
	right := strings.TrimLeft(msg[idx+len(sep):], " ")
	end := 0
	for end < len(right) && right[end] >= '0' && right[end] <= '9' {
		end++
	}
	if end > 0 {
		if v, perr := strconv.ParseUint(right[:end], 10, 64); perr == nil {
			included = v
		}
	}
	return expected, included
}

func hexBytes(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexChars[v>>4]
		out[i*2+1] = hexChars[v&0x0f]
	}
	return string(out)
}
