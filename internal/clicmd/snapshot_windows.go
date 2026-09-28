//go:build windows

package clicmd

import "golang.org/x/sys/windows"

// snapshotReplaceFile atomically moves source over destination. Plain
// os.Rename on Windows goes through MoveFile, which refuses to overwrite an
// existing file (the same problem internal/domain/atomic_windows.go works
// around for the settings and registry files), so MoveFileEx with
// MOVEFILE_REPLACE_EXISTING is used instead, and MOVEFILE_WRITE_THROUGH
// flushes to disk before returning.
func snapshotReplaceFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
