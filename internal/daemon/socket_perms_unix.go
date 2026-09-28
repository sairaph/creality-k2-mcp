//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// withRestrictedSocketUmask runs open (mcp-wizard's daemon/socket.Server.Open)
// under a restrictive umask, so the socket file it creates is not left
// world- or group-writable. net.Listen("unix", ...) applies the process
// umask like any other file creation; without this, a typical 022 umask
// leaves the socket at 0755, reachable by any other local account
// (dev_docs/review-backlog.md item 22, matching
// references/interactive-terminal-mcp/internal/ipc/socket_unix.go).
func withRestrictedSocketUmask(open func() error) error {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	return open()
}

// restrictSocketPermissions chmods the socket file to 0600 (owner read/write
// only) as a second, explicit guarantee on top of the umask above: this
// user's daemon socket carries watchdog arm/disarm and camera viewer
// control, and must not be reachable by any other account on a shared
// machine.
func restrictSocketPermissions(socketPath string) error {
	return os.Chmod(socketPath, 0o600)
}

// verifyDirOwnerAndMode backs VerifyDir (dirsecurity.go, item 30) on Unix:
// dir must be owned by the current user, and is chmod'd to 0700 if it was
// not already - but only when we own it. A directory owned by someone else
// is refused outright rather than fixed, since chmod on it would only
// succeed if we already had write access we should not have, and silently
// "fixing" another user's directory is never the right move; VerifyDir's
// caller (Open, or a client's dial) simply treats this as "the daemon
// directory cannot be trusted" the same way it would treat any other error
// here.
//
// Open must stay single-threaded across MkdirAll, this check and the
// umask-guarded socket.Open call that follows it (see Open's own comment):
// nothing else in this process may create or modify paths.Dir concurrently,
// or the ownership/mode this check just established could already be stale
// by the time the socket is created inside it.
func verifyDirOwnerAndMode(dir string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("daemon: cannot determine the owner of %s", dir)
	}
	uid := uint32(os.Getuid())
	if stat.Uid != uid {
		return fmt.Errorf("daemon: %s is owned by uid %d, not the current user (uid %d); refusing to trust a "+
			"directory owned by someone else", dir, stat.Uid, uid)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("daemon: chmod %s to 0700: %w", dir, err)
		}
	}
	return nil
}
