package policy

import (
	"context"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Field feedback item 1 (issue #3, plan-v0.3.1.md R2): a start stopped on the
// printer screen, or never taken, must not hold the start record for 15
// minutes once the printer is positively at rest.

func restSnap(taken time.Time) printerstate.Snapshot {
	zero := 0.0
	snap := printerstate.Snapshot{
		Taken:           taken,
		PrintStats:      &moonraker.PrintStats{State: "standby"},
		Extruder:        &moonraker.Extruder{Target: &zero},
		HeaterBed:       &moonraker.HeaterBed{Target: &zero},
		WS9999Reachable: true,
	}
	snap.WS9999.State = crealityws.Int{Value: 0, Present: true}
	snap.WS9999.DeviceState = crealityws.Int{Value: 0, Present: true}
	return snap
}

var restDerived = printerstate.Derived{State: printerstate.StateIdle, Bucket: printerstate.BucketI}

func TestStartRecord_EndsWhenThePrinterIsAtRestAfterTheGrace(t *testing.T) {
	now := time.Now()
	pl := &printerLock{}
	rec := &startInFlight{filename: "a", issuedAt: now, sentAt: now}
	pl.setStartRec(rec)
	if pl.activeStartRec(restSnap(now.Add(startRestGrace/2)), restDerived) == nil {
		t.Fatal("ended inside the rest grace")
	}
	if pl.activeStartRec(restSnap(now.Add(startRestGrace+time.Second)), restDerived) != nil || pl.getStartRec() != nil {
		t.Fatal("not ended at rest after the grace")
	}
}

func TestStartRecord_StaysUnlessPositivelyAtRest(t *testing.T) {
	now := time.Now()
	later := now.Add(startRestGrace + time.Minute)
	f := func(v float64) *float64 { return &v }
	cases := map[string]func(s *printerstate.Snapshot, d *printerstate.Derived, r *startInFlight){
		"frame not sent yet": func(_ *printerstate.Snapshot, _ *printerstate.Derived, r *startInFlight) { r.sentAt = time.Time{} },
		"busy_command (bucket B)": func(_ *printerstate.Snapshot, d *printerstate.Derived, _ *startInFlight) {
			d.Bucket = printerstate.BucketB
		},
		"preparing (a signal)": func(_ *printerstate.Snapshot, d *printerstate.Derived, _ *startInFlight) {
			d.Bucket = printerstate.BucketPP
		},
		"nozzle held hot":       func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) { s.Extruder.Target = f(140) },
		"bed target":            func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) { s.HeaterBed.Target = f(70) },
		"nozzle target unknown": func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) { s.Extruder = nil },
		"bed target unknown":    func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) { s.HeaterBed.Target = nil },
		"9999 unreachable":      func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) { s.WS9999Reachable = false },
		"deviceState absent": func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) {
			s.WS9999.DeviceState = crealityws.Int{}
		},
		"deviceState 1": func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) {
			s.WS9999.DeviceState = crealityws.Int{Value: 1, Present: true}
		},
		"state 1 with deviceState": func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) {
			s.WS9999.State = crealityws.Int{Value: 1, Present: true}
		},
		"state 7 (stopping)": func(s *printerstate.Snapshot, _ *printerstate.Derived, _ *startInFlight) {
			s.WS9999.State = crealityws.Int{Value: 7, Present: true}
		},
	}
	for name, change := range cases {
		pl := &printerLock{}
		rec := &startInFlight{filename: "a", issuedAt: now, sentAt: now}
		snap := restSnap(later)
		d := restDerived
		change(&snap, &d, rec)
		pl.setStartRec(rec)
		if pl.activeStartRec(snap, d) == nil {
			t.Errorf("%s: the record ended", name)
		}
	}
}

// The whole path: a record from a start that the printer never shows (the
// owner stopped it on the screen and the printer sits at state 0) stops making
// the printer "preparing" once the rest grace has passed.
func TestStartRecord_ScreenStopToStateZeroFreesThePrinter(t *testing.T) {
	now := time.Now()
	pl := &printerLock{}
	pl.setStartRec(&startInFlight{filename: "a", issuedAt: now, sentAt: now})
	snap := restSnap(now.Add(startRestGrace + time.Second))
	d := applyStartWindow(restDerived, snap, pl.activeStartRec(snap, restDerived))
	if d.StartWindow || d.Bucket != printerstate.BucketI {
		t.Fatalf("%s/%s window %v, want bucket I", d.State, d.Bucket, d.StartWindow)
	}
}

// Review m3: with the feed row above cancelled (R3), a CFS feed right after a
// cancel derives filament_operation; the cancel itself has still settled.
func TestCancelSettlesDespiteAFeedAfterIt(t *testing.T) {
	f, p, printer := cfsSetup(t, "cancel-feed")
	f.setPrinting("model.gcode")
	f.setFeedState(2) // a feed during the print stays printing, and continues after the cancel
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal: %v", err)
	}
	res, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token)
	if err != nil || !res.Accepted || res.Effect != "confirmed" {
		t.Fatalf("cancel: %v %+v", err, res)
	}
	if res.After.ActivityState != printerstate.StateFilamentOperation {
		t.Fatalf("after = %s, want filament_operation (the feed)", res.After.ActivityState)
	}
}

// Review m6: the whole derivation path. A record whose start the printer never
// took (stopped on the screen, printer quiet) ends through deriveFor itself.
func TestStartRecord_EndsThroughDeriveFor(t *testing.T) {
	f, _, printer := cfsSetup(t, "rest-derive")
	pl := &printerLock{}
	sent := time.Now().Add(-startRestGrace - time.Second)
	pl.setStartRec(&startInFlight{filename: "a", issuedAt: sent, sentAt: sent})
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	d := deriveFor(pl, snap, nil)
	if d.StartWindow || d.Bucket != printerstate.BucketI || pl.getStartRec() != nil {
		t.Fatalf("%s/%s window %v record %v, want bucket I and no record", d.State, d.Bucket, d.StartWindow, pl.getStartRec())
	}
	// The same with the printer still heating: the record holds the window.
	f.withLock(func() { f.nozzleTarget = 140 })
	pl.setStartRec(&startInFlight{filename: "a", issuedAt: sent, sentAt: sent})
	snap = printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if d := deriveFor(pl, snap, nil); !d.StartWindow || d.Bucket != printerstate.BucketPP {
		t.Fatalf("heating: %s/%s window %v, want the start window", d.State, d.Bucket, d.StartWindow)
	}
}
