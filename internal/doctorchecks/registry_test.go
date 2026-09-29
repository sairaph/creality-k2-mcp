package doctorchecks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// setTestHome redirects domain's per-user base directory (~/.creality-k2-mcp)
// into a fresh, short-named temp directory, so no test here ever reads or
// writes the real user's registry or settings. This deliberately does not
// use t.TempDir() directly: that nests the directory under this package's
// (often long) test function name, and DaemonCheck's own tests build a real
// AF_UNIX socket several path segments further down
// (<home>/.creality-k2-mcp/daemon/daemon.sock) - long enough, combined with
// a descriptive test name, to exceed Windows's sockaddr_un path limit and
// fail with "bind: invalid argument". os.MkdirTemp with a short prefix
// keeps every path this package's tests build well under that limit.
func setTestHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "k2doc")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows reads USERPROFILE
	t.Setenv(domain.EnvHost, "")  // never let a real K2_MCP_HOST leak into these tests
	return home
}

func TestRegistryCheckNoPrintersWarns(t *testing.T) {
	setTestHome(t)

	res := RegistryCheck{}.Run(context.Background())
	if res.Status != doctor.Warn {
		t.Fatalf("Status = %v, want Warn", res.Status)
	}
	if !strings.Contains(res.Detail, "no printers are enabled") {
		t.Errorf("Detail = %q, want a hint about no enabled printers", res.Detail)
	}
	if !strings.Contains(res.Detail, "0 printer(s), 0 enabled") {
		t.Errorf("Detail = %q, want the printer counts", res.Detail)
	}
}

func TestRegistryCheckEnvOverride(t *testing.T) {
	setTestHome(t)
	t.Setenv(domain.EnvHost, "192.168.1.50")

	res := RegistryCheck{}.Run(context.Background())
	if res.Status != doctor.OK {
		t.Fatalf("Status = %v, want OK", res.Status)
	}
	if !strings.Contains(res.Detail, domain.EnvHost+" environment variable") {
		t.Errorf("Detail = %q, want it to name the env override as the source", res.Detail)
	}
	if !strings.Contains(res.Detail, "1 printer(s), 1 enabled") {
		t.Errorf("Detail = %q, want one enabled printer", res.Detail)
	}
}

func TestRegistryCheckEnabledPrinterIsOK(t *testing.T) {
	setTestHome(t)

	reg := domain.Registry{Version: 1, Printers: []domain.Printer{
		{ID: "k2-1", Name: "k2-1", Host: "192.168.1.10", MoonrakerPort: 7125, Hostname: "k2-1.local", Enabled: true},
	}}
	path, err := domain.GlobalRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.SaveRegistry(path, reg); err != nil {
		t.Fatal(err)
	}

	res := RegistryCheck{}.Run(context.Background())
	if res.Status != doctor.OK {
		t.Fatalf("Status = %v, Detail = %q, want OK", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "(global)") {
		t.Errorf("Detail = %q, want it to name the global registry", res.Detail)
	}
	if !strings.Contains(res.Detail, "1 printer(s), 1 enabled") {
		t.Errorf("Detail = %q, want one enabled printer", res.Detail)
	}
}

func TestRegistryCheckProjectWins(t *testing.T) {
	setTestHome(t)
	dir := t.TempDir()
	t.Chdir(dir)

	projectPath := domain.ProjectRegistryPath(dir)
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reg := domain.Registry{Version: 1, Printers: []domain.Printer{
		{ID: "k2-1", Name: "k2-1", Host: "192.168.1.10", MoonrakerPort: 7125, Hostname: "k2-1.local", Enabled: true},
	}}
	if err := domain.SaveRegistry(projectPath, reg); err != nil {
		t.Fatal(err)
	}

	res := RegistryCheck{}.Run(context.Background())
	if !strings.Contains(res.Detail, "(project)") {
		t.Errorf("Detail = %q, want it to name the project registry", res.Detail)
	}
}

// A duplicate hostname is dropped by LoadRegistryFile (dedupeByHostname)
// rather than failing the whole load; RegistryCheck must surface that as a
// warning, not silently hide it. The file is written directly (not through
// SaveRegistry, which validates and would reject this) to match a
// hand-edited registry file.
func TestRegistryCheckReportsDroppedEntries(t *testing.T) {
	setTestHome(t)
	path, err := domain.GlobalRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := `{"version":1,"printers":[
		{"id":"k2-1","name":"k2-1","host":"192.168.1.10","moonraker_port":7125,"hostname":"k2-1.local","enabled":true},
		{"id":"k2-2","name":"k2-2","host":"192.168.1.11","moonraker_port":7125,"hostname":"k2-1.local","enabled":true}
	]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	res := RegistryCheck{}.Run(context.Background())
	if res.Status != doctor.Warn {
		t.Fatalf("Status = %v, Detail = %q, want Warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "dropped at load") {
		t.Errorf("Detail = %q, want it to report the dropped duplicate", res.Detail)
	}
}
