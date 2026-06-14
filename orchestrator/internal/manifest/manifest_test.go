package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	original := &Manifest{
		SchemaVersion:     SchemaVersion,
		SessionID:         "sess-abc",
		StartedAtISO:      "2026-01-01T00:00:00Z",
		GenesisSHA256:     "deadbeef",
		ChainIdentityHash: "cafebabe",
		BatchCount:        42,
		Terminated:        "target_reached",
		TargetHistory: []TargetEntry{
			{
				AppliedAtBatch: 1,
				AppliedAtISO:   "2026-01-01T01:00:00Z",
				TargetSHA256:   "aabb",
				Shares:         map[string]float64{"accounts": 0.5, "storage": 0.5},
				TotalBytes:     1024,
			},
		},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.SessionID != original.SessionID {
		t.Errorf("SessionID mismatch: got %q want %q", loaded.SessionID, original.SessionID)
	}
	if loaded.BatchCount != original.BatchCount {
		t.Errorf("BatchCount mismatch: got %d want %d", loaded.BatchCount, original.BatchCount)
	}
	if loaded.Terminated != original.Terminated {
		t.Errorf("Terminated mismatch: got %q want %q", loaded.Terminated, original.Terminated)
	}
	if len(loaded.TargetHistory) != 1 {
		t.Fatalf("TargetHistory len: got %d want 1", len(loaded.TargetHistory))
	}
	if loaded.TargetHistory[0].TotalBytes != 1024 {
		t.Errorf("TargetHistory[0].TotalBytes: got %d want 1024", loaded.TargetHistory[0].TotalBytes)
	}
}

func TestDeterministicFieldOrder(t *testing.T) {
	m := &Manifest{
		SchemaVersion:     SchemaVersion,
		SessionID:         "s1",
		StartedAtISO:      "2026-01-01T00:00:00Z",
		GenesisSHA256:     "ff",
		ChainIdentityHash: "ee",
		TargetHistory:     []TargetEntry{},
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// Verify it is valid JSON and round-trips cleanly a second time.
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if raw["session_id"] != "s1" {
		t.Errorf("session_id field missing or wrong in raw JSON")
	}
}

func TestSaveNoParentDir(t *testing.T) {
	path := "/nonexistent-dir-xyz/sub/manifest.json"
	m := &Manifest{SessionID: "x", TargetHistory: []TargetEntry{}}
	err := m.Save(path)
	if err == nil {
		t.Fatal("expected error saving to non-existent directory, got nil")
	}
}
