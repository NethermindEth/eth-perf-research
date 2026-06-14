package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// TestIsTransportErrorClassifiesNMUnreachable pins the classifier: socket-level
// and dial failures are transport (NM down), a JSON-RPC error envelope and a
// plain chain-rejection error are logical.
func TestIsTransportErrorClassifiesNMUnreachable(t *testing.T) {
	transportCases := []struct {
		name string
		err  error
	}{
		{"io.EOF", io.EOF},
		{"wrapped io.EOF", fmt.Errorf("rpc: http: %w", io.EOF)},
		{"unexpected EOF", io.ErrUnexpectedEOF},
		{"connection refused string", errors.New("dial tcp 127.0.0.1:8551: connect: connection refused")},
		{"connection reset string", errors.New("read tcp: connection reset by peer")},
		{"net.OpError", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}},
		{"context deadline", context.DeadlineExceeded},
		{"wrapped deadline", fmt.Errorf("rpc: ChainID: %w", context.DeadlineExceeded)},
		{"no such host", errors.New("lookup nethermind: no such host")},
	}
	for _, tc := range transportCases {
		if !isTransportError(tc.err) {
			t.Errorf("%s: isTransportError = false, want true", tc.name)
		}
	}

	logicalCases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"rpc error envelope", &rpc.RpcError{Code: -32000, Message: "block rejected"}},
		{"wrapped rpc error", fmt.Errorf("rpc: TestingCommitBlockV1: %w",
			&rpc.RpcError{Code: -32602, Message: "invalid params"})},
		{"chain rejection", errors.New("expected 100 transactions but only 0 were included")},
		{"plain logical error", errors.New("controller.Apply: residual diverged")},
	}
	for _, tc := range logicalCases {
		if isTransportError(tc.err) {
			t.Errorf("%s: isTransportError = true, want false", tc.name)
		}
	}
}

// fakeProbe is a reconnectProbe whose ChainID fails for the first failFor calls
// then succeeds, recording the total call count.
type fakeProbe struct {
	failFor int32
	calls   atomic.Int32
	err     error
}

func (f *fakeProbe) ChainID(ctx context.Context) (uint64, error) {
	n := f.calls.Add(1)
	if n <= f.failFor {
		return 0, f.err
	}
	return 1, nil
}

// alwaysDownProbe always returns a transport error.
type alwaysDownProbe struct{ calls atomic.Int32 }

func (p *alwaysDownProbe) ChainID(ctx context.Context) (uint64, error) {
	p.calls.Add(1)
	return 0, io.EOF
}

// TestAwaitReconnectResumesAfterProbeSucceeds verifies the loop retries on a
// transport error and returns nil once the probe answers.
func TestAwaitReconnectResumesAfterProbeSucceeds(t *testing.T) {
	probe := &fakeProbe{failFor: 2, err: io.EOF}
	err := awaitReconnect(context.Background(), probe,
		10*time.Second, 1*time.Millisecond, 8*time.Millisecond)
	if err != nil {
		t.Fatalf("awaitReconnect = %v, want nil (resume)", err)
	}
	if got := probe.calls.Load(); got != 3 {
		t.Errorf("probe called %d times, want 3 (2 fail + 1 success)", got)
	}
}

// TestAwaitReconnectTimesOut verifies the loop gives up (returns the last error)
// once max wait elapses while NM stays down.
func TestAwaitReconnectTimesOut(t *testing.T) {
	probe := &alwaysDownProbe{}
	start := time.Now()
	err := awaitReconnect(context.Background(), probe,
		30*time.Millisecond, 5*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("awaitReconnect = nil, want timeout error")
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("returned after %v, want >= max wait 30ms", elapsed)
	}
	if probe.calls.Load() == 0 {
		t.Error("probe was never called")
	}
}

// TestAwaitReconnectHonoursContextCancel verifies a cancelled context aborts
// the wait promptly with ctx.Err().
func TestAwaitReconnectHonoursContextCancel(t *testing.T) {
	probe := &alwaysDownProbe{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := awaitReconnect(ctx, probe,
		10*time.Second, 50*time.Millisecond, 100*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("awaitReconnect = %v, want context.Canceled", err)
	}
}
