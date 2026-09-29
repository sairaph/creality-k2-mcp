package policy

import (
	"fmt"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// The start window (dev_docs/plan-v0.2.0.md section 8a.1; review-2 MF1).
//
// After a CFS or spool start frame the printer runs a 3-4 minute self-test
// with print_stats still "standby" and virtual_sdcard inactive
// (dev_docs/cfs-print-start.md section 4.3). Left alone that derives as idle
// (bucket I), which would let every write through and leave cancel
// unavailable. Two things close the hole:
//
//   - printerstate derives "preparing" (bucket PP) from the signals that
//     survive a restart: 9999 withSelfTest not 100, or a non-identity
//     box.map.
//   - this file keeps a per-identity startInFlight record, set right before
//     the colorMatch frame and NOT cleared when Execute returns, so the
//     window is covered before either signal has appeared. applyStartWindow
//     merges it into the derived state and is used by BOTH Available and
//     Execute (Execute derives with a nil pending, so it must be applied
//     explicitly).
//
// The record lives in process memory only; that is acceptable for this
// single-user tool, and the signal rows still work across processes.

// startInFlightMaxAge is the anti-hang guard on the record (plan 8a.1: 15
// minutes after issue).
const startInFlightMaxAge = 15 * time.Minute

// stopDuringStartVerified gates the 9999 stop used to cancel during the start
// window (plan 8a.1). Moonraker's cancel is not known to stop the self-test
// (print_stats has no job yet), but Creality's own {"stop":1} was verified live
// in the supervised session (2026-09-29): 9999 state 7 (stopping) right after
// the frame, the current self-test step finishes (about 20 s), the heater
// targets go to 0, the map returns to identity and the printer is idle about a
// minute after the frame. A variable only so tests can exercise both settings;
// nothing in production assigns it.
var stopDuringStartVerified = true

// startWindowCancelRefusal is the one message for cancel during the start
// window, used by the gate (so the actions list and Execute agree) and by the
// parameter check.
// startWindowStopEffects and startWindowStopCommands describe what a confirmed
// cancel in the start window really does (the 9999 stop), for the proposal.
var (
	startWindowStopEffects = []string{
		"sends Creality's own 9999 stop message, because print_stats has no job yet and Moonraker's cancel does not stop the self-test",
		"the printer finishes its current self-test step (about 20 s), turns the heaters off, returns the filament map to identity and is idle within about a minute of the frame",
		"the reply returns as soon as the printer shows it is stopping (9999 state 7 or 4) and does not wait for the wind-down; follow with get_printer_status",
	}
	startWindowStopCommands = []string{"stop (port 9999)"}
)

const startWindowCancelRefusal = "the printer is in its print-start self-test, where a cancel from this server is not known to stop it: stop it on the printer screen"

// startInFlight is the record itself.
type startInFlight struct {
	filename string
	path     string
	mapping  string
	issuedAt time.Time
	// sentAt is when the start frame was sent (or may have been); zero until then.
	// The 9999 state 3 or 4 rule only applies after it (plus startStateGrace):
	// state 4 persists at rest from an earlier stopped job and only flips to 9
	// and 1 within about a second of the frame.
	sentAt time.Time
	// priorJob is the job identity at issue time. A stale "complete" or
	// "cancelled" left over from the previous job must not end the record;
	// only a settled state of a DIFFERENT job does.
	priorJob *printerstate.JobIdentity
}

func (l *printerLock) setStartRec(r *startInFlight) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.startRec = r
}

// markStartSent stamps the current record with the time its start frame was
// sent (or may have been).
func (l *printerLock) markStartSent() {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.startRec != nil {
		l.startRec.sentAt = time.Now()
	}
}

// startStateGrace is how long after the start frame a 9999 state of 3 or 4 is
// still the stale value from before the start.
const startStateGrace = 3 * time.Second

func (l *printerLock) startSentAt(rec *startInFlight) time.Time {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return rec.sentAt
}

func (l *printerLock) getStartRec() *startInFlight {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return l.startRec
}

// clearStartRecIf clears the record only if it is still the one judged (a
// compare-and-clear under the lock, safety review m4): a stale snapshot
// processed after a newer start installed a fresh record must not wipe it.
func (l *printerLock) clearStartRecIf(rec *startInFlight) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.startRec == rec {
		l.startRec = nil
	}
}

// startJobChanged reports whether print_stats now names a different job than
// before the start, comparing only what changes when print_stats really moves
// to a new job: filename, metadata uuid and start_time. The 9999 printId and
// the history head are deliberately not compared (safety review M3): the
// printer may assign them at the start frame, while print_stats still shows
// the previous job's complete or cancelled.
func startJobChanged(before, after *printerstate.JobIdentity) bool {
	if before == nil && after == nil {
		return false
	}
	if before == nil || after == nil {
		return true
	}
	return before.Filename != after.Filename || before.UUID != after.UUID || before.StartTime != after.StartTime
}

// activeStartRec returns the record if it is still in force as of snap, and
// clears it otherwise: 15 minutes after issue, or once a snapshot shows
// print_stats printing, paused or error, or complete or cancelled for a
// different job (filename, uuid or start_time) than the one before the start
// (plan 8a.1). A snapshot taken before the record existed never clears it.
func (l *printerLock) activeStartRec(snap printerstate.Snapshot) *startInFlight {
	rec := l.getStartRec()
	if rec == nil {
		return nil
	}
	if snap.Taken.Before(rec.issuedAt) {
		return rec
	}
	if snap.Taken.Sub(rec.issuedAt) > startInFlightMaxAge {
		l.clearStartRecIf(rec)
		return nil
	}
	// A stopped or failed start (9999 state 3 or 4), seen in a snapshot taken after
	// the start frame and its grace period, ends the record.
	if st := snap.WS9999.State; st.Present && (st.Value == 3 || st.Value == 4) {
		if sent := l.startSentAt(rec); !sent.IsZero() && snap.Taken.After(sent.Add(startStateGrace)) {
			l.clearStartRecIf(rec)
			return nil
		}
	}
	if snap.PrintStats != nil {
		switch snap.PrintStats.State {
		case "printing", "paused", "error":
			l.clearStartRecIf(rec)
			return nil
		case "complete", "cancelled":
			if startJobChanged(rec.priorJob, printerstate.JobIdentityFrom(snap)) {
				l.clearStartRecIf(rec)
				return nil
			}
		}
	}
	return rec
}

// applyStartWindow merges the in-flight record into derived. When the record
// is active, the printer is still in bucket I (or busy_command, bucket B) and
// print_stats has not moved on (standby, or the previous job's stale
// complete or cancelled), the state becomes preparing, bucket PP, class busy,
// with a reason naming the record. Printing, paused, transitions, homing and
// every unknown or error state are left exactly as derived. The signal half
// of 8a.1 is already in printerstate; this is the record half.
func applyStartWindow(derived printerstate.Derived, snap printerstate.Snapshot, rec *startInFlight) printerstate.Derived {
	if rec == nil || snap.PrintStats == nil {
		return derived
	}
	// Bucket I, busy_command, and filament_operation (bucket U: the CFS feeding
	// during the window must not take cancel away from the window, safety review
	// m3).
	if derived.Bucket != printerstate.BucketI && derived.State != printerstate.StateBusyCommand && derived.State != printerstate.StateFilamentOperation {
		return derived
	}
	switch snap.PrintStats.State {
	case "standby", "complete", "cancelled":
	default:
		return derived
	}
	out := derived
	out.State = printerstate.StatePreparing
	out.Bucket = printerstate.BucketPP
	out.Class = printerstate.ClassBusy
	out.StartWindow = true
	out.Reasons = append([]string{fmt.Sprintf(
		"a print start of %s was sent at %s and has not yet shown as printing (the self-test runs with print_stats still %s)",
		rec.filename, rec.issuedAt.Format(time.RFC3339), snap.PrintStats.State)}, derived.Reasons...)
	return out
}

// deriveFor is the one derivation every policy path uses: the printerstate
// derivation plus the start-window record. It also runs the pause-record
// lifecycle, since every snapshot for an identity is a chance to notice the
// job is no longer paused (plan 8a.5).
func deriveFor(pl *printerLock, snap printerstate.Snapshot, pending *printerstate.PendingAction) printerstate.Derived {
	derived := printerstate.DeriveActivityState(snap, pending)
	pl.observePause(snap)
	derived = applyStartWindow(derived, snap, pl.activeStartRec(snap))
	derived = applyFlights(derived, snap, pl.activeResumeFlight(snap), pl.activePauseFlight(snap))
	derived.PauseRecorded = pl.getPauseRec() != nil
	return derived
}

// inStartWindow reports whether derived is the start window: the self-test of a
// print start (printerstate's signal row, or the record applied above), not the
// ordinary START_PRINT prepare phase, where print_stats is printing and
// Moonraker's cancel works.
func inStartWindow(_ printerstate.Snapshot, derived printerstate.Derived) bool {
	return derived.StartWindow
}

// startWindowFileGuard refuses an upload-over or delete of the file whose
// start is in flight (plan 8a.1): print_stats.filename is empty until
// "Starting SD card print", so the ordinary current-print-file checks cannot
// protect the file whose filament map was just verified. When only the
// printerstate signals show the window (no record in this process, e.g. after
// a restart) the file is not known, so any upload-over or delete is refused
// for the duration.
func (p *Policy) startWindowFileGuard(identity string, snap printerstate.Snapshot, derived printerstate.Derived, filename, verb string) *Error {
	rec := p.locks.get(identity).getStartRec()
	if rec != nil && rec.filename == filename {
		return &Error{Code: CodeConflict, Message: fmt.Sprintf("cannot %s %s: a print start of this file is in flight", verb, filename)}
	}
	if rec == nil && inStartWindow(snap, derived) {
		return &Error{Code: CodeConflict, Message: fmt.Sprintf("cannot %s %s: the printer is in its print-start self-test and the file being started is not known", verb, filename)}
	}
	return nil
}
