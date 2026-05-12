// Package probe runs a characterisation sequence (N txs per verb in single
// blocks) to seed the controller F matrix and AvgTxRLP from real measurements
// before the main control loop starts.
package probe

import (
	"context"
	"fmt"
	"math"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/controller"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/referencef"
)

const defaultTxCount = 100
const defaultSanityGate = 3.0

// Runner is the integration seam: submit txs of verb V, observe state delta.
// In production this is wired to the builder pool + signer + RPC.
// In tests it is a fake.
type Runner interface {
	RunOneVerb(ctx context.Context, verb string, count int) (txRLPBytes uint64, pre, post controller.Observation, err error)
}

// Result is returned after the probe runs all verbs.
type Result struct {
	F        map[string]map[controller.Axis]float64
	AvgTxRLP map[string]float64
}

// Probe drives the characterisation sequence.
type Probe struct {
	Verbs      []string
	TxCount    int
	SanityGate float64 // multiplier; <=0 disables gate
}

// New constructs a Probe with the given parameters.
func New(verbs []string, txCount int, sanityGate float64) *Probe {
	verbsCopy := make([]string, len(verbs))
	copy(verbsCopy, verbs)
	if txCount <= 0 {
		txCount = defaultTxCount
	}
	if sanityGate == 0 {
		sanityGate = defaultSanityGate
	}
	return &Probe{
		Verbs:      verbsCopy,
		TxCount:    txCount,
		SanityGate: sanityGate,
	}
}

// Run iterates verbs, calls runner.RunOneVerb for each, collects F and avg_tx_rlp.
// If ref is non-nil, asserts the probed F is within SanityGate× of the reference.
// Returns an error if any verb fails or the sanity gate trips.
func (p *Probe) Run(ctx context.Context, runner Runner, ref *referencef.ReferenceF) (*Result, error) {
	f := make(map[string]map[controller.Axis]float64, len(p.Verbs))
	avgTxRLP := make(map[string]float64, len(p.Verbs))

	for _, verb := range p.Verbs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("probe: context cancelled before verb %s: %w", verb, err)
		}

		rlpBytes, pre, post, err := runner.RunOneVerb(ctx, verb, p.TxCount)
		if err != nil {
			return nil, fmt.Errorf("probe: verb %s: %w", verb, err)
		}

		count := float64(p.TxCount)
		measured := map[controller.Axis]float64{
			controller.AxisAccounts: float64(post.AccountTrieBytes-pre.AccountTrieBytes) / count,
			controller.AxisStorage:  float64(post.StorageTrieBytes-pre.StorageTrieBytes) / count,
			controller.AxisCode:     float64(post.CodeBytesTotal-pre.CodeBytesTotal) / count,
		}

		if ref != nil && p.SanityGate > 0 {
			if err := sanityGate(verb, measured, ref, p.SanityGate); err != nil {
				return nil, err
			}
		}

		f[verb] = measured

		if rlpBytes > 0 {
			avgTxRLP[verb] = float64(rlpBytes) / count
		}
	}

	return &Result{F: f, AvgTxRLP: avgTxRLP}, nil
}

// sanityGate checks that the measured F is within multiplier× of the reference.
// For storagerefundtx.storage the comparison is on absolute magnitudes.
// If ref_val == 0 and |measured| > multiplier, the gate trips.
func sanityGate(verb string, measured map[controller.Axis]float64, ref *referencef.ReferenceF, multiplier float64) error {
	refVerb := ref.Verbs[verb] // nil if verb not in reference → treated as all-zero

	for _, ax := range controller.Axes {
		refVal := refVerb[string(ax)] // 0 if not present
		measVal := measured[ax]

		// storagerefundtx.storage: compare magnitudes only (sign may flip).
		if verb == "storagerefundtx" && ax == controller.AxisStorage {
			refVal = math.Abs(refVal)
			measVal = math.Abs(measVal)
		}

		if refVal == 0 {
			if math.Abs(measVal) > multiplier {
				return fmt.Errorf(
					"probe: sanity gate: %s.%s: measured %.2f vs reference 0",
					verb, ax, measured[ax],
				)
			}
			continue
		}

		ratio := math.Abs(measVal) / math.Max(math.Abs(refVal), 1e-6)
		if ratio > multiplier || ratio < 1.0/multiplier {
			return fmt.Errorf(
				"probe: sanity gate: %s.%s: measured %.2f vs reference %.2f (ratio %.2f)",
				verb, ax, measured[ax], refVerb[string(ax)], ratio,
			)
		}
	}
	return nil
}
