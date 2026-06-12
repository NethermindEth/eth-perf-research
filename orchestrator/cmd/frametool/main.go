package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/payloads"
)

// frametool <payloads.rlp> <blockNumber>: prints the recorded frame's tx senders,
// nonces, and fee fields for divergence debugging.
func main() {
	target, _ := strconv.ParseUint(os.Args[2], 10, 64)
	r, err := payloads.OpenReader(os.Args[1])
	if err != nil { panic(err) }
	defer r.Close()
	if _, err := r.SkipToBlock(target - 1); err != nil { panic(err) }
	for {
		p, err := r.Next()
		if err == io.EOF { fmt.Println("not found"); return }
		if err != nil { panic(err) }
		if p.Number != target { continue }
		out := map[string]any{"number": p.Number, "txCount": len(p.Transactions), "gasLimit": p.GasLimit, "gasUsed": p.GasUsed, "timestamp": p.Timestamp, "baseFee": p.BaseFeePerGas.String(), "stateRoot": p.StateRoot.Hex()}
		var txs []map[string]any
		for i, raw := range p.Transactions {
			if i >= 3 && i < len(p.Transactions)-1 { continue }
			var tx types.Transaction
			if err := tx.UnmarshalBinary(raw); err != nil { txs = append(txs, map[string]any{"i": i, "err": err.Error()}); continue }
			signer := types.LatestSignerForChainID(tx.ChainId())
			from, ferr := types.Sender(signer, &tx)
			rec := map[string]any{"i": i, "from": from.Hex(), "chainId": tx.ChainId().String(), "nonce": tx.Nonce(), "gas": tx.Gas(), "to": fmt.Sprint(tx.To())}
			if ferr != nil { rec["senderErr"] = ferr.Error() }
			txs = append(txs, rec)
		}
		out["txs"] = txs
		b, _ := json.MarshalIndent(out, "", " ")
		fmt.Println(string(b))
		return
	}
}
