package printerstate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

// Field feedback items 1 and 6 (dev_docs/field-feedback.md, plan-v0.3.1.md R1):
// the start window must come only from live evidence of a start.

// stuckWS9999 is the real 9999 full push read on 2026-10-05 after a printer
// power cycle (testdata/ws9999_stuck_after_power_cycle_20261005.json): state 0,
// deviceState 0, withSelfTest 0 at rest, no job, every target 0, CFS connected.
func stuckWS9999(t *testing.T) crealityws.Status {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(readTestdata(t, "ws9999_stuck_after_power_cycle_20261005.json"), &raw); err != nil {
		t.Fatalf("decode the stuck frame: %v", err)
	}
	return crealityws.StatusFromPush(raw)
}

// withTargets sets the Moonraker heater targets the way the printer reported
// them on 9999.
func withTargets(s *Snapshot, nozzle, bed float64) {
	s.Extruder = &moonraker.Extruder{Target: floatPtr(nozzle)}
	s.HeaterBed = &moonraker.HeaterBed{Target: floatPtr(bed)}
}

func TestStuckFrameAfterPowerCycleDerivesIdle(t *testing.T) {
	ws := stuckWS9999(t)
	if !ws.WithSelfTest.Present || ws.WithSelfTest.Value != 0 || ws.DeviceState.Value != 0 || ws.State.Value != 0 {
		t.Fatalf("fixture changed: %+v", ws)
	}
	for _, prev := range []string{"standby", "complete", "cancelled"} {
		s := cfsIdle(t)
		s.PrintStats.State = prev
		s.WS9999 = ws
		withTargets(&s, 0, 0)
		d := DeriveActivityState(s, nil)
		if d.Bucket != BucketI || d.StartWindow {
			t.Errorf("print_stats %s with the stuck frame: %s/%s window %v %v, want bucket I", prev, d.State, d.Bucket, d.StartWindow, d.Reasons)
		}
	}
}

// replayStart merges the captured frames of a real CFS start and its stop
// (testdata/ws9999_cfs_start_stop_20260929.jsonl, supervised session
// 2026-09-29) one at a time and derives after each, with the heater targets the
// printer reported, over each previous print_stats value and with idle_timeout
// Ready (the least evidence) or Printing.
func TestCapturedStartAndStopSequence(t *testing.T) {
	data := readTestdata(t, "ws9999_cfs_start_stop_20260929.jsonl")
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	// want is the start window after each frame, in file order.
	want := []bool{
		false, // full push: state 0, deviceState 0, withSelfTest 100
		false, // state 9 alone (deviceState still 0 for about 0.4 s): no window yet
		true,  // deviceState 1, withSelfTest 0: a self-test with live activity
		true,  // state 1 with deviceState 1
		true,  // nozzle target 140
		true,  // bed target 70
		true,  // state 7 (the stop) with deviceState 1
		true,  // withSelfTest 100: state 7 and deviceState 1 still hold
		true,  // targets back to 0
		false, // state 4, deviceState 0: the stop is over
	}
	for _, run := range []struct{ prev, idle string }{
		{"standby", "Ready"}, {"complete", "Ready"}, {"cancelled", "Ready"}, {"standby", "Printing"}, {"complete", "Printing"},
	} {
		prev := run.prev + "/" + run.idle
		merged := map[string]any{}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		i := 0
		for sc.Scan() {
			var line struct {
				T     string         `json:"t"`
				Frame map[string]any `json:"frame"`
			}
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				t.Fatalf("line %d: %v", i+1, err)
			}
			for k, v := range line.Frame {
				merged[k] = v
			}
			if i >= len(want) {
				t.Fatalf("more frames than expectations")
			}
			s := cfsIdle(t)
			s.PrintStats.State = run.prev
			s.IdleTimeout.State = strPtr(run.idle)
			s.WS9999 = crealityws.StatusFromPush(merged)
			nozzle, _ := merged["targetNozzleTemp"].(float64)
			bed, _ := merged["targetBedTemp0"].(float64)
			withTargets(&s, nozzle, bed)
			d := DeriveActivityState(s, nil)
			if d.StartWindow != want[i] {
				t.Errorf("%s, frame %d (%s): window %v (%s/%s %v), want %v", prev, i+1, line.T, d.StartWindow, d.State, d.Bucket, d.Reasons, want[i])
			}
			if want[i] && (d.State != StatePreparing || d.Bucket != BucketPP) {
				t.Errorf("%s, frame %d: %s/%s, want preparing/PP", prev, i+1, d.State, d.Bucket)
			}
			i++
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		if i != len(want) {
			t.Fatalf("%d frames, want %d", i, len(want))
		}
	}
}

// A filament load from the printer screen heats the nozzle and moves; with a
// stale withSelfTest it must derive filament_operation, never a print start,
// whatever print_stats kept from the last job (R1, R3).
func TestScreenFilamentLoadIsNotAStart(t *testing.T) {
	for _, prev := range []string{"standby", "complete", "cancelled"} {
		for _, ds := range []int{10, 11} {
			s := cfsIdle(t)
			s.PrintStats.State = prev
			s.WS9999 = stuckWS9999(t)
			s.WS9999.DeviceState = present(ds)
			s.IdleTimeout.State = strPtr("Printing")
			withTargets(&s, 250, 0)
			d := DeriveActivityState(s, nil)
			if d.State != StateFilamentOperation || d.StartWindow {
				t.Errorf("%s, deviceState %d: %s window %v %v, want filament_operation", prev, ds, d.State, d.StartWindow, d.Reasons)
			}
		}
	}
}

// The quiet frame with a leftover filament map is idle; the map is named only
// when a start really shows.
func TestLeftoverFilamentMapAloneIsNotAStart(t *testing.T) {
	s := cfsIdle(t)
	s.Box.Map["T1B"] = "T1C"
	if d := DeriveActivityState(s, nil); d.State != StateIdle || d.StartWindow {
		t.Fatalf("leftover map alone: %s window %v", d.State, d.StartWindow)
	}
	s.WS9999.State = present(1)
	s.WS9999.DeviceState = present(1)
	d := DeriveActivityState(s, nil)
	if !d.StartWindow || !hasReason(d.Reasons, "T1B -> T1C") {
		t.Fatalf("start with a map: window %v %v", d.StartWindow, d.Reasons)
	}
}

// The stale bed-mesh flag (true after a screen levelling until reboot) never
// makes a printer calibrating; it is named in the reasons.
func TestStaleBedMeshFlagIsOnlyAReason(t *testing.T) {
	s := syntheticIdle()
	s.VirtualSDCard.BedMeshCalibrateState = boolPtr(true)
	d := DeriveActivityState(s, nil)
	if d.State != StateIdle {
		t.Fatalf("idle with the flag: %s, want idle", d.State)
	}
	if !hasReason(d.Reasons, "not treated as calibration") {
		t.Fatalf("reasons %v do not name the flag", d.Reasons)
	}
	s.PrintStats.State = "printing"
	s.PrintStats.PrintDuration = 10
	s.VirtualSDCard.IsActive = boolPtr(true)
	if d := DeriveActivityState(s, nil); d.State != StatePrinting || hasReason(d.Reasons, "bed_mesh_calibate_state") {
		t.Fatalf("printing with the flag: %s %v", d.State, d.Reasons)
	}
}

// Review M1: heating an idle printer whose withSelfTest is stale (a preheat on
// the screen, or set_nozzle_temperature from here) must not become a start
// window, or turning the heater off would be refused.
func TestHeatingWithAStaleSelfTestIsNotAStart(t *testing.T) {
	for _, prev := range []string{"standby", "complete", "cancelled"} {
		s := cfsIdle(t)
		s.PrintStats.State = prev
		s.WS9999 = stuckWS9999(t)
		withTargets(&s, 200, 60)
		d := DeriveActivityState(s, nil)
		if d.Bucket != BucketI || d.StartWindow {
			t.Errorf("%s, heated with the stuck frame: %s/%s window %v %v, want bucket I", prev, d.State, d.Bucket, d.StartWindow, d.Reasons)
		}
	}
}

// Live capture 2026-10-06 (v0.3.1 live test 4): a filament load from the
// printer screen reports feedState 101 with deviceState 0, homes, sets
// idle_timeout Printing and heats the nozzle to 250. With the stale
// withSelfTest 0 of field item 6 it must not be a print start.
func TestScreenLoadSignatureWithStaleSelfTestIsNotAStart(t *testing.T) {
	for _, prev := range []string{"standby", "complete", "cancelled"} {
		for _, homing := range []bool{true, false} {
			s := cfsIdle(t)
			s.PrintStats.State = prev
			s.WS9999 = stuckWS9999(t)
			s.WS9999.FeedState = present(101)
			s.IdleTimeout.State = strPtr("Printing")
			s.MotorControl.IsHoming = boolPtr(homing)
			withTargets(&s, 250, 0)
			d := DeriveActivityState(s, nil)
			if d.StartWindow || d.State == StatePreparing {
				t.Errorf("%s, homing %v: %s/%s window %v %v, want no start window", prev, homing, d.State, d.Bucket, d.StartWindow, d.Reasons)
			}
			if d.Bucket == BucketI {
				t.Errorf("%s, homing %v: bucket I during a load", prev, homing)
			}
		}
	}
}
