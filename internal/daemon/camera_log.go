// This file wires internal/camera.EventLogger into the daemon's own
// *log.Logger (review backlog item 46): a Session opening or closing, a
// keyframe finally arriving, and any session-level error now reach
// daemon.log, instead of only ever being visible through a throwaway
// diagnostic tool's stdout (dev_docs/camera-keyframe-rca.md's "Live
// verification" section) or not at all
// (dev_docs/t11e-soak-report.md item 4: "No daemon-log visibility into
// camera/session/stream/recording lifecycle events").
package daemon

import (
	"log"
	"sync"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
)

// pliLogRateLimit is how often daemonCameraLogger summarizes repeated PLI
// (keyframe request) events for one host into a single log line, instead of
// writing one line per PLI. camera.Session.retryPLIUntilKeyframe (and, as
// of review backlog item 44, internal/daemon's own viewer/recorder retry
// loops) can resend a PLI every second or so for as long as a session waits
// for a keyframe, which would otherwise flood daemon.log during exactly the
// condition dev_docs/t11e-soak-report.md and
// dev_docs/camera-keyframe-rca.md describe: the printer's camera sitting
// idle, not responding, for minutes at a time. A package variable, not a
// constant, so a test can shrink it.
var pliLogRateLimit = 10 * time.Second

// daemonCameraLogger implements camera.EventLogger by writing to a
// *log.Logger (the daemon's own daemon.log). Every method here only ever
// receives a host string (an IP or hostname, never a credential), a
// duration, or an error - camera.EventLogger's interface carries nothing
// else, so this adapter can never log a token or API key by construction.
type daemonCameraLogger struct {
	log *log.Logger

	mu      sync.Mutex
	pending map[string]*pliWindow
}

// pliWindow tracks one host's PLI count since it was last flushed to the
// log (either because pliLogRateLimit elapsed, or because the session
// closed or finally received a keyframe).
type pliWindow struct {
	count       int
	windowStart time.Time
}

// newDaemonCameraLogger builds a daemonCameraLogger writing to l. l must
// not be nil.
func newDaemonCameraLogger(l *log.Logger) *daemonCameraLogger {
	return &daemonCameraLogger{log: l, pending: make(map[string]*pliWindow)}
}

var _ camera.EventLogger = (*daemonCameraLogger)(nil)

func (d *daemonCameraLogger) SessionOpened(host string) {
	d.log.Printf("camera: session opened host=%s", host)
}

func (d *daemonCameraLogger) SessionClosed(host string, err error) {
	d.flushPLI(host)
	if err != nil {
		d.log.Printf("camera: session closed host=%s err=%v", host, err)
		return
	}
	d.log.Printf("camera: session closed host=%s", host)
}

// KeyframeRequested records one PLI for host, flushing a summary line once
// pliLogRateLimit has elapsed since the current window started (review
// backlog item 46: "PLI sent (rate-limited in the log, e.g. summarise
// counts per 10 s)"). The very first PLI in a fresh window is never logged
// on its own; it is folded into the next flush (by time, or by
// SessionClosed/KeyframeReceived) so a session that only ever sends one or
// two PLIs before succeeding still gets a count, without a dedicated log
// line for every single one.
func (d *daemonCameraLogger) KeyframeRequested(host string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	w, ok := d.pending[host]
	if !ok {
		d.pending[host] = &pliWindow{count: 1, windowStart: now}
		return
	}
	w.count++
	if now.Sub(w.windowStart) >= pliLogRateLimit {
		d.log.Printf("camera: pli sent host=%s count=%d window=%s", host, w.count, pliLogRateLimit)
		w.count = 0
		w.windowStart = now
	}
}

// flushPLI logs and clears any pending PLI count for host, so the final
// (necessarily shorter than pliLogRateLimit) window is never silently
// dropped when a session closes or its keyframe finally arrives.
func (d *daemonCameraLogger) flushPLI(host string) {
	d.mu.Lock()
	w, ok := d.pending[host]
	delete(d.pending, host)
	d.mu.Unlock()
	if ok && w.count > 0 {
		d.log.Printf("camera: pli sent host=%s count=%d (final)", host, w.count)
	}
}

func (d *daemonCameraLogger) KeyframeReceived(host string, waited time.Duration, packets int) {
	d.flushPLI(host)
	d.log.Printf("camera: keyframe received host=%s waited=%s packets=%d", host, waited, packets)
}

func (d *daemonCameraLogger) Error(host string, err error) {
	d.log.Printf("camera: error host=%s err=%v", host, err)
}
