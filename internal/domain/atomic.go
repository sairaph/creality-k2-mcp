package domain

import (
	"fmt"
	"os"
	"path/filepath"
)

// appDirName is the per-user application directory shared by the printer
// registry, settings and lock files.
const appDirName = ".creality_k2_mcp"

// baseDir resolves ~/.creality_k2_mcp. It does not create anything: a caller
// that only reads must not bring the directory into existence.
func baseDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, appDirName), nil
}

// WriteFileAtomic writes data to path by creating a temporary file in the
// same directory, syncing it, then atomically replacing whatever was at path.
// A reader never observes a partially written file, and a crash mid-write
// leaves the previous file (or none) rather than a truncated one. Exported
// so other packages with their own crash-safety needs (internal/daemon's
// recording sidecars, review backlog item 34) reuse this exact pattern,
// including replaceFile's Windows MoveFileEx handling, instead of
// duplicating it with a plain os.WriteFile that a crash mid-write could
// truncate or a concurrent reader could observe half-written.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("set permissions on temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := replaceFile(name, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}
