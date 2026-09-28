//go:build windows

package domain

import "golang.org/x/sys/windows"

// replaceFile atomically moves source over destination. Plain rename on
// Windows goes through MoveFile, which refuses to overwrite an existing
// file, so MoveFileEx with MOVEFILE_REPLACE_EXISTING is used instead, and
// MOVEFILE_WRITE_THROUGH flushes to disk before returning.
func replaceFile(source, destination string) error {
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
