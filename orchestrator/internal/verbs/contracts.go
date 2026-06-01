package verbs

import (
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
)

// ContractName identifies a Spamoor scenario contract the orchestrator must
// deploy once at startup before any contract-calling verb can do real work.
type ContractName string

const (
	// ContractStorageSpam backs the storagespam verb (and the erc20_bloater
	// and uniswap_swaps stand-ins). setRandomForGas writes pseudo-random
	// storage slots — the primary storage bloater.
	ContractStorageSpam ContractName = "StorageSpam"
	// ContractTestToken backs the erc20tx verb. transferMint grows the
	// token's balance mapping.
	ContractTestToken ContractName = "TestToken"
	// ContractStorageRefund backs the storagerefundtx verb. execute writes
	// and clears storage windows to exercise SSTORE refunds.
	ContractStorageRefund ContractName = "StorageRefund"
	// ContractGasBurner backs the gasburnertx verb. Its runtime loops,
	// burning gas until a remainder threshold, then emits one LOG1.
	ContractGasBurner ContractName = "GasBurner"
	// ContractUniswapRouter is the Uniswap V2 router the uniswap_swaps verb
	// targets. It is intentionally NOT in contractCatalog: the orchestrator's
	// bootstrap has no init code for a Uniswap router/pool, so this contract
	// can never enter the deployed-and-verified set. A verb declaring it as a
	// dependency is therefore structurally excluded on any devnet that does
	// not already host Uniswap — exactly the desired behaviour.
	ContractUniswapRouter ContractName = "UniswapRouter"
)

// ContractSpec describes one contract the bootstrap phase deploys.
type ContractSpec struct {
	Name ContractName
	// InitCode is the CREATE-tx calldata (creation/init bytecode).
	InitCode []byte
	// Gas is the gas limit for the bootstrap CREATE transaction.
	Gas uint64
}

// contractCatalog returns the deterministic, ordered list of contracts the
// bootstrap phase deploys. Order is fixed so CreateAddress nonce arithmetic is
// reproducible across runs. The gas burner init code is assembled at call time
// from the geas templates (gasBurnerCreationCode). deployGas is the per-CREATE-tx
// gas limit from RunConfig.
func contractCatalog(deployGas uint64) []ContractSpec {
	return []ContractSpec{
		{Name: ContractStorageSpam, InitCode: mustHex(storageSpamInitHex), Gas: deployGas},
		{Name: ContractTestToken, InitCode: mustHex(testTokenInitHex), Gas: deployGas},
		{Name: ContractStorageRefund, InitCode: mustHex(storageRefundInitHex), Gas: deployGas},
		{Name: ContractGasBurner, InitCode: gasBurnerCreationCode(), Gas: deployGas},
	}
}

// ContractRegistry maps a deployed contract name to its on-chain address. It
// is produced by the bootstrap phase, persisted to the state dir, and threaded
// into every contract-calling verb via BuildCtx.
type ContractRegistry struct {
	Contracts map[ContractName]common.Address
}

// NewContractRegistry returns an empty registry.
func NewContractRegistry() *ContractRegistry {
	return &ContractRegistry{Contracts: map[ContractName]common.Address{}}
}

// Set records a deployed contract address.
func (r *ContractRegistry) Set(name ContractName, addr common.Address) {
	r.Contracts[name] = addr
}

// Get returns the deployed address for name. The bool is false when the
// contract is absent — callers that need the address must fail loudly.
func (r *ContractRegistry) Get(name ContractName) (common.Address, bool) {
	if r == nil {
		return common.Address{}, false
	}
	a, ok := r.Contracts[name]
	return a, ok
}

// MustGet returns the deployed address for name or panics. Verbs call this
// after the lifecycle's fail-loud guard has already asserted every contract
// has code, so a missing entry here is a programming error.
func (r *ContractRegistry) MustGet(name ContractName) common.Address {
	a, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("verbs: contract %q not in registry", name))
	}
	return a
}

// Names returns the registry's contract names in sorted order.
func (r *ContractRegistry) Names() []ContractName {
	out := make([]ContractName, 0, len(r.Contracts))
	for n := range r.Contracts {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// contractVerbTargets maps each contract-calling verb to the contract it must
// target. Verbs absent from this map (eoatx, deploytx, factorydeploytx) need
// no pre-deployed contract.
//
//   - storagespam      → StorageSpam     (setRandomForGas — real storage write)
//   - erc20tx          → TestToken       (transferMint — real mint/transfer)
//   - storagerefundtx  → StorageRefund   (execute — real refund method)
//   - gasburnertx      → GasBurner       (geas gas-burner)
//   - calltx           → StorageSpam     (getStorage — non-reverting touch)
//   - erc20_bloater    → StorageSpam     (setRandomForGas — bulk storage write;
//     shares StorageSpam with storagespam,
//     it has no dedicated Spamoor contract)
//
// uniswap_swaps is absent: it targets a fixed router placeholder address that
// the bootstrap deployer cannot CREATE (verbTarget is never called for it).
// Its real dependency is declared in verbContractDeps below, not here.
var contractVerbTargets = map[string]ContractName{
	"storagespam":     ContractStorageSpam,
	"erc20tx":         ContractTestToken,
	"storagerefundtx": ContractStorageRefund,
	"gasburnertx":     ContractGasBurner,
	"calltx":          ContractStorageSpam,
	"erc20_bloater":   ContractStorageSpam,
}

// verbContractDeps maps every verb to the full set of contracts it needs
// deployed-and-verified on-chain before it can do real work. It is the
// authoritative verb→contract-dependency declaration the controller's
// eligibility gate consumes.
//
// It is a superset of contractVerbTargets: a verb in contractVerbTargets
// depends on its target contract, AND uniswap_swaps — which has no catalog
// target but does require a Uniswap V2 router at its placeholder address —
// declares ContractUniswapRouter here. Because ContractUniswapRouter is not in
// contractCatalog the bootstrap never deploys it, so uniswap_swaps can satisfy
// its dependency only on a chain that already hosts Uniswap.
//
// A verb absent from this map (eoatx, deploytx, factorydeploytx) has no
// contract dependency and is always contract-eligible.
var verbContractDeps = map[string][]ContractName{
	"storagespam":     {ContractStorageSpam},
	"erc20tx":         {ContractTestToken},
	"storagerefundtx": {ContractStorageRefund},
	"gasburnertx":     {ContractGasBurner},
	"calltx":          {ContractStorageSpam},
	"erc20_bloater":   {ContractStorageSpam},
	"uniswap_swaps":   {ContractUniswapRouter},
}

// ContractVerbTarget returns the contract a verb must target, or false when
// the verb needs no pre-deployed contract.
func ContractVerbTarget(verb string) (ContractName, bool) {
	c, ok := contractVerbTargets[verb]
	return c, ok
}

// VerbContractDeps returns the contracts a verb depends on. An empty slice
// means the verb has no contract dependency (it is always contract-eligible).
func VerbContractDeps(verb string) []ContractName {
	return verbContractDeps[verb]
}

// RequiredContracts returns the set of contracts the given verbs require, in
// catalog order. Used by the bootstrap phase to deploy only what is needed and
// by the fail-loud guard to know which addresses must have code. The catalog
// order does not depend on the deploy-gas value, so a zero is passed here.
func RequiredContracts(verbs []string) []ContractName {
	want := map[ContractName]bool{}
	for _, v := range verbs {
		if c, ok := contractVerbTargets[v]; ok {
			want[c] = true
		}
	}
	out := make([]ContractName, 0, len(want))
	for _, spec := range contractCatalog(0) {
		if want[spec.Name] {
			out = append(out, spec.Name)
		}
	}
	return out
}

// ContractSpecFor returns the catalog spec for a contract name, with the spec's
// Gas set to deployGas (the resolved RunConfig per-CREATE-tx gas limit).
func ContractSpecFor(name ContractName, deployGas uint64) (ContractSpec, bool) {
	for _, spec := range contractCatalog(deployGas) {
		if spec.Name == name {
			return spec, true
		}
	}
	return ContractSpec{}, false
}

// ContractInitCodeHex returns a contract's init code as a 0x-prefixed hex
// string — used by the bootstrap deployer's logging so the exact deploy tx
// payload is recoverable from the run log.
func ContractInitCodeHex(spec ContractSpec) string {
	return "0x" + hex.EncodeToString(spec.InitCode)
}

// verbTarget resolves the deployed contract address a verb must call. It
// returns an error when the registry is missing the verb's contract — verbs
// surface this so the dispatcher fails the batch loudly rather than sending a
// no-op tx to a dead address.
func verbTarget(ctx BuildCtx, verb string) (common.Address, error) {
	name, ok := ContractVerbTarget(verb)
	if !ok {
		return common.Address{}, fmt.Errorf("verbs: %s is not a contract-calling verb", verb)
	}
	addr, ok := ctx.Contracts.Get(name)
	if !ok {
		return common.Address{}, fmt.Errorf("verbs: %s target contract %q not in registry", verb, name)
	}
	return addr, nil
}
