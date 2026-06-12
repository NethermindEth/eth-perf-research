package replay

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// MemGate throttles replay so the EL client is never driven faster than it can
// flush state to disk — the replay analogue of the bloating run loop, which is
// "strictly sequential and sensor-paced ... never runs ahead of the sensor".
// Without it, engine_newPayload is dispatched back-to-back and a path-scheme
// Geth accumulates in-memory trie diff layers / journal buffer for heavy
// storage-spam blocks faster than its background flush drains them, until it is
// OOM-killed (observed on VM2: 50 GiB anon-rss, ~16k unflushed blocks lost and
// rewound on restart).
//
// The gate polls the client's live heap via debug_memStats every CheckEvery
// blocks. Once it crosses High the gate pauses dispatch — forcing a GC +
// return-to-OS with debug_freeOSMemory each tick — until the heap drains below
// Low (the EL's buffer/journal flush has caught up) or MaxWait elapses. A nil
// *MemGate is a no-op, so the gate is fully opt-in.
type MemGate struct {
	Client     *rpc.Client // EL debug RPC (e.g. geth http :8545 with the debug namespace)
	High       uint64      // pause when live heap exceeds this many bytes
	Low        uint64      // resume once live heap drops below this many bytes
	CheckEvery int         // poll cadence, in blocks
	Poll       time.Duration
	MaxWait    time.Duration
	// Flush, when non-nil, is invoked each drain tick while the gate is paused.
	// debug_freeOSMemory only scavenges already-freed pages; it cannot release
	// dirty state the EL has not yet persisted. The replay wires this to re-issue
	// forkchoiceUpdated(finalized=lastHead), which drives the EL's state-persist
	// pipeline forward while no new blocks are being imported, so the in-memory
	// diff-layer/journal buffer actually drains instead of merely stalling.
	Flush func(ctx context.Context) error
}

// memStats is the subset of the Go runtime.MemStats that debug_memStats returns
// which we use to gauge the client's live, OS-resident footprint. Sys is the
// total obtained from the OS and HeapReleased is the part already handed back,
// so Sys-HeapReleased tracks resident heap — the quantity the OOM killer sees
// (modulo the EL's off-heap fastcaches, which are roughly constant).
type memStats struct {
	Sys          uint64
	HeapReleased uint64
}

func (g *MemGate) readLive(ctx context.Context) (uint64, error) {
	var ms memStats
	if err := g.Client.Call(ctx, "debug_memStats", []any{}, &ms); err != nil {
		return 0, err
	}
	if ms.HeapReleased > ms.Sys {
		return 0, nil
	}
	return ms.Sys - ms.HeapReleased, nil
}

// Wait blocks while the client's live heap exceeds High, returning once it is
// back under Low (or MaxWait elapses, or ctx is cancelled). A read error is
// logged and treated as "not gated" so a transient RPC hiccup never wedges the
// replay.
func (g *MemGate) Wait(ctx context.Context) error {
	live, err := g.readLive(ctx)
	if err != nil {
		slog.Warn("mem gate: debug_memStats read failed, skipping", "err", err)
		return nil
	}
	if live < g.High {
		return nil
	}

	slog.Info("mem gate engaged: pausing replay for EL flush",
		"live_gb", float64(live)/1e9, "high_gb", float64(g.High)/1e9)
	deadline := time.Now().Add(g.MaxWait)
	var discard json.RawMessage
	for {
		// Drive the EL's state-persist pipeline (re-assert finalized head) so the
		// dirty diff-layer/journal buffer actually flushes while paused, then force
		// a GC + scavenge so the flushed pages are returned to the OS and the next
		// read reflects post-flush resident heap (and RSS actually drops).
		if g.Flush != nil {
			if err := g.Flush(ctx); err != nil {
				slog.Warn("mem gate: flush nudge failed", "err", err)
			}
		}
		_ = g.Client.Call(ctx, "debug_freeOSMemory", []any{}, &discard)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.Poll):
		}

		live, err = g.readLive(ctx)
		if err == nil && live < g.Low {
			slog.Info("mem gate released", "live_gb", float64(live)/1e9)
			return nil
		}
		if time.Now().After(deadline) {
			slog.Warn("mem gate: max wait elapsed, resuming", "live_gb", float64(live)/1e9)
			return nil
		}
	}
}
