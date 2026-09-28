package policy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

func TestExecute_StartPrint_ReachesPrinting(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	p := New()
	printer := testPrinter(f, "start-print")

	res := mustExecute(t, p, f, printer, ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if !res.Accepted || res.Effect != "confirmed" {
		t.Fatalf("start_print result = %+v, want accepted+confirmed", res)
	}
	if res.After.ActivityState != "printing" {
		t.Errorf("after state = %q, want printing", res.After.ActivityState)
	}
	if start, _, _, _ := f.counts(); start != 1 {
		t.Errorf("PrintStart called %d times, want 1", start)
	}
}

func TestExecute_StartPrint_RefusesUnlistedFile(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	p := New()
	printer := testPrinter(f, "start-print-missing")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionStartPrint, Params{Filename: "ghost.gcode"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeNotFound {
		t.Fatalf("err = %#v, want CodeNotFound", err)
	}
	if start, _, _, _ := f.counts(); start != 0 {
		t.Errorf("PrintStart called %d times, want 0", start)
	}
}

func TestExecute_PauseThenResume_RoundTrip(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setHomed(true)
	f.setHot(220)
	p := New()
	printer := testPrinter(f, "pause-resume")

	pauseRes := mustExecute(t, p, f, printer, ActionPausePrint, Params{}, "")
	if pauseRes.After.ActivityState != "paused" {
		t.Fatalf("after pause, state = %q, want paused", pauseRes.After.ActivityState)
	}

	// resume_print needs a proposal_token (D3).
	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, "")
	if err != nil {
		t.Fatalf("resume proposal: %v", err)
	}
	if !proposal.Proposed || proposal.Token == "" {
		t.Fatalf("resume proposal = %+v, want Proposed with a token", proposal)
	}
	if _, _, resumeCalls, _ := f.counts(); resumeCalls != 0 {
		t.Fatalf("PrintResume called during proposal phase, want 0 calls")
	}

	confirmed := mustExecute(t, p, f, printer, ActionResumePrint, Params{}, proposal.Token)
	if confirmed.After.ActivityState != "printing" {
		t.Fatalf("after resume, state = %q, want printing", confirmed.After.ActivityState)
	}
	if !f.hasEvent("resumed") {
		t.Error("expected RESUME_BASE's pause check to pass and the actual resume to run")
	}
}

func TestExecute_PausePrint_BlockedWhilePreparing(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.withLock(func() {
		f.printState = "printing"
		f.sdActive = true
		f.printDuration = 0 // preparing sub-state: bucket PP, not P
	})
	p := New()
	printer := testPrinter(f, "pause-preparing")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionPausePrint, Params{}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable (pause blocked in PP)", err)
	}
}

func TestExecute_CancelPrint_AllowedWhilePreparing(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.withLock(func() {
		f.printState = "printing"
		f.sdActive = true
		f.printDuration = 0 // preparing: bucket PP
		f.filename = "model.gcode"
	})
	p := New()
	printer := testPrinter(f, "cancel-preparing")

	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal while preparing: %v", err)
	}
	res := mustExecute(t, p, f, printer, ActionCancelPrint, Params{}, proposal.Token)
	if res.After.ActivityState != "cancelled" {
		t.Fatalf("after cancel, state = %q, want cancelled", res.After.ActivityState)
	}
}

func TestExecute_SetNozzleTemperature_MidPrintBand(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.withLock(func() { f.nozzleTemp, f.nozzleTarget = 210, 210 })
	p := New()
	printer := testPrinter(f, "nozzle-band")

	// Inside the default +-10 C band: applies directly, no token.
	res := mustExecute(t, p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 215}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("nozzle temp result = %+v, want confirmed", res)
	}
	if res.After.NozzleTargetC == nil || *res.After.NozzleTargetC != 215 {
		t.Fatalf("after nozzle target = %v, want 215", res.After.NozzleTargetC)
	}

	// Outside the band: refused, never sent.
	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetNozzleTemperature, Params{TargetC: 240}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeInvalidInput {
		t.Fatalf("err = %#v, want CodeInvalidInput (outside band)", err)
	}
}

func TestExecute_SetNozzleTemperature_IdleArmsWatchdog(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter() // idle baseline
	wd := newFakeWatchdog()
	f.setWatchdog(wd)
	p := New()
	printer := testPrinter(f, "nozzle-idle")

	res := mustExecute(t, p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 200}, "")
	if res.IdleHeatArm == nil {
		t.Fatal("expected an IdleHeatArmRequest for a heater set while idle (D2)")
	}
	if res.IdleHeatArm.Heater != "extruder" || res.IdleHeatArm.TargetC != 200 {
		t.Errorf("IdleHeatArm = %+v, want extruder/200", res.IdleHeatArm)
	}
	if wd.armCount() != 1 {
		t.Fatalf("watchdog armed %d times, want 1", wd.armCount())
	}
}

func TestExecute_SetNozzleTemperature_BlockedWhilePaused(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setPaused()
	p := New()
	printer := testPrinter(f, "nozzle-paused")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetNozzleTemperature, Params{TargetC: 200}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable (temperatures blocked while paused)", err)
	}
}

func TestExecute_SetNozzleTemperature_LiveCapUnknownRefuses(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setOmitProductParam(true) // product_param never reported (10-hazard-analysis.md 5.2)
	p := New()
	printer := testPrinter(f, "nozzle-nocap")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetNozzleTemperature, Params{TargetC: 210}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable (live cap unknown)", err)
	}
}

func TestExecute_SetFanSpeed_PartFanFloorMidPrint(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setPartFan(80)
	p := New()
	printer := testPrinter(f, "fan-floor")

	// 80% -> floor is 50% of 80 = 40%. 35% is below the floor.
	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 35}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeInvalidInput {
		t.Fatalf("err = %#v, want CodeInvalidInput (below part fan floor)", err)
	}

	res := mustExecute(t, p, f, printer, ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 50}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("fan result = %+v, want confirmed", res)
	}
}

func TestExecute_SetSpeedAndFlowFactor_Bands(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	p := New()
	printer := testPrinter(f, "speed-flow")

	if _, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetSpeedFactor, Params{Percent: 300}, ""); err == nil {
		t.Fatal("expected speed factor 300 to be refused (outside 50-150 band)")
	}
	res := mustExecute(t, p, f, printer, ActionSetSpeedFactor, Params{Percent: 120}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("speed factor result = %+v, want confirmed", res)
	}

	if _, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetFlowFactor, Params{Percent: 50}, ""); err == nil {
		t.Fatal("expected flow factor 50 to be refused (outside 90-110 band)")
	}
	res2 := mustExecute(t, p, f, printer, ActionSetFlowFactor, Params{Percent: 105}, "")
	if res2.Effect != "confirmed" {
		t.Fatalf("flow factor result = %+v, want confirmed", res2)
	}
}

func TestExecute_SetLight_AllowedWhenOffline(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setKlippyUnreachable()
	p := New()
	printer := testPrinter(f, "light-klippy-down")

	// klippy_not_ready is not the same as offline (server/info itself
	// unreachable); set_light must still work here.
	res := mustExecute(t, p, f, printer, ActionSetLight, Params{On: true}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("set_light result = %+v, want confirmed even with klippy unreachable", res)
	}
}

func TestExecute_ExcludeObject_LastObjectRefused(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.addObject("only_object")
	p := New()
	printer := testPrinter(f, "exclude-last")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionExcludeObject, Params{ObjectName: "only_object"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeInvalidInput {
		t.Fatalf("err = %#v, want CodeInvalidInput (last object)", err)
	}
}

func TestExecute_ExcludeObject_CaseInsensitiveRoundTrip(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.addObject("part_a")
	f.addObject("part_b")
	p := New()
	printer := testPrinter(f, "exclude-ok")

	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionExcludeObject, Params{ObjectName: "Part_A"}, "")
	if err != nil {
		t.Fatalf("exclude proposal: %v", err)
	}
	res := mustExecute(t, p, f, printer, ActionExcludeObject, Params{ObjectName: "Part_A"}, proposal.Token)
	if res.Effect != "confirmed" {
		t.Fatalf("exclude result = %+v, want confirmed", res)
	}
}

func TestExecute_UploadThenDelete(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	p := New()
	printer := testPrinter(f, "upload-delete")

	res := mustExecute(t, p, f, printer, ActionUploadGCodeFile, Params{Filename: "new.gcode", LocalPath: "/tmp/new.gcode"}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("upload result = %+v, want confirmed", res)
	}

	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionDeleteGCodeFile, Params{Filename: "new.gcode"}, "")
	if err != nil {
		t.Fatalf("delete proposal: %v", err)
	}
	delRes := mustExecute(t, p, f, printer, ActionDeleteGCodeFile, Params{Filename: "new.gcode"}, proposal.Token)
	if delRes.Effect != "confirmed" {
		t.Fatalf("delete result = %+v, want confirmed", delRes)
	}
}

func TestExecute_Upload_OverwriteNeedsToken(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("existing.gcode")
	p := New()
	printer := testPrinter(f, "upload-overwrite")

	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionUploadGCodeFile, Params{Filename: "existing.gcode", LocalPath: "/tmp/x"}, "")
	if err != nil {
		t.Fatalf("upload overwrite: %v", err)
	}
	if !proposal.Proposed {
		t.Fatalf("upload over an existing file must require a proposal_token, got %+v", proposal)
	}
	res := mustExecute(t, p, f, printer, ActionUploadGCodeFile, Params{Filename: "existing.gcode", LocalPath: "/tmp/x"}, proposal.Token)
	if res.Effect != "confirmed" {
		t.Fatalf("overwrite result = %+v, want confirmed", res)
	}
}

func TestExecute_Upload_RefusesOverCurrentPrintFile(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("active.gcode")
	p := New()
	printer := testPrinter(f, "upload-active")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionUploadGCodeFile, Params{Filename: "active.gcode", LocalPath: "/tmp/x"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict", err)
	}
}

func TestExecute_Delete_RefusesCurrentPrintFile(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("active.gcode")
	p := New()
	printer := testPrinter(f, "delete-active")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionDeleteGCodeFile, Params{Filename: "active.gcode"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict", err)
	}
}

func TestExecute_ForbidsWhenAllowControlFalse(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	printer := testPrinter(f, "no-control")
	printer.AllowControl = false
	p := New()

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetLight, Params{On: true}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeForbidden {
		t.Fatalf("err = %#v, want CodeForbidden", err)
	}
}

func TestExecute_CFSConnectedBlocksResumeButNotCancel(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setPaused()
	f.setCFSConnected(true)
	p := New()
	printer := testPrinter(f, "cfs-block")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionResumePrint, Params{}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("resume err = %#v, want CodeUnavailable (CFS connected)", err)
	}

	// D5: cancel stays available while CFS is connected.
	proposal, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal while CFS connected: %v", err)
	}
	res := mustExecute(t, p, f, printer, ActionCancelPrint, Params{}, proposal.Token)
	if res.After.ActivityState != "cancelled" {
		t.Fatalf("after cancel with CFS connected, state = %q, want cancelled", res.After.ActivityState)
	}
}

// envOverridePrinter builds a domain.Printer shaped exactly like
// domain.EnvOverride's result (dev_docs/safety-architecture.md 3.4): no
// persisted Hostname, since it is rebuilt fresh from K2_MCP_HOST on every
// call with nothing yet learned from the printer.
func envOverridePrinter(host string) domain.Printer {
	return domain.Printer{
		ID: domain.EnvPrinterID, Name: domain.EnvPrinterID,
		Host: host, MoonrakerPort: 7125, Enabled: true, AllowControl: true,
	}
}

// TestExecute_EnvOverride_ResolvesHostnameBeforeLocking confirms Execute
// resolves the env-override printer's Klipper hostname from a fresh
// printer/info read and locks on it, not on Host (dev_docs/safety-
// architecture.md 3.4, review backlog item 3).
func TestExecute_EnvOverride_ResolvesHostnameBeforeLocking(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinterInfo("K2-resolved", nil)
	p := New()
	printer := envOverridePrinter("10.0.0.5")

	res := mustExecute(t, p, f, printer, ActionSetLight, Params{On: true}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("set_light result = %+v, want confirmed", res)
	}

	hostnamePath, err := domain.LockPath("K2-resolved")
	if err != nil {
		t.Fatalf("LockPath(hostname): %v", err)
	}
	if _, statErr := os.Stat(hostnamePath); statErr != nil {
		t.Errorf("expected a lock file at the resolved hostname's path %s, stat error: %v", hostnamePath, statErr)
	}

	hostPath, err := domain.LockPath("10.0.0.5")
	if err != nil {
		t.Fatalf("LockPath(host): %v", err)
	}
	if _, statErr := os.Stat(hostPath); statErr == nil {
		t.Errorf("expected no lock file at the host's path %s; Execute must never lock on the host", hostPath)
	}
}

// TestExecute_EnvOverride_RefusesWriteWhenHostnameUnresolvable confirms
// Execute never falls back to locking on Host when the fresh printer/info
// read fails: the write is refused as unavailable instead, and no lock (on
// either identity) is ever taken.
func TestExecute_EnvOverride_RefusesWriteWhenHostnameUnresolvable(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinterInfo("", errors.New("connection refused"))
	p := New()
	printer := envOverridePrinter("10.0.0.6")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetLight, Params{On: true}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
	if perr.Message == "" {
		t.Error("want a non-empty hint explaining the hostname could not be resolved")
	}

	hostPath, lpErr := domain.LockPath("10.0.0.6")
	if lpErr != nil {
		t.Fatalf("LockPath(host): %v", lpErr)
	}
	if _, statErr := os.Stat(hostPath); statErr == nil {
		t.Errorf("expected no lock file at the host's path %s; a write must never lock before the hostname is known", hostPath)
	}
}

// TestExecute_EnvOverride_RefusesWriteWhenHostnameEmpty is the same as
// TestExecute_EnvOverride_RefusesWriteWhenHostnameUnresolvable but for a
// printer/info read that succeeds with no error yet reports an empty
// hostname, which must be treated the same as an unreachable read.
func TestExecute_EnvOverride_RefusesWriteWhenHostnameEmpty(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinterInfo("", nil)
	p := New()
	printer := envOverridePrinter("10.0.0.7")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetLight, Params{On: true}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable", err)
	}
}
