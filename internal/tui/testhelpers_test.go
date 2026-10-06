package tui

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// isolateHome points domain's baseDir (~/.creality-k2-mcp) at a fresh temp
// directory, matching internal/wizard and internal/clicmd's own test
// helpers of the same name, so a test never reads or writes a real user's
// registry or settings file (AGENTS.md hard testing rule).
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K2_MCP_HOST", "")
	return home
}

func registerTestPrinter(t *testing.T, dir string, p domain.Printer) {
	t.Helper()
	reg, path, _, err := domain.LoadRegistry(dir)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	reg, err = domain.AddPrinter(reg, p)
	if err != nil {
		t.Fatalf("AddPrinter: %v", err)
	}
	if err := domain.SaveRegistry(path, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
}

// --- fake printerstate clients (never open a socket) ---

type fakeMoonrakerClient struct {
	serverInfo     moonraker.ServerInfoResult
	serverInfoErr  error
	printerInfo    moonraker.PrinterInfoResult
	printerInfoErr error
	objects        map[string]json.RawMessage
}

func (f *fakeMoonrakerClient) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	return f.serverInfo, f.serverInfoErr
}

func (f *fakeMoonrakerClient) PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error) {
	return f.printerInfo, f.printerInfoErr
}

func (f *fakeMoonrakerClient) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	return f.objects, nil
}

func (f *fakeMoonrakerClient) HistoryList(ctx context.Context, limit, start int) (moonraker.HistoryList, error) {
	return moonraker.HistoryList{}, nil
}

func (f *fakeMoonrakerClient) GCodeStore(ctx context.Context, count int) ([]moonraker.GCodeStoreEntry, error) {
	return nil, nil
}

type fakeWS9999Client struct{}

func (fakeWS9999Client) ReadStatus(ctx context.Context) (crealityws.Status, error) {
	return crealityws.Status{}, nil
}

func fakeIdleServerInfo() moonraker.ServerInfoResult {
	return moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"}
}

// fakePrinterClients builds a Deps.PrinterClients that reports every
// printer as reachable and idle, unless overridden per printer id.
func fakePrinterClients(overrides map[string]*fakeMoonrakerClient) PrinterClientsFunc {
	return func(p domain.Printer) printerstate.Deps {
		moon := overrides[p.ID]
		if moon == nil {
			moon = &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()}
		}
		return printerstate.Deps{Moonraker: moon, WS9999: fakeWS9999Client{}}
	}
}

// --- fake daemon client ---

// fakeDaemonClient implements DaemonClient without ever dialing the real
// background daemon (AGENTS.md hard testing rule).
type fakeDaemonClient struct {
	mu sync.Mutex

	viewerURL    string
	viewerURLErr error

	startResult daemon.RecordingInfo
	startErr    error

	stopResult daemon.RecordingInfo
	stopErr    error

	listResult daemon.RecordingListResult
	listErr    error

	deleteErr error

	statusHeaters []daemon.HeaterStatus
	statusOK      bool

	snapshotResult *camera.SnapshotResult
	snapshotErr    error

	// calls records every method invocation for assertions.
	calls []string
}

func (f *fakeDaemonClient) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeDaemonClient) ViewerURL(ctx context.Context, printerID string) (string, error) {
	f.record("ViewerURL")
	return f.viewerURL, f.viewerURLErr
}

func (f *fakeDaemonClient) StartRecording(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error) {
	f.record("StartRecording")
	return f.startResult, f.startErr
}

func (f *fakeDaemonClient) StopRecording(ctx context.Context, id string) (daemon.RecordingInfo, error) {
	f.record("StopRecording")
	return f.stopResult, f.stopErr
}

func (f *fakeDaemonClient) ListRecordings(ctx context.Context) (daemon.RecordingListResult, error) {
	f.record("ListRecordings")
	return f.listResult, f.listErr
}

func (f *fakeDaemonClient) DeleteRecording(ctx context.Context, id string) error {
	f.record("DeleteRecording")
	return f.deleteErr
}

func (f *fakeDaemonClient) Status(ctx context.Context, identity string) ([]daemon.HeaterStatus, bool) {
	f.record("Status")
	return f.statusHeaters, f.statusOK
}

func (f *fakeDaemonClient) Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error) {
	f.record("Snapshot")
	return f.snapshotResult, f.snapshotErr
}

// --- fake opener ---

type fakeOpener struct {
	mu      sync.Mutex
	opened  []string
	failErr error
}

func (f *fakeOpener) open(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, target)
	return f.failErr
}

// --- tea.Cmd draining ---

// drainCmd runs cmd (and, recursively, every command inside a tea.BatchMsg
// it returns) and collects every resulting message, in the order commands
// were run. A nil cmd or a nil message yields nothing.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drainCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// driveCmd repeatedly executes cmd, feeds every resulting message through
// update, and queues whatever further command each feed returns, until
// nothing is pending. This simulates bubbletea's own event loop for
// multi-hop test flows (e.g. select a printer, then an action menu item,
// then a daemon call's result). Never use this with a screen that
// reschedules itself indefinitely (Status's live-view ticker): it would
// never settle.
func driveCmd(cmd tea.Cmd, update func(tea.Msg) tea.Cmd) {
	pending := []tea.Cmd{cmd}
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		for _, m := range drainCmd(c) {
			if next := update(m); next != nil {
				pending = append(pending, next)
			}
		}
	}
}

// firstOfType returns the first message in msgs assignable to *out (a
// pointer to the desired concrete message type), or false.
func firstOfType[T any](msgs []tea.Msg) (T, bool) {
	var zero T
	for _, m := range msgs {
		if v, ok := m.(T); ok {
			return v, true
		}
	}
	return zero, false
}

// --- root model harness ---

func keyRune(r rune) tea.KeyMsg        { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func keyType(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func keyString(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return keyType(tea.KeyEnter)
	case "esc":
		return keyType(tea.KeyEsc)
	case "up":
		return keyType(tea.KeyUp)
	case "down":
		return keyType(tea.KeyDown)
	case "space":
		return keyType(tea.KeySpace)
	case "ctrl+c":
		return keyType(tea.KeyCtrlC)
	case "backspace":
		return keyType(tea.KeyBackspace)
	case "delete":
		return keyType(tea.KeyDelete)
	case "end":
		return keyType(tea.KeyEnd)
	case "left":
		return keyType(tea.KeyLeft)
	case "right":
		return keyType(tea.KeyRight)
	}
	return keyRune([]rune(s)[0])
}

// harness drives the root Model the way bubbletea's event loop would: it feeds
// a key or message to Update and then runs every command that comes back,
// feeding each result in turn, until nothing is pending. Status's refresh tick
// and the spinner tick are dropped (they re-arm themselves while work runs); a test that wants a tick sends it.
type harness struct {
	t      *testing.T
	m      *Model
	w, h   int
	quit   bool
	deps   Deps
	closed int
}

func newHarness(t *testing.T, deps Deps, w, h int) *harness {
	t.Helper()
	hs := &harness{t: t, w: w, h: h, deps: deps.withDefaults()}
	hs.m = newModel(context.Background(), "0.4.0-test", hs.deps)
	hs.send(tea.WindowSizeMsg{Width: w, Height: h})
	hs.feed(hs.m.Init())
	return hs
}

func (h *harness) resize(w, hh int) {
	h.w, h.h = w, hh
	h.send(tea.WindowSizeMsg{Width: w, Height: hh})
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	_, cmd := h.m.Update(msg)
	h.feed(cmd)
}

func (h *harness) key(keys ...string) {
	h.t.Helper()
	for _, k := range keys {
		h.send(keyString(k))
	}
}

func (h *harness) typeText(text string) {
	h.t.Helper()
	for _, r := range text {
		h.send(keyRune(r))
	}
}

func (h *harness) feed(cmd tea.Cmd) {
	h.t.Helper()
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			h.t.Fatal("commands did not settle")
		}
		c := queue[0]
		queue = queue[1:]
		for _, msg := range drainCmd(c) {
			switch msg.(type) {
			case statusTickMsg, spinTickMsg:
				continue
			case tea.QuitMsg:
				h.quit = true
				continue
			}
			_, next := h.m.Update(msg)
			if next != nil {
				queue = append(queue, next)
			}
		}
	}
}

// open picks a menu item by label and settles.
func (h *harness) open(label string) {
	h.t.Helper()
	for i, item := range menuItems {
		if item.label == label {
			h.m.menu.list.cursor = i
			h.key("enter")
			return
		}
	}
	h.t.Fatalf("no menu item %q", label)
}

// lines is the drawn screen with colours stripped, one string per terminal row.
func (h *harness) lines() []string {
	return strings.Split(ansi.Strip(h.m.View()), "\n")
}

func (h *harness) text() string { return strings.Join(h.lines(), "\n") }

func (h *harness) footer() string {
	ls := h.lines()
	return ls[len(ls)-1]
}

// requireFrame asserts the layout every screen shares: exactly H rows, the app
// header on row 1, a blank row 2, the footer on the last row, nothing wider
// than W-1 columns, and a footer made only of the hints the screen offers.
func (h *harness) requireFrame(what string) {
	h.t.Helper()
	ls := h.lines()
	if len(ls) != h.h {
		h.t.Fatalf("%s at %dx%d: %d rows, want exactly %d:\n%s", what, h.w, h.h, len(ls), h.h, strings.Join(ls, "\n"))
	}
	if !strings.HasPrefix(ls[0], "creality-k2-mcp") {
		h.t.Errorf("%s at %dx%d: row 1 = %q, want the header", what, h.w, h.h, ls[0])
	}
	if ls[1] != "" {
		h.t.Errorf("%s at %dx%d: row 2 = %q, want blank", what, h.w, h.h, ls[1])
	}
	for i, l := range ls {
		if got := ansi.StringWidth(l); got > h.w-1 {
			h.t.Errorf("%s at %dx%d: row %d is %d columns wide: %q", what, h.w, h.h, i+1, got, l)
		}
	}
	footer := strings.TrimSpace(ls[len(ls)-1])
	if footer == "" {
		h.t.Fatalf("%s at %dx%d: the last row is empty, want the footer", what, h.w, h.h)
	}
	offered := map[string]bool{}
	for _, hint := range h.m.screen.Hints(h.w, h.h-3) {
		offered[strings.TrimSpace(hint.Keys+" "+hint.Label)] = true
	}
	for _, part := range strings.Split(footer, " · ") {
		if !offered[part] {
			h.t.Errorf("%s at %dx%d: footer part %q is not one of the screen's hints %v", what, h.w, h.h, part, offered)
		}
	}
	for i := 2; i < len(ls)-1; i++ {
		if strings.Contains(ls[i], " · ") && strings.Contains(ls[i], "esc back") {
			h.t.Errorf("%s at %dx%d: a footer-like line in the body at row %d: %q", what, h.w, h.h, i+1, ls[i])
		}
	}
}

// sizes are the two terminal sizes every screen is checked at.
var sizes = [][2]int{{120, 36}, {80, 24}}
