package daemon

import (
	"fmt"
	"os"
)

// VerifyDir checks that dir - the daemon's own directory (paths.Dir, already
// created by os.MkdirAll(dir, 0o700) in Open, or resolved by DefaultPaths /
// PathsIn for a client that is about to dial into it) - is safe to trust
// before anything is placed inside it, or before a connection is made into
// it. dev_docs/review-backlog.md item 30: daemonDir's fallback path lives
// under os.TempDir, a directory shared with every other local account, so a
// directory that already exists there before this process ever created it
// must never be trusted blindly - it could be a symlink planted by another
// user to redirect the daemon's socket, pid file and log, or a directory
// that user already owns.
//
// This refuses anything that is not a plain directory (no symlink, no other
// file type) outright, then applies a platform-specific ownership/mode check
// (see verifyDirOwnerAndMode in socket_perms_unix.go and
// socket_perms_windows.go).
//
// Both Open (the daemon side, which creates the directory) and every dial in
// internal/daemon/client (the client side, which only ever reads it) call
// this before trusting dir, so the two sides apply exactly the same check.
func VerifyDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("daemon: stat %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("daemon: %s is a symlink, refusing to trust it", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("daemon: %s is not a directory", dir)
	}
	return verifyDirOwnerAndMode(dir, info)
}
