package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// start_print with a CFS connected (plan 3.4, 8a): proposal, token, mapping,
// verifyMap, the start frame, and every binding.

const (
	threeMaterial = "PLA;PETG;PETG"
	threeColors   = "#FF0000;#000000;#FFFFFF"
)

func startSetup(t *testing.T, host string) (*fakePrinter, *Policy, domain.Printer) {
	t.Helper()
	fastTimers(t)
	f, p, printer := cfsSetup(t, host)
	f.addCFSFile("three.gcode", threeMaterial, threeColors)
	return f, p, printer
}

func startParams() Params { return Params{Filename: "three.gcode"} }

func propose(t *testing.T, p *Policy, f *fakePrinter, printer domain.Printer, params Params) Result {
	t.Helper()
	prop, err := exec(p, f, printer, ActionStartPrint, params, "")
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}
	if !prop.Proposed || prop.Token == "" {
		t.Fatalf("not a proposal: %+v", prop)
	}
	if f.cfs.colorMatchCalls != 0 || len(f.wsEventList()) != 0 {
		t.Fatalf("a proposal sent something: %v", f.wsEventList())
	}
	return prop
}

func TestStartCFS_ProposalShowsTheMappingAndSendsNothing(t *testing.T) {
	f, p, printer := startSetup(t, "start-proposal")
	prop := propose(t, p, f, printer, startParams())
	if len(prop.Mapping) != 3 {
		t.Fatalf("mapping = %+v", prop.Mapping)
	}
	want := []struct{ tool, slot, ftype string }{{"T1A", "T1A", "PLA"}, {"T1B", "T1B", "PETG"}, {"T1C", "T1D", "PETG"}}
	for i, w := range want {
		m := prop.Mapping[i]
		if m.ToolID != w.tool || m.Slot != w.slot || m.FileType != w.ftype || m.SlotType != w.ftype || m.Index != i {
			t.Errorf("row %d = %+v, want %+v", i, m, w)
		}
		if m.Distance != 0 || m.SlotColor == "" || m.SlotName == "" || m.SlotVendor != "Acme" {
			t.Errorf("row %d details = %+v", i, m)
		}
	}
	txt := strings.Join(prop.Effects, "\n")
	for _, want := range []string{"slots cannot be checked for physical filament", "self-test before printing: false", "Device Manager algorithm"} {
		if !strings.Contains(txt, want) {
			t.Errorf("effects missing %q:\n%s", want, txt)
		}
	}
	if strings.Join(prop.Commands, ",") != "colorMatch (port 9999),multiColorPrint (port 9999)" {
		t.Errorf("commands = %v", prop.Commands)
	}
	if start, pause, resume, cancel := f.counts(); start != 0 || pause != 0 || resume != 0 || cancel != 0 {
		t.Fatalf("a Moonraker lifecycle call was sent by a proposal: start %d pause %d resume %d cancel %d", start, pause, resume, cancel)
	}
	// No 9999 write of any kind (colorMatch, multiColorPrint, opGcodeFile, stop,
	// modifyMaterial are all recorded) and no Moonraker template (M221 or other).
	if ev := f.wsEventList(); len(ev) != 0 || f.cfs.colorMatchCalls+f.cfs.startFrames+f.cfs.spoolStarts+f.cfs.stopCalls+f.cfs.modifyCalls != 0 {
		t.Fatalf("a proposal wrote to port 9999: %v", ev)
	}
	f.mu.Lock()
	templates := f.templateCalls
	f.mu.Unlock()
	if templates != 0 {
		t.Fatalf("a proposal sent %d Moonraker templates (M221 or other)", templates)
	}
}

func TestStartCFS_ExecuteSendsColorMatchThenVerifiedStart(t *testing.T) {
	f, p, printer := startSetup(t, "start-exec")
	wd := newFakeWatchdog()
	f.setWatchdog(wd)

	prop := propose(t, p, f, printer, startParams())
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.Effect != "sent" || !res.Accepted {
		t.Fatalf("effect %s accepted %v (%v)", res.Effect, res.Accepted, res.Effects)
	}
	if got := strings.Join(f.wsEventList(), ","); got != "colorMatch,multiColorPrint" {
		t.Fatalf("frames = %s", got)
	}
	if !strings.Contains(strings.Join(res.Effects, " "), "map verified and start sent; the printer runs its self-test for several minutes") {
		t.Fatalf("effects = %v", res.Effects)
	}
	if strings.Contains(strings.Join(res.Effects, " "), "started") && res.Effect == "started" {
		t.Fatal("a CFS start must never report started")
	}
	if f.cfs.lastStartPath != "/mnt/UDISK/printer_data/gcodes/three.gcode" || f.cfs.lastSelfTest {
		t.Fatalf("path %q selfTest %v", f.cfs.lastStartPath, f.cfs.lastSelfTest)
	}
	items := f.cfs.lastItems
	if len(items) != 3 || items[0].ID != "T1A" || items[0].BoxID != 1 || items[0].MaterialID != 0 || items[0].Type != "PLA" || items[0].Color != "#ff0000" ||
		items[2].ID != "T1C" || items[2].MaterialID != 3 || items[2].Color != "#ffffff" {
		t.Fatalf("colorMatch items = %+v", items)
	}
	if start, _, _, _ := f.counts(); start != 0 {
		t.Fatal("the Moonraker PrintStart was used for a CFS start")
	}
	if len(wd.disarmCalls) != 1 || wd.disarmCalls[0] != "start-exec" {
		t.Fatalf("watchdog disarm calls = %v, want one for the identity", wd.disarmCalls)
	}
	// The window is visible right away and the start record exists.
	if p.locks.get("start-exec").getStartRec() == nil {
		t.Fatal("no start record")
	}
	if res.After.ActivityState != "preparing" || res.After.Bucket != "PP" {
		t.Fatalf("after = %s/%s, want preparing/PP", res.After.ActivityState, res.After.Bucket)
	}
	// The token is single-use.
	_, err = exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	wantErr(t, err, CodeNotFound, "")
}

func TestStartCFS_SelfTestDefaultsToThePrintersOwnSetting(t *testing.T) {
	f, p, printer := startSetup(t, "start-selftest")
	f.cfs9999(func(c *fakeCFS) { c.enableSelfTest = 1 })
	prop := propose(t, p, f, printer, startParams())
	if !strings.Contains(strings.Join(prop.Effects, " "), "self-test before printing: true") {
		t.Fatalf("effects = %v", prop.Effects)
	}
	if _, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token); err != nil {
		t.Fatal(err)
	}
	if !f.cfs.lastSelfTest {
		t.Fatal("selfTest = false, want the printer's own 1")
	}

	// An explicit false wins over the printer's setting.
	f2, p2, printer2 := startSetup(t, "start-selftest-off")
	f2.cfs9999(func(c *fakeCFS) { c.enableSelfTest = 1 })
	params := Params{Filename: "three.gcode", SelfTest: false, SelfTestExplicit: true}
	prop = propose(t, p2, f2, printer2, params)
	if _, err := exec(p2, f2, printer2, ActionStartPrint, params, prop.Token); err != nil {
		t.Fatal(err)
	}
	if f2.cfs.lastSelfTest {
		t.Fatal("explicit self_test false was overridden")
	}
	// An absent enableSelfTest means false.
	f3, p3, printer3 := startSetup(t, "start-selftest-absent")
	f3.omit9999("enableSelfTest")
	prop = propose(t, p3, f3, printer3, startParams())
	if !strings.Contains(strings.Join(prop.Effects, " "), "self-test before printing: false") {
		t.Fatalf("effects = %v", prop.Effects)
	}
}

func TestStartCFS_TheTokenBindsTheSelfTestChoice(t *testing.T) {
	f, p, printer := startSetup(t, "start-bind-params")
	prop := propose(t, p, f, printer, startParams())
	_, err := exec(p, f, printer, ActionStartPrint, Params{Filename: "three.gcode", SelfTest: true, SelfTestExplicit: true}, prop.Token)
	perr := wantErr(t, err, CodeConflict, "")
	if !containsStr(perr.Changed, "params") {
		t.Fatalf("changed = %v, want params", perr.Changed)
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Execute recomputes the mapping and refuses when the mapping, the file
// identity or ANY slot definition (mapped or not) differs from the proposal.
func TestStartCFS_TokenBindsMappingFileAndEverySlot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fakePrinter)
	}{
		{"unmapped slot colour", func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.slotAt(1, 2).Color = "#0123456" }) }},
		{"mapped slot rfid", func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.slotAt(1, 0).RFID = "99004" }) }},
		{"slot state", func(f *fakePrinter) {
			f.cfs9999(func(c *fakeCFS) {
				two := 2
				c.slotAt(1, 2).State = &two
			})
		}},
		{"file size", func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.fileInfos[0].Size++ }) }},
		{"file create_time", func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.fileInfos[0].CreateTime++ }) }},
		{"file material list", func(f *fakePrinter) {
			f.cfs9999(func(c *fakeCFS) { c.fileInfos[0].MaterialColors = "#FF0000;#000000;#EEEEEE" })
		}},
		{"mapping changes", func(f *fakePrinter) {
			f.cfs9999(func(c *fakeCFS) {
				c.slotAt(1, 3).Color = "#0000000" // white slot becomes black: filament 2 maps elsewhere
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p, printer := startSetup(t, "start-bind-"+strings.ReplaceAll(tc.name, " ", "-"))
			prop := propose(t, p, f, printer, startParams())
			tc.mutate(f)
			f.cfs9999(func(c *fakeCFS) { c.regroup() })
			_, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
			if err == nil {
				t.Fatal("the start went through after the bound situation changed")
			}
			if len(f.wsEventList()) != 0 {
				t.Fatalf("frames were sent: %v", f.wsEventList())
			}
		})
	}
}

// A mismatch between the requested and the printer's map sends NO start frame.
func TestStartCFS_MapMismatchSendsNoStartFrame(t *testing.T) {
	f, p, printer := startSetup(t, "start-mismatch")
	f.setExtrudeFactor(80) // D7: the flow reset must be undone
	f.cfs9999(func(c *fakeCFS) { c.forcedMapping = map[string]string{"T1A": "T1D"} })
	prop := propose(t, p, f, printer, startParams())
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "refused_map_mismatch" || res.Accepted {
		t.Fatalf("effect %s accepted %v", res.Effect, res.Accepted)
	}
	if got := strings.Join(f.wsEventList(), ","); got != "colorMatch,verify_failed" {
		t.Fatalf("frames = %s, want colorMatch and NO multiColorPrint", got)
	}
	if f.cfs.startFrames != 0 {
		t.Fatal("a start frame was sent")
	}
	txt := strings.Join(res.Effects, " ")
	if !strings.Contains(txt, "NOT sent") || !strings.Contains(txt, "Requested map: T1A=T1A") || !strings.Contains(txt, "T1A=T1D") || !strings.Contains(txt, "may keep the written map") {
		t.Fatalf("effects = %v", res.Effects)
	}
	if p.locks.get("start-mismatch").getStartRec() != nil {
		t.Fatal("the start record must not survive a start that was never sent")
	}
	if res.StartPrintFlowRestored == nil || *res.StartPrintFlowRestored != 80 {
		t.Fatalf("flow restored = %v, want 80", res.StartPrintFlowRestored)
	}
}

// The printer canonicalises a request for T1C to the group head T1B: same
// material and colour, so the map verifies.
func TestStartCFS_CanonicalisationToTheGroupHeadIsAccepted(t *testing.T) {
	f, p, printer := startSetup(t, "start-canon")
	params := Params{Filename: "three.gcode", SlotMap: "T1B=T1C"}
	prop := propose(t, p, f, printer, params)
	row := prop.Mapping[1]
	if row.Slot != "T1C" || row.LikelyRunAs != "T1B" || strings.Join(row.SameGroup, ",") != "T1B,T1C" {
		t.Fatalf("row = %+v", row)
	}
	if len(row.Warnings) != 1 || !strings.Contains(row.Warnings[0], "the printer may use T1B instead of T1C: both must hold this spool") {
		t.Fatalf("warnings = %v", row.Warnings)
	}
	if !strings.Contains(strings.Join(prop.Effects, "\n"), "the printer may use T1B instead of T1C") {
		t.Fatalf("effects = %v", prop.Effects)
	}
	res, err := exec(p, f, printer, ActionStartPrint, params, prop.Token)
	if err != nil || res.Effect != "sent" {
		t.Fatalf("effect %s err %v (%v)", res.Effect, err, res.Effects)
	}
	if f.cfs.boxMap["T1B"] != "T1B" {
		t.Fatalf("printer map = %v, want the canonical head", f.cfs.boxMap)
	}
}

func TestStartCFS_ColourDistanceWarning(t *testing.T) {
	f, p, printer := startSetup(t, "start-distance")
	f.addCFSFile("magenta.gcode", "PETG", "#FF00FF")
	prop := propose(t, p, f, printer, Params{Filename: "magenta.gcode"})
	if prop.Mapping[0].Distance <= distanceWarnThreshold {
		t.Fatalf("distance = %v, want above %v", prop.Mapping[0].Distance, distanceWarnThreshold)
	}
	if !strings.Contains(strings.Join(prop.Mapping[0].Warnings, " "), "colour distance") {
		t.Fatalf("warnings = %v", prop.Mapping[0].Warnings)
	}
}

func TestStartCFS_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *fakePrinter)
		params Params
		code   Code
		msg    string
	}{
		{"unmatched type", func(f *fakePrinter) { f.addCFSFile("abs.gcode", "ABS", "#FF0000") }, Params{Filename: "abs.gcode"}, CodeUnavailable, "no slot holds that type"},
		{"unmatched after consumption", func(f *fakePrinter) { f.addCFSFile("four.gcode", "PLA;PLA", "#FF0000;#FF0000") }, Params{Filename: "four.gcode"}, CodeUnavailable, "already used by other filaments"},
		{"no filament list", func(f *fakePrinter) { f.addCFSFile("blank.gcode", "", "") }, Params{Filename: "blank.gcode"}, CodeUnavailable, "re-slice with Creality Print"},
		{"lengths differ", func(f *fakePrinter) { f.addCFSFile("skew.gcode", "PLA;PETG", "#FF0000") }, Params{Filename: "skew.gcode"}, CodeUnavailable, "do not line up"},
		{"no 9999 record", func(f *fakePrinter) {}, Params{Filename: "ghost.gcode"}, CodeNotFound, ""},
		{"file list unreadable", func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.fileInfoErr = errFakeUnreachable }) }, startParams(), CodeUnavailable, "file list could not be read"},
		{"boxsInfo unreadable", func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) { c.boxsInfoErr = errFakeUnreachable }) }, startParams(), CodeUnavailable, "slot definitions"},
		{"bad source", func(f *fakePrinter) {}, Params{Filename: "three.gcode", Source: "usb"}, CodeInvalidInput, "cfs or spool"},
		{"spool while unverified", func(f *fakePrinter) {}, Params{Filename: "three.gcode", Source: "spool"}, CodeUnavailable, "not yet verified"},
		{"slot map wrong type", func(f *fakePrinter) {}, Params{Filename: "three.gcode", SlotMap: "T1A=T1B"}, CodeInvalidInput, "material types must be equal"},
		{"slot map malformed", func(f *fakePrinter) {}, Params{Filename: "three.gcode", SlotMap: "T1A"}, CodeInvalidInput, "slot_map"},
		{"slot map slot reuse", func(f *fakePrinter) {}, Params{Filename: "three.gcode", SlotMap: "T1B=T1D,T1C=T1D"}, CodeInvalidInput, "slot_map"},
		{"slot map undefined slot", func(f *fakePrinter) {
			f.cfs9999(func(c *fakeCFS) { z := 0; c.slotAt(1, 3).State = &z })
		}, Params{Filename: "three.gcode", SlotMap: "T1C=T1D"}, CodeInvalidInput, "not a defined, unused CFS slot"},
		{"empty filename", func(f *fakePrinter) {}, Params{}, CodeInvalidInput, "filename"},
		{"missing on the printer", func(f *fakePrinter) { f.cfs9999(func(c *fakeCFS) {}) }, Params{Filename: "nothere.gcode"}, CodeNotFound, "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p, printer := startSetup(t, "start-ref-"+strings.ReplaceAll(tc.name, " ", "-"))
			tc.setup(f)
			_, err := exec(p, f, printer, ActionStartPrint, tc.params, "")
			wantErr(t, err, tc.code, tc.msg)
			if len(f.wsEventList()) != 0 {
				t.Fatalf("frames were sent: %v", f.wsEventList())
			}
		})
	}
}

func TestStartCFS_D7FlowResetOnlyWhenNotAlready100(t *testing.T) {
	f, p, printer := startSetup(t, "start-flow100")
	prop := propose(t, p, f, printer, startParams())
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil || strings.Contains(strings.Join(res.Commands, ","), "M221") {
		t.Fatalf("err %v commands %v: no M221 expected at 100", err, res.Commands)
	}

	f2, p2, printer2 := startSetup(t, "start-flow80")
	f2.setExtrudeFactor(80)
	prop = propose(t, p2, f2, printer2, startParams())
	res, err = exec(p2, f2, printer2, ActionStartPrint, startParams(), prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Commands[0] != "M221 S100" {
		t.Fatalf("commands = %v, want M221 S100 first", res.Commands)
	}
	f2.mu.Lock()
	got := f2.extrudeFactor
	f2.mu.Unlock()
	if got != 1.0 || res.StartPrintFlowRestored != nil {
		t.Fatalf("flow %v restored %v: after the start frame the flow stays at 100", got, res.StartPrintFlowRestored)
	}
}

func TestStartCFS_StartFrameNotSentRestoresFlowAndClearsTheRecord(t *testing.T) {
	f, p, printer := startSetup(t, "start-notsent")
	f.setExtrudeFactor(90)
	f.cfs9999(func(c *fakeCFS) { c.startNotSent = true })
	prop := propose(t, p, f, printer, startParams())
	res, err := exec(p, f, printer, ActionStartPrint, startParams(), prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "not_sent" || res.Accepted {
		t.Fatalf("effect %s accepted %v", res.Effect, res.Accepted)
	}
	if res.StartPrintFlowRestored == nil || *res.StartPrintFlowRestored != 90 {
		t.Fatalf("flow restored = %v", res.StartPrintFlowRestored)
	}
	if p.locks.get("start-notsent").getStartRec() != nil {
		t.Fatal("record kept for a start that was not sent")
	}
}

func TestStartCFS_SpoolSourceWhenVerified(t *testing.T) {
	old := spoolStartVerified
	spoolStartVerified = true
	t.Cleanup(func() { spoolStartVerified = old })

	f, p, printer := startSetup(t, "start-spool")
	f.addCFSFile("single.gcode", "PLA", "#FFFFFF")
	params := Params{Filename: "single.gcode", Source: "spool"}
	prop := propose(t, p, f, printer, params)
	if len(prop.Mapping) != 0 || strings.Join(prop.Commands, ",") != "opGcodeFile (port 9999)" {
		t.Fatalf("spool proposal = mapping %v commands %v", prop.Mapping, prop.Commands)
	}
	res, err := exec(p, f, printer, ActionStartPrint, params, prop.Token)
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "sent" || strings.Join(f.wsEventList(), ",") != "opGcodeFile" {
		t.Fatalf("effect %s frames %v (%v)", res.Effect, f.wsEventList(), res.Effects)
	}

	f.setSelfTest(100)
	p.locks.get("start-spool").setStartRec(nil)

	// A multi-filament file, or a material that differs from the side spool, is refused.
	_, err = exec(p, f, printer, ActionStartPrint, Params{Filename: "three.gcode", Source: "spool"}, "")
	wantErr(t, err, CodeInvalidInput, "single-filament")
	f.addCFSFile("petg.gcode", "PETG", "#FFFFFF")
	_, err = exec(p, f, printer, ActionStartPrint, Params{Filename: "petg.gcode", Source: "spool"}, "")
	wantErr(t, err, CodeUnavailable, "side spool must be defined and hold PETG")
}

func TestStartCFS_SlotsAreNeverStartedWithoutTheProposal(t *testing.T) {
	// Even with the gate hook skipped, a bare start is refused while a CFS is
	// connected: sendStartPrint is unreachable for a CFS printer.
	old := testHookSkipGate
	testHookSkipGate = func(ActionName) bool { return true }
	t.Cleanup(func() { testHookSkipGate = old })
	f, p, printer := startSetup(t, "start-bare")
	f.setDeviceState(1)
	// ConfirmationProposalToken means the first call is a proposal; a bare
	// PrintStart can only be reached with a bind of nil, which send refuses.
	res, err := p.send(t.Context(), f.deps(), printer, "start-bare", testSettings(), specs[ActionStartPrint], startParams(),
		snapshotOf(t, f, printer), derivedCFS(), &acquiredLocks{pl: p.locks.get("start-bare")}, nil)
	_ = res
	wantErr(t, err, CodeUnavailable, "bare start is refused")
	if start, _, _, _ := f.counts(); start != 0 {
		t.Fatal("PrintStart was sent")
	}
}

// --- matching by full path (8a.10) ---

func TestMatchGcodeFile_FullPathIncludingSubdirectories(t *testing.T) {
	root := "/mnt/UDISK/printer_data/gcodes/"
	files := []crealityws.GcodeFileInfo{
		{Name: "a.gcode", Path: root + "a.gcode"},
		{Name: "a.gcode", Path: root + "sub/a.gcode"},
		{Name: "b.gcode", Path: root + "sub/deeper/b.gcode"},
		{Name: "c.gcode", Path: "/elsewhere/c.gcode"},
	}
	got, err := matchGcodeFile(files, "a.gcode")
	if err != nil || got.Path != root+"a.gcode" {
		t.Fatalf("root a.gcode: %+v %v (the subdirectory copy must not match)", got, err)
	}
	if got, err := matchGcodeFile(files, "sub/a.gcode"); err != nil || got.Path != root+"sub/a.gcode" {
		t.Fatalf("sub/a.gcode: %+v %v", got, err)
	}
	if got, err := matchGcodeFile(files, "sub/deeper/b.gcode"); err != nil || got.Path != root+"sub/deeper/b.gcode" {
		t.Fatalf("deeper: %+v %v", got, err)
	}
	if _, err := matchGcodeFile(files, "b.gcode"); err == nil {
		t.Fatal("b.gcode lives in a subdirectory: a bare name must not match")
	}
	if _, err := matchGcodeFile(files, "c.gcode"); err == nil {
		t.Fatal("a path outside a gcodes root must not match")
	}
	dup := append(files, crealityws.GcodeFileInfo{Name: "a.gcode", Path: "/other/gcodes/a.gcode"})
	if _, err := matchGcodeFile(dup, "a.gcode"); err == nil || err.Code != CodeConflict {
		t.Fatalf("two matches: %v, want a conflict", err)
	}
}

func TestSplitListAndPool(t *testing.T) {
	if got := splitList(" PLA ; PETG ;;"); len(got) != 2 || got[0] != "PLA" || got[1] != "PETG" {
		t.Fatalf("splitList = %q", got)
	}
	f := newFakeCFS()
	pool := slotPool(f.boxs)
	if len(pool) != 4 || pool[0].Label != "T1A" || pool[0].Color != "#ff0000" {
		t.Fatalf("pool = %+v (the side spool must not be in the CFS pool)", pool)
	}
	zero := 0
	f.slotAt(1, 1).State = &zero
	f.slotAt(1, 2).Color = ""
	if got := slotPool(f.boxs); len(got) != 2 {
		t.Fatalf("pool with an undefined and a colourless slot = %d, want 2", len(got))
	}
}

func TestCheckBoxMap(t *testing.T) {
	f := newFakePrinter()
	f.setCFSConnected(true)
	box := boxFromFake(t, f)
	req := map[string]string{"T1A": "T1A", "T1B": "T1C"}
	if err := checkBoxMap(box, req); err != nil {
		t.Fatalf("identity map with equal C/B values: %v", err)
	}
	if err := checkBoxMap(nil, req); err == nil || !strings.Contains(err.reason, "could not be read") {
		t.Fatalf("nil box: %v", err)
	}
	box.Map["T1B"] = "T1D"
	if err := checkBoxMap(box, req); err == nil || !strings.Contains(err.reason, "was requested") {
		t.Fatalf("wrong slot: %v", err)
	}
	delete(box.Map, "T1B")
	if err := checkBoxMap(box, req); err == nil || !strings.Contains(err.reason, "no entry") {
		t.Fatalf("missing key: %v", err)
	}
	box = boxFromFake(t, f)
	delete(box.Units, "T1")
	if err := checkBoxMap(box, req); err == nil || !strings.Contains(err.reason, "no Moonraker material data") {
		t.Fatalf("missing unit: %v", err)
	}
	box = boxFromFake(t, f)
	box.Units["T1"].MaterialType[0] = ""
	if err := checkBoxMap(box, req); err == nil {
		t.Fatal("an empty material_type must fail")
	}
	box = boxFromFake(t, f)
	box.Map = nil
	if err := checkBoxMap(box, req); err == nil || !strings.Contains(err.reason, "absent") {
		t.Fatalf("nil map: %v", err)
	}
}

func snapshotOf(t *testing.T, f *fakePrinter, printer domain.Printer) printerstate.Snapshot {
	t.Helper()
	return printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
}

func derivedCFS() printerstate.Derived {
	return printerstate.Derived{State: printerstate.StateIdle, Bucket: printerstate.BucketI, CFSConnected: true, CFSKnown: true, CFSQuiescent: true}
}

// boxFromFake decodes the Moonraker box object the fake reports.
func boxFromFake(t *testing.T, f *fakePrinter) *moonraker.Box {
	t.Helper()
	raw, err := f.QueryObjects(context.Background(), map[string][]string{"box": nil})
	if err != nil {
		t.Fatal(err)
	}
	box, err := moonraker.DecodeBox(raw["box"])
	if err != nil {
		t.Fatal(err)
	}
	return &box
}

// A warning names the head only when it differs from the mapped slot; when the
// mapped slot IS the head of a shared group, the warning says who shares it.
func TestStartCFS_RefillGroupWarningsNameTheRightSlots(t *testing.T) {
	f, p, printer := startSetup(t, "warn-head")
	prop := propose(t, p, f, printer, startParams())
	row := prop.Mapping[1] // T1B is the head of the T1B+T1C group
	if row.Slot != "T1B" || row.LikelyRunAs != "T1B" || len(row.Warnings) != 1 {
		t.Fatalf("row = %+v", row)
	}
	w := row.Warnings[0]
	if strings.Contains(w, "may use T1B instead of T1B") || !strings.Contains(w, "T1B shares an auto-refill group with T1C: if T1B runs out the printer may continue from them, so they must hold the same spool") {
		t.Fatalf("warning = %q", w)
	}
	if len(prop.Mapping[0].Warnings) != 0 {
		t.Fatalf("a slot alone in its group needs no warning: %v", prop.Mapping[0].Warnings)
	}
	// A slot that is NOT the head still gets the "instead of" warning.
	prop2 := propose(t, p, f, printer, Params{Filename: "three.gcode", SlotMap: "T1B=T1C"})
	if !strings.Contains(prop2.Mapping[1].Warnings[0], "the printer may use T1B instead of T1C") {
		t.Fatalf("warning = %v", prop2.Mapping[1].Warnings)
	}
}

func TestDisplayName(t *testing.T) {
	for _, c := range [][3]string{{"Generic", "Generic PETG", "Generic PETG"}, {"generic", "Generic PLA", "Generic PLA"}, {"Acme", "Test PLA", "Acme Test PLA"}, {"", "Solo", "Solo"}} {
		if got := DisplayName(c[0], c[1]); got != c[2] {
			t.Errorf("DisplayName(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
