package domain

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLockPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path, err := LockPath("K2-5885")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, appDirName, "locks", "K2-5885.lock")
	if path != want {
		t.Errorf("LockPath(%q) = %q, want %q", "K2-5885", path, want)
	}
}

func TestLockPathRejectsEmpty(t *testing.T) {
	if _, err := LockPath(""); err == nil {
		t.Error("LockPath(\"\") = nil error, want error")
	}
	if _, err := LockPath("   "); err == nil {
		t.Error("LockPath(\"   \") = nil error, want error")
	}
}

func TestLockPathRejectsPathSeparators(t *testing.T) {
	if _, err := LockPath("../escape"); err == nil {
		t.Error("LockPath with a path separator = nil error, want error")
	}
	if _, err := LockPath(`some\path`); err == nil {
		t.Error("LockPath with a backslash = nil error, want error")
	}
}

func TestLockPathDiffersByHostname(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	a, err := LockPath("K2-5885")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LockPath("K2-9999")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("LockPath for two different hostnames produced the same path")
	}
	if !strings.HasSuffix(a, ".lock") {
		t.Errorf("LockPath(%q) = %q, want a .lock suffix", "K2-5885", a)
	}
}
