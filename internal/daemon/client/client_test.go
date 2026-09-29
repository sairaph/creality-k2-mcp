package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/daemon/daemontest"
	"github.com/sairaph/creality-k2-mcp/internal/policy"
)

// TestMain sets K2_MCP_NO_AUTOSTART=1 for every test in this package, belt
// and suspenders alongside isTestBinaryOrGuarded's own .test/.test.exe
// suffix check (dev_docs/review-backlog.md item 42): any test here that
// constructs a real Client, including via New with no options at all, must
// never be able to reach lock.EnsureRunning (AGENTS.md's hard testing rule).
// Individual tests that need to exercise the suffix check or the env guard
// in isolation clear or set this with t.Setenv, which restores this value
// once that test returns.
//
// daemontest.Guard additionally points daemon.FallbackRoot at a short,
// package-owned temp directory: New calls daemon.DefaultPaths(), and a test
// here that also redirects HOME/USERPROFILE to a t.TempDir() (long enough
// on its own to push the preferred path past the socket length budget)
// would otherwise silently create a creality-k2-mcp-<hash> directory in the
// real user's real temp directory (dev_docs/review-backlog.md item 43).
func TestMain(m *testing.M) {
	os.Setenv(noAutostartEnv, "1")
	os.Exit(daemontest.Guard(m))
}

// newTestOptions uses internal/daemon's real production wiring
// (registry-backed StateSource/TemplateSender): this package only tests the
// IPC round trip (Alive/Arm/Disarm/Status against a real socket), never
// expiry behaviour, which internal/daemon's own tests already cover with
// fakes. No entry ever gets added to the registry in these tests, so the
// watchdog's own printer access is never actually exercised here.
func newTestOptions(t *testing.T) daemon.Options {
	t.Helper()
	// Redirect the per-user base directory (~/.creality-k2-mcp, which the
	// production Watchdog's registry lookups read from) into a fresh
	// t.TempDir(), so this test never reads the real user's printer
	// registry (matching internal/policy/helpers_test.go's setTestHome).
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := t.TempDir()
	return daemon.NewProductionOptions(daemon.PathsIn(dir))
}

func startTestDaemon(t *testing.T) daemon.Paths {
	t.Helper()
	opts := newTestOptions(t)
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
	return opts.Paths
}

// Alive is true (with no autostart needed) once a real daemon is up, and
// Arm/Disarm round-trip against it over the real socket.
func TestClient_AliveArmDisarm_AgainstRunningDaemon(t *testing.T) {
	paths := startTestDaemon(t)
	c := &Client{paths: paths} // no executable set: autostart must never be attempted

	if !c.Alive(context.Background()) {
		t.Fatal("Alive = false, want true against a running daemon")
	}

	err := c.Arm(context.Background(), policy.IdleHeatArmRequest{
		Identity: "printer-1", Heater: "extruder", TargetC: 200, ArmMinutes: 5,
		Host: "printer-1", MoonrakerPort: 7125,
	})
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}

	heaters, ok := c.Status(context.Background(), "printer-1")
	if !ok || len(heaters) != 1 || !heaters[0].Armed {
		t.Fatalf("Status = (%+v, %v), want one armed heater", heaters, ok)
	}

	if err := c.Disarm(context.Background(), "printer-1"); err != nil {
		t.Fatalf("Disarm: %v", err)
	}
	heaters, ok = c.Status(context.Background(), "printer-1")
	if !ok || len(heaters) != 1 || heaters[0].Armed {
		t.Fatalf("Status after Disarm = (%+v, %v), want one disarmed heater", heaters, ok)
	}
}

// Alive is false, quickly, when nothing is listening and this Client has no
// executable path to autostart with - it must never hang or panic.
func TestClient_Alive_FalseWhenNotRunningAndNoAutostart(t *testing.T) {
	dir := t.TempDir()
	c := &Client{paths: daemon.PathsIn(dir)} // executable == "": autostart disabled

	start := time.Now()
	alive := c.Alive(context.Background())
	if alive {
		t.Fatal("Alive = true, want false: nothing is running")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Alive took %s, want a fast failure with no daemon and no autostart path", elapsed)
	}
}

// Disarm against a daemon that is not running is treated as success (best
// effort, matching internal/policy's requirement that a print starting must
// never be blocked by the background daemon).
func TestClient_Disarm_NoopWhenDaemonNotRunning(t *testing.T) {
	dir := t.TempDir()
	c := &Client{paths: daemon.PathsIn(dir)}

	if err := c.Disarm(context.Background(), "printer-1"); err != nil {
		t.Fatalf("Disarm against a stopped daemon = %v, want nil (best-effort no-op)", err)
	}
}

// Arm against a daemon that is not running does return an error (unlike
// Disarm, Arm's caller - internal/policy - only calls it after Alive
// already reported true, so a failure here is meaningful and must not be
// swallowed).
func TestClient_Arm_ErrorsWhenDaemonNotRunning(t *testing.T) {
	dir := t.TempDir()
	c := &Client{paths: daemon.PathsIn(dir)}

	err := c.Arm(context.Background(), policy.IdleHeatArmRequest{Identity: "p", Heater: "extruder", TargetC: 1, ArmMinutes: 1})
	if err == nil {
		t.Fatal("Arm against a stopped daemon = nil, want an error")
	}
}

var _ policy.Watchdog = (*Client)(nil)

// New(WithoutAutostart()) marks the returned Client so canAutostart is
// always false, regardless of whether this process resolved its own
// executable path (dev_docs/review-backlog.md item 29).
func TestNew_WithoutAutostart_DisablesAutostart(t *testing.T) {
	c, err := New(WithoutAutostart())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.canAutostart() {
		t.Fatal("canAutostart() = true after WithoutAutostart, want false")
	}
	if !c.noAutostart {
		t.Fatal("noAutostart = false, want true after WithoutAutostart")
	}
}

// A Client built with WithoutAutostart never runs lock.EnsureRunning, even
// when it does know its own executable path: Alive against a stopped
// daemon must return false quickly, never spawn a process.
func TestClient_WithoutAutostart_AliveNeverStarts(t *testing.T) {
	dir := t.TempDir()
	c := &Client{paths: daemon.PathsIn(dir), executable: os.Args[0], noAutostart: true}

	start := time.Now()
	if c.Alive(context.Background()) {
		t.Fatal("Alive = true, want false: nothing is running and autostart is disabled")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Alive took %s, want a fast failure with WithoutAutostart", elapsed)
	}
}

// A Client built with WithoutAutostart never runs lock.EnsureRunning from
// ViewerURL either.
func TestClient_WithoutAutostart_ViewerURLNeverStarts(t *testing.T) {
	dir := t.TempDir()
	c := &Client{paths: daemon.PathsIn(dir), executable: os.Args[0], noAutostart: true}

	start := time.Now()
	if _, err := c.ViewerURL(context.Background(), ""); err == nil {
		t.Fatal("ViewerURL = nil error, want a dial failure: nothing is running")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("ViewerURL took %s, want a fast failure with WithoutAutostart", elapsed)
	}
}

// Ping is a plain, non-autostarting round trip that returns the daemon's
// real pid, even for a Client that could otherwise autostart (it has no
// executable set here, but Ping never even looks at that field).
func TestClient_Ping_AgainstRunningDaemon(t *testing.T) {
	paths := startTestDaemon(t)
	c := &Client{paths: paths}

	res, ok := c.Ping(context.Background())
	if !ok {
		t.Fatal("Ping = not ok, want ok against a running daemon")
	}
	if res.PID == 0 {
		t.Error("Ping PID = 0, want the daemon's real pid")
	}
}

// Ping against nothing listening returns quickly, never blocking on a
// missing daemon directory or socket.
func TestClient_Ping_FalseWhenNotRunning(t *testing.T) {
	dir := t.TempDir()
	c := &Client{paths: daemon.PathsIn(dir)}

	start := time.Now()
	if _, ok := c.Ping(context.Background()); ok {
		t.Fatal("Ping = ok, want false: nothing is running")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Ping took %s, want a fast failure with nothing listening", elapsed)
	}
}

// dial (and so every RPC method) refuses a symlinked daemon directory via
// daemon.VerifyDir (dev_docs/review-backlog.md item 30): a client must
// apply the same directory trust check the daemon's own Open does before it
// ever dials into paths.Dir.
func TestClient_RefusesSymlinkDaemonDir(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("MkdirAll real: %v", err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	c := &Client{paths: daemon.PathsIn(link)}
	if c.Alive(context.Background()) {
		t.Fatal("Alive against a symlinked daemon dir = true, want false")
	}
	if err := c.Arm(context.Background(), policy.IdleHeatArmRequest{Identity: "p", Heater: "extruder", TargetC: 1, ArmMinutes: 1}); err == nil {
		t.Fatal("Arm against a symlinked daemon dir = nil, want an error")
	}
}

// New (no options at all) must still resolve to a Client that can never
// autostart when called from inside this test binary: os.Executable()
// resolves to this package's own *.test / *.test.exe, which
// isTestBinaryOrGuarded's suffix check catches on its own, with no help from
// the K2_MCP_NO_AUTOSTART env guard (cleared here to isolate the two
// guards). This is item 42's core requirement: a test that forgets
// WithoutAutostart must still never be able to spawn the real daemon.
func TestNew_TestBinaryNeverAutostarts(t *testing.T) {
	t.Setenv(noAutostartEnv, "")

	c, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.executable == "" {
		t.Skip("os.Executable() did not resolve a path in this environment; nothing to assert")
	}
	if c.canAutostart() {
		t.Fatalf("canAutostart() = true for a Client built inside a Go test binary (%s), want false", c.executable)
	}

	start := time.Now()
	if c.Alive(context.Background()) {
		t.Fatal("Alive = true, want false: nothing is running and this is a test binary")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Alive took %s, want a fast failure with no daemon and a test-binary executable", elapsed)
	}
}

// K2_MCP_NO_AUTOSTART=1 disables autostart even for a Client whose
// executable does not look like a test binary at all - the explicit escape
// hatch half of item 42's guard, independent of the suffix check.
func TestCanAutostart_EnvGuardDisablesAutostart(t *testing.T) {
	t.Setenv(noAutostartEnv, "1")

	c := &Client{executable: filepath.Join("usr", "local", "bin", "creality-k2-mcp")}
	if c.canAutostart() {
		t.Fatal("canAutostart() = true with K2_MCP_NO_AUTOSTART=1, want false")
	}
}

// isTestBinaryOrGuarded's suffix matching, tested directly (env guard
// cleared so only the suffix check is exercised): case-insensitive, matches
// a prefixed name (e.g. a package-derived binary name before ".test"), and
// leaves an ordinary production executable name alone.
func TestIsTestBinaryOrGuarded_SuffixMatching(t *testing.T) {
	t.Setenv(noAutostartEnv, "")

	cases := []struct {
		exe  string
		want bool
	}{
		{filepath.Join("C:", "proj", "internal", "daemon", "client", "client.test.exe"), true},
		{filepath.Join("tmp", "go-build123", "b001", "client.test"), true},
		{filepath.Join("tmp", "go-build123", "b001", "CLIENT.TEST"), true},
		{filepath.Join("tmp", "go-build123", "b001", "CLIENT.TEST.EXE"), true},
		{filepath.Join("C:", "Program Files", "creality-k2-mcp", "creality-k2-mcp.exe"), false},
		{filepath.Join("usr", "local", "bin", "creality-k2-mcp"), false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isTestBinaryOrGuarded(tc.exe); got != tc.want {
			t.Errorf("isTestBinaryOrGuarded(%q) = %v, want %v", tc.exe, got, tc.want)
		}
	}
}
