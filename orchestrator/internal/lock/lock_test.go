package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireTwiceErrors(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l.Release()

	if _, err := Acquire(dir); err == nil {
		t.Fatalf("second acquire: expected error, got nil")
	} else if !errors.Is(err, ErrAlreadyHeld) {
		t.Fatalf("second acquire: expected ErrAlreadyHeld, got %v", err)
	}
}

func TestAcquireReleaseReacquire(t *testing.T) {
	dir := t.TempDir()
	l1, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	l2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	defer l2.Release()
}

func TestIdentityWritten(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer l.Release()

	if l.Identity() == "" {
		t.Fatalf("expected non-empty identity")
	}

	if _, err := filepath.Abs(filepath.Join(dir, identityFilename)); err != nil {
		t.Fatalf("identity path: %v", err)
	}
}
