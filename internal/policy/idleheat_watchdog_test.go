package policy

import (
	"context"
	"testing"
)

// This file implements dev_docs/safety-architecture.md section 10 D2's
// enforcement: the idle-heat watchdog is armed by this package itself,
// synchronously, before the heater command is ever sent, and an idle-bucket
// temperature write is refused outright when the watchdog is not wired up,
// not alive, or refuses to arm.

// No watchdog wired up at all (production's "daemon not running" case):
// refused, and the heater command is never sent.
func TestExecute_SetTemperature_IdleRefusedWithoutWatchdog(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter() // idle baseline, no watchdog set
	p := New()
	printer := testPrinter(f, "idle-no-watchdog")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetNozzleTemperature, Params{TargetC: 200}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
	f.mu.Lock()
	got := f.nozzleTarget
	f.mu.Unlock()
	if got != 0 {
		t.Fatalf("nozzleTarget = %v, want 0 (the heater command must never have been sent)", got)
	}
	if f.hasEvent("heater_set:extruder:200") {
		t.Error("the heater command must never have reached the fake")
	}
}

// Watchdog wired up but not alive: refused the same way as no watchdog at
// all, and the heater command is never sent.
func TestExecute_SetTemperature_IdleRefusedWhenWatchdogDead(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	wd := newFakeWatchdog()
	wd.setAlive(false)
	f.setWatchdog(wd)
	p := New()
	printer := testPrinter(f, "idle-dead-watchdog")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetBedTemperature, Params{TargetC: 60}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
	if wd.armCount() != 0 {
		t.Fatalf("watchdog armed %d times, want 0 (never call Arm on a dead watchdog)", wd.armCount())
	}
	if f.hasEvent("heater_set:heater_bed:60") {
		t.Error("the heater command must never have reached the fake")
	}
}

// The watchdog is armed before the heater command is sent, not after
// (order asserted via a shared event log).
func TestExecute_SetTemperature_IdleArmedBeforeSend(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	wd := newFakeWatchdog()
	wd.record = f.recordEvent
	f.setWatchdog(wd)
	p := New()
	printer := testPrinter(f, "idle-arm-order")

	res := mustExecute(t, p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 200}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("result = %+v, want confirmed", res)
	}

	events := f.eventsSnapshot()
	armIdx, sendIdx := -1, -1
	for i, e := range events {
		switch e {
		case "watchdog_armed:extruder":
			armIdx = i
		case "heater_set:extruder:200":
			sendIdx = i
		}
	}
	if armIdx == -1 {
		t.Fatal("expected a watchdog_armed:extruder event")
	}
	if sendIdx == -1 {
		t.Fatal("expected a heater_set:extruder:200 event")
	}
	if armIdx >= sendIdx {
		t.Fatalf("watchdog was armed at event %d, heater command sent at event %d: arm must happen first", armIdx, sendIdx)
	}
}

// A watchdog that is alive but refuses to arm blocks the write entirely:
// nothing is sent.
func TestExecute_SetTemperature_IdleArmFailureRefusesWithoutSending(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	wd := newFakeWatchdog()
	wd.setArmFails(true)
	f.setWatchdog(wd)
	p := New()
	printer := testPrinter(f, "idle-arm-fails")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetNozzleTemperature, Params{TargetC: 200}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
	f.mu.Lock()
	got := f.nozzleTarget
	f.mu.Unlock()
	if got != 0 {
		t.Fatalf("nozzleTarget = %v, want 0 (arm failed, nothing should have been sent)", got)
	}
	if f.hasEvent("heater_set:extruder:200") {
		t.Error("the heater command must never have reached the fake")
	}
}

// A target of 0 while idle (turning the heater off) needs no watchdog at
// all: it succeeds even with nothing wired up, and arms nothing.
func TestExecute_SetTemperature_IdleTargetZeroNeedsNoWatchdog(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter() // idle baseline, no watchdog set
	p := New()
	printer := testPrinter(f, "idle-target-zero")

	res := mustExecute(t, p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 0}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("result = %+v, want confirmed", res)
	}
	if res.IdleHeatArm != nil {
		t.Fatalf("IdleHeatArm = %+v, want nil (target 0 needs no watchdog)", res.IdleHeatArm)
	}
}

// Mid-print temperature changes never touch the watchdog at all, even one
// that would refuse every arm call.
func TestExecute_SetTemperature_PrintingBucketNeverTouchesWatchdog(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.withLock(func() { f.nozzleTemp, f.nozzleTarget = 210, 210 })
	wd := newFakeWatchdog()
	wd.setArmFails(true) // would refuse every arm call, proving it is never consulted
	f.setWatchdog(wd)
	p := New()
	printer := testPrinter(f, "midprint-no-watchdog")

	res := mustExecute(t, p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 215}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("result = %+v, want confirmed", res)
	}
	if wd.armCount() != 0 {
		t.Fatalf("watchdog armed %d times, want 0 (mid-print must never touch the watchdog)", wd.armCount())
	}
	if res.IdleHeatArm != nil {
		t.Fatalf("IdleHeatArm = %+v, want nil (mid-print, not idle)", res.IdleHeatArm)
	}
}
