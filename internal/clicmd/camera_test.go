package clicmd

import (
	"bytes"
	"context"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// fakeCameraViewer implements CameraViewer without ever starting the real
// background daemon (AGENTS.md hard testing rule).
type fakeCameraViewer struct {
	url          string
	err          error
	gotPrinterID *string // set to a fresh string on every call, for tests that check it
}

func (f *fakeCameraViewer) ViewerURL(ctx context.Context, printerID string) (string, error) {
	if f.gotPrinterID != nil {
		*f.gotPrinterID = printerID
	}
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}

// fakeCameraRecorder implements CameraRecorder without ever talking to a
// real background daemon.
type fakeCameraRecorder struct {
	startInfo daemon.RecordingInfo
	startErr  error
	stopInfo  daemon.RecordingInfo
	stopErr   error
	listRes   daemon.RecordingListResult
	listErr   error
	deleteErr error

	gotStartParams daemon.RecordingStartParams
	gotStopID      string
	gotDeleteID    string
	deleteCalled   bool
}

func (f *fakeCameraRecorder) StartRecording(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error) {
	f.gotStartParams = req
	return f.startInfo, f.startErr
}

func (f *fakeCameraRecorder) StopRecording(ctx context.Context, id string) (daemon.RecordingInfo, error) {
	f.gotStopID = id
	return f.stopInfo, f.stopErr
}

func (f *fakeCameraRecorder) ListRecordings(ctx context.Context) (daemon.RecordingListResult, error) {
	return f.listRes, f.listErr
}

func (f *fakeCameraRecorder) DeleteRecording(ctx context.Context, id string) error {
	f.deleteCalled = true
	f.gotDeleteID = id
	return f.deleteErr
}

// cameraTestDeps builds a Deps with every camera seam faked: no real daemon,
// no real browser, no real stdin prompt (AGENTS.md hard testing rule).
func cameraTestDeps(t *testing.T, stdout, stderr *bytes.Buffer) Deps {
	t.Helper()
	return Deps{
		Stdout: stdout,
		Stderr: stderr,
		PrinterClients: func(p domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()},
				WS9999:    fakeWS9999Client{},
			}
		},
		CameraViewer:   &fakeCameraViewer{url: "http://127.0.0.1:9000/?token=abc"},
		CameraRecorder: &fakeCameraRecorder{},
		Opener:         func(url string) error { return nil },
		IsInteractive:  func() bool { return true },
		Confirm:        func(prompt string) (bool, error) { return true, nil },
	}
}

// --- reorderArgsFlagsFirst ---

func TestReorderArgsFlagsFirst(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		boolFlags map[string]bool
		want      []string
	}{
		{
			name: "positional before flag",
			args: []string{"k2-5885", "--until", "print_end"},
			want: []string{"--until", "print_end", "k2-5885"},
		},
		{
			name:      "bool flag takes no value token",
			args:      []string{"k2-5885", "--yes"},
			boolFlags: map[string]bool{"yes": true},
			want:      []string{"--yes", "k2-5885"},
		},
		{
			// The "--" is re-emitted right before the positional tokens it
			// introduced, so flag.FlagSet still treats "-not-a-flag" as
			// positional instead of trying (and failing) to parse it as an
			// unknown flag.
			name:      "bare -- terminates flag parsing; rest is positional",
			args:      []string{"--yes", "--", "-not-a-flag"},
			boolFlags: map[string]bool{"yes": true},
			want:      []string{"--yes", "--", "-not-a-flag"},
		},
		{
			name: "bare -- with nothing after it",
			args: []string{"k2-5885", "--"},
			want: []string{"--", "k2-5885"},
		},
		{
			name: "bare -- preserves order and keeps flag-looking tokens positional",
			args: []string{"--", "--yes", "-x", "k2-5885"},
			want: []string{"--", "--yes", "-x", "k2-5885"},
		},
		{
			name: "-- part way through moves everything after it to positional",
			args: []string{"a", "--", "b", "c"},
			want: []string{"--", "a", "b", "c"},
		},
		{
			name: "no -- at all leaves output unchanged apart from reordering",
			args: []string{"k2-5885"},
			want: []string{"k2-5885"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reorderArgsFlagsFirst(tc.args, tc.boolFlags)
			if len(got) != len(tc.want) {
				t.Fatalf("reorderArgsFlagsFirst(%q) = %q, want %q", tc.args, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("reorderArgsFlagsFirst(%q) = %q, want %q", tc.args, got, tc.want)
				}
			}
		})
	}
}

// The reordered output must actually parse the way reorderArgsFlagsFirst
// intends once fed to a real flag.FlagSet: a bare "--" ends flag parsing and
// every token after it, including one that looks like a flag, comes back
// through Args() unchanged.
func TestReorderArgsFlagsFirst_TerminatorParsesAsPositionalInFlagSet(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "")

	got := reorderArgsFlagsFirst([]string{"--yes", "--", "-not-a-flag", "-x"}, map[string]bool{"yes": true})
	if err := fs.Parse(got); err != nil {
		t.Fatalf("flag.FlagSet.Parse(%q) = %v, want no error", got, err)
	}
	if !*yes {
		t.Error("--yes was not parsed as a flag")
	}
	if want := []string{"-not-a-flag", "-x"}; fs.NArg() != len(want) || fs.Arg(0) != want[0] || fs.Arg(1) != want[1] {
		t.Errorf("fs.Args() = %q, want %q", fs.Args(), want)
	}
}

// A recording id that happens to start with "-" is only usable positionally
// after a bare "--": without it, flag.FlagSet (via reorderArgsFlagsFirst)
// would try to parse it as a flag and fail with a usage error.
func TestRunCameraDelete_DashPrefixedIDAfterTerminator(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"delete", "--yes", "--", "-weird-id"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !rec.deleteCalled || rec.gotDeleteID != "-weird-id" {
		t.Errorf("DeleteRecording not called with the dash-prefixed id: called=%v id=%q", rec.deleteCalled, rec.gotDeleteID)
	}
}

// --- RunCamera dispatch ---

func TestRunCameraNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCamera(context.Background(), cameraTestDeps(t, &stdout, &stderr), nil)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "camera open") {
		t.Errorf("usage missing from stderr:\n%s", stderr.String())
	}
}

func TestRunCameraUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCamera(context.Background(), cameraTestDeps(t, &stdout, &stderr), []string{"bogus"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunCameraHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCamera(context.Background(), cameraTestDeps(t, &stdout, &stderr), []string{"help"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "camera delete") {
		t.Errorf("help output missing subcommand list:\n%s", stdout.String())
	}
}

// TestRunCameraLeafSubcommandsHelpFlag confirms -h/--help on every "camera"
// leaf subcommand prints its own usage to stdout and exits 0, matching the
// top-level --help's own contract (main.go's cli.ErrUsage handling) and
// RunCamera's own "camera -h"/"camera help" handling (TestRunCameraHelp)
// rather than being treated as an ordinary usage error (stderr, exit 2).
// "camera open", "camera record" and "camera delete" reach this via
// flag.ErrHelp (they use a flag.FlagSet); "camera stop" and "camera
// recordings" take no flags at all, so they check for it directly
// (isHelpFlag).
func TestRunCameraLeafSubcommandsHelpFlag(t *testing.T) {
	cases := []struct {
		subcommand string
		wantUsage  string
	}{
		{"open", "camera open"},
		{"record", "camera record"},
		{"stop", "camera stop"},
		{"recordings", "camera recordings"},
		{"delete", "camera delete"},
	}
	for _, tc := range cases {
		for _, flag := range []string{"-h", "--help"} {
			t.Run(tc.subcommand+"/"+flag, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				code := RunCamera(context.Background(), cameraTestDeps(t, &stdout, &stderr), []string{tc.subcommand, flag})
				if code != 0 {
					t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
				}
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want empty (usage must go to stdout)", stderr.String())
				}
				if !strings.Contains(stdout.String(), tc.wantUsage) {
					t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.wantUsage)
				}
			})
		}
	}
}

// --- camera open ---

func TestRunCameraOpenNoPrinterArgShowsEveryEnabled(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	var gotPrinterID string
	var openedURL string
	deps.CameraViewer = &fakeCameraViewer{url: "http://127.0.0.1:9000/", gotPrinterID: &gotPrinterID}
	deps.Opener = func(url string) error { openedURL = url; return nil }

	code := RunCamera(context.Background(), deps, []string{"open"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPrinterID != "" {
		t.Errorf("ViewerURL called with printerID %q, want empty (no printer given)", gotPrinterID)
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:9000/") {
		t.Errorf("stdout = %q, want it to contain the URL", stdout.String())
	}
	if openedURL != "http://127.0.0.1:9000/" {
		t.Errorf("opener called with %q, want the printed URL", openedURL)
	}
}

func TestRunCameraOpenResolvesGivenPrinter(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	var gotPrinterID string
	deps.CameraViewer = &fakeCameraViewer{url: "http://127.0.0.1:9000/", gotPrinterID: &gotPrinterID}

	code := RunCamera(context.Background(), deps, []string{"open", "k2-5885", "--no-browser"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPrinterID != "k2-5885" {
		t.Errorf("ViewerURL called with printerID %q, want k2-5885", gotPrinterID)
	}
}

func TestRunCameraOpenNoBrowserSkipsOpener(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	called := false
	deps.Opener = func(url string) error { called = true; return nil }

	code := RunCamera(context.Background(), deps, []string{"open", "--no-browser"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if called {
		t.Error("opener was called despite --no-browser")
	}
}

func TestRunCameraOpenOpenerFailureStillPrintsURL(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.Opener = func(url string) error { return errBoom }

	code := RunCamera(context.Background(), deps, []string{"open"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:9000") {
		t.Errorf("URL should still be printed to stdout on opener failure:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "could not open a browser") {
		t.Errorf("stderr = %q, want opener failure message", stderr.String())
	}
}

func TestRunCameraOpenViewerUnavailable(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraViewer = nil

	code := RunCamera(context.Background(), deps, []string{"open"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not wired up") {
		t.Errorf("stderr = %q, want the unavailable message", stderr.String())
	}
}

func TestRunCameraOpenUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunCamera(context.Background(), cameraTestDeps(t, &stdout, &stderr), []string{"open", "a", "b"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// errBoom is a simple sentinel error for tests that just need any non-nil error.
var errBoom = errBoomType{}

type errBoomType struct{}

func (errBoomType) Error() string { return "boom" }

// --- camera record ---

func TestRunCameraRecordStartsAndPrintsIDAndPath(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.PrinterClients = func(p domain.Printer) printerstate.Deps {
		return printerstate.Deps{
			Moonraker: &fakeMoonrakerClient{
				serverInfo:  fakeIdleServerInfo(),
				printerInfo: moonraker.PrinterInfoResult{Hostname: "K2-5885"},
			},
			WS9999: fakeWS9999Client{},
		}
	}
	rec := &fakeCameraRecorder{startInfo: daemon.RecordingInfo{
		ID: "k2-5885/20260928T150405Z",
		Parts: []daemon.RecordingPart{
			{Path: "/home/user/.creality-k2-mcp/recordings/k2-5885/20260928T150405Z.mp4"},
		},
	}}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != rec.startInfo.ID || lines[1] != rec.startInfo.Parts[0].Path {
		t.Errorf("stdout = %q, want id then path", stdout.String())
	}
	if rec.gotStartParams.PrinterID != "k2-5885" || rec.gotStartParams.Identity != "K2-5885" {
		t.Errorf("StartRecording params = %+v", rec.gotStartParams)
	}
	if rec.gotStartParams.Until != "" {
		t.Errorf("Until = %q, want empty (left for the daemon to default per mode)", rec.gotStartParams.Until)
	}
	if rec.gotStartParams.Mode != string(daemon.RecordModeVideo) {
		t.Errorf("Mode = %q, want video (default)", rec.gotStartParams.Mode)
	}
	if rec.gotStartParams.MaxDurationSeconds != 0 {
		t.Errorf("MaxDurationSeconds = %d, want 0 (unset)", rec.gotStartParams.MaxDurationSeconds)
	}
}

func TestRunCameraRecordModeTimelapsePrintsNoteInsteadOfPath(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.PrinterClients = func(p domain.Printer) printerstate.Deps {
		return printerstate.Deps{
			Moonraker: &fakeMoonrakerClient{
				serverInfo:  fakeIdleServerInfo(),
				printerInfo: moonraker.PrinterInfoResult{Hostname: "K2-5885"},
			},
			WS9999: fakeWS9999Client{},
		}
	}
	rec := &fakeCameraRecorder{startInfo: daemon.RecordingInfo{
		ID:   "k2-5885/20260928T150405Z-timelapse",
		Mode: daemon.RecordModeTimelapse,
	}}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885", "--mode", "timelapse"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if rec.gotStartParams.Mode != string(daemon.RecordModeTimelapse) {
		t.Errorf("Mode = %q, want timelapse", rec.gotStartParams.Mode)
	}
	out := stdout.String()
	if !strings.Contains(out, rec.startInfo.ID) {
		t.Errorf("stdout = %q, want the recording id", out)
	}
	if !strings.Contains(out, "timelapse") {
		t.Errorf("stdout = %q, want a timelapse note instead of a fabricated file path", out)
	}
}

func TestRunCameraRecordInvalidMode(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885", "--mode", "sideways"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunCameraRecordUntilAndMaxDuration(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.PrinterClients = func(p domain.Printer) printerstate.Deps {
		return printerstate.Deps{
			Moonraker: &fakeMoonrakerClient{
				serverInfo:  fakeIdleServerInfo(),
				printerInfo: moonraker.PrinterInfoResult{Hostname: "K2-5885"},
			},
			WS9999: fakeWS9999Client{},
		}
	}
	rec := &fakeCameraRecorder{startInfo: daemon.RecordingInfo{ID: "k2-5885/x"}}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885", "--until", "print_end", "--max-duration", "2h"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if rec.gotStartParams.Until != string(daemon.RecordUntilPrintEnd) {
		t.Errorf("Until = %q, want print_end", rec.gotStartParams.Until)
	}
	if rec.gotStartParams.MaxDurationSeconds != int((2 * time.Hour).Seconds()) {
		t.Errorf("MaxDurationSeconds = %d, want %d", rec.gotStartParams.MaxDurationSeconds, int((2 * time.Hour).Seconds()))
	}
}

func TestRunCameraRecordInvalidUntil(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885", "--until", "sideways"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunCameraRecordNegativeMaxDuration(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885", "--max-duration", "-1h"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunCameraRecordUnverifiedIdentityRefuses(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	// PrinterClients from cameraTestDeps returns a fake with no PrinterInfo
	// set, so Identity resolves to printerstate.UnverifiedIdentity.
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "could not be verified") {
		t.Errorf("stderr = %q, want the unverified-identity message", stderr.String())
	}
	if rec.gotStartParams != (daemon.RecordingStartParams{}) {
		t.Error("StartRecording must not have been called")
	}
}

func TestRunCameraRecordRecorderUnavailable(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = nil

	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not wired up") {
		t.Errorf("stderr = %q, want the unavailable message", stderr.String())
	}
}

func TestRunCameraRecordStartError(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.PrinterClients = func(p domain.Printer) printerstate.Deps {
		return printerstate.Deps{
			Moonraker: &fakeMoonrakerClient{
				serverInfo:  fakeIdleServerInfo(),
				printerInfo: moonraker.PrinterInfoResult{Hostname: "K2-5885"},
			},
			WS9999: fakeWS9999Client{},
		}
	}
	deps.CameraRecorder = &fakeCameraRecorder{startErr: errBoom}

	code := RunCamera(context.Background(), deps, []string{"record", "k2-5885"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

// --- camera stop ---

func TestRunCameraStopByPrinterName(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	rec := &fakeCameraRecorder{
		listRes: daemon.RecordingListResult{Recordings: []daemon.RecordingInfo{
			{ID: "k2-5885/20260928T150405Z", PrinterID: "k2-5885", Active: true},
		}},
		stopInfo: daemon.RecordingInfo{ID: "k2-5885/20260928T150405Z", StopReason: "stopped by request"},
	}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"stop", "k2-5885"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if rec.gotStopID != "k2-5885/20260928T150405Z" {
		t.Errorf("StopRecording called with id %q, want the active recording's id", rec.gotStopID)
	}
	if !strings.Contains(stdout.String(), "Stopped recording") {
		t.Errorf("stdout = %q, want a stopped confirmation", stdout.String())
	}
}

func TestRunCameraStopByRawRecordingID(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	rec := &fakeCameraRecorder{stopInfo: daemon.RecordingInfo{ID: "k2-5885/20260928T150405Z"}}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"stop", "k2-5885/20260928T150405Z"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if rec.gotStopID != "k2-5885/20260928T150405Z" {
		t.Errorf("StopRecording called with id %q, want the raw argument passed through", rec.gotStopID)
	}
}

func TestRunCameraStopNoActiveRecording(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = &fakeCameraRecorder{listRes: daemon.RecordingListResult{}}

	code := RunCamera(context.Background(), deps, []string{"stop", "k2-5885"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no active recording") {
		t.Errorf("stderr = %q, want a no-active-recording message", stderr.String())
	}
}

func TestRunCameraStopRecorderUnavailable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = nil

	code := RunCamera(context.Background(), deps, []string{"stop", "some-id"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunCameraStopUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	code := RunCamera(context.Background(), deps, []string{"stop", "a", "b"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// --- camera recordings ---

func TestRunCameraRecordingsListsAllWithDiskUsage(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = &fakeCameraRecorder{listRes: daemon.RecordingListResult{
		Recordings: []daemon.RecordingInfo{
			{ID: "a/1", PrinterID: "a", Bytes: 1000, DurationSeconds: 30, Parts: []daemon.RecordingPart{{}}},
			{ID: "b/1", PrinterID: "b", Bytes: 2000, DurationSeconds: 60, Parts: []daemon.RecordingPart{{}, {}}, Active: true},
		},
		DiskUsageBytes: 3000,
	}}

	code := RunCamera(context.Background(), deps, []string{"recordings"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "a/1") || !strings.Contains(out, "b/1") {
		t.Errorf("output missing a recording id:\n%s", out)
	}
	if !strings.Contains(out, "3000 bytes total") {
		t.Errorf("output missing total disk usage:\n%s", out)
	}
}

func TestRunCameraRecordingsFilterByPrinter(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-a", Name: "K2-A", Host: "192.168.1.10", MoonrakerPort: 7125, Hostname: "K2-A", Enabled: true,
	})
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-b", Name: "K2-B", Host: "192.168.1.11", MoonrakerPort: 7125, Hostname: "K2-B", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = &fakeCameraRecorder{listRes: daemon.RecordingListResult{
		Recordings: []daemon.RecordingInfo{
			{ID: "k2-a/1", PrinterID: "k2-a"},
			{ID: "k2-b/1", PrinterID: "k2-b"},
		},
		DiskUsageBytes: 500,
	}}

	code := RunCamera(context.Background(), deps, []string{"recordings", "k2-a"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "k2-a/1") {
		t.Errorf("output missing k2-a's recording:\n%s", out)
	}
	if strings.Contains(out, "k2-b/1") {
		t.Errorf("output should not include k2-b's recording:\n%s", out)
	}
}

func TestRunCameraRecordingsEmpty(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = &fakeCameraRecorder{}

	code := RunCamera(context.Background(), deps, []string{"recordings"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "(no recordings)") {
		t.Errorf("stdout = %q, want the no-recordings message", stdout.String())
	}
}

func TestRunCameraRecordingsRecorderUnavailable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = nil

	code := RunCamera(context.Background(), deps, []string{"recordings"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

// --- camera delete ---

func TestRunCameraDeleteYesSkipsConfirmation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.Confirm = func(prompt string) (bool, error) {
		t.Fatal("Confirm should not be called with --yes")
		return false, nil
	}
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"delete", "k2-5885/x", "--yes"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !rec.deleteCalled || rec.gotDeleteID != "k2-5885/x" {
		t.Errorf("DeleteRecording not called with the right id: called=%v id=%q", rec.deleteCalled, rec.gotDeleteID)
	}
	if !strings.Contains(stdout.String(), "Deleted recording") {
		t.Errorf("stdout = %q, want a deleted confirmation", stdout.String())
	}
}

func TestRunCameraDeleteNonInteractiveWithoutYesRefuses(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.IsInteractive = func() bool { return false }
	deps.Confirm = func(prompt string) (bool, error) {
		t.Fatal("Confirm should not be called in a non-interactive session")
		return false, nil
	}
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"delete", "k2-5885/x"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if rec.deleteCalled {
		t.Error("DeleteRecording must not have been called")
	}
	if !strings.Contains(stderr.String(), "--yes") {
		t.Errorf("stderr = %q, want it to mention --yes", stderr.String())
	}
}

func TestRunCameraDeleteInteractiveConfirmYes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.IsInteractive = func() bool { return true }
	deps.Confirm = func(prompt string) (bool, error) { return true, nil }
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"delete", "k2-5885/x"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !rec.deleteCalled {
		t.Error("DeleteRecording must have been called after a yes confirmation")
	}
}

func TestRunCameraDeleteInteractiveConfirmNo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.IsInteractive = func() bool { return true }
	deps.Confirm = func(prompt string) (bool, error) { return false, nil }
	rec := &fakeCameraRecorder{}
	deps.CameraRecorder = rec

	code := RunCamera(context.Background(), deps, []string{"delete", "k2-5885/x"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if rec.deleteCalled {
		t.Error("DeleteRecording must not have been called after a no confirmation")
	}
	if !strings.Contains(stdout.String(), "Not deleted") {
		t.Errorf("stdout = %q, want a not-deleted message", stdout.String())
	}
}

func TestRunCameraDeleteUsageErrorNoID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	code := RunCamera(context.Background(), deps, []string{"delete", "--yes"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunCameraDeleteRecorderUnavailable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := cameraTestDeps(t, &stdout, &stderr)
	deps.CameraRecorder = nil

	code := RunCamera(context.Background(), deps, []string{"delete", "k2-5885/x", "--yes"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}
