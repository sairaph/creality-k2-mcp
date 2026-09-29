package policy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// set_speed_preset tests (plan-v0.3.0.md sections 0-2 and 2a). Fakes only: the
// fake Moonraker/9999 pair follows the macro behaviour of protocol doc 2.3
// (Qmode captures the factor and sets 50%, Qmode_exit restores it).

// fastSpeedSettle keeps the hold short but the cap generous: the fakes are
// event-driven, so a confirming call returns as soon as the hold is observed and
// correctness never depends on how fast the machine is. Tests that expect the
// window to run out use shortCap.
func fastSpeedSettle(t *testing.T) {
	t.Helper()
	oc, oh := speedSettleCap, speedHold
	speedSettleCap, speedHold = 8*time.Second, 40*time.Millisecond
	t.Cleanup(func() { speedSettleCap, speedHold = oc, oh })
}

// shortCap makes a call that is expected NOT to confirm give up quickly. It can
// only turn a pass into an early "unconfirmed", never the reverse.
func shortCap(t *testing.T) {
	t.Helper()
	oc := speedSettleCap
	speedSettleCap = 700 * time.Millisecond
	t.Cleanup(func() { speedSettleCap = oc })
}

func speedSetup(t *testing.T, host string) (*fakePrinter, *Policy, domain.Printer) {
	t.Helper()
	setTestHome(t)
	fastSpeedSettle(t)
	f := newFakePrinter()
	f.setPrinting("model.gcode")
	f.setHot(220)
	return f, New(), testPrinter(f, host)
}

func preset(name string) Params { return Params{Preset: name} }

func mustPreset(t *testing.T, p *Policy, f *fakePrinter, printer domain.Printer, name string) Result {
	t.Helper()
	res, err := exec(p, f, printer, ActionSetSpeedPreset, preset(name), "")
	if err != nil {
		t.Fatalf("set_speed_preset %s: %v", name, err)
	}
	return res
}

func factorNow(f *fakePrinter) float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.speedFactor
}

func silentNow(f *fakePrinter) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.silent
}

func m220Count(f *fakePrinter) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m220Calls
}

func hasEffect(res Result, substr string) bool {
	for _, e := range res.Effects {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// Frames and order per preset (S1, 2a.2): Silent is speedMode:1 only, never a
// factor write; the others send M220 (never setFeedratePct) and speedMode:0
// first only when Silent is on.
func TestSpeedPreset_FramesAndOrder(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-frames")

	res := mustPreset(t, p, f, printer, "silent")
	if res.Effect != "confirmed" || !silentNow(f) || factorNow(f) != 0.5 {
		t.Fatalf("silent: effect=%s silent=%v factor=%v", res.Effect, silentNow(f), factorNow(f))
	}
	if got := f.speedFramesSnapshot(); !reflect.DeepEqual(got, []bool{true}) || m220Count(f) != 0 {
		t.Fatalf("silent sent frames %v and %d M220s, want [true] and none", got, m220Count(f))
	}
	if !hasEffect(res, "speed_mode.json") {
		t.Errorf("the Silent effects do not disclose the persistent file write: %v", res.Effects)
	}

	// Silent -> stable: speedMode:0 first, then M220 S50.
	before := len(f.eventsSnapshot())
	res = mustPreset(t, p, f, printer, "stable")
	ev := f.eventsSnapshot()[before:]
	if !reflect.DeepEqual(ev, []string{"speedMode:false", "m220:50"}) {
		t.Fatalf("stable from silent sent %v, want speedMode:false then m220:50", ev)
	}
	if res.Effect != "confirmed" || silentNow(f) || factorNow(f) != 0.5 {
		t.Fatalf("stable: effect=%s silent=%v factor=%v", res.Effect, silentNow(f), factorNow(f))
	}
	if !hasEffect(res, "restores the velocity, acceleration, corner velocity, pressure advance and fan values") {
		t.Errorf("leaving Silent does not say what it restores: %v", res.Effects)
	}

	// Silent off: M220 only, no speedMode frame.
	frames := len(f.speedFramesSnapshot())
	res = mustPreset(t, p, f, printer, "standard")
	if res.Effect != "confirmed" || factorNow(f) != 1.0 || len(f.speedFramesSnapshot()) != frames {
		t.Fatalf("standard: effect=%s factor=%v frames=%v", res.Effect, factorNow(f), f.speedFramesSnapshot())
	}
	res = mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "confirmed" || factorNow(f) != 1.25 {
		t.Fatalf("ultrafast: effect=%s factor=%v", res.Effect, factorNow(f))
	}
	if res.After.SpeedPreset != "ultrafast" || res.After.SilentMode != "off" {
		t.Errorf("after block = preset %q silent %q", res.After.SpeedPreset, res.After.SilentMode)
	}
	for _, c := range res.Commands {
		if strings.Contains(c, "setFeedratePct") {
			t.Errorf("commands mention setFeedratePct: %v", res.Commands)
		}
	}
}

func TestSpeedPreset_NoChange(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-nochange")
	res := mustPreset(t, p, f, printer, "standard")
	if res.Effect != "no_change" || res.Accepted || m220Count(f) != 0 || len(f.speedFramesSnapshot()) != 0 {
		t.Fatalf("standard at 100%%: effect=%s accepted=%v m220=%d frames=%v", res.Effect, res.Accepted, m220Count(f), f.speedFramesSnapshot())
	}
	f.setSilent(true)
	res = mustPreset(t, p, f, printer, "silent")
	if res.Effect != "no_change" || len(f.speedFramesSnapshot()) != 0 {
		t.Fatalf("silent while silent: effect=%s frames=%v", res.Effect, f.speedFramesSnapshot())
	}
	// Silent and Stable are both 50%: stable is NOT a no_change while Silent is on.
	res = mustPreset(t, p, f, printer, "stable")
	if res.Effect != "confirmed" || silentNow(f) || m220Count(f) != 1 {
		t.Fatalf("stable from silent: effect=%s silent=%v m220=%d", res.Effect, silentNow(f), m220Count(f))
	}
	// A 9999 disagreement is not a no_change (fail closed): send again.
	feed := 100
	f.withLock(func() { f.ws9999FeedratePct = &feed })
	res = mustPreset(t, p, f, printer, "stable")
	if res.Effect == "no_change" {
		t.Fatalf("no_change decided while port 9999 disagrees")
	}
}

func TestSpeedPreset_BandRefusals(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-band")
	s := testSettings()
	s.Bands.SpeedFactorMinPercent = 60
	run := func(name string) error {
		_, err := p.Execute(context.Background(), f.deps(), printer, s, ActionSetSpeedPreset, preset(name), "")
		return err
	}
	wantErr(t, run("silent"), CodeInvalidInput, "outside the configured band 60-150")
	wantErr(t, run("stable"), CodeInvalidInput, "outside the configured band")
	if err := run("ultrafast"); err != nil {
		t.Fatalf("ultrafast in band: %v", err)
	}

	// Silent is on and the band excludes every non-Silent preset: say how to leave it.
	f.setSilent(true)
	s.Bands.SpeedFactorMinPercent, s.Bands.SpeedFactorMaxPercent = 130, 150
	perr := wantErr(t, run("standard"), CodeInvalidInput, "outside the configured band")
	for _, want := range []string{"Silent is on", "let the print finish", "cancel it", "printer screen"} {
		if !strings.Contains(perr.Message, want) {
			t.Errorf("refusal %q lacks %q", perr.Message, want)
		}
	}
	// Silent on, an in-band non-Silent preset exists: leaving Silent is allowed and suggested.
	s.Bands.SpeedFactorMinPercent, s.Bands.SpeedFactorMaxPercent = 110, 150
	perr = wantErr(t, run("stable"), CodeInvalidInput, "leave it with ultrafast")
	_ = perr
	if err := run("ultrafast"); err != nil {
		t.Fatalf("leaving Silent to an in-band preset must be allowed: %v", err)
	}
	if silentNow(f) {
		t.Errorf("Silent still on after leaving it")
	}
}

func TestSpeedPreset_BucketRefusals(t *testing.T) {
	setTestHome(t)
	fastSpeedSettle(t)
	// Idle.
	f := newFakePrinter()
	printer := testPrinter(f, "sp-idle")
	p := New()
	_, err := exec(p, f, printer, ActionSetSpeedPreset, preset("standard"), "")
	wantErr(t, err, CodeUnavailable, "not available")
	// Paused.
	f2 := newFakePrinter()
	f2.setPrinting("m.gcode")
	f2.setPaused()
	_, err = exec(p, f2, testPrinter(f2, "sp-paused"), ActionSetSpeedPreset, preset("standard"), "")
	wantErr(t, err, CodeUnavailable, "not available")
	// Preparing (the print-start self-test).
	f3 := newFakePrinter()
	f3.setSelfTest(40)
	_, err = exec(p, f3, testPrinter(f3, "sp-prep"), ActionSetSpeedPreset, preset("standard"), "")
	wantErr(t, err, CodeUnavailable, "not available")
	if len(f.speedFramesSnapshot())+len(f2.speedFramesSnapshot())+len(f3.speedFramesSnapshot()) != 0 {
		t.Errorf("a refused preset sent a frame")
	}
}

func TestSpeedPreset_InvalidName(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-name")
	_, err := exec(p, f, printer, ActionSetSpeedPreset, preset("turbo"), "")
	wantErr(t, err, CodeInvalidInput, "not one of")
	_, err = exec(p, f, printer, ActionSetSpeedPreset, preset(""), "")
	wantErr(t, err, CodeInvalidInput, "not one of")
}

func TestSpeedPreset_CFSRule(t *testing.T) {
	f, p, printer := cfsSetup(t, "sp-cfs")
	fastSpeedSettle(t)
	f.setPrinting("model.gcode")
	res, err := exec(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"), "")
	if err != nil {
		t.Fatalf("healthy CFS: %v", err)
	}
	if !hasEffect(res, "filament-change G-code may override speed and acceleration during a swap") {
		t.Errorf("no CFS swap note: %v", res.Effects)
	}
	f.setCFSErr(4)
	_, err = exec(p, f, printer, ActionSetSpeedPreset, preset("standard"), "")
	wantErr(t, err, CodeUnavailable, "cfsKnownNoError")
}

func TestSpeedPreset_ForbiddenWithoutControl(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-forbid")
	printer.AllowControl = false
	_, err := exec(p, f, printer, ActionSetSpeedPreset, preset("standard"), "")
	wantErr(t, err, CodeForbidden, "allow_control")
}

func TestSpeedPreset_UnconfirmedWhenTheFakeNeverChanges(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-mute")
	shortCap(t)
	f.withLock(func() { f.speedMute = true })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "unconfirmed" || !res.Accepted {
		t.Fatalf("effect=%s accepted=%v, want unconfirmed and accepted", res.Effect, res.Accepted)
	}
}

// 2a.5: Moonraker matches but port 9999 reports something else -> unconfirmed,
// both channels shown, and the state block's preset is unknown, not confirmed.
func TestSpeedPreset_NineNineNineNineDisagreementIsUnconfirmed(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-disagree")
	feed := 100
	f.withLock(func() { f.ws9999FeedratePct = &feed })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "unconfirmed" {
		t.Fatalf("effect = %s, want unconfirmed", res.Effect)
	}
	if res.SpeedPreset == nil || res.SpeedPreset.CurFeedratePct9999 == nil || *res.SpeedPreset.CurFeedratePct9999 != 100 ||
		res.SpeedPreset.MoonrakerFactorPct == nil || int(*res.SpeedPreset.MoonrakerFactorPct+0.5) != 125 {
		t.Fatalf("report = %+v", res.SpeedPreset)
	}
	if res.After.SpeedPreset != "unknown" || res.After.CurFeedratePct9999 == nil || *res.After.CurFeedratePct9999 != 100 {
		t.Errorf("after block preset=%q feed9999=%v, want unknown and 100", res.After.SpeedPreset, res.After.CurFeedratePct9999)
	}
	if !hasEffect(res, "channels disagree") {
		t.Errorf("no disagreement note: %v", res.Effects)
	}

	// A speedMode disagreement too.
	f2, p2, printer2 := speedSetup(t, "sp-disagree2")
	mode := 1
	f2.withLock(func() { f2.ws9999SpeedMode = &mode })
	res = mustPreset(t, p2, f2, printer2, "ultrafast")
	if res.Effect != "unconfirmed" || res.SpeedPreset.SpeedMode9999 == nil {
		t.Fatalf("effect=%s report=%+v", res.Effect, res.SpeedPreset)
	}
}

func TestSpeedPreset_NineNineNineNineSilentIsNoted(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-9999silent")
	f.withLock(func() { f.ws9999OmitSpeed = true })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "confirmed" || !hasEffect(res, "port 9999 did not report speedMode") {
		t.Fatalf("effect=%s effects=%v", res.Effect, res.Effects)
	}
}

// 2a.5: the target must hold for speedHold. The factor landing and then being
// overwritten (Qmode_exit's M220 racing ours) must not confirm.
func TestSpeedPreset_TargetMustHold(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-hold")
	speedSettleCap, speedHold = 1200*time.Millisecond, 500*time.Millisecond
	f.withLock(func() { f.m220OverwriteAfter, f.m220OverwriteTo = 150*time.Millisecond, 1.0 })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "unconfirmed" {
		t.Fatalf("a factor overwritten after 150 ms confirmed: %s", res.Effect)
	}
}

// 2a.3: speedMode:0 was sent and the M220 fails: retry once; when it still
// fails, report the actual state, never a bare error.
func TestSpeedPreset_PartialWhenTheFactorWriteFailsAfterLeavingSilent(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-partial")
	shortCap(t)
	f.withLock(func() { f.speedFactor = 1.25 })
	f.setSilent(true) // entered from ultrafast: Qmode_exit will restore 125%
	f.withLock(func() { f.failM220 = true })
	res, err := exec(p, f, printer, ActionSetSpeedPreset, preset("stable"), "")
	if err != nil {
		t.Fatalf("a failed M220 after speedMode:0 must not be a bare error: %v", err)
	}
	if res.Effect != "partial" || res.Accepted {
		t.Fatalf("effect=%s accepted=%v, want partial", res.Effect, res.Accepted)
	}
	if m220Count(f) != 2 {
		t.Errorf("M220 attempts = %d, want the call and one retry", m220Count(f))
	}
	if !hasEffect(res, "a fresh read shows Silent off and the speed factor at 125%, not 50%") {
		t.Errorf("partial note missing: %v", res.Effects)
	}
	if res.After.ActivityState != "printing" || res.After.SilentMode != "off" {
		t.Errorf("after block = %s / silent %s", res.After.ActivityState, res.After.SilentMode)
	}
}

func TestSpeedPreset_RetryOnceSucceeds(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-retry")
	f.setSilent(true)
	f.withLock(func() { f.failM220Times = 1 })
	res := mustPreset(t, p, f, printer, "standard")
	if res.Effect != "confirmed" || m220Count(f) != 2 {
		t.Fatalf("effect=%s m220=%d, want confirmed after one retry", res.Effect, m220Count(f))
	}
}

func TestSpeedPreset_ModeFrameFailureSendsNoFactor(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-modefail")
	f.setSilent(true)
	f.withLock(func() { f.failSpeedMode = true })
	res, err := exec(p, f, printer, ActionSetSpeedPreset, preset("standard"), "")
	if err != nil {
		t.Fatalf("bare error after a failed frame: %v", err)
	}
	if res.Effect != "unconfirmed" || res.Accepted || m220Count(f) != 0 {
		t.Fatalf("effect=%s accepted=%v m220=%d: a factor must not be sent while Silent stays on", res.Effect, res.Accepted, m220Count(f))
	}
	if !silentNow(f) || res.After.SilentMode != "on" {
		t.Errorf("Silent state after = %v / %s", silentNow(f), res.After.SilentMode)
	}
}

// 2a.6: the job ended between the snapshot and the frame.
type endsBeforeFrame struct{ *fakePrinter }

func (e endsBeforeFrame) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	if _, ok := objects["pause_resume"]; ok && len(objects) == 2 {
		e.withLock(func() { e.printState, e.sdActive = "complete", false })
	}
	return e.fakePrinter.QueryObjects(ctx, objects)
}

func TestSpeedPreset_RefusesWhenThePrintEndedBeforeTheFrame(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-race")
	deps := f.deps()
	deps.Moonraker = endsBeforeFrame{f}
	_, err := p.Execute(context.Background(), deps, printer, testSettings(), ActionSetSpeedPreset, preset("silent"), "")
	wantErr(t, err, CodeUnavailable, "no longer printing")
	if len(f.speedFramesSnapshot()) != 0 {
		t.Fatalf("speedMode:1 was sent although the job had ended")
	}
}

func TestSpeedPreset_StuckSilentPossibleWhenThePrintEndsAfterTheFrame(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-stuck")
	f.withLock(func() { f.endPrintAfterSpeed = true })
	res := mustPreset(t, p, f, printer, "silent")
	if res.Effect != "stuck_silent_possible" {
		t.Fatalf("effect = %s, want stuck_silent_possible", res.Effect)
	}
	if !hasEffect(res, "Klipper restarts") || !hasEffect(res, "power cycle") {
		t.Errorf("the S8 text is missing: %v", res.Effects)
	}
}

// 2a.7: entering Silent here records the job; leaving it for the same job has
// no "entered elsewhere" warning, leaving a foreign Silent does.
func TestSpeedPreset_SilentRecord(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-record")
	mustPreset(t, p, f, printer, "silent")
	res := mustPreset(t, p, f, printer, "standard")
	if hasEffect(res, "entered elsewhere") {
		t.Errorf("warned about a Silent this server entered: %v", res.Effects)
	}
	// Silent entered elsewhere (at the printer screen).
	f.setSilent(true)
	res = mustPreset(t, p, f, printer, "standard")
	if !hasEffect(res, "Silent was entered elsewhere or is left over from an earlier print") {
		t.Errorf("no warning for a Silent this server did not enter: %v", res.Effects)
	}
	// Entered here for one job, still on for another: warn.
	mustPreset(t, p, f, printer, "silent")
	f.setPrinting("other.gcode")
	res = mustPreset(t, p, f, printer, "standard")
	if !hasEffect(res, "entered elsewhere or is left over") {
		t.Errorf("no warning when the record belongs to another job: %v", res.Effects)
	}
}

// 2a.1/2a.11: S9 and the gate.
func TestSetSpeedFactor_BlockedWhileSilentOrUnknown(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-s9")
	f.setSilent(true)
	_, err := exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 50}, "")
	perr := wantErr(t, err, CodeUnavailable, "Silent mode is active: when it ends it restores the speed factor it saved on entry, which would silently discard this change; use set_speed_preset")
	_ = perr
	_, err = exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 100}, "")
	wantErr(t, err, CodeUnavailable, "Silent mode is active")

	// The actions list says the same.
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	gates := p.Available(printer, snap, testSettings())
	for _, g := range gates {
		if g.Name == "set_speed_factor" && (g.Status != "blocked" || !strings.Contains(g.Reason, "use set_speed_preset")) {
			t.Errorf("set_speed_factor gate = %+v", g)
		}
		if g.Name == "set_speed_preset" && g.Status != "available" {
			t.Errorf("set_speed_preset gate = %+v", g)
		}
	}

	// Unknown (fail closed): both source objects absent, or disagreeing.
	f2, p2, printer2 := speedSetup(t, "sp-s9-unknown")
	f2.withLock(func() { f2.qmodeOmit = true })
	_, err = exec(p2, f2, printer2, ActionSetSpeedFactor, Params{Percent: 100}, "")
	wantErr(t, err, CodeUnavailable, "could not be read")
	_, err = exec(p2, f2, printer2, ActionSetSpeedPreset, preset("standard"), "")
	wantErr(t, err, CodeUnavailable, "could not be read")
	f3, p3, printer3 := speedSetup(t, "sp-s9-mismatch")
	f3.withLock(func() { f3.qmodeMismatch = true })
	_, err = exec(p3, f3, printer3, ActionSetSpeedFactor, Params{Percent: 100}, "")
	wantErr(t, err, CodeUnavailable, "disagree")
	if m220Count(f2)+m220Count(f3) != 0 {
		t.Errorf("a blocked action sent an M220")
	}

	// Silent off: unchanged behaviour.
	f4, p4, printer4 := speedSetup(t, "sp-s9-off")
	if _, err := exec(p4, f4, printer4, ActionSetSpeedFactor, Params{Percent: 120}, ""); err != nil {
		t.Fatalf("set_speed_factor with Silent off: %v", err)
	}
}

// 2a.8: while Silent is on M106 caps the fans; the result says so and settles
// against the capped value instead of reporting a false unconfirmed.
func TestSetFanSpeed_UnderSilentIsCappedAndSaysSo(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-fan")
	f.setPartFan(40)
	f.setSilent(true)
	res, err := exec(p, f, printer, ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 80}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "confirmed" || !hasEffect(res, "Silent mode is on: it caps all fans") {
		t.Fatalf("effect=%s effects=%v", res.Effect, res.Effects)
	}
	// Below the cap: the requested value applies.
	res, err = exec(p, f, printer, ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 45}, "")
	if err != nil || res.Effect != "confirmed" {
		t.Fatalf("45%% under Silent: %v %s", err, res.Effect)
	}
}

// 2a.9: the stuck-Silent warning in start_print's result (a warning, not a refusal).
func TestStartPrint_WarnsWhenSilentIsStuckOn(t *testing.T) {
	setTestHome(t)
	fastTimers(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	f.withLock(func() { f.silent = true }) // idle with the flag left on
	p := New()
	printer := testPrinter(f, "sp-start")
	res, err := exec(p, f, printer, ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if err != nil {
		t.Fatalf("a stuck Silent must not refuse the start: %v", err)
	}
	if !hasEffect(res, StuckSilentText) {
		t.Errorf("start_print result does not warn about the stuck Silent: %v", res.Effects)
	}
	// No warning when Silent is off.
	f2 := newFakePrinter()
	f2.addFile("model.gcode")
	res2, err := exec(New(), f2, testPrinter(f2, "sp-start2"), ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if err != nil || hasEffect(res2, "Silent mode is still on") {
		t.Errorf("unexpected warning or error: %v %v", err, res2.Effects)
	}
	// The proposal path (a CFS start) adds it through proposalWarnings.
	d := printerstate.Derived{Bucket: printerstate.BucketI, Qmode: printerstate.QmodeOn}
	if got := proposalWarnings(ActionStartPrint, []string{"x"}, d); len(got) != 2 || got[1] != StuckSilentText {
		t.Errorf("proposalWarnings = %v", got)
	}
	if got := proposalWarnings(ActionCancelPrint, []string{"x"}, d); len(got) != 1 {
		t.Errorf("proposalWarnings changed another action: %v", got)
	}
}

// --- 2a.4: pause and cancel pre-empt an in-flight setpoint action ---

func TestPauseAndCancelPreemptAnInFlightSetpointAction(t *testing.T) {
	type tc struct {
		name   string
		action ActionName
		params Params
		setup  func(f *fakePrinter)
	}
	cases := []tc{
		{"speed preset", ActionSetSpeedPreset, preset("ultrafast"), nil},
		{"speed factor", ActionSetSpeedFactor, Params{Percent: 110}, nil},
		{"flow factor", ActionSetFlowFactor, Params{Percent: 105}, nil},
		{"fan", ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 60}, func(f *fakePrinter) { f.setPartFan(50) }},
		{"nozzle", ActionSetNozzleTemperature, Params{TargetC: 225}, nil},
		{"bed", ActionSetBedTemperature, Params{TargetC: 62}, func(f *fakePrinter) { f.withLock(func() { f.bedTarget = 60 }) }},
		{"light", ActionSetLight, Params{On: true}, func(f *fakePrinter) { f.withLock(func() { f.blockLight = true }) }},
	}
	for _, killer := range []ActionName{ActionPausePrint, ActionCancelPrint} {
		for i, c := range cases {
			c, killer := c, killer
			t.Run(string(killer)+"/"+c.name, func(t *testing.T) {
				fastLifecycle(t)
				f, p, printer := speedSetup(t, "sp-preempt-"+string(killer)+"-"+string(rune('a'+i)))
				if c.setup != nil {
					c.setup(f)
				}
				if c.action != ActionSetLight {
					f.withLock(func() { f.blockTemplate = true })
				}
				// The cancel proposal is requested first, while nothing is in flight: only
				// the confirming call (with the token) may pre-empt.
				token := ""
				if killer == ActionCancelPrint {
					prop, err := exec(p, f, printer, killer, Params{}, "")
					if err != nil || !prop.Proposed {
						t.Fatalf("cancel proposal: %v %+v", err, prop)
					}
					token = prop.Token
				}
				var wg sync.WaitGroup
				wg.Add(1)
				var setRes Result
				var setErr error
				go func() {
					defer wg.Done()
					setRes, setErr = exec(p, f, printer, c.action, c.params, "")
				}()
				// Wait until the setpoint action is really in flight (holding the lock).
				deadline := time.Now().Add(20 * time.Second)
				for {
					f.mu.Lock()
					inFlight := f.templateCalls > 0 || f.lightHeld > 0
					f.mu.Unlock()
					if inFlight && lockHeld(p, printer) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the setpoint action never went in flight")
					}
					time.Sleep(5 * time.Millisecond)
				}
				start := time.Now()
				res, err := exec(p, f, printer, killer, Params{}, token)
				if err != nil {
					t.Fatalf("%s failed while a setpoint action was in flight: %v", killer, err)
				}
				if took := time.Since(start); took > preemptWait+10*time.Second {
					t.Errorf("%s took %v to pre-empt", killer, took)
				}
				if killer == ActionPausePrint && res.Effect != "confirmed" && res.Effect != "pausing" {
					t.Errorf("pause effect = %s", res.Effect)
				}
				if killer == ActionCancelPrint && (res.Proposed || res.Effect != "confirmed") {
					t.Errorf("confirming cancel = %+v", res)
				}
				wg.Wait()
				if setErr != nil {
					t.Fatalf("the pre-empted action returned an error instead of a result: %v", setErr)
				}
				if setRes.Effect != "preempted" {
					t.Errorf("pre-empted action effect = %q, want preempted", setRes.Effect)
				}
				if setRes.After.PrinterID == "" {
					t.Errorf("pre-empted result carries no fresh snapshot")
				}
			})
		}
	}
}

// lockHeld reports whether some action currently holds this printer's lock.
func lockHeld(p *Policy, printer domain.Printer) bool {
	pl := p.locks.get(printer.Hostname)
	if pl.mu.TryLock() {
		pl.mu.Unlock()
		return false
	}
	return true
}

// A non-pre-emptible action in flight still conflicts with a pause, as before
// (only setpoint actions are pre-empted).
func TestPauseStillConflictsWithANonPreemptibleAction(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-nopreempt")
	pl := p.locks.get(printer.Hostname)
	pl.mu.Lock() // an action that registered no cancel func
	defer pl.mu.Unlock()
	_, err := exec(p, f, printer, ActionPausePrint, Params{}, "")
	wantErr(t, err, CodeConflict, "already in progress")
}

// A finished setpoint action must not be cancelled by a later pause: the
// cancel func is cleared on release.
func TestPreemptCancelFuncIsClearedOnRelease(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-clear")
	mustExecute(t, p, f, printer, ActionSetSpeedFactor, Params{Percent: 110}, "")
	pl := p.locks.get(printer.Hostname)
	// A released action leaves no cancel func: a pre-empting caller simply takes
	// the free lock instead of "pre-empting" something that already finished.
	if got := pl.tryAcquire(nil, true); got != lockTaken {
		t.Fatalf("tryAcquire after release = %v, want lockTaken", got)
	}
	pl.unlockHolder()
}

// The actions list for set_speed_preset and set_speed_factor per state and
// Silent state, computed from the same derived value Execute gates on.
func TestAvailableFor_SpeedPresetAndSpeedFactor(t *testing.T) {
	cases := []struct {
		name         string
		bucket       printerstate.Bucket
		qmode        printerstate.QmodeState
		preset, fact string
	}{
		{"printing, Silent off", printerstate.BucketP, printerstate.QmodeOff, "available", "available"},
		{"printing, Silent on", printerstate.BucketP, printerstate.QmodeOn, "available", "blocked"},
		{"printing, Silent unknown", printerstate.BucketP, printerstate.QmodeUnknown, "blocked", "blocked"},
		{"paused", printerstate.BucketZ, printerstate.QmodeOff, "blocked", "blocked"},
		{"idle", printerstate.BucketI, printerstate.QmodeOff, "blocked", "blocked"},
		{"preparing", printerstate.BucketPP, printerstate.QmodeOff, "blocked", "blocked"},
	}
	for _, c := range cases {
		d := printerstate.Derived{State: "x", Bucket: c.bucket, Class: printerstate.ClassBusy, Qmode: c.qmode, CFSKnown: true, CFSQuiescent: true}
		gates := AvailableFor(d, testSettings())
		if g := gateStatus(t, gates, ActionSetSpeedPreset); g.Status != c.preset {
			t.Errorf("%s: set_speed_preset = %q, want %q (%s)", c.name, g.Status, c.preset, g.Reason)
		}
		if g := gateStatus(t, gates, ActionSetSpeedFactor); g.Status != c.fact {
			t.Errorf("%s: set_speed_factor = %q, want %q (%s)", c.name, g.Status, c.fact, g.Reason)
		}
	}
}

// Every setpoint-type action is pre-emptible and nothing else is.
func TestPreemptibleSet(t *testing.T) {
	want := map[ActionName]bool{
		ActionSetSpeedPreset: true, ActionSetSpeedFactor: true, ActionSetFlowFactor: true, ActionSetFanSpeed: true,
		ActionSetNozzleTemperature: true, ActionSetBedTemperature: true, ActionSetLight: true,
	}
	for _, name := range Actions {
		if preemptible(name) != want[name] {
			t.Errorf("preemptible(%s) = %v, want %v", name, preemptible(name), want[name])
		}
	}
}

// 2a.8: the firmware does not clamp velocity, so no confirmation or gate may
// read toolhead.max_velocity (it is shown as information only).
func TestSpeedPresetNeverReadsToolheadVelocity(t *testing.T) {
	for _, file := range []string{"execute_speedpreset.go", "checks.go", "actions.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "MaxVelocity") {
			t.Errorf("%s reads Toolhead.MaxVelocity", file)
		}
	}
}

// A setpoint action still waiting for the cross-process lock is pre-empted too:
// it reports preempted rather than a conflict, and the pause goes through.
func TestPreemptWhileWaitingForTheCrossProcessLock(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-preempt-flock")
	path, err := domain.LockPath(printer.Hostname)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	other := flock.New(path) // another server process holding the printer
	if err := other.Lock(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var setRes Result
	var setErr error
	go func() {
		defer wg.Done()
		setRes, setErr = exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 110}, "")
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !lockHeld(p, printer) {
		if time.Now().After(deadline) {
			t.Fatal("the setpoint action never took the in-process lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = other.Unlock()
	}()
	if _, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err != nil {
		t.Fatalf("pause_print failed while a setpoint action waited for the file lock: %v", err)
	}
	wg.Wait()
	if setErr != nil || setRes.Effect != "preempted" {
		t.Fatalf("pre-empted action = %v, %+v", setErr, setRes.Effect)
	}
	if m220Count(f) != 0 {
		t.Errorf("the pre-empted action sent an M220 although it never got the lock")
	}
}
