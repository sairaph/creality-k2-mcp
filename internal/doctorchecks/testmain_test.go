package doctorchecks

import (
	"os"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/daemon/daemontest"
)

// TestMain points daemon.FallbackRoot at a short, package-owned temp
// directory for every test in this package, and fails the run if a
// creality_k2_mcp-* directory nonetheless appears in the real OS temp
// directory anyway (dev_docs/review-backlog.md item 43): DaemonCheck.Run
// (daemon.go) builds an internal/daemon/client.Client with plain client.New,
// which calls daemon.DefaultPaths, and setTestHome (registry_test.go)
// redirects HOME/USERPROFILE to a short os.MkdirTemp directory specifically
// to avoid the fallback - this is a backstop against a future test here
// reaching it some other way.
func TestMain(m *testing.M) {
	os.Exit(daemontest.Guard(m))
}
