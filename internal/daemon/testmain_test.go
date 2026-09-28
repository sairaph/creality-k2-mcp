package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMain points FallbackRoot at a short, package-owned temp directory for
// every test in this package (rather than the real os.TempDir()), and fails
// the run if a creality_k2_mcp-* directory nonetheless appears in the real
// OS temp directory anyway (dev_docs/review-backlog.md item 43): this
// package defines FallbackRoot and daemonDir itself, so its own tests
// (paths_test.go's fallback tests, daemon_test.go's
// TestOpenClose_LongHomeNeverTouchesRealTempDir) are exactly what this
// guards. internal/daemon/daemontest.Guard does the same thing for every
// other package that can reach this package's fallback; it is not used
// here to avoid this package's own test binary importing a package that
// imports it back.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "k2fb")
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: TestMain: MkdirTemp: %v\n", err)
		os.Exit(1)
	}

	realTemp := os.TempDir()
	before := crealityDirsForTest(realTemp)

	prev := FallbackRoot
	FallbackRoot = func() string { return root }

	// os.Exit below does not run deferred functions, so root is removed and
	// FallbackRoot restored explicitly here rather than via defer - a defer
	// here would silently never run, which is exactly the kind of leftover
	// temp directory this TestMain exists to prevent.
	code := m.Run()
	FallbackRoot = prev
	os.RemoveAll(root)

	after := crealityDirsForTest(realTemp)
	if leaked := newEntriesForTest(before, after); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr,
			"daemon: %d creality_k2_mcp-* directory(ies) leaked into the real OS temp dir %s during this test run: %v\n",
			len(leaked), realTemp, leaked)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func crealityDirsForTest(root string) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "creality_k2_mcp-") {
			out[filepath.Join(root, e.Name())] = true
		}
	}
	return out
}

func newEntriesForTest(before, after map[string]bool) []string {
	var leaked []string
	for k := range after {
		if !before[k] {
			leaked = append(leaked, k)
		}
	}
	sort.Strings(leaked)
	return leaked
}
