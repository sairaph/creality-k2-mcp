package policy

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Possibly-queued writes (live finding, supervised print 2026-09-29). Moonraker
// runs G-code in order, so a template write sent during a CFS purge waits behind
// the running macro; two timed-out M220s both ran later. A write whose delivery is
// unknown is never retried, and this file keeps a per-printer record of it so a
// later call cannot take a "nothing to do" shortcut while the late command is still
// in the queue: the record makes such a call send anyway (Klipper runs queued
// commands in order, so the newest write wins) and say what may still be ahead.

// queuedWriteMaxAge is an anti-hang bound on how long a record can suppress the
// shortcut, not a quota.
const queuedWriteMaxAge = 5 * time.Minute

// queuedWrite is one possibly-queued template write.
type queuedWrite struct {
	kind     string // the setting it changes: "speed", "flow", "fan:part", "nozzle", "bed", "exclude:<name>"
	action   ActionName
	target   string // human text, for example "M220 S125 (the ultrafast preset)"
	issuedAt time.Time
	job      *printerstate.JobIdentity
	// applied reports whether a fresh read shows the write's target.
	applied func(printerstate.Snapshot) bool
}

func (l *printerLock) setQueuedWrite(w *queuedWrite) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.queued = w
}

// clearQueuedKind drops the record when a later write of the same setting
// completed: Moonraker runs G-code in order, so a write that got its answer proves
// the earlier queued one already ran.
func (l *printerLock) clearQueuedKind(kind string) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.queued != nil && l.queued.kind == kind {
		l.queued = nil
	}
}

// outstandingQueued returns the record for kind while it is still in force, and
// clears it when a fresh read shows its target applied, when the job state changed
// (the print ended or was cancelled, or another job started), or after
// queuedWriteMaxAge. Only one record is kept per printer: the newest.
func (l *printerLock) outstandingQueued(kind string, snap printerstate.Snapshot) *queuedWrite {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	w := l.queued
	if w == nil {
		return nil
	}
	clear := func() *queuedWrite { l.queued = nil; return nil }
	if snap.Taken.Sub(w.issuedAt) > queuedWriteMaxAge {
		return clear()
	}
	if snap.PrintStats != nil {
		if s := snap.PrintStats.State; s != "printing" && s != "paused" {
			return clear()
		}
		if !sameJob(w.job, printerstate.JobIdentityFrom(snap)) {
			return clear()
		}
	}
	if w.applied != nil && w.applied(snap) {
		return clear()
	}
	if w.kind != kind {
		return nil
	}
	return w
}

// queuedNote is the reply note for an outstanding record.
func queuedNote(w *queuedWrite) string {
	return fmt.Sprintf("an earlier %s to %s may still be queued; this command runs after it", w.action, w.target)
}

// settingKind names the setting a template action changes.
func settingKind(name ActionName, params Params) string {
	switch name {
	case ActionSetSpeedFactor, ActionSetSpeedPreset:
		return "speed"
	case ActionSetFlowFactor:
		return "flow"
	case ActionSetFanSpeed:
		return "fan:" + string(params.Fan)
	case ActionSetNozzleTemperature:
		return "nozzle"
	case ActionSetBedTemperature:
		return "bed"
	case ActionExcludeObject:
		return "exclude:" + strings.ToLower(params.ObjectName)
	}
	return string(name)
}

// writeTarget describes a template action's target for notes.
func writeTarget(name ActionName, params Params) string {
	switch name {
	case ActionSetSpeedFactor:
		return fmt.Sprintf("M220 S%g", params.Percent)
	case ActionSetFlowFactor:
		return fmt.Sprintf("M221 S%g", params.Percent)
	case ActionSetFanSpeed:
		return fmt.Sprintf("%s fan %g%%", params.Fan, params.FanPercent)
	case ActionSetNozzleTemperature:
		return fmt.Sprintf("nozzle %g C", params.TargetC)
	case ActionSetBedTemperature:
		return fmt.Sprintf("bed %g C", params.TargetC)
	case ActionExcludeObject:
		return "exclude " + params.ObjectName
	}
	return string(name)
}

// deliveryUnknown reports whether a failed template write may still take effect:
// the request may have gone out before the error, so it may be queued behind a
// running macro (Moonraker answers only after the macro finishes) and run later. It
// is never retried; the settle read decides. Only a definite HTTP status answer
// from Moonraker, or an error where nothing was sent, is a definite "not applied":
//   - a *moonraker.Error with an HTTP status is Moonraker's own answer (a body-read
//     failure after a 2xx answer is the exception: the script already ran);
//   - invalid input, request build and encode errors, and dial failures (connection
//     refused, no such host, an unreachable network, a connect timeout) never
//     reached Moonraker;
//   - everything else (a response timeout, EOF, a reset, an unknown error, and a
//     cancellation, since a cancelled request may already have been written and the
//     printer still runs it) is a possibly-queued write.
func deliveryUnknown(err error) bool {
	if err == nil {
		return false
	}
	var merr *moonraker.Error
	if errors.As(err, &merr) {
		if merr.Status != 0 {
			return merr.Status >= 200 && merr.Status < 300 && strings.HasPrefix(merr.Body, "read response:")
		}
		if merr.Code == moonraker.CodeInvalidInput || strings.HasPrefix(merr.Body, "build request") || strings.HasPrefix(merr.Body, "encode request") {
			return false
		}
		return !nothingSent(merr.Body)
	}
	return !nothingSent(err.Error())
}

// nothingSent recognises transport errors that happen before any request bytes go
// out.
func nothingSent(msg string) bool {
	for _, s := range []string{"dial tcp", "dial udp", "dial unix", "connection refused", "no such host", "network is unreachable", "no route to host"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
