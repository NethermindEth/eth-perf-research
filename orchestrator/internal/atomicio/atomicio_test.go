package atomicio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NethermindEth/eth-perf-research/orchestrator/internal/atomicio"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	want := []byte("hello atomicio")

	if err := atomicio.WriteFile(path, want, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content mismatch: got %q, want %q", got, want)
	}
}

func TestOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	first := []byte("first version")
	second := []byte("second version — longer content to ensure truncation")

	if err := atomicio.WriteFile(path, first, 0o644); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	if err := atomicio.WriteFile(path, second, 0o644); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(second) {
		t.Fatalf("overwrite content mismatch: got %q, want %q", got, second)
	}
}
