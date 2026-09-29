package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Review 3: possibly-queued record, the evidence rule, the error classification,
// and the start-flow notes.

// deliveryUnknown against the production error shapes (*moonraker.Error with
// Status 0 for transport errors, the wrapper client.go builds).
func TestDeliveryUnknown_ClassifiesTheProductionErrorShapes(t *testing.T) {
	mk := func(status int, code moonraker.Code, body string) error {
		return &moonraker.Error{Op: "RunTemplate", Status: status, Code: code, Body: body}
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"nil":                         {nil, false},
		"response timeout":            {mk(0, moonraker.CodeUnavailable, "Post \"http://p/printer/gcode/script\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), true},
		"EOF after the write":         {mk(0, moonraker.CodeUnavailable, "Post \"http://p\": EOF"), true},
		"connection reset":            {mk(0, moonraker.CodeUnavailable, "read tcp 1.2.3.4:5->6.7.8.9:7125: connection reset by peer"), true},
		"wrapped":                     {fmt.Errorf("send: %w", mk(0, moonraker.CodeUnavailable, "context deadline exceeded")), true},
		"connection refused":          {mk(0, moonraker.CodeUnavailable, "dial tcp 1.2.3.4:7125: connect: connection refused"), false},
		"connect timeout":             {mk(0, moonraker.CodeUnavailable, "dial tcp 1.2.3.4:7125: i/o timeout"), false},
		"no such host":                {mk(0, moonraker.CodeUnavailable, "dial tcp: lookup k2.local: no such host"), false},
		"template render":             {mk(0, moonraker.CodeInvalidInput, "argument \"name\" does not match"), false},
		"build request":               {mk(0, moonraker.CodeInternal, "build request: bad url"), false},
		"encode request":              {mk(0, moonraker.CodeInternal, "encode request: bad json"), false},
		"HTTP 500":                    {mk(500, moonraker.CodeInternal, "Internal Server Error"), false},
		"HTTP 400":                    {mk(400, moonraker.CodeInvalidInput, "bad"), false},
		"body read failure after 200": {mk(200, moonraker.CodeInternal, "read response: unexpected EOF"), true},
		"plain error":                 {errors.New("boom"), true},
		"cancelled request":           {context.Canceled, true},
		"deadline":                    {context.DeadlineExceeded, true},
	} {
		if got := deliveryUnknown(tc.err); got != tc.want {
			t.Errorf("%s: deliveryUnknown = %v, want %v", name, got, tc.want)
		}
	}
}

// --- M1: the possibly-queued record ---

// The live sequence: ultrafast times out behind a purge; a follow-up for the
// CURRENT preset (standard) must send instead of returning no_change, because the
// queued M220 S125 will land and it must run after it.
func TestQueuedRecord_FollowUpForTheCurrentPresetSendsInsteadOfNoChange(t *testing.T) {
	f, p, printer := speedSetup(t, "q-record")
	shortCap(t)
	f.withLock(func() { f.timeoutLost = true })
	first, err := exec(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"), "")
	if err != nil || first.Effect != "unconfirmed" || !hasEffect(first, queuedPendingNote) {
		t.Fatalf("first call = %v %+v", err, first.Effect)
	}
	// The status still reads 100% (standard). The printer is no longer busy, so the
	// follow-up goes through, but it must send.
	f.withLock(func() { f.timeoutLost = false })
	before := m220Count(f)
	second, err := exec(p, f, printer, ActionSetSpeedPreset, preset("standard"), "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Effect == "no_change" || m220Count(f) != before+1 {
		t.Fatalf("follow-up = %s, M220 sent %d times: it must send (the late M220 S125 is still ahead of it)", second.Effect, m220Count(f)-before)
	}
	if !hasEffect(second, "an earlier set_speed_preset to M220 S125 (the ultrafast preset) may still be queued; this command runs after it") {
		t.Errorf("no note about the outstanding write: %v", second.Effects)
	}
}

// Without an outstanding record the shortcut is unchanged.
func TestQueuedRecord_NoRecordKeepsNoChange(t *testing.T) {
	f, p, printer := speedSetup(t, "q-none")
	res := mustPreset(t, p, f, printer, "standard")
	if res.Effect != "no_change" || m220Count(f) != 0 {
		t.Fatalf("effect=%s m220=%d", res.Effect, m220Count(f))
	}
}

// The same record also produces the note for a plain speed-factor call.
func TestQueuedRecord_SpeedFactorGetsTheNote(t *testing.T) {
	f, p, printer := speedSetup(t, "q-factor")
	shortCap(t)
	f.withLock(func() { f.timeoutLost = true })
	// set_speed_factor's own settle window is 10 s; an already short-circuit is not
	// available there, so use the preset to create the record and the factor tool to read it.
	if _, err := exec(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"), ""); err != nil {
		t.Fatal(err)
	}
	f.withLock(func() { f.timeoutLost = false })
	res, err := exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 110}, "")
	if err != nil || !hasEffect(res, "may still be queued; this command runs after it") {
		t.Fatalf("%v %v", err, res.Effects)
	}
}

// The record is cleared when a fresh read shows its target, when the job state
// changes, and after five minutes.
func TestQueuedRecord_Clearing(t *testing.T) {
	now := time.Now()
	job := &printerstate.JobIdentity{Filename: "a.gcode", UUID: "u"}
	mkSnap := func(state string, factor float64) printerstate.Snapshot {
		s := printerstate.Snapshot{Taken: now.Add(time.Second)}
		s.PrintStats = &moonraker.PrintStats{State: state, Filename: "a.gcode"}
		s.VirtualSDCard = &moonraker.VirtualSDCard{CurPrintData: &moonraker.CurPrintData{Filename: "a.gcode", Metadata: &moonraker.CrealityPrintMetadata{UUID: "u"}}}
		s.GCodeMove = &moonraker.GCodeMove{SpeedFactor: &factor}
		return s
	}
	rec := func() *queuedWrite {
		return &queuedWrite{kind: "speed", action: ActionSetSpeedPreset, target: "M220 S125", issuedAt: now, job: job,
			applied: func(s printerstate.Snapshot) bool {
				return s.GCodeMove != nil && s.GCodeMove.SpeedFactor != nil && floatEqual(*s.GCodeMove.SpeedFactor*100, 125)
			}}
	}
	pl := &printerLock{}
	pl.setQueuedWrite(rec())
	if pl.outstandingQueued("speed", mkSnap("printing", 1.0)) == nil {
		t.Fatal("record not outstanding while the target is not applied")
	}
	if pl.outstandingQueued("flow", mkSnap("printing", 1.0)) != nil {
		t.Error("a different setting saw the record")
	}
	if pl.outstandingQueued("speed", mkSnap("printing", 1.25)) != nil || pl.queued != nil {
		t.Error("a read showing the target did not clear the record")
	}
	for _, state := range []string{"complete", "cancelled", "standby", "error"} {
		pl.setQueuedWrite(rec())
		if pl.outstandingQueued("speed", mkSnap(state, 1.0)) != nil || pl.queued != nil {
			t.Errorf("print_stats %s did not clear the record", state)
		}
	}
	pl.setQueuedWrite(rec())
	other := mkSnap("printing", 1.0)
	other.PrintStats.Filename = "b.gcode"
	other.VirtualSDCard.CurPrintData.Filename = "b.gcode"
	if pl.outstandingQueued("speed", other) != nil {
		t.Error("another job did not clear the record")
	}
	pl.setQueuedWrite(rec())
	late := mkSnap("printing", 1.0)
	late.Taken = now.Add(queuedWriteMaxAge + time.Second)
	if pl.outstandingQueued("speed", late) != nil {
		t.Error("the age bound did not clear the record")
	}
}

// --- M2: the evidence rule ---

func speedNotes(res Result) string { return strings.Join(res.Effects, " | ") }

// Live case (call 12): Silent on, a swap already put the factor at 100, exit to
// standard, the M220 timed out. The target held before the send: neutral, not accepted.
func TestEvidence_SilentExitWhenTheTargetAlreadyHeld(t *testing.T) {
	f, p, printer := speedSetup(t, "ev-live")
	f.setSilent(true) // captured 100%
	f.withLock(func() { f.speedFactor = 1.0; f.timeoutLost = true })
	res := mustPreset(t, p, f, printer, "standard")
	if res.Effect != "confirmed" || res.Accepted {
		t.Fatalf("effect=%s accepted=%v, want confirmed and not accepted", res.Effect, res.Accepted)
	}
	if !hasEffect(res, neutralConfirmNote) || hasEffect(res, queuedRanNote) {
		t.Errorf("notes = %s", speedNotes(res))
	}
}

// Silent's exit restores the target itself (saved 125, target ultrafast): the read
// is explained by the exit, so no promotion even though the M220 never ran.
func TestEvidence_SilentExitRestoresTheTarget(t *testing.T) {
	f, p, printer := speedSetup(t, "ev-restore")
	f.withLock(func() { f.speedFactor = 1.25 })
	f.setSilent(true) // saved 125%, running at 50%
	f.withLock(func() { f.timeoutLost = true })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "confirmed" || res.Accepted || !hasEffect(res, neutralConfirmNote) || hasEffect(res, queuedRanNote) {
		t.Fatalf("effect=%s accepted=%v notes=%s", res.Effect, res.Accepted, speedNotes(res))
	}
}

// Silent's saved factor unreadable: the exit cannot be ruled out, so no promotion.
func TestEvidence_UnreadableSavedFactorIsNoEvidence(t *testing.T) {
	f, p, printer := speedSetup(t, "ev-nosaved")
	f.setSilent(true)
	f.withLock(func() { f.omitSavedFactor = true; f.timeoutApplied = true })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "confirmed" || res.Accepted || !hasEffect(res, neutralConfirmNote) {
		t.Fatalf("effect=%s accepted=%v notes=%s", res.Effect, res.Accepted, speedNotes(res))
	}
}

// Target differs from the pre-send factor and from Silent's restore: promoted.
func TestEvidence_GenuineRunIsStillPromoted(t *testing.T) {
	f, p, printer := speedSetup(t, "ev-genuine")
	f.setSilent(true) // saved 100%
	f.withLock(func() { f.timeoutApplied = true })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "confirmed" || !res.Accepted || !hasEffect(res, queuedRanNote) {
		t.Fatalf("effect=%s accepted=%v notes=%s", res.Effect, res.Accepted, speedNotes(res))
	}
	// No Silent exit involved: promoted when the target differs from the pre-send factor.
	f2, p2, printer2 := speedSetup(t, "ev-genuine2")
	f2.withLock(func() { f2.timeoutApplied = true })
	if res := mustPreset(t, p2, f2, printer2, "ultrafast"); !res.Accepted || !hasEffect(res, queuedRanNote) {
		t.Fatalf("no-exit case: accepted=%v notes=%s", res.Accepted, speedNotes(res))
	}
}

// The neutral wording is the same in the setpoint path (already matched).
func TestEvidence_SetpointAlreadyMatchedUsesTheNeutralNote(t *testing.T) {
	f, p, printer := speedSetup(t, "ev-setpoint")
	f.withLock(func() { f.timeoutLost = true })
	res, err := exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 100}, "")
	if err != nil || res.Effect != "confirmed" || res.Accepted || !hasEffect(res, neutralConfirmNote) {
		t.Fatalf("%v effect=%s accepted=%v notes=%s", err, res.Effect, res.Accepted, speedNotes(res))
	}
}

// --- minors: classification through the fakes ---

// A dial error (nothing sent) is a definite not-sent: no queued note, and the
// preset's one retry happens.
func TestClassification_DialErrorIsRetriedAndNotCalledQueued(t *testing.T) {
	f, p, printer := speedSetup(t, "cls-dial")
	shortCap(t)
	f.withLock(func() { f.refusedTemplate = true })
	res, err := exec(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"), "")
	if err != nil {
		t.Fatal(err)
	}
	if templateCallCount(f) != 2 {
		t.Errorf("template calls = %d, want the call and one retry", templateCallCount(f))
	}
	if hasEffect(res, "may still be queued") || res.Effect == "confirmed" {
		t.Errorf("effect=%s notes=%s", res.Effect, speedNotes(res))
	}
}

// A definite HTTP error on a setpoint: no queued note, no record, not accepted.
func TestClassification_HTTPErrorOnASetpointIsNotQueued(t *testing.T) {
	f, p, printer := speedSetup(t, "cls-http")
	f.withLock(func() { f.http500Template = true })
	res, err := exec(p, f, printer, ActionSetFlowFactor, Params{Percent: 105}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || hasEffect(res, "queued") {
		t.Errorf("accepted=%v notes=%s", res.Accepted, speedNotes(res))
	}
	if p.locks.get(printer.Hostname).queued != nil {
		t.Error("a definite HTTP error left a possibly-queued record")
	}
}

// --- start-flow notes ---

func plainStartSetup(t *testing.T, host string) (*fakePrinter, *Policy) {
	t.Helper()
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	f.setExtrudeFactor(80)
	return f, New()
}

// start_print without a CFS: the M221 reset times out, the print is not started, and the
// reply says the reset may be queued and that the restore goes behind it (and what
// happens when the restore itself gets no answer).
func TestStartPrint_ResetTimeoutNotes(t *testing.T) {
	f, p := plainStartSetup(t, "st-reset")
	printer := testPrinter(f, "st-reset")
	f.withLock(func() { f.timeoutLost = true })
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	res, err := p.Execute(ctx, f.deps(), printer, testSettings(), ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if start, _, _, _ := f.counts(); start != 0 {
		t.Fatalf("PrintStart was called although the reset got no answer")
	}
	if !hasEffect(res, "the M221 flow reset got no answer and was not retried") || !hasEffect(res, "restore below is sent behind the reset") {
		t.Errorf("reset notes missing: %v", res.Effects)
	}
	if !hasEffect(res, "the flow factor restore got no answer and was not retried") {
		t.Errorf("restore timeout note missing: %v", res.Effects)
	}
	if hasEffect(res, "still 100%") {
		t.Errorf("claims the flow is still 100%%: %v", res.Effects)
	}
}

// start_print with a CFS: the reset error says it was not retried and may be queued.
func TestStartCFS_ResetTimeoutMessage(t *testing.T) {
	f, p, printer := startSetup(t, "st-cfs-reset")
	f.setExtrudeFactor(80)
	prop := propose(t, p, f, printer, startParams())
	f.withLock(func() { f.timeoutLost = true })
	_, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	perr := wantErr(t, err, CodeUnavailable, "could not reset the flow factor")
	if !strings.Contains(perr.Message, "was not retried") || !strings.Contains(perr.Message, "may still be queued") {
		t.Errorf("message = %s", perr.Message)
	}
}

// start_print with a CFS, map mismatch, the restore times out: it does not claim the flow is
// still 100%.
func TestStartCFS_RestoreTimeoutWording(t *testing.T) {
	f, p, printer := startSetup(t, "st-cfs-restore")
	f.setExtrudeFactor(80)
	f.cfs9999(func(c *fakeCFS) { c.forcedMapping = map[string]string{"T1A": "T1D"} })
	prop := propose(t, p, f, printer, startParams())
	f.withLock(func() { f.timeoutLostAfter = 1 }) // the reset (call 1) works, the restore does not
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEffect(res, "the flow factor restore to 80% got no answer and was not retried") || hasEffect(res, "it is still 100%") {
		t.Errorf("effects = %v", res.Effects)
	}
}

// --- round A hardening ---

// A record never shadows print states, and the derived state is exactly the plain one.
func TestStartWindow_RecordLeavesEveryPrintStateExactlyAsDerived(t *testing.T) {
	for _, state := range []string{"printing", "paused", "error"} {
		f, p, printer := motionSetup(t, "sw-exact-"+state)
		snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
		p.locks.get(printer.Hostname).setStartRec(&startInFlight{filename: "new.gcode", issuedAt: snap.Taken, priorJob: printerstate.JobIdentityFrom(snap)})
		f.setPrinting("new.gcode")
		switch state {
		case "paused":
			f.setPaused()
		case "error":
			f.withLock(func() { f.printState = "error" })
		}
		f.withLock(func() { f.homing = true })
		snap = printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
		got := p.Derive(snap)
		plain := printerstate.DeriveActivityState(snap, nil)
		if got.State != plain.State || got.Bucket != plain.Bucket || got.StartWindow {
			t.Errorf("%s + homing + record: %s/%s window %v, plain derivation %s/%s", state, got.State, got.Bucket, got.StartWindow, plain.State, plain.Bucket)
		}
	}
}

// --- re-review R1, R2 ---

// R1: a later write of the same setting that got its answer clears the record
// (FIFO: it proves the earlier queued one already ran), so nothing afterwards says
// "may still be queued" and the no_change shortcut is back.
func TestQueuedRecord_LaterAnsweredWriteClearsIt(t *testing.T) {
	f, p, printer := speedSetup(t, "q-clear")
	shortCap(t)
	f.withLock(func() { f.timeoutLost = true })
	if _, err := exec(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"), ""); err != nil {
		t.Fatal(err)
	}
	pl := p.locks.get(printer.Hostname)
	if pl.queued == nil {
		t.Fatal("no record after the timeout")
	}
	f.withLock(func() { f.timeoutLost = false })
	second := mustPreset(t, p, f, printer, "standard") // sends, and is answered
	if second.Effect != "confirmed" || pl.queued != nil {
		t.Fatalf("effect=%s record=%v: an answered write must clear the record", second.Effect, pl.queued)
	}
	third := mustPreset(t, p, f, printer, "standard")
	if third.Effect != "no_change" || hasEffect(third, "may still be queued") {
		t.Fatalf("after the clear: effect=%s notes=%s", third.Effect, speedNotes(third))
	}
	// A different tool of the same setting clears it too.
	f.withLock(func() { f.timeoutLost = true })
	_, _ = exec(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"), "")
	f.withLock(func() { f.timeoutLost = false })
	_, err := exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 110}, "")
	if err != nil || pl.queued != nil {
		t.Fatalf("%v record=%v", err, pl.queued)
	}
	after, _ := exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 115}, "")
	if hasEffect(after, "may still be queued") {
		t.Errorf("a stale note after the clear: %v", after.Effects)
	}
}

// R2: a pre-empted set_speed_preset whose M220 may already have gone out records a
// possibly-queued entry, so a follow-up for the current preset sends.
func TestQueuedRecord_PreemptedM220LeavesARecord(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "q-preempt")
	f.withLock(func() { f.blockTemplate = true })
	wait := runAsync(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"))
	waitFor(t, "the M220 in flight", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.templateCalls > 0
	})
	preemptWith(t, p, f, printer, func() bool { return true })
	if res, _ := wait(); res.Effect != "preempted" {
		t.Fatalf("effect = %s", res.Effect)
	}
	pl := p.locks.get(printer.Hostname)
	if pl.queued == nil || pl.queued.kind != "speed" {
		t.Fatalf("record = %+v, want a speed entry for the interrupted M220", pl.queued)
	}
}
