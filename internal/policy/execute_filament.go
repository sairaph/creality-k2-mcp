package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// set_filament_definition (dev_docs/plan-v0.2.0.md section 3.3): rewrite one
// slot's filament definition with Creality's own modifyMaterial message. The
// flow is deliberately long because the write is persistent and the printer
// gives no error reply for a bad one: catalog resolution before the lock,
// then under the lock a fresh snapshot and gate, a fresh boxsInfo with every
// per-slot refusal, a re-read and re-gate immediately before the write, and a
// two-channel read-back (the confirming 9999 frame AND Moonraker).

// sideSpoolEditVerified gates editing the side spool (boxId 0, boxType 1). The
// message shape is the same as for a CFS slot; it was verified live in the
// supervised session (2026-09-29: edit and restore, confirmed on both
// channels; the side spool is never in a same_material group, which
// crealityws.MaterialEdit.Confirms accounts for). A variable only so tests can
// exercise both settings; nothing in production assigns it.
var sideSpoolEditVerified = true

// moonrakerReadbackTimeout and moonrakerReadbackInterval bound the Moonraker
// half of the read-back (plan 3.3 step 6: poll up to 3 s). Variables so the
// unconfirmed-outcome test does not wait real seconds.
var (
	moonrakerReadbackTimeout  = 3 * time.Second
	moonrakerReadbackInterval = 250 * time.Millisecond
)

// filamentTarget is a validated slot address.
type filamentTarget struct {
	label   string // "T1A" or "side_spool"
	boxID   int
	slotID  int
	boxType int // 0 CFS unit, 1 side spool holder
}

// normHex reduces "#rrggbb", "rrggbb" and the printer's "#0rrggbb" to
// lower-case "rrggbb", or "" for anything else.
func normHex(s string) string {
	s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if len(s) == 7 && s[0] == '0' {
		s = s[1:]
	}
	if len(s) != 6 {
		return ""
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return s
}

// parseRGB accepts exactly "#rrggbb" or "rrggbb" and returns lower-case
// "rrggbb", or "" (the printer's 7-digit form is not a user input).
func parseRGB(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return ""
	}
	return normHex(s)
}

func parseFilamentTarget(slot string) (filamentTarget, *Error) {
	s := strings.ToLower(strings.TrimSpace(slot))
	if s == "side_spool" {
		return filamentTarget{label: "side_spool", boxID: 0, slotID: 0, boxType: 1}, nil
	}
	box, mat, ok := parseSlotLabel(s)
	if !ok {
		return filamentTarget{}, &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("slot %q is not T1A..T4D or side_spool", slot)}
	}
	return filamentTarget{label: slotLabel(box, mat), boxID: box, slotID: mat, boxType: 0}, nil
}

// resolveCatalogEntry finds the one catalog entry material names: a 5
// character id or an exact (case-insensitive) name. A 6-character Moonraker
// code ("0" + id) is rejected with a hint.
func resolveCatalogEntry(catalog []crealityws.CatalogEntry, material string) (crealityws.CatalogEntry, *Error) {
	material = strings.TrimSpace(material)
	if material == "" {
		return crealityws.CatalogEntry{}, &Error{Code: CodeInvalidInput, Message: "material must not be empty: give a catalog id (5 characters) or an exact catalog name; list_filament_catalog shows them"}
	}
	var matches []crealityws.CatalogEntry
	for _, e := range catalog {
		if e.ID == material || strings.EqualFold(e.Name, material) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(material) == 6 && material[0] == '0' {
			return crealityws.CatalogEntry{}, &Error{Code: CodeInvalidInput, Message: fmt.Sprintf(
				"%q looks like a 6-character Moonraker material code; use the 5-character catalog id (drop the leading 0): %q", material, material[1:])}
		}
		return crealityws.CatalogEntry{}, &Error{Code: CodeNotFound, Message: fmt.Sprintf("no catalog entry has the id or exact name %q; list_filament_catalog shows the printer's own catalog", material)}
	default:
		ids := make([]string, len(matches))
		for i, m := range matches {
			ids[i] = m.ID
		}
		return crealityws.CatalogEntry{}, &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("%q matches %d catalog entries (%s); use the 5-character catalog id", material, len(matches), strings.Join(ids, ", "))}
	}
}

// checkCatalogEntryWritable applies the entry rules that do not need the
// printer: 0 < min <= max <= the code ceiling, and 0 <= pressure advance <= 1.
// An entry with a 0-0 range is refused by name (it is a placeholder, and
// writing it would give the slot no usable temperature range).
func checkCatalogEntryWritable(e crealityws.CatalogEntry) *Error {
	if strings.TrimSpace(e.Type) == "" || strings.TrimSpace(e.Name) == "" {
		return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("catalog entry %s has no material type or no name, so it cannot be written to a slot", e.ID)}
	}
	if e.MinTemp <= 0 || e.MaxTemp <= 0 {
		return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf(
			"catalog entry %s (%s) has no nozzle temperature range (%g-%g) and cannot be written to a slot", e.ID, e.Name, e.MinTemp, e.MaxTemp)}
	}
	if e.MinTemp > e.MaxTemp {
		return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("catalog entry %s (%s) has an inverted temperature range %g-%g", e.ID, e.Name, e.MinTemp, e.MaxTemp)}
	}
	if e.MaxTemp > domain.NozzleCeilingC {
		return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("catalog entry %s (%s) has a maximum of %g C, above this server's nozzle ceiling of %g C", e.ID, e.Name, e.MaxTemp, domain.NozzleCeilingC)}
	}
	if e.PressureAdvance == nil {
		return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("catalog entry %s (%s) has no pressure advance value", e.ID, e.Name)}
	}
	if *e.PressureAdvance < 0 || *e.PressureAdvance > 1.0 {
		return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("catalog entry %s (%s) has a pressure advance of %g, outside 0-1", e.ID, e.Name, *e.PressureAdvance)}
	}
	return nil
}

// findSlot locates the slot t addresses in b and returns it with the unit's
// state. ok is false when the unit or slot is absent.
func findSlot(b crealityws.BoxsInfo, t filamentTarget) (box crealityws.MaterialBox, slot crealityws.SlotMaterial, ok bool) {
	for _, bx := range b.MaterialBoxs {
		if bx.ID != t.boxID || bx.Type != t.boxType {
			continue
		}
		for _, m := range bx.Materials {
			if m.ID == t.slotID {
				return bx, m, true
			}
		}
		return bx, crealityws.SlotMaterial{}, false
	}
	return crealityws.MaterialBox{}, crealityws.SlotMaterial{}, false
}

// checkEditableSlot applies the per-slot refusals of plan 3.3 step 3, each
// with its own message. The same checks back get_filaments's editable and
// why_not fields, so this returns the plain reason (empty when editable).
func checkEditableSlot(t filamentTarget, box crealityws.MaterialBox, slot crealityws.SlotMaterial, found bool) string {
	if t.boxType == 0 && (box.ID != t.boxID || box.State != 1) {
		return fmt.Sprintf("the CFS unit %d is not connected", t.boxID)
	}
	if !found {
		return fmt.Sprintf("the printer does not report slot %s", t.label)
	}
	if slot.State == nil || *slot.State == 0 {
		return fmt.Sprintf("slot %s has no definition or is not ready (state %s)", t.label, optIntPtr(slot.State))
	}
	if *slot.State == 2 {
		return fmt.Sprintf("slot %s holds an RFID-managed spool: change it on the printer", t.label)
	}
	if *slot.State != 1 {
		return fmt.Sprintf("slot %s reports an unknown state %d", t.label, *slot.State)
	}
	if slot.EditStatus == nil || *slot.EditStatus != 1 {
		return fmt.Sprintf("slot %s is being written or is not editable right now (editStatus %s)", t.label, optIntPtr(slot.EditStatus))
	}
	if slot.Selected != nil && *slot.Selected == 1 {
		return fmt.Sprintf("slot %s is currently selected at the filament hub: unload it on the printer first", t.label)
	}
	return ""
}

func optIntPtr(p *int) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprint(*p)
}

func slotDefinition(m crealityws.SlotMaterial) SlotDefinition {
	d := SlotDefinition{RFID: m.RFID, Vendor: m.Vendor, Type: m.Type, Name: m.Name}
	if c := normHex(m.Color); c != "" {
		d.Color = "#" + c
	}
	if m.State != nil {
		d.State = *m.State
	}
	return d
}

// sameGroupStrings renders the printer's same_material groups for the
// Result ("T1A+T1B (PETG #000000)"), or nil when they are unknown.
func sameGroupStrings(b crealityws.BoxsInfo) []string {
	if !b.SameMaterialOK {
		return nil
	}
	out := make([]string, 0, len(b.SameMaterial))
	for _, g := range b.SameMaterial {
		names := make([]string, len(g.Slots))
		for i, s := range g.Slots {
			names[i] = slotLabel(s.BoxID, s.MaterialID)
		}
		color := g.Color
		if c := normHex(g.Color); c != "" {
			color = "#" + c
		}
		out = append(out, fmt.Sprintf("%s (%s %s)", strings.Join(names, "+"), g.Name, color))
	}
	return out
}

// moonrakerSlotValues reads the Moonraker material_type and color_value that
// correspond to target from a box / filament_rack query.
func moonrakerSlotValues(box *moonraker.Box, rack *moonraker.FilamentRack, t filamentTarget) (materialType, colorValue string, ok bool) {
	if t.boxType == 1 {
		if rack == nil || rack.MaterialType == nil || rack.ColorValue == nil {
			return "", "", false
		}
		return *rack.MaterialType, *rack.ColorValue, true
	}
	if box == nil {
		return "", "", false
	}
	u, has := box.Units[fmt.Sprintf("T%d", t.boxID)]
	if !has || t.slotID >= len(u.MaterialType) || t.slotID >= len(u.ColorValue) {
		return "", "", false
	}
	return u.MaterialType[t.slotID], u.ColorValue[t.slotID], true
}

// queryBoxAndRack is the light Moonraker read the read-backs and the map
// verification use (review-2 MF7: box only, not a full snapshot).
func queryBoxAndRack(ctx context.Context, deps Deps) (*moonraker.Box, *moonraker.FilamentRack) {
	raw, err := deps.Moonraker.QueryObjects(ctx, map[string][]string{"box": nil, "filament_rack": nil})
	if err != nil {
		return nil, nil
	}
	var box *moonraker.Box
	var rack *moonraker.FilamentRack
	if r, ok := raw["box"]; ok {
		if v, err := moonraker.DecodeBox(r); err == nil {
			box = &v
		}
	}
	if r, ok := raw["filament_rack"]; ok {
		if v, err := moonraker.DecodeFilamentRack(r); err == nil {
			rack = &v
		}
	}
	return box, rack
}

func (p *Policy) executeSetFilament(ctx context.Context, deps Deps, printer domain.Printer, spec actionSpec, params Params, identity string) (Result, error) {
	fail := func(e *Error) (Result, error) {
		e.Action = spec.Name
		return Result{}, e
	}

	// Cheap parameter validation and the side spool switch, before
	// anything touches the printer.
	target, terr := parseFilamentTarget(params.Slot)
	if terr != nil {
		return fail(terr)
	}
	if target.boxType == 1 && !sideSpoolEditVerified {
		return fail(&Error{Code: CodeUnavailable, Message: "editing the side spool is not yet verified on this firmware; edit it on the printer"})
	}
	rgb := parseRGB(params.Color)
	if rgb == "" {
		return fail(&Error{Code: CodeInvalidInput, Message: fmt.Sprintf("colour %q is not #rrggbb or rrggbb", params.Color)})
	}

	// Step 1, before the lock: the catalog read is slow (about 173 KB) and
	// must not extend the lock hold (plan 8a.9).
	catalog, err := deps.WS9999.Materials(ctx)
	if err != nil {
		return fail(&Error{Code: CodeUnavailable, Message: "the printer's filament catalog could not be read over port 9999: " + err.Error()})
	}
	entry, rerr := resolveCatalogEntry(catalog, params.Material)
	if rerr != nil {
		return fail(rerr)
	}
	if werr := checkCatalogEntryWritable(entry); werr != nil {
		return fail(werr)
	}

	// Step 2: the lock, the pending marker and a fresh snapshot and gate.
	locks, lockErr := acquireLocks(ctx, p.locks, identity)
	if lockErr != nil {
		return fail(lockErr)
	}
	defer locks.release()
	locks.pl.setPending(&printerstate.PendingAction{Kind: printerstate.PendingCFSOperation, IssuedAt: time.Now(), Timeout: 60 * time.Second})
	defer locks.pl.setPending(nil)

	snap := printerstate.Take(ctx, deps.stateDeps(), printer)
	derived := deriveFor(locks.pl, snap, nil)
	if !skipGate(spec.Name) {
		if gerr := checkGate(spec, derived); gerr != nil {
			return fail(gerr)
		}
	}
	// The live nozzle cap joins the domain ceiling (plan 3.3 step 1).
	var liveCap domain.LiveCap
	if snap.ProductParam != nil {
		liveCap = liveTempCap(snap.ProductParam.NozzleTemp)
	}
	limit, lerr := domain.EffectiveTemperatureLimit(domain.NozzleCeilingC, liveCap, nil)
	if lerr != nil {
		return fail(&Error{Code: CodeUnavailable, Message: "live nozzle temperature cap (product_param) has not been read; refusing to write a temperature range"})
	}
	if entry.MaxTemp > limit {
		return fail(&Error{Code: CodeInvalidInput, Message: fmt.Sprintf("catalog entry %s (%s) has a maximum of %g C, above this printer's nozzle limit of %g C", entry.ID, entry.Name, entry.MaxTemp, limit)})
	}

	// Step 3: fresh boxsInfo and the per-slot refusals.
	boxs, berr := deps.WS9999.BoxsInfo(ctx)
	if berr != nil {
		return fail(&Error{Code: CodeUnavailable, Message: "the slot definitions could not be read over port 9999 (unreachable?): " + berr.Error() + "; refusing to edit without reading the slot first"})
	}
	box, slot, found := findSlot(boxs, target)
	if why := checkEditableSlot(target, box, slot, found); why != "" {
		return fail(&Error{Code: CodeUnavailable, Message: "cannot edit " + target.label + ": " + why})
	}

	before := printerstate.BuildStateBlock(snap, derived, nil)
	change := &FilamentChange{
		Slot: target.label, Before: slotDefinition(slot),
		SameMaterialBefore: sameGroupStrings(boxs),
	}
	result := Result{
		Action: spec.Name, Effects: spec.Effects, Commands: spec.Commands,
		Before: before, Printer: printer, Job: printerstate.JobIdentityFrom(snap), Filament: change,
	}

	// Step 4: nothing to do.
	if slot.RFID == entry.ID && normHex(slot.Color) == rgb {
		change.After = change.Before
		change.SameMaterialAfter = change.SameMaterialBefore
		result.Effect = "no_change"
		result.After = before
		return result, nil
	}

	// Step 5: re-read and re-gate immediately before the write; refuse on any
	// change since step 3.
	boxs2, berr2 := deps.WS9999.BoxsInfo(ctx)
	if berr2 != nil {
		return fail(&Error{Code: CodeUnavailable, Message: "the slot definitions could not be re-read before the write: " + berr2.Error()})
	}
	if slotsHash(boxs2) != slotsHash(boxs) {
		return fail(&Error{Code: CodeConflict, Message: "the slot definitions changed while the edit was being prepared; nothing was written, try again", Changed: []string{"slots"}})
	}
	snap2 := printerstate.Take(ctx, deps.stateDeps(), printer)
	derived2 := deriveFor(locks.pl, snap2, nil)
	if !skipGate(spec.Name) {
		if gerr := checkGate(spec, derived2); gerr != nil {
			return fail(gerr)
		}
	}
	if derived2.State != derived.State || derived2.Bucket != derived.Bucket ||
		len(printerstate.JobIdentityDiff(printerstate.JobIdentityFrom(snap), printerstate.JobIdentityFrom(snap2))) > 0 ||
		boxMapString(snap2.Box) != boxMapString(snap.Box) || boxEnableString(snap2.Box) != boxEnableString(snap.Box) {
		return fail(&Error{Code: CodeConflict, Message: "the printer state changed while the edit was being prepared; nothing was written, try again", Changed: []string{"state"}})
	}

	// The write.
	edit := crealityws.MaterialEdit{BoxID: target.boxID, SlotID: target.slotID, BoxType: target.boxType, Entry: entry, Color: "#" + rgb}
	frame, sent, werr := deps.WS9999.ModifyMaterial(ctx, edit)
	if !sent {
		msg := "modifyMaterial was not sent"
		if werr != nil {
			msg += ": " + werr.Error()
		}
		return fail(&Error{Code: CodeUnavailable, Message: msg})
	}
	result.Accepted = true

	// Step 6: read-back on two channels.
	nineConfirmed := frame != nil
	after := boxs2
	if frame != nil {
		after = *frame
	} else if fresh, ferr := deps.WS9999.BoxsInfo(ctx); ferr == nil {
		after = fresh
		nineConfirmed = edit.Confirms(fresh)
	}
	if _, s2, ok := findSlot(after, target); ok {
		change.After = slotDefinition(s2)
	}
	change.SameMaterialAfter = sameGroupStrings(after)

	wantType, wantColor := "0"+entry.ID, "0"+rgb
	moonConfirmed := false
	deadline := time.Now().Add(moonrakerReadbackTimeout)
	for {
		mbox, mrack := queryBoxAndRack(ctx, deps)
		if mt, cv, ok := moonrakerSlotValues(mbox, mrack, target); ok {
			change.MoonrakerMaterialType, change.MoonrakerColor = mt, cv
			if mt == wantType && strings.EqualFold(cv, wantColor) {
				moonConfirmed = true
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(moonrakerReadbackInterval):
		}
	}

	if nineConfirmed && moonConfirmed {
		result.Effect = "confirmed"
	} else {
		result.Effect = "unconfirmed"
		result.Effects = append(append([]string(nil), result.Effects...), fmt.Sprintf(
			"read-back: port 9999 confirmed=%v (slot now rfid %q colour %q); Moonraker confirmed=%v (material_type %q, color_value %q, expected %q and %q). "+
				"The write was sent; check the slot on the printer before relying on it",
			nineConfirmed, change.After.RFID, change.After.Color, moonConfirmed, change.MoonrakerMaterialType, change.MoonrakerColor, wantType, wantColor))
	}

	afterSnap := printerstate.Take(ctx, deps.stateDeps(), printer)
	afterDerived := deriveFor(locks.pl, afterSnap, nil)
	result.After = printerstate.BuildStateBlock(afterSnap, afterDerived, nil)
	result.Job = printerstate.JobIdentityFrom(afterSnap)
	return result, nil
}

// SlotEditability reports whether slot (T1A..T4D or side_spool) can be edited
// by set_filament_definition right now, from a boxsInfo read, and why not when
// it cannot. It applies exactly the per-slot checks Execute applies (plan 3.3
// step 3), so get_filaments's editable and why_not fields can never promise an
// edit Execute would then refuse for a slot-level reason. It says nothing
// about the printer state (bucket, CFS quiescence), which the action's own
// gate reports.
func SlotEditability(b crealityws.BoxsInfo, slot string) (editable bool, whyNot string) {
	t, err := parseFilamentTarget(slot)
	if err != nil {
		return false, err.Message
	}
	if t.boxType == 1 && !sideSpoolEditVerified {
		return false, "editing the side spool is not yet verified on this firmware; edit it on the printer"
	}
	box, m, found := findSlot(b, t)
	if why := checkEditableSlot(t, box, m, found); why != "" {
		return false, why
	}
	return true, ""
}

// CatalogWritable reports whether a catalog entry can be written to a slot,
// and why not when it cannot (a 0-0 temperature range, a missing or absurd
// pressure advance, a maximum above this server's nozzle ceiling). It does
// not include the live nozzle cap, which needs a printer snapshot.
func CatalogWritable(e crealityws.CatalogEntry) (writable bool, whyNot string) {
	if err := checkCatalogEntryWritable(e); err != nil {
		return false, err.Message
	}
	return true, ""
}
