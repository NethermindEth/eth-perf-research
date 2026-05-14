package healthd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/mode"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
)

type fakeRPC struct {
	headBN atomic.Uint64
}

func (f *fakeRPC) BlockByNumber(_ context.Context, _ int64) (*rpc.BlockHeader, error) {
	return &rpc.BlockHeader{Hash: common.Hash{}, Number: f.headBN.Load()}, nil
}

type fakeSensor struct {
	bn atomic.Uint64
}

func (f *fakeSensor) PollOnce(_ context.Context) (*sensor.Snapshot, error) {
	return &sensor.Snapshot{BlockNumber: f.bn.Load(), ObservedAt: time.Now()}, nil
}

func tickStep(h *Health, ctx context.Context, when time.Time) {
	h.step(ctx, when)
}

func TestClassifyTransitions(t *testing.T) {
	th := Thresholds{
		ThrottleBlocks:  200,
		HaltBlocks:      2000,
		ThrottleSeconds: 30,
		HaltSeconds:     300,
		RecoveryTicks:   3,
		TickInterval:    500 * time.Millisecond,
	}
	cases := []struct {
		name     string
		blocks   uint64
		seconds  float64
		expected StalenessLevel
	}{
		{"normal lag", 30, 1, StalenessOK},
		{"just under throttle", 200, 5, StalenessOK},
		{"degraded by blocks", 201, 5, StalenessDegraded},
		{"degraded by seconds", 50, 31, StalenessDegraded},
		{"critical by blocks", 2001, 10, StalenessCritical},
		{"critical by seconds", 50, 301, StalenessCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.blocks, tc.seconds, th)
			if got != tc.expected {
				t.Fatalf("classify(%d,%f)=%v want %v", tc.blocks, tc.seconds, got, tc.expected)
			}
		})
	}
}

func TestStepThrottlesOnDegradedSensor(t *testing.T) {
	r := &fakeRPC{}
	s := &fakeSensor{}
	r.headBN.Store(1000)
	s.bn.Store(500) // 500 blocks behind > 200 threshold
	mc := mode.NewModeController()

	th := DefaultThresholds()
	th.TickInterval = 10 * time.Millisecond
	h := NewWithThresholds(r, s, mc, th)

	tickStep(h, context.Background(), time.Now())

	if got := mc.Get(); got != mode.ModeThrottle {
		t.Fatalf("mode = %v, want Throttle", got)
	}
	if h.Snapshot().Level != StalenessDegraded {
		t.Fatalf("level = %v, want Degraded", h.Snapshot().Level)
	}
}

func TestStepHaltsOnCriticalSensor(t *testing.T) {
	r := &fakeRPC{}
	s := &fakeSensor{}
	r.headBN.Store(10000)
	s.bn.Store(5000) // 5000 blocks behind > 2000 halt threshold
	mc := mode.NewModeController()

	h := NewWithThresholds(r, s, mc, DefaultThresholds())
	tickStep(h, context.Background(), time.Now())

	if got := mc.Get(); got != mode.ModeHalt {
		t.Fatalf("mode = %v, want Halt", got)
	}
	if h.Snapshot().Level != StalenessCritical {
		t.Fatalf("level = %v, want Critical", h.Snapshot().Level)
	}
}

func TestRecoveryRequiresNConsecutiveOKTicks(t *testing.T) {
	r := &fakeRPC{}
	s := &fakeSensor{}
	r.headBN.Store(1000)
	s.bn.Store(500) // degraded
	mc := mode.NewModeController()

	th := DefaultThresholds()
	th.RecoveryTicks = 3
	h := NewWithThresholds(r, s, mc, th)

	// First tick: degraded -> throttle.
	tickStep(h, context.Background(), time.Now())
	if mc.Get() != mode.ModeThrottle {
		t.Fatalf("expected throttle after degraded tick, got %v", mc.Get())
	}

	// Sensor catches up; lastAdvanceUnix moves forward each tick.
	s.bn.Store(1000)
	r.headBN.Store(1000)

	// Tick 1 OK -> still throttle.
	tickStep(h, context.Background(), time.Now())
	if mc.Get() != mode.ModeThrottle {
		t.Fatalf("recovered after 1 OK tick, want %v, got %v", mode.ModeThrottle, mc.Get())
	}
	// Tick 2 OK -> still throttle.
	tickStep(h, context.Background(), time.Now())
	if mc.Get() != mode.ModeThrottle {
		t.Fatalf("recovered after 2 OK ticks, want %v, got %v", mode.ModeThrottle, mc.Get())
	}
	// Tick 3 OK -> run.
	tickStep(h, context.Background(), time.Now())
	if mc.Get() != mode.ModeRun {
		t.Fatalf("did not recover after 3 OK ticks, got %v", mc.Get())
	}
}

func TestRecoveryStreakResetsOnDegraded(t *testing.T) {
	r := &fakeRPC{}
	s := &fakeSensor{}
	mc := mode.NewModeController()

	th := DefaultThresholds()
	th.RecoveryTicks = 3
	h := NewWithThresholds(r, s, mc, th)

	// Start degraded.
	r.headBN.Store(1000)
	s.bn.Store(500)
	tickStep(h, context.Background(), time.Now())
	if mc.Get() != mode.ModeThrottle {
		t.Fatalf("want throttle got %v", mc.Get())
	}

	// One OK tick.
	s.bn.Store(1000)
	tickStep(h, context.Background(), time.Now())

	// Back to degraded; streak resets.
	r.headBN.Store(2000)
	s.bn.Store(1500)
	tickStep(h, context.Background(), time.Now())

	// Now provide 2 OK ticks — must NOT recover (streak was reset).
	r.headBN.Store(2000)
	s.bn.Store(2000)
	tickStep(h, context.Background(), time.Now())
	tickStep(h, context.Background(), time.Now())
	if mc.Get() != mode.ModeThrottle {
		t.Fatalf("recovered after only 2 OK ticks (streak should reset), got %v", mc.Get())
	}
}

func TestRunStopsOnCtxCancel(t *testing.T) {
	r := &fakeRPC{}
	s := &fakeSensor{}
	mc := mode.NewModeController()
	th := DefaultThresholds()
	th.TickInterval = 5 * time.Millisecond
	h := NewWithThresholds(r, s, mc, th)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = h.Run(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestSnapshotCopiesStaleness(t *testing.T) {
	r := &fakeRPC{}
	s := &fakeSensor{}
	mc := mode.NewModeController()
	h := NewWithThresholds(r, s, mc, DefaultThresholds())

	// Concurrent readers must never see torn fields.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = h.Snapshot()
			}
		}()
	}
	for i := 0; i < 100; i++ {
		r.headBN.Store(uint64(i * 100))
		s.bn.Store(uint64(i * 50))
		tickStep(h, context.Background(), time.Now())
	}
	wg.Wait()
}
