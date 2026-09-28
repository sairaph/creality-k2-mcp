package mcpserver

import (
	"os"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/daemon/daemontest"
)

// TestMain points daemon.FallbackRoot at a short, package-owned temp
// directory for every test in this package, and fails the run if a
// creality_k2_mcp-* directory nonetheless appears in the real OS temp
// directory anyway (dev_docs/review-backlog.md item 43). This package's
// tests build Deps with fake CameraViewer/CameraRecorder implementations
// today, never a real internal/daemon/client.Client, but this guards
// against a future test wiring one up over a redirected, long HOME.
func TestMain(m *testing.M) {
	os.Exit(daemontest.Guard(m))
}
