// Package userhome is the one place the per-user home directory is resolved
// for everything that lives under it: the registry, settings and lock files
// (internal/domain), and the daemon's socket, log and recordings
// (internal/daemon). Funnelling them through Dir lets a test binary be
// stopped from ever touching the developer's real home.
package userhome

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
// real home: the binary's TestMain did not isolate the environment
// (RealHomeEnv unset), or something reset HOME/USERPROFILE back to the real
// home. The real home is computed fresh from the OS account database
// (RealHome), never from HOME/USERPROFILE and never trusted from the marker
// alone, so a stale or forged marker cannot let the real home through. The
// tests then fail loudly instead of writing into the owner's printer
// registry, settings and lock files.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if testing.Testing() {
		marker := os.Getenv(RealHomeEnv)
		if marker == "" {
			panic(fmt.Sprintf("userhome: a test resolved the per-user home directory (%s) but this test binary's TestMain did not isolate it; "+
				"add `func TestMain(m *testing.M) { os.Exit(testhome.Run(m)) }` (internal/userhome/testhome) to the package", home))
		}
		if samePath(home, marker) {
			panic(fmt.Sprintf("userhome: a test resolved the REAL home directory %s; point HOME and USERPROFILE at a temp directory (t.Setenv) before using per-user paths", home))
		}
		if real, err := RealHome(); err == nil && samePath(home, real) {
			panic(fmt.Sprintf("userhome: a test resolved the REAL home directory %s (the marker %s did not name it); point HOME and USERPROFILE at a temp directory (t.Setenv) before using per-user paths", home, RealHomeEnv))
		}
	}
	return home, nil
}

var (
	realHomeOnce sync.Once
	realHome     string
	realHomeErr  error
)

// RealHome returns the current account's home directory as the operating
// system records it (os/user), which HOME and USERPROFILE do not influence.
// The result is cached: the account's home does not change during a run.
func RealHome() (string, error) {
	realHomeOnce.Do(func() {
		u, err := user.Current()
		switch {
		case err != nil:
			realHomeErr = err
		case u.HomeDir == "":
			realHomeErr = fmt.Errorf("account %q has no home directory", u.Username)
		default:
			realHome = u.HomeDir
		}
	})
	return realHome, realHomeErr
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Shorten replaces every occurrence of the home directory in text with "~",
// where it starts a path (it is followed by a separator, whitespace or the end
// of the text), for places that show a path to a person. A text without the
// home directory, or a failure to resolve it, comes back unchanged.
func Shorten(text string) string {
	home, err := Dir()
	if err != nil || home == "" {
		return text
	}
	home = strings.TrimRight(home, `/\`)
	if home == "" {
		return text
	}
	index := func(from int) int {
		if runtime.GOOS != "windows" {
			return strings.Index(text[from:], home)
		}
		for k := from; k+len(home) <= len(text); k++ {
			if strings.EqualFold(text[k:k+len(home)], home) {
				return k - from
			}
		}
		return -1
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		j := index(i)
		if j < 0 {
			b.WriteString(text[i:])
			break
		}
		end := i + j + len(home)
		if end < len(text) && !strings.ContainsRune(`/\ `+"\t\n,;:)\"'", rune(text[end])) {
			b.WriteString(text[i:end])
			i = end
			continue
		}
		b.WriteString(text[i : i+j])
		b.WriteString("~")
		i = end
	}
	return b.String()
}
