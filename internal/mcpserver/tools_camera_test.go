package mcpserver

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// fakeCameraSnapshotter is a CameraSnapshotter (deps.go) that returns a
// fixed result or error instead of opening a real WebRTC session, so these
// tests never contact a real printer or open a non-loopback socket
// (AGENTS.md hard testing rule).
type fakeCameraSnapshotter struct {
	result *camera.SnapshotResult
	err    error
	called *bool
}

func (f fakeCameraSnapshotter) Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error) {
	if f.called != nil {
		*f.called = true
	}
	return f.result, f.err
}

// slowCameraSnapshotter is a CameraSnapshotter that never returns on its
// own: it blocks until ctx is done and then reports ctx's own error. It
// stands in for a camera capture that hangs, so a test can verify
// get_camera_snapshot's overall budget (snapshotToolBudget) actually bounds
// the capture instead of letting the tool call hang indefinitely.
type slowCameraSnapshotter struct{}

func (slowCameraSnapshotter) Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// delayedCameraSnapshotter is a CameraSnapshotter that sleeps for a fixed
// delay before returning result, used only by
// TestGetCameraSnapshotStateAndCaptureRunConcurrently to make the capture
// itself take a measurable, controlled amount of time.
type delayedCameraSnapshotter struct {
	delay  time.Duration
	result *camera.SnapshotResult
}

func (d delayedCameraSnapshotter) Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.result, nil
}

// solidImage returns a small, uniformly colored image, which JPEG compresses
// to a tiny size regardless of quality, for tests that need a known image
// well under any size cap.
func solidImage(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// randomNoiseImage returns a w x h image filled with deterministic
// pseudo-random bytes (fixed seed), which JPEG cannot compress away, for
// tests that need to force fitJPEG's downscale path (a small image, or one
// with large flat areas, would fit under the cap on quality reduction
// alone).
func randomNoiseImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rand.New(rand.NewSource(7)).Read(img.Pix)
	return img
}

// imageData returns the first ImageContent's raw bytes from res, failing the
// test if none is present.
func imageData(t *testing.T, res *mcp.CallToolResult) []byte {
	t.Helper()
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			return ic.Data
		}
	}
	t.Fatal("get_camera_snapshot result has no image content")
	return nil
}

// --- happy path ---

func TestGetCameraSnapshotHappyPath(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraSnapshot = fakeCameraSnapshotter{result: &camera.SnapshotResult{
		Image:      solidImage(640, 480, color.RGBA{R: 20, G: 20, B: 20, A: 255}),
		CapturedAt: time.Now(),
		Width:      640,
		Height:     480,
	}}
	cs := testSession(t, deps)
	res := call(t, cs, "get_camera_snapshot", nil)
	if res.IsError {
		t.Fatalf("get_camera_snapshot failed: %v", texts(res))
	}
	if n := images(res); n != 1 {
		t.Fatalf("get_camera_snapshot returned %d image content items, want 1", n)
	}
	data := imageData(t, res)
	if len(data) == 0 {
		t.Fatal("get_camera_snapshot returned empty image data")
	}
	if len(data) > maxImageBytes {
		t.Fatalf("image is %d bytes, over the %d byte cap", len(data), maxImageBytes)
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{
		"captured_at:", "width: 640", "height: 480", "jpeg_bytes:",
		"activity_state: idle", "printer_id: k2",
		"onboard chamber camera", "no infrared", "no authentication",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("get_camera_snapshot reply missing %q:\n%s", want, text)
		}
	}
	// The fixture's idle capture has output_pin LED value 0.0 (light off).
	if !strings.Contains(text, "light_on: false") || !strings.Contains(text, "chamber light is currently off") {
		t.Fatalf("get_camera_snapshot reply missing off-light guidance:\n%s", text)
	}
}

func TestGetCameraSnapshotNotFoundPrinter(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraSnapshot = fakeCameraSnapshotter{result: &camera.SnapshotResult{
		Image: solidImage(64, 64, color.RGBA{A: 255}), CapturedAt: time.Now(), Width: 64, Height: 64,
	}}
	cs := testSession(t, deps)
	res := call(t, cs, "get_camera_snapshot", map[string]any{"printer": "does-not-exist"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("get_camera_snapshot not_found reply = %s", text)
	}
}

// --- body guidance depends on whether control tools are enabled ---

func TestGetCameraSnapshotSuggestsSetLightWhenControlEnabled(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetControl, nil)
	deps.CameraSnapshot = fakeCameraSnapshotter{result: &camera.SnapshotResult{
		Image: solidImage(64, 64, color.RGBA{A: 255}), CapturedAt: time.Now(), Width: 64, Height: 64,
	}}
	cs := testSession(t, deps)
	res := call(t, cs, "get_camera_snapshot", nil)
	if res.IsError {
		t.Fatalf("get_camera_snapshot failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "Call set_light with on: true") {
		t.Fatalf("get_camera_snapshot with control enabled should suggest set_light:\n%s", text)
	}
	if strings.Contains(text, "ask the user") {
		t.Fatalf("get_camera_snapshot with control enabled should not ask the user:\n%s", text)
	}
}

func TestGetCameraSnapshotAsksUserWhenControlDisabled(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraSnapshot = fakeCameraSnapshotter{result: &camera.SnapshotResult{
		Image: solidImage(64, 64, color.RGBA{A: 255}), CapturedAt: time.Now(), Width: 64, Height: 64,
	}}
	cs := testSession(t, deps)
	res := call(t, cs, "get_camera_snapshot", nil)
	if res.IsError {
		t.Fatalf("get_camera_snapshot failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "ask the user to turn the chamber light on") {
		t.Fatalf("get_camera_snapshot with control disabled should ask the user:\n%s", text)
	}
	if strings.Contains(text, "Call set_light") {
		t.Fatalf("get_camera_snapshot with control disabled should not suggest set_light:\n%s", text)
	}
}

// --- image size cap enforcement (downscale path) ---

func TestGetCameraSnapshotEnforcesImageCapWithDownscale(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	// 3000x3000 of incompressible random noise, requested at its own native
	// width via max_width so the initial max-width clamp does not by itself
	// explain any size reduction: only the quality/downscale fitting loop
	// can bring this under maxImageBytes.
	big := randomNoiseImage(3000, 3000)
	deps.CameraSnapshot = fakeCameraSnapshotter{result: &camera.SnapshotResult{
		Image: big, CapturedAt: time.Now(), Width: 3000, Height: 3000,
	}}
	cs := testSession(t, deps)
	res := call(t, cs, "get_camera_snapshot", map[string]any{"max_width": 3000})
	if res.IsError {
		t.Fatalf("get_camera_snapshot failed: %v", texts(res))
	}
	data := imageData(t, res)
	if len(data) > maxImageBytes {
		t.Fatalf("image is %d bytes, over the %d byte cap", len(data), maxImageBytes)
	}
	text := strings.Join(texts(res), "\n")
	if strings.Contains(text, "width: 3000") {
		t.Fatalf("get_camera_snapshot did not downscale a 3000px-wide incompressible image under the size cap:\n%s", text)
	}
}

// --- fitJPEG quality and aspect ratio behavior ---

// TestGetCameraSnapshotFitJPEGNeverDropsQualityBelowMinBeforeMinWidth uses
// snapshotFitObserve (tools_camera.go) to record every width and quality
// fitJPEG tries while fitting a large, incompressible (noisy) image, and
// asserts that jpegQualityMin (70) is never undercut while the image is
// still wider than minSnapshotWidthPx: dropping quality further only starts
// once downscaling is no longer available.
func TestGetCameraSnapshotFitJPEGNeverDropsQualityBelowMinBeforeMinWidth(t *testing.T) {
	prevObserve := snapshotFitObserve
	defer func() { snapshotFitObserve = prevObserve }()

	type attempt struct{ width, quality int }
	var attempts []attempt
	snapshotFitObserve = func(width, quality int) {
		attempts = append(attempts, attempt{width, quality})
	}

	img := randomNoiseImage(2000, 1500)
	data, _, err := fitJPEG(img, 0, maxImageBytes)
	if err != nil {
		t.Fatalf("fitJPEG failed: %v", err)
	}
	if len(data) > maxImageBytes {
		t.Fatalf("fitJPEG returned %d bytes, over the %d byte cap", len(data), maxImageBytes)
	}
	if len(attempts) == 0 {
		t.Fatal("fitJPEG made no observed encode attempts")
	}
	for _, a := range attempts {
		if a.quality < jpegQualityMin && a.width > minSnapshotWidthPx {
			t.Fatalf("fitJPEG tried quality %d at width %d, below the %d floor before reaching the %d minimum width",
				a.quality, a.width, jpegQualityMin, minSnapshotWidthPx)
		}
	}
}

// TestGetCameraSnapshotFitJPEGDownscalePreservesAspectRatio checks that
// downscaleImage (tools_camera.go), fitJPEG's resize step, keeps the source
// image's aspect ratio when it scales width down to a target.
func TestGetCameraSnapshotFitJPEGDownscalePreservesAspectRatio(t *testing.T) {
	img := solidImage(1000, 400, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	out := downscaleImage(img, 250)
	bounds := out.Bounds()
	if bounds.Dx() != 250 {
		t.Fatalf("downscaleImage width = %d, want 250", bounds.Dx())
	}
	wantHeight := 400 * 250 / 1000
	if bounds.Dy() != wantHeight {
		t.Fatalf("downscaleImage height = %d, want %d (source is 1000x400, aspect ratio not preserved)",
			bounds.Dy(), wantHeight)
	}
}

// --- overall call budget ---

// TestGetCameraSnapshotRespectsOverallBudget shrinks snapshotToolBudget for
// the duration of the test and points the camera capture at
// slowCameraSnapshotter, which blocks until its context is done. This
// proves get_camera_snapshot bounds the state snapshot and the camera
// capture together under one deadline: the call returns quickly (near the
// shrunk budget, not hanging) with an unavailable error, rather than
// blocking for the production 15 second budget or forever.
func TestGetCameraSnapshotRespectsOverallBudget(t *testing.T) {
	prevBudget := snapshotToolBudget
	snapshotToolBudget = 50 * time.Millisecond
	defer func() { snapshotToolBudget = prevBudget }()

	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraSnapshot = slowCameraSnapshotter{}
	cs := testSession(t, deps)

	start := time.Now()
	res := call(t, cs, "get_camera_snapshot", nil)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("get_camera_snapshot took %v with a stuck camera capture, want it bounded near the %v test budget",
			elapsed, snapshotToolBudget)
	}
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("get_camera_snapshot with a stuck camera capture reply = %s", text)
	}
	if images(res) != 0 {
		t.Fatalf("get_camera_snapshot with a stuck camera capture carries %d image(s), want 0", images(res))
	}
}

// --- unavailable paths ---

func TestGetCameraSnapshotUnavailableWhenCameraCaptureFails(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraSnapshot = fakeCameraSnapshotter{err: errors.New("no keyframe received within budget")}
	cs := testSession(t, deps)
	res := call(t, cs, "get_camera_snapshot", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("get_camera_snapshot camera-failure reply = %s", text)
	}
	if images(res) != 0 {
		t.Fatalf("get_camera_snapshot camera-failure reply carries %d image(s), want 0", images(res))
	}
}

// TestGetCameraSnapshotUnavailableWhenDaemonClientNotWired confirms review
// backlog item 51's "no direct fallback, one path": a nil Deps.CameraSnapshot
// (the daemon client could not be built) must report the camera as
// unavailable, never silently fall back to any other capture path.
func TestGetCameraSnapshotUnavailableWhenDaemonClientNotWired(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraSnapshot = nil
	cs := testSession(t, deps)
	res := call(t, cs, "get_camera_snapshot", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("get_camera_snapshot with no daemon client wired reply = %s", text)
	}
	if images(res) != 0 {
		t.Fatalf("get_camera_snapshot with no daemon client wired carries %d image(s), want 0", images(res))
	}
}

// TestGetCameraSnapshotUnavailableWhenPrinterOffline confirms review backlog
// item 51 review's concurrency fix (dev_docs/item51-review.md part 2a): the
// state check and the capture now run concurrently, so an offline printer's
// call must not block on a capture that will never be reported.
// slowCameraSnapshotter blocks until its context is cancelled, standing in
// for a real capture that would otherwise run for its own multi-second
// budget; the call must still return promptly (get_camera_snapshot cancels
// the capture's context once the offline state is known) with an
// unavailable result and no image content.
func TestGetCameraSnapshotUnavailableWhenPrinterOffline(t *testing.T) {
	moon := fakeMoonraker(t, nil)
	wsHost, wsPort := fake9999(t)
	printer := testPrinter("k2", moon, wsHost, wsPort)
	moon.Close() // nothing listens at moon.URL any more: server/info fails, derived state offline

	deps := Deps{
		Settings: settingsWithPreset(domain.PresetCamera),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
		CameraSnapshot: slowCameraSnapshotter{},
	}
	cs := testSession(t, deps)

	start := time.Now()
	res := call(t, cs, "get_camera_snapshot", nil)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("get_camera_snapshot took %v for an offline printer with a stuck camera capture, "+
			"want it to return promptly without waiting for the capture", elapsed)
	}
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("get_camera_snapshot offline-printer reply = %s", text)
	}
	if images(res) != 0 {
		t.Fatalf("get_camera_snapshot offline-printer reply carries %d image(s), want 0", images(res))
	}
}

// TestGetCameraSnapshotStateAndCaptureRunConcurrently confirms the state
// check and the capture actually overlap rather than running one after the
// other (dev_docs/item51-review.md part 2a's proposed fix): both are given
// an artificial delay, and the call's total wall time must stay well under
// their sum, close to whichever delay is larger instead.
func TestGetCameraSnapshotStateAndCaptureRunConcurrently(t *testing.T) {
	const delay = 150 * time.Millisecond
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	deps.CameraSnapshot = delayedCameraSnapshotter{
		delay: delay,
		result: &camera.SnapshotResult{
			Image: solidImage(64, 64, color.RGBA{A: 255}), CapturedAt: time.Now(), Width: 64, Height: 64,
		},
	}
	cs := testSession(t, deps)

	start := time.Now()
	res := call(t, cs, "get_camera_snapshot", nil)
	elapsed := time.Since(start)

	if res.IsError {
		t.Fatalf("get_camera_snapshot failed: %v", texts(res))
	}
	// The printerstate.Take side of this call talks to fakeMoonraker/fake9999
	// over real (loopback) HTTP/WS round trips of its own, so this only needs
	// to rule out the two delays serializing (>= 2*delay), not pin the exact
	// elapsed time.
	if elapsed >= 2*delay {
		t.Errorf("get_camera_snapshot took %v, want well under %v (the state check and the capture must run "+
			"concurrently, not sequentially)", elapsed, 2*delay)
	}
}

// --- preset filtering ---

func TestGetCameraSnapshotPresetFiltering(t *testing.T) {
	for _, tc := range []struct {
		preset  domain.ToolPreset
		present bool
	}{
		{domain.PresetMonitor, false},
		{domain.PresetCamera, true},
		{domain.PresetControl, true},
	} {
		deps, _ := singlePrinterDeps(t, tc.preset, nil)
		cs := testSession(t, deps)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("preset %s: ListTools: %v", tc.preset, err)
		}
		present := false
		for _, tool := range res.Tools {
			if tool.Name == "get_camera_snapshot" {
				present = true
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
					t.Fatalf("preset %s: get_camera_snapshot missing readOnlyHint annotation", tc.preset)
				}
			}
		}
		if present != tc.present {
			t.Fatalf("preset %s: get_camera_snapshot present = %v, want %v", tc.preset, present, tc.present)
		}
	}
}
