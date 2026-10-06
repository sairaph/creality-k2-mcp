package tui

import (
	"image"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
)

// cameraHarness opens Camera on one enabled printer, so the action list shows
// directly. dir is Deps.Dir (where a snapshot is written); empty means the
// global registry directory.
func cameraHarness(t *testing.T, w, h int, d *fakeDaemonClient, opener *fakeOpener, dir string) *harness {
	t.Helper()
	isolateHome(t)
	addPrinter(t, "k2-5885", "K2-5885", "192.168.1.10", true, false)
	deps := Deps{Dir: dir, PrinterClients: fakePrinterClients(nil)}
	if d != nil {
		deps.Daemon = d
	}
	if opener != nil {
		deps.Opener = opener.open
	}
	hs := newHarness(t, deps, w, h)
	hs.open("Camera")
	return hs
}

func TestCameraOnePrinterShowsTheActionsDirectly(t *testing.T) {
	for _, sz := range sizes {
		h := cameraHarness(t, sz[0], sz[1], &fakeDaemonClient{}, &fakeOpener{}, "")
		h.requireFrame("camera")
		ls := h.lines()
		if ls[0] != "creality-k2-mcp  Camera  K2-5885 (192.168.1.10)" {
			t.Fatalf("header = %q", ls[0])
		}
		want := []string{
			"  > Open live view in the browser", "    Take a snapshot", "    Start recording", "    Stop recording",
		}
		for i, w := range want {
			if ls[2+i] != w {
				t.Errorf("row %d = %q, want %q", 3+i, ls[2+i], w)
			}
		}
		if strings.Contains(h.text(), "Back") {
			t.Errorf("the action list has no Back item:\n%s", h.text())
		}
		if got := strings.TrimSpace(h.footer()); got != "↑↓ move · enter run · esc back · q quit" {
			t.Errorf("footer = %q", got)
		}
	}
}

func TestCameraFooterKeysChangeState(t *testing.T) {
	h := cameraHarness(t, 120, 36, &fakeDaemonClient{viewerURL: "http://127.0.0.1:9100/"}, &fakeOpener{}, "")
	h.key("down")
	if !strings.HasPrefix(h.lines()[3], "  > Take a snapshot") {
		t.Errorf("down did not move the cursor: %q", h.lines()[3])
	}
	h.key("up")
	if !strings.HasPrefix(h.lines()[2], "  > Open live view") {
		t.Errorf("up did not move the cursor: %q", h.lines()[2])
	}
	h.key("enter")
	if !strings.Contains(h.text(), "Opened http://127.0.0.1:9100/ in your browser.") {
		t.Errorf("enter did not run the action:\n%s", h.text())
	}
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Errorf("esc did not leave: %q", h.lines()[0])
	}
}

func TestCameraOpenLiveView(t *testing.T) {
	d := &fakeDaemonClient{viewerURL: "http://127.0.0.1:9100/?printer=k2-5885"}
	opener := &fakeOpener{}
	h := cameraHarness(t, 120, 36, d, opener, "")
	h.key("enter")
	if len(opener.opened) != 1 || opener.opened[0] != d.viewerURL {
		t.Fatalf("opener.opened = %v, want [%s]", opener.opened, d.viewerURL)
	}
	if !containsCall(d.calls, "ViewerURL") {
		t.Errorf("calls = %v, want ViewerURL", d.calls)
	}
	h.requireFrame("camera after opening the live view")
}

func TestCameraStartAndStopRecording(t *testing.T) {
	d := &fakeDaemonClient{
		startResult: daemon.RecordingInfo{ID: "rec-1"},
		listResult:  daemon.RecordingListResult{Recordings: []daemon.RecordingInfo{{ID: "rec-1", PrinterID: "k2-5885", Active: true}}},
		stopResult:  daemon.RecordingInfo{ID: "rec-1", StopReason: "stopped by user"},
	}
	h := cameraHarness(t, 120, 36, d, &fakeOpener{}, "")
	h.key("down", "down", "enter")
	if !containsCall(d.calls, "StartRecording") || !strings.Contains(h.text(), "Recording started (rec-1).") {
		t.Errorf("start: calls = %v\n%s", d.calls, h.text())
	}
	h.key("down", "enter")
	if !containsCall(d.calls, "StopRecording") || !strings.Contains(h.text(), "Recording rec-1 stopped (stopped by user).") {
		t.Errorf("stop: calls = %v\n%s", d.calls, h.text())
	}
}

func TestCameraStopWithNothingRecording(t *testing.T) {
	h := cameraHarness(t, 120, 36, &fakeDaemonClient{}, &fakeOpener{}, "")
	h.key("down", "down", "down", "enter")
	if !strings.Contains(h.text(), "K2-5885 has no active recording") {
		t.Errorf("want a no-active-recording error:\n%s", h.text())
	}
}

// TestCameraSnapshot confirms "Take a snapshot" calls Deps.Daemon.Snapshot
// (review backlog item 51: the daemon hub's rolling GOP buffer, not a direct
// WebRTC session) and saves the result as a JPEG file under Deps.Dir.
func TestCameraSnapshot(t *testing.T) {
	dir := t.TempDir()
	d := &fakeDaemonClient{snapshotResult: &camera.SnapshotResult{
		Image: image.NewRGBA(image.Rect(0, 0, 4, 4)), CapturedAt: time.Now(),
	}}
	h := cameraHarness(t, 120, 36, d, &fakeOpener{}, dir)
	h.key("down", "enter")
	if !containsCall(d.calls, "Snapshot") {
		t.Errorf("calls = %v, want Snapshot", d.calls)
	}
	if !strings.Contains(h.text(), "Snapshot saved to") {
		t.Errorf("want the saved path in the result line:\n%s", h.text())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
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

func TestCameraWithoutADaemonSaysSoForEveryAction(t *testing.T) {
	h := cameraHarness(t, 120, 36, nil, nil, "")
	for i := 0; i < len(cameraActions); i++ {
		h.key("enter")
		if !strings.Contains(h.text(), "The camera daemon is not available.") {
			t.Errorf("action %d: no daemon message:\n%s", i, h.text())
		}
		h.key("down")
	}
}

func TestCameraActionFailureIsShown(t *testing.T) {
	d := &fakeDaemonClient{viewerURLErr: os.ErrDeadlineExceeded}
	h := cameraHarness(t, 80, 24, d, &fakeOpener{}, "")
	h.key("enter")
	if !strings.Contains(h.text(), "open the live view:") {
		t.Errorf("the failure is not shown:\n%s", h.text())
	}
	h.requireFrame("camera after a failure")
}

// While the daemon works the screen is not locked: enter does not start a
// second action, but q and esc work and the late result is dropped.
func TestCameraKeysWorkWhileAnActionRuns(t *testing.T) {
	d := &fakeDaemonClient{viewerURL: "http://x/"}
	h := cameraHarness(t, 120, 36, d, &fakeOpener{}, "")
	s := h.m.screen.(*cameraScreen)
	s.working, s.kind, s.result = true, resultWorking, "Opening the live view..."
	gen := s.opGen

	h.requireFrame("camera while working")
	if !strings.Contains(h.text(), "Opening the live view...") {
		t.Errorf("the working line is missing:\n%s", h.text())
	}
	if got := strings.TrimSpace(h.footer()); got != "↑↓ move · working... · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("enter")
	if len(d.calls) != 0 {
		t.Errorf("enter started a second action: %v", d.calls)
	}

	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Fatalf("esc did not leave while working: %q", h.lines()[0])
	}
	h.send(cameraResultMsg{gen: gen, text: "late"}) // must not panic or show anywhere
	if strings.Contains(h.text(), "late") {
		t.Error("a result of a left screen showed up")
	}

	h = cameraHarness(t, 120, 36, d, &fakeOpener{}, "")
	h.m.screen.(*cameraScreen).working = true
	h.key("q")
	if !h.quit {
		t.Error("q did not quit while an action ran")
	}
}

func TestCameraPickerForSeveralPrinters(t *testing.T) {
	for _, sz := range sizes {
		isolateHome(t)
		addPrinter(t, "k2-5885", "K2-5885", "192.168.1.10", true, false)
		addPrinter(t, "k2-lab", "K2-Lab", "192.168.1.11", true, false)
		h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil), Daemon: &fakeDaemonClient{}}, sz[0], sz[1])
		h.open("Camera")
		h.requireFrame("camera picker")
		if h.lines()[0] != "creality-k2-mcp  Camera  choose a printer" {
			t.Fatalf("header = %q", h.lines()[0])
		}
		h.key("down", "enter")
		if h.lines()[0] != "creality-k2-mcp  Camera  K2-Lab (192.168.1.11)" {
			t.Fatalf("after choosing: header = %q", h.lines()[0])
		}
		h.requireFrame("camera actions")
		h.key("esc")
		if h.lines()[0] != "creality-k2-mcp  Camera  choose a printer" {
			t.Errorf("esc must return to the picker: %q", h.lines()[0])
		}
		h.key("esc")
		if !strings.Contains(h.lines()[0], "Menu") {
			t.Errorf("esc must return to the menu: %q", h.lines()[0])
		}
	}
}

func TestCameraNoPrinterEnabled(t *testing.T) {
	isolateHome(t)
	addPrinter(t, "k2-off", "K2-Off", "192.168.1.10", false, false)
	h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil)}, 80, 24)
	h.open("Camera")
	h.requireFrame("camera without printers")
	if !strings.Contains(h.text(), "No printer is enabled. Enable one in Printers.") {
		t.Errorf("missing the empty-state sentence:\n%s", h.text())
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
