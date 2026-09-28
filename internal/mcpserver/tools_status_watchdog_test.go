package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/daemon"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// This file tests get_printer_status's D2 idle-heat watchdog surfacing
// (dev_docs/safety-architecture.md section 10 D2, review backlog item 20):
// daemon liveness and armed heaters via Deps.WatchdogStatus, bounded and
// never autostarting the daemon, plus the idle-heaters-unprotected guidance.

// fakeWatchdogStatus is a WatchdogStatusSource (deps.go) that returns a
// fixed heater list or ok=false instead of dialling a real background
// daemon socket, so these tests never open a non-loopback socket or spawn a
// process (AGENTS.md hard testing rule); it records the identity it was
// last called with.
type fakeWatchdogStatus struct {
	heaters      []daemon.HeaterStatus
	ok           bool
	lastIdentity string
}

func (f *fakeWatchdogStatus) Status(ctx context.Context, identity string) ([]daemon.HeaterStatus, bool) {
	f.lastIdentity = identity
	return f.heaters, f.ok
}

func TestGetPrinterStatus_WatchdogAliveReportsOnlyArmedHeaters(t *testing.T) {
	deadline := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	wd := &fakeWatchdogStatus{ok: true, heaters: []daemon.HeaterStatus{
		{Heater: "extruder", Armed: true, TargetC: 200, DeadlineAt: deadline},
		{Heater: "heater_bed", Armed: false, TargetC: 60},
	}}
	deps.WatchdogStatus = wd
	cs := testSession(t, deps)

	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "daemon_alive: true") {
		t.Fatalf("expected daemon_alive: true in frontmatter:\n%s", text)
	}
	if !strings.Contains(text, "heater: extruder") {
		t.Fatalf("expected the armed extruder heater in frontmatter:\n%s", text)
	}
	if strings.Contains(text, "heater: heater_bed") {
		t.Fatalf("the unarmed heater_bed must not be listed under armed_heaters:\n%s", text)
	}
	if !strings.Contains(text, deadline.Format(time.RFC3339)) {
		t.Fatalf("expected the armed heater's deadline_at in frontmatter:\n%s", text)
	}
	if wd.lastIdentity == "" {
		t.Fatal("WatchdogStatus.Status was never called with a printer identity")
	}
}

func TestGetPrinterStatus_WatchdogUnreachableReportsUnknownAndWarnsIdleHeaters(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, map[string]http.HandlerFunc{
		"/printer/objects/query": objectsQueryOverride(t, map[string]func(map[string]any){
			"extruder": func(m map[string]any) { m["target"] = 200.0 },
		}),
	})
	deps.WatchdogStatus = &fakeWatchdogStatus{ok: false}
	cs := testSession(t, deps)

	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if strings.Contains(text, "daemon_alive:") {
		t.Fatalf("daemon_alive must be omitted (unknown) when the daemon could not be reached:\n%s", text)
	}
	if strings.Contains(text, "armed_heaters:") {
		t.Fatalf("armed_heaters must be omitted when the daemon could not be reached:\n%s", text)
	}
	if !strings.Contains(text, "not currently protected by its automatic turn-off") {
		t.Fatalf("expected the idle-heaters-unprotected warning:\n%s", text)
	}
	if !strings.Contains(text, "never starts the daemon just to check it") {
		t.Fatalf("expected the never-autostart note referencing a later option:\n%s", text)
	}
}

func TestGetPrinterStatus_NilWatchdogStatusReportsUnknown(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	// deps.WatchdogStatus left nil deliberately.
	cs := testSession(t, deps)

	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if strings.Contains(text, "daemon_alive:") {
		t.Fatalf("daemon_alive must be omitted when Deps.WatchdogStatus is not wired up:\n%s", text)
	}
}

func TestGetPrinterStatus_WatchdogAliveWithNoIdleHeatersNoWarning(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	deps.WatchdogStatus = &fakeWatchdogStatus{ok: true}
	cs := testSession(t, deps)

	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if strings.Contains(text, "not currently protected by its automatic turn-off") {
		t.Fatalf("must not warn when no heater target is set while idle:\n%s", text)
	}
	if !strings.Contains(text, "daemon_alive: true") {
		t.Fatalf("expected daemon_alive: true in frontmatter:\n%s", text)
	}
}
