// Package facade implements the batch-builder + signer integration layer.
// A Dispatcher calls a batchBuilder to obtain unsigned TxIn templates, fills
// in the fee/nonce/chainID fields that the Python worker omits, applies
// deadline-byte and gas-cap trimming, then signs the final slice and returns
// the raw signed RLPs together with cursor-advance metadata.
package facade

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/builderpool"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/signer"
)

// batchBuilder is the narrow interface the Dispatcher needs from builderpool.Pool.
// *builderpool.Pool satisfies it implicitly.
type batchBuilder interface {
	Build(ctx context.Context, req *orchpb.BuildBatchRequest) (*orchpb.BuildBatchResponse, error)
}

// Dispatcher builds and signs one batch per call to Dispatch.
type Dispatcher struct {
	Pool   batchBuilder
	Signer *signer.Signer
}

// New constructs a Dispatcher. pool must not be nil; it is usually a
// *builderpool.Pool but any batchBuilder is accepted (useful for tests).
func New(pool *builderpool.Pool, s *signer.Signer) *Dispatcher {
	return &Dispatcher{Pool: pool, Signer: s}
}

// Result is returned by Dispatch.
type Result struct {
	SignedRLP   [][]byte
	TxCount     int
	RLPBytes    uint64            // sum of len(SignedRLP[i])
	TxRLPHashes [][]byte          // Keccak256(SignedRLP[i]) for pending-sidecar
	NewCursor   uint64            // ctx.AddressCursor after this batch
	NewSalt     uint64            // ctx.SaltCursor after this batch
	VerbGasUsed map[string]uint64 // for EWMA feedback; may be empty
}

// Dispatch builds + signs one batch for plan.
//
// Side-effects: mutates c.AddressCursor and c.SaltCursor to reflect the number
// of transactions that were actually signed.
func (d *Dispatcher) Dispatch(ctx context.Context, plan *controller.BatchPlan, c *Context) (*Result, error) {
	if plan == nil {
		return nil, errors.New("facade: plan must not be nil")
	}
	if c == nil {
		return nil, errors.New("facade: context must not be nil")
	}

	// 1. Build the raw batch from the worker pool.
	count := uint32(plan.NMaxTxs)
	if count == 0 {
		count = 1 // always request at least one tx for forward-progress
	}
	req := &orchpb.BuildBatchRequest{
		Verb:     plan.Verb,
		StartIdx: c.AddressCursor,
		Count:    count,
		Ctx:      c.ToProto(),
	}
	resp, err := d.Pool.Build(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("facade: build batch: %w", err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("facade: worker error: %s", resp.Error)
	}

	signables := resp.Signables

	// 2. Fill per-tx fee/nonce/chainID fields omitted by the Python worker.
	maxFee := c.MaxFeePerGas
	maxPri := c.MaxPriorityFeePerGas
	if maxFee == nil {
		maxFee = new(big.Int)
	}
	if maxPri == nil {
		maxPri = new(big.Int)
	}
	for i, tx := range signables {
		tx.ChainId = c.ChainID
		tx.Nonce = c.AddressCursor + uint64(i)
		tx.MaxFeePerGas = maxFee.Bytes()
		tx.MaxPriorityFeePerGas = maxPri.Bytes()
	}

	// 3. Trim by deadline_bytes and gas cap.
	signables = trimSignables(signables, plan.DeadlineBytes, c.BlockGasLimit)

	if len(signables) == 0 {
		// Edge case: nothing survived trimming — return an empty result without
		// advancing cursors so the caller can handle forward-progress logic.
		return &Result{
			VerbGasUsed: map[string]uint64{},
		}, nil
	}

	// 4. Sign the trimmed slice.
	raws, err := d.Signer.SignBatch(ctx, signables)
	if err != nil {
		return nil, fmt.Errorf("facade: sign batch: %w", err)
	}

	// 5. Compute per-tx Keccak256 hashes and accumulate byte count.
	hashes := make([][]byte, len(raws))
	var totalBytes uint64
	for i, raw := range raws {
		hashes[i] = crypto.Keccak256(raw)
		totalBytes += uint64(len(raw))
	}

	// 6. Advance cursors.
	txCount := len(raws)
	c.AddressCursor += uint64(txCount)
	c.SaltCursor = resp.NewSaltCursor

	return &Result{
		SignedRLP:   raws,
		TxCount:     txCount,
		RLPBytes:    totalBytes,
		TxRLPHashes: hashes,
		NewCursor:   c.AddressCursor,
		NewSalt:     c.SaltCursor,
		VerbGasUsed: map[string]uint64{},
	}, nil
}

// estimateTxSize returns a conservative upper-bound on the signed RLP size for
// a TxIn. Matches the Python heuristic in _builder.py:
//
//	200 + len(data) + 50 * len(access_list)
func estimateTxSize(tx *orchpb.TxIn) int {
	n := 200 + len(tx.Data)
	for range tx.AccessList {
		n += 50
	}
	return n
}

// trimSignables enforces the deadline-bytes cap and the 0.95 × BlockGasLimit
// gas cap, returning the longest prefix of txs that fits within both limits.
// At least one tx is always kept for forward-progress (matches Python behaviour).
func trimSignables(txs []*orchpb.TxIn, deadlineBytes int, blockGasLimit uint64) []*orchpb.TxIn {
	if len(txs) == 0 {
		return txs
	}

	var (
		gasCeiling      uint64
		hasGasCap       = blockGasLimit > 0
		accBytes        int
		accGas          uint64
	)
	if hasGasCap {
		// integer arithmetic: floor(blockGasLimit * 95 / 100)
		gasCeiling = blockGasLimit * 95 / 100
	}

	for i, tx := range txs {
		est := estimateTxSize(tx)
		gas := tx.Gas

		if i > 0 {
			// Check limits before accepting tx[i] (first tx is always kept).
			if deadlineBytes > 0 && accBytes+est > deadlineBytes {
				return txs[:i]
			}
			if hasGasCap && accGas+gas > gasCeiling {
				return txs[:i]
			}
		}

		accBytes += est
		accGas += gas
	}
	return txs
}
