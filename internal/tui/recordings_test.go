package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func initRecordingsScreen(t *testing.T, ctx context.Context, deps Deps) *recordingsScreen {
	t.Helper()
	s := newRecordingsScreen()
	for _, m := range drainCmd(s.Init(ctx, deps)) {
		s.Update(ctx, deps, m)
	}
	return s
}

func TestRecordingsScreenLists(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	fakeDaemon := &fakeDaemonClient{listResult: daemon.RecordingListResult{
		Recordings: []daemon.RecordingInfo{
			{
				ID: "rec-1", PrinterID: "k2-5885", Mode: daemon.RecordModeVideo, Active: true,
				StartedAt: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
				Bytes:     2048,
				Parts:     []daemon.RecordingPart{{Path: "/recordings/k2-5885/rec-1.mp4"}},
			},
		},
	}}
	deps := Deps{Daemon: fakeDaemon}.withDefaults()

	s := initRecordingsScreen(t, ctx, deps)
	if s.loadErr != "" {
		t.Fatalf("loadErr = %q", s.loadErr)
	}
	if s.table == nil || len(s.table.Rows) != 1 {
		t.Fatalf("table rows = %v, want 1", s.table)
	}
	view := s.View()
	if !strings.Contains(view, "rec-1") || !strings.Contains(view, "K2-5885") {
		t.Errorf("view missing expected content:\n%s", view)
	}
}

func TestRecordingsScreenOpenFolder(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	recDir := filepath.Join(t.TempDir(), "k2-5885")
	partPath := filepath.Join(recDir, "rec-1.mp4")
	fakeDaemon := &fakeDaemonClient{listResult: daemon.RecordingListResult{
		Recordings: []daemon.RecordingInfo{
			{ID: "rec-1", PrinterID: "k2-5885", Parts: []daemon.RecordingPart{{Path: partPath}}},
		},
	}}
	opener := &fakeOpener{}
	deps := Deps{Daemon: fakeDaemon, OpenFolder: opener.open}.withDefaults()

	s := initRecordingsScreen(t, ctx, deps)
	cmd := s.Update(ctx, deps, keyRune('o'))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if len(opener.opened) != 1 || opener.opened[0] != recDir {
		t.Fatalf("opener.opened = %v, want [%s]", opener.opened, recDir)
	}
}

func TestRecordingsScreenDeleteConfirm(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	fakeDaemon := &fakeDaemonClient{listResult: daemon.RecordingListResult{
		Recordings: []daemon.RecordingInfo{
			{ID: "rec-1", PrinterID: "k2-5885", Parts: []daemon.RecordingPart{{Path: "/recordings/k2-5885/rec-1.mp4"}}},
		},
	}}
	deps := Deps{Daemon: fakeDaemon}.withDefaults()

	s := initRecordingsScreen(t, ctx, deps)
	if cmd := s.Update(ctx, deps, keyRune('d')); cmd != nil {
		t.Error("expected no command from opening the delete confirmation")
	}
	if s.mode != recordingsModeConfirmDelete {
		t.Fatalf("mode = %v, want recordingsModeConfirmDelete", s.mode)
	}
	if !containsCall(fakeDaemon.calls, "ListRecordings") {
		t.Fatal("expected the initial list call")
	}

	// A second ListRecordings call happens on reload after delete.
	fakeDaemon.listResult = daemon.RecordingListResult{}
	cmd := s.Update(ctx, deps, keyRune('y'))
	driveCmd(cmd, func(m tea.Msg) tea.Cmd { return s.Update(ctx, deps, m) })
	if !containsCall(fakeDaemon.calls, "DeleteRecording") {
		t.Errorf("calls = %v, want DeleteRecording", fakeDaemon.calls)
	}
	if len(s.recordings) != 0 {
		t.Errorf("len(recordings) = %d, want 0 after delete+reload", len(s.recordings))
	}
}

func TestRecordingsScreenNoDaemon(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{}.withDefaults()

	s := initRecordingsScreen(t, ctx, deps)
	if !strings.Contains(s.loadErr, "not available") {
		t.Errorf("loadErr = %q, want it to say the daemon is unavailable", s.loadErr)
	}
}
