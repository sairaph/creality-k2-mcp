package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sairaph/mcp-wizard/daemon/socket"
)

// ErrAlreadyRunning means another daemon already holds the lock. It wraps
// whatever error mcp-wizard's daemon/socket.Server.Open returns for that
// case (it does not export a sentinel of its own), matched by
// errors.Is/text below only where callers actually branch on it; today no
// caller in this package does.
var ErrAlreadyRunning = errors.New("daemon: a camera/watchdog daemon is already running")

// IdleShutdownAfter is how long the daemon waits with no open camera
// connections (viewers or recordings) and no armed watchdogs before exiting
// on its own (dev_docs/plan-v0.1.0.md T11a).
const IdleShutdownAfter = 10 * time.Minute

// idlePollIntervalMax bounds how often Serve checks the idle condition; the
// actual interval is a fraction of idleShutdown (see Server.idlePollInterval)
// so a test-shrunk IdleShutdown does not have to wait out a fixed 5s tick.
const idlePollIntervalMax = 5 * time.Second
const idlePollIntervalMin = 10 * time.Millisecond

// logMaxBytes caps daemon.log; once a fresh Open finds it over this size,
// the previous contents are kept as one rotated generation (daemon.log.1)
// and a fresh file is started, so the log never grows without bound.
const logMaxBytes = 5 * 1024 * 1024

// Options configures Open. Hub, Watchdog and Recorder have no usable
// default (a nil one makes Open fail): production callers build them with
// NewProductionOptions; tests build them from fakes.
type Options struct {
	Paths        Paths
	Hub          *Hub
	Watchdog     *Watchdog
	Recorder     *Recorder
	IdleShutdown time.Duration // defaults to IdleShutdownAfter when zero
	// Printers lists the enabled printers the camera viewer HTTP server
	// (T11b) may show. Optional: a nil Printers still lets the viewer's
	// base URL and access token work (viewer.url), but GET / and GET
	// /api/printers report an empty printer list, and GET /stream/... never
	// resolves any printer id. NewProductionOptions sets this to the real
	// printer registry.
	Printers PrinterLister
}

// Server is the background camera/idle-heat daemon: one process per user,
// reached over a local socket (github.com/sairaph/mcp-wizard/daemon/socket,
// AF_UNIX, which works on Windows 10+), owning a camera Hub, an idle-heat
// Watchdog and a recording Recorder (T11c).
type Server struct {
	paths Paths

	sock   *socket.Server
	hub    *Hub
	wd     *Watchdog
	rec    *Recorder
	viewer *viewerServer

	logFile *os.File
	log     *log.Logger

	startedAt time.Time

	idleShutdown time.Duration
}

// Open acquires the singleton daemon lock, binds the socket, opens the
// (rotated) log and writes the pid file. It returns an error - wrapping
// whatever socket.Server.Open reports - if another daemon already holds the
// lock.
func Open(opts Options) (*Server, error) {
	if opts.Hub == nil || opts.Watchdog == nil || opts.Recorder == nil {
		return nil, fmt.Errorf("daemon: Open requires a non-nil Hub, Watchdog and Recorder")
	}
	paths := opts.Paths
	if paths.Dir == "" {
		return nil, fmt.Errorf("daemon: Open requires Paths.Dir")
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create %s: %w", paths.Dir, err)
	}
	// VerifyDir (item 30) must run right after MkdirAll and before anything
	// else touches paths.Dir: MkdirAll is a no-op when the directory already
	// exists, so a directory some other local account pre-created in the
	// shared os.TempDir fallback path (daemonDir) would otherwise be trusted
	// silently. Open must stay single-threaded from here through the
	// umask-guarded socket.Open call below - nothing else in this process may
	// create or modify paths.Dir concurrently, or the ownership/mode this
	// check just established could already be stale by the time the socket
	// is created inside it.
	if err := VerifyDir(paths.Dir); err != nil {
		return nil, fmt.Errorf("daemon: refusing to start: %w", err)
	}

	sock := socket.New(paths.Dir, socketName)
	// The umask/chmod guard (item 22) wraps Open itself: the socket file is
	// created inside Open, and the window between creation and a later
	// chmod would otherwise leave it briefly at whatever the ambient umask
	// allows.
	if err := withRestrictedSocketUmask(sock.Open); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAlreadyRunning, err)
	}
	if err := restrictSocketPermissions(sock.SocketPath()); err != nil {
		sock.Close()
		return nil, fmt.Errorf("daemon: restrict socket permissions: %w", err)
	}

	logFile, err := openRotatedLog(paths.Log)
	if err != nil {
		sock.Close()
		return nil, err
	}

	if err := os.WriteFile(paths.PID, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		logFile.Close()
		sock.Close()
		return nil, fmt.Errorf("daemon: write pid file: %w", err)
	}

	idleShutdown := opts.IdleShutdown
	if idleShutdown <= 0 {
		idleShutdown = IdleShutdownAfter
	}

	s := &Server{
		paths:        paths,
		sock:         sock,
		hub:          opts.Hub,
		wd:           opts.Watchdog,
		rec:          opts.Recorder,
		logFile:      logFile,
		log:          log.New(logFile, "", log.LstdFlags|log.LUTC),
		startedAt:    time.Now(),
		idleShutdown: idleShutdown,
	}
	// Wire camera session lifecycle events (open/close, PLI sent, keyframe
	// received, errors) into this daemon's own log (review backlog item
	// 46). Hub was built earlier, in NewProductionOptions, before this log
	// file existed; SetLogger is a no-op for a test's fake opener.
	s.hub.SetLogger(newDaemonCameraLogger(s.log))
	s.viewer = newViewerServer(s.hub, opts.Printers, s.log)
	s.registerHandlers()
	s.log.Printf("daemon started, pid %d, socket %s", os.Getpid(), sock.SocketPath())
	return s, nil
}

// openRotatedLog opens path for appending, first rotating it to path+".1"
// (overwriting any previous generation) if it is already over logMaxBytes.
func openRotatedLog(path string) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > logMaxBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open log: %w", err)
	}
	return f, nil
}

func (s *Server) registerHandlers() {
	s.sock.Handle(MethodPing, s.handlePing)
	s.sock.Handle(MethodWatchdogArm, s.handleArm)
	s.sock.Handle(MethodWatchdogDisarm, s.handleDisarm)
	s.sock.Handle(MethodWatchdogStatus, s.handleStatus)
	s.sock.Handle(MethodViewerURL, s.handleViewerURL)
	s.sock.Handle(MethodCameraStatus, s.handleCameraStatus)
	s.sock.Handle(MethodCameraSnapshot, s.handleCameraSnapshot)
	s.sock.Handle(MethodRecordingStart, s.handleRecordingStart)
	s.sock.Handle(MethodRecordingStop, s.handleRecordingStop)
	s.sock.Handle(MethodRecordingList, s.handleRecordingList)
	s.sock.Handle(MethodRecordingDelete, s.handleRecordingDelete)
}

func (s *Server) handlePing(ctx context.Context, params json.RawMessage) (any, error) {
	return PingResult{OK: true, PID: os.Getpid(), StartedAt: s.startedAt}, nil
}

func (s *Server) handleArm(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ArmParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode arm params: %w", err)
	}
	if err := s.wd.Arm(ArmRequest{
		Identity: p.Identity, Heater: p.Heater, TargetC: p.TargetC, ArmMinutes: p.ArmMinutes,
		Host: p.Host, MoonrakerPort: p.MoonrakerPort, APIKey: p.APIKey,
	}); err != nil {
		return nil, err
	}
	// Never log p.APIKey or p.Host (review backlog item 36): the arm log
	// line stays limited to exactly what it logged before this fix.
	s.log.Printf("watchdog armed: identity=%s heater=%s target_c=%g arm_minutes=%d", p.Identity, p.Heater, p.TargetC, p.ArmMinutes)
	return ArmResult{OK: true}, nil
}

func (s *Server) handleDisarm(ctx context.Context, raw json.RawMessage) (any, error) {
	var p DisarmParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode disarm params: %w", err)
	}
	s.wd.Disarm(p.Identity)
	s.log.Printf("watchdog disarmed: identity=%s", p.Identity)
	return DisarmResult{OK: true}, nil
}

func (s *Server) handleStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var p StatusParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode status params: %w", err)
	}
	return StatusResult{Heaters: s.wd.Status(p.Identity)}, nil
}

// handleViewerURL answers MethodViewerURL (T11b): the local browser viewer's
// URL, starting the viewer HTTP server lazily on first call.
func (s *Server) handleViewerURL(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ViewerURLParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode viewer.url params: %w", err)
	}
	u, err := s.viewer.URL(p.PrinterID)
	if err != nil {
		return nil, fmt.Errorf("start the camera viewer: %w", err)
	}
	s.log.Printf("viewer.url served (printer_id=%q)", p.PrinterID)
	return ViewerURLResult{URL: u}, nil
}

// handleCameraStatus answers MethodCameraStatus (review backlog item 47):
// the hub's live connection state for one printer's camera host, never
// starting a connection just to answer the query.
func (s *Server) handleCameraStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var p CameraStatusParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode camera.status params: %w", err)
	}
	at, hasMedia, connected := s.hub.LastMediaAt(p.Host)
	return CameraStatusResult{Connected: connected, HasMedia: hasMedia, LastMediaAt: at}, nil
}

// cameraSnapshotWireJPEGQuality is the JPEG quality handleCameraSnapshot
// encodes the decoded frame at before sending it over the socket. High,
// not maximal: the image travels the wire once and is then re-encoded by
// the caller anyway (internal/mcpserver/tools_camera.go's fitJPEG,
// internal/clicmd/snapshot.go's own JPEG write), so this only needs to
// avoid throwing away detail those callers might still want, not to be
// lossless.
const cameraSnapshotWireJPEGQuality = 95

// handleCameraSnapshot answers MethodCameraSnapshot (review backlog item
// 51): captures the latest decodable picture from the hub's rolling GOP
// buffer for p.Host (opening the upstream connection on demand, keeping it
// warm afterward - see Hub.Snapshot) and returns it JPEG-encoded.
func (s *Server) handleCameraSnapshot(ctx context.Context, raw json.RawMessage) (any, error) {
	var p CameraSnapshotParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode camera.snapshot params: %w", err)
	}
	if strings.TrimSpace(p.Host) == "" {
		return nil, fmt.Errorf("daemon: camera.snapshot requires host")
	}

	if p.BudgetMs < 0 {
		return nil, fmt.Errorf("daemon: camera.snapshot budget_ms must not be negative")
	}
	result, err := s.hub.Snapshot(ctx, p.Host, time.Duration(p.BudgetMs)*time.Millisecond)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, result.Image, &jpeg.Options{Quality: cameraSnapshotWireJPEGQuality}); err != nil {
		return nil, fmt.Errorf("daemon: encode snapshot image: %w", err)
	}

	s.log.Printf("camera snapshot served: host=%s %dx%d bytes=%d", p.Host, result.Width, result.Height, buf.Len())
	return CameraSnapshotResult{
		ImageJPEG:  buf.Bytes(),
		Width:      result.Width,
		Height:     result.Height,
		CapturedAt: result.CapturedAt,
	}, nil
}

// handleRecordingStart answers MethodRecordingStart (T11c).
func (s *Server) handleRecordingStart(ctx context.Context, raw json.RawMessage) (any, error) {
	var p RecordingStartParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode recording.start params: %w", err)
	}
	req := StartRecordingRequest{
		PrinterID:   p.PrinterID,
		Host:        p.Host,
		Identity:    p.Identity,
		Mode:        RecordMode(p.Mode),
		Until:       RecordUntil(p.Until),
		MaxDuration: maxDurationFrom(p.MaxDurationSeconds),
	}
	info, err := s.rec.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	return RecordingStartResult{Recording: info}, nil
}

// handleRecordingStop answers MethodRecordingStop.
func (s *Server) handleRecordingStop(ctx context.Context, raw json.RawMessage) (any, error) {
	var p RecordingStopParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode recording.stop params: %w", err)
	}
	info, err := s.rec.Stop(p.ID)
	if err != nil {
		return nil, err
	}
	s.log.Printf("recording stop requested: id=%s reason=%q", info.ID, info.StopReason)
	return RecordingStopResult{Recording: info}, nil
}

// handleRecordingList answers MethodRecordingList.
func (s *Server) handleRecordingList(ctx context.Context, raw json.RawMessage) (any, error) {
	res, err := s.rec.List()
	if err != nil {
		return nil, err
	}
	return RecordingListResult{Recordings: res.Recordings, DiskUsageBytes: res.DiskUsageBytes}, nil
}

// handleRecordingDelete answers MethodRecordingDelete.
func (s *Server) handleRecordingDelete(ctx context.Context, raw json.RawMessage) (any, error) {
	var p RecordingDeleteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode recording.delete params: %w", err)
	}
	if err := s.rec.Delete(p.ID); err != nil {
		return nil, err
	}
	s.log.Printf("recording deleted: id=%s", p.ID)
	return RecordingDeleteResult{OK: true}, nil
}

// Serve accepts connections until ctx is cancelled, a client asks the
// daemon to stop, or the idle self-exit check fires. It returns once every
// in-flight handler has finished.
func (s *Server) Serve(ctx context.Context) error {
	idleCtx, cancelIdle := context.WithCancel(ctx)
	defer cancelIdle()
	go s.watchIdle(idleCtx)

	return s.sock.Serve(ctx)
}

// watchIdle stops the daemon once it has had no open camera connections
// (viewers or recordings) and no armed watchdogs for idleShutdown, per
// dev_docs/plan-v0.1.0.md T11a's "idle self-exit when there are no viewers,
// recordings or armed watchdogs for 10 minutes".
func (s *Server) watchIdle(ctx context.Context) {
	if s.idleShutdown <= 0 {
		return
	}
	ticker := time.NewTicker(s.idlePollInterval())
	defer ticker.Stop()

	idleSince := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.hub.ActiveCount() > 0 || s.wd.ArmedCount() > 0 {
				idleSince = time.Now()
				continue
			}
			if time.Since(idleSince) >= s.idleShutdown {
				s.log.Printf("idle for %s with no camera connections or armed watchdogs; exiting", s.idleShutdown)
				s.sock.Close() // unblocks Serve; Close (below) does the rest
				return
			}
		}
	}
}

// idlePollInterval is a tenth of idleShutdown, clamped to
// [idlePollIntervalMin, idlePollIntervalMax], so a small (test-shrunk)
// IdleShutdown is still checked promptly instead of waiting for a fixed
// multi-second tick.
func (s *Server) idlePollInterval() time.Duration {
	d := s.idleShutdown / 10
	if d < idlePollIntervalMin {
		d = idlePollIntervalMin
	}
	if d > idlePollIntervalMax {
		d = idlePollIntervalMax
	}
	return d
}

// Close releases the socket, the daemon lock, every open camera connection
// and the pid file, and closes the log. Safe to call more than once.
func (s *Server) Close() {
	s.viewer.stop()
	s.hub.Close()
	s.sock.Close()
	os.Remove(s.paths.PID)
	if s.logFile != nil {
		s.log.Printf("daemon stopped")
		s.logFile.Close()
	}
}

// Alive reports whether this Server can currently answer, for a same
// process caller (a client over the socket uses MethodPing instead).
func (s *Server) Alive() bool { return true }
