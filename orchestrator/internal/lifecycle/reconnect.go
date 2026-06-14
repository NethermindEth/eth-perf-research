package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// terminationNMUnreachable is the terminal reason recorded when NM stays
// unreachable past ReconnectMaxWaitS — the only outcome where a transport
// error still terminates the run.
const terminationNMUnreachable = "nm_unreachable_timeout"

// reconnectProbe is the cheap NM endpoint the reconnect-wait loop polls to
// decide NM is back. ChainID maps to eth_chainId — a constant-time call that
// touches no chain state; it is strictly additive (no RPC method is changed).
type reconnectProbe interface {
	ChainID(ctx context.Context) (uint64, error)
}

// isTransportError reports whether err signals NM is unreachable (process down,
// socket closed, redeploy in progress) as opposed to a logical failure the
// chain returned (a JSON-RPC error envelope: bad params, batch rejected). Only
// clear NM-unreachable signals count as transport — anything ambiguous returns
// false so the caller keeps its existing terminate-on-error behaviour.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	// A JSON-RPC error envelope means NM was reachable and answered — that is a
	// logical error regardless of any transport-sounding words in the message.
	if _, ok := errors.AsType[*rpc.RpcError](err); ok {
		return false
	}
	// Socket-level closure during an in-flight call.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Dial / read deadline elapsed before NM answered.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// net.Error covers dial timeouts, connection refused wrapped as *net.OpError,
	// and DNS failures.
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	// Fallback substring match for transport failures that arrive as plain
	// strings (e.g. http transport wrapping a closed connection).
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"connection refused",
		"connection reset",
		"broken pipe",
		"no such host",
		"network is unreachable",
		"i/o timeout",
		"eof",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// awaitReconnect polls probe with exponential backoff until it answers (NM is
// back) or maxWait elapses. Returns nil once NM is reachable again; returns the
// last probe error if maxWait is exceeded; returns ctx.Err() if cancelled.
//
// The orchestrator's address/nonce cursor, journal and controller State all
// live in memory and are untouched here — on a nil return the caller simply
// resumes the loop from where it left off.
func awaitReconnect(ctx context.Context, probe reconnectProbe, maxWait, backoffInitial, backoffMax time.Duration) error {
	slog.Warn("lifecycle: NM unreachable — entering reconnect wait",
		"max_wait", maxWait, "backoff_initial", backoffInitial, "backoff_max", backoffMax)

	deadline := time.Now().Add(maxWait)
	backoff := backoffInitial

	for {
		// Sleep first: the call that triggered the reconnect just failed, so
		// an immediate retry would almost certainly fail too.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		// Bound each probe so a hung dial cannot outlast the overall deadline.
		probeCtx, cancel := context.WithTimeout(ctx, backoff)
		_, err := probe.ChainID(probeCtx)
		cancel()
		if err == nil {
			slog.Info("lifecycle: NM reconnected — resuming")
			return nil
		}
		if time.Now().After(deadline) {
			slog.Error("lifecycle: NM still unreachable past max reconnect wait — terminating",
				"max_wait", maxWait, "last_err", err)
			return err
		}
		slog.Warn("lifecycle: NM still unreachable — backing off", "err", err, "next_backoff", backoff)

		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}
