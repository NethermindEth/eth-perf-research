package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/sensor"
)

// computeChainIdentity returns sha256(genesis_sha256_bytes || target_sha256_bytes).
// Both inputs are hex strings (with or without 0x prefix). They must decode to
// non-empty byte slices; mismatched lengths are accepted.
func computeChainIdentity(genesisHex, targetHex string) ([32]byte, error) {
	gb, err := decodeHexFlexible(genesisHex)
	if err != nil {
		return [32]byte{}, fmt.Errorf("lifecycle: genesis sha: %w", err)
	}
	tb, err := decodeHexFlexible(targetHex)
	if err != nil {
		return [32]byte{}, fmt.Errorf("lifecycle: target sha: %w", err)
	}
	h := sha256.New()
	h.Write(gb)
	h.Write(tb)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func decodeHexFlexible(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return nil, fmt.Errorf("empty hex")
	}
	return hex.DecodeString(s)
}

// reachedTarget returns true when each axis has met its share of the byte budget.
// Mirrors Python `_target_reached`.
func reachedTarget(obs *controller.Observation, t *controller.Target) bool {
	if obs == nil || t == nil {
		return false
	}
	totals := map[controller.Axis]uint64{
		controller.AxisAccounts: obs.AccountTrieBytes,
		controller.AxisStorage:  obs.StorageTrieBytes,
		controller.AxisCode:     obs.CodeBytesTotal,
	}
	for _, ax := range controller.Axes {
		share, ok := t.Shares[ax]
		if !ok {
			continue
		}
		need := uint64(share * float64(t.TotalBytes))
		if totals[ax] < need {
			return false
		}
	}
	return true
}

// buildExecutionPayloadV3 maps a fetched BlockHeader plus the dispatched
// signed-tx RLPs into the canonical Engine API ExecutionPayloadV3 shape.
//
// Fee recipient, prev_randao, logs_bloom, receipts_root and withdrawals are
// not part of the orchestrator's BlockHeader projection; we fill the zero
// values (matching the Python `_block_to_payload` for synthetic Nethermind
// commits, where the test commit path produces an empty miner/randao/etc.).
func buildExecutionPayloadV3(block *rpc.BlockHeader, signedRLP [][]byte) *payloads.ExecutionPayloadV3 {
	baseFee := block.BaseFee
	if baseFee == nil {
		baseFee = new(big.Int)
	}
	return &payloads.ExecutionPayloadV3{
		ParentHash:    block.ParentHash,
		FeeRecipient:  common.Address{},
		StateRoot:     block.StateRoot,
		ReceiptsRoot:  common.Hash{},
		LogsBloom:     make([]byte, 256),
		Random:        common.Hash{},
		Number:        block.Number,
		GasLimit:      block.GasLimit,
		GasUsed:       block.GasUsed,
		Timestamp:     block.Timestamp,
		ExtraData:     nil,
		BaseFeePerGas: new(big.Int).Set(baseFee),
		BlockHash:     block.Hash,
		Transactions:  signedRLP,
		Withdrawals:   nil,
		BlobGasUsed:   new(uint64),
		ExcessBlobGas: new(uint64),
	}
}

// flatFromAxisMap flattens a verb->axis->float map into "verb.axis" keys.
func flatFromAxisMap(m map[string]map[controller.Axis]float64) map[string]float64 {
	out := make(map[string]float64, len(m)*3)
	for verb, row := range m {
		for ax, v := range row {
			out[fmt.Sprintf("%s.%s", verb, ax)] = v
		}
	}
	return out
}

// shareTargetFromTarget converts a *target.Target-ish view (raw shares as
// strings) into a *controller.Target.
func shareTargetFromTarget(shares map[string]float64, totalBytes int64) *controller.Target {
	out := &controller.Target{
		Shares:     make(map[controller.Axis]float64, len(shares)),
		TotalBytes: totalBytes,
	}
	for k, v := range shares {
		if ax, ok := controller.ParseAxis(k); ok {
			out.Shares[ax] = v
		}
	}
	return out
}

// targetSha256Bytes decodes the hex digest stored on target.Target.SHA256
// into a raw 32-byte slice. Returns nil on error (we don't want to fail the
// run just because the digest is missing).
func targetSha256Bytes(hexDigest string) []byte {
	b, err := decodeHexFlexible(hexDigest)
	if err != nil {
		return nil
	}
	return b
}

// waitForValidSensor polls the state-comp sensor until it returns a snapshot
// with non-zero trie stats. During a bootstrap or recovery scan the plugin
// publishes all-zero gauges; committing batches under such an observation
// would feed garbage into the controller. We block the cycle here until the
// plugin is healthy. Honours ctx cancellation. pollGap and logEvery are the
// resolved RunConfig values: it logs once at start and every logEvery
// thereafter while waiting.
func waitForValidSensor(ctx context.Context, sens *sensor.Sensor, pollGap, logEvery time.Duration) (*sensor.Snapshot, error) {
	lastLog := time.Now().Add(-logEvery)
	for {
		snap, err := sens.PollOnce(ctx)
		if err == nil && snap != nil &&
			(snap.AccountTrieBytes != 0 || snap.StorageTrieBytes != 0 || snap.CodeBytesTotal != 0 || snap.BlockNumber != 0) {
			return snap, nil
		}
		if time.Since(lastLog) >= logEvery {
			if isTransportError(err) {
				// NM is unreachable mid-restart: tolerate the gap and keep
				// polling rather than failing the run — the sensor resumes
				// once NM answers again.
				slog.Warn("lifecycle: sensor poll hit transport error — NM unreachable, will keep polling", "err", err)
			} else {
				slog.Warn("lifecycle: sensor returns zero stats; waiting for state-comp plugin", "err", err)
			}
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollGap):
		}
	}
}

// waitSensorForBlock blocks until statecomp_get's blockNumber reaches target —
// i.e. the StateComposition trie-diff has folded the just-committed block into
// the sensor. It is the gate that makes the bloating loop strictly sensor-
// paced: the orchestrator never commits block N+1 until it holds fresh sensor
// data for block N. A baseline rescan freezes the sensor; this waits it out,
// logging every logEvery, rather than letting the loop race ahead. Returns a
// non-nil error only on ctx cancellation.
func waitSensorForBlock(ctx context.Context, sens *sensor.Sensor, target uint64, logEvery time.Duration) (*sensor.Snapshot, error) {
	lastLog := time.Now()
	for {
		// sens.Read polls statecomp_get until blockNumber >= target or its own
		// deadline; on timeout it returns ErrSensorTimeout with the last
		// snapshot. Retry across timeouts so the wait is unbounded.
		snap, err := sens.Read(ctx, target)
		if err == nil && snap != nil && snap.BlockNumber >= target {
			return snap, nil
		}
		if ctx.Err() != nil {
			return snap, ctx.Err()
		}
		if time.Since(lastLog) >= logEvery {
			var seen uint64
			if snap != nil {
				seen = snap.BlockNumber
			}
			slog.Warn("lifecycle: waiting for sensor to fold in committed block — trie-diff lagging or rescan in progress",
				"target_block", target, "sensor_block", seen)
			lastLog = time.Now()
		}
	}
}
