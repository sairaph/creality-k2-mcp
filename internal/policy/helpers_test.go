package policy

import (
	"context"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// setTestHome redirects domain's per-user base directory (~/.creality_k2_mcp)
// into a fresh t.TempDir() for the duration of one test, so Execute's
// cross-process file lock (domain.LockPath) never touches the real user
// home directory during tests, matching how internal/domain's own lock
// tests isolate themselves (internal/domain/lock_test.go).
func setTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

// testPrinter builds a control-enabled domain.Printer identified by
// hostname, distinct per test so cross-process lock files never collide
// between unrelated tests even when they share a HOME. It also configures
// f's PrinterInfo to answer with the same hostname (setPrinterInfo,
// fakeprinter_test.go): since resolveExecuteIdentity (execute.go) now
// verifies every registry-backed printer's persisted hostname against a
// fresh printer/info read (review backlog item 24), every test that wants
// its printer treated as a normal, identity-verified one must give f a
// matching live hostname, not just a persisted one. A test that wants to
// exercise a mismatch or an unverified identity calls f.setPrinterInfo
// itself afterward to override this.
func testPrinter(f *fakePrinter, hostname string) domain.Printer {
	f.setPrinterInfo(hostname, nil)
	return domain.Printer{
		ID:            "test",
		Name:          "test",
		Host:          "127.0.0.1",
		MoonrakerPort: 7125,
		Hostname:      hostname,
		Enabled:       true,
		AllowControl:  true,
	}
}

func testSettings() domain.Settings {
	return domain.DefaultSettings()
}

// mustExecute is a small assertion helper for the happy path.
func mustExecute(t *testing.T, p *Policy, f *fakePrinter, printer domain.Printer, name ActionName, params Params, token string) Result {
	t.Helper()
	res, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), name, params, token)
	if err != nil {
		t.Fatalf("Execute(%s): unexpected error: %v", name, err)
	}
	return res
}
