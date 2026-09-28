// Package client implements internal/policy.Watchdog over the background
// daemon's IPC socket (internal/daemon), autostarting the daemon on first
// need so idle heating works without the user having to run anything by
// hand (dev_docs/safety-architecture.md section 10 D2).
//
// Autostart is guarded so it can never fire from a test process (AGENTS.md's
// hard testing rule that no test may reach lock.EnsureRunning or exec the
// product): canAutostart refuses whenever this process's own executable path
// looks like a Go test binary (base name ending in ".test" or ".test.exe",
// case-insensitive) or the environment sets K2_MCP_NO_AUTOSTART=1. See
// isTestBinaryOrGuarded and dev_docs/review-backlog.md item 42.
package client

import (
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sairaph/mcp-wizard/daemon/lock"
	"github.com/sairaph/mcp-wizard/daemon/socket"

	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/daemon"
	"github.com/sairaph/creality_k2_mcp/internal/policy"
)

// noAutostartEnv is an explicit escape hatch: any process (test or not) that
// sets K2_MCP_NO_AUTOSTART=1 in its environment never runs lock.EnsureRunning
// from this package, regardless of what New resolved this process's own
// executable path to be (dev_docs/review-backlog.md item 42). A test's own
// TestMain sets this alongside relying on the automatic .test/.test.exe
// suffix guard below, so a Client built any way inside that test binary -
// including via New with no options - can never spawn the real daemon.
const noAutostartEnv = "K2_MCP_NO_AUTOSTART"

// isTestBinaryOrGuarded reports whether executable must never be used to
// autostart the daemon: either it looks like the binary `go test` builds (a
// base name ending in ".test" or ".test.exe", case-insensitive - the actual
// executable every _test.go file in this repo runs inside, per AGENTS.md's
// hard testing rule that no test may reach lock.EnsureRunning) or the
// process has opted out via K2_MCP_NO_AUTOSTART=1. Matching is by suffix
// only (not an exact "== .test"), since `go test -o` and IDE test runners
// often add a package- or dir-derived prefix before it.
func isTestBinaryOrGuarded(executable string) bool {
	if os.Getenv(noAutostartEnv) == "1" {
		return true
	}
	base := strings.ToLower(filepath.Base(executable))
	return strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe")
}

// aliveTimeout bounds a single ping over the socket, so a hung or
// half-dead daemon cannot make policy.Watchdog.Alive block the caller.
const aliveTimeout = 2 * time.Second

// autostartArgs is what EnsureRunning execs: the hidden `camera serve`
// subcommand (dev_docs/plan-v0.1.0.md T11a).
var autostartArgs = []string{"camera", "serve"}

// Client is the production policy.Watchdog: every call dials the daemon's
// socket fresh (matching daemon/socket.Client's own "re-dial after any
// failure" contract) rather than holding a long-lived connection, since a
// tool call happens far less often than the daemon might restart.
type Client struct {
	paths       daemon.Paths
	executable  string // this binary's own path, for autostart; "" disables it
	noAutostart bool   // set by WithoutAutostart; see canAutostart
}

// Option configures New. The zero value of Client (no options) is the
// normal, autostarting production client; WithoutAutostart is the only
// option today, for read-only callers (doctorchecks, get_printer_status's
// watchdog status path) that must never start the daemon as a side effect
// of a status probe (dev_docs/review-backlog.md item 29).
type Option func(*Client)

// WithoutAutostart makes every probe on the returned Client (Alive,
// ViewerURL, StartRecording, ListRecordings, DeleteRecording) purely
// read-only: none of them will ever run lock.EnsureRunning to start the
// daemon, regardless of this process's own executable path. Arm, Disarm and
// Status never autostart in the first place (see their own doc comments),
// so this option does not change their behaviour.
func WithoutAutostart() Option {
	return func(c *Client) { c.noAutostart = true }
}

// New resolves the daemon's paths and this process's own executable path
// (for autostart) and returns a ready Client. It does not contact the
// daemon or start one.
func New(opts ...Option) (*Client, error) {
	paths, err := daemon.DefaultPaths()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "" // autostart becomes a no-op; Alive still probes a running daemon
	}
	c := &Client{paths: paths, executable: exe}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

var _ policy.Watchdog = (*Client)(nil)

// canAutostart reports whether a failed probe below may fall back to
// starting the daemon: only when this Client was not built with
// WithoutAutostart, this process knows its own executable path, and
// isTestBinaryOrGuarded says that path (or the environment) does not rule
// autostart out entirely (dev_docs/review-backlog.md item 42). This last
// check is what makes it safe to call New() with no options from inside a
// test binary at all: it still returns a Client, but one that can never
// autostart, rather than requiring every test to remember WithoutAutostart.
func (c *Client) canAutostart() bool {
	if c.noAutostart || c.executable == "" {
		return false
	}
	return !isTestBinaryOrGuarded(c.executable)
}

// dial verifies the daemon directory (the same check daemon.Open runs
// before it trusts that directory, dev_docs/review-backlog.md item 30) and
// then dials the socket inside it. A directory that fails the check (not a
// plain directory, or on Unix not owned by us at mode 0700) is refused
// before a connection is even attempted, so a hostile pre-created directory
// in a shared temp fallback path can never be dialed into.
func (c *Client) dial() (*socket.Client, error) {
	if err := daemon.VerifyDir(c.paths.Dir); err != nil {
		return nil, err
	}
	return socket.Dial(c.paths.Socket)
}

// Alive reports whether the daemon is reachable right now, autostarting it
// (detached, matching interactive-terminal-mcp/sana-mcp's own pattern) if a
// first ping fails, this process knows its own executable path, and this
// Client was not built with WithoutAutostart.
func (c *Client) Alive(ctx context.Context) bool {
	if c.ping(ctx) {
		return true
	}
	if !c.canAutostart() {
		return false
	}
	lock.EnsureRunning(c.executable, autostartArgs, lock.Options{
		LockFile: c.paths.Lock,
		LogFile:  c.paths.Log,
	})
	return c.ping(ctx)
}

// Ping performs one non-autostarting round trip to the daemon's MethodPing,
// bounded by aliveTimeout, regardless of how this Client was built: it never
// runs lock.EnsureRunning. This is the probe doctorchecks and any other
// read-only caller should use to learn the daemon's pid and start time
// without risking a start-as-a-side-effect (dev_docs/review-backlog.md item
// 29); ok is false for any dial or call failure, most commonly "the daemon
// is not running".
func (c *Client) Ping(ctx context.Context) (result daemon.PingResult, ok bool) {
	pingCtx, cancel := context.WithTimeout(ctx, aliveTimeout)
	defer cancel()

	conn, err := c.dial()
	if err != nil {
		return daemon.PingResult{}, false
	}
	defer conn.Close()

	var res daemon.PingResult
	if err := conn.Call(pingCtx, daemon.MethodPing, nil, &res); err != nil {
		return daemon.PingResult{}, false
	}
	return res, res.OK
}

// ping is Alive's own probe: Ping's ok result alone, never the full
// PingResult Alive has no use for.
func (c *Client) ping(ctx context.Context) bool {
	_, ok := c.Ping(ctx)
	return ok
}

// Arm implements policy.Watchdog: it never autostarts on its own (Execute
// always calls Alive first per dev_docs/safety-architecture.md section 10
// D2, and armIdleHeatWatchdog refuses outright when Alive is false, so by
// the time Arm is called the daemon is already known to be up).
func (c *Client) Arm(ctx context.Context, req policy.IdleHeatArmRequest) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	params := daemon.ArmParams{
		Identity: req.Identity, Heater: req.Heater, TargetC: req.TargetC, ArmMinutes: req.ArmMinutes,
		Host: req.Host, MoonrakerPort: req.MoonrakerPort, APIKey: req.APIKey,
	}
	var res daemon.ArmResult
	return conn.Call(ctx, daemon.MethodWatchdogArm, params, &res)
}

// Disarm implements the Watchdog interface's D2 cancel hook. It is
// best-effort and never blocks start_print on a daemon problem: if the
// daemon is not running there is nothing armed to cancel (Arm always
// requires Alive first), so a dial failure here is treated as success, not
// an error worth surfacing to a print that is starting.
func (c *Client) Disarm(ctx context.Context, identity string) error {
	conn, err := c.dial()
	if err != nil {
		return nil
	}
	defer conn.Close()

	var res daemon.DisarmResult
	return conn.Call(ctx, daemon.MethodWatchdogDisarm, daemon.DisarmParams{Identity: identity}, &res)
}

// ViewerURL returns the local browser camera viewer URL for printerID
// (empty shows every enabled printer), autostarting the daemon (see Alive)
// if a first attempt cannot reach it, then trying once more. This is the
// production seam behind open_camera_view (dev_docs/plan-v0.1.0.md T11b):
// mcpserver never talks to the daemon's socket directly.
func (c *Client) ViewerURL(ctx context.Context, printerID string) (string, error) {
	u, err := c.viewerURLOnce(ctx, printerID)
	if err == nil {
		return u, nil
	}
	if !c.canAutostart() {
		return "", err
	}
	lock.EnsureRunning(c.executable, autostartArgs, lock.Options{
		LockFile: c.paths.Lock,
		LogFile:  c.paths.Log,
	})
	return c.viewerURLOnce(ctx, printerID)
}

func (c *Client) viewerURLOnce(ctx context.Context, printerID string) (string, error) {
	conn, err := c.dial()
	if err != nil {
		return "", err
	}
	defer conn.Close()

	var res daemon.ViewerURLResult
	if err := conn.Call(ctx, daemon.MethodViewerURL, daemon.ViewerURLParams{PrinterID: printerID}, &res); err != nil {
		return "", err
	}
	return res.URL, nil
}

// StartRecording begins a new recording (T11c), autostarting the daemon
// (see Alive) if a first attempt cannot reach it, then trying once more.
func (c *Client) StartRecording(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error) {
	info, err := c.startRecordingOnce(ctx, req)
	if err == nil {
		return info, nil
	}
	if !c.canAutostart() {
		return daemon.RecordingInfo{}, err
	}
	lock.EnsureRunning(c.executable, autostartArgs, lock.Options{
		LockFile: c.paths.Lock,
		LogFile:  c.paths.Log,
	})
	return c.startRecordingOnce(ctx, req)
}

func (c *Client) startRecordingOnce(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error) {
	conn, err := c.dial()
	if err != nil {
		return daemon.RecordingInfo{}, err
	}
	defer conn.Close()

	var res daemon.RecordingStartResult
	if err := conn.Call(ctx, daemon.MethodRecordingStart, req, &res); err != nil {
		return daemon.RecordingInfo{}, err
	}
	return res.Recording, nil
}

// StopRecording stops the active recording id and returns its final
// metadata. It does not autostart the daemon: if the daemon is not
// running, no recording is active either, so there is nothing to stop.
func (c *Client) StopRecording(ctx context.Context, id string) (daemon.RecordingInfo, error) {
	conn, err := c.dial()
	if err != nil {
		return daemon.RecordingInfo{}, err
	}
	defer conn.Close()

	var res daemon.RecordingStopResult
	if err := conn.Call(ctx, daemon.MethodRecordingStop, daemon.RecordingStopParams{ID: id}, &res); err != nil {
		return daemon.RecordingInfo{}, err
	}
	return res.Recording, nil
}

// ListRecordings lists every recording the daemon knows about (active plus
// completed, from disk), autostarting the daemon if it is not already
// running: a freshly started daemon still finds every completed recording
// by scanning the recordings directory, so this is never wasted work.
func (c *Client) ListRecordings(ctx context.Context) (daemon.RecordingListResult, error) {
	res, err := c.listRecordingsOnce(ctx)
	if err == nil {
		return res, nil
	}
	if !c.canAutostart() {
		return daemon.RecordingListResult{}, err
	}
	lock.EnsureRunning(c.executable, autostartArgs, lock.Options{
		LockFile: c.paths.Lock,
		LogFile:  c.paths.Log,
	})
	return c.listRecordingsOnce(ctx)
}

func (c *Client) listRecordingsOnce(ctx context.Context) (daemon.RecordingListResult, error) {
	conn, err := c.dial()
	if err != nil {
		return daemon.RecordingListResult{}, err
	}
	defer conn.Close()

	var res daemon.RecordingListResult
	if err := conn.Call(ctx, daemon.MethodRecordingList, daemon.RecordingListParams{}, &res); err != nil {
		return daemon.RecordingListResult{}, err
	}
	return res, nil
}

// DeleteRecording removes a completed recording's files, autostarting the
// daemon for the same reason ListRecordings does (deletion only ever reads
// and removes files on disk, never anything a fresh daemon would not see).
func (c *Client) DeleteRecording(ctx context.Context, id string) error {
	err := c.deleteRecordingOnce(ctx, id)
	if err == nil {
		return nil
	}
	if !c.canAutostart() {
		return err
	}
	lock.EnsureRunning(c.executable, autostartArgs, lock.Options{
		LockFile: c.paths.Lock,
		LogFile:  c.paths.Log,
	})
	return c.deleteRecordingOnce(ctx, id)
}

func (c *Client) deleteRecordingOnce(ctx context.Context, id string) error {
	conn, err := c.dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	var res daemon.RecordingDeleteResult
	return conn.Call(ctx, daemon.MethodRecordingDelete, daemon.RecordingDeleteParams{ID: id}, &res)
}

// Snapshot captures one decoded camera frame from host through the
// daemon's hub (review backlog item 51: the rolling GOP buffer / keep-warm
// snapshot path, MethodCameraSnapshot), autostarting the daemon (see
// Alive) if a first attempt cannot reach it, then trying once more. This
// is the one path get_camera_snapshot (internal/mcpserver), the CLI
// "snapshot" command and the TUI's camera screen all use - there is no
// other, direct-WebRTC fallback for any of them any more (owner design
// decision, review backlog item 51: "stream at the camera's own rate when
// needed and take snapshots from the stream").
func (c *Client) Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error) {
	result, err := c.snapshotOnce(ctx, host)
	if err == nil {
		return result, nil
	}
	if !c.canAutostart() {
		return nil, err
	}
	lock.EnsureRunning(c.executable, autostartArgs, lock.Options{
		LockFile: c.paths.Lock,
		LogFile:  c.paths.Log,
	})
	return c.snapshotOnce(ctx, host)
}

func (c *Client) snapshotOnce(ctx context.Context, host string) (*camera.SnapshotResult, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var res daemon.CameraSnapshotResult
	if err := conn.Call(ctx, daemon.MethodCameraSnapshot, daemon.CameraSnapshotParams{Host: host}, &res); err != nil {
		return nil, err
	}
	img, err := jpeg.Decode(bytes.NewReader(res.ImageJPEG))
	if err != nil {
		return nil, fmt.Errorf("daemon: decode camera.snapshot image: %w", err)
	}
	return &camera.SnapshotResult{
		Image:      img,
		CapturedAt: res.CapturedAt,
		Width:      res.Width,
		Height:     res.Height,
	}, nil
}

// Status fetches watchdog status for identity (armed heaters, deadlines,
// last action) for display, e.g. by get_printer_status. It is the
// WatchdogStatus probe dev_docs/review-backlog.md item 29 asks for: it never
// autostarts the daemon regardless of how this Client was built, and returns
// ok=false when the daemon cannot be reached, never a fabricated empty
// status.
func (c *Client) Status(ctx context.Context, identity string) (heaters []daemon.HeaterStatus, ok bool) {
	conn, err := c.dial()
	if err != nil {
		return nil, false
	}
	defer conn.Close()

	var res daemon.StatusResult
	if err := conn.Call(ctx, daemon.MethodWatchdogStatus, daemon.StatusParams{Identity: identity}, &res); err != nil {
		return nil, false
	}
	return res.Heaters, true
}

// CameraStatus fetches the hub's live camera connection state for host, for
// get_printer_status to surface alongside 9999's own camera flags (review
// backlog item 47). Like Status, it never autostarts the daemon regardless
// of how this Client was built, and reports ok=false (never a fabricated
// "not connected") when the daemon cannot be reached.
func (c *Client) CameraStatus(ctx context.Context, host string) (lastMediaAt time.Time, hasMedia bool, connected bool, ok bool) {
	conn, err := c.dial()
	if err != nil {
		return time.Time{}, false, false, false
	}
	defer conn.Close()

	var res daemon.CameraStatusResult
	if err := conn.Call(ctx, daemon.MethodCameraStatus, daemon.CameraStatusParams{Host: host}, &res); err != nil {
		return time.Time{}, false, false, false
	}
	return res.LastMediaAt, res.HasMedia, res.Connected, true
}
