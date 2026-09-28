package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file exercises VerifyDir (dirsecurity.go, dev_docs/review-backlog.md
// item 30) on every platform: the symlink and non-directory refusals apply
// equally on Unix and Windows. The Unix-only ownership/mode checks live in
// socket_perms_unix_test.go, where they can use syscall.Stat_t directly.

// VerifyDir refuses a symlink outright, even when it points at a directory
// this same user owns: a pre-created symlink in the shared os.TempDir
// fallback path (daemonDir) must never be trusted, since it could redirect
// the daemon's socket, pid file and log anywhere.
func TestVerifyDir_RejectsSymlink(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("MkdirAll real: %v", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	err := VerifyDir(link)
	if err == nil {
		t.Fatal("VerifyDir(symlink) = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("VerifyDir(symlink) error = %v, want it to mention the symlink refusal", err)
	}
}

// VerifyDir refuses a path that exists but is not a directory at all (for
// example a stale regular file left where the daemon directory should be).
func TestVerifyDir_RejectsRegularFile(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := VerifyDir(file)
	if err == nil {
		t.Fatal("VerifyDir(regular file) = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("VerifyDir(regular file) error = %v, want it to say it is not a directory", err)
	}
}

// VerifyDir refuses a path that does not exist at all, the same as any
// other stat failure: Open always calls it right after MkdirAll, so this
// only matters for a caller (a client dial) that races a daemon that has
// not created its directory yet, and it must fail closed, not panic.
func TestVerifyDir_RejectsMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := VerifyDir(missing); err == nil {
		t.Fatal("VerifyDir(missing path) = nil, want an error")
	}
}

// Open refuses to start when paths.Dir is a symlink, even though
// os.MkdirAll on it is a silent no-op (it follows the symlink to an
// already-existing directory), demonstrating VerifyDir catches exactly the
// case MkdirAll alone would miss.
func TestOpen_RefusesSymlinkDaemonDir(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("MkdirAll real: %v", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	opts := testOptions(t)
	opts.Paths = PathsIn(link)

	_, err := Open(opts)
	if err == nil {
		t.Fatal("Open with a symlinked daemon dir = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Open error = %v, want it to mention the symlink refusal", err)
	}
}
