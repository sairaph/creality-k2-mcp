//go:build !windows

package clicmd

import "os"

// snapshotReplaceFile atomically moves source over destination; plain
// os.Rename already overwrites an existing destination on POSIX systems.
func snapshotReplaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
