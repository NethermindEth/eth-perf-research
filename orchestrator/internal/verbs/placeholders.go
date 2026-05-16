package verbs

import "github.com/ethereum/go-ethereum/common"

// Placeholder contract addresses pre-deployed in lab-genesis.json. Each verb
// targets one of these; ported verbatim from orchestrator-py facade/verbs.py
// (SPAMOOR_PLACEHOLDERS).
var (
	addrCalltx          = common.HexToAddress("0x1111111111111111111111111111111111111111")
	addrFactoryDeploytx = common.HexToAddress("0x2222222222222222222222222222222222222222")
	addrGasburnertx     = common.HexToAddress("0x3333333333333333333333333333333333333333")
	addrUniswapSwaps    = common.HexToAddress("0x4444444444444444444444444444444444444444")
	addrErc20tx         = common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	addrStorageSpam     = common.HexToAddress("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	addrErc20Bloater    = common.HexToAddress("0xdddddddddddddddddddddddddddddddddddddddd")
	addrStorageRefund   = common.HexToAddress("0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	addrBlobCombined    = common.HexToAddress("0x1000000000000000000000000000000000000000")
)
