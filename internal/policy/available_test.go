package policy

import (
	"context"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

func gateStatus(t *testing.T, gates []printerstate.ActionGate, name ActionName) printerstate.ActionGate {
	t.Helper()
	for _, g := range gates {
		if g.Name == string(name) {
			return g
		}
	}
	t.Fatalf("no gate found for action %s", name)
	return printerstate.ActionGate{}
}

func TestAvailableFor_IdleState(t *testing.T) {
	f := newFakePrinter()
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), testPrinterValue(f))
	derived := printerstate.DeriveActivityState(snap, nil)
	gates := AvailableFor(derived, testSettings())

	if g := gateStatus(t, gates, ActionStartPrint); g.Status != "available" {
		t.Errorf("start_print while idle: status = %q, want available", g.Status)
	}
	if g := gateStatus(t, gates, ActionPausePrint); g.Status != "blocked" {
		t.Errorf("pause_print while idle: status = %q, want blocked", g.Status)
	}
	if g := gateStatus(t, gates, ActionResumePrint); g.Status != "blocked" {
		t.Errorf("resume_print while idle: status = %q, want blocked", g.Status)
	}
	if g := gateStatus(t, gates, ActionSetLight); g.Status != "available" {
		t.Errorf("set_light while idle: status = %q, want available", g.Status)
	}
}

func TestAvailableFor_PausedState(t *testing.T) {
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setPaused()
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), testPrinterValue(f))
	derived := printerstate.DeriveActivityState(snap, nil)
	gates := AvailableFor(derived, testSettings())

	if g := gateStatus(t, gates, ActionResumePrint); g.Status != "needs_confirmation" {
		t.Errorf("resume_print while paused: status = %q, want needs_confirmation", g.Status)
	}
	if g := gateStatus(t, gates, ActionSetNozzleTemperature); g.Status != "blocked" {
		t.Errorf("set_nozzle_temperature while paused: status = %q, want blocked", g.Status)
	}
}

// D5 converted to the section 3.1 table (dev_docs/plan-v0.2.0.md): with a healthy
// CFS mid-print, the nozzle and flow are refused by their rules (V5), pause and
// cancel stay reachable, and the fan and speed rules allow a Known, error free
// CFS. The per-row and per-signal cases live in cfs_policy_test.go.
func TestAvailableFor_CFSConnectedBlocksControlKeepsStopping(t *testing.T) {
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setCFSConnected(true)
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), testPrinterValue(f))
	derived := printerstate.DeriveActivityState(snap, nil)
	gates := AvailableFor(derived, testSettings())

	if g := gateStatus(t, gates, ActionSetNozzleTemperature); g.Status != "blocked" {
		t.Errorf("set_nozzle_temperature with CFS connected mid-print: status = %q, want blocked", g.Status)
	}
	if g := gateStatus(t, gates, ActionSetFlowFactor); g.Status != "blocked" {
		t.Errorf("set_flow_factor with CFS connected: status = %q, want blocked", g.Status)
	}
	if g := gateStatus(t, gates, ActionSetFanSpeed); g.Status != "available" {
		t.Errorf("set_fan_speed with a healthy CFS mid-print: status = %q, want available", g.Status)
	}
	if g := gateStatus(t, gates, ActionPausePrint); g.Status != "available" {
		t.Errorf("pause_print with CFS connected: status = %q, want available", g.Status)
	}
	if g := gateStatus(t, gates, ActionCancelPrint); g.Status != "needs_confirmation" {
		t.Errorf("cancel_print with CFS connected: status = %q, want needs_confirmation", g.Status)
	}
}

func TestAvailableFor_NeverGatesAWrite(t *testing.T) {
	// Available's output must have no bearing on what Execute actually
	// allows: even if a caller ignored Available entirely, Execute's own
	// fresh-snapshot check is what matters. This test just pins that the
	// two independently computed gates agree for a representative case,
	// documenting that Available is display-only rather than something
	// Execute consults.
	setTestHome(t)
	f := newFakePrinter()
	p := New()
	printer := testPrinter(f, "available-not-a-gate")

	gates := p.Available(printer, printerstate.Take(context.Background(), f.deps().stateDeps(), printer), testSettings())
	if g := gateStatus(t, gates, ActionStartPrint); g.Status != "available" {
		t.Fatalf("status = %q, want available", g.Status)
	}

	// Execute still independently re-derives and enforces the same gate.
	f.addFile("model.gcode")
	res, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Effect != "confirmed" {
		t.Fatalf("Effect = %q, want confirmed", res.Effect)
	}
}

// A printer with allow_control off must show every action blocked, with the
// same reason Execute refuses with, even ones its state would allow (the
// idle set_light the live printer listed as available).
func TestGatesFor_AllowControlOffBlocksEverything(t *testing.T) {
	f := newFakePrinter()
	printer := testPrinterValue(f)
	printer.AllowControl = false
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	derived := printerstate.DeriveActivityState(snap, nil)

	want := checkAllowControl(ActionSetLight, printer).Message
	for _, g := range GatesFor(printer, derived, testSettings()) {
		if g.Status != "blocked" || g.Reason != want {
			t.Errorf("%s: status %q reason %q, want blocked with %q", g.Name, g.Status, g.Reason, want)
		}
	}

	printer.AllowControl = true
	if g := gateStatus(t, GatesFor(printer, derived, testSettings()), ActionSetLight); g.Status != "available" {
		t.Errorf("set_light with control on while idle: status %q, want available", g.Status)
	}
}

// testPrinterValue also configures f's PrinterInfo to answer with the same
// "avail-test" hostname (review backlog item 24: printerstate.Take now
// always reads printer/info, and DeriveActivityState fails closed to
// identity_unverified for a persisted hostname with nothing to verify it
// against), so these tests still exercise the bucket/state they name rather
// than the new identity check.
func testPrinterValue(f *fakePrinter) domain.Printer {
	f.setPrinterInfo("avail-test", nil)
	return domain.Printer{ID: "t", Name: "t", Host: "127.0.0.1", MoonrakerPort: 7125, Hostname: "avail-test", Enabled: true, AllowControl: true}
}
