package verbs

import "github.com/ethereum/go-ethereum/common"

// Remaining fixed placeholder addresses. The contract-calling verbs
// (storagespam, erc20tx, storagerefundtx, gasburnertx, calltx, erc20_bloater,
// uniswap_swaps) no longer use placeholders — they target the contracts the
// bootstrap phase deploys, resolved via BuildCtx.Contracts. Only the two verbs
// whose mechanism does not depend on a pre-existing contract keep a fixed
// address:
//
//   - addrFactoryDeploytx — factorydeploytx calls a CREATE2 factory; the
//     factory itself is expected pre-deployed in lab-genesis.json (its
//     deployment is not part of this orchestrator's scope).
//   - addrBlobCombined — blob_combined is an unfinished type-2 stub (real
//     blob txs need KZG sidecars the signer does not yet produce).
var (
	addrFactoryDeploytx = common.HexToAddress("0x2222222222222222222222222222222222222222")
	addrBlobCombined    = common.HexToAddress("0x1000000000000000000000000000000000000000")
)
