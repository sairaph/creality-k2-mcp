package policy

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// Pause and resume in flight (supervised session 2026-09-29, final review M1,
// M2, m2). Moonraker answers pause, resume and cancel only after the whole macro
// has run: about 17 s for the K2 PAUSE (park, wipe) and 60 to 75 s for RESUME
// (reheat, purge, wipe). Both tools therefore send their POST from a goroutine
// and reply as soon as the printer shows the first sign; what happens in the
// meantime is tracked by one record each on the printer lock, applied in
// deriveFor so that Execute and Available both derive "pausing" or "resuming"
// (bucket T, cancel allowed, everything else refused) whether or not 9999 shows
// a state, and so that a second pause or resume is refused while one is running.

const (
	// pauseFlightMax and resumeFlightMax are the anti-hang guards on the records.
	pauseFlightMax  = 3 * time.Minute
	resumeFlightMax = 5 * time.Minute
)

// flightRec is one in-flight pause or resume.
type flightRec struct {
	issuedAt time.Time
	job      *printerstate.JobIdentity
	// sawPaused (pause only) is set once a snapshot showed the job paused: the
	// pause settled. The record then lives on only until it has been finalised
	// (the resume record made), and ends if the job is seen printing again.
	sawPaused bool
}

func (l *printerLock) setResumeFlight(r *flightRec) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.resumeFlight = r
}

func (l *printerLock) getResumeFlight() *flightRec {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return l.resumeFlight
}

func (l *printerLock) setPauseFlight(r *flightRec) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.pauseFlight = r
}

func (l *printerLock) getPauseFlight() *flightRec {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return l.pauseFlight
}

func (l *printerLock) clearResumeFlightIf(r *flightRec) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.resumeFlight == r {
		l.resumeFlight = nil
	}
}

func (l *printerLock) clearPauseFlightIf(r *flightRec) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.pauseFlight == r {
		l.pauseFlight = nil
	}
}

func (l *printerLock) markPauseSettled(r *flightRec) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	r.sawPaused = true
}

func (l *printerLock) pauseSettled(r *flightRec) bool {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return r.sawPaused
}

// activeResumeFlight returns the resume record if it is still in force as of
// snap: it ends after resumeFlightMax, when print_stats shows anything but
// paused (printing, or a terminal state), and never because of a snapshot taken
// before it was issued or a partial one (print_stats missing).
func (l *printerLock) activeResumeFlight(snap printerstate.Snapshot) *flightRec {
	rec := l.getResumeFlight()
	if rec == nil || snap.Taken.Before(rec.issuedAt) {
		return rec
	}
	if snap.Taken.Sub(rec.issuedAt) > resumeFlightMax {
		l.clearResumeFlightIf(rec)
		return nil
	}
	if snap.PrintStats != nil && snap.PrintStats.State != "paused" {
		l.clearResumeFlightIf(rec)
		return nil
	}
	return rec
}

// activePauseFlight returns the pause record if it is still in force: it ends
// after pauseFlightMax, on a terminal print_stats state (complete, cancelled,
// error, standby), and when the job is seen printing again after it had paused.
func (l *printerLock) activePauseFlight(snap printerstate.Snapshot) *flightRec {
	rec := l.getPauseFlight()
	if rec == nil || snap.Taken.Before(rec.issuedAt) {
		return rec
	}
	if snap.Taken.Sub(rec.issuedAt) > pauseFlightMax {
		l.clearPauseFlightIf(rec)
		return nil
	}
	if snap.PrintStats == nil {
		return rec
	}
	switch snap.PrintStats.State {
	case "paused":
		l.markPauseSettled(rec)
	case "printing":
		if l.pauseSettled(rec) {
			l.clearPauseFlightIf(rec)
			return nil
		}
	default:
		l.clearPauseFlightIf(rec)
		return nil
	}
	return rec
}

// applyFlights merges the pause and resume records into derived. A resume in
// flight while print_stats is still paused derives "resuming" over paused,
// homing, calibrating and busy_command (the RESUME macro homes X/Y first, and
// cancel must stay allowed through that); a pause in flight while print_stats
// is still printing derives "pausing" over printing. Both are bucket T, class
// transitioning. Nothing else is rewritten.
func applyFlights(derived printerstate.Derived, snap printerstate.Snapshot, resume, pause *flightRec) printerstate.Derived {
	if snap.PrintStats == nil {
		return derived
	}
	out := derived
	switch {
	case resume != nil && snap.PrintStats.State == "paused" &&
		(derived.State == printerstate.StatePaused || derived.State == printerstate.StateHoming ||
			derived.State == printerstate.StateCalibrating || derived.State == printerstate.StateBusyCommand):
		out.State, out.Bucket, out.Class = printerstate.StateResuming, printerstate.BucketT, printerstate.ClassTransitioning
		out.Reasons = append([]string{"a resume this server sent at " + resume.issuedAt.Format(time.RFC3339) +
			" has not finished (the RESUME routine runs for 1-2 minutes with print_stats still paused)"}, derived.Reasons...)
	case pause != nil && snap.PrintStats.State == "printing" && derived.State == printerstate.StatePrinting:
		out.State, out.Bucket, out.Class = printerstate.StatePausing, printerstate.BucketT, printerstate.ClassTransitioning
		out.Reasons = append([]string{"a pause this server sent at " + pause.issuedAt.Format(time.RFC3339) +
			" has not settled (the PAUSE routine parks and wipes for about 20 s with print_stats still printing)"}, derived.Reasons...)
	}
	return out
}

// finalizePause makes the resume record for a pause this server sent once a
// snapshot shows it settled (paused, same job), under the same clean-CFS rule as
// before (recordPause), and ends the pause record. It is called from Execute
// (which has the clients) with every fresh snapshot, and from the pause tool
// itself when its own poll sees the pause settle.
func finalizePause(ctx context.Context, deps Deps, pl *printerLock, snap printerstate.Snapshot) {
	rec := pl.getPauseFlight()
	if rec == nil || snap.PrintStats == nil || snap.PrintStats.State != "paused" ||
		snap.PauseResume == nil || snap.PauseResume.IsPaused == nil || !*snap.PauseResume.IsPaused {
		return
	}
	if snap.Taken.Before(rec.issuedAt) {
		return
	}
	if startJobChanged(rec.job, printerstate.JobIdentityFrom(snap)) {
		pl.clearPauseFlightIf(rec)
		return
	}
	pl.markPauseSettled(rec)
	recordPause(ctx, deps, pl, snap, printerstate.DeriveActivityState(snap, nil))
	pl.clearPauseFlightIf(rec)
}

// isTransportError reports whether err is a transport failure or a timeout, as
// opposed to a definitive HTTP status answer from Moonraker. Only a transport
// error leaves it unknown whether the request took effect, so only then may a
// confirmed effect promote a send to accepted (final review M3).
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	var merr *moonraker.Error
	if errors.As(err, &merr) {
		return merr.Status == 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var nerr net.Error
	return errors.As(err, &nerr)
}
