package clicmd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// isolateHome points domain's baseDir (~/.creality_k2_mcp) at a fresh temp
// directory, the same pattern internal/wizard's own tests use
// (internal/wizard/wizard_test.go's isolateHome), so a clicmd test never
// reads or writes a real user's registry or settings file.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows reads USERPROFILE
	t.Setenv("K2_MCP_HOST", "")   // never let a developer's real env override leak into a test
	return home
}

// fakeMoonrakerClient implements printerstate.MoonrakerClient without ever
// opening a socket (AGENTS.md hard testing rule): status and snapshot only
// need to observe printerstate.Take's derived output, never a real
// Moonraker exchange.
type fakeMoonrakerClient struct {
	serverInfo     moonraker.ServerInfoResult
	serverInfoErr  error
	printerInfo    moonraker.PrinterInfoResult
	printerInfoErr error
	objects        map[string]json.RawMessage
	// serverInfoDelay, when non-zero, is slept through before ServerInfo
	// returns: used only by the snapshot concurrency tests to make the
	// state check itself take a measurable, controlled amount of time.
	serverInfoDelay time.Duration
}

func (f *fakeMoonrakerClient) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	if f.serverInfoDelay > 0 {
		time.Sleep(f.serverInfoDelay)
	}
	return f.serverInfo, f.serverInfoErr
}

// PrinterInfo only exists to satisfy printerstate.MoonrakerClient, which
// review backlog item 24 widened to include it (persisted-hostname
// verification, internal/printerstate/state.go checkIdentity). It answers
// with no hostname by default: a clicmd test that registers a printer with
// a persisted Hostname and needs its DeriveActivityState to positively
// match (rather than identity_unverified) should set printerInfo/
// printerInfoErr explicitly, the same way this fake's other fields work.
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

// fakeWS9999Client implements printerstate.WS9999Client, always reporting
// itself reachable with an empty (baseline) status.
type fakeWS9999Client struct{}

func (fakeWS9999Client) ReadStatus(ctx context.Context) (crealityws.Status, error) {
	return crealityws.Status{}, nil
}

// fakeIdleServerInfo is a klippy_connected/ready server/info result: not
// enough by itself to derive any settled state (no print_stats etc.), but
// enough that DeriveActivityState never takes row 1's offline branch, which
// is all the snapshot tests need from it.
func fakeIdleServerInfo() moonraker.ServerInfoResult {
	return moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"}
}
