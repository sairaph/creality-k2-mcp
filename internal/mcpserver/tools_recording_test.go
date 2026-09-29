package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// fakeCameraRecorder is a CameraRecorder (deps.go) driven entirely by
// canned results, so these tests never talk to a real background daemon or
// open a non-loopback socket (AGENTS.md hard testing rule). It also records
// the arguments it was last called with, so a test can confirm a tool
// resolved and passed through the right printer/identity/id before calling
// through.
type fakeCameraRecorder struct {
	startInfo daemon.RecordingInfo
	startErr  error
	stopInfo  daemon.RecordingInfo
	stopErr   error
	listRes   daemon.RecordingListResult
	listErr   error
	deleteErr error

	startCalled bool
	lastStart   daemon.RecordingStartParams
	lastStopID  string
	lastDelID   string
}

func (f *fakeCameraRecorder) StartRecording(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error) {
	f.startCalled = true
	f.lastStart = req
	if f.startErr != nil {
		return daemon.RecordingInfo{}, f.startErr
	}
	return f.startInfo, nil
}

func (f *fakeCameraRecorder) StopRecording(ctx context.Context, id string) (daemon.RecordingInfo, error) {
	f.lastStopID = id
	if f.stopErr != nil {
		return daemon.RecordingInfo{}, f.stopErr
	}
	return f.stopInfo, nil
}

func (f *fakeCameraRecorder) ListRecordings(ctx context.Context) (daemon.RecordingListResult, error) {
	if f.listErr != nil {
		return daemon.RecordingListResult{}, f.listErr
	}
	return f.listRes, nil
}

func (f *fakeCameraRecorder) DeleteRecording(ctx context.Context, id string) error {
	f.lastDelID = id
	return f.deleteErr
}

// --- start_recording ---

func TestStartRecording_Success(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	rec := &fakeCameraRecorder{startInfo: daemon.RecordingInfo{
		ID: "k2/20260101T000000Z", PrinterID: "k2", Mode: daemon.RecordModeVideo,
		Until: daemon.RecordUntilStopped, MaxDuration: 12 * time.Hour,
		StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Active: true,
	}}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	res := call(t, cs, "start_recording", map[string]any{"printer": "k2"})
	if res.IsError {
		t.Fatalf("start_recording failed: %v", texts(res))
	}
	if !rec.startCalled {
		t.Fatal("start_recording never called CameraRecorder.StartRecording")
	}
	if rec.lastStart.PrinterID != "k2" || rec.lastStart.Host == "" {
		t.Fatalf("StartRecording called with %+v, want printer_id k2 and a non-empty host", rec.lastStart)
	}
	if rec.lastStart.Identity != "k2.local" {
		t.Fatalf("StartRecording called with identity %q, want the verified hostname %q", rec.lastStart.Identity, "k2.local")
	}
	if rec.lastStart.Mode != "video" || rec.lastStart.Until != "stopped" {
		t.Fatalf("StartRecording called with mode=%q until=%q, want video/stopped defaults", rec.lastStart.Mode, rec.lastStart.Until)
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "id: k2/20260101T000000Z") {
		t.Fatalf("start_recording reply missing id frontmatter:\n%s", text)
	}
}

// mode: "timelapse" is accepted and passed through to the recorder, with
// until defaulting to "print_end" (decision 11, T11d), unlike video's own
// "stopped" default.
func TestStartRecording_TimelapseAcceptedDefaultsUntilPrintEnd(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	rec := &fakeCameraRecorder{startInfo: daemon.RecordingInfo{
		ID: "k2/20260101T000000Z-timelapse", PrinterID: "k2", Mode: daemon.RecordModeTimelapse,
		Until: daemon.RecordUntilPrintEnd, MaxDuration: 48 * time.Hour,
		StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Active: true,
	}}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	res := call(t, cs, "start_recording", map[string]any{"mode": "timelapse"})
	if res.IsError {
		t.Fatalf("start_recording mode=timelapse failed: %v", texts(res))
	}
	if !rec.startCalled {
		t.Fatal("start_recording never called CameraRecorder.StartRecording")
	}
	if rec.lastStart.Mode != "timelapse" {
		t.Fatalf("StartRecording called with mode=%q, want timelapse", rec.lastStart.Mode)
	}
	if rec.lastStart.Until != "print_end" {
		t.Fatalf("StartRecording called with until=%q, want the timelapse default print_end", rec.lastStart.Until)
	}
}

// An explicit until still overrides the mode-based default.
func TestStartRecording_TimelapseExplicitUntilStopped(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	rec := &fakeCameraRecorder{startInfo: daemon.RecordingInfo{
		ID: "k2/x-timelapse", PrinterID: "k2", Mode: daemon.RecordModeTimelapse, Until: daemon.RecordUntilStopped, Active: true,
	}}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	res := call(t, cs, "start_recording", map[string]any{"mode": "timelapse", "until": "stopped"})
	if res.IsError {
		t.Fatalf("start_recording failed: %v", texts(res))
	}
	if rec.lastStart.Until != "stopped" {
		t.Fatalf("StartRecording called with until=%q, want the explicit stopped", rec.lastStart.Until)
	}
}

func TestStartRecording_UnknownModeRejected(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	res := call(t, cs, "start_recording", map[string]any{"mode": "photo"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("start_recording mode=photo reply = %s", text)
	}
	if rec.startCalled {
		t.Fatal("start_recording called through to the recorder for an unknown mode")
	}
}

func TestStartRecording_UnverifiedIdentityRefused(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, map[string]http.HandlerFunc{
		"/printer/info": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	res := call(t, cs, "start_recording", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError {
		t.Fatalf("start_recording with an unverifiable printer identity succeeded, want an error:\n%s", text)
	}
	if !strings.Contains(text, "could not be verified") {
		t.Fatalf("start_recording reply missing identity-verification message:\n%s", text)
	}
	if rec.startCalled {
		t.Fatal("start_recording called through to the recorder with an unverified identity")
	}
}

func TestStartRecording_DaemonConflictMapsToConflict(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraRecorder = &fakeCameraRecorder{
		startErr: errors.New(`daemon: a recording (k2/x) is already active for printer "k2"`),
	}
	cs := testSession(t, deps)

	res := call(t, cs, "start_recording", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: conflict") {
		t.Fatalf("start_recording reply for an already-active recording = %s", text)
	}
}

func TestStartRecording_NilRecorderUnavailable(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	// deps.CameraRecorder left nil deliberately.
	cs := testSession(t, deps)

	res := call(t, cs, "start_recording", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("start_recording with no recorder wired up = %s", text)
	}
}

// --- stop_recording ---

func TestStopRecording_Success(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	rec := &fakeCameraRecorder{stopInfo: daemon.RecordingInfo{
		ID: "k2/x", PrinterID: "k2", Active: false, StopReason: "stopped by request",
		DurationSeconds: 12.5, Bytes: 4096, Parts: []daemon.RecordingPart{{Path: "a.mp4"}},
	}}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	res := call(t, cs, "stop_recording", map[string]any{"id": "k2/x"})
	if res.IsError {
		t.Fatalf("stop_recording failed: %v", texts(res))
	}
	if rec.lastStopID != "k2/x" {
		t.Fatalf("StopRecording called with id %q, want k2/x", rec.lastStopID)
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "stop_reason: stopped by request") {
		t.Fatalf("stop_recording reply missing stop_reason frontmatter:\n%s", text)
	}
}

func TestStopRecording_EmptyIDInvalidInput(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraRecorder = &fakeCameraRecorder{}
	cs := testSession(t, deps)

	res := call(t, cs, "stop_recording", map[string]any{"id": ""})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("stop_recording with empty id = %s", text)
	}
}

func TestStopRecording_NotFound(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraRecorder = &fakeCameraRecorder{stopErr: errors.New(`daemon: no active recording with id "k2/x"`)}
	cs := testSession(t, deps)

	res := call(t, cs, "stop_recording", map[string]any{"id": "k2/x"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("stop_recording for an unknown id = %s", text)
	}
}

// --- list_recordings ---

func TestListRecordings_Success(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraRecorder = &fakeCameraRecorder{listRes: daemon.RecordingListResult{
		DiskUsageBytes: 123456,
		Recordings: []daemon.RecordingInfo{
			{ID: "k2/a", PrinterID: "k2", Active: true, Mode: daemon.RecordModeVideo},
			{ID: "k2/b", PrinterID: "k2", Active: false, StopReason: "stopped by request", Mode: daemon.RecordModeVideo},
		},
	}}
	cs := testSession(t, deps)

	res := call(t, cs, "list_recordings", nil)
	if res.IsError {
		t.Fatalf("list_recordings failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "disk_usage_bytes: 123456") {
		t.Fatalf("list_recordings reply missing disk_usage_bytes:\n%s", text)
	}
	if !strings.Contains(text, "active_count: 1") {
		t.Fatalf("list_recordings reply missing active_count:\n%s", text)
	}
	if !strings.Contains(text, "k2/a") || !strings.Contains(text, "k2/b") {
		t.Fatalf("list_recordings reply missing both recordings:\n%s", text)
	}
}

// list_recordings shows a timelapse's frame count in its own column.
func TestListRecordings_ShowsFrameCounts(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	layer := 3
	deps.CameraRecorder = &fakeCameraRecorder{listRes: daemon.RecordingListResult{
		Recordings: []daemon.RecordingInfo{
			{ID: "k2/a-timelapse", PrinterID: "k2", Mode: daemon.RecordModeTimelapse, Frames: []daemon.TimelapseFrame{
				{Path: "frame_00001.jpg", Layer: &layer}, {Path: "frame_00002.jpg", Layer: &layer},
			}},
		},
	}}
	cs := testSession(t, deps)

	res := call(t, cs, "list_recordings", nil)
	if res.IsError {
		t.Fatalf("list_recordings failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "| k2/a-timelapse | k2 | timelapse |") {
		t.Fatalf("list_recordings reply missing the timelapse row:\n%s", text)
	}
	// The frames column (2 frames) must appear somewhere in that row.
	found := false
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "k2/a-timelapse") && strings.Contains(line, "| 2 |") {
			found = true
		}
	}
	if !found {
		t.Fatalf("list_recordings reply missing a frame count of 2 for the timelapse recording:\n%s", text)
	}
}

func TestListRecordings_FiltersByPrinter(t *testing.T) {
	moon := fakeMoonraker(t, nil)
	wsHost, wsPort := fake9999(t)
	p1 := testPrinter("k2", moon, wsHost, wsPort)
	p2 := p1
	p2.ID, p2.Name, p2.Hostname = "k2b", "k2b", "k2b.local"
	deps := Deps{
		Settings: settingsWithPreset(domain.PresetCamera),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{p1, p2}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
		CameraRecorder: &fakeCameraRecorder{listRes: daemon.RecordingListResult{
			Recordings: []daemon.RecordingInfo{
				{ID: "k2/a", PrinterID: "k2"},
				{ID: "k2b/a", PrinterID: "k2b"},
			},
		}},
	}
	cs := testSession(t, deps)

	res := call(t, cs, "list_recordings", map[string]any{"printer": "k2b"})
	if res.IsError {
		t.Fatalf("list_recordings failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if strings.Contains(text, "k2/a") {
		t.Fatalf("list_recordings printer filter leaked another printer's recording:\n%s", text)
	}
	if !strings.Contains(text, "k2b/a") {
		t.Fatalf("list_recordings printer filter dropped the matching recording:\n%s", text)
	}
}

// --- delete_recording ---

func TestDeleteRecording_ProposeThenConfirm(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	propose := call(t, cs, "delete_recording", map[string]any{"id": "k2/x"})
	if propose.IsError {
		t.Fatalf("delete_recording proposal failed: %v", texts(propose))
	}
	proposeText := strings.Join(texts(propose), "\n")
	if !strings.Contains(proposeText, "proposed: true") {
		t.Fatalf("delete_recording first call did not propose:\n%s", proposeText)
	}
	if rec.lastDelID != "" {
		t.Fatal("delete_recording called through to the recorder on the proposal call")
	}
	token := extractYAMLField(t, proposeText, "confirm_token")

	confirm := call(t, cs, "delete_recording", map[string]any{"id": "k2/x", "confirm_token": token})
	if confirm.IsError {
		t.Fatalf("delete_recording confirm failed: %v", texts(confirm))
	}
	if rec.lastDelID != "k2/x" {
		t.Fatalf("DeleteRecording called with id %q, want k2/x", rec.lastDelID)
	}
	confirmText := strings.Join(texts(confirm), "\n")
	if !strings.Contains(confirmText, "deleted: true") {
		t.Fatalf("delete_recording confirm reply missing deleted: true:\n%s", confirmText)
	}
}

func TestDeleteRecording_TokenIsSingleUse(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraRecorder = &fakeCameraRecorder{}
	cs := testSession(t, deps)

	propose := call(t, cs, "delete_recording", map[string]any{"id": "k2/y"})
	token := extractYAMLField(t, strings.Join(texts(propose), "\n"), "confirm_token")

	first := call(t, cs, "delete_recording", map[string]any{"id": "k2/y", "confirm_token": token})
	if first.IsError {
		t.Fatalf("first confirm failed: %v", texts(first))
	}
	second := call(t, cs, "delete_recording", map[string]any{"id": "k2/y", "confirm_token": token})
	text := strings.Join(texts(second), "\n")
	if !second.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("reusing a consumed confirm_token = %s", text)
	}
}

func TestDeleteRecording_WrongIDRejectsToken(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec
	cs := testSession(t, deps)

	propose := call(t, cs, "delete_recording", map[string]any{"id": "k2/z"})
	token := extractYAMLField(t, strings.Join(texts(propose), "\n"), "confirm_token")

	res := call(t, cs, "delete_recording", map[string]any{"id": "k2/other", "confirm_token": token})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("confirm_token issued for a different id = %s", text)
	}
	if rec.lastDelID != "" {
		t.Fatal("DeleteRecording called through with a token issued for a different id")
	}
}

func TestDeleteRecording_ActiveConflict(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraRecorder = &fakeCameraRecorder{
		deleteErr: errors.New(`daemon: recording "k2/x" is active; call recording.stop first`),
	}
	cs := testSession(t, deps)

	propose := call(t, cs, "delete_recording", map[string]any{"id": "k2/x"})
	token := extractYAMLField(t, strings.Join(texts(propose), "\n"), "confirm_token")

	res := call(t, cs, "delete_recording", map[string]any{"id": "k2/x", "confirm_token": token})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: conflict") {
		t.Fatalf("delete_recording on an active recording = %s", text)
	}
}
