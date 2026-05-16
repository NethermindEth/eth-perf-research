package verbs

import (
	"encoding/hex"
	"encoding/json"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// updateGolden, when set via `go test -update`, rewrites testdata/golden.json
// from the current verb output instead of asserting against it. Used to
// regenerate the vectors after a deliberate verb change.
var updateGolden = flag.Bool("update", false, "regenerate testdata/golden.json from current verb output")

// goldenEntry is one tx template vector. Since the verbs are now native Go (no
// Python oracle), golden.json is the corrected, verb-derived expectation set;
// the differential test pins To/Value/Data/Gas so any later verb regression is
// caught byte-for-byte.
type goldenEntry struct {
	Verb  string `json:"verb"`
	Idx   uint64 `json:"idx"`
	To    string `json:"to"`    // "" = contract creation, else 0x-hex
	Value uint64 `json:"value"` // these vectors all fit uint64
	Data  string `json:"data"`  // 0x-hex
	Gas   uint64 `json:"gas"`
}

// goldenContractRegistry returns the fixed contract registry the golden
// vectors are pinned against. The addresses are deterministic test constants —
// the bootstrap phase computes the real CreateAddress values at runtime, but
// the golden test only needs a stable, known mapping so contract-calling verbs
// resolve a target.
func goldenContractRegistry() *ContractRegistry {
	reg := NewContractRegistry()
	reg.Set(ContractStorageSpam, common.HexToAddress("0x00000000000000000000000000000000c0117ac1"))
	reg.Set(ContractTestToken, common.HexToAddress("0x00000000000000000000000000000000c0117ac2"))
	reg.Set(ContractStorageRefund, common.HexToAddress("0x00000000000000000000000000000000c0117ac3"))
	reg.Set(ContractGasBurner, common.HexToAddress("0x00000000000000000000000000000000c0117ac4"))
	reg.Set(ContractErc20Bloater, common.HexToAddress("0x00000000000000000000000000000000c0117ac5"))
	return reg
}

// goldenBuildCtx is the fixed build context the golden vectors use: base
// address = 0, revision = 1, stride = 1<<40, salt base = 0, the well-known lab
// signer address, and the fixed contract registry.
func goldenBuildCtx() BuildCtx {
	return BuildCtx{
		ChainID:       big.NewInt(1337),
		SignerAddr:    common.HexToAddress("0x19E7E376E7C213B7E7e7e46cc70A5dD086DAff2A"),
		BaseAddress:   make([]byte, 20),
		Revision:      1,
		AddressStride: 1 << 40,
		SaltBase:      0,
		Contracts:     goldenContractRegistry(),
	}
}

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	s = trim0x(s)
	if s == "" {
		return []byte{}
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

func trim0x(s string) string {
	if len(s) >= 2 && s[:2] == "0x" {
		return s[2:]
	}
	return s
}

// goldenIdxs is the fixed set of tx indices each verb is sampled at.
var goldenIdxs = []uint64{0, 1, 2, 1000, 999999}

// regenerateGolden rebuilds golden.json from the current verb output. Invoked
// by `go test -update` after a deliberate verb change.
func regenerateGolden(t *testing.T) {
	t.Helper()
	ctx := goldenBuildCtx()
	names := make([]string, 0, len(Registry))
	for n := range Registry {
		names = append(names, n)
	}
	sort.Strings(names)

	entries := make([]goldenEntry, 0, len(names)*len(goldenIdxs))
	for _, name := range names {
		verb := Registry[name]
		for _, idx := range goldenIdxs {
			tx, err := verb.BuildTx(idx, ctx)
			if err != nil {
				t.Fatalf("regen: BuildTx(%s, %d): %v", name, idx, err)
			}
			to := ""
			if tx.To != nil {
				to = tx.To.Hex()
			}
			value := uint64(0)
			if tx.Value != nil {
				value = tx.Value.Uint64()
			}
			entries = append(entries, goldenEntry{
				Verb:  name,
				Idx:   idx,
				To:    to,
				Value: value,
				Data:  "0x" + hex.EncodeToString(tx.Data),
				Gas:   tx.Gas,
			})
		}
	}
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatalf("regen: marshal: %v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(filepath.Join("testdata", "golden.json"), out, 0o644); err != nil {
		t.Fatalf("regen: write golden.json: %v", err)
	}
	t.Logf("regenerated golden.json with %d entries", len(entries))
}

// TestVerbsGolden asserts byte-equality between every native Go verb and the
// golden vectors. A verb whose template diverges in To, Value, Data or Gas
// silently corrupts on-chain bloat state, so this test is the load-bearing
// regression net. Run with `-update` to regenerate the vectors.
func TestVerbsGolden(t *testing.T) {
	if *updateGolden {
		regenerateGolden(t)
		return
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatalf("read golden.json: %v", err)
	}
	var entries []goldenEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("unmarshal golden.json: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("golden.json contains no entries")
	}

	ctx := goldenBuildCtx()
	seen := map[string]bool{}

	for _, e := range entries {
		seen[e.Verb] = true
		t.Run(e.Verb+"/"+itoa(e.Idx), func(t *testing.T) {
			verb, ok := Lookup(e.Verb)
			if !ok {
				t.Fatalf("verb %q not in native Registry", e.Verb)
			}
			tx, err := verb.BuildTx(e.Idx, ctx)
			if err != nil {
				t.Fatalf("BuildTx(%d): %v", e.Idx, err)
			}

			// To: "" => nil pointer (contract creation).
			wantTo := mustDecodeHex(t, e.To)
			if len(wantTo) == 0 {
				if tx.To != nil {
					t.Fatalf("To: want nil (creation), got %s", tx.To.Hex())
				}
			} else {
				if tx.To == nil {
					t.Fatalf("To: want %s, got nil", e.To)
				}
				if got := tx.To.Bytes(); !bytesEqual(got, wantTo) {
					t.Fatalf("To: want %x, got %x", wantTo, got)
				}
			}

			// Value.
			wantValue := new(big.Int).SetUint64(e.Value)
			gotValue := tx.Value
			if gotValue == nil {
				gotValue = new(big.Int)
			}
			if gotValue.Cmp(wantValue) != 0 {
				t.Fatalf("Value: want %s, got %s", wantValue, gotValue)
			}

			// Data — the byte-equality assertion that matters most.
			wantData := mustDecodeHex(t, e.Data)
			if !bytesEqual(tx.Data, wantData) {
				t.Fatalf("Data mismatch:\n want %x\n  got %x", wantData, tx.Data)
			}

			// Gas.
			if tx.Gas != e.Gas {
				t.Fatalf("Gas: want %d, got %d", e.Gas, tx.Gas)
			}
		})
	}

	// Every entry's verb must be registered; conversely every native verb
	// should have golden coverage.
	for name := range Registry {
		if !seen[name] {
			t.Errorf("verb %q is registered but has no golden vectors", name)
		}
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
