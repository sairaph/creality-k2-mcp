// This file implements the recorder: T11c's fragmented MP4 recordings and
// T11d's per-layer JPEG timelapse stills, both written to disk from a Hub
// subscription, with a JSON sidecar per recording, disk safety checks and
// automatic stop at print end. It is the daemon-side component
// start_recording/stop_recording/list_recordings/delete_recording
// (internal/mcpserver/tools_recording.go) talk to through the RPC methods
// in recorder_rpc.go and daemon.go's handlers.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	h264c "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/camera/fmp4"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// RecordMode is what a recording captures: continuous fragmented MP4
// (RecordModeVideo) or one JPEG still per printer layer change
// (RecordModeTimelapse, T11d, decision 11).
type RecordMode string

const (
	RecordModeVideo     RecordMode = "video"
	RecordModeTimelapse RecordMode = "timelapse"
)

// RecordUntil says when a recording stops on its own, beyond an explicit
// Recorder.Stop call or a disk-safety/max-duration limit.
type RecordUntil string

const (
	// RecordUntilPrintEnd polls the printer's state every
	// recorderStatePollInterval and stops once the job ends (complete,
	// cancelled, error, or idle after having been seen printing). A poll
	// that cannot reach the printer at all (StateSource.Snapshot returns
	// ok=false: server/info unreachable, a short network blip, etc.) is
	// never treated as the print having ended: run's pollC case simply
	// continues on the next tick rather than stopping, so a transient
	// network problem never truncates a print_end recording early. The
	// recording still has two independent backstops that do not depend on
	// reachability at all: MaxDuration (recorderDefaultMaxDuration if unset)
	// and the disk-safety check, either of which stops it and records the
	// reason ("stopped: reached the maximum recording duration ..." or the
	// disk-safety message) in RecordingInfo.StopReason, persisted to the
	// sidecar JSON by finish/writeSidecar, exactly like every other stop
	// reason (dev_docs/review-backlog.md item 35).
	RecordUntilPrintEnd RecordUntil = "print_end"
	// RecordUntilStopped only ever stops via an explicit Stop call, the
	// configured max duration, or a disk-safety limit.
	RecordUntilStopped RecordUntil = "stopped"
)

// recorderDefaultMaxDuration is used when StartRecordingRequest.MaxDuration
// is zero and Mode is video (dev_docs T11c: "optional max_duration default
// 12 h").
const recorderDefaultMaxDuration = 12 * time.Hour

// recorderDefaultTimelapseMaxDuration is used when
// StartRecordingRequest.MaxDuration is zero and Mode is timelapse
// (dev_docs T11d: "max_duration default 48 h for timelapse").
const recorderDefaultTimelapseMaxDuration = 48 * time.Hour

// recorderTimelapseJPEGQuality is the JPEG quality T11d encodes every
// timelapse still at (dev_docs T11d: "JPEG quality 85, full resolution").
const recorderTimelapseJPEGQuality = 85

// recorderMinFreeBytesToStart and recorderMinFreeBytesToStop are T11c's
// disk-safety thresholds: refuse to start below 1 GiB free, stop an active
// recording once free space falls below 512 MiB.
const (
	recorderMinFreeBytesToStart uint64 = 1 << 30
	recorderMinFreeBytesToStop  uint64 = 512 << 20
)

// RecorderKeyframeWait is how long a recording start waits for the camera's
// first keyframe. 25s, not a shorter value: live testing against a real K2
// observed cold-connect latency (session open to first usable keyframe) up to
// 16s, so a shorter wait cuts off recordings that would have started
// successfully (dev_docs/t11e-soak-report.md, dev_docs/camera-keyframe-rca.md).
const RecorderKeyframeWait = 25 * time.Second

// Package variables, not constants, so a test can shrink them instead of
// waiting out real multi-second/minute timings (the same pattern
// camera_hub.go's reconnectBackoffMin/Max and viewer_stream.go's
// streamFragmentDuration already use).
var (
	recorderStatePollInterval = 2 * time.Second
	recorderDiskCheckInterval = 30 * time.Second
	// recorderKeyframeWait is RecorderKeyframeWait, shrinkable by a test.
	recorderKeyframeWait     = RecorderKeyframeWait
	recorderFragmentDuration = fmp4.DefaultFragmentDuration
	recorderMaxKeyframeWait  = fmp4.DefaultMaxKeyframeWait
	recorderMaxBufferedBytes = fmp4.DefaultMaxBufferedBytes
	// recorderTimelapseFallbackInterval is T11d's fallback capture cadence
	// when neither print_stats.info.current_layer nor virtual_sdcard.layer
	// is known: one still every this-long, recorded in the sidecar as
	// interval_mode "time" (dev_docs plan-v0.1.0.md decision 11).
	recorderTimelapseFallbackInterval = 30 * time.Second
)

// RecordingPart is one fragmented MP4 file within a recording: the first
// part is named "<id-base>.mp4"; a parameter change or a camera reconnect
// (a PTS discontinuity) closes the current part and opens
// "<id-base>-partN.mp4" for N = 2, 3, ...
type RecordingPart struct {
	Path      string    `json:"path"`
	StartedAt time.Time `json:"started_at"`
	StoppedAt time.Time `json:"stopped_at,omitempty"`
	Bytes     int64     `json:"bytes"`
	// EndReason says why this part ended: empty while still being
	// written, otherwise e.g. "parameters changed" or "camera
	// reconnected".
	EndReason string `json:"end_reason,omitempty"`
}

// TimelapseFrame is one JPEG still within a timelapse recording (T11d).
// Layer is the printer layer number that triggered this capture, or nil
// when it was captured on the time-interval fallback instead (see
// RecordingInfo.IntervalMode).
type TimelapseFrame struct {
	Path       string    `json:"path"`
	Layer      *int      `json:"layer,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
	Bytes      int64     `json:"bytes"`
}

// RecordingInfo is one recording's metadata: the daemon's in-memory view
// while active, and a JSON sidecar file (<id-base>.json, next to the parts
// or the timelapse frames directory) once it stops, for
// list_recordings/delete_recording.
type RecordingInfo struct {
	ID          string        `json:"id"`
	PrinterID   string        `json:"printer_id"`
	Mode        RecordMode    `json:"mode"`
	Until       RecordUntil   `json:"until"`
	MaxDuration time.Duration `json:"max_duration"`
	StartedAt   time.Time     `json:"started_at"`
	StoppedAt   time.Time     `json:"stopped_at,omitempty"`
	Active      bool          `json:"active"`
	// StopReason is set once the recording has stopped: why (an explicit
	// stop, print end and the observed state, max duration reached, or a
	// disk-safety limit).
	StopReason string          `json:"stop_reason,omitempty"`
	Parts      []RecordingPart `json:"parts"`
	// Frames, IntervalMode and FallbackIntervalSeconds are set only for
	// Mode == RecordModeTimelapse (T11d); Parts is unused for that mode and
	// Frames is unused for RecordModeVideo. IntervalMode is "layer" when
	// frames were triggered by a detected print_stats.info.current_layer or
	// virtual_sdcard.layer change, or "time" when neither was known and
	// FallbackIntervalSeconds (recorderTimelapseFallbackInterval) was used
	// instead.
	Frames                  []TimelapseFrame `json:"frames,omitempty"`
	IntervalMode            string           `json:"interval_mode,omitempty"`
	FallbackIntervalSeconds int              `json:"fallback_interval_seconds,omitempty"`
	Bytes                   int64            `json:"bytes"`
	DurationSeconds         float64          `json:"duration_seconds"`
}

// StartRecordingRequest is Recorder.Start's request.
type StartRecordingRequest struct {
	PrinterID string
	Host      string
	// Identity is the printer identity (Klipper hostname, falling back to
	// host) StateSource.Snapshot resolves against, matching
	// internal/policy's own printerIdentity rule. Only used when Until is
	// RecordUntilPrintEnd.
	Identity    string
	Mode        RecordMode
	Until       RecordUntil
	MaxDuration time.Duration // 0 uses recorderDefaultMaxDuration
}

// activeRecording is one in-progress recording's mutable state. mu guards
// every field a concurrent List/Stop call can read while run's goroutine is
// still writing to it; ctx/cancel/done control that goroutine's lifetime.
type activeRecording struct {
	id          string
	printerID   string
	host        string
	identity    string
	mode        RecordMode
	until       RecordUntil
	maxDuration time.Duration
	dir         string
	startedAt   time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu           sync.Mutex
	parts        []RecordingPart
	current      *recordingPart
	stopReason   string // set by Stop before cancel; read by run's ctx.Done branch
	stoppedAt    time.Time
	needKeyframe bool
	lastPTS      uint64

	// --- Timelapse-only fields (T11d); unused when mode == RecordModeVideo. ---

	// framesDir is where this timelapse's JPEG stills are written:
	// <printer-dir>/<base>, a subdirectory of dir (ar.dir stays the printer
	// directory, matching video's own layout, so the sidecar - written at
	// dir/base.json by writeSidecar - sits next to the frames directory
	// rather than inside it).
	framesDir string
	frames    []TimelapseFrame
	// lastLayer is the most recently captured layer number, nil until the
	// first capture; used to detect a layer change on the next poll.
	lastLayer *int
	// intervalMode and fallbackIntervalSeconds mirror RecordingInfo's own
	// fields, kept live on ar so snapshotInfoLocked can read them under
	// ar.mu.
	intervalMode            string
	fallbackIntervalSeconds int
}

// recordingPart is one open fmp4 part file.
type recordingPart struct {
	path      string
	startedAt time.Time
	writer    *fmp4.Writer
}

// recorderHub is the subset of *Hub's API Recorder depends on, matching
// viewerHub's own shape; *Hub satisfies it directly. Snapshot backs
// runTimelapse's captures (review backlog item 51: timelapse frames are
// captured through the same hub rolling-buffer/decode path
// get_camera_snapshot and the CLI/TUI use, instead of runTimelapse
// running its own keyframe-wait-and-decode logic against the raw
// subscription channel).
type recorderHub interface {
	Subscribe(host string) (id int, ch <-chan camera.AccessUnit)
	Unsubscribe(host string, id int)
	RequestKeyframe(host string) error
	Snapshot(ctx context.Context, host string, budget time.Duration) (*camera.SnapshotResult, error)
}

var _ recorderHub = (*Hub)(nil)

// Recorder is T11c's recording component: it owns zero or more active
// recordings (at most one per printer), each fed by a Hub subscription like
// a browser viewer, muxed to fragmented MP4 files under dir with
// fmp4.NewRecordingWriter's crash-safety guarantee (fsync per fragment, so
// a hard kill leaves every already-flushed fragment playable).
type Recorder struct {
	dir   string
	hub   recorderHub
	state StateSource
	log   *log.Logger

	// freeBytes and now are overridden by tests (recorder_test.go, same
	// package) so disk-safety and duration checks never depend on the real
	// filesystem's free space or wall-clock time.
	freeBytes func(dir string) (uint64, error)
	now       func() time.Time

	mu              sync.Mutex
	active          map[string]*activeRecording
	activeByPrinter map[string]string
}

// NewRecorder builds a Recorder writing under dir (created if missing),
// fed by hub, using state for RecordUntilPrintEnd polling (the same
// StateSource the idle-heat Watchdog uses, per T11c's "via the existing
// state source used by the watchdog"). logger may be nil.
func NewRecorder(dir string, hub recorderHub, state StateSource, logger *log.Logger) *Recorder {
	return &Recorder{
		dir:             dir,
		hub:             hub,
		state:           state,
		log:             logger,
		freeBytes:       freeDiskBytes,
		now:             time.Now,
		active:          make(map[string]*activeRecording),
		activeByPrinter: make(map[string]string),
	}
}

func (r *Recorder) logf(format string, args ...any) {
	if r.log != nil {
		r.log.Printf(format, args...)
	}
}

// Start begins a new recording for req.PrinterID, refusing a second
// concurrent recording for the same printer (one active recording per
// printer) and refusing to start at all when free disk space on the
// recordings volume is below 1 GiB. It returns once the first keyframe has
// been captured and the recording file has been created, so a caller sees
// a real error for a camera that never produces a keyframe rather than a
// recording that silently never starts.
// stateSourceReason names why req needs r.state, for Start's "no state
// source wired up" error message.
func stateSourceReason(req StartRecordingRequest) string {
	if req.Mode == RecordModeTimelapse {
		return "mode \"timelapse\""
	}
	return "until \"print_end\""
}

func (r *Recorder) Start(ctx context.Context, req StartRecordingRequest) (RecordingInfo, error) {
	if strings.TrimSpace(req.PrinterID) == "" {
		return RecordingInfo{}, fmt.Errorf("daemon: recording.start requires printer_id")
	}
	// PrinterID becomes half of this recording's id (Recorder.Start below)
	// and, unvalidated, would let a hostile recording.start request over
	// the socket create a directory outside r.dir (e.g. printer_id
	// "../../whatever") that Delete would then also trust; validate it with
	// the exact same allowlist splitRecordingID applies to an id it is
	// asked to delete (review backlog item 33).
	if err := validateRecordingIDSegment(req.PrinterID); err != nil {
		return RecordingInfo{}, fmt.Errorf("daemon: invalid printer_id %q: %w", req.PrinterID, err)
	}
	if strings.TrimSpace(req.Host) == "" {
		return RecordingInfo{}, fmt.Errorf("daemon: recording.start requires host")
	}
	if req.Mode == "" {
		req.Mode = RecordModeVideo
	}
	if req.Mode != RecordModeVideo && req.Mode != RecordModeTimelapse {
		return RecordingInfo{}, fmt.Errorf("daemon: unknown recording mode %q, want \"video\" or \"timelapse\"", req.Mode)
	}
	if req.Until == "" {
		// Timelapse defaults to print_end (dev_docs T11d); video keeps
		// stopped as its own default (T11c, unchanged).
		if req.Mode == RecordModeTimelapse {
			req.Until = RecordUntilPrintEnd
		} else {
			req.Until = RecordUntilStopped
		}
	}
	if req.Until != RecordUntilPrintEnd && req.Until != RecordUntilStopped {
		return RecordingInfo{}, fmt.Errorf("daemon: unknown until %q, want \"print_end\" or \"stopped\"", req.Until)
	}
	// A timelapse always polls the printer's state to detect a layer change,
	// regardless of Until (print_end is only about when the recording stops
	// on its own; layer detection is separate and unconditional), so it
	// needs a state source either way.
	if r.state == nil && (req.Until == RecordUntilPrintEnd || req.Mode == RecordModeTimelapse) {
		return RecordingInfo{}, fmt.Errorf("daemon: %s requires a printer state source, which this daemon does not have wired up", stateSourceReason(req))
	}
	if req.MaxDuration <= 0 {
		if req.Mode == RecordModeTimelapse {
			req.MaxDuration = recorderDefaultTimelapseMaxDuration
		} else {
			req.MaxDuration = recorderDefaultMaxDuration
		}
	}

	if err := r.reservePrinter(req.PrinterID); err != nil {
		return RecordingInfo{}, err
	}
	reserved := true
	defer func() {
		if reserved {
			r.releasePrinterReservation(req.PrinterID)
		}
	}()

	// The free-space check needs r.dir to already exist (both
	// GetDiskFreeSpaceExW and statfs require an existing path), so the
	// recordings root is created here, before the check, rather than only
	// the printer subdirectory afterward.
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return RecordingInfo{}, fmt.Errorf("daemon: create recordings directory: %w", err)
	}
	if err := lstatNotSymlink(r.dir); err != nil {
		return RecordingInfo{}, err
	}

	free, err := r.freeBytes(r.dir)
	if err != nil {
		return RecordingInfo{}, fmt.Errorf("daemon: check free disk space: %w", err)
	}
	if free < recorderMinFreeBytesToStart {
		return RecordingInfo{}, fmt.Errorf(
			"daemon: refusing to start a recording: %d bytes free on the recordings volume, below the 1 GiB safety threshold",
			free)
	}

	printerDir := filepath.Join(r.dir, req.PrinterID)
	if err := os.MkdirAll(printerDir, 0o700); err != nil {
		return RecordingInfo{}, fmt.Errorf("daemon: create recordings directory: %w", err)
	}
	if err := lstatNotSymlink(printerDir); err != nil {
		return RecordingInfo{}, err
	}

	startedAt := r.now().UTC()
	base := startedAt.Format("20060102T150405Z")
	if req.Mode == RecordModeTimelapse {
		base += "-timelapse"
	}
	id := req.PrinterID + "/" + base

	arCtx, cancel := context.WithCancel(context.Background())
	ar := &activeRecording{
		id:          id,
		printerID:   req.PrinterID,
		host:        req.Host,
		identity:    req.Identity,
		mode:        req.Mode,
		until:       req.Until,
		maxDuration: req.MaxDuration,
		dir:         printerDir,
		startedAt:   startedAt,
		ctx:         arCtx,
		cancel:      cancel,
		done:        make(chan struct{}),
	}

	subID, ch := r.hub.Subscribe(req.Host)
	// Both modes confirm the camera stream is actually alive before this
	// call returns, so a caller sees a real error for a camera that never
	// produces a keyframe rather than a recording that silently never
	// starts. Video keeps that very keyframe as its first sample; a
	// timelapse discards it (it is not tied to any layer) and waits for its
	// own poll-triggered captures instead.
	keyframe, ok := waitForKeyframe(ctx, ch, recorderKeyframeWait)
	if !ok {
		r.hub.Unsubscribe(req.Host, subID)
		cancel()
		return RecordingInfo{}, fmt.Errorf("daemon: no camera keyframe received from %s within %s", req.Host, recorderKeyframeWait)
	}

	if req.Mode == RecordModeTimelapse {
		framesDir := filepath.Join(printerDir, base)
		if err := os.MkdirAll(framesDir, 0o700); err != nil {
			r.hub.Unsubscribe(req.Host, subID)
			cancel()
			return RecordingInfo{}, fmt.Errorf("daemon: create timelapse frames directory: %w", err)
		}
		if err := lstatNotSymlink(framesDir); err != nil {
			r.hub.Unsubscribe(req.Host, subID)
			cancel()
			return RecordingInfo{}, err
		}
		ar.framesDir = framesDir

		r.mu.Lock()
		r.active[id] = ar
		r.activeByPrinter[req.PrinterID] = id
		r.mu.Unlock()
		reserved = false // ownership of the printer reservation moved into r.active/activeByPrinter

		go r.runTimelapse(ar, subID)

		r.logf("timelapse recording started: id=%s printer=%s until=%s", id, req.PrinterID, req.Until)
		return ar.snapshotInfo(r.now()), nil
	}

	if err := r.beginPart(ar, 1, keyframe); err != nil {
		r.hub.Unsubscribe(req.Host, subID)
		cancel()
		return RecordingInfo{}, fmt.Errorf("daemon: start recording file: %w", err)
	}

	r.mu.Lock()
	r.active[id] = ar
	r.activeByPrinter[req.PrinterID] = id
	r.mu.Unlock()
	reserved = false // ownership of the printer reservation moved into r.active/activeByPrinter

	go r.run(ar, subID, ch)

	r.logf("recording started: id=%s printer=%s mode=%s until=%s", id, req.PrinterID, req.Mode, req.Until)
	return ar.snapshotInfo(r.now()), nil
}

// reservePrinter atomically claims req.PrinterID for a new recording,
// refusing when one is already active (or being started) for it.
func (r *Recorder) reservePrinter(printerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.activeByPrinter[printerID]; ok {
		return fmt.Errorf("daemon: a recording (%s) is already active for printer %q", id, printerID)
	}
	r.activeByPrinter[printerID] = "" // placeholder: reserved, not yet assigned an id
	return nil
}

// releasePrinterReservation undoes reservePrinter's placeholder when Start
// fails before the recording is actually registered.
func (r *Recorder) releasePrinterReservation(printerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeByPrinter[printerID] == "" {
		delete(r.activeByPrinter, printerID)
	}
}

// beginPart opens part number n for ar from a keyframe access unit,
// extracting its SPS/PPS for the new fmp4 file's init segment, and writes
// that keyframe as the part's first sample. The caller is responsible for
// closing any previously open part (closeCurrentPart) before calling this.
func (r *Recorder) beginPart(ar *activeRecording, n int, keyframe camera.AccessUnit) error {
	sps, pps, err := recorderParameterSets(keyframe.Data)
	if err != nil {
		return err
	}

	name := fmt.Sprintf("%s.mp4", filepath.Base(ar.id))
	if n > 1 {
		name = fmt.Sprintf("%s-part%d.mp4", filepath.Base(ar.id), n)
	}
	path := filepath.Join(ar.dir, name)

	w, err := fmp4.NewRecordingWriter(path, sps, pps, fmp4.Config{
		FragmentDuration: recorderFragmentDuration,
		MaxKeyframeWait:  recorderMaxKeyframeWait,
		MaxBufferedBytes: recorderMaxBufferedBytes,
	})
	if err != nil {
		return err
	}
	if err := w.WriteAccessUnit(keyframe.PTS, keyframe.Data); err != nil {
		_ = w.Close()
		return err
	}

	ar.mu.Lock()
	ar.current = &recordingPart{path: path, startedAt: r.now()}
	ar.current.writer = w
	ar.needKeyframe = false
	ar.lastPTS = keyframe.PTS
	ar.mu.Unlock()
	return nil
}

// closeCurrentPart closes ar's currently open part (if any), records its
// final byte count and end reason, and appends it to ar.parts.
func (r *Recorder) closeCurrentPart(ar *activeRecording, endReason string) {
	ar.mu.Lock()
	cur := ar.current
	ar.current = nil
	ar.mu.Unlock()
	if cur == nil {
		return
	}
	_ = cur.writer.Close()
	stoppedAt := r.now()
	size := fileSize(cur.path)
	part := RecordingPart{
		Path:      cur.path,
		StartedAt: cur.startedAt,
		StoppedAt: stoppedAt,
		Bytes:     size,
		EndReason: endReason,
	}
	ar.mu.Lock()
	ar.parts = append(ar.parts, part)
	ar.mu.Unlock()
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// run is the goroutine that pumps ar's hub subscription into its current
// fmp4 part, handling parameter changes (a new part), keyframe waits (a
// PLI, same writer), camera reconnects seen as a PTS discontinuity (a new
// part, since camera.AccessUnit's PTS resets to 0 at the start of each
// upstream camera.Session and fmp4.Writer requires strictly increasing
// timestamps), print-end polling, the max-duration limit and the disk-space
// stop threshold.
func (r *Recorder) run(ar *activeRecording, subID int, ch <-chan camera.AccessUnit) {
	defer close(ar.done)
	defer r.hub.Unsubscribe(ar.host, subID)

	maxDurationTimer := time.NewTimer(ar.maxDuration)
	defer maxDurationTimer.Stop()

	diskTicker := time.NewTicker(recorderDiskCheckInterval)
	defer diskTicker.Stop()

	var pollC <-chan time.Time
	if ar.until == RecordUntilPrintEnd {
		pollTicker := time.NewTicker(recorderStatePollInterval)
		defer pollTicker.Stop()
		pollC = pollTicker.C
	}

	// requestedKeyframe avoids calling r.hub.RequestKeyframe more than once
	// per gap from this consumer: the hub itself now owns retrying while a
	// keyframe is still missing (review backlog item 51, item 3 - it
	// coalesces every consumer's request, plus its own automatic recovery
	// retry, to at most one actual PLI per second per upstream), so this
	// loop no longer runs its own periodic retry ticker (that was review
	// backlog item 44's fix, now superseded: see the doc comment on the
	// ErrNoKeyframe case below).
	requestedKeyframe := false
	sawPrinting := false

	for {
		select {
		case <-ar.ctx.Done():
			ar.mu.Lock()
			reason := ar.stopReason
			ar.mu.Unlock()
			if reason == "" {
				reason = "stopped by request"
			}
			r.finish(ar, reason)
			return

		case <-maxDurationTimer.C:
			r.finish(ar, fmt.Sprintf("stopped: reached the maximum recording duration (%s)", ar.maxDuration))
			return

		case <-diskTicker.C:
			free, err := r.freeBytes(r.dir)
			if err == nil && free < recorderMinFreeBytesToStop {
				r.finish(ar, fmt.Sprintf("stopped: free disk space fell to %d bytes, below the 512 MiB safety threshold", free))
				return
			}

		case <-pollC:
			pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			snap, derived, ok := r.state.Snapshot(pollCtx, ar.identity)
			cancel()
			_ = snap
			if !ok {
				// The printer could not be reached on this poll (a short
				// network blip, a reboot, etc.). This is never treated as
				// the print having ended: the recording keeps running,
				// bounded only by MaxDuration and the disk-safety check
				// (see RecordUntilPrintEnd's doc comment).
				continue
			}
			switch derived.State {
			case printerstate.StatePrinting:
				sawPrinting = true
			case printerstate.StateComplete, printerstate.StateCancelled, printerstate.StateError:
				r.finish(ar, fmt.Sprintf("stopped: print ended (%s)", derived.State))
				return
			case printerstate.StateIdle:
				if sawPrinting {
					r.finish(ar, "stopped: print ended (idle after printing)")
					return
				}
			}

		case au, ok := <-ch:
			if !ok {
				r.finish(ar, "stopped: the camera connection ended")
				return
			}

			ar.mu.Lock()
			lastPTS := ar.lastPTS
			ar.mu.Unlock()

			if au.PTS <= lastPTS {
				// The upstream camera session reconnected (a fresh
				// camera.Session resets PTS to 0) or delivered an
				// out-of-order frame; fmp4.Writer requires strictly
				// increasing timestamps, so this part must end here.
				r.closeCurrentPart(ar, "camera reconnected")
				ar.mu.Lock()
				ar.needKeyframe = true
				ar.mu.Unlock()
				if au.Keyframe {
					partNum := len(ar.parts) + 1
					if err := r.beginPart(ar, partNum, au); err != nil {
						r.finish(ar, "stopped: could not start a new part: "+err.Error())
						return
					}
					requestedKeyframe = false
				}
				continue
			}

			ar.mu.Lock()
			cur := ar.current
			ar.mu.Unlock()
			if cur == nil {
				continue
			}

			err := cur.writer.WriteAccessUnit(au.PTS, au.Data)
			switch {
			case err == nil:
				requestedKeyframe = false
				ar.mu.Lock()
				ar.lastPTS = au.PTS
				ar.needKeyframe = false
				ar.mu.Unlock()

			case errors.Is(err, fmp4.ErrParametersChanged):
				r.closeCurrentPart(ar, "camera parameters changed")
				partNum := len(ar.parts) + 1
				if err := r.beginPart(ar, partNum, au); err != nil {
					r.finish(ar, "stopped: could not start a new part: "+err.Error())
					return
				}
				requestedKeyframe = false

			case errors.Is(err, fmp4.ErrNoKeyframe):
				// The fmp4 contract (internal/camera/fmp4's ErrNoKeyframe
				// doc comment): the Writer itself stays open and, on its
				// own, accepts and resumes on the very next keyframe access
				// unit passed to WriteAccessUnit (its internal needKeyframe
				// state), still on this same writer/part. So this case must
				// never close the current part or open a new one (that was
				// review backlog item 34's bug: it silently leaked the
				// still-open writer's file handle and dropped that part
				// from ar.parts, so it never reached the sidecar or
				// list_recordings, and delete_recording never removed its
				// file). This branch remembers that a keyframe is still
				// owed and asks the camera for one (PLI); the hub itself
				// (Hub.RequestKeyframe's coalescing plus its own automatic
				// recovery-retry ticker while no keyframe is buffered, see
				// internal/daemon/camera_hub.go's pump) keeps that request
				// alive at up to once per second for as long as it takes,
				// without this loop needing a retry ticker of its own
				// (review backlog item 51, superseding item 44's earlier,
				// per-consumer retry loop here).
				ar.mu.Lock()
				ar.needKeyframe = true
				ar.mu.Unlock()
				if !requestedKeyframe {
					_ = r.hub.RequestKeyframe(ar.host)
					requestedKeyframe = true
				}

			default:
				r.finish(ar, "stopped: write error: "+err.Error())
				return
			}
		}
	}
}

// finish closes any still-open part, writes the sidecar JSON, and removes
// ar from the active maps.
func (r *Recorder) finish(ar *activeRecording, reason string) {
	r.closeCurrentPart(ar, "")
	stoppedAt := r.now()

	ar.mu.Lock()
	ar.stopReason = reason
	ar.stoppedAt = stoppedAt
	info := ar.snapshotInfoLocked(stoppedAt, false)
	ar.mu.Unlock()

	if err := writeSidecar(ar.dir, filepath.Base(ar.id), info); err != nil {
		r.logf("recording %s: write sidecar: %v", ar.id, err)
	}

	r.mu.Lock()
	delete(r.active, ar.id)
	if r.activeByPrinter[ar.printerID] == ar.id {
		delete(r.activeByPrinter, ar.printerID)
	}
	r.mu.Unlock()

	r.logf("recording stopped: id=%s reason=%q parts=%d bytes=%d", ar.id, reason, len(info.Parts), info.Bytes)
}

// layerFromSnapshot returns the printer layer number reported by snap, per
// decision 11: print_stats.info.current_layer takes priority (Moonraker's
// own *int, nil when not reported), falling back to virtual_sdcard.layer
// (a plain int, known whenever VirtualSDCard itself is present). known is
// false only when neither is available, in which case the caller falls back
// to a time interval instead (dev_docs T11d).
func layerFromSnapshot(snap printerstate.Snapshot) (layer int, known bool) {
	if snap.PrintStats != nil && snap.PrintStats.Info.CurrentLayer != nil {
		return *snap.PrintStats.Info.CurrentLayer, true
	}
	if snap.VirtualSDCard != nil {
		return snap.VirtualSDCard.Layer, true
	}
	return 0, false
}

// timelapseFrameName names one timelapse still: "frame_<N:05d>.jpg", where N
// is the triggering layer number when known, or this capture's 1-based
// sequence number when it was captured on the time-interval fallback
// instead (layer == nil).
func timelapseFrameName(seq int, layer *int) string {
	if layer != nil {
		return fmt.Sprintf("frame_%05d.jpg", *layer)
	}
	return fmt.Sprintf("frame_%05d.jpg", seq)
}

// writeTimelapseFrame JPEG-encodes img (quality recorderTimelapseJPEGQuality,
// full resolution) and writes it to ar.framesDir via domain.WriteFileAtomic,
// naming it from layer (nil for a time-interval capture; see
// timelapseFrameName). It does not mutate ar.frames itself: the caller
// appends the returned TimelapseFrame under ar.mu once the write succeeds,
// matching how run/beginPart keep every ar mutation under that same lock.
func (r *Recorder) writeTimelapseFrame(ar *activeRecording, img image.Image, layer *int) (TimelapseFrame, error) {
	ar.mu.Lock()
	seq := len(ar.frames) + 1
	ar.mu.Unlock()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: recorderTimelapseJPEGQuality}); err != nil {
		return TimelapseFrame{}, fmt.Errorf("encode JPEG: %w", err)
	}
	path := filepath.Join(ar.framesDir, timelapseFrameName(seq, layer))
	if err := domain.WriteFileAtomic(path, buf.Bytes(), 0o600); err != nil {
		return TimelapseFrame{}, fmt.Errorf("write frame: %w", err)
	}

	frame := TimelapseFrame{Path: path, CapturedAt: r.now(), Bytes: int64(buf.Len())}
	if layer != nil {
		l := *layer
		frame.Layer = &l
	}
	return frame, nil
}

// timelapseCaptureResult is one asynchronous hub.Snapshot capture's
// outcome, delivered back to runTimelapse's main select loop by the
// goroutine captureTimelapseFrame starts (see runTimelapse).
type timelapseCaptureResult struct {
	result *camera.SnapshotResult
	layer  *int
	err    error
}

// captureTimelapseFrame calls the hub's snapshot path (review backlog item
// 51: the same path get_camera_snapshot, the CLI and the TUI use) in its
// own goroutine and delivers the outcome on resultCh, which must be
// buffered by at least 1 so this goroutine never blocks (and so never
// leaks) even if runTimelapse has already returned by the time the
// capture finishes - Stop cancels ar.ctx, which this capture's context is
// derived from, so a capture in flight when the recording stops is
// cancelled promptly rather than running out its full budget.
func captureTimelapseFrame(ar *activeRecording, hub recorderHub, layer *int, resultCh chan<- timelapseCaptureResult) {
	result, err := hub.Snapshot(ar.ctx, ar.host, 0)
	resultCh <- timelapseCaptureResult{result: result, layer: layer, err: err}
}

// runTimelapse is the goroutine that drives one timelapse recording (T11d):
// it polls ar's printer state every recorderStatePollInterval to detect a
// layer change (or, when neither print_stats.info.current_layer nor
// virtual_sdcard.layer is known, falls back to capturing every
// recorderTimelapseFallbackInterval instead), and on each trigger captures
// one frame through the hub's own snapshot path (Hub.Snapshot: the rolling
// GOP buffer, reused per-printer decoder, and PLI-on-demand internal/daemon
// already uses for get_camera_snapshot/the CLI/the TUI). subID's
// subscription is kept open for the whole recording purely to keep the
// upstream camera connection warm between captures (Hub.Snapshot's own
// keep-warm window is meant for occasional, not continuously spaced-out,
// calls), so every capture after the first is typically fast; the
// subscription's own channel is not read here at all once Start's initial
// connectivity check has consumed its first keyframe; any access unit an
// active subscription still delivers on it is simply left buffered and
// dropped, exactly as it would be for any other slow/non-reading
// subscriber (printerCamera.dispatch's non-blocking send). Print-end
// polling, max duration and the disk-space stop threshold mirror run's own
// handling for video.
func (r *Recorder) runTimelapse(ar *activeRecording, subID int) {
	defer close(ar.done)
	defer r.hub.Unsubscribe(ar.host, subID)

	maxDurationTimer := time.NewTimer(ar.maxDuration)
	defer maxDurationTimer.Stop()

	diskTicker := time.NewTicker(recorderDiskCheckInterval)
	defer diskTicker.Stop()

	pollTicker := time.NewTicker(recorderStatePollInterval)
	defer pollTicker.Stop()

	sawPrinting := false
	wantCapture := false
	lastCaptureAt := r.now()
	captureCh := make(chan timelapseCaptureResult, 1)

	for {
		select {
		case <-ar.ctx.Done():
			ar.mu.Lock()
			reason := ar.stopReason
			ar.mu.Unlock()
			if reason == "" {
				reason = "stopped by request"
			}
			r.finishTimelapse(ar, reason)
			return

		case <-maxDurationTimer.C:
			r.finishTimelapse(ar, fmt.Sprintf("stopped: reached the maximum recording duration (%s)", ar.maxDuration))
			return

		case <-diskTicker.C:
			free, err := r.freeBytes(r.dir)
			if err == nil && free < recorderMinFreeBytesToStop {
				r.finishTimelapse(ar, fmt.Sprintf("stopped: free disk space fell to %d bytes, below the 512 MiB safety threshold", free))
				return
			}

		case <-pollTicker.C:
			pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			snap, derived, ok := r.state.Snapshot(pollCtx, ar.identity)
			cancel()
			if !ok {
				// Same rule as video's until:"print_end" polling: an
				// unreachable printer never ends the recording or the
				// layer-change detection on its own; the recording keeps
				// running, bounded only by MaxDuration and the disk-safety
				// check.
				continue
			}

			if ar.until == RecordUntilPrintEnd {
				switch derived.State {
				case printerstate.StatePrinting:
					sawPrinting = true
				case printerstate.StateComplete, printerstate.StateCancelled, printerstate.StateError:
					r.finishTimelapse(ar, fmt.Sprintf("stopped: print ended (%s)", derived.State))
					return
				case printerstate.StateIdle:
					if sawPrinting {
						r.finishTimelapse(ar, "stopped: print ended (idle after printing)")
						return
					}
				}
			}

			if wantCapture {
				// A capture is already in flight; do not pile up a second
				// trigger on top of it.
				continue
			}

			layer, layerKnown := layerFromSnapshot(snap)
			ar.mu.Lock()
			lastLayer := ar.lastLayer
			if layerKnown {
				ar.intervalMode = "layer"
			} else {
				ar.intervalMode = "time"
				ar.fallbackIntervalSeconds = int(recorderTimelapseFallbackInterval.Seconds())
			}
			ar.mu.Unlock()

			trigger := false
			var triggerLayer *int
			if layerKnown {
				if lastLayer == nil || layer != *lastLayer {
					trigger = true
					l := layer
					triggerLayer = &l
				}
			} else if r.now().Sub(lastCaptureAt) >= recorderTimelapseFallbackInterval {
				trigger = true
			}
			if trigger {
				wantCapture = true
				go captureTimelapseFrame(ar, r.hub, triggerLayer, captureCh)
			}

		case res := <-captureCh:
			wantCapture = false
			if res.err != nil {
				r.logf("timelapse %s: capture failed, skipping this trigger: %v", ar.id, res.err)
				continue
			}

			frame, err := r.writeTimelapseFrame(ar, res.result.Image, res.layer)
			if err != nil {
				r.logf("timelapse %s: write frame failed: %v", ar.id, err)
				continue
			}
			ar.mu.Lock()
			ar.frames = append(ar.frames, frame)
			if res.layer != nil {
				l := *res.layer
				ar.lastLayer = &l
			}
			ar.mu.Unlock()
			lastCaptureAt = r.now()
		}
	}
}

// finishTimelapse mirrors finish (video) for a timelapse recording: no part
// to close (and, since review backlog item 51, no decoder of its own to
// close either - Hub.Snapshot owns and reuses one per printer), so
// finishTimelapse only needs to write the sidecar and remove ar from the
// active maps.
func (r *Recorder) finishTimelapse(ar *activeRecording, reason string) {
	stoppedAt := r.now()

	ar.mu.Lock()
	ar.stopReason = reason
	ar.stoppedAt = stoppedAt
	info := ar.snapshotInfoLocked(stoppedAt, false)
	ar.mu.Unlock()

	if err := writeSidecar(ar.dir, filepath.Base(ar.id), info); err != nil {
		r.logf("timelapse %s: write sidecar: %v", ar.id, err)
	}

	r.mu.Lock()
	delete(r.active, ar.id)
	if r.activeByPrinter[ar.printerID] == ar.id {
		delete(r.activeByPrinter, ar.printerID)
	}
	r.mu.Unlock()

	r.logf("timelapse recording stopped: id=%s reason=%q frames=%d bytes=%d", ar.id, reason, len(info.Frames), info.Bytes)
}

// snapshotInfo builds a RecordingInfo for ar as of now, for List and Start's
// return value.
func (ar *activeRecording) snapshotInfo(now time.Time) RecordingInfo {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	return ar.snapshotInfoLocked(now, true)
}

// snapshotInfoLocked is snapshotInfo's implementation; ar.mu must be held.
func (ar *activeRecording) snapshotInfoLocked(now time.Time, active bool) RecordingInfo {
	parts := append([]RecordingPart(nil), ar.parts...)
	var totalBytes int64
	for _, p := range parts {
		totalBytes += p.Bytes
	}
	if ar.current != nil {
		size := fileSize(ar.current.path)
		totalBytes += size
		parts = append(parts, RecordingPart{
			Path:      ar.current.path,
			StartedAt: ar.current.startedAt,
			Bytes:     size,
		})
	}
	frames := append([]TimelapseFrame(nil), ar.frames...)
	for _, f := range frames {
		totalBytes += f.Bytes
	}
	stoppedAt := ar.stoppedAt
	end := now
	if !active {
		end = stoppedAt
	}
	return RecordingInfo{
		ID:                      ar.id,
		PrinterID:               ar.printerID,
		Mode:                    ar.mode,
		Until:                   ar.until,
		MaxDuration:             ar.maxDuration,
		StartedAt:               ar.startedAt,
		StoppedAt:               stoppedAt,
		Active:                  active,
		StopReason:              ar.stopReason,
		Parts:                   parts,
		Frames:                  frames,
		IntervalMode:            ar.intervalMode,
		FallbackIntervalSeconds: ar.fallbackIntervalSeconds,
		Bytes:                   totalBytes,
		DurationSeconds:         end.Sub(ar.startedAt).Seconds(),
	}
}

// Stop ends the active recording id, waiting for it to flush and close
// before returning its final metadata. It returns an error if no active
// recording with this id exists. id is validated with the same strict rule
// as Delete (review backlog item 33) before anything else, even though an
// active recording is looked up from an in-memory map (never a filesystem
// path built from the raw id): a malformed id is rejected uniformly by
// every recording method rather than only the ones that happen to touch
// disk today.
func (r *Recorder) Stop(id string) (RecordingInfo, error) {
	if _, _, err := splitRecordingID(id); err != nil {
		return RecordingInfo{}, err
	}

	r.mu.Lock()
	ar, ok := r.active[id]
	r.mu.Unlock()
	if !ok {
		return RecordingInfo{}, fmt.Errorf("daemon: no active recording with id %q", id)
	}

	ar.mu.Lock()
	if ar.stopReason == "" {
		ar.stopReason = "stopped by request"
	}
	ar.mu.Unlock()
	ar.cancel()

	select {
	case <-ar.done:
	case <-time.After(10 * time.Second):
		return RecordingInfo{}, fmt.Errorf("daemon: recording %q did not stop within 10s", id)
	}

	info, err := readSidecar(ar.dir, filepath.Base(ar.id))
	if err != nil {
		return ar.snapshotInfo(r.now()), nil
	}
	return info, nil
}

// ListRecordingsResult is Recorder.List's result: every recording (active,
// from memory, plus completed ones read from their sidecar JSON files) and
// the recordings directory's total disk usage.
type ListRecordingsResult struct {
	Recordings     []RecordingInfo
	DiskUsageBytes int64
}

// List returns every recording this Recorder knows about: active
// recordings from memory, plus every completed recording found by scanning
// dir for sidecar JSON files, newest first, along with the recordings
// directory's total disk usage.
func (r *Recorder) List() (ListRecordingsResult, error) {
	r.mu.Lock()
	actives := make([]*activeRecording, 0, len(r.active))
	for _, ar := range r.active {
		actives = append(actives, ar)
	}
	r.mu.Unlock()

	seen := make(map[string]bool, len(actives))
	recordings := make([]RecordingInfo, 0, len(actives))
	for _, ar := range actives {
		info := ar.snapshotInfo(r.now())
		recordings = append(recordings, info)
		seen[info.ID] = true
	}

	var totalBytes int64
	printerDirs, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			printerDirs = nil
		} else {
			return ListRecordingsResult{}, fmt.Errorf("daemon: list recordings directory: %w", err)
		}
	}
	for _, pd := range printerDirs {
		// Skip a symlinked entry outright, in either position (review
		// backlog item 38): pd.Type() reflects the directory entry's own
		// Lstat-equivalent mode (os.ReadDir never follows a symlink to
		// decide IsDir), so this never descends into or trusts one.
		if pd.Type()&os.ModeSymlink != 0 || !pd.IsDir() {
			continue
		}
		printerID := pd.Name()
		printerDir := filepath.Join(r.dir, printerID)
		if err := lstatNotSymlink(printerDir); err != nil {
			continue
		}

		// Disk usage recurses into printerDir (review backlog item 38: a
		// timelapse's frames live in a subdirectory,
		// printerDir/<base>/frame_*.jpg, not directly in printerDir the way
		// a video recording's parts do), skipping any symlinked entry along
		// the way rather than following it: fs.WalkDir's own DirEntry
		// values are Lstat-derived (it never dereferences a symlink to
		// decide whether to recurse), so a symlinked file is seen here but
		// never opened, and a symlinked directory is never walked into.
		_ = filepath.WalkDir(printerDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if info, err := d.Info(); err == nil {
				totalBytes += info.Size()
			}
			return nil
		})

		files, err := os.ReadDir(printerDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.Type()&os.ModeSymlink != 0 || f.IsDir() {
				continue
			}
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			base := strings.TrimSuffix(f.Name(), ".json")
			id := printerID + "/" + base
			// Defense in depth (review backlog item 33): validate every id
			// built from disk before it is ever handed back to a caller
			// that might pass it straight into Stop/Delete, even though
			// printerID and base both come from os.ReadDir entries (which
			// can never themselves contain a path separator) rather than
			// from untrusted input.
			if _, _, err := splitRecordingID(id); err != nil {
				continue
			}
			if seen[id] {
				continue
			}
			rec, err := readSidecar(printerDir, base)
			if err != nil {
				continue
			}
			recordings = append(recordings, rec)
			seen[id] = true
		}
	}

	sort.Slice(recordings, func(i, j int) bool {
		return recordings[i].StartedAt.After(recordings[j].StartedAt)
	})
	return ListRecordingsResult{Recordings: recordings, DiskUsageBytes: totalBytes}, nil
}

// Delete removes a completed recording's files (every part or timelapse
// frame plus its sidecar), refusing when id is currently active.
func (r *Recorder) Delete(id string) error {
	r.mu.Lock()
	_, active := r.active[id]
	r.mu.Unlock()
	if active {
		return fmt.Errorf("daemon: recording %q is active; call recording.stop first", id)
	}

	printerID, base, err := splitRecordingID(id)
	if err != nil {
		return err
	}
	dir := filepath.Join(r.dir, printerID)
	if !pathContainedIn(r.dir, dir) {
		return fmt.Errorf("daemon: recording id %q resolves outside the recordings directory", id)
	}
	// Review backlog item 38: refuse before trusting dir at all if it turns
	// out to be a symlink (e.g. a hostile printer_id directory planted to
	// redirect a delete elsewhere under the recordings root).
	if err := lstatNotSymlink(dir); err != nil {
		return err
	}
	info, err := readSidecar(dir, base)
	if err != nil {
		return fmt.Errorf("daemon: recording %q not found: %w", id, err)
	}

	var firstErr error
	for _, p := range info.Parts {
		// The sidecar is our own JSON, written only by writeSidecar, but a
		// part path is still checked for containment before Remove: a
		// sidecar file that was tampered with (or corrupted) on disk must
		// never cause a delete to reach outside this recording's own
		// directory (review backlog item 33), and Lstat'd before removal so
		// a symlink planted at that path is refused rather than unlinked
		// silently (review backlog item 38).
		if !pathContainedIn(dir, p.Path) {
			if firstErr == nil {
				firstErr = fmt.Errorf("part path %q is outside the recording's directory; refusing to remove it", p.Path)
			}
			continue
		}
		if err := lstatNotSymlink(p.Path); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := os.Remove(p.Path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}

	if info.Mode == RecordModeTimelapse {
		// A timelapse's stills live in their own subdirectory
		// (dir/<base>/frame_*.jpg, see Recorder.Start), removed as a whole
		// rather than frame by frame from the sidecar's Frames list: this
		// also catches any stray file a crash left behind that never made
		// it into the sidecar (see removeTimelapseDir).
		framesDir := filepath.Join(dir, base)
		if !pathContainedIn(dir, framesDir) {
			if firstErr == nil {
				firstErr = fmt.Errorf("timelapse frames directory %q is outside the recording's directory; refusing to remove it", framesDir)
			}
		} else if err := removeTimelapseDir(framesDir); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}

	scPath := sidecarPath(dir, base)
	if !pathContainedIn(dir, scPath) {
		return fmt.Errorf("daemon: recording %q: sidecar path resolves outside the recording's directory", id)
	}
	if err := lstatNotSymlink(scPath); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	} else if err := os.Remove(scPath); err != nil && !os.IsNotExist(err) && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		return fmt.Errorf("daemon: delete recording %q: %w", id, firstErr)
	}
	return nil
}

// removeTimelapseDir removes dir (one timelapse recording's own frames
// directory) and every file directly inside it, Lstat-checking dir itself
// and every entry first and refusing outright if any of them is a symlink
// or an unexpected subdirectory (review backlog item 38), rather than a
// plain os.RemoveAll: unlinking a symlink never touches its target, so this
// is not a fix for a real deletion-through-symlink risk, but it does refuse
// to silently proceed past a symlink planted where only plain JPEG files are
// ever expected.
func removeTimelapseDir(dir string) error {
	if err := lstatNotSymlink(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("daemon: list timelapse frames directory %s: %w", dir, err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("daemon: %s is a symlink, refusing to remove the timelapse frames directory", p)
		}
		if e.IsDir() {
			return fmt.Errorf("daemon: unexpected subdirectory %s inside a timelapse frames directory, refusing to remove it automatically", p)
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("daemon: remove %s: %w", p, err)
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: remove %s: %w", dir, err)
	}
	return nil
}

// recordingIDSegmentPattern is the strict allowlist for one half of a
// recording id (either the printer id or the timestamp-based recording
// name, see Recorder.Start): ASCII letters, digits, dot, underscore and
// hyphen, at least one character, never starting with a dot (which would
// otherwise let a lone "." or ".." slip past the separate literal ".."
// check below, or a hidden-file-like name past it). No path separator, on
// either platform, is ever a member of this set.
var recordingIDSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)

// maxRecordingIDSegmentLength caps either half of a recording id at 64
// characters, matching domain.idPattern's own cap on a registry printer id
// (review backlog item 38).
const maxRecordingIDSegmentLength = 64

// validateRecordingIDSegment refuses everything splitRecordingID must
// reject in one half of a recording id: empty, too long, "." or "..", a
// literal ".." substring, a path separator (forward or back slash, so a
// backslash sequence smuggled through a single path segment is caught even
// on a platform whose filepath.Separator is "/"), an absolute path, a
// Windows drive letter, a character outside recordingIDSegmentPattern, a
// trailing dot or space, or a Windows reserved device name (with or without
// an extension) (dev_docs/review-backlog.md items 33 and 38,
// dev_docs/safety-architecture.md P1 "fail closed"). This is checked before
// the segment is ever used to build a filesystem path.
func validateRecordingIDSegment(s string) error {
	if s == "" {
		return fmt.Errorf("daemon: recording id segment must not be empty")
	}
	if len(s) > maxRecordingIDSegmentLength {
		return fmt.Errorf("daemon: recording id segment %q exceeds the maximum length of %d characters", s, maxRecordingIDSegmentLength)
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("daemon: recording id segment %q must not contain \"..\"", s)
	}
	if strings.ContainsAny(s, "/\\") {
		return fmt.Errorf("daemon: recording id segment %q must not contain a path separator", s)
	}
	if filepath.IsAbs(s) || (len(s) >= 2 && s[1] == ':') {
		return fmt.Errorf("daemon: recording id segment %q must not be an absolute path or a drive reference", s)
	}
	if !recordingIDSegmentPattern.MatchString(s) {
		return fmt.Errorf("daemon: recording id segment %q contains characters outside the allowed set (letters, digits, dot, underscore, hyphen; no leading dot)", s)
	}
	if domain.HasTrailingDotOrSpace(s) {
		return fmt.Errorf("daemon: recording id segment %q must not end with a dot or space", s)
	}
	if domain.IsReservedDeviceName(s) {
		return fmt.Errorf("daemon: recording id segment %q must not be a Windows reserved device name (CON, PRN, AUX, NUL, COM1-9, LPT1-9)", s)
	}
	return nil
}

// lstatNotSymlink Lstats path and refuses it if it is a symlink (review
// backlog item 38): defense in depth against a symlink planted somewhere
// under the recordings tree redirecting a read, write or delete outside it.
// A path that does not exist yet is not an error here (the caller may be
// about to create it); only an existing symlink is refused.
func lstatNotSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("daemon: stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("daemon: %s is a symlink, refusing to trust it", path)
	}
	return nil
}

// splitRecordingID splits an "<printer-id>/<timestamp>" recording id (see
// Recorder.Start) into its two parts, refusing anything else: an id that
// does not contain exactly one "/" separator, or whose printer-id or
// recording-name half fails validateRecordingIDSegment (a ".." component in
// either half, an absolute path, a drive letter, an extra separator, an
// empty segment, or a character outside the strict allowlist), is refused
// before either half is ever joined into a filesystem path (review backlog
// item 33, dev_docs/safety-architecture.md P1). This is the only place a
// recording id becomes a printer-id/base pair anywhere in this package.
func splitRecordingID(id string) (printerID, base string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("daemon: invalid recording id %q: want exactly one \"/\" separating printer id and recording name", id)
	}
	printerID, base = parts[0], parts[1]
	if err := validateRecordingIDSegment(printerID); err != nil {
		return "", "", fmt.Errorf("daemon: invalid recording id %q: %w", id, err)
	}
	if err := validateRecordingIDSegment(base); err != nil {
		return "", "", fmt.Errorf("daemon: invalid recording id %q: %w", id, err)
	}
	return printerID, base, nil
}

// pathContainedIn reports whether target (after filepath.Clean) is root
// itself or a descendant of root (also filepath.Clean'd), via filepath.Rel:
// the final defense-in-depth check after building a path from a validated
// recording id segment, before that path is ever read from or removed
// (review backlog item 33). This is checked in addition to, never instead
// of, validateRecordingIDSegment: Clean alone would silently resolve a
// traversal like "a/../.." into something that might still land outside
// root, which is exactly the case this catches.
func pathContainedIn(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if root == target {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func sidecarPath(dir, base string) string {
	return filepath.Join(dir, base+".json")
}

// writeSidecar persists info as dir/base.json via domain.WriteFileAtomic
// (temp file in the same directory, fsync, then atomic rename, including
// Windows MoveFileEx) rather than a plain os.WriteFile, so a reader (List,
// readSidecar, or an external process) never observes a partially written
// sidecar, and a crash mid-write during rotation leaves the previous
// sidecar (or none) rather than a truncated, unparseable one (review
// backlog item 34). readSidecar's callers already tolerate a missing
// sidecar (List skips a read error; Stop falls back to the in-memory
// snapshot), which is exactly what a reader can observe for the instant
// between a part's file appearing and its rotation's sidecar write landing.
func writeSidecar(dir, base string, info RecordingInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return domain.WriteFileAtomic(sidecarPath(dir, base), data, 0o600)
}

func readSidecar(dir, base string) (RecordingInfo, error) {
	data, err := os.ReadFile(sidecarPath(dir, base))
	if err != nil {
		return RecordingInfo{}, err
	}
	var info RecordingInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return RecordingInfo{}, err
	}
	info.Active = false
	return info, nil
}

// recorderParameterSets extracts the SPS and PPS NAL units from a keyframe
// access unit's Annex-B data, matching viewer_stream.go's parameterSets
// (duplicated here rather than shared: the two live on independent,
// small-surface code paths and neither should depend on the other's file).
func recorderParameterSets(annexB []byte) (sps, pps []byte, err error) {
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(annexB); err != nil {
		return nil, nil, fmt.Errorf("parse keyframe access unit: %w", err)
	}
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		switch h264c.NALUType(n[0] & 0x1f) {
		case h264c.NALUTypeSPS:
			sps = n
		case h264c.NALUTypePPS:
			pps = n
		}
	}
	if sps == nil || pps == nil {
		return nil, nil, fmt.Errorf("keyframe access unit is missing SPS or PPS")
	}
	return sps, pps, nil
}
