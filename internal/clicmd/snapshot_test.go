package clicmd

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

func fakeSnapshotImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 10), G: uint8(y * 10), B: 200, A: 255})
		}
	}
	return img
}

func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	return dir
}

func TestRunSnapshotWritesDefaultTimestampedFile(t *testing.T) {
	isolateHome(t)
	chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	fixedNow := time.Date(2026, 9, 28, 15, 4, 5, 0, time.UTC)
	var gotHost string
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{
		serverInfo: fakeIdleServerInfo(),
	})
	deps.Now = func() time.Time { return fixedNow }
	deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
		gotHost = host
		return &camera.SnapshotResult{Image: fakeSnapshotImage(), CapturedAt: fixedNow}, nil
	}

	code := RunSnapshot(context.Background(), deps, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotHost != "192.168.1.10" {
		t.Errorf("CameraSnapshot called with host %q, want 192.168.1.10", gotHost)
	}

	wantPath := "k2-5885-20260928-150405.jpg"
	printedPath := strings.TrimSpace(stdout.String())
	if printedPath != wantPath {
		t.Errorf("printed path = %q, want %q", printedPath, wantPath)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read snapshot file: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("snapshot file is not a valid JPEG: %v", err)
	}
}

func TestRunSnapshotOutFlag(t *testing.T) {
	isolateHome(t)
	dir := chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()})
	deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
		return &camera.SnapshotResult{Image: fakeSnapshotImage(), CapturedAt: time.Now()}, nil
	}

	wantPath := filepath.Join(dir, "chosen.jpg")
	code := RunSnapshot(context.Background(), deps, []string{"--out", wantPath})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != wantPath {
		t.Errorf("printed path = %q, want %q", stdout.String(), wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("expected file at %s: %v", wantPath, err)
	}
}

func TestRunSnapshotRefusesToOverwriteExistingFile(t *testing.T) {
	isolateHome(t)
	dir := chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	existingPath := filepath.Join(dir, "chosen.jpg")
	const existingContent = "not a jpeg, just a marker"
	if err := os.WriteFile(existingPath, []byte(existingContent), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()})
	deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
		return &camera.SnapshotResult{Image: fakeSnapshotImage(), CapturedAt: time.Now()}, nil
	}

	code := RunSnapshot(context.Background(), deps, []string{"--out", existingPath})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already exists") || !strings.Contains(stderr.String(), "--force") {
		t.Errorf("stderr = %q, want it to mention already exists and --force", stderr.String())
	}

	data, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatalf("read existing file: %v", err)
	}
	if string(data) != existingContent {
		t.Errorf("existing file was modified: got %q, want %q", data, existingContent)
	}
}

func TestRunSnapshotForceOverwritesExistingFile(t *testing.T) {
	isolateHome(t)
	dir := chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	existingPath := filepath.Join(dir, "chosen.jpg")
	if err := os.WriteFile(existingPath, []byte("not a jpeg, just a marker"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()})
	deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
		return &camera.SnapshotResult{Image: fakeSnapshotImage(), CapturedAt: time.Now()}, nil
	}

	code := RunSnapshot(context.Background(), deps, []string{"--out", existingPath, "--force"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != existingPath {
		t.Errorf("printed path = %q, want %q", stdout.String(), existingPath)
	}

	data, err := os.ReadFile(existingPath)
	if err != nil {
		t.Fatalf("read overwritten file: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("overwritten file is not a valid JPEG: %v", err)
	}

	// No leftover temporary file from the write-then-rename should remain
	// in the directory alongside the final snapshot.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".snapshot-") {
			t.Errorf("leftover temporary file %q was not cleaned up", e.Name())
		}
	}
}

// TestRunSnapshotOfflinePrinterDoesNotWaitForCapture confirms review backlog
// item 51 review's concurrency fix (dev_docs/item51-review.md part 2a): the
// state check and the capture now run concurrently rather than the state
// check gating the capture, so an offline printer's command must not block
// on a capture that will never be reported. deps.CameraSnapshot here blocks
// until its context is cancelled, standing in for a real capture that would
// otherwise run for its own multi-second budget; RunSnapshot must still
// return promptly (it cancels captureCtx once the offline state is known),
// with the offline message and without ever using a capture result.
func TestRunSnapshotOfflinePrinterDoesNotWaitForCapture(t *testing.T) {
	isolateHome(t)
	dir := chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	moon := &fakeMoonrakerClient{serverInfoErr: fmt.Errorf("connection refused")}
	deps := testDeps(t, &stdout, &stderr, moon)
	deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan int, 1)
	go func() { done <- RunSnapshot(context.Background(), deps, nil) }()

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunSnapshot did not return promptly for an offline printer; it must not wait for the capture")
	}
	if !strings.Contains(stderr.String(), "offline") {
		t.Errorf("stderr = %q, want it to mention offline", stderr.String())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("offline printer must not write any snapshot file, found %v", entries)
	}
}

// TestRunSnapshotStateAndCaptureRunConcurrently confirms the state check and
// the capture actually overlap rather than running one after the other
// (dev_docs/item51-review.md part 2a's proposed fix): both are given an
// artificial delay, and the command's total wall time must stay well under
// their sum, close to whichever delay is larger instead.
func TestRunSnapshotStateAndCaptureRunConcurrently(t *testing.T) {
	isolateHome(t)
	chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	const delay = 150 * time.Millisecond
	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{
		serverInfo:      fakeIdleServerInfo(),
		serverInfoDelay: delay,
	})
	deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
		time.Sleep(delay)
		return &camera.SnapshotResult{Image: fakeSnapshotImage(), CapturedAt: time.Now()}, nil
	}

	start := time.Now()
	code := RunSnapshot(context.Background(), deps, nil)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	// Sequential execution would take at least 2*delay; concurrent execution
	// should stay close to one delay plus scheduling/JPEG-encode slack.
	if elapsed >= 2*delay {
		t.Errorf("RunSnapshot took %v, want well under %v (the state check and the capture must run concurrently, not sequentially)", elapsed, 2*delay)
	}
}

func TestRunSnapshotCameraError(t *testing.T) {
	isolateHome(t)
	chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()})
	deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
		return nil, fmt.Errorf("no keyframe")
	}

	code := RunSnapshot(context.Background(), deps, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no keyframe") {
		t.Errorf("stderr = %q, want it to mention the capture error", stderr.String())
	}
}

// TestRunSnapshotUnavailableWhenDaemonClientNotWired confirms review
// backlog item 51's "no direct fallback, one path": a nil
// Deps.CameraSnapshot (the daemon client could not be built) must report
// the camera as unavailable, never fall back to any other capture path.
func TestRunSnapshotUnavailableWhenDaemonClientNotWired(t *testing.T) {
	isolateHome(t)
	chdirTemp(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()})
	// deps.CameraSnapshot is left nil by testDeps.

	code := RunSnapshot(context.Background(), deps, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not wired up") {
		t.Errorf("stderr = %q, want it to mention the daemon client is not wired up", stderr.String())
	}
}

// TestRunSnapshotFlagOrder confirms flags are accepted both before and
// after the positional printer argument (dev_docs/review-backlog.md item
// 48, dev_docs/t11e-soak-report-2.md anomaly 1): a plain Go flag.FlagSet
// stops parsing at the first non-flag token, so "snapshot k2-5885 --out
// file.jpg --force" (the order snapshotUsage's own usage string documents)
// used to be misread as three positional arguments and fail with a usage
// error, exactly as camera.go's reorderArgsFlagsFirst doc comment
// describes for "camera record".
func TestRunSnapshotFlagOrder(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"flags before printer", []string{"--out", "PLACEHOLDER", "--force", "k2-5885"}},
		{"flags after printer", []string{"k2-5885", "--out", "PLACEHOLDER", "--force"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			dir := chdirTemp(t)
			registerTestPrinter(t, "", domain.Printer{
				ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
				Hostname: "K2-5885", Enabled: true,
			})

			outPath := filepath.Join(dir, "chosen.jpg")
			args := make([]string, len(tc.args))
			for i, a := range tc.args {
				if a == "PLACEHOLDER" {
					a = outPath
				}
				args[i] = a
			}

			var stdout, stderr bytes.Buffer
			var gotHost string
			deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()})
			deps.CameraSnapshot = func(ctx context.Context, host string) (*camera.SnapshotResult, error) {
				gotHost = host
				return &camera.SnapshotResult{Image: fakeSnapshotImage(), CapturedAt: time.Now()}, nil
			}

			code := RunSnapshot(context.Background(), deps, args)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			if gotHost != "192.168.1.10" {
				t.Errorf("CameraSnapshot called with host %q, want 192.168.1.10", gotHost)
			}
			if strings.TrimSpace(stdout.String()) != outPath {
				t.Errorf("printed path = %q, want %q", stdout.String(), outPath)
			}
			if _, err := os.Stat(outPath); err != nil {
				t.Errorf("expected file at %s: %v", outPath, err)
			}
		})
	}
}

func TestRunSnapshotUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{})
	code := RunSnapshot(context.Background(), deps, []string{"a", "b"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

// TestRunSnapshotHelpFlag confirms -h/--help prints usage to stdout and
// exits 0, matching the top-level --help's own contract (main.go's
// cli.ErrUsage handling: printUsage(os.Stdout); os.Exit(0)) rather than
// being treated as an ordinary usage error (stderr, exit 2).
func TestRunSnapshotHelpFlag(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{})
			code := RunSnapshot(context.Background(), deps, []string{flag})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty (usage must go to stdout)", stderr.String())
			}
			if !strings.Contains(stdout.String(), "usage: snapshot") {
				t.Errorf("stdout = %q, want it to contain the usage string", stdout.String())
			}
		})
	}
}
