package policy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// Tests for the final safety and surface reviews (dev_docs/v0.2.0-review-*.md).

// --- B1: the watchdog is disarmed only after the map check passed ---

func TestStartCFS_DisarmsTheWatchdogOnlyAfterTheMapCheckAndBeforeTheStartFrame(t *testing.T) {
	f, p, printer := startSetup(t, "b1-order")
	wd := newFakeWatchdog()
	f.setWatchdog(wd)
	wd.record = func(s string) {
		f.mu.Lock()
		f.cfs.wsEvents = append(f.cfs.wsEvents, s)
		f.mu.Unlock()
	}
	prop := propose(t, p, f, printer, startParams())
	if _, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.wsEventList(), ",")
	if got != "colorMatch,watchdog_disarmed:b1-order,multiColorPrint" {
		t.Fatalf("event order = %s, want colorMatch, then the disarm, then the start frame", got)
	}
}

func TestStartCFS_RefusedStartLeavesTheWatchdogUntouched(t *testing.T) {
	f, p, printer := startSetup(t, "b1-mismatch")
	wd := newFakeWatchdog()
	f.setWatchdog(wd)
	f.setExtrudeFactor(80)
	f.cfs9999(func(c *fakeCFS) { c.forcedMapping = map[string]string{"T1A": "T1D"} })
	prop := propose(t, p, f, printer, startParams())
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil || res.Effect != "refused_map_mismatch" {
		t.Fatalf("effect %s err %v", res.Effect, err)
	}
	if len(wd.disarmCalls) != 0 {
		t.Fatalf("watchdog disarmed %d times by a refused start; the preheated heater would stay on", len(wd.disarmCalls))
	}
}

func TestStartCFS_FlowResetFailureLeavesTheWatchdogUntouched(t *testing.T) {
	f, p, printer := startSetup(t, "b1-m221")
	wd := newFakeWatchdog()
	f.setWatchdog(wd)
	f.setExtrudeFactor(80)
	prop := propose(t, p, f, printer, startParams())
	f.withLock(func() { f.failTemplate = true })
	_, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	wantErr(t, err, CodeUnavailable, "could not reset the flow factor")
	if len(wd.disarmCalls) != 0 || len(f.wsEventList()) != 0 {
		t.Fatalf("disarms %d frames %v: nothing may be touched when the M221 reset fails", len(wd.disarmCalls), f.wsEventList())
	}
}

// --- m1: an ambiguous start-frame write error is unconfirmed ---

func TestStartCFS_StartFrameWriteErrorIsUnconfirmedAndKeepsTheRecordAndFlow(t *testing.T) {
	f, p, printer := startSetup(t, "m1-ambiguous")
	f.setExtrudeFactor(80)
	f.cfs9999(func(c *fakeCFS) { c.startAmbiguous = true })
	prop := propose(t, p, f, printer, startParams())
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "unconfirmed" || res.Accepted {
		t.Fatalf("effect %s accepted %v, want unconfirmed", res.Effect, res.Accepted)
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "the print may be starting") {
		t.Fatalf("effects = %v", res.Effects)
	}
	if res.StartPrintFlowRestored != nil {
		t.Fatal("the flow must not be restored under a print that may be starting")
	}
	f.mu.Lock()
	flow := f.extrudeFactor
	f.mu.Unlock()
	if flow != 1.0 {
		t.Fatalf("flow = %v, want it left at 100%%", flow)
	}
	if p.locks.get("m1-ambiguous").getStartRec() == nil {
		t.Fatal("the start record was cleared for a start that may be running")
	}
}

// --- flow disclosure (surface review M4) ---

func TestStartCFS_FlowResetIsDisclosedInTheProposalAndTheResult(t *testing.T) {
	f, p, printer := startSetup(t, "flow-disclose")
	f.setExtrudeFactor(85)
	prop := propose(t, p, f, printer, startParams())
	txt := strings.Join(prop.Effects, " ")
	if !strings.Contains(txt, "flow factor is 85% and will be reset to 100% first") || !strings.Contains(txt, "stays at 100% once the start is sent") {
		t.Fatalf("proposal effects = %v", prop.Effects)
	}
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil || res.Effect != "sent" {
		t.Fatal(err, res.Effect)
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "reset from 85% to 100% for this start and stays at 100%") {
		t.Fatalf("result effects = %v", res.Effects)
	}

	f2, p2, printer2 := startSetup(t, "flow-restored")
	f2.setExtrudeFactor(85)
	f2.cfs9999(func(c *fakeCFS) { c.forcedMapping = map[string]string{"T1A": "T1D"} })
	prop = propose(t, p2, f2, printer2, startParams())
	res, _ = exec(p2, f2, printer2, ActionStartPrint, startParams(), prop.Token)
	if !strings.Contains(strings.Join(res.Effects, " "), "the flow factor was restored to 85%") {
		t.Fatalf("mismatch effects = %v", res.Effects)
	}
}

// --- M5 (surface): CFS-only arguments without a CFS are refused ---

func TestStartPrint_CFSOnlyArgumentsWithoutACFSAreRefused(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	p := New()
	printer := testPrinter(f, "no-cfs-args")
	for name, params := range map[string]Params{
		"source":    {Filename: "model.gcode", Source: "cfs"},
		"slot_map":  {Filename: "model.gcode", SlotMap: "T1A=T1B"},
		"self_test": {Filename: "model.gcode", SelfTest: true, SelfTestExplicit: true},
	} {
		_, err := exec(p, f, printer, ActionStartPrint, params, "")
		wantErr(t, err, CodeInvalidInput, "only apply with a CFS connected")
		if start, _, _, _ := f.counts(); start != 0 {
			t.Fatalf("%s: a plain print was started", name)
		}
	}
	if _, err := exec(p, f, printer, ActionStartPrint, Params{Filename: "model.gcode"}, ""); err != nil {
		t.Fatalf("plain start: %v", err)
	}
}

// --- M3: the start record ignores printId and the history head ---

func TestStartWindow_OnlyFilenameUuidOrStartTimeEndsTheRecordOverAStaleComplete(t *testing.T) {
	f, p, printer := cfsSetup(t, "m3-printid")
	f.setPrinting("old.gcode")
	f.withLock(func() { f.printState = "complete"; f.sdActive = false })
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	pl := p.locks.get("m3-printid")
	pl.setStartRec(&startInFlight{filename: "new.gcode", issuedAt: snap.Taken, priorJob: printerstate.JobIdentityFrom(snap)})

	// The printer assigns a new printId (and history head) at the start frame,
	// while print_stats still shows the previous job's complete.
	snap2 := snap
	snap2.WS9999.PrintID = crealityws.Str{Value: "brand-new-print-id", Present: true}
	snap2.History = moonraker.HistoryList{Jobs: []moonraker.HistoryJob{{JobID: "00000F", StartTime: 9999}}}
	if d := p.Derive(snap2); d.State != printerstate.StatePreparing {
		t.Fatalf("state = %s, want preparing: a new printId must not end the record", d.State)
	}
	if pl.getStartRec() == nil {
		t.Fatal("record cleared by a printId change")
	}
	// A real new job (uuid or start_time) does end it.
	snap3 := snap
	snap3.VirtualSDCard = &moonraker.VirtualSDCard{CurPrintData: &moonraker.CurPrintData{Filename: "old.gcode", StartTime: 424242}}
	p.Derive(snap3)
	if pl.getStartRec() != nil {
		t.Fatal("a changed start_time must end the record")
	}
}

// --- m4: compare-and-clear ---

func TestStartRecord_AStaleSnapshotNeverWipesANewerRecord(t *testing.T) {
	pl := &printerLock{}
	now := time.Now()
	old := &startInFlight{filename: "a", issuedAt: now.Add(-20 * time.Minute)}
	pl.setStartRec(old)
	fresh := &startInFlight{filename: "b", issuedAt: now}
	// The old record is judged expired, but a newer one was installed meanwhile.
	pl.setStartRec(fresh)
	pl.clearStartRecIf(old)
	if pl.getStartRec() != fresh {
		t.Fatal("clearStartRecIf wiped a record it did not judge")
	}
	// A snapshot older than the record never clears it.
	snap := printerstate.Snapshot{Taken: now.Add(-time.Minute), PrintStats: &moonraker.PrintStats{State: "printing"}}
	if pl.activeStartRec(snap) == nil {
		t.Fatal("a snapshot taken before the record cleared it")
	}
}

func TestPauseRecord_PartialSnapshotsAndOlderSnapshotsNeverClearIt(t *testing.T) {
	pl := &printerLock{}
	now := time.Now()
	rec := &pauseRecord{at: now, job: &printerstate.JobIdentity{Filename: "m.gcode"}}
	pl.setPauseRec(rec)
	paused := true
	// Partial: pause_resume missing, print_stats missing, is_paused missing.
	for name, snap := range map[string]printerstate.Snapshot{
		"no print_stats":  {Taken: now.Add(time.Second), PauseResume: &moonraker.PauseResume{IsPaused: &paused}},
		"no pause_resume": {Taken: now.Add(time.Second), PrintStats: &moonraker.PrintStats{State: "printing"}},
		"is_paused nil":   {Taken: now.Add(time.Second), PrintStats: &moonraker.PrintStats{State: "printing"}, PauseResume: &moonraker.PauseResume{}},
		"older snapshot":  {Taken: now.Add(-time.Second), PrintStats: &moonraker.PrintStats{State: "printing"}, PauseResume: &moonraker.PauseResume{IsPaused: boolp(false)}},
	} {
		pl.observePause(snap)
		if pl.getPauseRec() == nil {
			t.Fatalf("%s cleared the pause record", name)
		}
	}
	pl.observePause(printerstate.Snapshot{Taken: now.Add(time.Second), PrintStats: &moonraker.PrintStats{State: "printing"}, PauseResume: &moonraker.PauseResume{IsPaused: boolp(false)}})
	if pl.getPauseRec() != nil {
		t.Fatal("a positive not-paused snapshot must clear the record")
	}
}

func boolp(b bool) *bool { return &b }

// --- M4: the 9999 reads on the pause and resume paths are bounded ---

func TestPause_HungBoxsInfoDoesNotHoldTheLockPastItsBound(t *testing.T) {
	old := pauseRecordReadTimeout
	pauseRecordReadTimeout = 80 * time.Millisecond
	t.Cleanup(func() { pauseRecordReadTimeout = old })

	f, p, printer := cfsSetup(t, "m4-hang")
	f.setPrinting("model.gcode")
	f.cfs9999(func(c *fakeCFS) { c.boxsInfoHang = true })
	begin := time.Now()
	res, err := exec(p, f, printer, ActionPausePrint, Params{}, "")
	if err != nil || res.After.ActivityState != "paused" {
		t.Fatalf("pause: %v %+v", err, res.After)
	}
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("pause took %s with a hung 9999", d)
	}
	if p.locks.get("m4-hang").getPauseRec() != nil {
		t.Fatal("a pause record was kept although its slot read failed")
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "no resume record was kept") {
		t.Fatalf("effects = %v, want the no-record disclosure", res.Effects)
	}
	// A cancel right after is not answered with a conflict.
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil {
		t.Fatalf("cancel after the pause: %v", err)
	}
	if _, err := exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token); err != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestResume_HungBoxsInfoIsBoundedAndRefuses(t *testing.T) {
	f, p, printer := pausedByServer(t, "m4-resume")
	old := pauseRecordReadTimeout
	pauseRecordReadTimeout = 80 * time.Millisecond
	t.Cleanup(func() { pauseRecordReadTimeout = old })
	f.cfs9999(func(c *fakeCFS) { c.boxsInfoHang = true })
	begin := time.Now()
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	wantErr(t, err, CodeUnavailable, "slot definitions could not be read")
	if time.Since(begin) > 3*time.Second {
		t.Fatal("resume held the lock too long")
	}
}

// --- m3 (safety): the gate list agrees with Execute ---

func TestAvailable_ResumeWithoutARecordIsBlockedWithTheExecuteReason(t *testing.T) {
	f, p, printer := cfsSetup(t, "m3-resume")
	f.setPrinting("model.gcode")
	f.setPaused() // paused at the printer screen: no record
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	gates := p.Available(printer, snap, testSettings())
	g := gateStatus(t, gates, ActionResumePrint)
	if g.Status != "blocked" || !strings.Contains(g.Reason, noPauseRecordReason) {
		t.Fatalf("resume gate = %q %q", g.Status, g.Reason)
	}
	_, err := exec(p, f, printer, ActionResumePrint, Params{}, "")
	perr := wantErr(t, err, CodeUnavailable, noPauseRecordReason)
	if perr.Message != g.Reason {
		t.Fatalf("gate reason and Execute message differ:\n%s\n%s", g.Reason, perr.Message)
	}

	f2, p2, printer2 := pausedByServer(t, "m3-resume2")
	snap = printerstate.Take(context.Background(), f2.deps().stateDeps(), printer2)
	if g := gateStatus(t, p2.Available(printer2, snap, testSettings()), ActionResumePrint); g.Status != "needs_confirmation" {
		t.Fatalf("with a record the gate = %q (%s), want needs_confirmation", g.Status, g.Reason)
	}
}

func TestAvailable_CancelInTheWindowSaysTheSameAsExecute(t *testing.T) {
	for _, verified := range []bool{true, false} {
		old := stopDuringStartVerified
		stopDuringStartVerified = verified
		t.Cleanup(func() { stopDuringStartVerified = old })
		f, p, printer, _ := startedFixture(t, fmt.Sprintf("m3-cancel-%v", verified))
		snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
		g := gateStatus(t, p.Available(printer, snap, testSettings()), ActionCancelPrint)
		_, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
		if verified {
			if err != nil || g.Status != "needs_confirmation" {
				t.Fatalf("verified: gate %q %q, Execute err %v", g.Status, g.Reason, err)
			}
		} else {
			perr := wantErr(t, err, CodeUnavailable, "stop it on the printer screen")
			if g.Status != "blocked" || g.Reason != perr.Message {
				t.Fatalf("unverified: gate %q %q vs Execute %q", g.Status, g.Reason, perr.Message)
			}
		}
	}
	// An ordinary START_PRINT prepare phase (print_stats printing) is not the window.
	f2, p2, printer2 := cfsSetup(t, "m3-cancel2")
	f2.setPreparing("model.gcode")
	snap := printerstate.Take(context.Background(), f2.deps().stateDeps(), printer2)
	if g := gateStatus(t, p2.Available(printer2, snap, testSettings()), ActionCancelPrint); g.Status != "needs_confirmation" {
		t.Fatalf("cancel in the ordinary prepare phase = %q, want needs_confirmation", g.Status)
	}
}

// A CFS feed during the window still leaves cancel reachable once the stop is
// verified (the record is applied over filament_operation).
func TestStartWindow_FeedingDuringTheWindowStaysInTheWindow(t *testing.T) {
	f, p, printer, _ := startedFixture(t, "m3-feed")
	f.setSelfTest(100)
	f.cfs9999(func(c *fakeCFS) { c.resetMap(); c.state = 0 })
	f.setFeedState(2)
	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	d := p.Derive(snap)
	if d.State != printerstate.StatePreparing || !d.StartWindow || d.Bucket != printerstate.BucketPP {
		t.Fatalf("state %s/%s window %v, want preparing/PP/true", d.State, d.Bucket, d.StartWindow)
	}
}

// --- m5: catalog validation ---

func TestSetFilament_CatalogEntryWithoutATypeOrNameIsRefused(t *testing.T) {
	f, p, printer := filamentSetup(t, "m5-type")
	f.cfs9999(func(c *fakeCFS) {
		pa := 0.04
		c.catalog = append(c.catalog,
			crealityws.CatalogEntry{ID: "99020", Brand: "Acme", Name: "Acme NoType", Type: "", MinTemp: 190, MaxTemp: 240, PressureAdvance: &pa},
			crealityws.CatalogEntry{ID: "99021", Brand: "Acme", Name: " ", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: &pa})
	})
	for _, id := range []string{"99020", "99021"} {
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", id, "#00ff00"), "")
		wantErr(t, err, CodeInvalidInput, "no material type or no name")
	}
	if f.cfs.modifyCalls != 0 {
		t.Fatal("a write was sent")
	}
}

// --- the three verification switches stay off together (safety test gap 7) ---

// The switches verified in the supervised session (2026-09-29) are on; the spool
// start stays off: with a CFS connected the side spool is not in the feed path
// and it could not be tested.
func TestVerificationSwitchesMatchWhatWasVerifiedOnHardware(t *testing.T) {
	if !sideSpoolEditVerified || !stopDuringStartVerified {
		t.Fatalf("side spool edit %v, stop in window %v: both were verified live and must be on", sideSpoolEditVerified, stopDuringStartVerified)
	}
	if spoolStartVerified {
		t.Fatal("spoolStartVerified must stay off: it could not be verified on hardware")
	}
}
