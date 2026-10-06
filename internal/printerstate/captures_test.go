package printerstate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
)

// Replays of the v0.3.1 live test captures (2026-10-06, testdata
// ws9999_screen_*_20261006.jsonl): the initial full push, then every delta
// that changed state, deviceState, withSelfTest, feedState or a heater target.

type replayStep struct {
	t      string
	merged map[string]any
}

// replayFrames returns the merged 9999 view after each frame of a capture.
func replayFrames(t *testing.T, name string) []replayStep {
	t.Helper()
	data := bytes.TrimPrefix(readTestdata(t, name), []byte("\xef\xbb\xbf"))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	merged := map[string]any{}
	var steps []replayStep
	for sc.Scan() {
		var line struct {
			T     string         `json:"t"`
			Frame map[string]any `json:"frame"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("%s line %d: %v", name, len(steps)+1, err)
		}
		for k, v := range line.Frame {
			merged[k] = v
		}
		cp := make(map[string]any, len(merged))
		for k, v := range merged {
			cp[k] = v
		}
		steps = append(steps, replayStep{t: line.T, merged: cp})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return steps
}

// replaySnapshot builds a CFS-connected snapshot from a replayed 9999 view,
// with the heater targets the printer reported.
func replaySnapshot(t *testing.T, step replayStep, prev, idle string) Snapshot {
	s := cfsIdle(t)
	s.PrintStats.State = prev
	s.IdleTimeout.State = strPtr(idle)
	s.WS9999 = crealityws.StatusFromPush(step.merged)
	nozzle, _ := step.merged["targetNozzleTemp"].(float64)
	bed, _ := step.merged["targetBedTemp0"].(float64)
	withTargets(&s, nozzle, bed)
	return s
}

// A print started on the printer screen shows the same signature as one sent
// from here (state 9, then 1 with deviceState 1, targets 140/70, stop on 7,
// rest on 4 with deviceState 0), so the signal alone covers it.
func TestCapturedScreenStartAndStop(t *testing.T) {
	steps := replayFrames(t, "ws9999_screen_start_stop_20261006.jsonl")
	want := []bool{false, false, true, true, true, true, true, true, true, false}
	if len(steps) != len(want) {
		t.Fatalf("%d frames, want %d", len(steps), len(want))
	}
	for _, prev := range []string{"standby", "complete", "cancelled"} {
		for _, idle := range []string{"Ready", "Printing"} {
			for i, step := range steps {
				d := DeriveActivityState(replaySnapshot(t, step, prev, idle), nil)
				if d.StartWindow != want[i] {
					t.Errorf("%s/%s frame %d (%s): window %v (%s/%s), want %v", prev, idle, i+1, step.t, d.StartWindow, d.State, d.Bucket, want[i])
				}
			}
		}
	}
}

// A filament load from the printer screen (feedState 101 to 107 with
// deviceState 0, idle_timeout Printing, nozzle 250 to 270) is a filament
// operation, never a print start, while the feeder is busy, also with the stale
// withSelfTest 0 left by a power cycle (field feedback item 6). With a normal
// withSelfTest no frame is a start. Known residual with the stale value: in the
// last second of the load the feeder is back at rest while idle_timeout still
// reads Printing, which reads as the start window until idle_timeout settles
// (cancel stays available; it clears by itself; pinned below). Before the load
// begins idle_timeout is still Ready, so the start of a load is not affected.
// idle_timeout Printing stays start evidence because without it an uncaptured
// start type would derive busy_command, where cancel is refused.
func TestCapturedScreenLoadIsNeverAStart(t *testing.T) {
	steps := replayFrames(t, "ws9999_screen_load_20261006.jsonl")
	fed := 0
	for _, stale := range []bool{false, true} {
		for _, prev := range []string{"standby", "complete", "cancelled"} {
			// idle_timeout as captured: Ready until the load began (feedState 101
			// at 02:14:48, Printing from 02:14:50), Printing to the end of the file.
			started := false
			for i, step := range steps {
				if fs, ok := step.merged["feedState"].(float64); ok && !cfsIdleFeedStates[int(fs)] {
					started = true
				}
				idle := "Ready"
				if started {
					idle = "Printing"
				}
				s := replaySnapshot(t, step, prev, idle)
				if stale {
					s.WS9999.WithSelfTest = present(0)
				}
				d := DeriveActivityState(s, nil)
				fs := s.WS9999.FeedState
				feeding := fs.Present && !cfsIdleFeedStates[fs.Value]
				// The residual, pinned: stale value, load begun, feeder back at rest.
				wantWindow := stale && started && !feeding
				if d.StartWindow != wantWindow {
					t.Errorf("stale %v, %s, frame %d (%s): %s window %v, want %v", stale, prev, i+1, step.t, d.State, d.StartWindow, wantWindow)
				}
				if feeding {
					fed++
				}
				if feeding && d.State != StateFilamentOperation {
					t.Errorf("stale %v, %s, frame %d: feedState %d derives %s, want filament_operation", stale, prev, i+1, fs.Value, d.State)
				}
			}
		}
	}
	if fed == 0 {
		t.Fatal("the capture has no frame with the feeder busy")
	}
}
