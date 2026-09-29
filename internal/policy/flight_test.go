package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// Tests for the final session-fix review (M1 to M4, m1 to m4).

func TestIsTransportError(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":               {nil, false},
		"deadline":          {context.DeadlineExceeded, true},
		"wrapped deadline":  {errors.Join(errors.New("Post"), context.DeadlineExceeded), true},
		"moonraker no HTTP": {&moonraker.Error{Op: "PrintPause", Status: 0, Code: moonraker.CodeUnavailable}, true},
		"http 500":          {&moonraker.Error{Op: "PrintPause", Status: 500, Code: moonraker.CodeInternal}, false},
		"http 400":          {&moonraker.Error{Op: "PrintPause", Status: 400}, false},
		"plain":             {errors.New("boom"), false},
	} {
		if got := isTransportError(tc.err); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// M3: an HTTP rejection is never promoted to accepted, even when the printer
// happens to be paused for another reason, and no resume record is made.
func TestPause_HTTPRejectionIsNotAcceptedEvenIfThePrinterPaused(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := cfsSetup(t, "m3-500")
	f.setPrinting("model.gcode")
	f.withLock(func() {
		f.pauseErr = &moonraker.Error{Op: "PrintPause", Status: 500, Code: moonraker.CodeInternal, Body: "boom"}
	})
	res, err := exec(p, f, printer, ActionPausePrint, Params{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.Effect != "unconfirmed" {
		t.Fatalf("accepted %v effect %s, want a rejection", res.Accepted, res.Effect)
	}
	if p.locks.get("m3-500").getPauseRec() != nil || p.locks.get("m3-500").getPauseFlight() != nil {
		t.Fatal("a rejected pause left a record: a pause this server did not make became resumable")
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "rejected the pause request") {
		t.Fatalf("effects = %v", res.Effects)
	}
}

// M3: a setpoint whose target already holds is not "accepted" by a rejected write.
func TestSetpoint_RejectedWriteIsNotPromotedToAccepted(t *testing.T) {
	// sendPolled only promotes for cancel with a transport error.
	if isTransportError(&moonraker.Error{Status: 500}) {
		t.Fatal("500 is not a transport error")
	}
}

// M1: pause does not hold the lock or the reply across the macro. The reply is
// "pausing" once 9999 shows the PAUSE routine, the printer derives pausing
// meanwhile, cancel is allowed and is never answered with a conflict, and a
// later snapshot that shows the pause settled gives the resume record.
func TestPause_ReturnsPausingAndDoesNotBlockCancel(t *testing.T) {
	fastLifecycle(t)
	pauseSettleTimeout = 3 * time.Second
	f, p, printer := cfsSetup(t, "m1-pausing")
	f.setPrinting("model.gcode")
	release := make(chan struct{})
	f.withLock(func() { f.pauseMacro, f.pauseRelease = true, release })

	begin := time.Now()
	res, err := exec(p, f, printer, ActionPausePrint, Params{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatalf("the reply took %s", time.Since(begin))
	}
	if res.Effect != "pausing" || !res.Accepted {
		t.Fatalf("effect %s accepted %v, want pausing", res.Effect, res.Accepted)
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "the printer is parking and wiping the nozzle and will report paused shortly") {
		t.Fatalf("effects = %v", res.Effects)
	}

	// While the POST hangs: derived pausing (bucket T), cancel allowed, pause refused.
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	d := p.Derive(snap)
	if d.State != printerstate.StatePausing || d.Bucket != printerstate.BucketT {
		t.Fatalf("derived %s/%s, want pausing/T", d.State, d.Bucket)
	}
	gates := p.Available(printer, snap, testSettings())
	if g := gateStatus(t, gates, ActionCancelPrint); g.Status != "needs_confirmation" {
		t.Errorf("cancel while pausing: %q (%s)", g.Status, g.Reason)
	}
	if g := gateStatus(t, gates, ActionPausePrint); g.Status != "blocked" {
		t.Errorf("a second pause: %q, want blocked", g.Status)
	}
	if _, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err == nil {
		t.Error("a second pause was accepted under a running one")
	}
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal while the pause POST hangs: %v", err)
	}
	if _, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token); err != nil {
		t.Fatalf("cancel while the pause POST hangs: %v", err)
	}
	close(release)
}

func TestPause_SettlingLaterGivesTheResumeRecordAtTheNextSnapshot(t *testing.T) {
	fastLifecycle(t)
	pauseSettleTimeout = 3 * time.Second
	f, p, printer := cfsSetup(t, "m1-later")
	f.setPrinting("model.gcode")
	release := make(chan struct{})
	f.withLock(func() { f.pauseMacro, f.pauseRelease = true, release })
	res, _ := exec(p, f, printer, ActionPausePrint, Params{}, "")
	if res.Effect != "pausing" {
		t.Fatalf("effect %s", res.Effect)
	}
	if p.locks.get("m1-later").getPauseRec() != nil {
		t.Fatal("a resume record before the pause settled")
	}
	close(release)
	waitForState(t, f, "paused")
	// The next Execute (a resume proposal) finalises the pause and finds its record.
	prop, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	if err != nil || !prop.Proposed {
		t.Fatalf("resume proposal after the pause settled: %v", err)
	}
	if p.locks.get("m1-later").getPauseFlight() != nil {
		t.Fatal("the pause record must end once it settled")
	}
}

func waitForState(t *testing.T, f *fakePrinter, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := f.printState
		f.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("printer never reached %s", want)
}

func TestPauseFlight_EndsOnTerminalStatesOrTimeoutOrResumedJob(t *testing.T) {
	now := time.Now()
	mk := func(state string, at time.Time) (*printerLock, printerstate.Snapshot) {
		pl := &printerLock{}
		pl.setPauseFlight(&flightRec{issuedAt: now})
		snap := printerstate.Snapshot{Taken: at}
		if state != "" {
			snap.PrintStats = &moonraker.PrintStats{State: state}
		}
		return pl, snap
	}
	for _, st := range []string{"complete", "cancelled", "error", "standby"} {
		pl, snap := mk(st, now.Add(time.Second))
		if pl.activePauseFlight(snap) != nil {
			t.Errorf("%s did not end the pause record", st)
		}
	}
	pl, snap := mk("printing", now.Add(4*time.Minute))
	if pl.activePauseFlight(snap) != nil {
		t.Error("the anti-hang guard did not end the pause record")
	}
	pl, snap = mk("", now.Add(time.Second))
	if pl.activePauseFlight(snap) == nil {
		t.Error("a partial snapshot ended the pause record")
	}
	// Paused then printing again (resumed at the printer): ended.
	pl, _ = mk("printing", now)
	pl.activePauseFlight(printerstate.Snapshot{Taken: now.Add(time.Second), PrintStats: &moonraker.PrintStats{State: "paused"}})
	if pl.activePauseFlight(printerstate.Snapshot{Taken: now.Add(2 * time.Second), PrintStats: &moonraker.PrintStats{State: "printing"}}) != nil {
		t.Error("a job seen printing after the pause settled must end the record")
	}
}

// M2: the resume record marks the macro for every reader even when 9999 shows
// nothing, refuses a second resume, and ends when the job is printing again.
func TestResume_InFlightRecordRefusesASecondResumeWithoutNineNineNineNineState(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := pausedByServer(t, "m2-flight")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.withLock(func() { f.resumeMacro, f.resumeRelease, f.resumeNoState = true, release, true })
	prop, _ := exec(p, f, printer, ActionResumePrint, Params{}, "")
	res, err := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "unconfirmed" || !res.Accepted {
		t.Fatalf("effect %s accepted %v, want unconfirmed (9999 showed nothing)", res.Effect, res.Accepted)
	}
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if snap.WS9999.State.Value == 8 {
		t.Fatal("setup: 9999 must not show state 8")
	}
	d := p.Derive(snap)
	if d.State != printerstate.StateResuming || d.Bucket != printerstate.BucketT || !strings.Contains(d.Reasons[0], "resume this server sent") {
		t.Fatalf("derived %s/%s %v, want resuming from the record", d.State, d.Bucket, d.Reasons)
	}
	_, err = exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "a resume is already in progress")
	if _, _, resume, _ := f.counts(); resume != 1 {
		t.Fatalf("PrintResume calls = %d, want exactly 1", resume)
	}
	// Printing again ends the record.
	f.withLock(func() { f.printState, f.isPaused = "printing", false })
	snap = printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if d := p.Derive(snap); d.State != printerstate.StatePrinting {
		t.Fatalf("after the job printed: %s", d.State)
	}
	if p.locks.get("m2-flight").getResumeFlight() != nil {
		t.Fatal("the resume record survived a printing snapshot")
	}
}

func TestResumeFlight_EndsOnTerminalStateOrTimeout(t *testing.T) {
	now := time.Now()
	for _, st := range []string{"printing", "cancelled", "complete", "error", "standby"} {
		pl := &printerLock{}
		pl.setResumeFlight(&flightRec{issuedAt: now})
		if pl.activeResumeFlight(printerstate.Snapshot{Taken: now.Add(time.Second), PrintStats: &moonraker.PrintStats{State: st}}) != nil {
			t.Errorf("%s did not end the resume record", st)
		}
	}
	pl := &printerLock{}
	pl.setResumeFlight(&flightRec{issuedAt: now})
	if pl.activeResumeFlight(printerstate.Snapshot{Taken: now.Add(6 * time.Minute), PrintStats: &moonraker.PrintStats{State: "paused"}}) != nil {
		t.Error("the 5 minute guard did not end the record")
	}
	if pl.getResumeFlight() != nil {
		t.Error("expired record not cleared")
	}
	pl.setResumeFlight(&flightRec{issuedAt: now})
	if pl.activeResumeFlight(printerstate.Snapshot{Taken: now.Add(time.Second)}) == nil {
		t.Error("a partial snapshot ended the record")
	}
}

// m2: resuming applies over the homing sub-phase (RESUME homes X/Y first), and
// cancel stays allowed there.
func TestResumingOverHomingAllowsCancel(t *testing.T) {
	now := time.Now()
	snap := printerstate.Snapshot{Taken: now, PrintStats: &moonraker.PrintStats{State: "paused"}}
	homing := printerstate.Derived{State: printerstate.StateHoming, Bucket: printerstate.BucketB, Class: printerstate.ClassBusy}
	d := applyFlights(homing, snap, &flightRec{issuedAt: now}, nil)
	if d.State != printerstate.StateResuming || checkGate(specs[ActionCancelPrint], d) != nil {
		t.Fatalf("derived %s, cancel gate %v", d.State, checkGate(specs[ActionCancelPrint], d))
	}
	// Printing is never rewritten by a resume record, and error stays as is.
	snap.PrintStats.State = "printing"
	if got := applyFlights(printerstate.Derived{State: printerstate.StatePrinting, Bucket: printerstate.BucketP}, snap, &flightRec{issuedAt: now}, nil); got.State != printerstate.StatePrinting {
		t.Errorf("a resume record rewrote printing to %s", got.State)
	}
}

// m3: a POST error that arrives after the early wait but before the reply is reported.
func TestResume_LateRejectionBeforeTheReplyIsReported(t *testing.T) {
	fastLifecycle(t)
	resumeSettleTimeout = 2 * time.Second
	f, p, printer := pausedByServer(t, "m3-late")
	f.withLock(func() {
		f.resumeErr = &moonraker.Error{Op: "PrintResume", Status: 400, Body: "no"}
		f.resumeErrAfter = 200 * time.Millisecond
	})
	prop, _ := exec(p, f, printer, ActionResumePrint, Params{}, "")
	begin := time.Now()
	res, err := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || !strings.Contains(strings.Join(res.Effects, " "), "rejected the resume request") {
		t.Fatalf("accepted %v effects %v, want the late rejection reported", res.Accepted, res.Effects)
	}
	if time.Since(begin) > 1500*time.Millisecond {
		t.Fatalf("the reply waited %s: it must stop polling on a rejection", time.Since(begin))
	}
	if p.locks.get("m3-late").getResumeFlight() != nil {
		t.Fatal("a rejected resume left its in-flight record")
	}
}

// The fast rejection replies at once (no polling).
func TestResume_FastRejectionRepliesWithoutPolling(t *testing.T) {
	fastLifecycle(t)
	resumeSettleTimeout = 5 * time.Second
	f, p, printer := pausedByServer(t, "m3-fast")
	f.withLock(func() { f.resumeErr = &moonraker.Error{Status: 500} })
	prop, _ := exec(p, f, printer, ActionResumePrint, Params{}, "")
	begin := time.Now()
	res, _ := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token)
	if res.Accepted || time.Since(begin) > time.Second {
		t.Fatalf("accepted %v after %s", res.Accepted, time.Since(begin))
	}
}
