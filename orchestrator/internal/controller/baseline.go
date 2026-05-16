package controller

// baseGasPerVerb is the cold-start per-tx gas budget used by Pick when sizing
// batches before VerbStats has enough EWMA samples to be trustworthy. Values
// are empirical upper-bounds observed in bloatnet journals; computeGasBasedMax
// targets gasFillFraction (0.90) of the block limit using these. Some verbs
// (storagespam, gasburnertx) genuinely consume 1.5-2M+ gas per tx, so
// under-estimating crashes the dispatcher's hard 0.95 × block-gas assertion.
// Per-verb estimates are upper bounds on the verb's exec-tx gas limit (the
// Gas field the verb sets), which now matches the EELS-adapted spamoor
// scenarios. Under-estimating crashes the dispatcher's hard 0.95 × block-gas
// assertion, so each entry is >= the verb's actual gas.
var baseGasPerVerb = map[string]uint64{
	"eoatx":           21_000,
	"deploytx":        1_000_000,
	"factorydeploytx": 500_000,
	"storagespam":     2_050_000,
	"storagerefundtx": 3_000_000,
	"erc20tx":         100_000,
	"erc20_bloater":   16_700_000,
	"uniswap_swaps":   200_000,
	"gasburnertx":     2_000_000,
	"calltx":          500_000,
	"evm_fuzz":        1_000_000,
	"noop":            21_000,
}

// defaultBaseGasPerVerb is used when a verb is missing from baseGasPerVerb.
// Conservative high value so unknown verbs do not blow the gas cap.
const defaultBaseGasPerVerb uint64 = 1_000_000

// baselineGasPerVerb returns the cold-start gas estimate for verb, defaulting
// to defaultBaseGasPerVerb for unknown verbs and clamping zero to the default
// (so a misconfigured table entry can't divide by zero downstream).
func baselineGasPerVerb(verb string) uint64 {
	g, ok := baseGasPerVerb[verb]
	if !ok || g == 0 {
		return defaultBaseGasPerVerb
	}
	return g
}
