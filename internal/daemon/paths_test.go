package daemon

import (
	"path/filepath"
	"strings"
	"testing"
)

// This file exercises daemonDir's fallback decision (dev_docs/review-backlog.md
// item 21) in isolation, as pure string/length logic: it never touches the
// real home directory or opens a socket.

// A short home directory keeps the normal, home-rooted path.
func TestDaemonDir_ShortHomeUsesPreferredPath(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "alice")
	got := daemonDir(home)
	want := filepath.Join(home, appDirName, daemonSubdir)
	if got != want {
		t.Fatalf("daemonDir(%q) = %q, want %q", home, got, want)
	}
	if !fitsSocketPath(got) {
		t.Fatalf("daemonDir(%q) = %q does not fit the socket path budget; the test fixture should be short", home, got)
	}
}

// A long home directory (the common Windows case: a long domain profile
// path) falls back to a short path under os.TempDir whose socket file still
// fits the budget.
func TestDaemonDir_LongHomeFallsBackUnderTempDir(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users",
		strings.Repeat("a-very-long-domain-profile-directory-name-", 4))
	preferred := filepath.Join(home, appDirName, daemonSubdir)
	if fitsSocketPath(preferred) {
		t.Fatalf("test fixture home %q is not actually long enough to trigger the fallback", home)
	}

	got := daemonDir(home)
	if !strings.HasPrefix(got, FallbackRoot()) {
		t.Fatalf("daemonDir(%q) = %q, want it under FallbackRoot() (%q)", home, got, FallbackRoot())
	}
	if !fitsSocketPath(got) {
		t.Fatalf("fallback daemonDir(%q) = %q still does not fit the socket path budget", home, got)
	}
}

// The fallback path is stable across repeated calls for the same home (so a
// client and the daemon, resolving it independently, always agree) and
// distinct for different homes (so two users sharing a machine's temp
// directory never collide on the same socket).
func TestDaemonDir_FallbackIsStableAndDistinctPerHome(t *testing.T) {
	longHomeA := filepath.Join(string(filepath.Separator), "Users",
		strings.Repeat("first-very-long-domain-profile-name-", 4))
	longHomeB := filepath.Join(string(filepath.Separator), "Users",
		strings.Repeat("second-very-long-domain-profile-name-", 4))

	firstA := daemonDir(longHomeA)
	secondA := daemonDir(longHomeA)
	if firstA != secondA {
		t.Fatalf("daemonDir(%q) is not stable across calls: %q vs %q", longHomeA, firstA, secondA)
	}

	b := daemonDir(longHomeB)
	if firstA == b {
		t.Fatalf("daemonDir returned the same fallback path for two different home directories: %q", firstA)
	}
}

// DefaultPaths itself always produces a Paths whose Socket fits the AF_UNIX
// budget, regardless of how long the real home directory on this machine
// happens to be.
func TestDefaultPaths_SocketAlwaysFitsBudget(t *testing.T) {
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	if len(paths.Socket) > maxSocketPathBytes {
		t.Fatalf("DefaultPaths().Socket = %q (%d bytes), want at most %d bytes", paths.Socket, len(paths.Socket), maxSocketPathBytes)
	}
}
