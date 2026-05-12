// Package builderpool manages a pool of Python subprocess workers that build
// transaction batches via the framed-protobuf wire protocol.
package builderpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
)

const (
	defaultBootDeadline    = 10 * time.Second
	defaultRequestTimeout  = 5 * time.Second
	sigkillGrace           = 5 * time.Second
)

// Config configures the worker pool.
type Config struct {
	// Cmd is the command to spawn each worker, e.g. {"python", "-m", "builder_worker"}.
	Cmd []string
	// Workers is the number of worker processes (0 → runtime.NumCPU()).
	Workers int
	// StartupEnv holds extra environment variables passed to each worker.
	StartupEnv []string
	// BootDeadline is the maximum time to wait for the first worker to be ready.
	// Defaults to 10s.
	BootDeadline time.Duration
}

// Pool manages a set of builder worker subprocesses.
// Build is safe for concurrent use.
type Pool struct {
	cfg     Config
	workers []*worker
	// semaphore: one slot per worker; acquiring a slot gives exclusive access
	// to the worker at the same index.
	sem     chan int // sends the worker index
	mu      sync.Mutex
	closed  bool
}

// Open launches cfg.Workers subprocesses and returns a ready Pool.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.BootDeadline <= 0 {
		cfg.BootDeadline = defaultBootDeadline
	}

	p := &Pool{
		cfg:     cfg,
		workers: make([]*worker, cfg.Workers),
		sem:     make(chan int, cfg.Workers),
	}

	bootCtx, cancel := context.WithTimeout(ctx, cfg.BootDeadline)
	defer cancel()

	// Start all workers; the boot deadline covers the whole startup phase.
	var wg sync.WaitGroup
	errs := make([]error, cfg.Workers)
	for i := range cfg.Workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			w := newWorker(idx, cfg.Cmd, cfg.StartupEnv)
			if err := w.start(); err != nil {
				errs[idx] = err
				return
			}
			p.workers[idx] = w
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-bootCtx.Done():
		return nil, fmt.Errorf("builderpool: workers did not start within boot deadline: %w", bootCtx.Err())
	case <-done:
	}

	var startErr error
	for _, e := range errs {
		startErr = errors.Join(startErr, e)
	}
	if startErr != nil {
		// Kill any workers that did start.
		for _, w := range p.workers {
			if w != nil {
				w.kill()
			}
		}
		return nil, fmt.Errorf("builderpool: start workers: %w", startErr)
	}

	// Fill the semaphore: each slot carries the index of a free worker.
	for i := range cfg.Workers {
		p.sem <- i
	}
	return p, nil
}

// Build sends req to a free worker and returns the response.
// Honours ctx.Done(). On worker crash, respawns once and retries.
func (p *Pool) Build(ctx context.Context, req *orchpb.BuildBatchRequest) (*orchpb.BuildBatchResponse, error) {
	resp, err := p.buildOnce(ctx, req)
	if err == nil {
		return resp, nil
	}

	// Retry once.
	slog.Warn("builderpool: worker error, will retry once", "err", err)
	resp, err2 := p.buildOnce(ctx, req)
	if err2 != nil {
		return nil, fmt.Errorf("builderpool: build failed after retry: %w", errors.Join(err, err2))
	}
	return resp, nil
}

func (p *Pool) buildOnce(ctx context.Context, req *orchpb.BuildBatchRequest) (*orchpb.BuildBatchResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, defaultRequestTimeout)
	defer cancel()

	// Acquire a free worker slot.
	var idx int
	select {
	case <-reqCtx.Done():
		return nil, fmt.Errorf("builderpool: acquire worker: %w", reqCtx.Err())
	case idx = <-p.sem:
	}

	w := p.workers[idx]
	if !w.isAlive() {
		// Respawn before use.
		if err := p.respawn(idx); err != nil {
			p.sem <- idx
			return nil, fmt.Errorf("builderpool: respawn worker[%d]: %w", idx, err)
		}
		w = p.workers[idx]
	}

	w.mu.Lock()
	resp, err := w.callLocked(req)
	w.mu.Unlock()

	if err != nil {
		// Worker crashed; kill and respawn for next caller.
		w.kill()
		slog.Warn("builderpool: worker crashed, respawning", "worker", idx, "err", err)
		if respawnErr := p.respawn(idx); respawnErr != nil {
			slog.Warn("builderpool: respawn failed", "worker", idx, "err", respawnErr)
		}
		p.sem <- idx
		return nil, fmt.Errorf("builderpool: worker[%d] call: %w", idx, err)
	}

	p.sem <- idx
	return resp, nil
}

// respawn replaces the worker at idx with a fresh subprocess.
func (p *Pool) respawn(idx int) error {
	if p.workers[idx] != nil {
		p.workers[idx].kill()
	}
	w := newWorker(idx, p.cfg.Cmd, p.cfg.StartupEnv)
	if err := w.start(); err != nil {
		return err
	}
	p.workers[idx] = w
	return nil
}

// Close terminates all workers (SIGTERM, then SIGKILL after 5s) and waits.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	var wg sync.WaitGroup
	for _, w := range p.workers {
		if w == nil {
			continue
		}
		wg.Add(1)
		go func(wk *worker) {
			defer wg.Done()
			if wk.proc == nil || wk.proc.Process == nil {
				return
			}
			// Graceful SIGTERM.
			if err := wk.proc.Process.Signal(syscall.SIGTERM); err != nil {
				wk.kill()
				return
			}
			// Wait with a 5s deadline then SIGKILL.
			done := make(chan struct{})
			go func() {
				wk.proc.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(sigkillGrace):
				slog.Warn("builderpool: worker did not exit in time, sending SIGKILL", "worker", wk.idx)
				wk.kill()
			}
		}(w)
	}
	wg.Wait()
	return nil
}

