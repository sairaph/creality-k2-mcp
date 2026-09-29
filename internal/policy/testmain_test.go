package policy

import (
	"os"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/daemon/daemontest"
)

// TestMain points daemon.FallbackRoot at a short, package-owned temp
// directory for every test in this package, and fails the run if a
// creality-k2-mcp-* directory nonetheless appears in the real OS temp
// directory anyway (dev_docs/review-backlog.md item 43). setTestHome
// (helpers_test.go) redirects HOME/USERPROFILE to a t.TempDir(), which is
// long enough on its own to push "<home>/.creality-k2-mcp/daemon/daemon.sock"
// past the socket length budget; identity_regression_test.go's
// startRealTestDaemon deliberately uses a short, hand-rolled dir with
// daemon.PathsIn to avoid daemon.DefaultPaths' fallback entirely, but this
// guards every test in this package against reaching it some other way.
func TestMain(m *testing.M) {
	os.Exit(daemontest.Guard(m))
}
