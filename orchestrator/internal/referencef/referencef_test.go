package referencef

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleYAML = `
verbs:
  transfer:
    accounts: 1200.5
    storage: 0.0
  deploy:
    accounts: 800.0
    storage: 4000.0
avg_tx_rlp:
  transfer: 110.0
  deploy: 320.0
`

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reference_f.yaml")
	if err := os.WriteFile(path, []byte(sampleYAML), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if rf.Verbs["transfer"]["accounts"] != 1200.5 {
		t.Errorf("transfer/accounts: got %v want 1200.5", rf.Verbs["transfer"]["accounts"])
	}
	if rf.Verbs["deploy"]["storage"] != 4000.0 {
		t.Errorf("deploy/storage: got %v want 4000.0", rf.Verbs["deploy"]["storage"])
	}
	if rf.AvgTxRLP["transfer"] != 110.0 {
		t.Errorf("avg_tx_rlp/transfer: got %v want 110.0", rf.AvgTxRLP["transfer"])
	}
	if rf.AvgTxRLP["deploy"] != 320.0 {
		t.Errorf("avg_tx_rlp/deploy: got %v want 320.0", rf.AvgTxRLP["deploy"])
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load("/nonexistent/path/ref.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

// TestDefaultReferenceFNoZeroRow guards the cold-start fix: every verb in the
// built-in default must be non-zero on at least one axis so no verb seeds an
// all-zero (zero-gradient) F-row. It also pins the design-v3 §B.1 eoatx values
// and the intentional negative storagerefundtx storage sign.
func TestDefaultReferenceFNoZeroRow(t *testing.T) {
	def := DefaultReferenceF()
	for verb, row := range def.Verbs {
		allZero := true
		for _, axis := range []string{"accounts", "storage", "code"} {
			if row[axis] != 0 {
				allZero = false
			}
		}
		if allZero {
			t.Errorf("verb %q seeds an all-zero F-row", verb)
		}
	}
	if got := def.Verbs["eoatx"]["accounts"]; got != 160 {
		t.Errorf("eoatx/accounts: got %v want 160", got)
	}
	if got := def.Verbs["storagerefundtx"]["storage"]; got != -180 {
		t.Errorf("storagerefundtx/storage: got %v want -180 (negative by design)", got)
	}
}

// TestWithDefaultsBackfillsMissingVerbs verifies a partial supplied file cannot
// reintroduce an all-zero row: a verb absent from the file is backfilled, while
// values explicitly present in the file win over the default.
func TestWithDefaultsBackfillsMissingVerbs(t *testing.T) {
	partial := &ReferenceF{
		Verbs: map[string]map[string]float64{
			"calltx": {"accounts": 99, "storage": 0, "code": 0},
		},
	}
	merged := WithDefaults(partial)
	// Supplied value wins.
	if got := merged.Verbs["calltx"]["accounts"]; got != 99 {
		t.Errorf("calltx/accounts: got %v want 99 (supplied value)", got)
	}
	// Verb absent from the partial file is backfilled from the default.
	if got := merged.Verbs["eoatx"]["accounts"]; got != 160 {
		t.Errorf("eoatx/accounts: got %v want 160 (backfilled default)", got)
	}
}
