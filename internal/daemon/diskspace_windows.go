//go:build windows

package daemon

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = modkernel32.NewProc("GetDiskFreeSpaceExW")
)

// freeDiskBytes reports the number of bytes free to the current user on the
// volume containing dir (T11c's disk-safety checks, Recorder.freeBytes):
// GetDiskFreeSpaceExW's lpFreeBytesAvailable, the caller's quota-aware
// figure, not the raw volume total. dir must already exist.
func freeDiskBytes(dir string) (uint64, error) {
	ptr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("daemon: free disk space: %w", err)
	}
	var freeBytesAvailable uint64
	r, _, callErr := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		0,
		0,
	)
	if r == 0 {
		return 0, fmt.Errorf("daemon: GetDiskFreeSpaceExW(%s): %w", dir, callErr)
	}
	return freeBytesAvailable, nil
}
