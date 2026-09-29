// Package daemontest provides shared test-only helpers for keeping
// internal/daemon's short-path socket fallback (paths.go's daemonDir) away
// from the real OS temp directory during tests (dev_docs/review-backlog.md
// item 43).
//
// It imports "testing" and calls os.Exit, so it deliberately lives in its
// own package rather than inside internal/daemon itself, matching the
// standard library's own httptest pattern: only _test.go files should ever
// import it, never production code.
package daemontest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
)

// fallbackDirPrefix matches daemon.go's own "creality-k2-mcp-" literal
// (paths.go daemonDir).
const fallbackDirPrefix = "creality-k2-mcp-"

// Guard points daemon.FallbackRoot at a short, package-owned temp directory
// for the duration of m.Run(), so every test in the calling package that
// resolves daemon.DefaultPaths (directly, or indirectly through
// internal/daemon/client.New) with a long HOME/USERPROFILE - the common
// case, since testing.T.TempDir() embeds the full test name - lands inside
// that directory instead of silently creating a creality-k2-mcp-<hash>
// directory in the real user's real temp directory.
//
// As a backstop against some future test reaching the fallback a different
// way (or forgetting to use a short home at all), Guard also snapshots the
// real OS temp directory's own creality-k2-mcp-* entries before m.Run() and
// again after, and fails the whole test binary if any new one appeared
// there.
//
// It calls os.Exit itself, matching how (*testing.M).Run is normally used
// directly in TestMain, so a package's TestMain should be exactly:
//
//	func TestMain(m *testing.M) { os.Exit(daemontest.Guard(m)) }
func Guard(m *testing.M) int {
	root, err := os.MkdirTemp("", "k2fb")
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemontest: MkdirTemp: %v\n", err)
		return 1
	}
	defer os.RemoveAll(root)

	realTemp := os.TempDir()
	before := crealityDirs(realTemp)

	prev := daemon.FallbackRoot
	daemon.FallbackRoot = func() string { return root }
	defer func() { daemon.FallbackRoot = prev }()

	code := m.Run()

	after := crealityDirs(realTemp)
	if leaked := newEntries(before, after); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr,
			"daemontest: %d creality-k2-mcp-* directory(ies) leaked into the real OS temp dir %s during this test run (daemon.FallbackRoot was not honored): %v\n",
			len(leaked), realTemp, leaked)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// crealityDirs lists root's own creality-k2-mcp-* subdirectories. A missing
// or unreadable root yields an empty set rather than an error: TestMain has
// no *testing.T to report through, and a root that cannot be listed cannot
// have leaked anything visible either.
func crealityDirs(root string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), fallbackDirPrefix) {
			out[filepath.Join(root, e.Name())] = true
		}
	}
	return out
}

// newEntries returns the entries present in after but not before, sorted
// for a stable failure message.
func newEntries(before, after map[string]bool) []string {
	var leaked []string
	for k := range after {
		if !before[k] {
			leaked = append(leaked, k)
		}
	}
	sort.Strings(leaked)
	return leaked
}
