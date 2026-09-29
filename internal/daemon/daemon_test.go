package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/mcp-wizard/daemon/socket"
)

// This file's own testOptions helper (below) is shared by every other test
// file in this package that needs a full Server (socket_perms_unix_test.go,
// viewer_test.go, and this file's own IPC round-trip tests).

// This file exercises Server end to end over its real socket in a temp
// directory (AF_UNIX is not network I/O, so this is allowed under AGENTS.md
// hard testing rule even though it is a real socket file) - never a real
// printer, never a non-loopback listener.

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	hub := NewHub(&fakeOpener{})
	recDir := filepath.Join(t.TempDir(), "recordings")
	return Options{
		Paths:    PathsIn(dir),
		Hub:      hub,
		Watchdog: newTestWatchdog(newFakeStateSource(), newFakeSender()),
		Recorder: NewRecorder(recDir, hub, newFakeStateSource(), nil),
	}
}

func openTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func serveInBackground(t *testing.T, s *Server) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

// Ping, arm, status and disarm all round-trip correctly over the socket.
func TestServer_IPCRoundTrip(t *testing.T) {
	opts := testOptions(t)
	s := openTestServer(t, opts)
	serveInBackground(t, s)

	conn, err := socket.Dial(opts.Paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	ctx := context.Background()

	var ping PingResult
	if err := conn.Call(ctx, MethodPing, nil, &ping); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !ping.OK {
		t.Fatalf("ping = %+v, want OK", ping)
	}

	_, idleDerived := idleSnapshot(200)
	opts.Watchdog.direct.(*fakeStateSource).set("printer-x", snapshotEntry{
		derived: idleDerived, ok: true,
	})

	var armRes ArmResult
	armParams := ArmParams{Identity: "printer-x", Heater: "extruder", TargetC: 200, ArmMinutes: 5, Host: "printer-x", MoonrakerPort: 7125}
	if err := conn.Call(ctx, MethodWatchdogArm, armParams, &armRes); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if !armRes.OK {
		t.Fatalf("armRes = %+v, want OK", armRes)
	}

	var statusRes StatusResult
	if err := conn.Call(ctx, MethodWatchdogStatus, StatusParams{Identity: "printer-x"}, &statusRes); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(statusRes.Heaters) != 1 || !statusRes.Heaters[0].Armed || statusRes.Heaters[0].Heater != "extruder" {
		t.Fatalf("status = %+v, want one armed extruder entry", statusRes.Heaters)
	}

	var disarmRes DisarmResult
	if err := conn.Call(ctx, MethodWatchdogDisarm, DisarmParams{Identity: "printer-x"}, &disarmRes); err != nil {
		t.Fatalf("disarm: %v", err)
	}
	if !disarmRes.OK {
		t.Fatalf("disarmRes = %+v, want OK", disarmRes)
	}
	if opts.Watchdog.ArmedCount() != 0 {
		t.Fatalf("ArmedCount = %d, want 0 after disarm", opts.Watchdog.ArmedCount())
	}
}

// An invalid arm request (an unknown heater) reaches the client as an
// error, not a silently-accepted no-op.
func TestServer_IPC_ArmRejectsInvalidRequest(t *testing.T) {
	opts := testOptions(t)
	s := openTestServer(t, opts)
	serveInBackground(t, s)

	conn, err := socket.Dial(opts.Paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var res ArmResult
	err = conn.Call(context.Background(), MethodWatchdogArm, ArmParams{Identity: "p", Heater: "bogus", TargetC: 1, ArmMinutes: 1}, &res)
	if err == nil {
		t.Fatal("want an error for an invalid heater, got nil")
	}
}

// A second Open against the same directory fails: single instance per user.
func TestServer_Open_SingleInstance(t *testing.T) {
	opts := testOptions(t)
	openTestServer(t, opts)

	_, err := Open(Options{Paths: opts.Paths, Hub: NewHub(&fakeOpener{}), Watchdog: newTestWatchdog(newFakeStateSource(), newFakeSender())})
	if err == nil {
		t.Fatal("want an error opening a second daemon against the same lock, got nil")
	}
}

// Once the first daemon closes, a second Open against the same directory
// succeeds (the lock was actually released, not merely reported free).
func TestServer_Open_ReleasesLockOnClose(t *testing.T) {
	opts := testOptions(t)
	first, err := Open(opts)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first.Close()

	secondHub := NewHub(&fakeOpener{})
	second, err := Open(Options{
		Paths:    opts.Paths,
		Hub:      secondHub,
		Watchdog: newTestWatchdog(newFakeStateSource(), newFakeSender()),
		Recorder: NewRecorder(filepath.Join(t.TempDir(), "recordings"), secondHub, newFakeStateSource(), nil),
	})
	if err != nil {
		t.Fatalf("second Open after Close: %v", err)
	}
	second.Close()
}

// Open requires a Hub, a Watchdog and a Recorder.
func TestServer_Open_RequiresHubAndWatchdog(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(Options{Paths: PathsIn(dir)}); err == nil {
		t.Fatal("want an error with no Hub/Watchdog, got nil")
	}
}

// The daemon exits on its own once idle (no camera connections, no armed
// watchdogs) for longer than IdleShutdown.
func TestServer_IdleSelfExit(t *testing.T) {
	opts := testOptions(t)
	opts.IdleShutdown = 30 * time.Millisecond
	s := openTestServer(t, opts)

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil (idle self-exit)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return within 2s of being idle")
	}
}

// An armed watchdog postpones idle self-exit; once it stands down (target
// changed under it, in this case) the idle clock starts and the daemon
// still exits.
func TestServer_IdleSelfExit_WaitsForArmedWatchdog(t *testing.T) {
	opts := testOptions(t)
	opts.IdleShutdown = 30 * time.Millisecond
	opts.Watchdog.minute = 20 * time.Millisecond
	s := openTestServer(t, opts)

	opts.Watchdog.direct.(*fakeStateSource).set("printer-y", snapshotEntry{ok: false}) // stands down at expiry
	if err := opts.Watchdog.Arm(ArmRequest{Identity: "printer-y", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: "printer-y", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within 3s once the watchdog stood down and the daemon went idle")
	}
}

func TestPathsIn_MatchesSocketPackageLayout(t *testing.T) {
	dir := t.TempDir()
	paths := PathsIn(dir)
	if paths.Socket != filepath.Join(dir, "daemon.sock") {
		t.Errorf("Socket = %q, want %q", paths.Socket, filepath.Join(dir, "daemon.sock"))
	}
	if paths.Lock != filepath.Join(dir, "lock") {
		t.Errorf("Lock = %q, want %q", paths.Lock, filepath.Join(dir, "lock"))
	}
}

// TestOpenClose_LongHomeNeverTouchesRealTempDir is dev_docs/review-backlog.md
// item 43's regression test: opening and closing a real daemon.Server whose
// paths come from DefaultPaths under a long home directory - long enough on
// its own to push the preferred "<home>/.creality-k2-mcp/daemon/daemon.sock"
// past maxSocketPathBytes, triggering daemonDir's fallback - must never
// create anything under the REAL os.TempDir(), only under FallbackRoot()
// (this package's TestMain, testmain_test.go, points that at a short,
// package-owned directory for exactly this reason). Before the fix,
// daemonDir's fallback was hardcoded to os.TempDir() itself, so any test
// whose HOME/USERPROFILE resolved to a long path - testing.T.TempDir()
// commonly does, since it embeds the full test name - silently created a
// creality-k2-mcp-<hash> directory in the real user's real temp directory,
// never cleaned up.
func TestOpenClose_LongHomeNeverTouchesRealTempDir(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users",
		strings.Repeat("a-very-long-domain-profile-directory-name-", 4))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	preferred := filepath.Join(home, appDirName, daemonSubdir)
	if fitsSocketPath(preferred) {
		t.Fatalf("test fixture home %q is not long enough to trigger the fallback", home)
	}

	realTemp := os.TempDir()
	before := crealityDirsForTest(realTemp)

	paths, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	if !strings.HasPrefix(paths.Dir, FallbackRoot()) {
		t.Fatalf("DefaultPaths().Dir = %q, want it under FallbackRoot() (%q): the fallback did not trigger", paths.Dir, FallbackRoot())
	}

	opts := testOptions(t)
	opts.Paths = paths
	s := openTestServer(t, opts)
	s.Close()

	after := crealityDirsForTest(realTemp)
	if leaked := newEntriesForTest(before, after); len(leaked) > 0 {
		t.Fatalf("Open/Close under a long home created new creality-k2-mcp-* director(ies) in the real OS temp dir %s: %v", realTemp, leaked)
	}
}
