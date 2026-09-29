// Package userhome is the one place the per-user home directory is resolved
// for everything that lives under it: the registry, settings and lock files
// (internal/domain), and the daemon's socket, log and recordings
// (internal/daemon). Funnelling them through Dir lets a test binary be
// stopped from ever touching the developer's real home.
package userhome

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// RealHomeEnv names the marker a test binary's TestMain sets (through
// internal/userhome/testhome) to the developer's real home directory before
// it redirects HOME and USERPROFILE to a temp directory. It is how Dir tells
// an isolated test run from one that forgot to isolate.
const RealHomeEnv = "CREALITY_K2_MCP_TEST_REAL_HOME"

// Dir returns the current user's home directory (os.UserHomeDir: HOME on
// Unix, USERPROFILE on Windows).
//
// Under `go test` (testing.Testing, the same kind of test-binary guard
// internal/daemon/client uses for autostart) it panics rather than return the
// real home: either the binary's TestMain did not isolate the environment
// (RealHomeEnv unset), or something reset HOME/USERPROFILE back to the real
// home. The tests then fail loudly instead of writing into the owner's
// printer registry, settings and lock files.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if testing.Testing() {
		real := os.Getenv(RealHomeEnv)
		if real == "" {
			panic(fmt.Sprintf("userhome: a test resolved the per-user home directory (%s) but this test binary's TestMain did not isolate it; "+
				"add `func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }` (internal/userhome/testhome) to the package", home))
		}
		if samePath(home, real) {
			panic(fmt.Sprintf("userhome: a test resolved the REAL home directory %s; point HOME and USERPROFILE at a temp directory (t.Setenv) before using per-user paths", home))
		}
	}
	return home, nil
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
