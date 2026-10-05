package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Live bug (supervised print 2026-09-29): during the pre-print self-test the
// printer homes and probes the bed while print_stats still shows the previous
// job's complete. The start window must win over that motion, so cancel (the
// verified 9999 stop) stays available, every other write stays refused, and the
// actions list and Execute agree.

func motionSetup(t *testing.T, host string) (*fakePrinter, *Policy, domain.Printer) {
	t.Helper()
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("old.gcode")
	f.withLock(func() { f.printState = "complete"; f.sdActive = false })
	f.setHot(140)
	return f, New(), testPrinter(f, host)
}

func gateOf(gates []printerstate.ActionGate, name ActionName) printerstate.ActionGate {
	for _, g := range gates {
		if g.Name == string(name) {
			return g
		}
	}
	return printerstate.ActionGate{}
}

// assertWindowGates checks the derived state, the actions list, and that Execute
// gives the same answer for every action.
func assertWindowGates(t *testing.T, p *Policy, f *fakePrinter, printer domain.Printer, wantMotion string) {
	t.Helper()
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	d := p.Derive(snap)
	if d.State != printerstate.StatePreparing || d.Bucket != printerstate.BucketPP || !d.StartWindow {
		t.Fatalf("derived %s/%s window %v (%v), want the start window", d.State, d.Bucket, d.StartWindow, d.Reasons)
	}
	if !strings.Contains(strings.Join(d.Reasons, "; "), wantMotion) {
		t.Errorf("reasons %v do not name %q", d.Reasons, wantMotion)
	}
	gates := p.Available(printer, snap, testSettings())
	if g := gateOf(gates, ActionCancelPrint); g.Status == "blocked" {
		t.Fatalf("cancel_print is blocked in the start window: %s", g.Reason)
	}
	// Cancel through Execute: a proposal (the confirm would send the 9999 stop).
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil || !prop.Proposed {
		t.Fatalf("cancel_print proposal: %v %+v", err, prop)
	}
	if !hasEffect(prop, "9999") {
		t.Errorf("the proposal does not describe the 9999 stop: %v", prop.Effects)
	}
	// Every other write stays refused, and the list agrees with Execute.
	for _, tc := range []struct {
		name   ActionName
		params Params
	}{
		{ActionStartPrint, Params{Filename: "new.gcode"}},
		{ActionPausePrint, Params{}},
		{ActionResumePrint, Params{}},
		{ActionSetNozzleTemperature, Params{TargetC: 140}},
		{ActionSetBedTemperature, Params{TargetC: 60}},
		{ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 50}},
		{ActionSetSpeedFactor, Params{Percent: 100}},
		{ActionSetSpeedPreset, Params{Preset: "standard"}},
		{ActionSetFlowFactor, Params{Percent: 100}},
		{ActionExcludeObject, Params{ObjectName: "a"}},
	} {
		if g := gateOf(gates, tc.name); g.Status != "blocked" {
			t.Errorf("%s listed %q in the start window, want blocked", tc.name, g.Status)
		}
		if _, err := exec(p, f, printer, tc.name, tc.params, ""); err == nil {
			t.Errorf("%s was allowed in the start window", tc.name)
		}
	}
}

// Record active + the self-test homing.
func TestStartWindow_RecordWinsOverSelfTestHoming(t *testing.T) {
	f, p, printer := motionSetup(t, "sw-motion-rec-home")
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	p.locks.get(printer.Hostname).setStartRec(&startInFlight{filename: "new.gcode", issuedAt: snap.Taken, priorJob: printerstate.JobIdentityFrom(snap)})
	f.withLock(func() { f.homing = true })
	assertWindowGates(t, p, f, printer, "self-test homing")
}

// Record active + the self-test's bed probing (calibrating).
func TestStartWindow_RecordWinsOverSelfTestCalibrating(t *testing.T) {
	f, p, printer := motionSetup(t, "sw-motion-rec-cal")
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	p.locks.get(printer.Hostname).setStartRec(&startInFlight{filename: "new.gcode", issuedAt: snap.Taken, priorJob: printerstate.JobIdentityFrom(snap)})
	f.withLock(func() { f.leveling = 1 })
	assertWindowGates(t, p, f, printer, "self-test calibrating")
}

// Signals only (no record in this process, for example after a restart) + motion.
func TestStartWindow_SignalsOnlyWinOverSelfTestMotion(t *testing.T) {
	f, p, printer := motionSetup(t, "sw-motion-sig")
	f.cfs9999(func(c *fakeCFS) { c.withSelfTest = 40; c.deviceState = 1 }) // a live self-test
	f.withLock(func() { f.homing = true })
	assertWindowGates(t, p, f, printer, "self-test homing")
	f.withLock(func() { f.homing = false; f.leveling = 1 })
	assertWindowGates(t, p, f, printer, "self-test calibrating")
}

// No start window: a genuine homing or calibration derives as before, and the
// stale complete alone is idle.
func TestStartWindow_GenuineHomingOutsideAWindowIsUnchanged(t *testing.T) {
	f, p, printer := motionSetup(t, "sw-motion-none")
	f.withLock(func() { f.homing = true })
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	d := p.Derive(snap)
	if d.State != printerstate.StateHoming || d.Bucket != printerstate.BucketB || d.StartWindow {
		t.Fatalf("homing without a window: %s/%s window %v", d.State, d.Bucket, d.StartWindow)
	}
	if g := gateOf(p.Available(printer, snap, testSettings()), ActionCancelPrint); g.Status != "blocked" {
		t.Errorf("cancel_print outside a print and a window = %q, want blocked as before", g.Status)
	}
	f.withLock(func() { f.homing = false; f.leveling = 1 })
	snap = printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if d := p.Derive(snap); d.State != printerstate.StateCalibrating || d.StartWindow {
		t.Fatalf("calibrating without a window: %s window %v", d.State, d.StartWindow)
	}
}

// A record never shadows printing or paused, even while homing is reported.
func TestStartWindow_RecordNeverShadowsPrintingOrPaused(t *testing.T) {
	for _, state := range []string{"printing", "paused"} {
		f, p, printer := motionSetup(t, "sw-motion-"+state)
		snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
		p.locks.get(printer.Hostname).setStartRec(&startInFlight{filename: "new.gcode", issuedAt: snap.Taken, priorJob: printerstate.JobIdentityFrom(snap)})
		f.setPrinting("new.gcode")
		if state == "paused" {
			f.setPaused()
		}
		f.withLock(func() { f.homing = true })
		snap = printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
		if d := p.Derive(snap); d.StartWindow {
			t.Errorf("%s + homing derived the start window: %s", state, d.State)
		}
	}
}
