package clicmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// snapshotJPEGQuality is the quality "snapshot" writes at. No size cap
// applies here, unlike get_camera_snapshot's MCP result (which must fit an
// image content block, internal/mcpserver/tools_camera.go's fitJPEG), so the
// full-resolution capture is kept for local use.
const snapshotJPEGQuality = 92

const snapshotUsage = "usage: snapshot [printer] [--out file.jpg] [--force]\n"

// RunSnapshot is the "snapshot" one-shot command: it resolves a printer
// exactly as RunStatus (and the get_camera_snapshot MCP tool) does, then
// runs the same light offline check get_camera_snapshot runs
// (internal/mcpserver/tools_camera.go's getCameraSnapshotHandler)
// concurrently with the capture itself through deps.CameraSnapshot -
// production: the background daemon's hub (review backlog item 51: the
// rolling GOP buffer, kept warm across repeated calls, not a fresh
// independent WebRTC session per call), autostarted the same way "camera
// open" autostarts it - then saves the result as a JPEG file. There is no
// other, direct capture path: if the daemon client could not be wired up,
// this reports the camera as unavailable rather than falling back to
// anything else.
//
// The state check and the capture run concurrently, not sequentially: item
// 51's own live measurement found the state check running ahead of the
// capture, not the capture itself, behind most of this command's observed
// 1.75-2s latency (dev_docs/item51-review.md part 2a), so this command's
// total latency is now roughly max(state check, capture) instead of their
// sum. Camera use is allowed in every printer state (dev_docs/
// safety-architecture.md 4.1's "camera, recording" row: gating class "all"),
// so starting the capture before the state check resolves is never unsafe;
// the offline check below only decides which result to report and use, and
// cancels the capture's context so an offline printer's command does not
// wait for a capture attempt that was never going to be reported.
//
// Flags are accepted before or after the positional printer argument (e.g.
// both "snapshot --out file.jpg --force k2-5885" and "snapshot k2-5885
// --out file.jpg --force" work), via camera.go's shared
// reorderArgsFlagsFirst: a plain Go flag.FlagSet stops parsing at the first
// non-flag token, so without this a printer positional before any flag
// would make every following flag look like a second and third positional
// argument and fail with the usage message (dev_docs/review-backlog.md
// item 48, dev_docs/t11e-soak-report-2.md anomaly 1).
func RunSnapshot(ctx context.Context, deps Deps, args []string) int {
	deps = deps.withDefaults()

	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	out := fs.String("out", "", "output file path (default: a timestamped .jpg in the current directory)")
	force := fs.Bool("force", false, "overwrite an existing --out file instead of refusing")
	if err := fs.Parse(reorderArgsFlagsFirst(args, map[string]bool{"force": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(deps.Stdout, snapshotUsage)
			return 0
		}
		fmt.Fprint(deps.Stderr, snapshotUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprint(deps.Stderr, snapshotUsage)
		return 2
	}
	query := ""
	if fs.NArg() == 1 {
		query = fs.Arg(0)
	}

	printer, err := resolvePrinter(deps, query)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "snapshot:", err)
		return 1
	}

	// The state check and the capture are started together, not one after
	// the other; see RunSnapshot's own doc comment above for why this is
	// safe. captureCtx is cancelled once the state result is in, whether it
	// turns out offline (the capture's result will never be used) or the
	// command returns for any other reason before the capture is awaited.
	pdeps := deps.PrinterClients(printer)
	stateCh := make(chan printerstate.Snapshot, 1)
	go func() { stateCh <- printerstate.Take(ctx, pdeps, printer) }()

	type captureOutcome struct {
		result *camera.SnapshotResult
		err    error
	}
	captureCtx, cancelCapture := context.WithCancel(ctx)
	defer cancelCapture()
	captureCh := make(chan captureOutcome, 1)
	if deps.CameraSnapshot != nil {
		go func() {
			result, err := deps.CameraSnapshot(captureCtx, printer.Host)
			captureCh <- captureOutcome{result, err}
		}()
	}

	snap := <-stateCh
	derived := printerstate.DeriveActivityState(snap, nil)
	if derived.State == printerstate.StateOffline {
		cancelCapture()
		fmt.Fprintf(deps.Stderr, "snapshot: %s is offline; its camera cannot be reached either.\n", printer.Name)
		return 1
	}

	if deps.CameraSnapshot == nil {
		fmt.Fprintln(deps.Stderr, "snapshot:", cameraDaemonUnavailable)
		return 1
	}
	outcome := <-captureCh
	if outcome.err != nil {
		fmt.Fprintf(deps.Stderr, "snapshot: could not capture from %s: %v\n", printer.Name, outcome.err)
		return 1
	}

	path := *out
	if path == "" {
		path = fmt.Sprintf("%s-%s.jpg", printer.ID, deps.Now().Format("20060102-150405"))
	}

	if err := writeSnapshotJPEG(path, outcome.result.Image, *force); err != nil {
		fmt.Fprintln(deps.Stderr, "snapshot:", err)
		return 1
	}

	fmt.Fprintln(deps.Stdout, path)
	return 0
}

// writeSnapshotJPEG saves img to path as a JPEG.
//
// Without force, path must not already exist: it is opened with
// O_CREATE|O_EXCL so this command never silently overwrites a snapshot (or
// anything else) a caller left at that path.
//
// With force, img is encoded into a temporary file created in the same
// directory as path, then that temporary file is renamed over path. This
// keeps an encode failure, or a crash mid-write, from leaving a truncated
// file where a good one used to be, and means a reader never observes a
// partially written file at path.
func writeSnapshotJPEG(path string, img image.Image, force bool) error {
	if !force {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("%s already exists; pass --force to overwrite", path)
			}
			return err
		}
		return encodeJPEGAndClose(f, img)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-*.jpg")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := encodeJPEGAndClose(tmp, img); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := snapshotReplaceFile(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// encodeJPEGAndClose writes img to f as a JPEG and closes f, returning
// whichever of the encode or close errors occurred first; a close error
// after a clean encode still matters, since some filesystems only surface a
// write failure on close.
func encodeJPEGAndClose(f *os.File, img image.Image) error {
	encErr := jpeg.Encode(f, img, &jpeg.Options{Quality: snapshotJPEGQuality})
	closeErr := f.Close()
	if encErr != nil {
		return encErr
	}
	return closeErr
}
