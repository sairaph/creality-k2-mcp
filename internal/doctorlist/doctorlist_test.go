package doctorlist

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sairaph/mcp-wizard/doctor"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/update"
)

func failingDetector(string) (*harness.Detector, error) { return nil, errors.New("no executable") }

func names(checks []doctor.Check) []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name()
	}
	return out
}

// The list is the one main.go's newDoctor built before the TUI shared it:
// same checks, same order, Update only for release builds. Nothing is run.
func TestChecksListAndOrder(t *testing.T) {
	opts := Options{Version: "dev", Update: update.Options{InstallDir: t.TempDir()}, NewDetector: failingDetector}
	dev := names(Checks(opts))
	if len(dev) < 5 || dev[0] != "Executable" || dev[1] != "PATH" || dev[2] != "Version" || dev[3] != "AI clients" {
		t.Fatalf("dev build checks = %v, want Executable, PATH, Version, AI clients, then the server's own", dev)
	}
	for _, n := range dev {
		if n == "Update" {
			t.Errorf("a development build must not run the Update check: %v", dev)
		}
	}

	opts.Version = "0.4.0"
	rel := names(Checks(opts))
	if len(rel) != len(dev)+1 || rel[4] != "Update" {
		t.Fatalf("release build checks = %v, want Update right after AI clients", rel)
	}
	if strings.Join(rel[5:], ",") != strings.Join(dev[4:], ",") {
		t.Errorf("the server's own checks differ: %v vs %v", rel[5:], dev[4:])
	}
}

func TestVersionCheckReportsVersionAndRestartAdvice(t *testing.T) {
	res := versionCheck{version: "1.2.3"}.Run(context.Background())
	if res.Name != "Version" || res.Status != doctor.OK {
		t.Fatalf("result = %+v", res)
	}
	for _, want := range []string{"1.2.3", "server_version", "restart the AI client"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail %q missing %q", res.Detail, want)
		}
	}
}

func TestClientsCheckFailsCleanlyWithoutADetector(t *testing.T) {
	res := clientsCheck{newDetector: failingDetector}.Run(context.Background())
	if res.Status != doctor.Fail || res.Detail != "no executable" {
		t.Errorf("result = %+v, want a fail carrying the detector error", res)
	}
	res = clientsCheck{}.Run(context.Background())
	if res.Status != doctor.Fail {
		t.Errorf("a missing detector func must fail, not panic: %+v", res)
	}
}
