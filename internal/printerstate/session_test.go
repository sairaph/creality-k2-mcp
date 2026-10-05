package printerstate

import (
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

// Supervised session 2026-09-29: virtual_sdcard.bed_mesh_calibate_state stays
// true for the whole print after the self-test levels the bed, and (field
// feedback, 2026-10-05) after a screen levelling until the next reboot. It
// behaves as "a mesh is loaded", so since v0.3.1 it never derives calibrating:
// not for a printing or paused job (pause and cancel would be refused) and not
// for an idle printer (it trapped one in calibrating).
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
	if d := DeriveActivityState(idle, nil); d.State != StateIdle {
		t.Fatalf("idle with the flag: %s, want idle (a loaded mesh, not a levelling)", d.State)
	}
	// Real levelling motion is still busy: idle_timeout Printing is busy_command.
	idle.IdleTimeout.State = strPtr("Printing")
	if d := DeriveActivityState(idle, nil); d.Bucket != BucketB {
		t.Fatalf("flag with motion: %s/%s, want bucket B", d.State, d.Bucket)
	}
}

func TestStartWindowSignalsFromNineNineNineNineState(t *testing.T) {
	// State 9, 1 or 7 with deviceState 1 (or absent) is a start or its stop, over
	// standby or the previous job's complete or cancelled (v0.3.1 R1).
	for _, prev := range []string{"standby", "complete", "cancelled"} {
		for _, st := range []int{9, 1, 7} {
			for _, ds := range []crealityws.Int{present(1), {}} {
				s := syntheticIdle()
				s.PrintStats.State = prev
				s.WS9999.State = present(st)
				s.WS9999.DeviceState = ds
				d := DeriveActivityState(s, nil)
				if d.State != StatePreparing || d.Bucket != BucketPP || !d.StartWindow {
					t.Errorf("%s, state %d, deviceState %+v: %s/%s window %v, want the start window", prev, st, ds, d.State, d.Bucket, d.StartWindow)
				}
			}
			// deviceState 0 vetoes: a lingering state value at rest is not a start.
			s := syntheticIdle()
			s.PrintStats.State = prev
			s.WS9999.State = present(st)
			if d := DeriveActivityState(s, nil); d.StartWindow || d.Bucket != BucketI {
				t.Errorf("%s, state %d, deviceState 0: %s/%s window %v, want bucket I", prev, st, d.State, d.Bucket, d.StartWindow)
			}
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
	// State 1 while printing is just printing.
	s := syntheticIdle()
	s.WS9999.State = present(1)
	s.WS9999.DeviceState = present(1)
	s.PrintStats.State = "printing"
	s.PrintStats.PrintDuration = 5
	s.VirtualSDCard.IsActive = boolPtr(true)
	if d := DeriveActivityState(s, nil); d.State != StatePrinting {
		t.Errorf("printing with state 1: %s", d.State)
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

// Live bug (supervised print 2026-09-29): the self-test after a CFS start homes
// and probes the bed while print_stats still shows the previous job's complete,
// and the homing or calibrating row used to win, taking cancel away. The start
// window now takes precedence over the self-test's own motion, naming it.
func TestStartWindowWinsOverTheSelfTestsOwnMotion(t *testing.T) {
	selfTest := func(prev string) Snapshot {
		s := syntheticIdle()
		s.PrintStats.State = prev
		s.WS9999.WithSelfTest = present(40)
		s.WS9999.DeviceState = present(1)
		return s
	}
	for _, prev := range []string{"complete", "standby", "cancelled"} {
		homing := selfTest(prev)
		homing.MotorControl.IsHoming = boolPtr(true)
		d := DeriveActivityState(homing, nil)
		if d.State != StatePreparing || d.Bucket != BucketPP || !d.StartWindow {
			t.Fatalf("%s + self-test + homing: %s/%s window %v, want the start window", prev, d.State, d.Bucket, d.StartWindow)
		}
		if !containsSub(d.Reasons, "self-test homing") || !containsSub(d.Reasons, "motor_control.is_homing is true") {
			t.Errorf("the reason does not name the motion: %v", d.Reasons)
		}
		cal := selfTest(prev)
		cal.CustomMacro.LevelingCalibration = intPtr(1)
		d = DeriveActivityState(cal, nil)
		if d.State != StatePreparing || d.Bucket != BucketPP || !d.StartWindow || !containsSub(d.Reasons, "self-test calibrating") {
			t.Fatalf("%s + self-test + calibrating: %s/%s window %v %v", prev, d.State, d.Bucket, d.StartWindow, d.Reasons)
		}
	}
	// The signal can also be the 9999 state (standby).
	s := syntheticIdle()
	s.WS9999.State = present(1)
	s.WS9999.DeviceState = present(1)
	s.MotorControl.IsHoming = boolPtr(true)
	if d := DeriveActivityState(s, nil); d.State != StatePreparing || !d.StartWindow {
		t.Fatalf("standby + 9999 state 1 + homing: %s", d.State)
	}
}

// Nothing else changes: a genuine homing or calibration outside a start window,
// a print, and a RESUME that homes all derive as before.
func TestHomingAndCalibratingOutsideAStartWindowAreUnchanged(t *testing.T) {
	homing := syntheticIdle()
	homing.MotorControl.IsHoming = boolPtr(true)
	if d := DeriveActivityState(homing, nil); d.State != StateHoming || d.Bucket != BucketB || d.StartWindow {
		t.Errorf("idle + homing: %s/%s window %v, want homing/B", d.State, d.Bucket, d.StartWindow)
	}
	cal := syntheticIdle()
	cal.CustomMacro.LevelingCalibration = intPtr(1)
	if d := DeriveActivityState(cal, nil); d.State != StateCalibrating || d.StartWindow {
		t.Errorf("idle + calibrating: %s window %v", d.State, d.StartWindow)
	}
	// A complete job with the self-test progress at 100 is not a window.
	done := syntheticIdle()
	done.PrintStats.State = "complete"
	done.WS9999.WithSelfTest = present(100)
	done.MotorControl.IsHoming = boolPtr(true)
	if d := DeriveActivityState(done, nil); d.State != StateHoming || d.StartWindow {
		t.Errorf("complete + finished self-test + homing: %s window %v", d.State, d.StartWindow)
	}
	// Printing and paused are never shadowed, even with the signals present.
	printing := syntheticIdle()
	printing.PrintStats.State = "printing"
	printing.PrintStats.PrintDuration = 10
	printing.VirtualSDCard.IsActive = boolPtr(true)
	printing.WS9999.WithSelfTest = present(40)
	printing.MotorControl.IsHoming = boolPtr(true)
	if d := DeriveActivityState(printing, nil); d.State != StateHoming || d.StartWindow {
		t.Errorf("printing + homing: %s window %v (unchanged: homing)", d.State, d.StartWindow)
	}
	paused := syntheticIdle()
	paused.PrintStats.State = "paused"
	paused.PauseResume.IsPaused = boolPtr(true)
	paused.WS9999.WithSelfTest = present(40)
	paused.MotorControl.IsHoming = boolPtr(true)
	if d := DeriveActivityState(paused, nil); d.StartWindow {
		t.Errorf("paused + homing became a start window: %s", d.State)
	}
}

func containsSub(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
