package policy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// This file implements dev_docs/safety-architecture.md section 6's sequence
// tests, one per hazard in 10-hazard-analysis.md sections 2-3.

// TOCTOU between proposal and execute (10-hazard-analysis.md 3.3, 3.9):
// a concurrent actor changes the printer's state between the proposal call
// and the confirming call; the token's bound-field check must catch it.
func TestSequence_TOCTOUBetweenProposalAndConfirm(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setPaused()
	p := New()
	printer := testPrinter(f, "toctou-proposal")

	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, "")
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}

	// A concurrent actor (human at the touchscreen, another client) cancels
	// the print between the proposal and the confirming call.
	if err := f.PrintCancel(context.Background()); err != nil {
		t.Fatalf("external cancel: %v", err)
	}

	_, err = p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, proposal.Token)
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict (state changed since the proposal was issued)", err)
	}
}

// TOCTOU between the final snapshot and send: Execute takes its fresh
// snapshot and sends synchronously with no gap in between, so this reduces
// to "the snapshot Execute acts on must be the latest one available", which
// this test pins directly: a state change made immediately before an
// Execute call must be the one Execute's own preconditions see, never a
// value cached from an earlier call.
func TestSequence_FreshSnapshotReflectsLatestStateBeforeSend(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	p := New()
	printer := testPrinter(f, "fresh-snapshot")

	// A first, unrelated read-ish call (Available) must not poison a later
	// Execute call with a stale snapshot of its own.
	firstSnap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	_ = p.Available(printer, firstSnap, testSettings())

	f.setPaused() // state changes between that read and the write below
	res := mustExecute(t, p, f, printer, ActionSetLight, Params{On: true}, "")
	if res.Before.ActivityState != "paused" {
		t.Fatalf("Execute's own fresh snapshot reported state %q, want paused (the latest state, not an earlier cached one)", res.Before.ActivityState)
	}
}

// Retry after timeout returning unconfirmed without resending
// (10-hazard-analysis.md 3.2, 3.6): a write whose settle poll times out is
// reported unconfirmed, and Execute never re-issues the command itself
// within that call.
func TestSequence_TimeoutReturnsUnconfirmedWithoutResending(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setMute(true) // accepted, but the effect never lands
	p := New()
	printer := testPrinter(f, "retry-timeout")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := p.Execute(ctx, f.deps(), printer, testSettings(), ActionPausePrint, Params{}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Effect != "unconfirmed" {
		t.Fatalf("Effect = %q, want unconfirmed", res.Effect)
	}
	if !res.Accepted {
		t.Fatalf("Accepted = false, want true (the write itself returned no error, only the settle timed out)")
	}
	if _, pauseCalls, _, _ := f.counts(); pauseCalls != 1 {
		t.Fatalf("PrintPause called %d times, want exactly 1: Execute must never auto-resend after a settle timeout", pauseCalls)
	}
}

// Stale token: a token, once consumed (successfully or not), can never be
// used again (dev_docs/safety-architecture.md section 3.3 point 2, "single
// use").
func TestSequence_StaleTokenIsSingleUse(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setPaused()
	p := New()
	printer := testPrinter(f, "stale-token")

	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, "")
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	if _, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, proposal.Token); err != nil {
		t.Fatalf("first confirm: %v", err)
	}

	f.setPaused() // put the printer back in a state that would otherwise pass every other check
	_, err = p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, proposal.Token)
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeNotFound {
		t.Fatalf("err = %#v, want CodeNotFound (single-use token already consumed)", err)
	}
}

// Restarted token: a token issued by one Policy instance (one server
// process) is unknown to a different instance, exactly as an actual server
// restart would forget every in-memory proposal
// (dev_docs/safety-architecture.md section 3.3 point 2).
func TestSequence_RestartedServerForgetsTokens(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setPaused()
	printer := testPrinter(f, "restart-token")

	p1 := New()
	proposal, err := p1.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, "")
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}

	p2 := New()
	_, err = p2.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, proposal.Token)
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeNotFound {
		t.Fatalf("err = %#v, want CodeNotFound", err)
	}
	if perr.Message == "" {
		t.Fatal("expected the distinct not_found wording naming expiry/use/restart")
	}
}

// Two processes contending on the lock (dev_docs/safety-architecture.md
// section 3.4): a genuine OS-level holder of the cross-process file lock
// (simulating a second creality_k2_mcp process) makes Execute fail fast
// with a conflict rather than silently racing it.
func TestSequence_TwoProcessesContendForTheSameLock(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	printer := testPrinter(f, "two-process")

	identity := printer.Hostname // testPrinter's fake PrinterInfo answers with this same hostname
	path, err := domain.LockPath(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	external := flock.New(path)
	ok, err := external.TryLock()
	if err != nil || !ok {
		t.Fatalf("external flock.TryLock() = %v, %v", ok, err)
	}
	defer external.Unlock()

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = p.Execute(ctx, f.deps(), printer, testSettings(), ActionSetLight, Params{On: true}, "")
	perr, isErr := err.(*Error)
	if !isErr || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict", err)
	}
}

// CFS connected mid-print (D5): start, resume and every setpoint action are
// blocked, but pause and cancel stay available ("stopping must never be
// blocked").
func TestSequence_CFSConnectedMidPrintBlocksControlButNotStopping(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setCFSConnected(true)
	p := New()
	printer := testPrinter(f, "cfs-midprint")

	blocked := []struct {
		name   ActionName
		params Params
	}{
		{ActionSetNozzleTemperature, Params{TargetC: 210}},
		{ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 50}},
		{ActionSetSpeedFactor, Params{Percent: 100}},
		{ActionSetFlowFactor, Params{Percent: 100}},
	}
	for _, tc := range blocked {
		_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), tc.name, tc.params, "")
		perr, ok := err.(*Error)
		if !ok || perr.Code != CodeUnavailable {
			t.Errorf("%s err = %#v, want CodeUnavailable while CFS is connected", tc.name, err)
		}
	}

	// Pause stays available.
	pauseRes := mustExecute(t, p, f, printer, ActionPausePrint, Params{}, "")
	if pauseRes.After.ActivityState != "paused" {
		t.Fatalf("pause with CFS connected: after state = %q, want paused", pauseRes.After.ActivityState)
	}

	// Cancel stays available too.
	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal with CFS connected: %v", err)
	}
	cancelRes := mustExecute(t, p, f, printer, ActionCancelPrint, Params{}, proposal.Token)
	if cancelRes.After.ActivityState != "cancelled" {
		t.Fatalf("cancel with CFS connected: after state = %q, want cancelled", cancelRes.After.ActivityState)
	}
}

// start_print is blocked while CFS is connected, even while otherwise idle.
func TestSequence_CFSConnectedBlocksStartPrint(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	f.setCFSConnected(true)
	p := New()
	printer := testPrinter(f, "cfs-start")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionStartPrint, Params{Filename: "model.gcode"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
}

// Last object exclusion refused (10-hazard-analysis.md 3.10).
func TestSequence_LastObjectExclusionRefused(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.addObject("solo")
	p := New()
	printer := testPrinter(f, "sequence-last-object")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionExcludeObject, Params{ObjectName: "solo"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeInvalidInput {
		t.Fatalf("err = %#v, want CodeInvalidInput", err)
	}
}

// Current file delete/overwrite refused (10-hazard-analysis.md 2.3).
func TestSequence_CurrentFileDeleteAndOverwriteRefused(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("active.gcode")
	p := New()
	printer := testPrinter(f, "sequence-current-file")

	if _, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionDeleteGCodeFile, Params{Filename: "active.gcode"}, ""); err == nil {
		t.Fatal("expected deleting the current print file to be refused")
	}
	if _, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionUploadGCodeFile, Params{Filename: "active.gcode", LocalPath: "/tmp/x"}, ""); err == nil {
		t.Fatal("expected uploading over the current print file to be refused")
	}
}

// Bands (D1): a mid-print change outside the configured band is refused,
// never silently clamped.
func TestSequence_BandsRefuseOutsideRange(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.withLock(func() { f.nozzleTemp, f.nozzleTarget = 200, 200 })
	p := New()
	printer := testPrinter(f, "sequence-bands")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetNozzleTemperature, Params{TargetC: 225}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeInvalidInput {
		t.Fatalf("err = %#v, want CodeInvalidInput (15 C outside the default +-10 C band)", err)
	}
}

// Live cap unknown refused (10-hazard-analysis.md section 6 item 5).
func TestSequence_LiveCapUnknownRefusesTemperatureWrite(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setOmitProductParam(true)
	p := New()
	printer := testPrinter(f, "sequence-livecap")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetBedTemperature, Params{TargetC: 60}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
}

// Flow restore on failed start (D7).
func TestSequence_StartPrintRestoresFlowOnFailedStart(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	f.setExtrudeFactor(105) // left over from a previous print/session
	f.setStartPrintFails(true)
	p := New()
	printer := testPrinter(f, "sequence-flow-restore")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := p.Execute(ctx, f.deps(), printer, testSettings(), ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Effect != "unconfirmed" {
		t.Fatalf("Effect = %q, want unconfirmed (the print never reached printing)", res.Effect)
	}
	if res.StartPrintFlowRestored == nil || *res.StartPrintFlowRestored != 105 {
		t.Fatalf("StartPrintFlowRestored = %v, want 105", res.StartPrintFlowRestored)
	}
	f.mu.Lock()
	got := f.extrudeFactor
	f.mu.Unlock()
	if got != 1.05 {
		t.Fatalf("fake extrudeFactor after restore = %v, want 1.05", got)
	}
}
