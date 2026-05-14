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
		LogsBloom:     [256]byte{},
		PrevRandao:    common.Hash{},
		BlockNumber:   block.Number,
		GasLimit:      block.GasLimit,
		GasUsed:       block.GasUsed,
		Timestamp:     block.Timestamp,
		ExtraData:     nil,
		BaseFeePerGas: new(big.Int).Set(baseFee),
		BlockHash:     block.Hash,
		Transactions:  signedRLP,
		Withdrawals:   nil,
		BlobGasUsed:   0,
		ExcessBlobGas: 0,
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

// observationFromSnapshot builds a controller.Observation from a sensor
// snapshot. Returns nil if snap is nil.
func observationFromSnapshot(snap *sensorSnapshotLike) *controller.Observation {
	if snap == nil {
		return nil
	}
	return &controller.Observation{
		AccountTrieBytes: snap.AccountTrieBytes,
		StorageTrieBytes: snap.StorageTrieBytes,
		CodeBytesTotal:   snap.CodeBytesTotal,
		BlockNumber:      snap.BlockNumber,
	}
}

// sensorSnapshotLike is the minimal shape we need from sensor.Snapshot — we
// keep it free of imports so helpers can be tested without the sensor pkg.
type sensorSnapshotLike struct {
	AccountTrieBytes uint64
	StorageTrieBytes uint64
	CodeBytesTotal   uint64
	BlockNumber      uint64
}

// shareTargetFromTarget converts a *target.Target-ish view (raw shares as
// strings) into a *controller.Target.
func shareTargetFromTarget(shares map[string]float64, totalBytes int64, sha256Hex string) *controller.Target {
	out := &controller.Target{
		Shares:     make(map[controller.Axis]float64, len(shares)),
		TotalBytes: totalBytes,
		SHA256Hex:  sha256Hex,
	}
	for k, v := range shares {
		out.Shares[controller.Axis(k)] = v
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
// plugin is healthy. Honours ctx cancellation. Logs once at start and every
// 30s thereafter while waiting.
func waitForValidSensor(ctx context.Context, sens *sensor.Sensor) (*sensor.Snapshot, error) {
	const pollGap = 2 * time.Second
	logEvery := 30 * time.Second
	lastLog := time.Now().Add(-logEvery)
	for {
		snap, err := sens.PollOnce(ctx)
		if err == nil && snap != nil &&
			(snap.AccountTrieBytes != 0 || snap.StorageTrieBytes != 0 || snap.CodeBytesTotal != 0 || snap.BlockNumber != 0) {
			return snap, nil
		}
		if time.Since(lastLog) >= logEvery {
			slog.Warn("lifecycle: sensor returns zero stats; waiting for state-comp plugin", "err", err)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollGap):
		}
	}
}
