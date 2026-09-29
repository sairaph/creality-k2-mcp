package tui

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"

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

// listSelect builds the app.ActionMsg a list.Model sends on "enter", for
// tests that drive a picker screen (Status, Camera) without going through a
// real key press.
func listSelect(idx int) app.ActionMsg {
	return app.ActionMsg{Source: "list", Value: "select", Data: idx}
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
