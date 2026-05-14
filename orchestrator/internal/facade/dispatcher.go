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

// DispatchInput carries the caller-supplied parameters for one Dispatch call.
// StartNonce and StartSalt are the atomically reserved cursor ranges; Dispatch
// no longer reads or mutates the Context cursors.
type DispatchInput struct {
	Plan       *controller.BatchPlan
	StartNonce uint64
	StartSalt  uint64
	NumNonces  uint64
}

// Result is returned by Dispatch.
type Result struct {
	SignedRLP   [][]byte
	TxCount     int
	RLPBytes    uint64            // sum of len(SignedRLP[i])
	TxRLPHashes [][]byte          // Keccak256(SignedRLP[i]) for pending-sidecar
	NewCursor   uint64            // first nonce after this batch (StartNonce + TxCount)
	NewSalt     uint64            // first salt after this batch (StartSalt + NumNonces)
	VerbGasUsed map[string]uint64 // for EWMA feedback; may be empty
}

// Dispatch builds + signs one batch for in.Plan, using the caller-reserved
// nonce range [in.StartNonce, in.StartNonce+in.NumNonces) and salt range
// [in.StartSalt, in.StartSalt+in.NumNonces). Dispatch is goroutine-safe with
// respect to the Context: it only reads immutable fields plus the per-call
// fee policy (which the lifecycle refreshes pre-reservation).
//
// Dispatch trusts the controller's NMaxTxs to fit the deadline-byte budget;
// no defensive trim is applied because mid-flight trimming would create nonce
// gaps under parallel planners (the reservation is atomic and committed
// before Dispatch returns). The gas cap is enforced as a hard ceiling: if
// the cumulative tx gas would exceed 0.95 × BlockGasLimit, Dispatch returns
// an error so the caller can fail the batch loudly rather than silently
// shipping an invalid block.
func (d *Dispatcher) Dispatch(ctx context.Context, in DispatchInput, c *Context) (*Result, error) {
	if in.Plan == nil {
		return nil, errors.New("facade: plan must not be nil")
	}
	if c == nil {
		return nil, errors.New("facade: context must not be nil")
	}

	plan := in.Plan

	// 1. Build the raw batch from the worker pool.
	count := uint32(plan.NMaxTxs)
	if count == 0 {
		count = 1 // always request at least one tx for forward-progress
	}
	ctxProto := c.ToProto()
	ctxProto.SaltCursor = in.StartSalt
	req := &orchpb.BuildBatchRequest{
		Verb:     plan.Verb,
		StartIdx: in.StartNonce,
		Count:    count,
		Ctx:      ctxProto,
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
	maxFee, maxPri := c.LoadFeePolicy()
	if maxFee == nil {
		maxFee = new(big.Int)
	}
	if maxPri == nil {
		maxPri = new(big.Int)
	}
	for i, tx := range signables {
		tx.ChainId = c.ChainID
		tx.Nonce = in.StartNonce + uint64(i)
		tx.MaxFeePerGas = maxFee.Bytes()
		tx.MaxPriorityFeePerGas = maxPri.Bytes()
	}

	// 3. Hard gas cap: assert cumulative tx gas <= 0.95 × BlockGasLimit. We
	// can't silently trim under parallel planners — it would create nonce
	// gaps. The controller's Pick is responsible for sizing the plan to fit;
	// if it didn't, fail the batch so the operator can fix the model.
	if blockGas := c.LoadBlockGasLimit(); blockGas > 0 {
		ceiling := blockGas * 95 / 100
		var accGas uint64
		for _, tx := range signables {
			accGas += tx.Gas
		}
		if accGas > ceiling {
			return nil, fmt.Errorf("facade: gas cap exceeded: cumulative=%d ceiling=%d txs=%d verb=%s",
				accGas, ceiling, len(signables), plan.Verb)
		}
	}

	if len(signables) == 0 {
		// Edge case: worker returned no txs. Caller's reserved nonce range
		// becomes a hole; surface as error so the planner shuts down rather
		// than silently leaking nonces.
		return nil, errors.New("facade: builder returned zero txs for non-zero plan")
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

	// 6. Compute cursor-advance results. Note: NewSalt is StartSalt+NumNonces
	// even if trimming dropped tail txs — the salt range was already reserved
	// atomically and unused slots are simply abandoned (salt domain is 2^64).
	txCount := len(raws)
	return &Result{
		SignedRLP:   raws,
		TxCount:     txCount,
		RLPBytes:    totalBytes,
		TxRLPHashes: hashes,
		NewCursor:   in.StartNonce + uint64(txCount),
		NewSalt:     in.StartSalt + in.NumNonces,
		VerbGasUsed: map[string]uint64{},
	}, nil
}

