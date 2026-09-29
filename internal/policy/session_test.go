package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Tests for the supervised print session of 2026-09-29.

// A2: Moonraker answers pause only after the macro; the HTTP call can time out
// although the printer paused. A pause that timed out (a transport error) but
// settled is accepted (keeps its resume record) and the reply does not claim a
// rejection.
func TestPause_ConfirmedDespiteAnHTTPErrorIsAcceptedAndKeepsItsRecord(t *testing.T) {
	f, p, printer := cfsSetup(t, "a2-pause")
	f.setPrinting("model.gcode")
	f.withLock(func() { f.pauseErr = fmt.Errorf("Post: %w", context.DeadlineExceeded) })
	res, err := exec(p, f, printer, ActionPausePrint, Params{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "confirmed" || !res.Accepted {
		t.Fatalf("effect %s accepted %v, want confirmed and accepted", res.Effect, res.Accepted)
	}
	if p.locks.get("a2-pause").getPauseRec() == nil {
		t.Fatal("a confirmed pause lost its resume record")
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "a resume record was kept") {
		t.Fatalf("effects = %v", res.Effects)
	}
}

// D: resume does not block the tool on the HTTP answer; state 8 is the RESUME
// routine and the reply says resuming.
func TestResume_ReturnsResumingWhileTheMacroRuns(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := pausedByServer(t, "d-resuming")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.withLock(func() {
		f.resumeMacro, f.resumeRelease = true, release
		f.storedHotendTemp = 250
	})
	prop, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	res, err := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > 10*time.Second {
		t.Fatalf("the reply took %s: it must not wait for the macro", time.Since(begin))
	}
	if res.Effect != "resuming" || !res.Accepted {
		t.Fatalf("effect %s accepted %v, want resuming", res.Effect, res.Accepted)
	}
	txt := strings.Join(res.Effects, " ")
	if !strings.Contains(txt, "resuming: the printer reheats to 250 C, purges and wipes (about 1-2 minutes); follow with get_printer_status") {
		t.Fatalf("effects = %v", res.Effects)
	}
	if res.After.ActivityState != "resuming" || res.After.Bucket != "T" {
		t.Fatalf("after = %s/%s, want resuming/T", res.After.ActivityState, res.After.Bucket)
	}
	if p.locks.get("d-resuming").getPauseRec() != nil {
		t.Fatal("the pause record must be single-use")
	}
}

// During the macro every write is refused except cancel (stopping is never blocked).
func TestResuming_OnlyCancelIsAllowed(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := pausedByServer(t, "d-cancel")
	f.cfs9999(func(c *fakeCFS) { c.state = 8 })
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	d := p.Derive(snap)
	if d.State != printerstate.StateResuming || d.Bucket != printerstate.BucketT {
		t.Fatalf("derived %s/%s, want resuming/T", d.State, d.Bucket)
	}
	gates := p.Available(printer, snap, testSettings())
	if g := gateStatus(t, gates, ActionCancelPrint); g.Status != "needs_confirmation" {
		t.Errorf("cancel while resuming: %q (%s)", g.Status, g.Reason)
	}
	for _, name := range []ActionName{ActionResumePrint, ActionPausePrint, ActionSetNozzleTemperature, ActionSetFanSpeed, ActionStartPrint, ActionSetFilamentDefinition} {
		if g := gateStatus(t, gates, name); g.Status != "blocked" {
			t.Errorf("%s while resuming: %q, want blocked", name, g.Status)
		}
	}
	// A second resume is not proposed under a running one.
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "")

	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal while resuming: %v", err)
	}
	if _, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token); err != nil {
		t.Fatalf("cancel while resuming: %v", err)
	}
	if _, _, _, cancel := f.counts(); cancel != 1 {
		t.Fatalf("PrintCancel calls = %d, want 1", cancel)
	}
}

// Cancel is allowed while resuming or pausing (the macros queue it), never
// while cancelling.
func TestCancelException_IsForResumingAndPausingOnly(t *testing.T) {
	spec := specs[ActionCancelPrint]
	for _, st := range []string{printerstate.StateResuming, printerstate.StatePausing} {
		d := printerstate.Derived{State: st, Bucket: printerstate.BucketT, Class: printerstate.ClassTransitioning}
		if err := checkGate(spec, d); err != nil {
			t.Errorf("cancel refused while %s: %v", st, err)
		}
	}
	d := printerstate.Derived{State: printerstate.StateCancelling, Bucket: printerstate.BucketT, Class: printerstate.ClassTransitioning}
	if err := checkGate(spec, d); err == nil {
		t.Error("cancel allowed while cancelling")
	}
}

// A fast rejection of the POST is reported as not accepted; the tool does not poll.
func TestResume_FastRejectionIsNotAccepted(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := pausedByServer(t, "d-reject")
	f.withLock(func() { f.resumeErr = errors.New("400 rejected") })
	prop, _ := exec(p, f, printer, ActionResumePrint, Params{}, "")
	res, err := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.Effect != "unconfirmed" || !strings.Contains(strings.Join(res.Effects, " "), "rejected the resume request") {
		t.Fatalf("accepted %v effect %s effects %v", res.Accepted, res.Effect, res.Effects)
	}
}

// An instant resume (no CFS, or a fast printer) is still confirmed.
func TestResume_AlreadyPrintingIsConfirmed(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := pausedByServer(t, "d-confirmed")
	prop, _ := exec(p, f, printer, ActionResumePrint, Params{}, "")
	res, err := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token)
	if err != nil || res.Effect != "confirmed" || res.After.ActivityState != "printing" {
		t.Fatalf("effect %s state %s err %v", res.Effect, res.After.ActivityState, err)
	}
}

// fastLifecycle shrinks the early-failure wait and the settle timeouts so a test
// never spends real seconds on them.
func fastLifecycle(t *testing.T) {
	t.Helper()
	oe, or, op := lifecycleEarlyWait, resumeSettleTimeout, pauseSettleTimeout
	lifecycleEarlyWait, resumeSettleTimeout, pauseSettleTimeout = 60*time.Millisecond, 400*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { lifecycleEarlyWait, resumeSettleTimeout, pauseSettleTimeout = oe, or, op })
}
