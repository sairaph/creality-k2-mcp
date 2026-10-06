package tui

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

func shrinkStatusInterval(t *testing.T) {
	t.Helper()
	old := statusRefreshInterval
	statusRefreshInterval = time.Millisecond
	t.Cleanup(func() { statusRefreshInterval = old })
}

// statusHarness opens Status on one enabled idle printer (plus a disabled one
// that must never show up) with a watchdog armed on the nozzle.
func statusHarness(t *testing.T, w, h int) *harness {
	t.Helper()
	isolateHome(t)
	shrinkStatusInterval(t)
	addPrinter(t, "k2-5885", "K2-5885", "192.168.1.102", true, false)
	addPrinter(t, "k2-off", "K2-Off", "192.168.1.103", false, false)
	deadline := time.Date(2026, 10, 6, 14, 5, 0, 0, time.Local)
	d := &fakeDaemonClient{
		statusOK:      true,
		statusHeaters: []daemon.HeaterStatus{{Heater: "extruder", Armed: true, TargetC: 60, DeadlineAt: deadline}},
	}
	deps := idleDeps(map[string]string{"k2-5885": "K2-5885", "k2-off": "K2-Off"}, d)
	deps.PrinterClients = fakePrinterClients(map[string]*fakeMoonrakerClient{
		"k2-5885": cleanIdleMoonraker("K2-5885"), "k2-off": cleanIdleMoonraker("K2-Off"),
	})
	hs := newHarness(t, deps, w, h)
	hs.open("Status")
	return hs
}

var statusRowRE = regexp.MustCompile(`^  (State|Nozzle|Bed|Job|Speed|Fans|Light|CFS|Watchdog|Links|Details) `)

// renderRows is the Status summary rows at width w, colours stripped.
func renderRows(w int, d statusData) string {
	return ansi.Strip(strings.Join(frame.Rows(w, statusLabelWidth, statusRows(d)...), "\n"))
}

func hasHint(hints []frame.Hint, text string) bool {
	for _, h := range hints {
		if strings.TrimSpace(h.Keys+" "+h.Label) == text {
			return true
		}
	}
	return false
}

func TestStatusSinglePrinterOpensDirectlyAndAligns(t *testing.T) {
	for _, sz := range sizes {
		h := statusHarness(t, sz[0], sz[1])
		h.requireFrame("status")
		ls := h.lines()
		if ls[0] != "creality-k2-mcp  Status  K2-5885 (192.168.1.102)" {
			t.Fatalf("%dx%d: header = %q, want the printer in the context (no picker for one printer)", sz[0], sz[1], ls[0])
		}
		if strings.Contains(h.text(), "K2-Off") {
			t.Errorf("a disabled printer is shown:\n%s", h.text())
		}
		// The printer name is in the header only.
		for _, l := range ls[2 : len(ls)-1] {
			if strings.Contains(l, "K2-5885") {
				t.Errorf("%dx%d: the printer name is repeated in the body: %q", sz[0], sz[1], l)
			}
		}

		// One label column: every row's value starts in column 19.
		rows := 0
		for _, l := range ls[2 : len(ls)-1] {
			if !statusRowRE.MatchString(l) {
				continue
			}
			rows++
			if len(l) <= 18 || l[17] != ' ' || l[18] == ' ' {
				t.Errorf("%dx%d: row %q does not put its value in column 19", sz[0], sz[1], l)
			}
		}
		if rows < 8 {
			t.Errorf("%dx%d: only %d summary rows:\n%s", sz[0], sz[1], rows, h.text())
		}
		for _, want := range []string{
			"State           ● Idle",
			"Job             none",
			"Light           ",
			"Watchdog        armed: nozzle until 14:05",
			"Links           Moonraker ok · port 9999 ok",
		} {
			if !strings.Contains(h.text(), want) {
				t.Errorf("%dx%d: missing %q:\n%s", sz[0], sz[1], want, h.text())
			}
		}
		if strings.Contains(h.text(), "Details") {
			t.Errorf("%dx%d: a clean idle printer must not show Details:\n%s", sz[0], sz[1], h.text())
		}
		if strings.Contains(h.text(), "bucket") || strings.Contains(h.text(), "class") {
			t.Errorf("%dx%d: bucket and class are dropped from the TUI:\n%s", sz[0], sz[1], h.text())
		}
		if got := strings.TrimSpace(h.footer()); got != "esc back · q quit" {
			t.Errorf("%dx%d: footer = %q, want no scroll hint when everything fits", sz[0], sz[1], got)
		}
	}
}

func TestStatusEscGoesToTheMenuForOnePrinter(t *testing.T) {
	h := statusHarness(t, 120, 36)
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Errorf("header = %q, want the menu", h.lines()[0])
	}
}

func TestStatusPickerForSeveralPrinters(t *testing.T) {
	for _, sz := range sizes {
		isolateHome(t)
		shrinkStatusInterval(t)
		addPrinter(t, "k2-5885", "K2-5885", "192.168.1.102", true, false)
		addPrinter(t, "k2-lab", "K2-Lab", "192.168.1.103", true, false)
		h := newHarness(t, idleDeps(map[string]string{"k2-5885": "K2-5885", "k2-lab": "K2-Lab"}, nil), sz[0], sz[1])
		h.open("Status")
		h.requireFrame("status picker")

		ls := h.lines()
		if ls[0] != "creality-k2-mcp  Status  choose a printer" {
			t.Fatalf("header = %q", ls[0])
		}
		if !strings.HasPrefix(ls[2], "  > K2-5885") || !strings.Contains(ls[2], "192.168.1.102") {
			t.Errorf("first picker row = %q, want a name and a host", ls[2])
		}
		if !strings.HasPrefix(ls[3], "    K2-Lab") {
			t.Errorf("second picker row = %q", ls[3])
		}
		if strings.Contains(h.text(), "Idle") || strings.Contains(h.text(), "idle") {
			t.Errorf("the picker must not probe or show a state:\n%s", h.text())
		}
		if got := strings.TrimSpace(h.footer()); got != "↑↓ move · enter open · esc back · q quit" {
			t.Errorf("footer = %q", got)
		}

		h.key("down", "enter")
		if ls := h.lines(); ls[0] != "creality-k2-mcp  Status  K2-Lab (192.168.1.103)" {
			t.Fatalf("after choosing: header = %q", ls[0])
		}
		h.key("esc")
		if ls := h.lines(); ls[0] != "creality-k2-mcp  Status  choose a printer" || !strings.HasPrefix(ls[3], "  > K2-Lab") {
			t.Errorf("esc from the live view must return to the picker with its cursor: %q / %q", ls[0], ls[3])
		}
		h.key("esc")
		if !strings.Contains(h.lines()[0], "Menu") {
			t.Errorf("esc from the picker must return to the menu: %q", h.lines()[0])
		}
	}
}

func TestStatusQuitsFromTheLiveView(t *testing.T) {
	h := statusHarness(t, 120, 36)
	h.key("q")
	if !h.quit {
		t.Error("q did not quit from the live view")
	}
}

func TestStatusNoPrintersEnabled(t *testing.T) {
	for _, sz := range sizes {
		isolateHome(t)
		addPrinter(t, "k2-off", "K2-Off", "192.168.1.103", false, false)
		h := newHarness(t, idleDeps(nil, nil), sz[0], sz[1])
		h.open("Status")
		h.requireFrame("status without printers")
		if !strings.Contains(h.text(), "No printer is enabled. Enable one in Printers.") {
			t.Errorf("missing the empty-state sentence:\n%s", h.text())
		}
		if got := strings.TrimSpace(h.footer()); got != "esc back · q quit" {
			t.Errorf("footer = %q", got)
		}
	}
}

func TestStatusRegistryErrorOffersRetry(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, idleDeps(nil, nil), 120, 36)
	h.open("Status")
	s := h.m.screen.(*statusScreen)
	h.send(pickerLoadedMsg{id: s.picker.id, err: errors.New("registry is unreadable")})
	h.requireFrame("status error")
	if !strings.Contains(h.text(), "registry is unreadable") {
		t.Errorf("the error is not shown:\n%s", h.text())
	}
	if got := strings.TrimSpace(h.footer()); got != "r retry · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("r") // reloads the (healthy, empty) registry
	if !strings.Contains(h.text(), "No printer is enabled.") || strings.Contains(h.text(), "unreadable") {
		t.Errorf("retry did not reload:\n%s", h.text())
	}
}

func TestStatusLoadingShowsASpinnerLine(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, idleDeps(nil, nil), 120, 36)
	s := newStatusScreen(h.m.ctx, h.deps)
	h.m.screen = s
	s.open(domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.102"}) // the load is not run
	got := h.lines()[2]
	if !strings.HasSuffix(got, " Reading K2-5885...") || !strings.HasPrefix(got, "  ") {
		t.Errorf("loading row = %q, want an indented spinner line", got)
	}
}

// Every activity state has a word and a colour group, and nothing else is in
// the table.
func TestStatusStateTableCoversEveryState(t *testing.T) {
	ok := []string{printerstate.StateIdle, printerstate.StateComplete, printerstate.StateCancelled}
	busy := []string{
		printerstate.StatePrinting, printerstate.StatePreparing, printerstate.StatePaused, printerstate.StatePausing,
		printerstate.StateResuming, printerstate.StateCancelling, printerstate.StateHoming, printerstate.StateCalibrating,
		printerstate.StateBusyCommand, printerstate.StateFilamentOperation, printerstate.StateCFSOperation,
		printerstate.StateUpgrading, printerstate.StateRecoveryPending,
	}
	blocked := []string{
		printerstate.StateError, printerstate.StateOffline, printerstate.StateKlippyNotReady, printerstate.StateUnknown,
		printerstate.StateIdentityMismatch, printerstate.StateIdentityUnverified,
	}
	all := append(append(append([]string{}, ok...), busy...), blocked...)
	if len(all) != 22 {
		t.Fatalf("test lists %d states, want all 22 printerstate.State* constants", len(all))
	}
	if len(statusStateWords) != len(all) {
		t.Errorf("the table has %d entries, want %d", len(statusStateWords), len(all))
	}

	check := func(group string, tone stateTone, states []string) {
		for _, s := range states {
			sw, found := statusStateWords[s]
			if !found || sw.word == "" {
				t.Errorf("%s: no word for state %q", group, s)
				continue
			}
			if sw.tone != tone {
				t.Errorf("state %q is in the wrong colour group", s)
			}
			// The state renders as its own row, whole.
			text := renderRows(80, statusData{block: printerstate.StateBlock{ActivityState: s}})
			if !strings.Contains(text, "State           ● "+sw.word) {
				t.Errorf("state %q: rendered rows lack %q:\n%s", s, sw.word, text)
			}
		}
	}
	check("ok", toneOK, ok)
	check("busy", toneBusy, busy)
	check("blocked", toneBlocked, blocked)

	want := map[string]string{
		printerstate.StateIdle: "Idle", printerstate.StateComplete: "Complete", printerstate.StateCancelled: "Cancelled",
		printerstate.StatePrinting: "Printing", printerstate.StatePreparing: "Starting", printerstate.StatePaused: "Paused",
		printerstate.StatePausing: "Pausing", printerstate.StateResuming: "Resuming", printerstate.StateCancelling: "Cancelling",
		printerstate.StateHoming: "Homing", printerstate.StateCalibrating: "Calibrating",
		printerstate.StateBusyCommand: "Busy (manual command)", printerstate.StateFilamentOperation: "Moving filament",
		printerstate.StateCFSOperation: "CFS operation", printerstate.StateUpgrading: "Updating firmware",
		printerstate.StateRecoveryPending: "Power-loss recovery pending",
		printerstate.StateError:           "Error", printerstate.StateOffline: "Offline",
		printerstate.StateKlippyNotReady: "Klipper not ready", printerstate.StateUnknown: "Unknown (writes blocked)",
		printerstate.StateIdentityMismatch: "Different printer at this address", printerstate.StateIdentityUnverified: "Identity not verified",
	}
	for state, word := range want {
		if got := statusStateWords[state].word; got != word {
			t.Errorf("state %q reads %q, want %q", state, got, word)
		}
	}

	if got := stateWordOf(printerstate.StateBlock{ActivityState: printerstate.StatePreparing, StartWindow: true}).word; got != "Starting (printer self-test)" {
		t.Errorf("preparing in the start window reads %q", got)
	}
	if sw := stateWordOf(printerstate.StateBlock{ActivityState: "brand_new_state"}); sw.word != "brand_new_state" || sw.tone != toneBlocked {
		t.Errorf("an unknown state must show as is, in red: %+v", sw)
	}
}

func TestStatusColoursFollowTheStateGroup(t *testing.T) {
	if !strings.Contains(styleOK.Render("x"), "\x1b[") {
		t.Skip("no colour profile in this environment")
	}
	render := func(state string) string {
		d := statusData{block: printerstate.StateBlock{ActivityState: state}}
		return strings.Join(frame.Rows(80, statusLabelWidth, statusRows(d)...), "\n")
	}
	ok, busy, blocked := render(printerstate.StateIdle), render(printerstate.StatePrinting), render(printerstate.StateError)
	if ok == busy || busy == blocked || ok == blocked {
		t.Errorf("the three state groups must render in three colours:\n%q\n%q\n%q", ok, busy, blocked)
	}
}

func TestStatusRowsForAPrintingJob(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	on := true
	d := statusData{
		block: printerstate.StateBlock{
			ActivityState:      printerstate.StatePrinting,
			NozzleTemperatureC: f(205.2), NozzleTargetC: f(210),
			BedTemperatureC: f(45), BedTargetC: f(0),
			Job:         &printerstate.JobIdentity{Filename: "bracket.gcode"},
			SpeedPreset: printerstate.PresetStandard, SpeedFactorPercent: f(100),
			PartFanPercent: f(80), CaseFanPercent: f(0),
			LightOn:         &on,
			Ws9999Reachable: true,
		},
		progress: 42, layer: 31, layers: 120, moonrakerOK: true, watchdogKnown: true,
	}
	text := renderRows(120, d)
	for _, want := range []string{
		"Nozzle          205 C  -> 210 C",
		"Bed             45 C   (off)",
		"Job             bracket.gcode   42%   layer 31/120",
		"Speed           standard (100%)",
		"Fans            part 80%   case 0%   aux unknown",
		"Light           on",
		"Watchdog        no heaters armed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "CFS") {
		t.Errorf("the CFS row must be left out without a CFS:\n%s", text)
	}

	d.block.SpeedPreset = printerstate.PresetSilent
	if got := renderRows(120, d); !strings.Contains(got, "Speed           silent\n") {
		t.Errorf("Silent must read plain \"silent\":\n%s", got)
	}
	d.block.SpeedPreset, d.block.SpeedFactorPercent = "", nil
	if got := renderRows(120, d); strings.Contains(got, "Speed") {
		t.Errorf("an unknown speed must leave the row out:\n%s", got)
	}
}

func TestStatusCFSAndWatchdogRows(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	base := statusData{block: printerstate.StateBlock{ActivityState: printerstate.StateIdle, Ws9999Reachable: true}, moonrakerOK: true, watchdogKnown: true}

	for state, want := range map[string]string{
		printerstate.CFSStateIdle:    "CFS             idle",
		printerstate.CFSStateInPrint: "CFS             in print",
		printerstate.CFSStateBusy:    "CFS             busy (feeding)",
		printerstate.CFSStateError:   "CFS             error: feeding",
		printerstate.CFSStateUnknown: "CFS             unknown",
	} {
		d := base
		d.block.CFS = &printerstate.CFSBlock{State: state, Reasons: []string{"feeding"}}
		if got := renderRows(120, d); !strings.Contains(got, want) {
			t.Errorf("cfs %q: missing %q:\n%s", state, want, got)
		}
	}

	// A CFS in error brings the Details row back even for an idle printer.
	d := base
	d.block.CFS = &printerstate.CFSBlock{State: printerstate.CFSStateError, Reasons: []string{"cfs box 1 error"}}
	d.block.Reasons = []string{"first", "second", "third", "fourth"}
	got := renderRows(120, d)
	if !strings.Contains(got, "Details         first") || strings.Contains(got, "fourth") {
		t.Errorf("Details must list at most 3 reasons:\n%s", got)
	}

	// Daemon down: amber only while a heater target is set.
	down := base
	down.watchdogKnown = false
	if got := renderRows(120, down); !strings.Contains(got, "Watchdog        daemon not running") {
		t.Errorf("missing the daemon line:\n%s", got)
	}
	if strings.Contains(styleWarn.Render("x"), "\x1b[") {
		cold := strings.Join(frame.Rows(120, statusLabelWidth, statusRows(down)...), "\n")
		down.block.NozzleTargetC = f(200)
		hot := strings.Join(frame.Rows(120, statusLabelWidth, statusRows(down)...), "\n")
		if cold == hot {
			t.Error("the daemon line must turn amber when a heater target is set")
		}
	}

	// Port 9999 down: an amber link and Details for an otherwise ok state.
	ws := base
	ws.block.Ws9999Reachable = false
	ws.block.Reasons = []string{"9999 is not answering"}
	got = renderRows(120, ws)
	if !strings.Contains(got, "port 9999 unreachable") || !strings.Contains(got, "Details         9999 is not answering") {
		t.Errorf("an unreachable 9999 must show:\n%s", got)
	}

	// A busy (amber) state never lists reasons; a blocked (red) one does; the
	// informational cfs_connected lines are never shown.
	busy := base
	busy.block.ActivityState = printerstate.StatePrinting
	busy.block.Reasons = []string{"print_stats.state is printing"}
	if got := renderRows(120, busy); strings.Contains(got, "Details") {
		t.Errorf("a printing state must not show Details:\n%s", got)
	}
	blocked := base
	blocked.block.ActivityState = printerstate.StateOffline
	blocked.block.Reasons = []string{"server/info unreachable", "cfs_connected: box object missing from snapshot, failing closed"}
	got = renderRows(120, blocked)
	if !strings.Contains(got, "Details         server/info unreachable") || strings.Contains(got, "cfs_connected") {
		t.Errorf("a blocked state shows its reasons but never cfs_connected lines:\n%s", got)
	}
	clean := base
	clean.block.Reasons = []string{"anything"}
	if got := renderRows(120, clean); strings.Contains(got, "Details") {
		t.Errorf("a clean idle must not show Details:\n%s", got)
	}
}

func TestStatusLongDetailsWrapUnderTheValueColumn(t *testing.T) {
	d := statusData{block: printerstate.StateBlock{
		ActivityState: printerstate.StateOffline,
		Reasons:       []string{"server/info unreachable: " + strings.Repeat("connection refused ", 12)},
	}}
	lines := strings.Split(renderRows(80, d), "\n")
	pad := strings.Repeat(" ", 18)
	found := false
	for i, l := range lines {
		if ansi.StringWidth(l) > 79 {
			t.Errorf("line %d is %d columns wide", i, ansi.StringWidth(l))
		}
		if strings.HasPrefix(l, "  Details") {
			found = true
			continue
		}
		if found && l != "" && !strings.HasPrefix(l, pad) {
			t.Errorf("continuation %q is not under the value column", l)
		}
	}
	if !found {
		t.Fatalf("no Details row:\n%s", strings.Join(lines, "\n"))
	}
}

// A tick that arrives while the previous load is still in flight must not
// start a second one: it only re-arms the ticker (dev_docs/review-backlog.md
// item 40).
func TestStatusTickSkipsWhileLoadInFlight(t *testing.T) {
	isolateHome(t)
	shrinkStatusInterval(t)
	s := newStatusScreen(t.Context(), Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults())
	p := domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}
	s.selected = &p
	s.gen, s.loadSeq, s.loading = 1, 1, true // the previous tick's load is still running

	cmd, _ := s.Update(statusTickMsg{gen: 1})
	for _, m := range drainCmd(cmd) {
		if _, ok := m.(statusLoadedMsg); ok {
			t.Fatal("a tick started a new load while one was already in flight")
		}
	}
	if !s.loading || s.loadSeq != 1 {
		t.Errorf("loading=%v loadSeq=%d, want the in-flight load untouched", s.loading, s.loadSeq)
	}
}

// Once nothing is loading, a tick starts a new load and bumps loadSeq; a tick
// of a retired chain (the user went back to the picker and chose again) does
// nothing.
func TestStatusTickStartsLoadWhenIdleAndIgnoresStaleChains(t *testing.T) {
	isolateHome(t)
	shrinkStatusInterval(t)
	s := newStatusScreen(t.Context(), Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults())
	p := domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}
	s.selected = &p
	s.gen, s.loadSeq = 2, 1

	cmd, _ := s.Update(statusTickMsg{gen: 1}) // a retired chain
	for _, m := range drainCmd(cmd) {
		switch m.(type) {
		case statusTickMsg, statusLoadedMsg:
			t.Errorf("a tick of a retired chain must be dropped, got %T", m)
		}
	}

	cmd, _ = s.Update(statusTickMsg{gen: 2})
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

// A statusLoadedMsg whose seq does not match the screen's current loadSeq (a
// stale result from a load that has since been superseded) must be dropped
// (item 40).
func TestStatusDropsStaleLoadResultBySequence(t *testing.T) {
	isolateHome(t)
	s := newStatusScreen(t.Context(), Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults())
	p := domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}
	s.selected = &p
	s.loading, s.loadSeq = true, 5 // a newer load is the one currently in flight

	stale := statusData{block: printerstate.StateBlock{ActivityState: printerstate.StatePrinting}}
	s.Update(statusLoadedMsg{seq: 3, data: stale})
	if s.data != nil || !s.loading {
		t.Errorf("a stale (lower-seq) result was applied: data=%v loading=%v", s.data, s.loading)
	}

	s.Update(statusLoadedMsg{seq: 5, data: stale})
	if s.data == nil || s.loading {
		t.Errorf("the current-seq result was not applied: data=%v loading=%v", s.data, s.loading)
	}
}

// The scroll offset survives a refresh: the live content is re-drawn from new
// data without jumping back to the top.
func TestStatusScrollOffsetSurvivesRefresh(t *testing.T) {
	isolateHome(t)
	s := newStatusScreen(t.Context(), Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults())
	p := domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}
	s.selected = &p
	s.loadSeq = 1
	d := statusData{block: printerstate.StateBlock{ActivityState: printerstate.StateOffline,
		Reasons: []string{"a", "b", "c"}}}
	s.Update(statusLoadedMsg{seq: 1, data: d})

	if got := len(s.Body(80, 5)); got != 5 {
		t.Fatalf("body rows = %d, want 5", got)
	}
	if hints := s.Hints(80, 5); !hasHint(hints, "↑↓ scroll") {
		t.Errorf("hints %v: want the scroll hint when the content overflows", hints)
	}
	s.Update(keyType(tea.KeyDown))
	s.Update(keyType(tea.KeyDown))
	if s.scroll.off != 2 {
		t.Fatalf("offset = %d, want 2", s.scroll.off)
	}

	s.loadSeq = 2
	s.Update(statusLoadedMsg{seq: 2, data: d})
	s.Body(80, 5)
	if s.scroll.off != 2 {
		t.Errorf("a refresh moved the offset to %d, want it kept at 2", s.scroll.off)
	}

	// A taller terminal clamps the offset instead of leaving it past the end.
	s.Body(80, 30)
	if s.scroll.off != 0 {
		t.Errorf("offset = %d on a body that fits, want 0", s.scroll.off)
	}
	if hints := s.Hints(80, 30); hasHint(hints, "↑↓ scroll") {
		t.Errorf("hints %v: no scroll hint when everything fits", hints)
	}
}

func TestStatusReadFailureOffersRetry(t *testing.T) {
	isolateHome(t)
	s := newStatusScreen(t.Context(), Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults())
	p := domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}
	s.selected = &p
	s.loadSeq = 1
	s.Update(statusLoadedMsg{seq: 1, err: fmt.Errorf("boom")})
	if got := ansi.Strip(strings.Join(s.Body(80, 20), "\n")); !strings.Contains(got, "Could not read status: boom") {
		t.Errorf("body = %q", got)
	}
	if !hasHint(s.Hints(80, 20), "r retry") {
		t.Error("a failed first read must offer r retry")
	}
}
