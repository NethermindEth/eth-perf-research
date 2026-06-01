// Package lock provides a POSIX flock(2) exclusive file lock for a state
// directory. The lock is paired with a UUID identity written to a sidecar
// file so callers can detect NFS/loopback races where two processes both
// believe they own the lock.
package lock

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	lockFilename     = ".lock"
	identityFilename = ".lock-identity"
)

// ErrAlreadyHeld is returned when the lock file is already locked by another
// process (or another goroutine using a different fd).
var ErrAlreadyHeld = errors.New("lock: already held")

// Lock represents an acquired exclusive lock on a state directory.
type Lock struct {
	fd       int
	identity string
}

// Acquire takes an exclusive flock on stateDir/.lock. It writes a fresh UUID
// into stateDir/.lock-identity and re-reads it after the flock to confirm
// no other process clobbered the identity (NFS-safe check).
func Acquire(stateDir string) (*Lock, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("lock: ensure state dir: %w", err)
	}
	lockPath := filepath.Join(stateDir, lockFilename)
	idPath := filepath.Join(stateDir, identityFilename)

	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %q: %w", lockPath, err)
	}

	// Non-blocking exclusive lock — fail fast if held elsewhere.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("lock: %s: %w", lockPath, ErrAlreadyHeld)
		}
		return nil, fmt.Errorf("lock: flock %q: %w", lockPath, err)
	}

	identity, err := newIdentity()
	if err != nil {
		syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
		return nil, err
	}

	if err := os.WriteFile(idPath, []byte(identity), 0o644); err != nil {
		syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
		return nil, fmt.Errorf("lock: write identity: %w", err)
	}

	// Re-read to confirm ownership (NFS-safe).
	got, err := os.ReadFile(idPath)
	if err != nil {
		syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
		return nil, fmt.Errorf("lock: read identity: %w", err)
	}
	if string(got) != identity {
		syscall.Flock(fd, syscall.LOCK_UN)
		syscall.Close(fd)
		return nil, fmt.Errorf("lock: identity mismatch after write (got %q, want %q): %w", string(got), identity, ErrAlreadyHeld)
	}

	return &Lock{
		fd:       fd,
		identity: identity,
	}, nil
}

// Release unlocks and closes the underlying file. It is safe to call more than
// once; subsequent calls return nil.
func (l *Lock) Release() error {
	if l == nil || l.fd < 0 {
		return nil
	}
	unlockErr := syscall.Flock(l.fd, syscall.LOCK_UN)
	closeErr := syscall.Close(l.fd)
	l.fd = -1
	if unlockErr != nil {
		return fmt.Errorf("lock: unlock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("lock: close: %w", closeErr)
	}
	return nil
}

func (l *Lock) Identity() string {
	if l == nil {
		return ""
	}
	return l.identity
}

func newIdentity() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("lock: rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
