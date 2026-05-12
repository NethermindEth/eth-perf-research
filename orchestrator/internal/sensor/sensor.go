// Package sensor polls Nethermind's statecomp_get JSON-RPC method and waits for
// the node to process a specific block number before returning a state snapshot.
package sensor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

const (
	defaultPollInterval = 100 * time.Millisecond
	defaultDeadline     = 5 * time.Second
)

// ErrSensorTimeout is returned when blockNumber fails to reach the expected
// value before the deadline. Use errors.Is to detect it; the error value may
// be wrapped with the context error via errors.Join.
var ErrSensorTimeout = errors.New("sensor: timeout waiting for blockNumber")

// Snapshot captures the statecomp report at a single point in time.
type Snapshot struct {
	BlockNumber      uint64
	AccountTrieBytes uint64
	StorageTrieBytes uint64
	CodeBytesTotal   uint64
	Raw              json.RawMessage // preserved verbatim for the journal
	ObservedAt       time.Time
}

// sensorOpts collects configuration for a Sensor.
type sensorOpts struct {
	pollInterval time.Duration
	deadline     time.Duration
}

// SensorOption configures a Sensor.
type SensorOption func(*sensorOpts)

// WithPollInterval sets how often statecomp_get is polled (default 100ms).
func WithPollInterval(d time.Duration) SensorOption {
	return func(o *sensorOpts) { o.pollInterval = d }
}

// WithDeadline sets the maximum time Read will wait for blockNumber >= expected
// (default 5s).
func WithDeadline(d time.Duration) SensorOption {
	return func(o *sensorOpts) { o.deadline = d }
}

// Sensor polls statecomp_get and returns snapshots to the orchestrator.
type Sensor struct {
	client       *rpc.Client
	pollInterval time.Duration
	deadline     time.Duration
}

// New constructs a Sensor backed by the provided RPC client.
func New(c *rpc.Client, opts ...SensorOption) *Sensor {
	o := &sensorOpts{
		pollInterval: defaultPollInterval,
		deadline:     defaultDeadline,
	}
	for _, fn := range opts {
		fn(o)
	}
	return &Sensor{
		client:       c,
		pollInterval: o.pollInterval,
		deadline:     o.deadline,
	}
}

// Read polls statecomp_get until the returned blockNumber >= expected, or until
// the deadline (default 5s) expires. Returns the matching snapshot on success.
//
// On timeout, ErrSensorTimeout is returned together with the most recent
// snapshot (which may be nil if no successful poll occurred). The two are
// returned as a compound error via errors.Join when the context also carries
// an error.
//
// The caller's ctx is respected: cancellation returns immediately with
// ctx.Err() wrapped.
func (s *Sensor) Read(ctx context.Context, expected uint64) (*Snapshot, error) {
	deadline := time.Now().Add(s.deadline)
	dctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var last *Snapshot
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	// Poll immediately before waiting for the first tick.
	for {
		snap, err := s.poll(dctx)
		if err == nil {
			last = snap
			if snap.BlockNumber >= expected {
				return snap, nil
			}
		}

		// Wait for next tick or cancellation.
		select {
		case <-dctx.Done():
			cause := dctx.Err()
			if errors.Is(cause, context.DeadlineExceeded) {
				return last, errors.Join(ErrSensorTimeout, fmt.Errorf("sensor: expected blockNumber %d, last seen %d: %w",
					expected, blockNumberOf(last), cause))
			}
			// Parent context cancelled.
			return last, fmt.Errorf("sensor: context cancelled: %w", cause)
		case <-ticker.C:
		}
	}
}

// poll calls statecomp_get once and parses the response into a Snapshot.
func (s *Sensor) poll(ctx context.Context) (*Snapshot, error) {
	raw, err := s.client.StatecompGet(ctx)
	if err != nil {
		return nil, err
	}
	return parseSnapshot(raw)
}

// wireStatecomp is the subset of statecomp_get fields the sensor cares about.
type wireStatecomp struct {
	BlockNumber any `json:"blockNumber"`
	TrieStats   struct {
		AccountTrieBytes any `json:"accountTrieBytes"`
		StorageTrieBytes any `json:"storageTrieBytes"`
		CodeBytesTotal   any `json:"codeBytesTotal"`
	} `json:"trieStats"`
}

func parseSnapshot(raw json.RawMessage) (*Snapshot, error) {
	var w wireStatecomp
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("sensor: parse statecomp response: %w", err)
	}

	blockNumber, err := parseAnyUint64(w.BlockNumber)
	if err != nil {
		return nil, fmt.Errorf("sensor: blockNumber: %w", err)
	}
	accountBytes, err := parseAnyUint64(w.TrieStats.AccountTrieBytes)
	if err != nil {
		return nil, fmt.Errorf("sensor: accountTrieBytes: %w", err)
	}
	storageBytes, err := parseAnyUint64(w.TrieStats.StorageTrieBytes)
	if err != nil {
		return nil, fmt.Errorf("sensor: storageTrieBytes: %w", err)
	}
	codeBytes, err := parseAnyUint64(w.TrieStats.CodeBytesTotal)
	if err != nil {
		return nil, fmt.Errorf("sensor: codeBytesTotal: %w", err)
	}

	return &Snapshot{
		BlockNumber:      blockNumber,
		AccountTrieBytes: accountBytes,
		StorageTrieBytes: storageBytes,
		CodeBytesTotal:   codeBytes,
		Raw:              raw,
		ObservedAt:       time.Now(),
	}, nil
}

// parseAnyUint64 accepts hex strings ("0x..."), decimal strings, or JSON numbers.
// Nethermind serializes quantities as hex; some fixtures use decimal.
func parseAnyUint64(v any) (uint64, error) {
	switch val := v.(type) {
	case float64:
		return uint64(val), nil
	case string:
		s := strings.TrimPrefix(val, "0x")
		s = strings.TrimPrefix(s, "0X")
		var n uint64
		if _, err := fmt.Sscanf(s, "%x", &n); err != nil {
			// Try decimal fallback.
			if _, err2 := fmt.Sscanf(val, "%d", &n); err2 != nil {
				return 0, fmt.Errorf("parseAnyUint64 %q: not a valid integer", val)
			}
		}
		return n, nil
	case json.Number:
		n, err := val.Int64()
		if err != nil {
			return 0, fmt.Errorf("parseAnyUint64 json.Number %q: %w", val, err)
		}
		return uint64(n), nil
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("parseAnyUint64: unexpected type %T", v)
	}
}

func blockNumberOf(s *Snapshot) uint64 {
	if s == nil {
		return 0
	}
	return s.BlockNumber
}
