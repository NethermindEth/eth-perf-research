// Package facade implements the batch-builder + signer integration layer.
// A Dispatcher builds unsigned tx templates in-process via the native verb
// registry, fills in the fee/nonce/chainID fields, applies the gas-cap check,
// then signs the final slice and returns the raw signed RLPs together with
// cursor-advance metadata.
package facade

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/orchpb"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/signer"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// verbRegistry is the narrow lookup interface the Dispatcher needs. It is
// satisfied by verbs.Lookup; tests substitute a fake registry.
type verbRegistry func(name string) (verbs.Verb, bool)

// Dispatcher builds and signs one batch per call to Dispatch.
type Dispatcher struct {
	Lookup verbRegistry
	Signer *signer.Signer
}

// New constructs a Dispatcher backed by the native verb registry.
func New(s *signer.Signer) *Dispatcher {
	return &Dispatcher{Lookup: verbs.Lookup, Signer: s}
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
	RLPBytes    uint64   // sum of len(SignedRLP[i])
	TxRLPHashes [][]byte // Keccak256(SignedRLP[i]) for pending-sidecar
	NewCursor   uint64   // first nonce after this batch (StartNonce + TxCount)
	NewSalt     uint64   // first salt after this batch (StartSalt + NumNonces)

	// BuildDuration / SignDuration are the wall-clock cost of the in-process
	// verb-template construction and the parallel secp256k1 signing phases,
	// surfaced for per-phase timing instrumentation.
	BuildDuration time.Duration
	SignDuration  time.Duration
}

// Dispatch builds + signs one batch for in.Plan, using the caller-reserved
// nonce range [in.StartNonce, in.StartNonce+in.NumNonces) and salt range
// [in.StartSalt, in.StartSalt+in.NumNonces). Dispatch is goroutine-safe with
// respect to the Context: it only reads immutable fields plus the per-call
// fee policy (which the lifecycle refreshes pre-reservation).
//
// Building is an in-process loop over the native verb's BuildTx; there is no
// out-of-process worker. Dispatch trusts the controller's NMaxTxs to fit the
// deadline-byte budget. The gas cap is enforced as a hard ceiling: if the
// cumulative tx gas would exceed 0.95 × BlockGasLimit, Dispatch returns an
// error so the caller can fail the batch loudly rather than silently shipping
// an invalid block.
func (d *Dispatcher) Dispatch(_ context.Context, in DispatchInput, c *Context) (*Result, error) {
	if in.Plan == nil {
		return nil, errors.New("facade: plan must not be nil")
	}
	if c == nil {
		return nil, errors.New("facade: context must not be nil")
	}

	plan := in.Plan

	verb, ok := d.Lookup(plan.Verb)
	if !ok {
		return nil, fmt.Errorf("facade: unknown verb: %s", plan.Verb)
	}

	count := plan.NMaxTxs
	if count == 0 {
		count = 1 // always build at least one tx for forward-progress
	}

	buildCtx := verbs.BuildCtx{
		BaseAddress:   c.BaseAddress,
		Revision:      c.Revision,
		AddressStride: c.AddressStride,
		SaltBase:      in.StartSalt,
		Contracts:     c.Contracts,
	}

	maxFee, maxPri := c.LoadFeePolicy()
	if maxFee == nil {
		maxFee = new(big.Int)
	}
	if maxPri == nil {
		maxPri = new(big.Int)
	}

	// Hoist fee bytes out of the per-tx loop: constant for the entire batch.
	maxFeeBytes := maxFee.Bytes()
	maxPriBytes := maxPri.Bytes()
	chainID := c.ChainID
	buildStart := time.Now()
	signables := make([]*orchpb.TxIn, 0, count)
	for i := 0; i < count; i++ {
		idx := in.StartNonce + uint64(i)
		tmpl, err := verb.BuildTx(idx, buildCtx)
		if err != nil {
			return nil, fmt.Errorf("facade: build verb %s idx=%d: %w", plan.Verb, idx, err)
		}
		var to []byte
		if tmpl.To != nil {
			to = tmpl.To.Bytes()
		}
		var value []byte
		if tmpl.Value != nil && tmpl.Value.Sign() > 0 {
			value = tmpl.Value.Bytes()
		}
		signables = append(signables, &orchpb.TxIn{
			ChainId:              chainID,
			Nonce:                idx,
			Gas:                  tmpl.Gas,
			To:                   to,
			Value:                value,
			Data:                 tmpl.Data,
			MaxFeePerGas:         maxFeeBytes,
			MaxPriorityFeePerGas: maxPriBytes,
		})
	}
	buildDur := time.Since(buildStart)

	// Hard gas cap: cumulative tx gas must not exceed 0.95 × BlockGasLimit.
	// Silent trim is not safe under parallel planners — it creates nonce gaps.
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
		return nil, errors.New("facade: builder returned zero txs for non-zero plan")
	}

	signStart := time.Now()
	raws, err := d.Signer.SignBatch(context.Background(), signables)
	if err != nil {
		return nil, fmt.Errorf("facade: sign batch: %w", err)
	}
	signDur := time.Since(signStart)

	hashes := make([][]byte, len(raws))
	var totalBytes uint64
	for i, raw := range raws {
		hashes[i] = crypto.Keccak256(raw)
		totalBytes += uint64(len(raw))
	}

	txCount := len(raws)
	return &Result{
		SignedRLP:     raws,
		TxCount:       txCount,
		RLPBytes:      totalBytes,
		TxRLPHashes:   hashes,
		NewCursor:     in.StartNonce + uint64(txCount),
		NewSalt:       in.StartSalt + in.NumNonces,
		BuildDuration: buildDur,
		SignDuration:  signDur,
	}, nil
}
