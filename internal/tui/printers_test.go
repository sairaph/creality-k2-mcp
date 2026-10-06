package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/creality-k2-mcp/internal/wizard"
)

// printersHarness opens Printers on one enabled printer (control off).
func printersHarness(t *testing.T, w, h int, deps Deps) *harness {
	t.Helper()
	isolateHome(t)
	addPrinter(t, "k2-5885", "K2-5885", "192.168.1.10", true, false)
	if deps.PrinterClients == nil {
		deps.PrinterClients = fakePrinterClients(nil)
	}
	hs := newHarness(t, deps, w, h)
	hs.open("Printers")
	return hs
}

func savedRegistry(t *testing.T) domain.Registry {
	t.Helper()
	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestPrintersListsEnabledControlReachableState(t *testing.T) {
	isolateHome(t)
	addPrinter(t, "k2-5885", "K2-5885", "192.168.1.10", true, true)
	for _, sz := range sizes {
		h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil)}, sz[0], sz[1])
		h.open("Printers")
		h.requireFrame("printers")
		ls := h.lines()
		if ls[0] != "creality-k2-mcp  Printers" {
			t.Errorf("header = %q", ls[0])
		}
		if got := strings.Fields(ls[2]); strings.Join(got, " ") != "NAME HOST ENABLED CONTROL REACHABLE STATE" {
			t.Errorf("%dx%d: columns = %v", sz[0], sz[1], got)
		}
		if got := strings.Fields(ls[3]); len(got) < 6 || got[1] != "K2-5885" || got[2] != "192.168.1.10" ||
			got[3] != "yes" || got[4] != "on" || got[5] != "yes" {
			t.Errorf("%dx%d: row = %v, want name, host, enabled, control and reachable", sz[0], sz[1], got)
		}
		if !strings.HasPrefix(ls[3], "  > ") {
			t.Errorf("row = %q, want the cursor", ls[3])
		}
	}
}

func TestPrintersFooterIsAdaptive(t *testing.T) {
	cases := []struct {
		w, h int
		want string
	}{
		{120, 36, "↑↓ move · space enable · c control · m add host · r rescan · n rename · d remove · esc back · q quit"},
		{80, 24, "↑↓ move · space enable · c control · m add host · esc back · q quit"},
	}
	for _, tc := range cases {
		h := printersHarness(t, tc.w, tc.h, Deps{})
		if got := strings.TrimSpace(h.footer()); got != tc.want {
			t.Errorf("%dx%d: footer = %q, want %q", tc.w, tc.h, got, tc.want)
		}
	}
}

func TestPrintersColumnsDropOnNarrowTerminals(t *testing.T) {
	s := newPrintersScreen(t.Context(), Deps{}.withDefaults())
	s.ps.Rows = []wizard.PrinterRow{{Printer: domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10"}, Addable: true}}
	names := func(w int) string {
		var out []string
		for _, c := range s.columns(w) {
			out = append(out, c.name)
		}
		return strings.Join(out, " ")
	}
	for w, want := range map[int]string{
		120: "NAME HOST ENABLED CONTROL REACHABLE STATE",
		80:  "NAME HOST ENABLED CONTROL REACHABLE STATE",
		70:  "NAME HOST ENABLED CONTROL STATE",
		60:  "NAME HOST ENABLED STATE",
		50:  "NAME ENABLED STATE",
	} {
		if got := names(w); got != want {
			t.Errorf("columns at %d = %q, want %q (REACHABLE, then CONTROL, then HOST go first)", w, got, want)
		}
	}
}

func TestPrintersNarrowFrame(t *testing.T) {
	h := printersHarness(t, 60, 16, Deps{})
	h.requireFrame("printers at 60x16")
}

func TestPrintersToggleEnabledPersists(t *testing.T) {
	h := printersHarness(t, 120, 36, Deps{})
	h.key("space")
	if savedRegistry(t).Printers[0].Enabled {
		t.Error("the registry on disk was not updated")
	}
	if !strings.Contains(h.lines()[3], "no") {
		t.Errorf("the row still reads enabled:\n%s", h.text())
	}
	h.key("space")
	if !savedRegistry(t).Printers[0].Enabled {
		t.Error("enabling again was not saved")
	}
}

func TestPrintersControlNeedsAConfirmation(t *testing.T) {
	for _, sz := range sizes {
		h := printersHarness(t, sz[0], sz[1], Deps{})
		h.key("c")
		h.requireFrame("control dialog")
		if got := strings.TrimSpace(h.footer()); got != "y yes · n no · esc cancel" {
			t.Errorf("dialog footer = %q", got)
		}
		if savedRegistry(t).Printers[0].AllowControl {
			t.Fatal("control must not be persisted before confirmation")
		}
		// The long warning wraps inside the screen instead of being cut off.
		flat := strings.Join(strings.Fields(h.text()), " ")
		if !strings.Contains(flat, "Only allow this for a printer, and an AI client, you trust with those actions.") {
			t.Errorf("%dx%d: the control warning is cut off:\n%s", sz[0], sz[1], h.text())
		}
		h.key("esc")
		if !strings.Contains(h.lines()[0], "Printers") || savedRegistry(t).Printers[0].AllowControl {
			t.Errorf("esc must close the dialog and stay (header %q)", h.lines()[0])
		}
		h.key("c", "q")
		if h.quit || !strings.Contains(h.lines()[0], "Printers") {
			t.Errorf("q must cancel the dialog, not quit (quit=%v)", h.quit)
		}
		h.key("c", "y")
		if !savedRegistry(t).Printers[0].AllowControl {
			t.Error("control was not persisted after confirming")
		}
		h.key("c") // turning it off needs no confirmation
		if savedRegistry(t).Printers[0].AllowControl {
			t.Error("control was not turned off")
		}
	}
}

func TestPrintersControlOnADisabledPrinterIsRefused(t *testing.T) {
	h := printersHarness(t, 120, 36, Deps{})
	h.key("space", "c")
	if !strings.Contains(h.text(), "Enable the printer before allowing control.") {
		t.Errorf("want the refusal:\n%s", h.text())
	}
}

func TestPrintersRenameTypesQAndPersists(t *testing.T) {
	h := printersHarness(t, 120, 36, Deps{})
	h.key("n")
	if !strings.Contains(h.text(), "New name for K2-5885:") {
		t.Fatalf("rename prompt:\n%s", h.text())
	}
	if got := strings.TrimSpace(h.footer()); got != "enter confirm · esc cancel" {
		t.Errorf("footer = %q", got)
	}
	for i := 0; i < len("K2-5885"); i++ {
		h.key("backspace")
	}
	h.typeText("Lab q2")
	if h.quit {
		t.Fatal("q quit while a name was being typed")
	}
	if !strings.Contains(h.text(), "Lab q2_") {
		t.Errorf("the typed name is not shown:\n%s", h.text())
	}
	h.key("enter")
	if got := savedRegistry(t).Printers[0].Name; got != "Lab q2" {
		t.Errorf("Name = %q, want %q", got, "Lab q2")
	}
}

func TestPrintersEscCancelsARename(t *testing.T) {
	h := printersHarness(t, 120, 36, Deps{})
	h.key("n", "esc")
	if !strings.Contains(h.lines()[0], "Printers") || strings.Contains(h.text(), "New name for") {
		t.Errorf("esc must cancel the field and stay:\n%s", h.text())
	}
	if got := savedRegistry(t).Printers[0].Name; got != "K2-5885" {
		t.Errorf("a cancelled rename changed the name to %q", got)
	}
}

func TestPrintersRemoveAndItsAliases(t *testing.T) {
	for _, alias := range []string{"d", "backspace", "delete"} {
		h := printersHarness(t, 120, 36, Deps{})
		h.key(alias)
		if !strings.Contains(h.text(), "Remove K2-5885?") {
			t.Fatalf("%s did not open the remove dialog:\n%s", alias, h.text())
		}
		if alias == "d" {
			h.key("esc")
			if !strings.Contains(h.lines()[0], "Printers") || len(savedRegistry(t).Printers) != 1 {
				t.Fatal("esc must cancel the dialog")
			}
			h.key("d")
		}
		h.key("y")
		if got := len(savedRegistry(t).Printers); got != 0 {
			t.Errorf("%s: %d printers left, want 0", alias, got)
		}
		if !strings.Contains(h.text(), "No printers found. Press m to add one by host, or r to rescan.") {
			t.Errorf("%s: want the empty-state sentence:\n%s", alias, h.text())
		}
	}
	if strings.Contains(printersFooterText(t), "backspace") {
		t.Error("the aliases stay out of the footer")
	}
}

func printersFooterText(t *testing.T) string {
	h := printersHarness(t, 200, 36, Deps{})
	return h.footer()
}

func TestPrintersAddHostTypesQAndPersists(t *testing.T) {
	deps := Deps{Probe: func(ctx context.Context, host string, port int) (discovery.Result, error) {
		return discovery.Result{Host: host, Hostname: "K2-NEW", Model: "K2", IdentifiedK2: true}, nil
	}}
	h := printersHarness(t, 120, 36, deps)
	h.key("m")
	if got := strings.TrimSpace(h.footer()); got != "enter probe · esc cancel" {
		t.Errorf("footer = %q", got)
	}
	h.typeText("q")
	if h.quit || !strings.Contains(h.text(), "q_") {
		t.Fatalf("q must be typed into the field (quit=%v):\n%s", h.quit, h.text())
	}
	h.key("backspace")
	h.key("enter")
	if !strings.Contains(h.text(), "Enter a host name or IP address.") {
		t.Errorf("an empty host must be refused inline:\n%s", h.text())
	}
	h.typeText("192.168.1.50")
	h.key("enter")

	reg := savedRegistry(t)
	if len(reg.Printers) != 2 {
		t.Fatalf("len(Printers) = %d, want 2", len(reg.Printers))
	}
	var added domain.Printer
	for _, p := range reg.Printers {
		if p.Host == "192.168.1.50" {
			added = p
		}
	}
	if added.Host == "" || added.AllowControl {
		t.Errorf("added printer = %+v, want host 192.168.1.50 with control off", added)
	}
}

func TestPrintersEscCancelsAddHost(t *testing.T) {
	h := printersHarness(t, 120, 36, Deps{})
	h.key("m")
	h.typeText("10.0.0.1")
	h.key("esc")
	if strings.Contains(h.text(), "Host name or IP address") || !strings.Contains(h.lines()[0], "Printers") {
		t.Errorf("esc must close the field and stay:\n%s", h.text())
	}
	h.key("m")
	if strings.Contains(h.text(), "10.0.0.1") {
		t.Errorf("the cancelled text came back:\n%s", h.text())
	}
}

func TestPrintersScanNeverSavesByItself(t *testing.T) {
	deps := Deps{Discover: func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{
			Results: []discovery.Result{{Host: "192.168.1.20", Hostname: "K2-New", Model: "K2", IdentifiedK2: true}},
			Scanned: 1, Total: 1,
		}, nil
	}}
	h := printersHarness(t, 120, 36, deps)
	h.key("r")
	if got := len(h.m.screen.(*printersScreen).ps.Rows); got != 2 {
		t.Fatalf("len(Rows) = %d, want 2 (existing + discovered)", got)
	}
	if !strings.Contains(h.text(), "K2-New") {
		t.Errorf("the discovered printer is not listed:\n%s", h.text())
	}
	if got := len(savedRegistry(t).Printers); got != 1 {
		t.Errorf("len(Printers) = %d, want 1 (a scan must never save)", got)
	}
}

// Leaving the screen (q or esc while a scan runs) cancels the scan through its
// context instead of waiting for it.
func TestPrintersCloseCancelsARunningScan(t *testing.T) {
	isolateHome(t)
	started := make(chan struct{})
	deps := Deps{PrinterClients: fakePrinterClients(nil), Discover: func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		close(started)
		<-ctx.Done()
		return discovery.Report{}, ctx.Err()
	}}.withDefaults()
	s := newPrintersScreen(t.Context(), deps)
	s.ps.Ready = true

	cmd := s.startScan()
	done := make(chan struct{})
	go func() {
		drainCmd(cmd)
		close(done)
	}()
	<-started
	s.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the scan did not stop when the screen was closed")
	}
}

func TestPrintersBusyWhileSavingIgnoresEveryKeyButCtrlC(t *testing.T) {
	h := printersHarness(t, 120, 36, Deps{})
	s := h.m.screen.(*printersScreen)
	s.ps.Saving = true

	h.requireFrame("printers while saving")
	if got := strings.TrimSpace(h.footer()); got != "saving... · ctrl+c quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("q", "esc", "space", "d")
	if h.quit || !strings.Contains(h.lines()[0], "Printers") || s.mode != printersModeNormal {
		t.Errorf("a key got through while saving (quit=%v header=%q)", h.quit, h.lines()[0])
	}
	h.key("ctrl+c")
	if !h.quit {
		t.Error("ctrl+c must quit at once, even while saving")
	}
}

func TestPrintersQAndEscWorkBeforeTheRegistryLoads(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil)}, 120, 36)
	h.m.screen = newPrintersScreen(h.m.ctx, h.deps) // never initialised: still "loading"
	h.requireFrame("printers while loading")
	if !strings.Contains(h.text(), "Loading the printer registry...") {
		t.Errorf("missing the loading line:\n%s", h.text())
	}
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Errorf("esc did not leave while loading: %q", h.lines()[0])
	}
	h.m.screen = newPrintersScreen(h.m.ctx, h.deps)
	h.key("q")
	if !h.quit {
		t.Error("q did not quit while loading")
	}
}

func TestPrintersRegistryErrorOffersRetry(t *testing.T) {
	h := printersHarness(t, 120, 36, Deps{})
	h.send(printersRegistryLoadedMsg{err: errors.New("registry is unreadable")})
	h.requireFrame("printers error")
	if !strings.Contains(h.text(), "registry is unreadable") {
		t.Errorf("the error is not shown:\n%s", h.text())
	}
	if got := strings.TrimSpace(h.footer()); got != "r retry · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("r")
	if strings.Contains(h.text(), "unreadable") || !strings.Contains(h.text(), "K2-5885") {
		t.Errorf("retry did not reload the registry:\n%s", h.text())
	}
}

func TestPrintersEmptyRegistry(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil)}, 80, 24)
	h.open("Printers")
	h.requireFrame("empty printers")
	if !strings.Contains(h.text(), "No printers found. Press m to add one by host, or r to rescan.") {
		t.Errorf("missing the empty-state sentence:\n%s", h.text())
	}
}

// --- cancellable probes (v0.4.0 review M1, T1) ---

// blockingProbe is a fake wizard.ProbeFunc that reports its context on started
// and then blocks: with honourCtx until the context is cancelled, without until
// release is closed (a probe that answers late, after the user cancelled it).
func blockingProbe(started chan<- context.Context, release <-chan struct{}, honourCtx bool) wizard.ProbeFunc {
	return func(ctx context.Context, host string, port int) (discovery.Result, error) {
		started <- ctx
		if honourCtx {
			select {
			case <-ctx.Done():
				return discovery.Result{}, ctx.Err()
			case <-release:
			}
		} else {
			<-release
		}
		return discovery.Result{Host: host, Hostname: "K2-LATE", Model: "K2", IdentifiedK2: true}, nil
	}
}

// runAsync runs cmd in the background (a blocked probe must not block the
// test) and delivers every message it produced.
func runAsync(cmd tea.Cmd) <-chan []tea.Msg {
	ch := make(chan []tea.Msg, 1)
	go func() { ch <- drainCmd(cmd) }()
	return ch
}

func awaitCtx(t *testing.T, started <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-started:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("the blocking fake was never called")
		return nil
	}
}

func requireCancelled(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the context was not cancelled", what)
	}
}

// startAddHostProbe opens the add-host field, types a host and presses enter
// without running the returned command through the harness (which would block).
func startAddHostProbe(t *testing.T, h *harness, started <-chan context.Context) (context.Context, <-chan []tea.Msg) {
	t.Helper()
	h.key("m")
	h.typeText("192.168.1.50")
	_, cmd := h.m.Update(keyString("enter"))
	msgs := runAsync(cmd)
	return awaitCtx(t, started), msgs
}

func TestPrintersAddHostProbeIsNotBusyEscAndQCancelIt(t *testing.T) {
	started := make(chan context.Context, 2)
	release := make(chan struct{})
	defer close(release)
	h := printersHarness(t, 120, 36, Deps{Probe: blockingProbe(started, release, true)})
	s := h.m.screen.(*printersScreen)

	ctx, _ := startAddHostProbe(t, h, started)
	if !s.ps.Probing || s.Mode() != ModeNormal {
		t.Fatalf("probing=%v mode=%v, want a probe that is not Busy", s.ps.Probing, s.Mode())
	}
	h.requireFrame("printers while probing")
	if got := strings.TrimSpace(h.footer()); got != "probing... · esc cancel · q quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("space", "d", "m")
	if !s.ps.Probing || h.quit {
		t.Fatal("an ordinary key must be swallowed while probing")
	}

	h.key("esc")
	requireCancelled(t, ctx, "esc")
	if h.quit || s.ps.Probing || s.ps.Adding || !strings.Contains(h.lines()[0], "Printers") {
		t.Errorf("esc must cancel the probe and close the field (quit=%v probing=%v adding=%v header=%q)",
			h.quit, s.ps.Probing, s.ps.Adding, h.lines()[0])
	}

	ctx2, _ := startAddHostProbe(t, h, started)
	h.key("q")
	if !h.quit {
		t.Error("q must quit while probing")
	}
	requireCancelled(t, ctx2, "q")
}

func TestPrintersLateProbeResultAfterCancelChangesNothing(t *testing.T) {
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	h := printersHarness(t, 120, 36, Deps{Probe: blockingProbe(started, release, false)})
	s := h.m.screen.(*printersScreen)

	ctx, msgs := startAddHostProbe(t, h, started)
	h.key("esc")
	requireCancelled(t, ctx, "esc")
	close(release)
	var late []tea.Msg
	select {
	case late = <-msgs:
	case <-time.After(5 * time.Second):
		t.Fatal("the late probe never answered")
	}
	if _, ok := firstOfType[printersProbeDoneMsg](late); !ok {
		t.Fatalf("the fake probe should have produced a result, got %v", late)
	}
	for _, m := range late {
		h.send(m)
	}
	if got := len(s.ps.Rows); got != 1 || s.ps.Saving || s.ps.Probing || s.ps.Adding {
		t.Errorf("a late probe result changed the screen: rows=%d saving=%v probing=%v adding=%v",
			got, s.ps.Saving, s.ps.Probing, s.ps.Adding)
	}
	if got := len(savedRegistry(t).Printers); got != 1 {
		t.Errorf("a late probe result saved: %d printers, want 1", got)
	}
}

// blockingMoonraker holds ServerInfo (the first call of the reachability probe)
// until its context ends or release is closed.
type blockingMoonraker struct {
	*fakeMoonrakerClient
	started chan struct{}
	release <-chan struct{}
}

func (b blockingMoonraker) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return b.fakeMoonrakerClient.ServerInfo(ctx)
}

// The owner's complaint: q and esc must work while the reachability probes
// that run when Printers opens are still in flight.
func TestPrintersQAndEscWorkWhileReachProbesRun(t *testing.T) {
	isolateHome(t)
	addPrinter(t, "k2-5885", "K2-5885", "192.168.1.10", true, false)
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	deps := Deps{PrinterClients: func(p domain.Printer) printerstate.Deps {
		moon := blockingMoonraker{fakeMoonrakerClient: &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()}, started: started, release: release}
		return printerstate.Deps{Moonraker: moon, WS9999: fakeWS9999Client{}}
	}}
	h := newHarness(t, deps, 120, 36)

	openBlocked := func() <-chan []tea.Msg {
		s := newPrintersScreen(h.m.ctx, h.deps)
		h.m.screen = s
		_, cmd := h.m.Update(s.loadCmd()())
		ch := runAsync(cmd)
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the reachability probe never started")
		}
		if s.Mode() != ModeNormal {
			t.Fatalf("mode = %v while the reachability probes run, want Normal", s.Mode())
		}
		return ch
	}

	pending := []<-chan []tea.Msg{openBlocked()}
	h.key("esc")
	if h.quit || !strings.Contains(h.lines()[0], "Menu") {
		t.Fatalf("esc did not leave Printers while probing (quit=%v header=%q)", h.quit, h.lines()[0])
	}
	pending = append(pending, openBlocked())
	h.key("q")
	if !h.quit {
		t.Error("q did not quit while the reachability probes run")
	}
	close(release)
	for _, ch := range pending {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a reachability probe did not finish after release")
		}
	}
}
