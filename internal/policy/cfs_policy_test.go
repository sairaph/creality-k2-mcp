package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// The D5 tests of v0.1.0 (blanket "CFS connected blocks control") are converted
// to the section 3.1 table of dev_docs/plan-v0.2.0.md: one test per cfsRule
// row, plus the pause/cancel-never-blocked matrix.

// cfsSetup builds a fake printer with a healthy, connected, idle CFS (every
// 9999 and Moonraker CFS field present and quiet).
func cfsSetup(t *testing.T, host string) (*fakePrinter, *Policy, domain.Printer) {
	t.Helper()
	setTestHome(t)
	f := newFakePrinter()
	f.setCFSConnected(true)
	return f, New(), testPrinter(f, host)
}

// fastTimers shrinks the read-back and map-verify waits so a failure path
// does not wait real seconds.
func fastTimers(t *testing.T) {
	t.Helper()
	oldR, oldRI, oldM, oldMI := moonrakerReadbackTimeout, moonrakerReadbackInterval, mapVerifyTimeout, mapVerifyInterval
	moonrakerReadbackTimeout, moonrakerReadbackInterval = 60*time.Millisecond, 10*time.Millisecond
	mapVerifyTimeout, mapVerifyInterval = 60*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		moonrakerReadbackTimeout, moonrakerReadbackInterval, mapVerifyTimeout, mapVerifyInterval = oldR, oldRI, oldM, oldMI
	})
}

func exec(p *Policy, f *fakePrinter, printer domain.Printer, name ActionName, params Params, token string) (Result, error) {
	return p.Execute(context.Background(), f.deps(), printer, testSettings(), name, params, token)
}

func wantErr(t *testing.T, err error, code Code, contains string) *Error {
	t.Helper()
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %#v, want a *policy.Error with code %s", err, code)
	}
	if perr.Code != code {
		t.Fatalf("code = %s (%s), want %s", perr.Code, perr.Message, code)
	}
	if contains != "" && !strings.Contains(perr.Message, contains) {
		t.Fatalf("message = %q, want it to contain %q", perr.Message, contains)
	}
	return perr
}

func gatesOf(t *testing.T, f *fakePrinter) ([]printerstate.ActionGate, printerstate.Derived) {
	t.Helper()
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), testPrinterValue(f))
	derived := printerstate.DeriveActivityState(snap, nil)
	return AvailableFor(derived, testSettings()), derived
}

// --- one test per cfsRule row ---

// cfsNone: pause_print, cancel_print, set_light, set_bed_temperature,
// upload_gcode_file, delete_gcode_file: the bucket gate decides, whatever the
// CFS reports.
func TestCFSRule_None(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-none")
	f.setPrinting("model.gcode")
	f.setCFSErr(9)
	f.setFeedState(2)
	f.setWS9999Unreachable(true)

	gates, derived := gatesOf(t, f)
	if derived.State != printerstate.StatePrinting || derived.CFSKnown || !derived.CFSConnected {
		t.Fatalf("setup: state %s known %v connected %v", derived.State, derived.CFSKnown, derived.CFSConnected)
	}
	for name, want := range map[ActionName]string{
		ActionPausePrint: "available", ActionCancelPrint: "needs_confirmation", ActionSetLight: "available",
		ActionSetBedTemperature: "available", ActionUploadGCodeFile: "available", ActionDeleteGCodeFile: "needs_confirmation",
	} {
		if g := gateStatus(t, gates, name); g.Status != want {
			t.Errorf("%s with a broken CFS mid-print: %q (%s), want %s", name, g.Status, g.Reason, want)
		}
	}

	printer = testPrinter(f, "rule-none") // gatesOf re-pointed the fake's live hostname
	// Through Execute: set_bed_temperature mid-print inside its band, and an
	// upload while idle with a CFS error.
	if _, err := exec(p, f, printer, ActionSetBedTemperature, Params{TargetC: 0}, ""); err != nil {
		t.Fatalf("set_bed_temperature with a broken CFS: %v", err)
	}
	f2, p2, printer2 := cfsSetup(t, "rule-none-idle")
	f2.setCFSErr(9)
	if _, err := exec(p2, f2, printer2, ActionUploadGCodeFile, Params{Filename: "new.gcode", LocalPath: "x"}, ""); err != nil {
		t.Fatalf("upload with a CFS error while idle: %v", err)
	}
	f2.addFile("old.gcode")
	prop, err := exec(p2, f2, printer2, ActionDeleteGCodeFile, Params{Filename: "old.gcode"}, "")
	if err != nil {
		t.Fatalf("delete proposal: %v", err)
	}
	if _, err := exec(p2, f2, printer2, ActionDeleteGCodeFile, Params{Filename: "old.gcode"}, prop.Token); err != nil {
		t.Fatalf("delete with a CFS error while idle: %v", err)
	}
}

// cfsKnownNoError: exclude_object and set_speed_factor need Known and no error.
func TestCFSRule_KnownNoError(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-known")
	f.setPrinting("model.gcode")
	f.addObject("a")
	f.addObject("b")

	if _, err := exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 100}, ""); err != nil {
		t.Fatalf("set_speed_factor with a healthy CFS: %v", err)
	}
	prop, err := exec(p, f, printer, ActionExcludeObject, Params{ObjectName: "a"}, "")
	if err != nil {
		t.Fatalf("exclude_object proposal with a healthy CFS: %v", err)
	}
	if _, err := exec(p, f, printer, ActionExcludeObject, Params{ObjectName: "a"}, prop.Token); err != nil {
		t.Fatalf("exclude_object with a healthy CFS: %v", err)
	}

	f.setCFSErr(4)
	_, err = exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 100}, "")
	wantErr(t, err, CodeUnavailable, "cfsKnownNoError")
	_, err = exec(p, f, printer, ActionExcludeObject, Params{ObjectName: "b"}, "")
	wantErr(t, err, CodeUnavailable, "cfsKnownNoError")

	f.setCFSErr(0)
	f.setWS9999Unreachable(true)
	_, err = exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 100}, "")
	wantErr(t, err, CodeUnavailable, "unreachable")
}

// cfsFan: idle needs a quiescent CFS, a print needs Known and no error.
func TestCFSRule_Fan(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-fan")
	if _, err := exec(p, f, printer, ActionSetFanSpeed, Params{Fan: domain.FanCase, FanPercent: 50}, ""); err != nil {
		t.Fatalf("idle fan with a quiescent CFS: %v", err)
	}
	f.setDeviceState(1) // not a filament operation, but not quiescent either
	_, err := exec(p, f, printer, ActionSetFanSpeed, Params{Fan: domain.FanCase, FanPercent: 60}, "")
	wantErr(t, err, CodeUnavailable, "cfsFan")

	f2, p2, printer2 := cfsSetup(t, "rule-fan-print")
	f2.setPrinting("model.gcode")
	f2.setDeviceState(1) // deviceState is 1 during prints: must not matter
	f2.setFeedState(2)   // a feed during a print is ordinary
	if _, err := exec(p2, f2, printer2, ActionSetFanSpeed, Params{Fan: domain.FanCase, FanPercent: 50}, ""); err != nil {
		t.Fatalf("fan mid-print with a Known, error free CFS: %v", err)
	}
	f2.setCFSErr(3)
	_, err = exec(p2, f2, printer2, ActionSetFanSpeed, Params{Fan: domain.FanCase, FanPercent: 60}, "")
	wantErr(t, err, CodeUnavailable, "cfsFan")
}

// cfsNozzle: idle needs a quiescent CFS; during a print it is refused (V5).
func TestCFSRule_Nozzle(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-nozzle")
	f.setWatchdog(newFakeWatchdog())
	if _, err := exec(p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 200}, ""); err != nil {
		t.Fatalf("idle nozzle with a quiescent CFS: %v", err)
	}
	f.setDeviceState(1)
	_, err := exec(p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 210}, "")
	wantErr(t, err, CodeUnavailable, "cfsNozzle")

	f2, p2, printer2 := cfsSetup(t, "rule-nozzle-print")
	f2.setPrinting("model.gcode")
	f2.setHot(210)
	_, err = exec(p2, f2, printer2, ActionSetNozzleTemperature, Params{TargetC: 212}, "")
	wantErr(t, err, CodeUnavailable, "cannot detect")
}

// cfsRefuse: set_flow_factor is refused whenever a CFS is connected (V5).
func TestCFSRule_Refuse(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-flow")
	f.setPrinting("model.gcode")
	_, err := exec(p, f, printer, ActionSetFlowFactor, Params{Percent: 100}, "")
	wantErr(t, err, CodeUnavailable, "flow")
	f.setCFSConnected(false)
	if _, err := exec(p, f, printer, ActionSetFlowFactor, Params{Percent: 100}, ""); err != nil {
		t.Fatalf("flow without a CFS must still work: %v", err)
	}
}

// cfsStart: start_print needs a quiescent CFS, plus the mapping proposal.
func TestCFSRule_Start(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-start")
	f.addCFSFile("three.gcode", "PLA", "#FF0000")
	f.setDeviceState(1)
	_, err := exec(p, f, printer, ActionStartPrint, Params{Filename: "three.gcode"}, "")
	wantErr(t, err, CodeUnavailable, "cfsStart")
	f.setDeviceState(0)
	prop, err := exec(p, f, printer, ActionStartPrint, Params{Filename: "three.gcode"}, "")
	if err != nil || !prop.Proposed || prop.Token == "" {
		t.Fatalf("a quiescent CFS start must return a proposal: %v %+v", err, prop)
	}
	if start, _, _, _ := f.counts(); start != 0 {
		t.Fatal("PrintStart was sent by a proposal")
	}
}

// cfsResume: resume_print needs Known and no error here; the rest is the pause
// record (resume tests below).
func TestCFSRule_Resume(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-resume")
	f.setPrinting("model.gcode")
	f.setPaused()
	f.setCFSErr(5)
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "cfsResume")
}

// cfsQuiescent: set_filament_definition needs a quiescent CFS.
func TestCFSRule_Quiescent(t *testing.T) {
	f, p, printer := cfsSetup(t, "rule-quiescent")
	f.setDeviceState(1)
	_, err := exec(p, f, printer, ActionSetFilamentDefinition, Params{Slot: "T1A", Material: "99002", Color: "#00ff00"}, "")
	wantErr(t, err, CodeUnavailable, "cfsQuiescent")
	if f.cfs.modifyCalls != 0 {
		t.Fatal("modifyMaterial was sent through a refused gate")
	}
}

// Every rule's message names the rule (so the caller can look it up) and a next
// step; the checkCFS unit is pure over Derived.
func TestCheckCFS_PureOverDerived(t *testing.T) {
	d := printerstate.Derived{CFSConnected: true, Bucket: printerstate.BucketI, CFSKnown: true, CFSQuiescent: false, CFSReasons: []string{"cfs busy: x"}}
	if err := checkCFS(specs[ActionSetFilamentDefinition], d, Params{}); err == nil || !strings.Contains(err.Message, "cfs busy: x") {
		t.Fatalf("err = %v, want the CFS reasons listed", err)
	}
	d.CFSConnected = false
	if err := checkCFS(specs[ActionSetFlowFactor], d, Params{}); err != nil {
		t.Fatalf("no CFS connected: %v", err)
	}
	for name, spec := range specs {
		if spec.CFS.String() == "cfsUnknown" {
			t.Errorf("%s has no CFS rule", name)
		}
	}
}

// --- pause and cancel are never blocked by a CFS signal ---

func TestPauseCancelNeverBlockedByCFSSignals(t *testing.T) {
	states := []struct {
		name  string
		setup func(f *fakePrinter)
		state string
		pause string // expected pause gate: "available" only while printing
	}{
		{"printing", func(f *fakePrinter) { f.setPrinting("model.gcode") }, printerstate.StatePrinting, "available"},
		{"paused", func(f *fakePrinter) { f.setPrinting("model.gcode"); f.setPaused() }, printerstate.StatePaused, "blocked"},
		{"preparing", func(f *fakePrinter) { f.setPreparing("model.gcode") }, printerstate.StatePreparing, "blocked"},
	}
	signals := []struct {
		name  string
		apply func(f *fakePrinter)
	}{
		{"feedState 2", func(f *fakePrinter) { f.setFeedState(2) }},
		{"deviceState 10", func(f *fakePrinter) { f.setDeviceState(10) }},
		{"unknown feedState 77", func(f *fakePrinter) { f.setFeedState(77) }},
		{"err set", func(f *fakePrinter) { f.setCFSErr(12) }},
		{"9999 unreachable", func(f *fakePrinter) { f.setWS9999Unreachable(true) }},
		{"box nil", func(f *fakePrinter) { f.setOmitBox(true) }},
	}
	for _, st := range states {
		for _, sg := range signals {
			t.Run(st.name+" x "+sg.name, func(t *testing.T) {
				f := newFakePrinter()
				f.setCFSConnected(true)
				st.setup(f)
				sg.apply(f)
				gates, derived := gatesOf(t, f)
				if derived.State != st.state {
					t.Fatalf("state = %s (%v), want %s: a CFS signal must never shadow it", derived.State, derived.Reasons, st.state)
				}
				if !derived.CFSConnected {
					t.Fatal("CFSConnected = false")
				}
				if g := gateStatus(t, gates, ActionCancelPrint); g.Status != "needs_confirmation" {
					t.Errorf("cancel_print: %q (%s), want needs_confirmation", g.Status, g.Reason)
				}
				g := gateStatus(t, gates, ActionPausePrint)
				if g.Status != st.pause {
					t.Errorf("pause_print: %q (%s), want %s", g.Status, g.Reason, st.pause)
				}
				if g.Status == "blocked" && strings.Contains(g.Reason, "rule cfs") {
					t.Errorf("pause_print blocked by a CFS rule: %s", g.Reason)
				}
			})
		}
	}
}

// The same matrix through Execute for the two stopping actions in a print.
func TestPauseCancelExecuteWithBrokenCFS(t *testing.T) {
	f, p, printer := cfsSetup(t, "stop-broken")
	f.setPrinting("model.gcode")
	f.setCFSErr(12)
	f.setFeedState(77)
	f.setWS9999Unreachable(true)
	if res, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err != nil || res.After.ActivityState != "paused" {
		t.Fatalf("pause: %v %+v", err, res.After)
	}
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal: %v", err)
	}
	if res, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token); err != nil || res.After.ActivityState != "cancelled" {
		t.Fatalf("cancel: %v %+v", err, res.After)
	}
}

// --- the start window (plan 8a.1) ---

// startedFixture runs a full CFS start through Execute and returns the state
// right after it: the fake's self-test is running with print_stats standby.
func startedFixture(t *testing.T, host string) (*fakePrinter, *Policy, domain.Printer, Result) {
	t.Helper()
	fastTimers(t)
	f, p, printer := cfsSetup(t, host)
	f.addCFSFile("three.gcode", "PLA;PETG;PETG", "#FF0000;#000000;#FFFFFF")
	prop, err := exec(p, f, printer, ActionStartPrint, Params{Filename: "three.gcode"}, "")
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	res, err := exec(p, f, printer, ActionStartPrint, Params{Filename: "three.gcode"}, prop.Token)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Effect != "sent" {
		t.Fatalf("effect = %s (%v)", res.Effect, res.Effects)
	}
	return f, p, printer, res
}

func TestStartWindow_RecordCoversTheWindowBeforeAnySignal(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "window-record")
	// The signals have not appeared yet: self-test progress 100, identity map.
	f.setSelfTest(100)
	f.cfs9999(func(c *fakeCFS) { c.resetMap(); c.state = 0 })

	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if d := printerstate.DeriveActivityState(snap, nil); d.State != printerstate.StateIdle {
		t.Fatalf("setup: without the record the state is %s, want idle (the hole this closes)", d.State)
	}
	d := p.Derive(snap)
	if d.State != printerstate.StatePreparing || d.Bucket != printerstate.BucketPP {
		t.Fatalf("with the record: %s/%s, want preparing/PP", d.State, d.Bucket)
	}
	gates := p.Available(printer, snap, testSettings())
	for _, name := range []ActionName{ActionSetNozzleTemperature, ActionSetBedTemperature, ActionSetFanSpeed, ActionStartPrint, ActionSetFilamentDefinition, ActionPausePrint} {
		if g := gateStatus(t, gates, name); g.Status != "blocked" {
			t.Errorf("%s in the start window: %q, want blocked", name, g.Status)
		}
	}
	// The actions list and Execute agree: cancel is blocked in the window with the
	// same reason Execute gives (safety review m3, surface review M2).
	// The 9999 stop is verified, so cancel is offered in the window; the gate and
	// Execute agree in both settings of the switch (see the tests below).
	if g := gateStatus(t, gates, ActionCancelPrint); g.Status != "needs_confirmation" {
		t.Errorf("cancel_print in the start window: %q %q, want needs_confirmation", g.Status, g.Reason)
	}

	// Execute applies the same record (it derives with a nil pending).
	f.setWatchdog(newFakeWatchdog())
	for _, tc := range []struct {
		name   ActionName
		params Params
	}{
		{ActionSetNozzleTemperature, Params{TargetC: 200}},
		{ActionSetBedTemperature, Params{TargetC: 60}},
		{ActionSetFanSpeed, Params{Fan: domain.FanCase, FanPercent: 50}},
		{ActionSetFilamentDefinition, Params{Slot: "T1A", Material: "99002", Color: "#00ff00"}},
		{ActionStartPrint, Params{Filename: "three.gcode"}},
	} {
		if _, err := exec(p, f, printer, tc.name, tc.params, ""); err == nil {
			t.Errorf("%s was allowed in the start window", tc.name)
		}
	}
	if f.cfs.modifyCalls != 0 {
		t.Fatal("a slot edit reached the printer during the start window")
	}
}

func TestStartWindow_UploadAndDeleteRefuseTheInFlightFile(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "window-files")
	f.setSelfTest(100)
	f.cfs9999(func(c *fakeCFS) { c.resetMap(); c.state = 0 })

	_, err := exec(p, f, printer, ActionUploadGCodeFile, Params{Filename: "three.gcode", LocalPath: "x"}, "")
	wantErr(t, err, CodeConflict, "print start of this file is in flight")
	_, err = exec(p, f, printer, ActionDeleteGCodeFile, Params{Filename: "three.gcode"}, "")
	wantErr(t, err, CodeConflict, "in flight")
	// Another file is fine while only the record (not a signal) shows the window.
	f.addFile("other.gcode")
	if _, err := exec(p, f, printer, ActionDeleteGCodeFile, Params{Filename: "other.gcode"}, ""); err != nil {
		t.Fatalf("delete of an unrelated file: %v", err)
	}
}

// With only the printerstate signal (no record: another process or a restart)
// the file is unknown, so upload-over and delete are refused for the window.
func TestStartWindow_SignalOnlyRefusesUploadOverAndDelete(t *testing.T) {
	f, p, printer := cfsSetup(t, "window-signal")
	f.addFile("x.gcode")
	f.cfs9999(func(c *fakeCFS) { c.withSelfTest = 40; c.deviceState = 1 }) // a live self-test: progress with deviceState 1
	_, err := exec(p, f, printer, ActionDeleteGCodeFile, Params{Filename: "x.gcode"}, "")
	wantErr(t, err, CodeConflict, "self-test")
	_, err = exec(p, f, printer, ActionUploadGCodeFile, Params{Filename: "y.gcode", LocalPath: "x"}, "")
	wantErr(t, err, CodeConflict, "self-test")
}

// While the 9999 stop is unverified (the switch off), cancel in the window is
// refused everywhere with the printer-screen reason.
func TestStartWindow_CancelRefusedWhileTheStopIsUnverified(t *testing.T) {
	old := stopDuringStartVerified
	stopDuringStartVerified = false
	t.Cleanup(func() { stopDuringStartVerified = old })
	f, p, printer, _ := startedFixture(t, "window-cancel")
	_, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "stop it on the printer screen")
	if _, _, _, cancel := f.counts(); cancel != 0 || f.cfs.stopCalls != 0 {
		t.Fatalf("cancel/stop reached the printer: %d/%d", cancel, f.cfs.stopCalls)
	}
}

// Verified live 2026-09-29: cancel in the window sends the 9999 stop frame (never
// Moonraker's cancel), the reply returns once 9999 state reads 7 (or 4) with
// Effect stopping, and it does not wait for the wind-down.
func TestStartWindow_CancelSendsTheNineNineNineNineStop(t *testing.T) {
	if !stopDuringStartVerified {
		t.Fatal("the stop must be verified (supervised session 2026-09-29)")
	}
	f, p, printer, _ := startedFixture(t, "window-stop")
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel proposal in the window: %v", err)
	}
	begin := time.Now()
	res, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if f.cfs.stopCalls != 1 {
		t.Fatalf("stop frames = %d, want 1", f.cfs.stopCalls)
	}
	if _, _, _, cancel := f.counts(); cancel != 0 {
		t.Fatal("a Moonraker cancel was also sent")
	}
	if !res.Accepted || res.Effect != "stopping" {
		t.Fatalf("accepted %v effect %s, want stopping", res.Accepted, res.Effect)
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatalf("the reply took %s: it must not wait for the wind-down", time.Since(begin))
	}
	txt := strings.Join(res.Effects, " ")
	for _, want := range []string{"finishes its current self-test step (about 20 s)", "turns the heaters off", "returns to idle within about a minute", "follow with get_printer_status"} {
		if !strings.Contains(txt, want) {
			t.Errorf("effects missing %q: %v", want, res.Effects)
		}
	}
	if p.locks.get("window-stop").getStartRec() != nil {
		t.Fatal("the start record survived a successful stop")
	}
}

// If neither state 7 nor 4 is seen, the frame was still sent: Effect sent.
func TestStartWindow_CancelWithoutASignOfStoppingIsSent(t *testing.T) {
	oldW, oldI := stopWaitTimeout, stopPollInterval
	stopWaitTimeout, stopPollInterval = 60*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { stopWaitTimeout, stopPollInterval = oldW, oldI })
	f, p, printer, _ := startedFixture(t, "window-stop-nosign")
	f.cfs9999(func(c *fakeCFS) { c.stopNoState = true })
	prop, _ := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	res, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token)
	if err != nil || !res.Accepted || res.Effect != "sent" {
		t.Fatalf("effect %s accepted %v err %v, want sent", res.Effect, res.Accepted, err)
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "returns to idle within about a minute") {
		t.Fatalf("effects = %v", res.Effects)
	}
}

// State 4 (aborted) persists at rest from an earlier stop, so a stale 4 alone is
// not "stopping": the reply is sent and the start record is kept (final review
// m1). State 4 counts once a state other than 4 was seen after the frame.
func TestStartWindow_StaleStateFourAloneIsNotStopping(t *testing.T) {
	oldW, oldI := stopWaitTimeout, stopPollInterval
	stopWaitTimeout, stopPollInterval = 80*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { stopWaitTimeout, stopPollInterval = oldW, oldI })
	f, p, printer, _ := startedFixture(t, "window-stop4-stale")
	f.cfs9999(func(c *fakeCFS) { c.stopNoState = true; c.state = 4 })
	prop, _ := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	res, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token)
	if err != nil || res.Effect != "sent" || !res.Accepted {
		t.Fatalf("effect %s accepted %v err %v, want sent", res.Effect, res.Accepted, err)
	}
	if p.locks.get("window-stop4-stale").getStartRec() == nil {
		t.Fatal("the start record was cleared on a stale 4")
	}
}

func TestStartWindow_StateFourAfterAnotherStateIsStopped(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "window-stop4")
	prop, _ := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	// The confirming call's own snapshot reads the first value, then the stop wait
	// reads 1 (another state) and then 4.
	f.cfs9999(func(c *fakeCFS) { c.stopNoState = true; c.stateSeq = []int{1, 1, 4} })
	res, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token)
	if err != nil || res.Effect != "stopping" {
		t.Fatalf("effect %s err %v, want stopping", res.Effect, err)
	}
	if p.locks.get("window-stop4").getStartRec() != nil {
		t.Fatal("the record must end once the printer is stopping")
	}
}

// The cancel proposal in the window describes the stop, not END_PRINT (final
// review M4).
func TestStartWindow_CancelProposalDescribesTheStop(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "window-prop")
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil || !prop.Proposed {
		t.Fatalf("proposal: %v", err)
	}
	txt := strings.Join(prop.Effects, " ") + " " + strings.Join(prop.Commands, " ")
	for _, want := range []string{"9999 stop", "about 20 s", "turns the heaters off", "within about a minute", "stop (port 9999)"} {
		if !strings.Contains(txt, want) {
			t.Errorf("proposal missing %q: %s", want, txt)
		}
	}
	for _, bad := range []string{"END_PRINT", "PrintCancel", "EEPROM"} {
		if strings.Contains(txt, bad) {
			t.Errorf("proposal mentions %q, which the stop path does not do: %s", bad, txt)
		}
	}
	// An ordinary cancel keeps its own description.
	f2, p2, printer2 := cfsSetup(t, "prop-plain")
	f2.setPrinting("m.gcode")
	plain, _ := exec(p2, f2, printer2, ActionCancelPrint, Params{}, "")
	if !strings.Contains(strings.Join(plain.Effects, " "), "END_PRINT") {
		t.Errorf("a plain cancel proposal lost its END_PRINT description: %v", plain.Effects)
	}
}

// A stopped or failed start (9999 state 3 or 4) ends the start record, but only
// in a snapshot taken after the frame and its grace period: state 4 persists at
// rest from an earlier stop and flips to 9 within about a second of the frame.
func TestStartRecord_ClearedByStateThreeOrFourAfterTheFrameAndGrace(t *testing.T) {
	now := time.Now()
	for _, st := range []int{3, 4} {
		pl := &printerLock{}
		rec := &startInFlight{filename: "a", issuedAt: now}
		pl.setStartRec(rec)
		snap := printerstate.Snapshot{Taken: now.Add(10 * time.Second)}
		snap.WS9999.State = crealityws.Int{Value: st, Present: true}

		if pl.activeStartRec(snap, printerstate.Derived{}) == nil {
			t.Fatalf("state %d: cleared before the frame was sent (a stale value from an earlier stop)", st)
		}
		pl.markStartSent()
		rec.sentAt = now // the frame went out at the start
		snap.Taken = now.Add(startStateGrace / 2)
		if pl.activeStartRec(snap, printerstate.Derived{}) == nil {
			t.Fatalf("state %d: cleared inside the grace period", st)
		}
		snap.Taken = now.Add(startStateGrace + time.Second)
		if pl.activeStartRec(snap, printerstate.Derived{}) != nil || pl.getStartRec() != nil {
			t.Fatalf("state %d: not cleared after the grace period", st)
		}
	}
	// States 1, 9, 7 and 0 never clear it.
	for _, st := range []int{0, 1, 7, 9} {
		pl := &printerLock{}
		rec := &startInFlight{filename: "a", issuedAt: now, sentAt: now}
		pl.setStartRec(rec)
		snap := printerstate.Snapshot{Taken: now.Add(startStateGrace + time.Minute)}
		snap.WS9999.State = crealityws.Int{Value: st, Present: true}
		if pl.activeStartRec(snap, printerstate.Derived{}) == nil {
			t.Fatalf("state %d cleared the record", st)
		}
	}
}

func TestStartCFS_StampsTheRecordWithTheTimeTheFrameWasSent(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "stamp")
	_ = f
	_ = printer
	rec := p.locks.get("stamp").getStartRec()
	if rec == nil || rec.sentAt.IsZero() {
		t.Fatalf("record = %+v, want sentAt set", rec)
	}
}

func TestStartWindow_RecordEndsWhenPrintStatsMovesOn(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "window-ends")
	pl := p.locks.get("window-ends")
	if pl.getStartRec() == nil {
		t.Fatal("no start record after the start")
	}
	f.setPrinting("three.gcode")
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if d := p.Derive(snap); d.State != printerstate.StatePrinting {
		t.Fatalf("state = %s, want printing (the record must never shadow it)", d.State)
	}
	if pl.getStartRec() != nil {
		t.Fatal("the record was not cleared once print_stats showed printing")
	}
}

func TestStartWindow_RecordExpiresAfterFifteenMinutes(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "window-expire")
	f.setSelfTest(100)
	f.cfs9999(func(c *fakeCFS) { c.resetMap(); c.state = 0 })
	pl := p.locks.get("window-expire")
	rec := pl.getStartRec()
	rec.issuedAt = rec.issuedAt.Add(-16 * time.Minute)
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if d := p.Derive(snap); d.State != printerstate.StateIdle {
		t.Fatalf("state = %s, want idle after the anti-hang expiry", d.State)
	}
	if pl.getStartRec() != nil {
		t.Fatal("expired record not cleared")
	}
}

// A stale "complete" or "cancelled" left by the previous job must not end the
// record nor hide the window; only a settled state of a DIFFERENT job does.
func TestStartWindow_StaleCompleteOfThePreviousJobKeepsTheWindow(t *testing.T) {
	f, p, printer := cfsSetup(t, "window-stale")
	f.setPrinting("old.gcode")
	f.withLock(func() { f.printState = "complete"; f.sdActive = false })
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	base := printerstate.DeriveActivityState(snap, nil)
	if base.State != printerstate.StateComplete {
		t.Fatalf("setup state = %s", base.State)
	}
	pl := p.locks.get("window-stale")
	pl.setStartRec(&startInFlight{filename: "new.gcode", issuedAt: snap.Taken, priorJob: printerstate.JobIdentityFrom(snap)})
	d := p.Derive(snap)
	if d.State != printerstate.StatePreparing || d.Bucket != printerstate.BucketPP {
		t.Fatalf("stale complete: %s/%s, want preparing/PP", d.State, d.Bucket)
	}
	// A different job's completion ends it.
	f.withLock(func() { f.filename = "new.gcode"; f.jobUUID = "fake-uuid-new" })
	snap2 := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	if d := p.Derive(snap2); d.State != printerstate.StateComplete {
		t.Fatalf("a different job's complete: state = %s, want complete", d.State)
	}
	if pl.getStartRec() != nil {
		t.Fatal("record not cleared by another job's settled state")
	}
}

func TestApplyStartWindow_NeverShadowsOtherStates(t *testing.T) {
	rec := &startInFlight{filename: "x", issuedAt: time.Now()}
	ps := moonraker.PrintStats{State: "standby"}
	snap := printerstate.Snapshot{Taken: time.Now(), PrintStats: &ps}
	for _, d := range []printerstate.Derived{
		{State: printerstate.StatePrinting, Bucket: printerstate.BucketP},
		{State: printerstate.StatePaused, Bucket: printerstate.BucketZ},
		{State: printerstate.StatePausing, Bucket: printerstate.BucketT},
		{State: printerstate.StateError, Bucket: printerstate.BucketE},
	} {
		if got := applyStartWindow(d, snap, rec); got.State != d.State {
			t.Errorf("%s was rewritten to %s", d.State, got.State)
		}
	}
	// The self-test's own homing and calibrating are part of the window too (live
	// bug, supervised print 2026-09-29): cancel must not disappear from it. This
	// used to assert the opposite; the ruling reversed it.
	for _, st := range []string{printerstate.StateHoming, printerstate.StateCalibrating} {
		got := applyStartWindow(printerstate.Derived{State: st, Bucket: printerstate.BucketB}, snap, rec)
		if got.State != printerstate.StatePreparing || got.Bucket != printerstate.BucketPP || !got.StartWindow {
			t.Errorf("%s with an active record = %s/%s window %v, want the start window", st, got.State, got.Bucket, got.StartWindow)
		}
	}
	// A CFS feed during the window (filament_operation, bucket U) is part of it:
	// cancel must not disappear from the window (safety review m3).
	fo := applyStartWindow(printerstate.Derived{State: printerstate.StateFilamentOperation, Bucket: printerstate.BucketU}, snap, rec)
	if fo.State != printerstate.StatePreparing || !fo.StartWindow {
		t.Errorf("filament_operation with an active record = %s window %v, want preparing/true", fo.State, fo.StartWindow)
	}
	if got := applyStartWindow(printerstate.Derived{State: printerstate.StateIdle, Bucket: printerstate.BucketI}, snap, nil); got.State != printerstate.StateIdle {
		t.Error("no record must change nothing")
	}
}

// --- resume with a CFS (plan 3.5, 8a.5) ---

func pausedByServer(t *testing.T, host string) (*fakePrinter, *Policy, domain.Printer) {
	t.Helper()
	f, p, printer := cfsSetup(t, host)
	f.setPrinting("model.gcode")
	res, err := exec(p, f, printer, ActionPausePrint, Params{}, "")
	if err != nil || res.After.ActivityState != "paused" {
		t.Fatalf("pause: %v %+v", err, res.After)
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "resume from this server is possible only while the CFS stays clean") {
		t.Fatalf("pause effects = %v, want the resume disclosure", res.Effects)
	}
	if p.locks.get(host).getPauseRec() == nil {
		t.Fatal("no pause record after a clean CFS pause")
	}
	return f, p, printer
}

func TestResume_AllowedForACleanPauseThisServerIssued(t *testing.T) {
	f, p, printer := pausedByServer(t, "resume-ok")
	prop, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	if err != nil || !prop.Proposed {
		t.Fatalf("resume proposal: %v", err)
	}
	res, err := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token)
	if err != nil || res.After.ActivityState != "printing" {
		t.Fatalf("resume: %v %+v", err, res.After)
	}
	if p.locks.get("resume-ok").getPauseRec() != nil {
		t.Fatal("the pause record must be single-use")
	}
}

func TestResume_RefusedWithoutARecord(t *testing.T) {
	f, p, printer := cfsSetup(t, "resume-norec")
	f.setPrinting("model.gcode")
	f.setPaused() // paused at the printer screen, not by this server
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "not issued by this server")
}

func TestResume_NotRecordedWhenTheCFSWasNotCleanAtPause(t *testing.T) {
	f, p, printer := cfsSetup(t, "resume-dirtypause")
	f.setPrinting("model.gcode")
	f.setCFSErr(7)
	if _, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if p.locks.get("resume-dirtypause").getPauseRec() != nil {
		t.Fatal("a pause with a CFS error must not be recorded")
	}
	f.setCFSErr(0)
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "not issued by this server")
}

func TestResume_RefusedWhileTheCFSIsNotClean(t *testing.T) {
	cases := map[string]func(f *fakePrinter){
		"deviceState 10":     func(f *fakePrinter) { f.setDeviceState(10) },
		"deviceState 11":     func(f *fakePrinter) { f.setDeviceState(11) },
		"feedState 2":        func(f *fakePrinter) { f.setFeedState(2) },
		"feedState absent":   func(f *fakePrinter) { f.omit9999("feedState") },
		"deviceState absent": func(f *fakePrinter) { f.omit9999("deviceState") },
		"repoPlrStatus 1":    func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.repoPlrStatus = 1 }) },
		"upgradeStatus 1":    func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.upgradeStatus = 1 }) },
		"resume_err":         func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.resumeErr = true }) },
		"filament_useup":     func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.useup = 1 }) },
		"err set":            func(f *fakePrinter) { f.setCFSErr(3) },
		"9999 unreachable":   func(f *fakePrinter) { f.setWS9999Unreachable(true) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, p, printer := pausedByServer(t, "resume-dirty-"+strings.ReplaceAll(name, " ", "-"))
			mutate(f)
			_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
			wantErr(t, err, CodeUnavailable, "")
			if _, _, resume, _ := f.counts(); resume != 0 {
				t.Fatal("PrintResume was sent")
			}
		})
	}
}

func TestResume_RefusedWhenMapEnableOrSlotsChanged(t *testing.T) {
	cases := map[string]func(f *fakePrinter){
		"map":    func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.boxMap["T1A"] = "T1B" }) },
		"enable": func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.enable = 0 }) },
		"slot definition": func(f *fakePrinter) {
			f.cfs9999(func(c *fakeCFS) { c.slotAt(1, 3).Color = "#0123456" })
		},
		"unmapped slot rfid": func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.slotAt(1, 0).RFID = "99004" }) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, p, printer := pausedByServer(t, "resume-changed-"+strings.ReplaceAll(name, " ", "-"))
			mutate(f)
			_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
			wantErr(t, err, CodeUnavailable, "changed since the pause")
		})
	}
}

// The record dies as soon as any snapshot shows the job not paused, so a later
// pause at the printer screen can never match it.
func TestResume_RecordClearedWhenTheJobIsNoLongerPaused(t *testing.T) {
	f, p, printer := pausedByServer(t, "resume-stale")
	f.withLock(func() { f.isPaused = false; f.printState = "printing" }) // resumed at the screen
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	p.Available(printer, snap, testSettings())
	if p.locks.get("resume-stale").getPauseRec() != nil {
		t.Fatal("the record survived a snapshot showing the job printing")
	}
	f.setPaused() // paused again at the screen
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "not issued by this server")
}

func TestResume_RecordIsPerJob(t *testing.T) {
	f, p, printer := pausedByServer(t, "resume-otherjob")
	f.withLock(func() { f.filename = "other.gcode"; f.jobUUID = "fake-uuid-other" })
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "")
}

// A pause on a printer with no CFS records nothing and needs nothing.
func TestResume_NoCFSIsUnchanged(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	p := New()
	printer := testPrinter(f, "resume-nocfs")
	if _, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err != nil {
		t.Fatal(err)
	}
	if p.locks.get("resume-nocfs").getPauseRec() != nil {
		t.Fatal("a pause record was kept without a CFS")
	}
	prop, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	if err != nil {
		t.Fatalf("resume proposal without a CFS: %v", err)
	}
	if _, err := exec(p, f, printer, ActionResumePrint, Params{}, prop.Token); err != nil {
		t.Fatalf("resume without a CFS: %v", err)
	}
}

// With a CFS connected, start_print is shown as needing the mapping proposal;
// set_filament_definition is listed and available only while idle.
func TestAvailableFor_CFSStartNeedsConfirmationAndFilamentEditIsListed(t *testing.T) {
	f := newFakePrinter()
	f.setCFSConnected(true)
	gates, _ := gatesOf(t, f)
	if g := gateStatus(t, gates, ActionStartPrint); g.Status != "needs_confirmation" {
		t.Errorf("start_print with a CFS: %q, want needs_confirmation", g.Status)
	}
	if g := gateStatus(t, gates, ActionSetFilamentDefinition); g.Status != "available" {
		t.Errorf("set_filament_definition while idle: %q (%s), want available", g.Status, g.Reason)
	}
	f.setDeviceState(1)
	gates, _ = gatesOf(t, f)
	if g := gateStatus(t, gates, ActionStartPrint); g.Status != "blocked" || !strings.Contains(g.Reason, "cfsStart") {
		t.Errorf("start_print with a busy CFS: %q %q", g.Status, g.Reason)
	}
	f.setPrinting("model.gcode")
	gates, _ = gatesOf(t, f)
	if g := gateStatus(t, gates, ActionSetFilamentDefinition); g.Status != "blocked" {
		t.Errorf("set_filament_definition mid-print: %q, want blocked", g.Status)
	}
}
