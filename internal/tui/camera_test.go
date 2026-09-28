package tui

import (
	"context"
	"image"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/daemon"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// openCameraActions drives a cameraScreen from Init through selecting the
// one enabled printer, returning it positioned in the action submenu.
func openCameraActions(t *testing.T, ctx context.Context, deps Deps) *cameraScreen {
	t.Helper()
	s := newCameraScreen()
	loaded, ok := firstOfType[cameraPrintersLoadedMsg](drainCmd(s.Init(ctx, deps)))
	if !ok {
		t.Fatal("Init did not produce cameraPrintersLoadedMsg")
	}
	s.Update(ctx, deps, loaded)
	if len(s.printers) != 1 {
		t.Fatalf("len(printers) = %d, want 1", len(s.printers))
	}
	if cmd := s.Update(ctx, deps, listSelect(0)); cmd != nil {
		s.Update(ctx, deps, drainOne(t, cmd))
	}
	if s.mode != cameraModeActions {
		t.Fatalf("mode = %v, want cameraModeActions", s.mode)
	}
	return s
}

func drainOne(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msgs := drainCmd(cmd)
	if len(msgs) == 0 {
		t.Fatal("command produced no message")
	}
	return msgs[0]
}

func TestCameraScreenOpenLiveView(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	fakeDaemon := &fakeDaemonClient{viewerURL: "http://127.0.0.1:9100/?printer=k2-5885"}
	opener := &fakeOpener{}
	deps := Deps{
		PrinterClients: fakePrinterClients(nil),
		Daemon:         fakeDaemon,
		Opener:         opener.open,
	}.withDefaults()

	s := openCameraActions(t, ctx, deps)
	// Cursor starts on "Open live view in browser".
	cmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if len(opener.opened) != 1 || opener.opened[0] != fakeDaemon.viewerURL {
		t.Fatalf("opener.opened = %v, want [%s]", opener.opened, fakeDaemon.viewerURL)
	}
	if !strings.Contains(s.message, fakeDaemon.viewerURL) {
		t.Errorf("message = %q, want it to mention the opened URL", s.message)
	}
	if !containsCall(fakeDaemon.calls, "ViewerURL") {
		t.Errorf("calls = %v, want ViewerURL", fakeDaemon.calls)
	}
}

func TestCameraScreenStartRecording(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	fakeDaemon := &fakeDaemonClient{startResult: daemon.RecordingInfo{ID: "rec-1"}}
	deps := Deps{PrinterClients: fakePrinterClients(nil), Daemon: fakeDaemon}.withDefaults()

	s := openCameraActions(t, ctx, deps)
	s.Update(ctx, deps, keyType(tea.KeyDown)) // -> Take a snapshot
	s.Update(ctx, deps, keyType(tea.KeyDown)) // -> Start recording
	cmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if !containsCall(fakeDaemon.calls, "StartRecording") {
		t.Errorf("calls = %v, want StartRecording", fakeDaemon.calls)
	}
	if !strings.Contains(s.message, "rec-1") {
		t.Errorf("message = %q, want it to mention the recording id", s.message)
	}
}

// TestCameraScreenSnapshot confirms "Take a snapshot" calls
// Deps.Daemon.Snapshot (review backlog item 51: the daemon hub's rolling
// GOP buffer, not a direct WebRTC session) and saves the result as a JPEG
// file under Deps.Dir.
func TestCameraScreenSnapshot(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	dir := t.TempDir()
	fakeDaemon := &fakeDaemonClient{snapshotResult: &camera.SnapshotResult{
		Image: image.NewRGBA(image.Rect(0, 0, 4, 4)), CapturedAt: time.Now(),
	}}
	deps := Deps{Dir: dir, PrinterClients: fakePrinterClients(nil), Daemon: fakeDaemon}.withDefaults()

	s := openCameraActions(t, ctx, deps)
	s.Update(ctx, deps, keyType(tea.KeyDown)) // -> Take a snapshot
	cmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if !containsCall(fakeDaemon.calls, "Snapshot") {
		t.Errorf("calls = %v, want Snapshot", fakeDaemon.calls)
	}
	if !strings.Contains(s.message, "Saved snapshot to") {
		t.Errorf("message = %q, want it to mention the saved snapshot path", s.message)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "k2-5885-") && strings.HasSuffix(e.Name(), ".jpg") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no k2-5885-*.jpg file found in %s, entries: %v", dir, entries)
	}
}

// TestCameraScreenSnapshotNoDaemon confirms the same "daemon not available"
// refusal every other camera action already gives when Deps.Daemon is nil
// applies to "Take a snapshot" too (review backlog item 51: no direct
// fallback capture path).
func TestCameraScreenSnapshotNoDaemon(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := openCameraActions(t, ctx, deps)
	s.Update(ctx, deps, keyType(tea.KeyDown)) // -> Take a snapshot
	cmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if !strings.Contains(s.message, "not available") {
		t.Errorf("message = %q, want it to say the daemon is unavailable", s.message)
	}
}

func TestCameraScreenStopRecordingNoneActive(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	fakeDaemon := &fakeDaemonClient{listResult: daemon.RecordingListResult{}}
	deps := Deps{PrinterClients: fakePrinterClients(nil), Daemon: fakeDaemon}.withDefaults()

	s := openCameraActions(t, ctx, deps)
	s.Update(ctx, deps, keyType(tea.KeyDown))
	s.Update(ctx, deps, keyType(tea.KeyDown))
	s.Update(ctx, deps, keyType(tea.KeyDown)) // -> Stop recording
	cmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if !strings.Contains(s.message, "no active recording") {
		t.Errorf("message = %q, want a no-active-recording error", s.message)
	}
}

func TestCameraScreenNoDaemon(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := openCameraActions(t, ctx, deps)
	cmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if !strings.Contains(s.message, "not available") {
		t.Errorf("message = %q, want it to say the daemon is unavailable", s.message)
	}
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
