// Package atomicio provides an atomic file-write primitive.
package atomicio

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile atomically replaces the file at path with data.
//
// Sequence: create temp file in the same directory as path → write → fsync →
// close → rename → fsync parent directory.  Using the same directory as path
// guarantees the rename is on the same filesystem, so it is atomic.
//
// On any error before the rename the temp file is removed.  perm is the
// permission bits applied to the temp file (and therefore the final file).
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("atomicio: create temp: %w", err)
	}
	tmpName := tmp.Name()

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("atomicio: chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("atomicio: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("atomicio: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomicio: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomicio: rename to %s: %w", path, err)
	}

	df, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("atomicio: open dir for fsync: %w", err)
	}
	defer df.Close()
	if err := df.Sync(); err != nil {
		return fmt.Errorf("atomicio: fsync dir: %w", err)
	}
	return nil
}
