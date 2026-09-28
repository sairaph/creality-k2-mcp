package policy

import (
	"context"
	"testing"
	"time"
)

// This file implements dev_docs/safety-architecture.md section 6's canary
// tests: with the fake's own lifecycle methods called directly (bypassing
// Policy.Execute's precondition check entirely - the "test-only hook" the
// task describes, since dispatchSend itself never gates anything, only
// Execute's checkGate call does), resume-while-idle must record a purge and
// pause-while-idle must record the 140 C heat, proving the fake actually
// exercises the confirmed-live hazard rather than a guard-aware
// reimplementation of it. With Policy.Execute's own checks in place (the
// normal path every other test in this package uses), neither ever reaches
// the fake at all.

func TestCanary_Ungated_ResumeWhileIdleRecordsPurge(t *testing.T) {
	f := newFakePrinter()
	f.setHot(220) // above the ~170 C can_extrude threshold

	// Idle, unhomed, never paused: calling the fake's PrintResume directly
	// is exactly what a policy bug that forgot to gate resume_print would
	// do.
	if err := f.PrintResume(context.Background()); err != nil {
		t.Fatalf("PrintResume: %v", err)
	}
	if !f.hasEvent("homed") {
		t.Error("expected RESUME_EXTERNAL_PROCESS to home the unhomed axes before RESUME_BASE's own pause check ever runs")
	}
	if !f.hasEvent("purge:82.0") {
		t.Error("expected RESUME_EXTERNAL_PROCESS to purge 82 mm while hot, before RESUME_BASE's own pause check ever runs")
	}
	if !f.hasEvent("resume_aborted_not_paused") {
		t.Error("expected RESUME_BASE's own pause check to still abort the actual printing transition, since nothing was ever paused")
	}
	if f.printState != "standby" {
		t.Errorf("print_stats.state = %q, want standby (the actual resume-to-printing transition must not have happened)", f.printState)
	}
}

func TestCanary_Ungated_PauseWhileIdleRecordsHeat(t *testing.T) {
	f := newFakePrinter()
	f.setHomed(true)

	if err := f.PrintPause(context.Background()); err != nil {
		t.Fatalf("PrintPause: %v", err)
	}
	if !f.hasEvent("heat_140") {
		t.Error("expected PAUSE to heat the nozzle to 140 C even though nothing was printing (Creality's macro only checks is_paused, never print_stats.state)")
	}
	if !f.hasEvent("park") {
		t.Error("expected PAUSE to park since the printer was homed")
	}
}

func TestCanary_Gated_PolicyExecuteBlocksResumeWhileIdle(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setHot(220)
	f.setHomed(true) // even homed and hot, must still be refused while idle
	p := New()
	printer := testPrinter(f, "canary-resume")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
	if _, _, resumeCalls, _ := f.counts(); resumeCalls != 0 {
		t.Fatalf("PrintResume was called %d times, want 0: Policy.Execute must refuse before ever reaching the command layer", resumeCalls)
	}
	if f.hasEvent("homed") || f.hasEvent("purge:82.0") {
		t.Error("the gated path must never have homed or purged")
	}
}

func TestCanary_Gated_PolicyExecuteBlocksPauseWhileIdle(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setHomed(true)
	p := New()
	printer := testPrinter(f, "canary-pause")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionPausePrint, Params{}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
	if _, pauseCalls, _, _ := f.counts(); pauseCalls != 0 {
		t.Fatalf("PrintPause was called %d times, want 0: Policy.Execute must refuse before ever reaching the command layer", pauseCalls)
	}
	if f.hasEvent("heat_140") {
		t.Error("the gated path must never have heated to 140 C")
	}
}

// setTestHookSkipGate sets the test-only checkGate override
// (testHookSkipGate, execute.go) so Execute treats name as always gated
// open, for the duration of t only: t.Cleanup restores it to nil
// (production default) so no other test in this package is affected.
func setTestHookSkipGate(t *testing.T, name ActionName) {
	t.Helper()
	testHookSkipGate = func(n ActionName) bool { return n == name }
	t.Cleanup(func() { testHookSkipGate = nil })
}

// TestTestHookSkipGateNilByDefault pins that the hook is nil unless a test
// explicitly sets it: nothing outside a _test.go file in this package can
// assign it (it is unexported), and no test here leaves it set once it
// finishes (setTestHookSkipGate's t.Cleanup).
func TestTestHookSkipGateNilByDefault(t *testing.T) {
	if testHookSkipGate != nil {
		t.Fatal("testHookSkipGate must be nil by default")
	}
}

// TestCanary_ThroughExecute_ResumeWhileIdleRecordsPurge is the mutation-style
// canary dev_docs/safety-architecture.md section 6 asks for: with Execute's
// own checkGate disabled via the test-only hook, this drives the real
// proposal/token/send path through Policy.Execute itself against the fake
// printer while idle (not the fake's lifecycle methods called directly, as
// the "Ungated" tests above do), proving Execute's send path genuinely
// reaches the fake's hazardous RESUME_EXTERNAL_PROCESS purge once its own
// precondition is out of the way.
func TestCanary_ThroughExecute_ResumeWhileIdleRecordsPurge(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setHot(220) // above the ~170 C can_extrude threshold
	setTestHookSkipGate(t, ActionResumePrint)
	p := New()
	printer := testPrinter(f, "canary-through-execute-resume")

	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, "")
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}

	// The fake never reaches "printing" here (nothing was ever paused, so
	// RESUME_BASE's own check aborts the transition, exactly like the
	// "Ungated" test above): bound the confirming call so its settle poll
	// times out fast instead of waiting out the real 120s settle timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.Execute(ctx, f.deps(), printer, testSettings(), ActionResumePrint, Params{}, proposal.Token); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !f.hasEvent("purge:82.0") {
		t.Error("expected Execute, with the precondition disabled, to have sent PrintResume and recorded the 82 mm purge")
	}
}

// TestCanary_ThroughExecute_PauseWhileIdleRecordsHeat is
// TestCanary_ThroughExecute_ResumeWhileIdleRecordsPurge's pause_print
// counterpart: pause_print needs no proposal token, so one Execute call
// with the gate disabled is enough to reach the fake's PAUSE heat.
func TestCanary_ThroughExecute_PauseWhileIdleRecordsHeat(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setHomed(true)
	setTestHookSkipGate(t, ActionPausePrint)
	p := New()
	printer := testPrinter(f, "canary-through-execute-pause")

	if _, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionPausePrint, Params{}, ""); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !f.hasEvent("heat_140") {
		t.Error("expected Execute, with the precondition disabled, to have sent PrintPause and recorded the 140 C heat")
	}
}
