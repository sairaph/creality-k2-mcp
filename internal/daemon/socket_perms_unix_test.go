//go:build !windows

package daemon

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

// setTestUmask sets the process umask to mask and returns the previous
// value, restored by restoreTestUmask. The umask is process-wide, so tests
// using it must not run in parallel with each other.
func setTestUmask(t *testing.T, mask int) int {
	t.Helper()
	return syscall.Umask(mask)
}

func restoreTestUmask(previous int) {
	syscall.Umask(previous)
}

// This file exercises the socket permission guard (dev_docs/review-backlog.md
// item 22) on platforms where it actually does something: chmod and file
// permission bits have no meaning on Windows, which documents ACL
// inheritance from the parent directory instead (socket_perms_windows.go).

// Opening the daemon leaves the socket file at exactly 0600 (owner
// read/write only), regardless of the ambient umask, so it is not reachable
// by any other local account on a shared machine.
func TestOpen_SocketFileIsOwnerOnly(t *testing.T) {
	previous := setTestUmask(t, 0o022) // a typical, permissive default
	defer restoreTestUmask(previous)

	opts := testOptions(t)
	s := openTestServer(t, opts)

	info, err := os.Stat(opts.Paths.Socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket permissions = %o, want 0600", perm)
	}
	s.Close()
}

// The daemon directory itself (holding the socket, lock, pid file and log)
// is created 0700, matching the socket's own restriction.
func TestOpen_DaemonDirIsOwnerOnly(t *testing.T) {
	opts := testOptions(t)
	openTestServer(t, opts)

	info, err := os.Stat(opts.Paths.Dir)
	if err != nil {
		t.Fatalf("stat daemon dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("daemon dir permissions = %o, want 0700", perm)
	}
}

// This section exercises VerifyDir's Unix-only ownership/mode check
// (dirsecurity.go, socket_perms_unix.go's own verifyDirOwnerAndMode,
// dev_docs/review-backlog.md item 30). The cross-platform symlink and
// non-directory refusals live in dirsecurity_test.go.

// VerifyDir fixes a directory this user owns but that ended up with a wider
// mode than 0700 - for example one MkdirAll created under a permissive
// umask before this check existed - rather than refusing it outright.
func TestVerifyDir_ChmodsOwnDirectoryToExpectedMode(t *testing.T) {
	dir := t.TempDir() // owned by us, created 0700 by testing.T.TempDir
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := VerifyDir(dir); err != nil {
		t.Fatalf("VerifyDir(own directory at 0755) = %v, want it to fix the mode instead of refusing", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("directory permissions after VerifyDir = %o, want 0700", perm)
	}
}

// VerifyDir refuses a directory owned by another local account outright,
// never attempting to chmod it - exactly the "pre-created dir in shared
// temp must not be trusted" case item 30 calls out. Chowning a directory to
// another uid requires privilege this test suite does not run with, so this
// only exercises the refusal when already running as root (rare, but the
// only way to set up the fixture); everywhere else it is skipped rather
// than faked, since a faked owner mismatch would not prove anything about
// the real syscall.Stat_t path.
func TestVerifyDir_RefusesDirectoryOwnedBySomeoneElse(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown a directory to another uid")
	}
	dir := t.TempDir()
	const otherUID = 1 // any uid that is not root's own 0
	if err := os.Chown(dir, otherUID, os.Getgid()); err != nil {
		t.Fatalf("chown: %v", err)
	}

	err := VerifyDir(dir)
	if err == nil {
		t.Fatal("VerifyDir(directory owned by someone else) = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "owned by") {
		t.Errorf("VerifyDir error = %v, want it to explain the ownership mismatch", err)
	}
}
