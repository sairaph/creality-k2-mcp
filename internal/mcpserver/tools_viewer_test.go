package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// fakeCameraViewer is a CameraViewer (deps.go) that returns a fixed URL or
// error instead of talking to a real background daemon, so these tests
// never spawn a process or open a non-loopback socket (AGENTS.md hard
// testing rule); it also records the printerID it was last called with, so
// a test can confirm open_camera_view resolves the right printer before
// calling through.
type fakeCameraViewer struct {
	url           string
	err           error
	called        bool
	lastPrinterID string
}

func (f *fakeCameraViewer) ViewerURL(ctx context.Context, printerID string) (string, error) {
	f.called = true
	f.lastPrinterID = printerID
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}

func TestOpenCameraView_SinglePrinterDefault(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	viewer := &fakeCameraViewer{url: "http://127.0.0.1:54321/?token=abc123"}
	deps.CameraViewer = viewer
	cs := testSession(t, deps)

	res := call(t, cs, "open_camera_view", nil)
	if res.IsError {
		t.Fatalf("open_camera_view failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "url: http://127.0.0.1:54321/?token=abc123") {
		t.Fatalf("open_camera_view reply missing url frontmatter:\n%s", text)
	}
	if !strings.Contains(text, "Open http://127.0.0.1:54321/?token=abc123 in a browser") {
		t.Fatalf("open_camera_view body missing open-in-browser guidance:\n%s", text)
	}
	if !strings.Contains(text, "local to this computer only") {
		t.Fatalf("open_camera_view body missing local-only guidance:\n%s", text)
	}
	if !strings.Contains(text, "shared by every browser viewer") {
		t.Fatalf("open_camera_view body missing shared-connection guidance:\n%s", text)
	}
	if !viewer.called {
		t.Fatal("open_camera_view never called CameraViewer.ViewerURL")
	}
	// With only one enabled printer and no `printer` argument, the tool
	// still covers "every enabled printer" (printerID left empty), not an
	// implicit single-printer resolution the way most other tools default.
	if viewer.lastPrinterID != "" {
		t.Fatalf("ViewerURL called with printerID = %q, want empty (no printer argument given)", viewer.lastPrinterID)
	}
}

func TestOpenCameraView_ExplicitPrinterResolves(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	viewer := &fakeCameraViewer{url: "http://127.0.0.1:9/?token=t&printer=k2"}
	deps.CameraViewer = viewer
	cs := testSession(t, deps)

	res := call(t, cs, "open_camera_view", map[string]any{"printer": "k2"})
	if res.IsError {
		t.Fatalf("open_camera_view failed: %v", texts(res))
	}
	if viewer.lastPrinterID != "k2" {
		t.Fatalf("ViewerURL called with printerID = %q, want %q", viewer.lastPrinterID, "k2")
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "id: k2") {
		t.Fatalf("open_camera_view reply missing the resolved printer in its printers list:\n%s", text)
	}
}

func TestOpenCameraView_UnknownPrinterNotFound(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraViewer = &fakeCameraViewer{url: "http://127.0.0.1:9/"}
	cs := testSession(t, deps)

	res := call(t, cs, "open_camera_view", map[string]any{"printer": "does-not-exist"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("open_camera_view unknown-printer reply = %s", text)
	}
}

func TestOpenCameraView_NoEnabledPrinters(t *testing.T) {
	deps := Deps{
		Settings: settingsWithPreset(domain.PresetCamera),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1}, nil
		},
		CameraViewer: &fakeCameraViewer{url: "http://127.0.0.1:9/"},
	}
	cs := testSession(t, deps)

	res := call(t, cs, "open_camera_view", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") || !strings.Contains(text, "No printer is enabled") {
		t.Fatalf("open_camera_view no-enabled-printers reply = %s", text)
	}
}

func TestOpenCameraView_NilCameraViewerUnavailable(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	// deps.CameraViewer left nil deliberately.
	cs := testSession(t, deps)

	res := call(t, cs, "open_camera_view", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("open_camera_view with nil CameraViewer reply = %s", text)
	}
}

func TestOpenCameraView_ViewerURLErrorReportsUnavailable(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraViewer = &fakeCameraViewer{err: errors.New("daemon: could not autostart")}
	cs := testSession(t, deps)

	res := call(t, cs, "open_camera_view", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("open_camera_view with a ViewerURL error reply = %s", text)
	}
}

func TestOpenCameraView_PresetFiltering(t *testing.T) {
	for _, tc := range []struct {
		preset  domain.ToolPreset
		present bool
	}{
		{domain.PresetMonitor, false},
		{domain.PresetCamera, true},
		{domain.PresetControl, true},
	} {
		deps, _ := singlePrinterDeps(t, tc.preset, nil)
		deps.CameraViewer = &fakeCameraViewer{url: "http://127.0.0.1:9/"}
		cs := testSession(t, deps)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("preset %s: ListTools: %v", tc.preset, err)
		}
		present := false
		for _, tool := range res.Tools {
			if tool.Name == "open_camera_view" {
				present = true
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
					t.Fatalf("preset %s: open_camera_view missing readOnlyHint annotation", tc.preset)
				}
			}
		}
		if present != tc.present {
			t.Fatalf("preset %s: open_camera_view present = %v, want %v", tc.preset, present, tc.present)
		}
	}
}
