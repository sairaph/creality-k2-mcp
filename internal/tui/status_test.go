package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app/detail"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

func TestStatusScreenPickerAndLiveView(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-disabled", Name: "K2-Disabled", Host: "192.168.1.11", MoonrakerPort: 7125,
		Hostname: "K2-Disabled", Enabled: false,
	})

	old := statusRefreshInterval
	statusRefreshInterval = time.Millisecond
	t.Cleanup(func() { statusRefreshInterval = old })

	ctx := context.Background()
	fakeDaemon := &fakeDaemonClient{
		statusOK:      true,
		statusHeaters: []daemon.HeaterStatus{{Heater: "extruder", Armed: true, TargetC: 60}},
	}
	// The identity check (internal/printerstate's checkIdentity) requires
	// PrinterInfo to corroborate the registry's persisted hostname before
	// anything else in the snapshot is trusted; without this the derived
	// state would be identity_unverified regardless of the other objects
	// below. The other objects put the printer in the idle row.
	overrides := map[string]*fakeMoonrakerClient{
		"k2-5885": {
			serverInfo:  fakeIdleServerInfo(),
			printerInfo: moonraker.PrinterInfoResult{Hostname: "K2-5885"},
			objects: map[string]json.RawMessage{
				"webhooks":       json.RawMessage(`{"state":"ready"}`),
				"print_stats":    json.RawMessage(`{"state":"standby"}`),
				"pause_resume":   json.RawMessage(`{"is_paused":false}`),
				"idle_timeout":   json.RawMessage(`{"state":"Ready"}`),
				"virtual_sdcard": json.RawMessage(`{"is_active":false}`),
			},
		},
	}
	deps := Deps{PrinterClients: fakePrinterClients(overrides), Daemon: fakeDaemon}.withDefaults()

	s := newStatusScreen()
	loaded, ok := firstOfType[statusPrintersLoadedMsg](drainCmd(s.Init(ctx, deps)))
	if !ok {
		t.Fatal("Init did not produce statusPrintersLoadedMsg")
	}
	s.Update(ctx, deps, loaded)
	if len(s.printers) != 1 {
		t.Fatalf("len(printers) = %d, want 1 (only the enabled one)", len(s.printers))
	}

	selectCmd := s.Update(ctx, deps, listSelect(0))
	if s.mode != statusModeLive {
		t.Fatalf("mode = %v, want statusModeLive", s.mode)
	}

	var text string
	for _, m := range drainCmd(selectCmd) {
		if lm, ok := m.(statusLoadedMsg); ok {
			text = lm.text
		}
		s.Update(ctx, deps, m)
	}
	if text == "" {
		t.Fatal("no status text was loaded")
	}
	if !strings.Contains(text, "K2-5885") || !strings.Contains(text, "idle") {
		t.Errorf("status text missing name/state:\n%s", text)
	}
	if !strings.Contains(text, "extruder") || !strings.Contains(text, "armed") {
		t.Errorf("status text missing watchdog line:\n%s", text)
	}
	if got := s.View(); !strings.Contains(got, "K2-5885") {
		t.Errorf("View() = %q, want it to contain the printer name", got)
	}

	// esc/q from live returns to the picker, not the menu.
	cmd := s.Update(ctx, deps, keyType(tea.KeyEsc))
	if cmd != nil {
		t.Error("expected no command when leaving live view for the picker")
	}
	if s.mode != statusModePicker {
		t.Fatalf("mode = %v, want statusModePicker after esc", s.mode)
	}
}

// A tick that arrives while the previous load is still in flight must not
// start a second one: it only re-arms the ticker (dev_docs/review-backlog.md
// item 40).
func TestStatusScreen_TickSkipsWhileLoadInFlight(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	old := statusRefreshInterval
	statusRefreshInterval = time.Millisecond
	t.Cleanup(func() { statusRefreshInterval = old })

	s := newStatusScreen()
	s.mode = statusModeLive
	s.selected = domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}
	s.detail = detail.New("K2-5885", "Loading...")
	s.loading = true // simulate the previous tick's load still running
	s.loadSeq = 1

	cmd := s.Update(ctx, deps, statusTickMsg{})
	for _, m := range drainCmd(cmd) {
		if _, ok := m.(statusLoadedMsg); ok {
			t.Fatal("a tick started a new load while one was already in flight")
		}
	}
	if !s.loading {
		t.Error("loading flag was cleared by a skipped tick")
	}
	if s.loadSeq != 1 {
		t.Errorf("loadSeq = %d, want unchanged at 1 (no new load started)", s.loadSeq)
	}
}

// Once loading is false, a tick does start a new load and marks loading
// again, bumping loadSeq.
func TestStatusScreen_TickStartsLoadWhenIdle(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	old := statusRefreshInterval
	statusRefreshInterval = time.Millisecond
	t.Cleanup(func() { statusRefreshInterval = old })

	s := newStatusScreen()
	s.mode = statusModeLive
	s.selected = domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}
	s.detail = detail.New("K2-5885", "Loading...")
	s.loadSeq = 1

	cmd := s.Update(ctx, deps, statusTickMsg{})
	found := false
	for _, m := range drainCmd(cmd) {
		if lm, ok := m.(statusLoadedMsg); ok {
			found = true
			if lm.seq != 2 {
				t.Errorf("statusLoadedMsg.seq = %d, want 2", lm.seq)
			}
		}
	}
	if !found {
		t.Fatal("a tick with no load in flight did not start one")
	}
}

// A statusLoadedMsg whose seq does not match the screen's current loadSeq
// (a stale result from a load that has since been superseded) must be
// dropped rather than overwriting the detail view (item 40).
func TestStatusScreen_DropsStaleLoadResultBySequence(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := newStatusScreen()
	s.mode = statusModeLive
	s.detail = detail.New("K2-5885", "Loading...")
	s.loading = true
	s.loadSeq = 5 // a newer load is the one currently in flight

	s.Update(ctx, deps, statusLoadedMsg{seq: 3, text: "STALE RESULT"})
	if got := s.detail.View(); strings.Contains(got, "STALE RESULT") {
		t.Errorf("a stale (lower-seq) result was applied to the detail view:\n%s", got)
	}
	if !s.loading {
		t.Error("loading flag was cleared by a dropped stale result")
	}

	s.Update(ctx, deps, statusLoadedMsg{seq: 5, text: "FRESH RESULT"})
	if got := s.detail.View(); !strings.Contains(got, "FRESH RESULT") {
		t.Errorf("the current-seq result was not applied:\n%s", got)
	}
	if s.loading {
		t.Error("loading flag was not cleared once the current-seq result arrived")
	}
}

func TestStatusScreenNoEnabledPrinters(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := newStatusScreen()
	loaded, ok := firstOfType[statusPrintersLoadedMsg](drainCmd(s.Init(ctx, deps)))
	if !ok {
		t.Fatal("Init did not produce statusPrintersLoadedMsg")
	}
	s.Update(ctx, deps, loaded)
	if !strings.Contains(s.View(), "No printer is enabled") {
		t.Errorf("View() = %q, want the no-enabled-printer message", s.View())
	}
}
