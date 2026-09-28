package mcpserver

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
	"golang.org/x/image/draw"
)

// This file implements the "Camera" tool group's snapshot tool
// (dev_docs/plan-v0.1.0.md's tool surface, decision 11, T9): get_camera_snapshot.
// It resolves a printer, then runs a light printerstate snapshot (purely for
// the result's frontmatter and body: this tool never calls printerstate.Take's
// full gather for gating purposes, since camera use is allowed in every state
// per dev_docs/safety-architecture.md 4.1's last row) concurrently with the
// capture itself through Deps.CameraSnapshot (production: the background
// daemon's hub, review backlog item 51 - a rolling GOP buffer decoded on
// demand, kept warm across repeated calls, autostarting the daemon like
// open_camera_view/start_recording already do; so tests never open a real
// WebRTC session or the real daemon), and returns it as image content per
// imageResult (result.go), re-encoding at lower quality and then
// downscaling until it fits maxImageBytes (decision 11). The state check and
// the capture run concurrently, not sequentially, so the whole call's
// latency is roughly max(state check, capture) rather than their sum
// (dev_docs/item51-review.md part 2a); the state result decides only
// whether the capture's result is used (an offline printer's capture is
// never awaited or reported), never whether the capture is allowed to
// start.

func registerCameraTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "get_camera_snapshot", Category: domain.ToolCategoryCamera}, &mcp.Tool{
		Name: "get_camera_snapshot",
		Description: "Captures one still frame from the printer's onboard chamber camera over WebRTC and " +
			"returns it as image content, alongside a short frontmatter (capture time, image size, and the " +
			"printer's current activity state, layer, progress and job file) and a body explaining what the " +
			"image shows. The camera has no infrared or night-vision sensor, so a mostly dark or black frame " +
			"most often just means the chamber light is off, not that anything is wrong; the body says whether " +
			"the light is currently reported on or off and, when it is off, whether calling set_light is an " +
			"option (only if control tools are enabled for this printer) or whether the user should be asked to " +
			"turn it on instead. The camera stream itself has no authentication on the LAN. An optional " +
			"max_width bounds the image's width in pixels before the server's own size cap is applied (default " +
			"1280); the image is always re-encoded at lower JPEG quality and, if still too large, downscaled " +
			"further until it fits. Camera use is read-only and works in every printer state except offline. " +
			"Related: get_printer_status and get_current_job for the same state without an image, " +
			"open_camera_view for a continuously updating local browser view.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getCameraSnapshotHandler(s))
}

// defaultSnapshotMaxWidth is get_camera_snapshot's max_width default: the
// camera's own native capture width (internal/camera.Snapshot's fake and
// real streams both produce 1280x720), so a caller that omits max_width
// gets the full native resolution before the size-fitting loop runs.
const defaultSnapshotMaxWidth = 1280

// JPEG re-encoding ladder for fitting a snapshot under maxImageBytes
// (decision 11: "re-encoded at lower quality, then downscaled, until it
// fits"). Quality is tried at jpegQualityStart and then jpegQualityMin;
// dropping quality below jpegQualityMin before downscaling would blur fine
// print detail that the snapshot is taken to let an AI judge, so once both
// of those are still over budget the image is downscaled instead (by
// downscaleFactor per step) and quality resets to jpegQualityStart. Only
// once the image is already at minSnapshotWidthPx, the smallest useful
// width, does quality drop further, down to the last-resort jpegQualityFloor.
// snapshotFitMaxAttempts bounds the total number of encode attempts so this
// always terminates.
const (
	jpegQualityStart       = 85
	jpegQualityMin         = 70
	jpegQualityFloor       = 40
	jpegQualityFloorStep   = 10
	downscaleFactor        = 0.75
	minSnapshotWidthPx     = 64
	snapshotFitMaxAttempts = 40
)

// snapshotToolBudget bounds the whole get_camera_snapshot call: the light
// state snapshot taken for the frontmatter and the camera capture run
// concurrently (see getCameraSnapshotHandler) and together must complete
// within this long, so a slow or stuck printer cannot hang the tool call
// indefinitely. 25s, not a shorter value: the capture itself goes through
// the daemon's Hub.Snapshot (review backlog item 51), which has its own 20s
// hubSnapshotBudget for a cold connection, and live testing against a real
// K2 observed cold-connect latency up to 16s, so this tool budget must
// leave that inner budget room to complete (dev_docs/t11e-soak-report.md,
// dev_docs/camera-keyframe-rca.md). It is a var, not a const, only so a
// test can shrink it temporarily instead of actually waiting 25 seconds.
var snapshotToolBudget = 25 * time.Second

// snapshotFitObserve, when non-nil, is called with the width and quality of
// every JPEG encode attempt fitJPEG makes, in order. Production code leaves
// it nil; it exists only so a test can observe the exact quality ladder
// fitJPEG walks (for example, that quality never drops below jpegQualityMin
// while width is still above minSnapshotWidthPx) without needing to reverse
// a JPEG's quality out of its encoded bytes.
var snapshotFitObserve func(width, quality int)

type cameraSnapshotInput struct {
	Printer  *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	MaxWidth *int    `json:"max_width,omitempty" jsonschema:"maximum image width in pixels before the server's own byte-size cap is applied; defaults to 1280"`
}

// cameraSnapshotFront is get_camera_snapshot's frontmatter: the shared
// StateBlock (dev_docs/safety-architecture.md section 5) plus the capture
// and job facts the task calls for explicitly (captured_at, width, height,
// jpeg_bytes, layer, progress, job file); light on/off and the derived
// state summary are already carried by the embedded StateBlock
// (light_on, activity_state).
type cameraSnapshotFront struct {
	printerstate.StateBlock `yaml:",inline"`
	CapturedAt              string   `yaml:"captured_at"`
	Width                   int      `yaml:"width"`
	Height                  int      `yaml:"height"`
	JPEGBytes               int      `yaml:"jpeg_bytes"`
	Layer                   int      `yaml:"layer,omitempty"`
	LayerCount              int      `yaml:"layer_count,omitempty"`
	ProgressPercent         *float64 `yaml:"progress_percent,omitempty"`
	JobFile                 string   `yaml:"job_file,omitempty"`
}

func (f *cameraSnapshotFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func getCameraSnapshotHandler(s *Server) func(context.Context, *mcp.CallToolRequest, cameraSnapshotInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in cameraSnapshotInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}

		// snapshotToolBudget bounds this whole call: the state snapshot
		// below and the camera capture, run concurrently (see this file's
		// package doc comment), share this one deadline rather than each
		// getting their own 15 seconds.
		budgetCtx, cancel := context.WithTimeout(ctx, snapshotToolBudget)
		defer cancel()

		// The state snapshot (for the frontmatter and body only: camera use
		// is allowed in every state but offline, safety-architecture.md 4.1)
		// and the capture are started together, not one after the other -
		// review backlog item 51 review, part 2a found the state check
		// running sequentially ahead of the capture behind most of this
		// tool's own observed latency. captureCtx is cancelled once the
		// state result is in, whether it turns out offline (the capture's
		// result will never be used) or the call returns for any other
		// reason before the capture is awaited.
		deps := s.deps.PrinterClients(printer)
		stateCh := make(chan printerstate.Snapshot, 1)
		go func() { stateCh <- printerstate.Take(budgetCtx, deps, printer) }()

		type captureOutcome struct {
			result *camera.SnapshotResult
			err    error
		}
		captureCtx, cancelCapture := context.WithCancel(budgetCtx)
		defer cancelCapture()
		captureCh := make(chan captureOutcome, 1)
		if s.deps.CameraSnapshot != nil {
			go func() {
				result, err := s.deps.CameraSnapshot.Snapshot(captureCtx, printer.Host)
				captureCh <- captureOutcome{result, err}
			}()
		}

		snap := <-stateCh
		derived := printerstate.DeriveActivityState(snap, nil)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		if derived.State == printerstate.StateOffline {
			cancelCapture()
			return render.ErrorResult(render.Error{
				Code:    render.CodeUnavailable,
				Message: fmt.Sprintf("%s is offline; its camera cannot be reached either.", block.PrinterName),
				Hint:    unavailableHint,
			}), nil, nil
		}

		if s.deps.CameraSnapshot == nil {
			return render.ErrorResult(render.Error{
				Code: render.CodeUnavailable,
				Message: "Camera snapshot capture is not available: this server's background daemon client is " +
					"not wired up.",
				Hint: "This usually means the server's own executable path could not be resolved at startup; " +
					"restarting the server should fix it.",
			}), nil, nil
		}
		outcome := <-captureCh
		if outcome.err != nil {
			if isNoVideoTimeout(outcome.err) {
				return render.ErrorResult(render.Error{
					Code:    render.CodeUnavailable,
					Message: fmt.Sprintf("Could not capture a camera snapshot from %s: %s.", block.PrinterName, noVideoMessage),
					Hint:    noVideoHint,
				}), nil, nil
			}
			return render.ErrorResult(render.Error{
				Code: render.CodeUnavailable,
				Message: fmt.Sprintf("Could not capture a camera snapshot from %s: %v",
					block.PrinterName, outcome.err),
				Hint: "Make sure the printer is powered on and its camera is reachable on the LAN (port 8000); " +
					"a busy or just-started camera stream can also miss a keyframe within the capture budget, " +
					"in which case trying again usually succeeds.",
			}), nil, nil
		}
		result := outcome.result

		maxWidth := defaultSnapshotMaxWidth
		if in.MaxWidth != nil && *in.MaxWidth > 0 {
			maxWidth = *in.MaxWidth
		}
		data, fitted, err := fitJPEG(result.Image, maxWidth, maxImageBytes)
		if err != nil {
			return render.ErrorResult(render.Error{
				Code:    render.CodeInternal,
				Message: fmt.Sprintf("Could not encode the camera snapshot: %v", err),
			}), nil, nil
		}
		fittedBounds := fitted.Bounds()

		front := &cameraSnapshotFront{
			StateBlock: block,
			CapturedAt: result.CapturedAt.UTC().Format(time.RFC3339),
			Width:      fittedBounds.Dx(),
			Height:     fittedBounds.Dy(),
			JPEGBytes:  len(data),
		}
		if snap.VirtualSDCard != nil {
			front.Layer = snap.VirtualSDCard.Layer
			front.LayerCount = snap.VirtualSDCard.LayerCount
			if snap.VirtualSDCard.Progress > 0 {
				pct := snap.VirtualSDCard.Progress * 100
				front.ProgressPercent = &pct
			}
		}
		if block.Job != nil {
			front.JobFile = block.Job.Filename
		}

		return imageResult(front, nil, cameraSnapshotBody(s, front), "image/jpeg", data), nil, nil
	}
}

// cameraSnapshotBody tells the AI what it is looking at: the printer's
// current activity, the fact that a dark frame most likely just means the
// light is off (this camera has no infrared), the exact next call for that
// depending on whether control tools are enabled, and that the camera
// stream carries no authentication on the LAN.
func cameraSnapshotBody(s *Server, front *cameraSnapshotFront) string {
	body := fmt.Sprintf("Live still from %s's onboard chamber camera, captured at %s (%dx%d, %d bytes as JPEG). "+
		"Printer state: %s.", front.PrinterName, front.CapturedAt, front.Width, front.Height, front.JPEGBytes,
		summarizeState(front.StateBlock))

	switch {
	case front.LightOn == nil:
		body += " The chamber light state could not be read. This camera has no infrared or night vision, so " +
			"a dark or black image most likely just means the light is off, not that anything is wrong."
	case !*front.LightOn:
		body += " The chamber light is currently off. This camera has no infrared or night vision, so a dark " +
			"or black image is expected and does not by itself indicate a problem."
		if controlToolsEnabled(s) {
			body += " Call set_light with on: true if a lit view is needed."
		} else {
			body += " Control tools are not enabled for this printer, so ask the user to turn the chamber " +
				"light on if a lit view is needed."
		}
	default:
		body += " The chamber light is currently on."
	}

	body += " This camera stream has no authentication on the local network: anyone on the same LAN can view it."
	return body
}

// controlToolsEnabled reports whether set_light (and therefore every other
// control tool) is currently enabled, the same computation registerTool
// itself uses, so the body's guidance about calling set_light matches what
// is actually registered.
func controlToolsEnabled(s *Server) bool {
	info := domain.ToolInfo{Name: "set_light", Category: domain.ToolCategoryControl}
	return s.deps.Settings.EnabledTools([]domain.ToolInfo{info})[info.Name]
}

// fitJPEG encodes img as JPEG, downscaling to at most maxWidth pixels wide
// first if it is wider, then fitting it under maxBytes. At each width it
// tries jpegQualityStart and jpegQualityMin only: dropping quality further
// would blur the print detail the snapshot exists to let an AI judge, so if
// both are still over budget the image is downscaled instead (by
// downscaleFactor) and quality resets to jpegQualityStart for the smaller
// image. Only once the image is already at minSnapshotWidthPx, the smallest
// useful width, does quality drop further, down to jpegQualityFloor, since
// downscaling is no longer an option there. The whole search is bounded by
// snapshotFitMaxAttempts encode attempts for deterministic termination. It
// returns the encoded bytes and the exact image (possibly downscaled) those
// bytes represent, so callers can report the true output dimensions.
func fitJPEG(img image.Image, maxWidth, maxBytes int) ([]byte, image.Image, error) {
	if maxWidth > 0 && img.Bounds().Dx() > maxWidth {
		img = downscaleImage(img, maxWidth)
	}

	attempts := 0
	var lastData []byte
	for {
		for _, quality := range [2]int{jpegQualityStart, jpegQualityMin} {
			attempts++
			if attempts > snapshotFitMaxAttempts {
				return nil, nil, fmt.Errorf("could not fit the snapshot under %d bytes after %d attempts", maxBytes, snapshotFitMaxAttempts)
			}
			if snapshotFitObserve != nil {
				snapshotFitObserve(img.Bounds().Dx(), quality)
			}
			data, err := encodeJPEG(img, quality)
			if err != nil {
				return nil, nil, fmt.Errorf("encode jpeg: %w", err)
			}
			lastData = data
			if len(data) <= maxBytes {
				return data, img, nil
			}
		}

		width := img.Bounds().Dx()
		if width > minSnapshotWidthPx {
			newWidth := int(float64(width) * downscaleFactor)
			if newWidth < minSnapshotWidthPx {
				newWidth = minSnapshotWidthPx
			}
			img = downscaleImage(img, newWidth)
			continue
		}

		// Already at the smallest useful width, so quality is the only
		// remaining lever: step it down below jpegQualityMin to the
		// last-resort jpegQualityFloor.
		for quality := jpegQualityMin - jpegQualityFloorStep; quality >= jpegQualityFloor; quality -= jpegQualityFloorStep {
			attempts++
			if attempts > snapshotFitMaxAttempts {
				// Best effort reached within the attempt budget: imageResult's
				// own size check (result.go) is the final safety net.
				return lastData, img, nil
			}
			if snapshotFitObserve != nil {
				snapshotFitObserve(img.Bounds().Dx(), quality)
			}
			data, err := encodeJPEG(img, quality)
			if err != nil {
				return nil, nil, fmt.Errorf("encode jpeg: %w", err)
			}
			lastData = data
			if len(data) <= maxBytes {
				return data, img, nil
			}
		}
		// Width and quality are both at their floors: return the best
		// effort reached. imageResult's own size check (result.go) is the
		// final safety net if this is still too big.
		return lastData, img, nil
	}
}

// encodeJPEG renders img as JPEG at the given quality (1-100).
func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// downscaleImage returns img scaled down so its width is targetWidth,
// preserving aspect ratio, via golang.org/x/image/draw's Catmull-Rom
// resampler. Catmull-Rom keeps far more of the fine print detail these
// snapshots are captured for than nearest-neighbor sampling would, at a
// resampling cost that is negligible next to the JPEG encode it precedes.
// It is a no-op (returns img unchanged) when targetWidth is not smaller
// than img's current width.
func downscaleImage(img image.Image, targetWidth int) image.Image {
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if targetWidth <= 0 || targetWidth >= srcW || srcW == 0 || srcH == 0 {
		return img
	}
	targetHeight := srcH * targetWidth / srcW
	if targetHeight < 1 {
		targetHeight = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Src, nil)
	return dst
}
