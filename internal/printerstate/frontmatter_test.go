package printerstate

import (
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

func TestBuildStateBlock_RealIdleCapture(t *testing.T) {
	snap := idleSnapshot(t)
	derived := DeriveActivityState(snap, nil)
	block := BuildStateBlock(snap, derived, nil)

	if block.ActivityState != StateIdle {
		t.Fatalf("ActivityState = %q, want %q", block.ActivityState, StateIdle)
	}
	if block.Bucket != string(BucketI) || block.GatingClass != string(ClassSafeToAct) {
		t.Fatalf("Bucket/GatingClass = %s/%s, want I/safe_to_act", block.Bucket, block.GatingClass)
	}
	if block.CFSConnected {
		t.Fatal("CFSConnected = true, want false on the disconnected baseline")
	}
	if block.Model != "F021" {
		t.Fatalf("Model = %q, want F021", block.Model)
	}
	if block.NozzleTemperatureC == nil || *block.NozzleTemperatureC != 29.34 {
		t.Fatalf("NozzleTemperatureC = %v, want 29.34", block.NozzleTemperatureC)
	}
	if block.BedTemperatureC == nil || *block.BedTemperatureC != 25.81 {
		t.Fatalf("BedTemperatureC = %v, want 25.81", block.BedTemperatureC)
	}
	if block.SpeedFactorPercent == nil || *block.SpeedFactorPercent != 100 {
		t.Fatalf("SpeedFactorPercent = %v, want 100", block.SpeedFactorPercent)
	}
	if block.FlowFactorPercent == nil || *block.FlowFactorPercent != 100 {
		t.Fatalf("FlowFactorPercent = %v, want 100", block.FlowFactorPercent)
	}
	if block.PartFanPercent == nil || *block.PartFanPercent != 0 {
		t.Fatalf("PartFanPercent = %v, want 0 (fan0 value is 0)", block.PartFanPercent)
	}
	if block.LightOn == nil || *block.LightOn {
		t.Fatalf("LightOn = %v, want false (LED value is 0)", block.LightOn)
	}
	// Not paused: stored pause targets must not be populated even though
	// PRINTER_PARAM happens to carry stale nonzero values from an earlier
	// pause/resume cycle in this real capture.
	if block.StoredHotendTargetC != nil {
		t.Fatalf("StoredHotendTargetC = %v, want nil while not paused", block.StoredHotendTargetC)
	}
	if block.Pending != nil {
		t.Fatalf("Pending = %+v, want nil", block.Pending)
	}
	// print_stats.filename is empty at idle, but cur_print_data keeps the
	// prior job's identity after it ends (11-state-model.md section 5.2), so
	// Job must still report it rather than going nil.
	if block.Job == nil || block.Job.Filename != "FigureJoints5mmball.stl_PLA_41m51s.gcode" {
		t.Fatalf("Job = %+v, want the last completed job's filename from cur_print_data", block.Job)
	}
}

func TestBuildStateBlock_StoredPauseTargetsOnlyWhilePaused(t *testing.T) {
	// control_test_20260928.log's own status query at the moment of pause
	// (line 38) did not include gcode_macro PRINTER_PARAM, so this uses a
	// synthetic snapshot with an explicit stored target instead of
	// overclaiming fidelity to a field that excerpt never actually
	// captured; PAUSE_EXTERNAL setting hotend_temp to the pre-pause target
	// (here 210) is documented in gcode_macro.cfg and
	// dev_docs/safety-architecture.md 4.2's resume_print disclosure.
	paused := syntheticIdle()
	paused.PrintStats.State = "paused"
	paused.PauseResume.IsPaused = boolPtr(true)
	paused.PrinterParam = &moonraker.PrinterParam{
		HotendTemp: floatPtr(210),
		Fan0Speed:  floatPtr(1.0),
		Fan2Speed:  floatPtr(0.5),
	}
	derived := DeriveActivityState(paused, nil)
	if derived.State != StatePaused {
		t.Fatalf("precondition failed: derived state = %q, want paused", derived.State)
	}
	block := BuildStateBlock(paused, derived, nil)
	if block.StoredHotendTargetC == nil || *block.StoredHotendTargetC != 210 {
		t.Fatalf("StoredHotendTargetC = %v, want 210 (PAUSE_EXTERNAL's stored target)", block.StoredHotendTargetC)
	}
	if block.StoredFan0Percent == nil || *block.StoredFan0Percent != 100 {
		t.Fatalf("StoredFan0Percent = %v, want 100", block.StoredFan0Percent)
	}
	if block.StoredFan2Percent == nil || *block.StoredFan2Percent != 50 {
		t.Fatalf("StoredFan2Percent = %v, want 50", block.StoredFan2Percent)
	}

	// The same stored PRINTER_PARAM values must not surface while not
	// actually paused (safety-architecture.md 3.1: stored targets are only
	// meaningful while paused).
	printing := syntheticIdle()
	printing.PrintStats.State = "printing"
	printing.PrintStats.PrintDuration = 10
	printing.PauseResume.IsPaused = boolPtr(false)
	printing.VirtualSDCard.IsActive = boolPtr(true)
	printing.PrinterParam = paused.PrinterParam
	printingDerived := DeriveActivityState(printing, nil)
	printingBlock := BuildStateBlock(printing, printingDerived, nil)
	if printingBlock.StoredHotendTargetC != nil {
		t.Fatalf("StoredHotendTargetC = %v, want nil while printing (not paused)", printingBlock.StoredHotendTargetC)
	}
}

func TestBuildStateBlock_PendingActionRendersElapsedAndTimeout(t *testing.T) {
	snap := syntheticIdle()
	pending := &PendingAction{
		Kind:     PendingPause,
		IssuedAt: fixedNow.Add(-5 * time.Second),
		Timeout:  30 * time.Second,
	}
	derived := DeriveActivityState(snap, pending)
	block := BuildStateBlock(snap, derived, pending)

	if block.Pending == nil {
		t.Fatal("Pending = nil, want a rendered pending action")
	}
	if block.Pending.Kind != string(PendingPause) {
		t.Fatalf("Pending.Kind = %q, want %q", block.Pending.Kind, PendingPause)
	}
	if block.Pending.ElapsedMs != 5000 {
		t.Fatalf("Pending.ElapsedMs = %d, want 5000", block.Pending.ElapsedMs)
	}
	if block.Pending.TimeoutMs != 30000 {
		t.Fatalf("Pending.TimeoutMs = %d, want 30000", block.Pending.TimeoutMs)
	}
}

func TestBuildStateBlock_NoPendingActionRendersNil(t *testing.T) {
	snap := syntheticIdle()
	derived := DeriveActivityState(snap, nil)
	block := BuildStateBlock(snap, derived, nil)
	if block.Pending != nil {
		t.Fatalf("Pending = %+v, want nil", block.Pending)
	}
}

func TestBuildStateBlock_PrinterIdentity(t *testing.T) {
	snap := syntheticIdle()
	snap.Printer = domain.Printer{ID: "k2-5885", Name: "Living Room K2", Host: "192.168.1.102", Hostname: "K2-5885"}
	derived := DeriveActivityState(snap, nil)
	block := BuildStateBlock(snap, derived, nil)
	if block.PrinterID != "k2-5885" || block.PrinterName != "Living Room K2" || block.PrinterHost != "192.168.1.102" || block.Hostname != "K2-5885" {
		t.Fatalf("printer identity fields = %+v, want the values from snap.Printer", block)
	}
}

func TestBuildStateBlock_MissingFanAndLightYieldNilNotZero(t *testing.T) {
	snap := syntheticIdle()
	// No Fan0/Fan1/Fan2/LED decoded at all (object absent from the query).
	derived := DeriveActivityState(snap, nil)
	block := BuildStateBlock(snap, derived, nil)
	if block.PartFanPercent != nil || block.CaseFanPercent != nil || block.AuxiliaryFanPercent != nil {
		t.Fatalf("fan percents = %+v, want all nil when the output_pin objects were never decoded", block)
	}
	if block.LightOn != nil {
		t.Fatalf("LightOn = %v, want nil when the LED output_pin was never decoded", block.LightOn)
	}
}

func TestBuildStateBlock_RecentActivityFromGCodeStoreTail(t *testing.T) {
	snap := syntheticIdle()
	snap.GCodeStoreTail = []moonraker.GCodeStoreEntry{
		{Message: "// cur_temp = 85.86", Type: "response"},
		{Message: "// start END_PRINT", Type: "response"},
	}
	derived := DeriveActivityState(snap, nil)
	block := BuildStateBlock(snap, derived, nil)
	if len(block.RecentActivity) != 2 || block.RecentActivity[0] != "// cur_temp = 85.86" {
		t.Fatalf("RecentActivity = %v, want the two gcode_store messages in order", block.RecentActivity)
	}
}
