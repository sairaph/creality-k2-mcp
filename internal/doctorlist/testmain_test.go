package doctorlist

import (
	"os"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/daemon/daemontest"
)

// TestMain isolates HOME for the whole package (Checks reads the registry to
// learn which printers are enabled) and guards the OS temp directory the same
// way every other package that touches the daemon packages does.
func TestMain(m *testing.M) {
	os.Exit(daemontest.Guard(m))
}
