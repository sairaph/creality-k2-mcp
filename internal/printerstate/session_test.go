package printerstate

import (
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// Supervised session 2026-09-29: virtual_sdcard.bed_mesh_calibate_state stays
// true for the whole print after the self-test levels the bed. A printing or
// paused job must never derive calibrating from it (pause and cancel would be
// refused), while an idle printer with the flag still counts as calibrating.
func TestBedMeshFlagDoesNotMakeARunningPrintCalibrating(t *testing.T) {
	printing := syntheticIdle()
	printing.PrintStats.State = "printing"
	printing.PrintStats.PrintDuration = 10
	printing.VirtualSDCard.IsActive = boolPtr(true)
	printing.VirtualSDCard.BedMeshCalibrateState = boolPtr(true)
	if d := DeriveActivityState(printing, nil); d.State != StatePrinting || d.Bucket != BucketP {
		t.Fatalf("printing with the flag: %s/%s, want printing/P", d.State, d.Bucket)
	}
	paused := syntheticIdle()
	paused.PrintStats.State = "paused"
	paused.PauseResume.IsPaused = boolPtr(true)
	paused.VirtualSDCard.BedMeshCalibrateState = boolPtr(true)
	if d := DeriveActivityState(paused, nil); d.State != StatePaused || d.Bucket != BucketZ {
		t.Fatalf("paused with the flag: %s/%s, want paused/Z", d.State, d.Bucket)
	}
	idle := syntheticIdle()
	idle.VirtualSDCard.BedMeshCalibrateState = boolPtr(true)
	if d := DeriveActivityState(idle, nil); d.State != StateCalibrating {
		t.Fatalf("idle with the flag: %s, want calibrating (a real levelling)", d.State)
	}
}

func TestStartWindowSignalsFromNineNineNineNineState(t *testing.T) {
	for _, st := range []int{9, 1, 7} {
		s := syntheticIdle()
		s.WS9999.State = present(st)
		d := DeriveActivityState(s, nil)
		if d.State != StatePreparing || d.Bucket != BucketPP || !d.StartWindow {
			t.Errorf("state %d with print_stats standby: %s/%s window %v, want the start window", st, d.State, d.Bucket, d.StartWindow)
		}
	}
	// State 4 (aborted) persists at rest and 0 is idle: neither is ever busy.
	for _, st := range []int{0, 4, 3} {
		s := syntheticIdle()
		s.WS9999.State = present(st)
		if d := DeriveActivityState(s, nil); d.State != StateIdle || d.StartWindow {
			t.Errorf("state %d: %s window %v, want idle", st, d.State, d.StartWindow)
		}
	}
	// State 1 while printing is just printing; the row is standby-only.
	s := syntheticIdle()
	s.WS9999.State = present(1)
	s.PrintStats.State = "printing"
	s.PrintStats.PrintDuration = 5
	s.VirtualSDCard.IsActive = boolPtr(true)
	if d := DeriveActivityState(s, nil); d.State != StatePrinting {
		t.Errorf("printing with state 1: %s", d.State)
	}
	// A stale complete or cancelled with 9999 state 1, 7 or 9 is NOT the window:
	// what 9999 reads after a natural completion or a Moonraker cancel was never
	// captured, and a lingering value must not make an idle printer "preparing".
	for _, prev := range []string{"complete", "cancelled"} {
		for _, st := range []int{1, 7, 9} {
			s = syntheticIdle()
			s.PrintStats.State = prev
			s.WS9999.State = present(st)
			d := DeriveActivityState(s, nil)
			if d.State == StatePreparing || d.StartWindow || d.Bucket != BucketI {
				t.Errorf("%s with 9999 state %d: %s/%s window %v, want bucket I", prev, st, d.State, d.Bucket, d.StartWindow)
			}
		}
	}
}

// 9999 state 8 while print_stats is paused is the RESUME routine.
func TestResumingFromNineNineNineNineState(t *testing.T) {
	s := syntheticIdle()
	s.PrintStats.State = "paused"
	s.PauseResume.IsPaused = boolPtr(true)
	s.WS9999.State = present(8)
	d := DeriveActivityState(s, nil)
	if d.State != StateResuming || d.Bucket != BucketT || d.Class != ClassTransitioning {
		t.Fatalf("%s/%s/%s, want resuming/T/transitioning", d.State, d.Bucket, d.Class)
	}
	if !hasReason(d.Reasons, "9999 state is 8") {
		t.Fatalf("reasons = %v", d.Reasons)
	}
	// State 5 (paused) stays paused; state 8 without a paused job is not resuming.
	s.WS9999.State = present(5)
	if d := DeriveActivityState(s, nil); d.State != StatePaused {
		t.Fatalf("state 5: %s", d.State)
	}
	s = syntheticIdle()
	s.WS9999.State = present(8)
	if d := DeriveActivityState(s, nil); d.State == StateResuming {
		t.Fatal("state 8 with print_stats standby must not derive resuming")
	}
}

func entries(msgs ...string) []moonraker.GCodeStoreEntry {
	out := make([]moonraker.GCodeStoreEntry, len(msgs))
	for i, m := range msgs {
		out[i] = moonraker.GCodeStoreEntry{Message: m, Type: "response", Time: float64(i)}
	}
	return out
}

func messages(es []moonraker.GCodeStoreEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Message
	}
	return out
}

func TestCollapseTemperatureReports(t *testing.T) {
	in := entries("G28", "B:71.0 /70.0 T0:208.3 /250.0", "B:71.0 /70.0 T0:211.1 /250.0", "B:70.9 /70.0 T0:213.9 /250.0", "// flush_temp: 250",
		"// cur_temp = 41.2", "// cur_temp = 41.1", "M104 S0", "B:70.0 /70.0 T0:200.0 /0.0")
	got := messages(CollapseTemperatureReports(in, false))
	want := []string{"G28", "(3 temperature reports, latest: B:70.9 /70.0 T0:213.9 /250.0)", "// flush_temp: 250",
		"(2 temperature reports, latest: // cur_temp = 41.1)", "M104 S0", "B:70.0 /70.0 T0:200.0 /0.0"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	// Newest first: the latest of a run is its first entry.
	nf := messages(CollapseTemperatureReports(entries("B:1.0 /0.0 T0:3.0 /0.0", "B:1.0 /0.0 T0:2.0 /0.0", "ok"), true))
	if len(nf) != 2 || nf[0] != "(2 temperature reports, latest: B:1.0 /0.0 T0:3.0 /0.0)" || nf[1] != "ok" {
		t.Fatalf("newest first: %q", nf)
	}
	// The time and type of the latest report are kept, and the input is not modified.
	c := CollapseTemperatureReports(in, false)
	if c[1].Time != 3 || in[1].Message != "B:71.0 /70.0 T0:208.3 /250.0" {
		t.Fatalf("collapsed %+v, input %+v", c[1], in[1])
	}
	if len(CollapseTemperatureReports(nil, false)) != 0 {
		t.Fatal("nil in, nil out")
	}
}

func TestRecentActivityCollapsesTemperatureRuns(t *testing.T) {
	s := syntheticIdle()
	s.GCodeStoreTail = entries("start", "B:71.0 /70.0 T0:208.3 /250.0", "B:71.0 /70.0 T0:211.1 /250.0", "end")
	got := recentActivity(s)
	if len(got) != 3 || got[1] != "(2 temperature reports, latest: B:71.0 /70.0 T0:211.1 /250.0)" {
		t.Fatalf("recent activity = %q", got)
	}
}
