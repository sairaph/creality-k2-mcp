package doctorchecks

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/mcp-wizard/daemon/socket"
	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
)

// startTestDaemonAtDefaultPaths opens and serves a real daemon.Server over a
// real AF_UNIX socket, exactly matching internal/daemon/client/client_test.go's
// own fixture (AGENTS.md's hard testing rule permits this: a Unix domain
// socket is not network I/O, and this never touches the real user's
// ~/.creality-k2-mcp/daemon or a real printer), serving at exactly the
// socket daemon.DefaultPaths resolves to, so DaemonCheck.Run (which builds
// its client with plain client.New and so resolves the same default paths)
// finds it. The caller must call setTestHome first, so that default
// resolves under a temp HOME/USERPROFILE rather than the real user's.
func startTestDaemonAtDefaultPaths(t *testing.T) daemon.Paths {
	t.Helper()
	paths, err := daemon.DefaultPaths()
	if err != nil {
		t.Fatalf("daemon.DefaultPaths: %v", err)
	}
	opts := daemon.NewProductionOptions(paths)
	srv, err := daemon.Open(opts)
	if err != nil {
		t.Fatalf("daemon.Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
	})
	return paths
}

func TestDaemonCheckRun_NotRunning(t *testing.T) {
	// DefaultPaths resolves under the user's real home directory, which this
	// test cannot control without redirecting HOME - and nothing should ever
	// be listening on a freshly redirected home's daemon socket, so this
	// exercises Run's real not-running path safely.
	setTestHome(t)

	// A daemon-less Run must come back quickly and never spawn a process:
	// the client this check builds uses client.WithoutAutostart, so nothing
	// here can fall back to lock.EnsureRunning even though this process
	// does have a real executable path (dev_docs/review-backlog.md item 29).
	start := time.Now()
	res := DaemonCheck{}.Run(context.Background())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run took %s, want a fast failure with nothing listening and no autostart", elapsed)
	}
	if res.Status != doctor.Warn {
		t.Fatalf("Status = %v, Detail = %q, want Warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "not running") {
		t.Errorf("Detail = %q, want it to say the daemon is not running", res.Detail)
	}
	if !strings.Contains(res.Detail, "idle heating is refused") || !strings.Contains(res.Detail, "starts it automatically on demand") {
		t.Errorf("Detail = %q, want the idle-heat-refused and autostart-on-demand explanation", res.Detail)
	}
}

func TestMaskViewerURLStripsTokenAndQuery(t *testing.T) {
	masked := maskViewerURL("http://127.0.0.1:54321/?token=deadbeef&printer=k2-1")
	if strings.Contains(masked, "deadbeef") {
		t.Errorf("maskViewerURL leaked the token: %q", masked)
	}
	if masked != "http://127.0.0.1:54321/" {
		t.Errorf("maskViewerURL = %q, want the base URL with no query", masked)
	}
}

func TestDaemonCheckRun_RunningReportsArmedCountAndViewerURL(t *testing.T) {
	setTestHome(t)
	paths := startTestDaemonAtDefaultPaths(t)

	conn, err := socket.Dial(paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	var armRes daemon.ArmResult
	armParams := daemon.ArmParams{Identity: "k2-1.local", Heater: "extruder", TargetC: 200, ArmMinutes: 5, Host: "k2-1.local", MoonrakerPort: 7125}
	if err := conn.Call(context.Background(), daemon.MethodWatchdogArm, armParams, &armRes); err != nil {
		t.Fatalf("arm: %v", err)
	}
	conn.Close()

	res := DaemonCheck{Identities: []string{"k2-1.local", "k2-2.local"}}.Run(context.Background())
	if res.Status != doctor.OK {
		t.Fatalf("Status = %v, Detail = %q, want OK", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "running (pid") {
		t.Errorf("Detail = %q, want a running line with the pid", res.Detail)
	}
	if !strings.Contains(res.Detail, "1 watchdog(s) armed across 2 enabled printer(s)") {
		t.Errorf("Detail = %q, want exactly one armed heater counted", res.Detail)
	}
	if !strings.Contains(res.Detail, "viewer URL: ok") {
		t.Errorf("Detail = %q, want a viewer URL ok line", res.Detail)
	}
	if strings.Contains(res.Detail, "token=") {
		t.Errorf("Detail = %q, must never contain the raw viewer access token query", res.Detail)
	}
}
