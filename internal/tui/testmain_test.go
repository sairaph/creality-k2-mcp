package tui

import (
	"os"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/daemon/daemontest"
)

// TestMain points daemon.FallbackRoot at a short, package-owned temp
// directory for every test in this package, and fails the run if a
// creality-k2-mcp-* directory nonetheless appears in the real OS temp
// directory anyway (dev_docs/review-backlog.md item 43). isolateHome
// (testhelpers_test.go) redirects HOME/USERPROFILE to a t.TempDir(), which
// is long enough on its own to push the preferred daemon socket path past
// its length budget; this package's tests only ever use fakeDaemonClient
// and fake doctor checks, never a real internal/daemon/client.Client, but this
// guards against a future test wiring one up over that redirected, long HOME.
// The spinner ticks every millisecond here so commands that wait on it settle
// at once.
func TestMain(m *testing.M) {
	spinInterval = time.Millisecond
	// Force colour codes so every width calculation is exercised on styled text
	// and the colour tests have something to compare; tests that read text strip
	// the codes.
	os.Setenv("CLICOLOR_FORCE", "1")
	os.Exit(daemontest.Guard(m))
}
