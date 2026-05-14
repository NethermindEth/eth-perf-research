package controller

// baseGasPerVerb is the cold-start per-tx gas budget used by Pick when sizing
// batches before VerbStats has enough EWMA samples to be trustworthy. Values
// are empirical upper-bounds observed in bloatnet journals; the safety margin
// (gasCapSafetyMargin) is multiplied on top in computeGasBasedMax. Some verbs
// (storagespam, gasburnertx) genuinely consume 1.5-2M+ gas per tx, so
// under-estimating crashes the dispatcher's hard 0.95 × block-gas assertion.
var baseGasPerVerb = map[string]uint64{
	"eoatx":           21_000,
	"deploytx":        200_000,
	"factorydeploytx": 200_000,
	"storagespam":     2_500_000,
	"storagerefundtx": 100_000,
	"erc20tx":         100_000,
	"erc20_bloater":   1_000_000,
	"uniswap_swaps":   300_000,
	"gasburnertx":     1_500_000,
	"calltx":          100_000,
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
