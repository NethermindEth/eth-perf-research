package verbs

import "github.com/ethereum/go-ethereum/common"

// Remaining fixed placeholder address. The contract-calling verbs
// (storagespam, erc20tx, storagerefundtx, gasburnertx, calltx, erc20_bloater)
// no longer use placeholders — they target the contracts the bootstrap phase
// deploys, resolved via BuildCtx.Contracts. factorydeploytx is the one verb
// whose mechanism does not depend on a bootstrap-deployed contract: it calls a
// CREATE2 factory expected pre-deployed in lab-genesis.json (its deployment is
// not part of this orchestrator's scope).
//
// uniswap_swaps targets the EELS placeholder router address directly (see
// uniswap_swaps.go); it is not a bootstrap-deployed contract.
var addrFactoryDeploytx = common.HexToAddress("0x2222222222222222222222222222222222222222")
