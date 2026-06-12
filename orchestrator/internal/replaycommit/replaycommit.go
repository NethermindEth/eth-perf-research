// Package replaycommit re-executes a recorded payloads.rlp through Nethermind
// via testing_commitBlockV1 — which EXECUTES the recorded transactions and
// commits with NM's OWN computed state root under NoValidation — and records
// the resulting CLEAN blocks to an output payloads file.
//
// The recorded payloads carry correct transactions but a poisoned state-root
// tail (a wedge artifact). The normal Engine-API replay validates roots and
// would reject the poisoned ones. testing_commitBlockV1 instead discards the
// recorded root and recomputes everything from the txs, reproducing the
// genuinely-correct state and reaching the same block height as the input.
package replaycommit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/lifecycle"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/rpc"
)

// Committer is the subset of *rpc.Client the driver needs. Narrowed to an
// interface so tests can supply a deterministic fake.
type Committer interface {
	// TestingCommitBlockV1 executes signedTxs at timestamp and returns the
	// committed block hash (NM's own computed block, NoValidation).
	TestingCommitBlockV1(ctx context.Context, signedTxs [][]byte, timestamp uint64) (common.Hash, error)
	// BlockByHash fetches the committed block header by hash.
	BlockByHash(ctx context.Context, h common.Hash, withTxs bool) (*rpc.BlockHeader, error)
	// Call issues a raw JSON-RPC call (used for eth_blockNumber head detection).
	Call(ctx context.Context, method string, params []any, out any) error
}

// payloadWriter is the subset of *payloads.Writer the driver appends through.
type payloadWriter interface {
	Append(p *payloads.ExecutionPayloadV3) error
}

// payloadReader is the subset of *payloads.Reader the driver reads through.
type payloadReader interface {
	Next() (*payloads.ExecutionPayloadV3, error)
}

// Driver holds the dependencies for a replay-commit run.
type Driver struct {
	Client Committer
}

// Run reads payloads from in, re-executes each via testing_commitBlockV1, and
// appends the CLEAN committed block to out. Payloads at or below the EL head
// are skipped (fast-forward / resume). Returns nil at input EOF or on
// context cancellation; any commit / fetch / append error is returned (fail
// loud).
func (d *Driver) Run(ctx context.Context, in payloadReader, out payloadWriter) error {
	head, err := d.headBlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("replaycommit: detect EL head: %w", err)
	}
	slog.Info("replaycommit: starting", "el_head", head)

	var committed, skipped int
	var lastNumber uint64
	var lastCleanRoot common.Hash
	for {
		p, err := in.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("replaycommit: read payload (committed=%d): %w", committed, err)
		}

		if p.Number <= head {
			skipped++
			if skipped%50000 == 0 {
				slog.Info("replaycommit: fast-forward over applied blocks", "skipped", skipped, "el_head", head)
			}
			continue
		}

		// Re-execute the recorded txs at the recorded timestamp. NoValidation
		// accepts the non-increasing ts of the poisoned tail verbatim — keep it.
		hash, err := d.Client.TestingCommitBlockV1(ctx, p.Transactions, p.Timestamp)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("replaycommit: cancelled", "committed", committed, "last_block", lastNumber)
				return nil
			}
			return fmt.Errorf("replaycommit: testing_commitBlockV1 block %d: %w", p.Number, err)
		}

		block, err := d.Client.BlockByHash(ctx, hash, true)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("replaycommit: cancelled", "committed", committed, "last_block", lastNumber)
				return nil
			}
			return fmt.Errorf("replaycommit: BlockByHash %s (block %d): %w", hash.Hex(), p.Number, err)
		}

		// Mirror commitBatch: build the recordable payload from the committed
		// block header + the (recorded) signed txs. The committed payload carries
		// NM's CLEAN computed stateRoot/hash, NOT the input's poisoned root.
		clean := lifecycle.BuildExecutionPayloadV3(block, p.Transactions)
		if err := out.Append(clean); err != nil {
			return fmt.Errorf("replaycommit: append clean payload (block %d): %w", block.Number, err)
		}

		committed++
		lastNumber = block.Number
		lastCleanRoot = block.StateRoot

		if committed%100 == 0 {
			slog.Info("replaycommit: progress",
				"committed", committed,
				"block", block.Number,
				"clean_state_root", shortHash(block.StateRoot),
				"input_state_root", shortHash(p.StateRoot),
			)
		}

		if ctx.Err() != nil {
			slog.Info("replaycommit: cancelled", "committed", committed, "last_block", lastNumber)
			return nil
		}
	}

	slog.Info("replaycommit: complete",
		"committed", committed,
		"skipped", skipped,
		"last_block", lastNumber,
		"last_clean_state_root", lastCleanRoot.Hex(),
	)
	return nil
}

// RunFiles is the file-backed entry point: it opens the input reader and the
// append-mode output writer (resuming if the out file already has frames) and
// drives Run, fsyncing the output on completion.
func (d *Driver) RunFiles(ctx context.Context, inPath, outPath string) error {
	r, err := payloads.OpenReader(inPath)
	if err != nil {
		return fmt.Errorf("replaycommit: open input: %w", err)
	}
	defer r.Close()

	w, err := payloads.OpenWriter(outPath)
	if err != nil {
		return fmt.Errorf("replaycommit: open output: %w", err)
	}
	defer w.Close()

	if err := d.Run(ctx, r, w); err != nil {
		return err
	}
	return w.Sync()
}

// headBlockNumber returns the EL's current head via eth_blockNumber, used to
// skip already-applied payloads on resume.
func (d *Driver) headBlockNumber(ctx context.Context) (uint64, error) {
	var hexNum string
	if err := d.Client.Call(ctx, "eth_blockNumber", []any{}, &hexNum); err != nil {
		return 0, err
	}
	return hexutil.DecodeUint64(hexNum)
}

// shortHash renders the first 4 bytes of a hash ("0xabcdef12") so progress logs
// can show a clean-vs-poisoned state-root divergence at a glance.
func shortHash(h common.Hash) string {
	return h.Hex()[:10]
}
