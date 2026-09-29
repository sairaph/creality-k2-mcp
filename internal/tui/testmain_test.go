package tui

import (
	"os"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/daemon/daemontest"
)

// TestMain points daemon.FallbackRoot at a short, package-owned temp
// directory for every test in this package, and fails the run if a
// creality-k2-mcp-* directory nonetheless appears in the real OS temp
// directory anyway (dev_docs/review-backlog.md item 43). isolateHome
// (testhelpers_test.go) redirects HOME/USERPROFILE to a t.TempDir(), which
// is long enough on its own to push the preferred daemon socket path past
// its length budget; this package's tests only ever use fakeDaemonClient
// and a no-op RunDoctor default today, never a real
// internal/daemon/client.Client, but this guards against a future test
// wiring one up over that redirected, long HOME.
func TestMain(m *testing.M) {
	os.Exit(daemontest.Guard(m))
}
