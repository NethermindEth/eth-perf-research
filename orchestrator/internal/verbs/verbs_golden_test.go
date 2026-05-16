package verbs

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// goldenEntry is one captured Python-oracle tx template. The golden vectors in
// testdata/golden.json are produced by capture_golden.py, which invokes the
// real EELS spamoor builders + the orchestrator-py facade wrappers.
type goldenEntry struct {
	Verb  string `json:"verb"`
	Idx   uint64 `json:"idx"`
	To    string `json:"to"`    // "" = contract creation, else 0x-hex
	Value uint64 `json:"value"` // these vectors all fit uint64
	Data  string `json:"data"`  // 0x-hex
	Gas   uint64 `json:"gas"`
}

// goldenBuildCtx mirrors the fixed build params capture_golden.py used:
// base address = 0, revision = 1, stride = 1<<40, salt base = 0, and the
// well-known lab signer address.
func goldenBuildCtx() BuildCtx {
	return BuildCtx{
		ChainID:       big.NewInt(1337),
		SignerAddr:    common.HexToAddress("0x19E7E376E7C213B7E7e7e46cc70A5dD086DAff2A"),
		BaseAddress:   make([]byte, 20),
		Revision:      1,
		AddressStride: 1 << 40,
		SaltBase:      0,
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

// TestVerbsGolden asserts byte-equality between every native Go verb and the
// captured Python oracle. A verb whose template diverges in To, Value, Data or
// Gas silently corrupts on-chain bloat state, so this test is the migration's
// load-bearing safety net.
func TestVerbsGolden(t *testing.T) {
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
