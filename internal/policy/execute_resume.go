package policy

import (
	"context"
	"fmt"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// pause_print and resume_print do not block the tool on Moonraker's HTTP answer
// (supervised session 2026-09-29): Moonraker replies only after the whole macro
// has run. The POST runs in a goroutine on a context that is not tied to the
// tool request (the client applies its own long lifecycle timeout, and a late
// answer is ignored), an in-flight record on the printer lock (flight.go) marks
// the action for every reader, and the per-printer lock is released BEFORE the
// reply polls, so a cancel issued meanwhile is never answered with a conflict.
// The reply polls for the first sign that the printer is acting.

var (
	// lifecycleEarlyWait is how long a fast failure of the POST is still awaited,
	// so a request Moonraker rejects at once is reported as such.
	lifecycleEarlyWait = 2 * time.Second
	// resumeSettleTimeout and pauseSettleTimeout bound the wait for the first sign.
	resumeSettleTimeout = 20 * time.Second
	pauseSettleTimeout  = 30 * time.Second
)

// flightOutcome is what one asynchronous send observed.
type flightOutcome struct {
	snap     printerstate.Snapshot
	derived  printerstate.Derived
	accepted bool
	sign     bool  // the printer showed the first sign of acting
	settled  bool  // the printer finished (paused, or printing again)
	postErr  error // the POST's own error, if it returned one before the reply
}

// runFlight sends post from a goroutine, waits briefly for a fast failure,
// releases the lock and polls until done reports a sign or settling, the
// timeout passes, or the POST returns a definitive HTTP rejection. A transport
// error or timeout from the POST leaves the effect unknown, so the poll goes on
// and the state decides; an HTTP status rejection ends it (final review M3, m3).
func (p *Policy) runFlight(ctx context.Context, deps Deps, printer domain.Printer, locks *acquiredLocks, post func(context.Context) error, timeout time.Duration, judge func(printerstate.Snapshot, printerstate.Derived) (sign, settled bool)) flightOutcome {
	done := make(chan error, 1)
	go func() { done <- post(context.Background()) }()

	out := flightOutcome{accepted: true}
	rejected := false
	early := func(err error) {
		out.postErr = err
		if err != nil && !isTransportError(err) {
			out.accepted, rejected = false, true
		}
	}
	select {
	case err := <-done:
		early(err)
		done = nil
	case <-time.After(lifecycleEarlyWait):
	case <-ctx.Done():
	}

	// The lock is not held while the reply waits for the printer.
	locks.release()

	deadline := time.Now().Add(timeout)
	for {
		out.snap = printerstate.Take(ctx, deps.stateDeps(), printer)
		out.derived = deriveFor(locks.pl, out.snap, nil)
		if rejected {
			return out
		}
		out.sign, out.settled = judge(out.snap, out.derived)
		if out.sign || out.settled {
			return out
		}
		if done != nil {
			select {
			case err := <-done:
				early(err)
				done = nil
				if rejected {
					return out
				}
			default:
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return out
		}
		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}
}

func (p *Policy) sendResume(ctx context.Context, deps Deps, printer domain.Printer, spec actionSpec, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)
	// The pause record is single-use (plan 8a.5), and the resume record marks the
	// macro as running for every reader until the job shows printing again.
	locks.pl.setPauseRec(nil)
	rec := &flightRec{issuedAt: time.Now(), job: printerstate.JobIdentityFrom(snap)}
	locks.pl.setResumeFlight(rec)

	out := p.runFlight(ctx, deps, printer, locks, deps.Moonraker.PrintResume, resumeSettleTimeout,
		func(s printerstate.Snapshot, d printerstate.Derived) (bool, bool) {
			printing := d.State == printerstate.StatePrinting
			return !printing && s.WS9999.State.Present && s.WS9999.State.Value == 8, printing
		})

	result := Result{
		Action: spec.Name, Accepted: out.accepted, Effect: "unconfirmed",
		Effects: append([]string(nil), spec.Effects...), Commands: spec.Commands, Before: before,
		After: printerstate.BuildStateBlock(out.snap, out.derived, nil), Printer: printer,
		Job: printerstate.JobIdentityFrom(out.snap),
	}
	switch {
	case out.settled:
		result.Effect = "confirmed"
		locks.pl.clearResumeFlightIf(rec)
	case out.sign:
		result.Effect = "resuming"
		target := "its stored target"
		if snap.PrinterParam != nil && snap.PrinterParam.HotendTemp != nil && *snap.PrinterParam.HotendTemp > 0 {
			target = fmt.Sprintf("%.0f C", *snap.PrinterParam.HotendTemp)
		}
		result.Effects = append(result.Effects, fmt.Sprintf(
			"resuming: the printer reheats to %s, purges and wipes (about 1-2 minutes); follow with get_printer_status", target))
	case !out.accepted:
		// A definitive rejection: nothing is in flight.
		locks.pl.clearResumeFlightIf(rec)
		result.Effects = append(result.Effects, "Moonraker rejected the resume request: "+out.postErr.Error())
	default:
		result.Effects = append(result.Effects, "the resume request was sent but the printer has not yet shown that it is resuming; follow with get_printer_status")
	}
	return result, nil
}

func (p *Policy) sendPause(ctx context.Context, deps Deps, printer domain.Printer, spec actionSpec, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)
	rec := &flightRec{issuedAt: time.Now(), job: printerstate.JobIdentityFrom(snap)}
	locks.pl.setPauseFlight(rec)

	out := p.runFlight(ctx, deps, printer, locks, deps.Moonraker.PrintPause, pauseSettleTimeout,
		func(s printerstate.Snapshot, d printerstate.Derived) (bool, bool) {
			paused := d.State == printerstate.StatePaused
			// 9999 state 5 or 6 while print_stats has not yet moved to paused: the
			// PAUSE routine is parking and wiping.
			return !paused && s.PrintStats != nil && s.PrintStats.State == "printing" && s.WS9999.State.Present &&
				(s.WS9999.State.Value == 5 || s.WS9999.State.Value == 6), paused
		})

	result := Result{
		Action: spec.Name, Accepted: out.accepted, Effect: "unconfirmed",
		Effects: withCFSNote(spec.Effects, spec.Name, derived), Commands: spec.Commands, Before: before,
		After: printerstate.BuildStateBlock(out.snap, out.derived, nil), Printer: printer,
		Job: printerstate.JobIdentityFrom(out.snap),
	}
	switch {
	case out.settled:
		result.Effect = "confirmed"
		// This server's pause settled: keep its resume record when the CFS was clean.
		finalizePause(ctx, deps, locks.pl, out.snap)
		if derived.CFSConnected {
			result.Effects = append(result.Effects, pauseRecordNote(locks.pl.getPauseRec() != nil))
		}
	case out.sign:
		result.Effect = "pausing"
		result.Effects = append(result.Effects, "pausing: the printer is parking and wiping the nozzle and will report paused shortly; follow with get_printer_status"+
			pauseRecordFutureNote(derived))
	case !out.accepted:
		locks.pl.clearPauseFlightIf(rec)
		result.Effects = append(result.Effects, "Moonraker rejected the pause request: "+out.postErr.Error())
	default:
		result.Effects = append(result.Effects, "the pause request was sent but the printer has not yet shown that it is pausing; follow with get_printer_status")
	}
	return result, nil
}

// pauseRecordFutureNote says, for a pause still settling with a CFS connected,
// that the resume record is made once it settles.
func pauseRecordFutureNote(derived printerstate.Derived) string {
	if !derived.CFSConnected {
		return ""
	}
	return ". With a CFS connected, a resume record is kept once the pause settles and the CFS reads clean; otherwise resume on the printer screen or in Creality Print"
}
