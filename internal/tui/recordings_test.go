package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
)

func recording(id, printer string, startedAt time.Time, active bool, dir string) daemon.RecordingInfo {
	return daemon.RecordingInfo{
		ID: id, PrinterID: printer, Mode: daemon.RecordModeVideo, Active: active,
		StartedAt: startedAt, Bytes: 2048, DurationSeconds: 75,
		Parts: []daemon.RecordingPart{{Path: filepath.Join(dir, id+".mp4")}},
	}
}

// recordingsHarness opens Recordings over three recordings, listed out of
// order, whose printers are K2-5885 (registered) and k2-gone (not).
func recordingsHarness(t *testing.T, w, h int, opener *fakeOpener) (*harness, *fakeDaemonClient) {
	t.Helper()
	isolateHome(t)
	addPrinter(t, "k2-5885", "K2-5885", "192.168.1.10", true, false)
	dir := filepath.Join(t.TempDir(), "k2-5885")
	d := &fakeDaemonClient{listResult: daemon.RecordingListResult{Recordings: []daemon.RecordingInfo{
		recording("rec-old", "k2-5885", time.Date(2026, 9, 1, 8, 0, 0, 0, time.Local), false, dir),
		recording("rec-new", "k2-5885", time.Date(2026, 10, 5, 9, 30, 0, 0, time.Local), true, dir),
		recording("rec-mid", "k2-gone", time.Date(2026, 9, 20, 18, 45, 0, 0, time.Local), false, dir),
	}}}
	deps := Deps{Daemon: d}
	if opener != nil {
		deps.OpenFolder = opener.open
	}
	hs := newHarness(t, deps, w, h)
	hs.open("Recordings")
	return hs, d
}

func headerColumns(h *harness) []string {
	return strings.Fields(h.lines()[2])
}

func TestRecordingsTableColumnsByWidth(t *testing.T) {
	cases := []struct {
		w, h int
		want string
	}{
		{120, 36, "STARTED PRINTER MODE DURATION SIZE STATUS ID"},
		{100, 30, "STARTED PRINTER MODE DURATION SIZE STATUS"},
		{80, 24, "STARTED PRINTER MODE DURATION SIZE STATUS"},
		{70, 24, "STARTED PRINTER MODE DURATION"},
		{60, 16, "STARTED PRINTER MODE DURATION"},
	}
	for _, tc := range cases {
		h, _ := recordingsHarness(t, tc.w, tc.h, nil)
		h.requireFrame("recordings")
		if got := strings.Join(headerColumns(h), " "); got != tc.want {
			t.Errorf("%dx%d: columns = %q, want %q", tc.w, tc.h, got, tc.want)
		}
		if strings.Contains(h.text(), "ACTIVE") {
			t.Errorf("%dx%d: ACTIVE was replaced by STATUS", tc.w, tc.h)
		}
	}
}

func TestRecordingsRowsNewestFirstWithStatusAndPrinterNames(t *testing.T) {
	h, _ := recordingsHarness(t, 120, 36, nil)
	ls := h.lines()
	order := []string{"rec-new", "rec-mid", "rec-old"}
	for i, id := range order {
		if !strings.Contains(ls[3+i], id) {
			t.Errorf("row %d = %q, want %s (newest first)", i+1, ls[3+i], id)
		}
	}
	if !strings.HasPrefix(ls[3], "  > ") || !strings.Contains(ls[3], "recording") || !strings.Contains(ls[3], "K2-5885") {
		t.Errorf("the active row = %q, want the cursor, its printer name and the recording status", ls[3])
	}
	if !strings.Contains(ls[4], "k2-gone") || !strings.Contains(ls[4], "done") {
		t.Errorf("an unregistered printer shows its id: %q", ls[4])
	}
	if !strings.Contains(ls[3], "1m15s") || !strings.Contains(ls[3], "2.0 KiB") {
		t.Errorf("duration and size are missing: %q", ls[3])
	}
	if !strings.Contains(ls[3], "2026-10-05 09:30") {
		t.Errorf("the start time is missing: %q", ls[3])
	}
	if strings.Contains(h.text(), "No data") || strings.Contains(h.text(), "page ") {
		t.Errorf("a library stock text leaked in:\n%s", h.text())
	}
}

func TestRecordingsFooterAndKeys(t *testing.T) {
	opener := &fakeOpener{}
	h, d := recordingsHarness(t, 120, 36, opener)
	if got := strings.TrimSpace(h.footer()); got != "↑↓ move · enter open folder · d delete · r refresh · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}

	h.key("down")
	if !strings.HasPrefix(h.lines()[4], "  > ") {
		t.Errorf("down did not move the cursor:\n%s", h.text())
	}
	h.key("up")
	if !strings.HasPrefix(h.lines()[3], "  > ") {
		t.Errorf("up did not move the cursor:\n%s", h.text())
	}

	h.key("enter")
	if len(opener.opened) != 1 || filepath.Base(opener.opened[0]) != "k2-5885" {
		t.Fatalf("opener.opened = %v, want the recording's folder", opener.opened)
	}
	if !strings.Contains(h.text(), "Opened ") {
		t.Errorf("no result line:\n%s", h.text())
	}

	// o and s were removed on purpose.
	h.key("o", "s")
	if len(opener.opened) != 1 {
		t.Errorf("o must no longer open the folder: %v", opener.opened)
	}
	if !strings.Contains(h.lines()[3], "rec-new") {
		t.Errorf("s must no longer sort:\n%s", h.text())
	}

	before := len(d.calls)
	h.key("r")
	if !containsCall(d.calls[before:], "ListRecordings") {
		t.Errorf("r did not reload: %v", d.calls[before:])
	}
	h.requireFrame("recordings after refresh")

	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Errorf("esc did not leave: %q", h.lines()[0])
	}
}

func TestRecordingsDeleteDialog(t *testing.T) {
	h, d := recordingsHarness(t, 120, 36, nil)
	h.key("d")
	h.requireFrame("recordings dialog")
	if got := strings.TrimSpace(h.footer()); got != "y yes · n no · esc cancel" {
		t.Errorf("dialog footer = %q", got)
	}
	if !strings.Contains(h.text(), "Delete recording rec-new?") || !strings.Contains(h.text(), "permanently deletes") {
		t.Errorf("dialog body:\n%s", h.text())
	}

	// q cancels the dialog instead of quitting; so do esc and n.
	h.key("q")
	if h.quit || strings.Contains(h.text(), "Delete recording") {
		t.Fatalf("q must cancel the dialog (quit=%v):\n%s", h.quit, h.text())
	}
	h.key("d", "esc")
	if strings.Contains(h.text(), "Delete recording") || !strings.Contains(h.lines()[0], "Recordings") {
		t.Fatalf("esc must close the dialog and stay on the screen:\n%s", h.text())
	}
	h.key("d", "n")
	if strings.Contains(h.text(), "Delete recording") {
		t.Fatal("n must cancel")
	}

	// enter takes the highlighted answer, which starts on Cancel.
	h.key("d", "enter")
	if containsCall(d.calls, "DeleteRecording") {
		t.Fatal("enter on the default answer deleted")
	}

	d.listResult = daemon.RecordingListResult{}
	h.key("d", "y")
	if !containsCall(d.calls, "DeleteRecording") {
		t.Fatalf("y did not delete: %v", d.calls)
	}
	if !strings.Contains(h.text(), "Deleted rec-new.") || !strings.Contains(h.text(), "No recordings yet.") {
		t.Errorf("after deleting:\n%s", h.text())
	}
}

func TestRecordingsDeleteWithTheArrowsAndEnter(t *testing.T) {
	h, d := recordingsHarness(t, 80, 24, nil)
	h.key("d", "down", "enter")
	if !containsCall(d.calls, "DeleteRecording") {
		t.Errorf("arrow then enter did not delete: %v", d.calls)
	}
}

func TestRecordingsEmptyState(t *testing.T) {
	for _, sz := range sizes {
		isolateHome(t)
		h := newHarness(t, Deps{Daemon: &fakeDaemonClient{}}, sz[0], sz[1])
		h.open("Recordings")
		h.requireFrame("empty recordings")
		if !strings.Contains(h.text(), "No recordings yet. Start one in Camera, or ask the AI to record a print.") {
			t.Errorf("%dx%d: missing the empty-state sentence:\n%s", sz[0], sz[1], h.text())
		}
		if got := strings.TrimSpace(h.footer()); got != "r refresh · esc back · q quit" {
			t.Errorf("footer = %q", got)
		}
	}
}

func TestRecordingsWithoutADaemonOffersRetry(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, Deps{}, 120, 36)
	h.open("Recordings")
	h.requireFrame("recordings without a daemon")
	if !strings.Contains(h.text(), "The camera daemon is not available.") {
		t.Errorf("missing the error:\n%s", h.text())
	}
	if got := strings.TrimSpace(h.footer()); got != "r retry · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("r")
	if !strings.Contains(h.text(), "The camera daemon is not available.") {
		t.Errorf("retry lost the error:\n%s", h.text())
	}
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Errorf("esc did not leave: %q", h.lines()[0])
	}
}

// The cursor stays visible in a list taller than the body and a resize only
// re-clamps.
func TestRecordingsLongListScrollsWithTheCursor(t *testing.T) {
	isolateHome(t)
	var recs []daemon.RecordingInfo
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < 40; i++ {
		recs = append(recs, recording(fmt.Sprintf("rec-%02d", i), "k2-5885", base.Add(time.Duration(i)*time.Hour), false, t.TempDir()))
	}
	h := newHarness(t, Deps{Daemon: &fakeDaemonClient{listResult: daemon.RecordingListResult{Recordings: recs}}}, 80, 24)
	h.open("Recordings")
	h.key("end")
	h.requireFrame("recordings at the end of a long list")
	if !strings.Contains(h.text(), "> ") || !strings.Contains(h.lines()[len(h.lines())-2], "2026-10-01 00:00") {
		t.Errorf("the last (oldest) row is not on screen:\n%s", h.text())
	}
	h.resize(80, 16)
	h.requireFrame("recordings after shrinking")
	var cursorRows int
	for _, l := range h.lines() {
		if strings.HasPrefix(l, "  > ") {
			cursorRows++
		}
	}
	if cursorRows != 1 {
		t.Errorf("the cursor row is not visible after the resize:\n%s", h.text())
	}
}
