package target

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const validYAML = `
shares:
  accounts: 0.5
  storage: 0.3
  code: 0.2
total_bytes: 1000000
`

const invalidSharesYAML = `
shares:
  accounts: 0.9
  storage: 0.9
total_bytes: 500000
`

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tgt, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tgt.Shares["accounts"] != 0.5 {
		t.Errorf("accounts share: got %v want 0.5", tgt.Shares["accounts"])
	}
	if tgt.TotalBytes != 1000000 {
		t.Errorf("total_bytes: got %d want 1000000", tgt.TotalBytes)
	}
	if tgt.SHA256 == "" {
		t.Error("SHA256 must not be empty")
	}
}

func TestLoadInvalidShares(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(path, []byte(invalidSharesYAML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for shares not summing to 1.0, got nil")
	}
}

func TestWatcherFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var callCount atomic.Int32
	w, err := NewWatcher(ctx, path, 100*time.Millisecond, func(tgt *Target) {
		callCount.Add(1)
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	// Modify the file to trigger the watcher.
	updated := `
shares:
  accounts: 0.6
  storage: 0.2
  code: 0.2
total_bytes: 2000000
`
	// Small sleep to let watcher settle before writing.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		t.Fatalf("WriteFile update: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if callCount.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if callCount.Load() == 0 {
		t.Error("onChange callback was never called after file change")
	}
}

func TestWatcherContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	w, err := NewWatcher(ctx, path, 100*time.Millisecond, func(*Target) {})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	cancel()

	done := make(chan struct{})
	go func() {
		w.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Error("Watcher did not exit within 1s after context cancel")
	}
}
