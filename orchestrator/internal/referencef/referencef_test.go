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
	if err := os.WriteFile(path, []byte(sampleYAML), 0644); err != nil {
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
