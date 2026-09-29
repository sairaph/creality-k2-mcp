package policy

import (
	"context"
	"testing"
)

// This file covers D2's "cancelled atomically by start_print before it
// sends anything" requirement (dev_docs/safety-architecture.md section 10):
// start_print must disarm any idle-heat watchdog for the printer it is
// about to start, and must do so before it sends PrintStart, without ever
// letting a watchdog problem block the print itself.

// start_print calls Disarm before sending PrintStart (order asserted via
// the shared event log, same pattern as the arm-order test).
func TestExecute_StartPrint_DisarmsBeforeSend(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	wd := newFakeWatchdog()
	wd.record = f.recordEvent
	f.setWatchdog(wd)
	p := New()
	printer := testPrinter(f, "start-print-disarm")

	res := mustExecute(t, p, f, printer, ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if !res.Accepted || res.Effect != "confirmed" {
		t.Fatalf("start_print result = %+v, want accepted+confirmed", res)
	}

	if identity, ok := wd.lastDisarm(); !ok || identity != "start-print-disarm" {
		t.Fatalf("lastDisarm = (%q, %v), want (\"start-print-disarm\", true)", identity, ok)
	}

	events := f.eventsSnapshot()
	disarmIdx, sendIdx := -1, -1
	for i, e := range events {
		switch e {
		case "watchdog_disarmed:start-print-disarm":
			disarmIdx = i
		case "start_reached_printing":
			sendIdx = i
		}
	}
	if disarmIdx == -1 {
		t.Fatal("expected a watchdog_disarmed:start-print-disarm event")
	}
	if sendIdx == -1 {
		t.Fatal("expected a start_reached_printing event")
	}
	if disarmIdx >= sendIdx {
		t.Fatalf("watchdog was disarmed at event %d, PrintStart sent at event %d: disarm must happen first", disarmIdx, sendIdx)
	}
}

// start_print with no watchdog wired up at all (production's "daemon not
// running" case) still starts normally: Disarm is only ever called when a
// watchdog exists, and its absence must never block a print from starting.
func TestExecute_StartPrint_NoWatchdogStillStarts(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter() // no watchdog set
	f.addFile("model.gcode")
	p := New()
	printer := testPrinter(f, "start-print-no-watchdog")

	res := mustExecute(t, p, f, printer, ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if !res.Accepted || res.Effect != "confirmed" {
		t.Fatalf("start_print result = %+v, want accepted+confirmed", res)
	}
}

// A Disarm error from the watchdog client is swallowed: a background daemon
// problem must never gate a print starting (stopping/starting a running
// job must never be blocked by an unrelated component).
type erroringDisarmWatchdog struct {
	*fakeWatchdog
}

func (w *erroringDisarmWatchdog) Disarm(ctx context.Context, identity string) error {
	_ = w.fakeWatchdog.Disarm(ctx, identity) // still record the call
	return context.DeadlineExceeded
}

func TestExecute_StartPrint_DisarmErrorDoesNotBlockStart(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	wd := &erroringDisarmWatchdog{fakeWatchdog: newFakeWatchdog()}
	f.setWatchdog(wd)
	p := New()
	printer := testPrinter(f, "start-print-disarm-error")

	res := mustExecute(t, p, f, printer, ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if !res.Accepted || res.Effect != "confirmed" {
		t.Fatalf("start_print result = %+v, want accepted+confirmed despite a Disarm error", res)
	}
	if wd.disarmCount() != 1 {
		t.Fatalf("Disarm called %d times, want 1", wd.disarmCount())
	}
}
