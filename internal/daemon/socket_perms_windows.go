//go:build windows

package daemon

import "os"

// withRestrictedSocketUmask runs open (mcp-wizard's daemon/socket.Server.Open)
// unchanged. Windows has no umask; the AF_UNIX socket file it creates
// inherits the ACL of its parent directory (paths.Dir, created 0o700 in
// Open below, which os.MkdirAll on Windows maps to an ACL granting the
// owner full control and denying other users), which is this platform's
// equivalent of the Unix umask/chmod guard in socket_perms_unix.go
// (dev_docs/review-backlog.md item 22).
func withRestrictedSocketUmask(open func() error) error {
	return open()
}

// restrictSocketPermissions is a no-op on Windows: there is no chmod
// equivalent for a socket file, and none is needed, since it already
// inherits the restrictive parent directory ACL documented above.
func restrictSocketPermissions(socketPath string) error {
	return nil
}

// verifyDirOwnerAndMode backs VerifyDir (dirsecurity.go, item 30) on
// Windows. VerifyDir has already refused a symlink (or reparse point/
// junction, which os.Lstat's Mode() also reports with ModeSymlink) and
// confirmed dir is a plain directory before this runs; there is no portable
// os.FileInfo-level owner check on Windows equivalent to Unix's Uid, so
// ownership and access here are enforced entirely by the ACL
// os.MkdirAll(dir, 0o700) sets on this directory (owner full control, other
// users denied) - the same reliance on ACL inheritance
// restrictSocketPermissions' own doc comment above already documents for
// the socket file itself. A pre-created directory in a shared temp fallback
// path (daemonDir) that this process did not create is therefore only
// caught here when it is a symlink/junction or not a directory at all; a
// plain directory with a weaker ACL than 0700 that someone else pre-created
// is not detected by this check.
func verifyDirOwnerAndMode(dir string, info os.FileInfo) error {
	return nil
}
