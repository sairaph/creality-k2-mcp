package clicmd

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/daemon"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// This file implements the "camera" one-shot command's user-facing
// subcommands (dev_docs/plan-v0.1.0.md's "TUI and CLI" section, T13a):
// open, record, stop, recordings, delete. Every one of them calls through
// Deps.CameraViewer/Deps.CameraRecorder, the same internal/daemon/client.Client
// seams internal/mcpserver's open_camera_view and start_recording/
// stop_recording/list_recordings/delete_recording tools use (tools_viewer.go,
// tools_recording.go), so a CLI call and the equivalent MCP tool call always
// agree on what the daemon does. "camera serve" (the hidden daemon
// foreground command) is not handled here: main.go dispatches it straight to
// daemon.Open/Serve, since it never resolves a printer or talks through
// these seams at all.

const cameraUsage = `usage: camera open|record|stop|recordings|delete ...
  camera open [printer] [--no-browser]      print (and open) the local live view URL
  camera record [printer] [--mode video|timelapse] [--until stopped|print_end] [--max-duration DURATION]
                                             start a recording; prints its id and, for video, its file path
  camera stop [printer|recording-id]        stop the active recording
  camera recordings [printer]               list recordings: size, duration, parts/frames, disk usage
  camera delete <recording-id> [--yes]      delete a recording's files
`

// cameraDaemonUnavailable is printed whenever a subcommand's
// Deps.CameraViewer or Deps.CameraRecorder is nil: the background daemon
// client could not be built (production: only when this process's own
// executable path could not be resolved, matching main.go's
// newWatchdogClient/CameraViewer/CameraRecorder doc comments).
const cameraDaemonUnavailable = "this server's background daemon client is not wired up; " +
	"make sure this process can launch its own executable, then try again."

// RunCamera dispatches the "camera" one-shot command's user-facing
// subcommands. "serve" is deliberately not one of them (see this file's
// package comment above); main.go never routes it here.
func RunCamera(ctx context.Context, deps Deps, args []string) int {
	deps = deps.withDefaults()

	if len(args) == 0 {
		fmt.Fprint(deps.Stderr, cameraUsage)
		return 2
	}
	switch args[0] {
	case "open":
		return runCameraOpen(ctx, deps, args[1:])
	case "record":
		return runCameraRecord(ctx, deps, args[1:])
	case "stop":
		return runCameraStop(ctx, deps, args[1:])
	case "recordings":
		return runCameraRecordings(ctx, deps, args[1:])
	case "delete":
		return runCameraDelete(ctx, deps, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(deps.Stdout, cameraUsage)
		return 0
	default:
		fmt.Fprintf(deps.Stderr, "camera: unknown subcommand %q\n%s", args[0], cameraUsage)
		return 2
	}
}

// reorderArgsFlagsFirst moves every flag token in args (and, for a flag not
// listed in boolFlags, the value token right after it) to the front,
// preserving every other token as a trailing positional argument, both in
// their original relative order. flag.FlagSet itself stops parsing at the
// first non-flag token, so "camera record k2-5885 --until print_end" (a
// printer positional before a flag, exactly the order every subcommand's own
// usage string documents) would otherwise be misread as two positional
// arguments rather than one positional plus one flag; reordering first lets
// every camera subcommand accept its flags before or after its positional
// printer/id argument.
//
// A bare "--" is the standard flag-parsing terminator: everything after it
// is positional, even a token that would otherwise look like a flag (e.g. a
// recording id that happens to start with "-"). reorderArgsFlagsFirst moves
// every such trailing token to the end like any other positional argument,
// but re-emits one "--" immediately before them in its output so
// flag.FlagSet - which only recognizes "--" as a terminator, not any
// position in the token, and would otherwise try to parse a dash-prefixed
// positional as an unknown flag - still treats them as positional once it
// parses the reordered result (dev_docs/review-backlog.md item 41).
func reorderArgsFlagsFirst(args []string, boolFlags map[string]bool) []string {
	var flags, positional []string
	sawTerminator := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			sawTerminator = true
			positional = append(positional, args[i+1:]...)
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			continue // "--flag=value" already carries its own value
		}
		if boolFlags[name] {
			continue // a boolean flag takes no separate value token
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	if sawTerminator {
		return append(append(flags, "--"), positional...)
	}
	return append(flags, positional...)
}

// isHelpFlag reports whether s is a help request token, matching
// RunCamera's own top-level "-h"/"--help"/"help" handling. Used by the leaf
// subcommands below that take a single, non-flag positional argument
// (camera stop, camera recordings) rather than a flag.FlagSet that would
// otherwise recognize -h/--help itself via flag.ErrHelp.
func isHelpFlag(s string) bool {
	return s == "-h" || s == "--help" || s == "help"
}

// --- camera open ---

const cameraOpenUsage = "usage: camera open [printer] [--no-browser]\n"

// runCameraOpen resolves the viewer URL exactly as open_camera_view does
// (empty printer id shows every enabled printer; a given printer argument is
// resolved the same way status/snapshot resolve one), prints it, and opens
// it in the default browser via deps.Opener unless --no-browser was passed.
func runCameraOpen(ctx context.Context, deps Deps, args []string) int {
	fs := flag.NewFlagSet("camera open", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	noBrowser := fs.Bool("no-browser", false, "print the URL only; do not open a browser")
	if err := fs.Parse(reorderArgsFlagsFirst(args, map[string]bool{"no-browser": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(deps.Stdout, cameraOpenUsage)
			return 0
		}
		fmt.Fprint(deps.Stderr, cameraOpenUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprint(deps.Stderr, cameraOpenUsage)
		return 2
	}

	var printerID string
	if fs.NArg() == 1 {
		p, err := resolvePrinter(deps, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(deps.Stderr, "camera open:", err)
			return 1
		}
		printerID = p.ID
	}

	if deps.CameraViewer == nil {
		fmt.Fprintln(deps.Stderr, "camera open:", cameraDaemonUnavailable)
		return 1
	}
	url, err := deps.CameraViewer.ViewerURL(ctx, printerID)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "camera open: could not start the camera viewer:", err)
		return 1
	}

	fmt.Fprintln(deps.Stdout, url)
	if *noBrowser {
		return 0
	}
	if err := deps.Opener(url); err != nil {
		fmt.Fprintln(deps.Stderr, "camera open: could not open a browser:", err)
		fmt.Fprintln(deps.Stderr, "Open the URL above manually.")
		return 1
	}
	return 0
}

// defaultOpener launches the user's default browser on url, cross-platform:
// Windows uses rundll32's FileProtocolHandler (no shell involved, so a URL's
// own special characters are never reinterpreted by a shell the way `cmd /c
// start` can); macOS uses `open`; everything else (Linux and other Unix)
// uses `xdg-open`. It only starts the process; it does not wait for the
// browser to exit (a browser window staying open is normal, not a hang).
func defaultOpener(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// --- camera record ---

const cameraRecordUsage = "usage: camera record [printer] [--mode video|timelapse] " +
	"[--until stopped|print_end] [--max-duration DURATION]\n"

// runCameraRecord resolves a printer exactly as status/snapshot do, verifies
// its identity exactly as start_recording does (dev_docs/review-backlog.md
// item 31: a recording is keyed to a verified printer identity, never the
// raw configured host), and starts a recording through Deps.CameraRecorder.
// mode: "video" (the default) writes a fragmented MP4; mode: "timelapse"
// (T11d) writes one JPEG still per printer layer change instead, with no
// single output file to report, so the printed second line differs by mode
// (see the mode switch near the end of this function).
func runCameraRecord(ctx context.Context, deps Deps, args []string) int {
	fs := flag.NewFlagSet("camera record", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	mode := fs.String("mode", string(daemon.RecordModeVideo), "video (default) or timelapse")
	// until has no flag default of its own (unlike mode/max-duration): when
	// not passed, its zero value "" is sent through unchanged so the daemon
	// applies its own mode-aware default (stopped for video, print_end for
	// timelapse, daemon/recorder.go's Start), matching start_recording's own
	// choice (tools_recording.go) rather than this command hardcoding
	// "stopped" for every mode.
	until := fs.String("until", "", "stopped or print_end; defaults to stopped for video, print_end for timelapse")
	maxDuration := fs.Duration("max-duration", 0, "maximum recording length (default 12h for video, 48h for timelapse)")
	if err := fs.Parse(reorderArgsFlagsFirst(args, nil)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(deps.Stdout, cameraRecordUsage)
			return 0
		}
		fmt.Fprint(deps.Stderr, cameraRecordUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprint(deps.Stderr, cameraRecordUsage)
		return 2
	}
	modeValue := strings.ToLower(strings.TrimSpace(*mode))
	if modeValue != string(daemon.RecordModeVideo) && modeValue != string(daemon.RecordModeTimelapse) {
		fmt.Fprintf(deps.Stderr, "camera record: unknown --mode %q; want \"video\" or \"timelapse\"\n", *mode)
		return 2
	}
	untilValue := strings.ToLower(strings.TrimSpace(*until))
	if untilValue != "" && untilValue != string(daemon.RecordUntilStopped) && untilValue != string(daemon.RecordUntilPrintEnd) {
		fmt.Fprintf(deps.Stderr, "camera record: unknown --until %q; want \"stopped\" or \"print_end\"\n", *until)
		return 2
	}
	if *maxDuration < 0 {
		fmt.Fprintln(deps.Stderr, "camera record: --max-duration must not be negative")
		return 2
	}

	query := ""
	if fs.NArg() == 1 {
		query = fs.Arg(0)
	}
	printer, err := resolvePrinter(deps, query)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "camera record:", err)
		return 1
	}

	if deps.CameraRecorder == nil {
		fmt.Fprintln(deps.Stderr, "camera record:", cameraDaemonUnavailable)
		return 1
	}

	pdeps := deps.PrinterClients(printer)
	snap := printerstate.Take(ctx, pdeps, printer)
	identity := printerstate.Identity(snap)
	if identity == printerstate.UnverifiedIdentity {
		fmt.Fprintf(deps.Stderr,
			"camera record: %s's identity could not be verified (its printer/info read failed or returned no hostname).\n"+
				"Make sure the printer is powered on and reachable, then try again.\n", printer.Name)
		return 1
	}

	params := daemon.RecordingStartParams{
		PrinterID:          printer.ID,
		Host:               printer.Host,
		Identity:           identity,
		Mode:               modeValue,
		Until:              untilValue,
		MaxDurationSeconds: int(maxDuration.Seconds()),
	}
	info, err := deps.CameraRecorder.StartRecording(ctx, params)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "camera record: could not start the recording:", err)
		return 1
	}

	fmt.Fprintln(deps.Stdout, info.ID)
	switch info.Mode {
	case daemon.RecordModeTimelapse:
		// No single output file exists yet (or ever): a timelapse writes one
		// JPEG still per layer change under its own frames directory as the
		// print progresses, so there is no path to report at start time.
		fmt.Fprintln(deps.Stdout, "(timelapse: JPEG stills will be written as layers change; "+
			"run `camera recordings` to check on it)")
	default:
		path := ""
		if len(info.Parts) > 0 {
			path = info.Parts[0].Path
		}
		fmt.Fprintln(deps.Stdout, path)
	}
	return 0
}

// --- camera stop ---

const cameraStopUsage = "usage: camera stop [printer|recording-id]\n"

// runCameraStop resolves query to a recording id and stops it: query is
// tried first as a printer id or name (the printer's currently active
// recording is looked up via ListRecordings), and only if that fails is it
// treated as a recording id directly, so `camera stop k2-5885` and
// `camera stop k2-5885/20260928T150405Z` both work.
func runCameraStop(ctx context.Context, deps Deps, args []string) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		fmt.Fprint(deps.Stdout, cameraStopUsage)
		return 0
	}
	if len(args) > 1 {
		fmt.Fprint(deps.Stderr, cameraStopUsage)
		return 2
	}
	query := ""
	if len(args) == 1 {
		query = args[0]
	}

	if deps.CameraRecorder == nil {
		fmt.Fprintln(deps.Stderr, "camera stop:", cameraDaemonUnavailable)
		return 1
	}

	id, err := resolveActiveRecordingID(ctx, deps, query)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "camera stop:", err)
		return 1
	}

	info, err := deps.CameraRecorder.StopRecording(ctx, id)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "camera stop: could not stop the recording:", err)
		return 1
	}
	fmt.Fprintf(deps.Stdout, "Stopped recording %s (%s). Duration %.0fs, %d bytes across %d part(s).\n",
		info.ID, info.StopReason, info.DurationSeconds, info.Bytes, len(info.Parts))
	return 0
}

// resolveActiveRecordingID resolves query (a printer id/name, or empty for
// the one enabled printer) to that printer's currently active recording id.
// If query does not name a registered, enabled printer at all, it is
// returned unchanged as a recording id, so a raw id (which is never also a
// printer id or name) still reaches StopRecording, whose own "not found"
// error is the final word on whether it was valid.
func resolveActiveRecordingID(ctx context.Context, deps Deps, query string) (string, error) {
	reg, regErr := loadRegistry(deps)
	if regErr != nil {
		if query == "" {
			return "", fmt.Errorf("load the printer registry: %w", regErr)
		}
		return query, nil
	}

	p, perr := domain.ResolvePrinter(reg.Printers, query)
	if perr != nil {
		if query == "" {
			return "", fmt.Errorf("%w\n%s", perr, printerListHint(reg))
		}
		return query, nil
	}

	res, err := deps.CameraRecorder.ListRecordings(ctx)
	if err != nil {
		return "", fmt.Errorf("list recordings for %s: %w", p.Name, err)
	}
	for _, r := range res.Recordings {
		if r.PrinterID == p.ID && r.Active {
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("%s has no active recording; run `camera recordings` to check", p.Name)
}

// --- camera recordings ---

const cameraRecordingsUsage = "usage: camera recordings [printer]\n"

func runCameraRecordings(ctx context.Context, deps Deps, args []string) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		fmt.Fprint(deps.Stdout, cameraRecordingsUsage)
		return 0
	}
	if len(args) > 1 {
		fmt.Fprint(deps.Stderr, cameraRecordingsUsage)
		return 2
	}

	if deps.CameraRecorder == nil {
		fmt.Fprintln(deps.Stderr, "camera recordings:", cameraDaemonUnavailable)
		return 1
	}

	var filterID string
	if len(args) == 1 {
		p, err := resolvePrinter(deps, args[0])
		if err != nil {
			fmt.Fprintln(deps.Stderr, "camera recordings:", err)
			return 1
		}
		filterID = p.ID
	}

	res, err := deps.CameraRecorder.ListRecordings(ctx)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "camera recordings: could not list recordings:", err)
		return 1
	}

	rows := res.Recordings
	if filterID != "" {
		filtered := rows[:0:0]
		for _, r := range rows {
			if r.PrinterID == filterID {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}

	if len(rows) == 0 {
		fmt.Fprintln(deps.Stdout, "(no recordings)")
	} else {
		tw := tabwriter.NewWriter(deps.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPRINTER\tMODE\tACTIVE\tSTARTED\tDURATION_S\tBYTES\tPARTS\tFRAMES\tSTOP_REASON")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%.0f\t%d\t%d\t%d\t%s\n",
				r.ID, r.PrinterID, r.Mode, yesNo(r.Active), r.StartedAt.UTC().Format(time.RFC3339),
				r.DurationSeconds, r.Bytes, len(r.Parts), len(r.Frames), r.StopReason)
		}
		tw.Flush()
	}
	fmt.Fprintf(deps.Stdout, "\n%d recording(s), %d bytes total on disk in the recordings directory.\n",
		len(rows), res.DiskUsageBytes)
	return 0
}

// --- camera delete ---

const cameraDeleteUsage = "usage: camera delete <recording-id> [--yes]\n"

// runCameraDelete deletes a recording's files, prompting for confirmation
// through deps.Confirm unless --yes was passed. A non-interactive session
// (deps.IsInteractive false) without --yes has no way to confirm, so it is
// refused outright with a usage error (exit 2) rather than silently doing
// nothing or silently deleting.
func runCameraDelete(ctx context.Context, deps Deps, args []string) int {
	fs := flag.NewFlagSet("camera delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	yes := fs.Bool("yes", false, "delete without an interactive confirmation prompt")
	if err := fs.Parse(reorderArgsFlagsFirst(args, map[string]bool{"yes": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(deps.Stdout, cameraDeleteUsage)
			return 0
		}
		fmt.Fprint(deps.Stderr, cameraDeleteUsage)
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(deps.Stderr, cameraDeleteUsage)
		return 2
	}
	id := fs.Arg(0)

	if !*yes {
		if !deps.IsInteractive() {
			fmt.Fprintln(deps.Stderr, "camera delete: refusing to delete without --yes in a non-interactive session")
			fmt.Fprint(deps.Stderr, cameraDeleteUsage)
			return 2
		}
		confirmed, err := deps.Confirm(fmt.Sprintf("Delete recording %s? This cannot be undone. [y/N]: ", id))
		if err != nil {
			fmt.Fprintln(deps.Stderr, "camera delete:", err)
			return 1
		}
		if !confirmed {
			fmt.Fprintln(deps.Stdout, "Not deleted.")
			return 0
		}
	}

	if deps.CameraRecorder == nil {
		fmt.Fprintln(deps.Stderr, "camera delete:", cameraDaemonUnavailable)
		return 1
	}
	if err := deps.CameraRecorder.DeleteRecording(ctx, id); err != nil {
		fmt.Fprintln(deps.Stderr, "camera delete: could not delete the recording:", err)
		return 1
	}
	fmt.Fprintf(deps.Stdout, "Deleted recording %s.\n", id)
	return 0
}

// promptConfirm is Deps.Confirm's production default: it prints prompt to
// os.Stdout and reads one line from os.Stdin. A test must always supply its
// own Deps.Confirm instead (AGENTS.md hard testing rule: a test process must
// never block waiting on real stdin).
func promptConfirm(prompt string) (bool, error) {
	fmt.Fprint(os.Stdout, prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false, scanner.Err()
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes", nil
}
