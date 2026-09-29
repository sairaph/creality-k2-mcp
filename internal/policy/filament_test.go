package policy

import (
	"strings"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// set_filament_definition (plan 3.3): every refusal, no_change, the re-gate
// before the write, and the confirmed / unconfirmed read-back. Fixtures are
// the invented catalog of fakecfs_test.go (V1).

func filamentSetup(t *testing.T, host string) (*fakePrinter, *Policy, domain.Printer) {
	t.Helper()
	fastTimers(t)
	return cfsSetup(t, host)
}

func editParams(slot, material, color string) Params {
	return Params{Slot: slot, Material: material, Color: color}
}

func TestSetFilament_HappyPathConfirmedOnBothChannels(t *testing.T) {
	f, p, printer := filamentSetup(t, "fil-happy")
	res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("t1a", "acme test petg", "000000"), "")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if res.Effect != "confirmed" || !res.Accepted {
		t.Fatalf("effect %s accepted %v (%v)", res.Effect, res.Accepted, res.Effects)
	}
	fc := res.Filament
	if fc == nil || fc.Slot != "T1A" {
		t.Fatalf("filament change = %+v", fc)
	}
	if fc.Before.RFID != "99001" || fc.Before.Color != "#ff0000" || fc.After.RFID != "99002" || fc.After.Color != "#000000" || fc.After.Name != "Acme Test PETG" {
		t.Fatalf("before %+v after %+v", fc.Before, fc.After)
	}
	if fc.MoonrakerMaterialType != "099002" || !strings.EqualFold(fc.MoonrakerColor, "0000000") {
		t.Fatalf("moonraker read-back = %q %q", fc.MoonrakerMaterialType, fc.MoonrakerColor)
	}
	// The edit joined the black PETG group: T1A+T1B+T1C after, T1B+T1C before.
	if strings.Contains(strings.Join(fc.SameMaterialBefore, "|")+strings.Join(fc.SameMaterialAfter, "|"), " 0") || !strings.Contains(strings.Join(fc.SameMaterialAfter, "|"), "PETG #000000") {
		t.Fatalf("refill group colours must render as #rrggbb: %v %v", fc.SameMaterialBefore, fc.SameMaterialAfter)
	}
	if !strings.Contains(strings.Join(fc.SameMaterialBefore, "|"), "T1B+T1C") || !strings.Contains(strings.Join(fc.SameMaterialAfter, "|"), "T1A+T1B+T1C") {
		t.Fatalf("same_material before %v after %v", fc.SameMaterialBefore, fc.SameMaterialAfter)
	}
	e := f.cfs.lastEdit
	if e.BoxID != 1 || e.SlotID != 0 || e.BoxType != 0 || e.Entry.ID != "99002" || e.Color != "#000000" {
		t.Fatalf("edit sent = %+v", e)
	}
	if res.Before.ActivityState == "" || res.After.ActivityState == "" || res.Printer.ID == "" {
		t.Fatal("state blocks missing")
	}
}

func TestSetFilament_ResolvesByIdAndByName(t *testing.T) {
	f, p, printer := filamentSetup(t, "fil-resolve")
	if res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1D", "99001", "#00ff00"), ""); err != nil || res.Effect != "confirmed" {
		t.Fatalf("by id: %v %v", err, res.Effect)
	}
	if res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1D", "ACME TEST PLA", "#0000ff"), ""); err != nil || res.Effect != "confirmed" {
		t.Fatalf("by name: %v %v", err, res.Effect)
	}
}

func TestSetFilament_NoChangeSendsNothing(t *testing.T) {
	f, p, printer := filamentSetup(t, "fil-nochange")
	res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99001", "#FF0000"), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "no_change" || res.Accepted || f.cfs.modifyCalls != 0 {
		t.Fatalf("effect %s accepted %v modifyCalls %d", res.Effect, res.Accepted, f.cfs.modifyCalls)
	}
	if res.Filament == nil || res.Filament.Before != res.Filament.After {
		t.Fatalf("filament = %+v", res.Filament)
	}
}

func TestSetFilament_ParameterRefusals(t *testing.T) {
	cases := []struct {
		name   string
		params Params
		code   Code
		msg    string
	}{
		{"bad slot", editParams("T5A", "99001", "#ff0000"), CodeInvalidInput, "T1A..T4D or side_spool"},
		{"empty slot", editParams("", "99001", "#ff0000"), CodeInvalidInput, "side_spool"},
		{"bad colour", editParams("T1A", "99001", "red"), CodeInvalidInput, "#rrggbb"},
		{"colour in printer form", editParams("T1A", "99001", "#0ff0000"), CodeInvalidInput, "#rrggbb"},
		{"empty material", editParams("T1A", "", "#ff0000"), CodeInvalidInput, "material"},
		{"unknown material", editParams("T1A", "nope", "#ff0000"), CodeNotFound, "list_filament_catalog"},
		{"6-char moonraker code", editParams("T1A", "099001", "#ff0000"), CodeInvalidInput, "drop the leading 0"},
		{"ambiguous name", editParams("T1A", "Acme Dup", "#ff0000"), CodeInvalidInput, "99008"},
		{"0-0 range", editParams("T1A", "99005", "#ff0000"), CodeInvalidInput, "Acme Zero"},
		{"no pressure advance", editParams("T1A", "99006", "#ff0000"), CodeInvalidInput, "pressure advance"},
		{"pressure advance above 1", editParams("T1A", "99010", "#ff0000"), CodeInvalidInput, "outside 0-1"},
		{"above the nozzle ceiling", editParams("T1A", "99007", "#ff0000"), CodeInvalidInput, "ceiling"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p, printer := filamentSetup(t, "fil-param-"+strings.ReplaceAll(tc.name, " ", "-"))
			_, err := exec(p, f, printer, ActionSetFilamentDefinition, tc.params, "")
			wantErr(t, err, tc.code, tc.msg)
			if f.cfs.modifyCalls != 0 {
				t.Fatal("a write was sent")
			}
		})
	}
}

func TestSetFilament_LiveNozzleCapRefusesAHotterEntry(t *testing.T) {
	f, p, printer := filamentSetup(t, "fil-livecap")
	f.cfs9999(func(c *fakeCFS) {
		c.catalog = append(c.catalog, crealityws.CatalogEntry{ID: "99011", Brand: "Acme", Name: "Acme Warm", Type: "PLA", MinTemp: 250, MaxTemp: 310, PressureAdvance: ptr(0.04)})
	})
	_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99011", "#ff0000"), "")
	wantErr(t, err, CodeInvalidInput, "nozzle limit of 300")

	f.setOmitProductParam(true)
	_, err = exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#ff0000"), "")
	wantErr(t, err, CodeUnavailable, "live nozzle temperature cap")
}

func ptr(v float64) *float64 { return &v }

func TestSetFilament_GateRefusals(t *testing.T) {
	t.Run("printing", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-gate-print")
		f.setPrinting("model.gcode")
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeUnavailable, "not available in state")
	})
	t.Run("9999 unreachable (catalog)", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-gate-ws")
		f.setWS9999Unreachable(true)
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeUnavailable, "catalog could not be read")
	})
	t.Run("boxsInfo unreadable", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-gate-box")
		f.cfs9999(func(c *fakeCFS) { c.boxsInfoErr = errFakeUnreachable })
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeUnavailable, "could not be read")
		if f.cfs.modifyCalls != 0 {
			t.Fatal("a write was sent")
		}
	})
	t.Run("allow_control off", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-gate-ctl")
		printer.AllowControl = false
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeForbidden, "")
	})
}

func TestSetFilament_SlotRefusalsEachWithItsOwnMessage(t *testing.T) {
	zero, two, sel := 0, 2, 1
	cases := []struct {
		name   string
		mutate func(c *fakeCFS)
		slot   string
		msg    string
	}{
		{"slot absent", func(c *fakeCFS) { c.boxs.MaterialBoxs[1].Materials = c.boxs.MaterialBoxs[1].Materials[:2] }, "T1D", "does not report slot T1D"},
		{"unit absent", func(c *fakeCFS) {}, "T2A", "unit 2 is not connected"},
		{"state nil", func(c *fakeCFS) { c.slotAt(1, 0).State = nil }, "T1A", "no definition or is not ready"},
		{"state 0", func(c *fakeCFS) { c.slotAt(1, 0).State = &zero }, "T1A", "no definition or is not ready"},
		{"state 2 rfid", func(c *fakeCFS) { c.slotAt(1, 0).State = &two }, "T1A", "RFID-managed"},
		{"editStatus 0", func(c *fakeCFS) { c.slotAt(1, 0).EditStatus = &zero }, "T1A", "being written"},
		{"editStatus nil", func(c *fakeCFS) { c.slotAt(1, 0).EditStatus = nil }, "T1A", "being written"},
		{"selected", func(c *fakeCFS) { c.slotAt(1, 0).Selected = &sel }, "T1A", "selected at the filament hub"},
		{"unit not ready", func(c *fakeCFS) { c.boxs.MaterialBoxs[1].State = 0 }, "T1A", "not connected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, p, printer := filamentSetup(t, "fil-slot-"+strings.ReplaceAll(tc.name, " ", "-"))
			f.cfs9999(tc.mutate)
			_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams(tc.slot, "99002", "#00ff00"), "")
			wantErr(t, err, CodeUnavailable, tc.msg)
			if f.cfs.modifyCalls != 0 {
				t.Fatal("a write was sent")
			}
		})
	}
}

// Re-read and re-gate immediately before the write: any change between the
// first read and the write refuses without sending.
func TestSetFilament_RegatesBeforeTheWrite(t *testing.T) {
	t.Run("another slot changes between the reads", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-regate-slot")
		f.cfs9999(func(c *fakeCFS) {
			c.boxsInfoHook = func(call int) {
				if call == 2 {
					f.mu.Lock()
					c.slotAt(1, 3).Color = "#0123456"
					f.mu.Unlock()
				}
			}
		})
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeConflict, "slot definitions changed")
		if f.cfs.modifyCalls != 0 {
			t.Fatal("a write was sent")
		}
	})
	t.Run("a print starts between the reads", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-regate-print")
		f.cfs9999(func(c *fakeCFS) {
			c.boxsInfoHook = func(call int) {
				if call == 2 {
					f.setPrinting("model.gcode")
				}
			}
		})
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeUnavailable, "")
		if f.cfs.modifyCalls != 0 {
			t.Fatal("a write was sent")
		}
	})
	t.Run("the CFS becomes busy between the reads", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-regate-cfs")
		f.cfs9999(func(c *fakeCFS) {
			c.boxsInfoHook = func(call int) {
				if call == 2 {
					f.setDeviceState(1)
				}
			}
		})
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeUnavailable, "cfsQuiescent")
		if f.cfs.modifyCalls != 0 {
			t.Fatal("a write was sent")
		}
	})
}

func TestSetFilament_UnconfirmedReportsEachChannel(t *testing.T) {
	t.Run("moonraker never reflects the edit", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-unc-moon")
		f.setMoonLag(true)
		res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Effect != "unconfirmed" || !res.Accepted {
			t.Fatalf("effect %s accepted %v", res.Effect, res.Accepted)
		}
		txt := strings.Join(res.Effects, " ")
		if !strings.Contains(txt, "port 9999 confirmed=true") || !strings.Contains(txt, "Moonraker confirmed=false") {
			t.Fatalf("effects = %v, want each channel reported", res.Effects)
		}
		if res.Filament.MoonrakerMaterialType != "099001" {
			t.Fatalf("moonraker value = %q, want the stale 099001", res.Filament.MoonrakerMaterialType)
		}
	})
	t.Run("the printer ignores the frame", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-unc-9999")
		f.cfs9999(func(c *fakeCFS) { c.modifyIgnored = true })
		res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Effect != "unconfirmed" || !strings.Contains(strings.Join(res.Effects, " "), "port 9999 confirmed=false") {
			t.Fatalf("effect %s effects %v", res.Effect, res.Effects)
		}
	})
	t.Run("no confirming frame but a fresh read confirms", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-unc-noframe")
		f.cfs9999(func(c *fakeCFS) { c.modifyNoFrame = true })
		res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		if err != nil || res.Effect != "confirmed" {
			t.Fatalf("effect %v err %v", res.Effect, err)
		}
	})
	t.Run("the frame could not be sent", func(t *testing.T) {
		f, p, printer := filamentSetup(t, "fil-notsent")
		f.cfs9999(func(c *fakeCFS) { c.modifyNotSent = true })
		_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("T1A", "99002", "#00ff00"), "")
		wantErr(t, err, CodeUnavailable, "was not sent")
	})
}

func TestSetFilament_SideSpoolEditWorksWhenVerified(t *testing.T) {
	if !sideSpoolEditVerified {
		t.Fatal("the side spool edit was verified live (2026-09-29) and must be on")
	}

	f, p, printer := filamentSetup(t, "fil-side")
	res, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("side_spool", "99001", "#112233"), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "confirmed" {
		t.Fatalf("effect %s (%v)", res.Effect, res.Effects)
	}
	if e := f.cfs.lastEdit; e.BoxID != 0 || e.SlotID != 0 || e.BoxType != 1 {
		t.Fatalf("edit = %+v, want box 0 slot 0 type 1", e)
	}
	if res.Filament.Slot != "side_spool" || res.Filament.MoonrakerMaterialType != "099001" {
		t.Fatalf("filament = %+v", res.Filament)
	}
}

func TestParseFilamentTarget(t *testing.T) {
	for in, want := range map[string]filamentTarget{
		"T1A": {"T1A", 1, 0, 0}, "t2d": {"T2D", 2, 3, 0}, " SIDE_SPOOL ": {"side_spool", 0, 0, 1},
	} {
		got, err := parseFilamentTarget(in)
		if err != nil || got != want {
			t.Errorf("parseFilamentTarget(%q) = %+v %v, want %+v", in, got, err, want)
		}
	}
}

// With the switch off (a release that has not verified it) the side spool is
// refused with a clear reason and nothing is sent.
func TestSetFilament_SideSpoolRefusedWhenTheSwitchIsOff(t *testing.T) {
	old := sideSpoolEditVerified
	sideSpoolEditVerified = false
	t.Cleanup(func() { sideSpoolEditVerified = old })
	f, p, printer := filamentSetup(t, "fil-side-off")
	_, err := exec(p, f, printer, ActionSetFilamentDefinition, editParams("side_spool", "99003", "#ffffff"), "")
	wantErr(t, err, CodeUnavailable, "not yet verified")
	if f.cfs.modifyCalls != 0 {
		t.Fatal("a write was sent")
	}
}
