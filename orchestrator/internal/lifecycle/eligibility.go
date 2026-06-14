package lifecycle

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/verbs"
)

// verbExclusion records why a configured verb was excluded from the run's
// candidate set.
type verbExclusion struct {
	verb   string
	reason string
}

// eligibilityResult is the outcome of computeEligibleVerbs: the verbs that may
// be selected this run and the reason each excluded verb was dropped.
type eligibilityResult struct {
	eligible   []string // contract-eligible verbs, in configuredVerbs order
	exclusions []verbExclusion
}

// computeEligibleVerbs partitions configuredVerbs into the set that may run on
// this chain and the set that cannot. A verb is contract-eligible iff every
// contract it declares (verbs.VerbContractDeps) is present in the
// deployed-and-verified registry. A verb with no contract dependency is always
// eligible.
//
// reg is the bootstrap's registry — every address in it has been cross-checked
// by the eth_getCode fail-loud guard, so registry membership is exactly the
// "deployed and has code on-chain" set the eligibility gate needs.
func computeEligibleVerbs(configuredVerbs []string, reg *verbs.ContractRegistry) eligibilityResult {
	var res eligibilityResult
	for _, v := range configuredVerbs {
		deps := verbs.VerbContractDeps(v)
		missing := missingContracts(deps, reg)
		if len(missing) == 0 {
			res.eligible = append(res.eligible, v)
			continue
		}
		res.exclusions = append(res.exclusions, verbExclusion{
			verb:   v,
			reason: fmt.Sprintf("required contract %s not deployed", strings.Join(missing, ", ")),
		})
	}
	return res
}

// missingContracts returns the names of the deps absent from reg, in sorted
// order for a deterministic log line.
func missingContracts(deps []verbs.ContractName, reg *verbs.ContractRegistry) []string {
	var missing []string
	for _, c := range deps {
		if _, ok := reg.Get(c); !ok {
			missing = append(missing, string(c))
		}
	}
	sort.Strings(missing)
	return missing
}

// logEligibility emits a single structured log line listing the eligible verbs
// and, for each excluded verb, the contract-dependency reason it was dropped.
// This makes the exclusion visible and debuggable on the live system.
func logEligibility(res eligibilityResult) {
	excluded := make([]string, 0, len(res.exclusions))
	for _, ex := range res.exclusions {
		excluded = append(excluded, fmt.Sprintf("%s: excluded — %s", ex.verb, ex.reason))
	}
	slog.Info("lifecycle: verb eligibility resolved",
		"eligible", strings.Join(res.eligible, ","),
		"excluded", strings.Join(excluded, "; "),
	)
}
