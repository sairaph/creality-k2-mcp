//go:build !windows

package daemon

import (
	"fmt"
	"syscall"
)

// freeDiskBytes reports the number of bytes free to an unprivileged caller
// on the volume containing dir (T11c's disk-safety checks,
// Recorder.freeBytes), via statfs's Bavail (available to non-root, unlike
// Bfree). dir must already exist.
func freeDiskBytes(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("daemon: statfs(%s): %w", dir, err)
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
