package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// start_print with a CFS connected (dev_docs/plan-v0.2.0.md sections 3.4 and
// 8a). It reproduces Creality's Device Manager "file already on the printer"
// path: colorMatch, then multiColorPrint, behind a proposal token that shows
// the filament-to-slot mapping. A bare Moonraker start is refused whenever a
// CFS is connected, because it would run the file with the printer's stale
// map.
//
// The first call (no token) reads the file's filament list and the slots
// from the printer, computes the mapping with Creality's own algorithm
// (colormatch.go) and returns it with a token. The second call re-reads and
// recomputes everything, refuses if the mapping, the file identity or ANY
// slot definition changed, and then runs the send: colorMatch, verify
// Moonraker's box.map, and only then multiColorPrint.

// spoolStartVerified gates Source "spool" (a side-spool start with a CFS
// connected): implemented, refused in every release unless the supervised
// session exercised it (plan V3). A variable only so tests can exercise both
// settings; nothing in production assigns it.
var spoolStartVerified = false

// mapVerifyTimeout and mapVerifyInterval bound the box.map poll between the
// colorMatch and the start frame (plan 3.4: every 250 ms up to 3 s). Variables
// so tests do not wait real seconds on a mismatch.
var (
	mapVerifyTimeout  = 3 * time.Second
	mapVerifyInterval = 250 * time.Millisecond
)

// distanceWarnThreshold is the CIEDE2000 distance above which the proposal
// carries a visible warning (plan 8a.4). Creality has no bound and the mapping
// is still allowed.
const distanceWarnThreshold = 10.0

// binding is what an action binds into its proposal token beyond Params, job
// and bucket (review-2 MF3): a canonical string compared on execute, plus what
// the send needs. It is nil for every action that binds nothing extra.
type binding struct {
	extra string

	// start_print with a CFS
	mapping   []MappedFilament
	items     []crealityws.ColorMatchItem
	path      string
	filename  string
	selfTest  bool
	spool     bool
	requested map[string]string // tool id -> requested slot label
	notes     []string
	flowNote  string
}

// computeBinding returns the binding for the actions that have one, or nil.
func computeBinding(ctx context.Context, deps Deps, pl *printerLock, spec actionSpec, snap printerstate.Snapshot, derived printerstate.Derived, params Params) (*binding, *Error) {
	if !derived.CFSConnected {
		if spec.Name == ActionStartPrint && (params.Source != "" || params.SlotMap != "" || params.SelfTestExplicit) {
			return nil, &Error{Code: CodeInvalidInput, Message: "source, slot_map and self_test only apply with a CFS connected; no CFS is connected, so this would start a plain print and ignore them: call start_print with just the filename"}
		}
		return nil, nil
	}
	switch spec.Name {
	case ActionStartPrint:
		return startBinding(ctx, deps, snap, params)
	case ActionResumePrint:
		return resumeBinding(ctx, deps, pl, snap, derived)
	}
	return nil, nil
}

// matchGcodeFile finds the printer's own record for filename by full path
// (plan 8a.10): "<gcodes root>/<filename>", subdirectories included. The
// gcodes root is not asked for; a record matches when its path ends in
// "/"+filename and the directory before that is named "gcodes", so a file of
// the same base name in a subdirectory cannot be mistaken for the root one.
// Exactly one match is required.
func matchGcodeFile(files []crealityws.GcodeFileInfo, filename string) (crealityws.GcodeFileInfo, *Error) {
	name := strings.TrimPrefix(filename, "/")
	var found []crealityws.GcodeFileInfo
	for _, f := range files {
		suffix := "/" + name
		if !strings.HasSuffix(f.Path, suffix) {
			continue
		}
		root := strings.TrimSuffix(f.Path, suffix)
		if root == "gcodes" || strings.HasSuffix(root, "/gcodes") {
			found = append(found, f)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return crealityws.GcodeFileInfo{}, &Error{Code: CodeNotFound, Message: fmt.Sprintf("the printer's own file list has no record of %s under its gcodes root", filename)}
	default:
		return crealityws.GcodeFileInfo{}, &Error{Code: CodeConflict, Message: fmt.Sprintf("%d printer file records match %s; refusing to guess", len(found), filename)}
	}
}

// splitList splits a ';' separated list, trims each element and drops empty
// trailing elements.
func splitList(s string) []string {
	parts := strings.Split(s, ";")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// slotPool builds the candidate pool from boxsInfo: CFS units (type 0, unit
// state 1), slots with state 1 or 2 and a colour, in unit then slot order.
func slotPool(b crealityws.BoxsInfo) []poolSlot {
	var pool []poolSlot
	for _, box := range b.MaterialBoxs {
		if box.Type != 0 || box.State != 1 {
			continue
		}
		for _, m := range box.Materials {
			if m.State == nil || (*m.State != 1 && *m.State != 2) {
				continue
			}
			c := normHex(m.Color)
			if c == "" {
				continue
			}
			pool = append(pool, poolSlot{
				BoxID: box.ID, MaterialID: m.ID, Label: slotLabel(box.ID, m.ID),
				Type: m.Type, Color: "#" + c, Vendor: m.Vendor, Name: m.Name, RFID: m.RFID, State: *m.State,
			})
		}
	}
	return pool
}

// groupOf returns the same_material group containing slot label, as slot
// names in the printer's order.
func groupOf(b crealityws.BoxsInfo, box, mat int) []string {
	for _, g := range b.SameMaterial {
		for _, s := range g.Slots {
			if s.BoxID == box && s.MaterialID == mat {
				out := make([]string, len(g.Slots))
				for i, m := range g.Slots {
					out[i] = slotLabel(m.BoxID, m.MaterialID)
				}
				return out
			}
		}
	}
	return nil
}

// startBinding computes the CFS start mapping and everything the token binds.
func startBinding(ctx context.Context, deps Deps, snap printerstate.Snapshot, params Params) (*binding, *Error) {
	spool := false
	switch strings.ToLower(strings.TrimSpace(params.Source)) {
	case "", "cfs":
	case "spool":
		spool = true
		if !spoolStartVerified {
			return nil, &Error{Code: CodeUnavailable, Message: "starting from the side spool while a CFS is connected is not yet verified on this firmware; start it on the printer, or use source cfs"}
		}
	default:
		return nil, &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("source %q must be cfs or spool", params.Source)}
	}

	files, err := deps.WS9999.GcodeFiles(ctx)
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Message: "the printer's file list could not be read over port 9999: " + err.Error()}
	}
	file, ferr := matchGcodeFile(files, params.Filename)
	if ferr != nil {
		return nil, ferr
	}
	if strings.TrimSpace(file.Material) == "" {
		return nil, &Error{Code: CodeUnavailable, Message: "this file has no filament list; re-slice with Creality Print"}
	}
	types := splitList(file.Material)
	colors := splitList(file.MaterialColors)
	if len(types) == 0 || len(types) != len(colors) {
		return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf(
			"the file's filament types (%q) and colours (%q) do not line up; re-slice with Creality Print", file.Material, file.MaterialColors)}
	}
	filaments := make([]fileFilament, len(types))
	for i := range types {
		c := ""
		if n := normHex(colors[i]); n != "" {
			c = "#" + n
		}
		filaments[i] = fileFilament{Index: i, Type: types[i], Color: c}
	}

	boxs, berr := deps.WS9999.BoxsInfo(ctx)
	if berr != nil {
		return nil, &Error{Code: CodeUnavailable, Message: "the slot definitions could not be read over port 9999: " + berr.Error()}
	}

	// The printer's own enableSelfTest is the default (plan 8a.7).
	selfTest := snap.WS9999.EnableSelfTest.Present && snap.WS9999.EnableSelfTest.Value == 1
	if params.SelfTestExplicit {
		selfTest = params.SelfTest
	}

	b := &binding{path: file.Path, filename: params.Filename, selfTest: selfTest, spool: spool}
	b.flowNote = flowNoteFor(snap)
	fileID := fmt.Sprintf("file=%s|size=%d|created=%d|material=%s|colors=%s", file.Path, file.Size, file.CreateTime, file.Material, file.MaterialColors)
	hash := slotsHash(boxs)

	if spool {
		if len(filaments) != 1 {
			return nil, &Error{Code: CodeInvalidInput, Message: "a side-spool start needs a single-filament file; this file lists " + strconv.Itoa(len(filaments)) + " filaments"}
		}
		var side *crealityws.SlotMaterial
		for _, box := range boxs.MaterialBoxs {
			if box.Type == 1 && len(box.Materials) > 0 {
				m := box.Materials[0]
				side = &m
			}
		}
		if side == nil || side.State == nil || *side.State == 0 || side.Type != filaments[0].Type {
			return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf(
				"the side spool must be defined and hold %s (the file's material) for a spool start", filaments[0].Type)}
		}
		b.extra = fmt.Sprintf("start|spool|selftest=%t|%s|slots=%s", selfTest, fileID, hash)
		b.notes = []string{
			"start from the side spool: the CFS is not used; confirm with the user that the spool on the holder is " + filaments[0].Type,
			fmt.Sprintf("self-test before printing: %t", selfTest),
			b.flowNote,
		}
		return b, nil
	}

	overrides, oerr := parseSlotMap(params.SlotMap)
	if oerr != nil {
		return nil, &Error{Code: CodeInvalidInput, Message: oerr.Error()}
	}
	pool := slotPool(boxs)
	assigned, unmatched, merr := matchFilaments(filaments, pool, overrides)
	if merr != nil {
		return nil, &Error{Code: CodeInvalidInput, Message: merr.Error()}
	}
	if len(unmatched) > 0 {
		var parts []string
		for _, idx := range unmatched {
			f := filaments[idx]
			var have []string
			for _, s := range pool {
				if s.Type == f.Type {
					have = append(have, s.Label)
				}
			}
			holder := "no slot holds that type"
			if len(have) > 0 {
				holder = "slots with that type: " + strings.Join(have, ", ") + " (already used by other filaments)"
			}
			parts = append(parts, fmt.Sprintf("filament %d (%s, %s): %s", idx, f.Type, f.Color, holder))
		}
		return nil, &Error{Code: CodeUnavailable, Message: "no slot can be mapped to " + strings.Join(parts, "; ") +
			". Load or define a matching slot (set_filament_definition), or pick a different file"}
	}

	b.requested = map[string]string{}
	for _, a := range assigned {
		row := MappedFilament{
			Index: a.Filament.Index, ToolID: toolID(a.Filament.Index), FileType: a.Filament.Type, FileColor: a.Filament.Color,
			Slot: a.Slot.Label, SlotVendor: a.Slot.Vendor, SlotName: a.Slot.Name, SlotType: a.Slot.Type, SlotColor: a.Slot.Color,
			Distance: a.Distance,
		}
		row.SameGroup = groupOf(boxs, a.Slot.BoxID, a.Slot.MaterialID)
		row.LikelyRunAs = a.Slot.Label
		if len(row.SameGroup) > 0 {
			row.LikelyRunAs = row.SameGroup[0]
		}
		if !boxs.SameMaterialOK {
			row.Warnings = append(row.Warnings, "the printer's same_material grouping could not be read: it may run another slot of the same definition instead of "+a.Slot.Label)
		} else if len(row.SameGroup) > 1 {
			if row.LikelyRunAs != a.Slot.Label {
				row.Warnings = append(row.Warnings, fmt.Sprintf(
					"the printer may use %s instead of %s: both must hold this spool (same_material group: %s)",
					row.LikelyRunAs, a.Slot.Label, strings.Join(row.SameGroup, ", ")))
			} else {
				var others []string
				for _, m := range row.SameGroup {
					if m != a.Slot.Label {
						others = append(others, m)
					}
				}
				row.Warnings = append(row.Warnings, fmt.Sprintf(
					"%s shares an auto-refill group with %s: if %s runs out the printer may continue from them, so they must hold the same spool",
					a.Slot.Label, strings.Join(others, ", "), a.Slot.Label))
			}
		}
		if a.Distance > distanceWarnThreshold {
			row.Warnings = append(row.Warnings, fmt.Sprintf(
				"colour distance %s is above %g: the slot colour is far from the file's colour (Creality still allows this mapping)",
				fmtDistance(a.Distance), distanceWarnThreshold))
		}
		b.mapping = append(b.mapping, row)
		b.items = append(b.items, crealityws.ColorMatchItem{
			ID: row.ToolID, Type: a.Slot.Type, Color: a.Slot.Color, BoxID: a.Slot.BoxID, MaterialID: a.Slot.MaterialID,
		})
		b.requested[row.ToolID] = a.Slot.Label
	}

	canon := canonicalMapping(assigned)
	b.extra = fmt.Sprintf("start|cfs|map=%s|selftest=%t|%s|slots=%s", canon, selfTest, fileID, hash)
	b.notes = []string{
		"slots cannot be checked for physical filament: confirm with the user that each mapped slot holds that spool",
		fmt.Sprintf("self-test before printing: %t (the printer's own default is used unless self_test was given)", selfTest),
		"the mapping follows Creality's Device Manager algorithm (exact material type, CIEDE2000 colour distance, one slot per filament)",
		b.flowNote,
	}
	for _, m := range b.mapping {
		for _, w := range m.Warnings {
			b.notes = append(b.notes, m.ToolID+" -> "+m.Slot+": "+w)
		}
	}
	return b, nil
}

// mapMismatchError is verifyMap's failure: the printer's box.map (or a
// missing reading) disagrees with the requested mapping.
type mapMismatchError struct {
	requested string
	actual    string
	reason    string
}

func (e *mapMismatchError) Error() string {
	return "box.map does not match the requested mapping: " + e.reason
}

func sortedPairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// checkBoxMap compares the printer's box.map with the requested tool to slot
// mapping (plan 3.4): for each mapped tool, the slot it maps to must have the
// same Moonraker (material_type, color_value) as the requested slot, so the
// printer's canonicalisation to a same_material group head is accepted
// (cfs-print-start.md 4.1) but a genuinely different slot is not. A nil or
// undecodable box, a missing key or unit, or an empty value is a failure.
func checkBoxMap(box *moonraker.Box, requested map[string]string) *mapMismatchError {
	req := sortedPairs(requested)
	if box == nil {
		return &mapMismatchError{requested: req, reason: "the box object could not be read from Moonraker"}
	}
	if box.Map == nil {
		return &mapMismatchError{requested: req, reason: "box.map is absent"}
	}
	actual := map[string]string{}
	for tool := range requested {
		actual[tool] = box.Map[tool]
	}
	values := func(label string) (string, string, bool) {
		b, m, ok := parseSlotLabel(label)
		if !ok {
			return "", "", false
		}
		u, has := box.Units[fmt.Sprintf("T%d", b)]
		if !has || m >= len(u.MaterialType) || m >= len(u.ColorValue) {
			return "", "", false
		}
		mt, cv := u.MaterialType[m], u.ColorValue[m]
		if mt == "" || cv == "" {
			return "", "", false
		}
		return mt, cv, true
	}
	for tool, want := range requested {
		got, ok := box.Map[tool]
		if !ok || got == "" {
			return &mapMismatchError{requested: req, actual: sortedPairs(actual), reason: "box.map has no entry for " + tool}
		}
		wt, wc, ok1 := values(want)
		gt, gc, ok2 := values(got)
		if !ok1 || !ok2 {
			return &mapMismatchError{requested: req, actual: sortedPairs(actual), reason: fmt.Sprintf("no Moonraker material data for %s or %s", want, got)}
		}
		if wt != gt || !strings.EqualFold(wc, gc) {
			return &mapMismatchError{requested: req, actual: sortedPairs(actual),
				reason: fmt.Sprintf("%s maps to %s (%s, %s) but %s (%s, %s) was requested", tool, got, gt, gc, want, wt, wc)}
		}
	}
	return nil
}

// sendStartCFS runs the confirmed CFS start (plan 3.4, 8a.1, 8a.2, 8a.7).
func (p *Policy) sendStartCFS(ctx context.Context, deps Deps, printer domain.Printer, identity string, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks, b *binding) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)

	// D2: cancel any armed idle-heat watchdog, but only at the last moment: after
	// the map check passed and immediately before the start frame (safety review
	// B1). A refused or unsent start leaves the printer idle with whatever heater
	// the user preheated, and this printer has no idle heater shutoff of its own,
	// so the watchdog must still be armed then. START_PRINT reheats by itself, so
	// a turn-off in the few milliseconds before the frame is harmless. Best
	// effort: the daemon must never gate a start.
	disarm := func() {
		if deps.Watchdog != nil {
			_ = deps.Watchdog.Disarm(ctx, identity)
		}
	}
	p.setUploading(identity, params.Filename, false)

	locks.pl.setPending(&printerstate.PendingAction{Kind: printerstate.PendingStart, IssuedAt: time.Now(), Timeout: spec.SettleTimeout})
	defer locks.pl.setPending(nil)

	// D7 (plan 8a.7): M221 S100 goes out before the colorMatch only when the
	// flow factor is not 100, and the previous value is restored only if the map
	// check fails or the start frame definitely was not sent. Once the start
	// frame was sent, or may have been (an ambiguous write error), the flow stays
	// at 100: nothing waits for "printing" any more and restoring under a print
	// that may be starting would defeat the reset.
	var prevFlowPercent *float64
	if snap.GCodeMove != nil && snap.GCodeMove.ExtrudeFactor != nil {
		v := *snap.GCodeMove.ExtrudeFactor * 100
		prevFlowPercent = &v
	}
	needsReset := prevFlowPercent != nil && !floatEqual(*prevFlowPercent, 100)
	commands := []string{}
	if needsReset {
		if err := deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM221, map[string]string{"percent": "100"}); err != nil {
			msg := "could not reset the flow factor to 100 before the start: " + err.Error() + "; nothing was sent to the CFS"
			if deliveryUnknown(err) {
				msg += ". The reset was not retried and " + queuedPendingNote
			}
			return Result{}, &Error{Action: spec.Name, Code: CodeUnavailable, Message: msg}
		}
		commands = append(commands, "M221 S100")
	}
	restoreFlow := func(result *Result) {
		if !needsReset {
			return
		}
		want := strconv.Itoa(int(*prevFlowPercent))
		if err := deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM221, map[string]string{"percent": want}); err == nil {
			result.StartPrintFlowRestored = prevFlowPercent
			result.Effects = append(result.Effects, "the flow factor was restored to "+want+"% (it had been reset to 100% for the start)")
		} else if deliveryUnknown(err) {
			result.Effects = append(result.Effects, "the flow factor restore to "+want+"% got no answer and was not retried: it may be queued behind a running macro and run when the printer gets to it; check get_printer_status")
		} else {
			result.Effects = append(result.Effects, "the flow factor could NOT be restored to "+want+"%: it is still 100%; set it back on the printer if you need it")
		}
	}
	flowStays := func(result *Result) {
		if needsReset {
			result.Effects = append(result.Effects, fmt.Sprintf(
				"the flow factor was reset from %.0f%% to 100%% for this start and stays at 100%%: it is restored only when a start is refused or definitely not sent", *prevFlowPercent))
		}
	}

	// The start-window record is set right before the first frame and is NOT
	// cleared when this call returns (8a.1); it is cleared only when the start
	// frame definitely was not sent.
	locks.pl.setStartRec(&startInFlight{
		filename: params.Filename, path: b.path, mapping: canonicalMappingString(b),
		issuedAt: time.Now(), priorJob: printerstate.JobIdentityFrom(snap),
	})

	result := Result{
		Action: spec.Name, Effects: append([]string(nil), b.notes...), Before: before, Printer: printer,
		Job: printerstate.JobIdentityFrom(snap), Mapping: b.mapping,
	}

	var sent bool
	var err error
	if b.spool {
		commands = append(commands, "opGcodeFile (port 9999)")
		disarm()
		sent, err = deps.WS9999.StartSpoolPrint(ctx, b.path, b.selfTest)
	} else {
		commands = append(commands, "colorMatch (port 9999)", "multiColorPrint (port 9999)")
		verify := func(vctx context.Context) error {
			if err := verifyMapPoll(vctx, deps, b.requested); err != nil {
				return err
			}
			disarm() // the map is verified; the start frame follows immediately
			return nil
		}
		sent, err = deps.WS9999.StartCFSPrint(ctx, b.path, b.items, b.selfTest, verify)
	}
	result.Commands = commands
	if sent || errors.As(err, new(*crealityws.StartFrameError)) {
		locks.pl.markStartSent()
	}

	var mismatch *mapMismatchError
	var ambiguous *crealityws.StartFrameError
	switch {
	case errors.As(err, &mismatch):
		locks.pl.setStartRec(nil)
		result.Effect = "refused_map_mismatch"
		result.Effects = append(result.Effects, fmt.Sprintf(
			"the start frame was NOT sent: %s. Requested map: %s. The printer reports: %s. The colorMatch frame was sent, so the printer may keep the written map until the next start, and get_printer_status may then show preparing (a start window) because of that leftover map even though nothing is printing",
			mismatch.reason, mismatch.requested, mismatch.actual))
		restoreFlow(&result)
	case errors.As(err, &ambiguous):
		// The write of the start frame itself failed: it may have been delivered.
		// Keep the record, leave the flow at 100 and say so plainly.
		result.Effect = "unconfirmed"
		result.Effects = append(result.Effects, "the start frame may have been delivered even though writing it reported an error ("+ambiguous.Error()+"): the print may be starting. Call get_printer_status to follow it before doing anything else")
		flowStays(&result)
	case !sent:
		locks.pl.setStartRec(nil)
		result.Effect = "not_sent"
		msg := "the start frame was not sent"
		if err != nil {
			msg += ": " + err.Error()
		}
		result.Effects = append(result.Effects, msg+". If the colorMatch frame was sent, the printer may keep the written map until the next start")
		restoreFlow(&result)
	default:
		result.Accepted = true
		result.Effect = "sent"
		if b.spool {
			enabledZero := false
			if box, _ := queryBoxAndRack(ctx, deps); box != nil && box.Enable != nil && *box.Enable == 0 {
				enabledZero = true
			}
			if !enabledZero {
				result.Effect = "unconfirmed"
			}
			result.Effects = append(result.Effects, fmt.Sprintf(
				"start sent from the side spool; box.enable reads 0 (spool mode): %t. The printer runs its self-test for several minutes; follow it with get_printer_status", enabledZero))
		} else {
			result.Effects = append(result.Effects,
				"map verified and start sent; the printer runs its self-test for several minutes; follow it with get_printer_status")
		}
		flowStays(&result)
	}

	afterSnap := printerstate.Take(ctx, deps.stateDeps(), printer)
	afterDerived := deriveFor(locks.pl, afterSnap, nil)
	result.After = printerstate.BuildStateBlock(afterSnap, afterDerived, nil)
	result.Job = printerstate.JobIdentityFrom(afterSnap)
	return result, nil
}

func canonicalMappingString(b *binding) string {
	if b.spool {
		return "spool"
	}
	return sortedPairs(b.requested)
}

// verifyMapPoll polls Moonraker's box object (a light query, not a full
// snapshot) until checkBoxMap passes or the timeout elapses; the last
// mismatch is returned.
func verifyMapPoll(ctx context.Context, deps Deps, requested map[string]string) error {
	deadline := time.Now().Add(mapVerifyTimeout)
	for {
		box, _ := queryBoxAndRack(ctx, deps)
		mm := checkBoxMap(box, requested)
		if mm == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return mm
		}
		select {
		case <-ctx.Done():
			return mm
		case <-time.After(mapVerifyInterval):
		}
	}
}

// startCommands lists the commands a CFS start will send, for the proposal.
func startCommands(b *binding) []string {
	if b.spool {
		return []string{"opGcodeFile (port 9999)"}
	}
	return []string{"colorMatch (port 9999)", "multiColorPrint (port 9999)"}
}

// stopWaitTimeout and stopPollInterval bound how long the stop reply waits for
// the printer to show it is stopping (9999 state 7) or stopped (4): the stop
// takes effect after about 20 s (the current self-test step finishes first), so
// the reply only waits for the first sign, not for the wind-down. Variables so
// tests do not wait real seconds.
var (
	stopWaitTimeout  = 15 * time.Second
	stopPollInterval = 500 * time.Millisecond
)

// sendStopInWindow cancels during the start window with Creality's own 9999
// stop (plan 8a.1), verified live 2026-09-29. It is reached when
// stopDuringStartVerified is true and the derived state is the start window:
// print_stats has no job yet during the self-test, so a Moonraker cancel is not
// known to stop it. The start-window record is cleared once the stop frame is
// sent. The reply returns as soon as 9999 state reads 7 (stopping) or 4
// (aborted), up to stopWaitTimeout, with Effect "stopping"; if neither is seen
// the frame was still sent and the Effect is "sent". It does not wait for the
// wind-down (about a minute) and does not hold the lock for it.
func (p *Policy) sendStopInWindow(ctx context.Context, deps Deps, printer domain.Printer, spec actionSpec, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)
	locks.pl.setPending(&printerstate.PendingAction{Kind: printerstate.PendingCancel, IssuedAt: time.Now(), Timeout: spec.SettleTimeout})
	defer locks.pl.setPending(nil)

	sent, err := deps.WS9999.Stop(ctx)
	accepted := sent && err == nil
	result := Result{
		Action: spec.Name, Accepted: accepted, Effect: "sent",
		Commands: []string{"stop (port 9999)"}, Before: before, Printer: printer,
	}
	if !accepted {
		result.Effect = "not_sent"
		msg := "the 9999 stop frame was not sent"
		if err != nil {
			msg += ": " + err.Error()
		}
		result.Effects = []string{msg + ". Stop the print on the printer screen."}
	} else {
		// State 7 (stopping) is always a sign. State 4 (aborted) persists at rest
		// from an earlier stop, so it only counts once a state other than 4 was seen
		// after the frame (final review m1); a stale 4 alone gives Effect sent.
		seen, sawOther := false, false
		deadline := time.Now().Add(stopWaitTimeout)
		for !seen {
			rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			st, rerr := deps.WS9999.ReadStatus(rctx)
			cancel()
			if rerr == nil && st.State.Present {
				switch {
				case st.State.Value == 7, st.State.Value == 4 && sawOther:
					seen = true
				case st.State.Value != 4:
					sawOther = true
				}
				if seen {
					break
				}
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(stopPollInterval):
			}
		}
		if seen {
			// The printer is stopping: the start record has done its job. A stale 4 alone
			// (Effect sent) keeps the record until the 9999 state moves.
			locks.pl.setStartRec(nil)
			result.Effect = "stopping"
		}
		result.Effects = []string{"cancel during the print start: the 9999 stop message was sent (Creality's own Stop), because print_stats has no job yet. " +
			"The printer finishes its current self-test step (about 20 s), turns the heaters off and returns to idle within about a minute; follow with get_printer_status"}
	}
	afterSnap := printerstate.Take(ctx, deps.stateDeps(), printer)
	afterDerived := deriveFor(locks.pl, afterSnap, nil)
	result.After = printerstate.BuildStateBlock(afterSnap, afterDerived, nil)
	result.Job = printerstate.JobIdentityFrom(afterSnap)
	return result, nil
}

// flowNoteFor discloses the flow reset of a start (D7): the flow factor is set
// to 100% first when it is not already, restored if the start is refused or
// definitely not sent, and left at 100% once the start frame was sent.
func flowNoteFor(snap printerstate.Snapshot) string {
	const tail = "it is put back only if the start is refused or definitely not sent, and stays at 100% once the start is sent"
	if snap.GCodeMove != nil && snap.GCodeMove.ExtrudeFactor != nil && !floatEqual(*snap.GCodeMove.ExtrudeFactor*100, 100) {
		return fmt.Sprintf("the flow factor is %.0f%% and will be reset to 100%% first: %s", *snap.GCodeMove.ExtrudeFactor*100, tail)
	}
	return "the flow factor is reset to 100% first when it is not already (it is 100% now, so nothing changes)"
}
