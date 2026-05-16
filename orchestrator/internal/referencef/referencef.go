package referencef

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

type ReferenceF struct {
	Verbs    map[string]map[string]float64 `json:"verbs"`
	AvgTxRLP map[string]float64            `json:"avg_tx_rlp"`
}

func Load(path string) (*ReferenceF, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("referencef: read %s: %w", path, err)
	}
	var rf ReferenceF
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("referencef: parse %s: %w", path, err)
	}
	return &rf, nil
}

// DefaultReferenceF returns the built-in per-verb per-axis seed footprints
// (bytes per tx) from design-v3 §B.1's REFERENCE_F table.
//
// WHY this exists: the controller seeds its effect matrix F from a reference-F
// in NewState. An all-zero F-row gives a verb a zero gradient in the
// ‖Fx − r‖² objective (∂/∂w = 2·F_colᵀ·(Fx − r) ≡ 0 when F_col is zero), so the
// projected-gradient optimiser can never assign that verb any weight — it never
// runs, F never learns from a measurement, and the verb is permanently dead
// state. With no --reference-f file on the live system loadReferenceF returned
// an empty table and EVERY verb seeded to zero, structurally trapping the
// account-creating eoatx verb. A non-zero seed for every verb breaks that trap:
// the gradient "sees" each verb from batch 1. Apply's online learning then
// overwrites the seed with measured values once a verb has actually run.
//
// The first nine verbs are design-v3 §B.1 verbatim. gasburnertx, evm_fuzz,
// noop and the blob_combined stub are not in §B.1's table — they emit ≈0 state,
// so they are seeded small but non-zero on every axis purely so they too have a
// non-zero gradient (the optimiser correctly down-weights them once observed).
// storagerefundtx's storage value is negative by design (SSTORE→0 shrinkage).
func DefaultReferenceF() *ReferenceF {
	return &ReferenceF{
		Verbs: map[string]map[string]float64{
			"eoatx":           {"accounts": 160, "storage": 10, "code": 0},
			"calltx":          {"accounts": 8, "storage": 20, "code": 0},
			"deploytx":        {"accounts": 160, "storage": 10, "code": 3500},
			"factorydeploytx": {"accounts": 160, "storage": 10, "code": 2200},
			"storagespam":     {"accounts": 5, "storage": 191, "code": 0},
			"erc20_bloater":   {"accounts": 10, "storage": 160, "code": 0},
			"erc20tx":         {"accounts": 5, "storage": 64, "code": 0},
			"uniswap_swaps":   {"accounts": 8, "storage": 220, "code": 0},
			"storagerefundtx": {"accounts": 5, "storage": -180, "code": 0},
			"gasburnertx":     {"accounts": 1, "storage": 1, "code": 1},
			"evm_fuzz":        {"accounts": 1, "storage": 1, "code": 1},
			"noop":            {"accounts": 1, "storage": 1, "code": 1},
			"blob_combined":   {"accounts": 1, "storage": 1, "code": 1},
		},
		AvgTxRLP: map[string]float64{},
	}
}

// WithDefaults returns a ReferenceF in which every verb missing from rf (or
// missing an individual axis) is backfilled from DefaultReferenceF. This
// guarantees no verb keeps an all-zero F-row even when a supplied --reference-f
// file is partial — a partial file must not reintroduce the cold-start
// zero-gradient trap. Values explicitly present in rf always win.
func WithDefaults(rf *ReferenceF) *ReferenceF {
	def := DefaultReferenceF()
	out := &ReferenceF{
		Verbs:    make(map[string]map[string]float64, len(def.Verbs)),
		AvgTxRLP: make(map[string]float64),
	}
	for verb, defRow := range def.Verbs {
		row := make(map[string]float64, len(defRow))
		for axis, v := range defRow {
			row[axis] = v
		}
		out.Verbs[verb] = row
	}
	if rf != nil {
		for verb, suppliedRow := range rf.Verbs {
			row, ok := out.Verbs[verb]
			if !ok {
				row = make(map[string]float64, len(suppliedRow))
				out.Verbs[verb] = row
			}
			for axis, v := range suppliedRow {
				row[axis] = v
			}
		}
		for verb, v := range rf.AvgTxRLP {
			out.AvgTxRLP[verb] = v
		}
	}
	return out
}
