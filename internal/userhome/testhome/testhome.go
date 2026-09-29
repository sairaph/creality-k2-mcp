// Package testhome isolates a test binary from the developer's real home
// directory. Like internal/daemon/daemontest it imports "testing" and is only
// ever imported from _test.go files.
package testhome

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/userhome"
)

// Run is what every test package's TestMain calls:
//
//	func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }
//
// It points HOME, USERPROFILE and the other per-user environment variables
// (APPDATA, LOCALAPPDATA, XDG_*) at a fresh temp directory for the whole
// package run and removes it afterwards, and records the real home in
// userhome.RealHomeEnv so userhome.Dir can refuse to resolve it. Tests that
// need their own home keep using t.Setenv.
func Run(m *testing.M) int {
	return RunFunc(m.Run)
}

// RunFunc is Run for a TestMain that wraps m.Run in more setup (daemontest.Guard).
func RunFunc(run func() int) int {
	if os.Getenv(userhome.RealHomeEnv) != "" {
		// Already isolated by an enclosing TestMain in this process tree.
		return run()
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testhome: resolve real home: %v\n", err)
		return 1
	}
	// A short name: some tests derive Unix socket paths from HOME, which
	// have a tight length budget.
	root, err := os.MkdirTemp("", "k2h")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testhome: MkdirTemp: %v\n", err)
		return 1
	}
	defer os.RemoveAll(root)

	env := map[string]string{
		userhome.RealHomeEnv: realHome,
		"HOME":               root,
		"USERPROFILE":        root,
		"APPDATA":            filepath.Join(root, "AppData", "Roaming"),
		"LOCALAPPDATA":       filepath.Join(root, "AppData", "Local"),
		"XDG_CONFIG_HOME":    filepath.Join(root, ".config"),
		"XDG_DATA_HOME":      filepath.Join(root, ".local", "share"),
		"XDG_CACHE_HOME":     filepath.Join(root, ".cache"),
		"XDG_STATE_HOME":     filepath.Join(root, ".local", "state"),
	}
	saved := map[string]*string{}
	for k, v := range env {
		if old, ok := os.LookupEnv(k); ok {
			o := old
			saved[k] = &o
		} else {
			saved[k] = nil
		}
		os.Setenv(k, v)
	}
	defer func() {
		for k, old := range saved {
			if old == nil {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, *old)
			}
		}
	}()
	return run()
}
