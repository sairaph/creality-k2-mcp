package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/policy"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
)

// This file implements the "Control" tool group (T10): the only tools that
// ever reach the printer through internal/policy.Execute
// (dev_docs/safety-architecture.md section 3.2's "Execute is the only path
// from a tool handler to a printer write"). Every handler below resolves a
// printer, calls executeControl, and renders the resulting policy.Result or
// *policy.Error; none of them ever calls a Moonraker or port-9999 client
// method directly. start_print, pause_print, resume_print and cancel_print
// are the job lifecycle; set_nozzle_temperature, set_bed_temperature,
// set_fan_speed, set_speed_factor, set_flow_factor, set_light and
// exclude_object are the setpoint writes. resume_print, cancel_print and
// exclude_object use the confirm_token proposal flow (D3); every other tool
// here sends in one call.

func registerControlTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "start_print", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "start_print",
		Description: "Starts printing filename from the printer's gcodes root (call list_gcode_files first to " +
			"pick one). Only available while the printer is idle. The printer's own START_PRINT macro heats the " +
			"bed and nozzle, homes and cleans the nozzle before printing actually begins, and this call resets " +
			"the flow factor to 100% first if it was not already (without a CFS the previous value is restored if the " +
			"print does not reach printing; with a CFS it is restored only if the start is refused or definitely not sent, " +
			"and stays at 100% once the start frame was sent). Because nothing the server can read confirms the " +
			"bed is actually clear, the AI must ask the user to confirm the bed is clear before calling this " +
			"tool, and only call it after that confirmation; nothing verifies it, not even the token a CFS start uses, so it " +
			"is the caller's responsibility. Without a CFS it sends immediately with no confirm_token. " +
			"WITH A CFS CONNECTED it is a two-step flow: the first call (no confirm_token) sends nothing and returns a " +
			"mapping proposal, computed by Creality's own algorithm from the file's filament list and the slots: for each " +
			"filament of the file, the slot it will use, the colour distance, and every slot of that slot's refill group " +
			"with a warning when the printer may run a different slot of the group instead (\"the printer may use T1B " +
			"instead of T1C: both must hold this spool\"). The AI must show the user this mapping including every " +
			"warning, ask the user to confirm that each mapped slot really holds that spool (slots cannot be checked for " +
			"physical filament) AND that the bed is clear, and only then call again with the returned confirm_token and " +
			"the SAME filename, source, slot_map and self_test. The second call verifies the printer accepted the map " +
			"before sending the start frame and reports effect sent: the printer then runs a self-test of several minutes " +
			"with the job still standby, so follow it with get_printer_status; it never reports started. Optional " +
			"slot_map overrides the automatic choice for a filament (filament is its 0-based index in the file, slot is " +
			"T1A to T4D; the slot must hold the same material type); self_test runs the printer's pre-print self-test " +
			"(default: the printer's own setting); source spool (start from the side spool) is not yet verified on this " +
			"firmware and is refused. source, slot_map and self_test only apply with a CFS connected; without one the call " +
			"is refused rather than silently ignoring them. Related: get_filaments, list_gcode_files, get_printer_status, pause_print and " +
			"cancel_print once printing.",
		InputSchema: withEnum(inputSchema[startPrintInput](), "source", "cfs", "spool"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, startPrintHandler(s))

	registerTool(s, domain.ToolInfo{Name: "pause_print", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "pause_print",
		Description: "Pauses the current print. Only available while printing (not during START_PRINT's own " +
			"prepare/heat/home sequence, where pausing is unverified and blocked). The printer's PAUSE macro " +
			"drops the nozzle target to 140 C and, if the toolhead is homed, lifts it, moves to the purge/clean " +
			"position, wipes the nozzle and parks; the part and auxiliary fans turn off. Sends immediately with " +
			"no confirm_token. Related: resume_print to continue (its proposal shows what will be restored), " +
			"cancel_print to stop the job instead, get_printer_status to see the paused state. With a CFS connected, resume from this server works only after a pause made here while the CFS read clean; the pause result says whether a resume record was kept.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, pausePrintHandler(s))

	registerTool(s, domain.ToolInfo{Name: "resume_print", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "resume_print",
		Description: "Resumes a paused print. Requires the two-step proposal flow: call once with no " +
			"confirm_token to get a proposal showing the stored pre-pause nozzle target and fan speeds this " +
			"will restore, then call again passing that proposal's confirm_token to actually resume. The " +
			"printer's RESUME macro reheats the nozzle to the stored target, homes X/Y first if unhomed, purges " +
			"82 mm of filament and wipes the nozzle before the firmware's own pause check ever runs, then " +
			"restores the stored part and auxiliary fan speeds. The token expires after 120 seconds and is " +
			"invalidated if the printer's state changes before it is used (a conflict error names what changed); " +
			"call again with no confirm_token for a fresh proposal in that case. Related: get_printer_status to " +
			"see the stored targets beforehand, pause_print, cancel_print to stop instead. With a CFS connected it is allowed only for a clean pause this server issued, with the CFS still reading clean and nothing changed at the printer; otherwise resume on the printer screen or in Creality Print.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, resumePrintHandler(s))

	registerTool(s, domain.ToolInfo{Name: "cancel_print", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "cancel_print",
		Description: "Cancels the current print. Requires the same two-step proposal flow as resume_print: call " +
			"once with no confirm_token to see what will happen, then again with that token to actually cancel. " +
			"Available while printing, during START_PRINT's own prepare sequence, or while paused. The printer's " +
			"END_PRINT macro runs (lifts, retracts if hot, turns off heaters and fans, parks) and the EEPROM " +
			"power-loss-recovery slot is cleared, a genuine physical write. This cannot be undone: the job " +
			"cannot be resumed after this. Related: pause_print to stop temporarily instead, get_current_job to " +
			"check progress before deciding. Cancel is never blocked by a CFS signal, but during the self-test right after a print start (state preparing while the job is still standby) a cancel from this server is refused: stop it on the printer screen.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, cancelPrintHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_nozzle_temperature", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_nozzle_temperature",
		Description: "Sets the nozzle (extruder) target temperature in Celsius. While a print is running, the " +
			"new target must fall within the configured band of the current target (default +-10 C; widen it in " +
			"settings for a larger change) and applies immediately with no confirm_token, since a later pause " +
			"would overwrite it with the stored pause target anyway. While idle, heating requires the idle-heat " +
			"watchdog background daemon to be running: it turns the heater off automatically after the " +
			"configured idle_heat_minutes, and this call is refused with a hint if the daemon is not reachable. " +
			"Setting a nozzle temperature while paused is blocked entirely: PAUSE stores the target and RESUME " +
			"restores it, so a change made while paused would be silently undone. Related: get_printer_status " +
			"for the current target and configured band, set_bed_temperature. With a CFS connected it is refused during a print (the CFS changes the nozzle temperature itself during filament changes, which this server cannot see) and while idle it needs the CFS at rest.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
	}, setNozzleTemperatureHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_bed_temperature", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_bed_temperature",
		Description: "Sets the bed target temperature in Celsius. While a print is running, the new target must " +
			"fall within the configured band of the current target (default +-5 C; widen it in settings for a " +
			"larger change) and applies immediately with no confirm_token. While idle, heating requires the " +
			"idle-heat watchdog background daemon to be running, the same as set_nozzle_temperature, and is " +
			"refused with a hint if it is not reachable. Setting a bed temperature while paused is blocked " +
			"entirely for the same reason as the nozzle: PAUSE/RESUME would silently undo it. Related: " +
			"get_printer_status for the current target and configured band, set_nozzle_temperature.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
	}, setBedTemperatureHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_fan_speed", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_fan_speed",
		Description: "Sets one fan channel's speed as a percent (0-100). fan must be one of \"part\" (the print " +
			"cooling fan), \"case\" (the chamber fan, which shares its output pins with the chamber_fan " +
			"thermostat and can be overridden again later by that thermostat if the chamber temperature crosses " +
			"its own target), or \"auxiliary\". Available while printing or idle. While printing, the part fan " +
			"may not be dropped below the configured floor of its current speed in one call (default 50% of the " +
			"current value; widen it in settings for a larger drop). Sends immediately with no confirm_token. " +
			"Related: get_printer_status for the current fan speeds. With a CFS connected, idle needs the CFS at rest and a print needs it to read clean with no error; a filament change can reset the value.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
		InputSchema: withEnum(inputSchema[setFanSpeedInput](), "fan", fanChannelValues()...),
	}, setFanSpeedHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_speed_factor", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_speed_factor",
		Description: "Sets the print speed factor as a percent (Moonraker M220), only available while printing, " +
			"bounded to the configured band (default 50-150%; widen it in settings for a larger change). This is " +
			"runtime only: both START_PRINT and END_PRINT reset it. Sends immediately with no confirm_token. " +
			"Related: set_flow_factor, get_printer_status for the current factor. With a CFS connected it needs the CFS to read clean with no error, and a filament change can reset it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
	}, setSpeedFactorHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_flow_factor", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_flow_factor",
		Description: "Sets the extrusion flow factor as a percent (Moonraker M221), only available while " +
			"printing, bounded to the configured band (default 90-110%; widen it in settings for a larger " +
			"change). Unlike the speed factor, Creality's own macros do not reset this at the end of a print; " +
			"start_print resets it defensively to 100% before starting a new one, so a value left over from a " +
			"previous print never carries into the next. Sends immediately with no confirm_token. Related: " +
			"set_speed_factor, get_printer_status for the current factor. Refused whenever a CFS is connected (flow scales the purge volumes of a filament change).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
	}, setFlowFactorHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_light", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_light",
		Description: "Turns the chamber light on or off. Available in every printer state except offline, even " +
			"while every other write is blocked, since it has no effect on the print itself. Sends immediately " +
			"with no confirm_token. Related: get_camera_snapshot, whose body explains when turning the light on " +
			"helps see the chamber.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
	}, setLightHandler(s))

	registerTool(s, domain.ToolInfo{Name: "exclude_object", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "exclude_object",
		Description: "Excludes one printable object from the rest of the current print (Klipper's " +
			"EXCLUDE_OBJECT), matched case-insensitively against the object names get_current_job reports. " +
			"Requires the two-step proposal flow: call once with no confirm_token to see the object and current " +
			"state, then again with that token to actually exclude it. Available while printing or paused. This " +
			"stops the object printing for the rest of this job and cannot be undone; refused if the object is " +
			"already excluded or if it is the last remaining non-excluded object (a print needs at least one " +
			"object left printing). Related: get_current_job to see which objects exist and which are already " +
			"excluded. With a CFS connected it needs the CFS to read clean with no error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, excludeObjectHandler(s))
}

// --- Shared plumbing ---

// executeControl builds this printer's policy.Deps (policyDeps, deps.go) and
// calls the shared *policy.Policy's Execute (dev_docs/safety-architecture.md
// section 3.2), the only path any handler below reaches the printer through.
func (s *Server) executeControl(ctx context.Context, printer domain.Printer, name policy.ActionName, params policy.Params, token string) (policy.Result, error) {
	deps, err := s.policyDeps(printer)
	if err != nil {
		return policy.Result{}, err
	}
	return s.deps.PolicyEngine.Execute(ctx, deps, printer, s.deps.Settings, name, params, token)
}

// controlFront is the frontmatter every control tool renders: the shared
// StateBlock (dev_docs/safety-architecture.md section 5) set to the most
// current state known (After once a write was actually sent, Before for a
// still-unsent proposal), plus the policy.Result fields the task calls for
// explicitly (accepted, effect, before/after blocks, proposal details).
type controlFront struct {
	printerstate.StateBlock `yaml:",inline"`

	Action    string `yaml:"action"`
	Accepted  bool   `yaml:"accepted"`
	Effect    string `yaml:"effect,omitempty"`
	Proposed  bool   `yaml:"proposed,omitempty"`
	Token     string `yaml:"confirm_token,omitempty"`
	ExpiresAt string `yaml:"expires_at,omitempty"`

	Effects  []string `yaml:"effects,omitempty"`
	Commands []string `yaml:"commands,omitempty"`

	Before *printerstate.StateBlock `yaml:"before,omitempty"`
	After  *printerstate.StateBlock `yaml:"after,omitempty"`

	IdleHeatArmMinutes        int      `yaml:"idle_heat_arm_minutes,omitempty"`
	IdleHeatArmTargetC        float64  `yaml:"idle_heat_arm_target_c,omitempty"`
	FlowFactorRestoredPercent *float64 `yaml:"flow_factor_restored_percent,omitempty"`

	// Mapping is a CFS start_print proposal's (or result's) filament to slot
	// mapping; Filament is a set_filament_definition edit (plan 3.2).
	Mapping  []mappingFront `yaml:"mapping,omitempty"`
	Filament *filamentFront `yaml:"filament,omitempty"`
}

func (f *controlFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

// controlFrontFrom renders a policy.Result into controlFront. The embedded
// StateBlock is Before for a still-unsent proposal (After is empty on a
// Proposed Result per its own doc comment) and After once a write was
// actually attempted, so a reader always sees the most current state in the
// conventional top-level fields as well as both blocks explicitly.
func controlFrontFrom(res policy.Result) *controlFront {
	before := res.Before
	mapping := mappingFrom(res.Mapping)
	f := &controlFront{
		StateBlock: before,
		Action:     string(res.Action),
		Proposed:   res.Proposed,
		Token:      res.Token,
		Accepted:   res.Accepted,
		Effect:     res.Effect,
		Effects:    res.Effects,
		Commands:   res.Commands,
		Before:     &before,
		Mapping:    mapping,
		Filament:   filamentFrom(res.Filament),
	}
	if res.Proposed {
		if !res.ExpiresAt.IsZero() {
			f.ExpiresAt = res.ExpiresAt.UTC().Format(time.RFC3339)
		}
		return f
	}
	after := res.After
	f.StateBlock = after
	f.After = &after
	if res.IdleHeatArm != nil {
		f.IdleHeatArmMinutes = res.IdleHeatArm.ArmMinutes
		f.IdleHeatArmTargetC = res.IdleHeatArm.TargetC
	}
	f.FlowFactorRestoredPercent = res.StartPrintFlowRestored
	return f
}

// mappingFront is one row of a CFS start mapping in the frontmatter.
type mappingFront struct {
	Filament    int      `yaml:"filament"`
	Tool        string   `yaml:"tool"`
	FileType    string   `yaml:"file_type"`
	FileColor   string   `yaml:"file_color"`
	Slot        string   `yaml:"slot"`
	SlotBrand   string   `yaml:"slot_brand,omitempty"`
	SlotName    string   `yaml:"slot_name,omitempty"`
	SlotType    string   `yaml:"slot_type"`
	SlotColor   string   `yaml:"slot_color"`
	Distance    float64  `yaml:"color_distance"`
	RefillGroup []string `yaml:"refill_group,omitempty"`
	LikelyRunAs string   `yaml:"likely_run_as,omitempty"`
	Warnings    []string `yaml:"warnings,omitempty"`
}

func mappingFrom(rows []policy.MappedFilament) []mappingFront {
	if len(rows) == 0 {
		return nil
	}
	out := make([]mappingFront, len(rows))
	for i, m := range rows {
		out[i] = mappingFront{
			Filament: m.Index, Tool: m.ToolID, FileType: m.FileType, FileColor: m.FileColor, Slot: m.Slot,
			SlotBrand: m.SlotVendor, SlotName: m.SlotName, SlotType: m.SlotType, SlotColor: m.SlotColor,
			Distance: float64(int(m.Distance*10+0.5)) / 10, RefillGroup: m.SameGroup, LikelyRunAs: m.LikelyRunAs, Warnings: m.Warnings,
		}
	}
	return out
}

// filamentFront is a set_filament_definition edit in the frontmatter.
type filamentFront struct {
	Slot                  string                `yaml:"slot"`
	Before                policy.SlotDefinition `yaml:"before"`
	After                 policy.SlotDefinition `yaml:"after"`
	RefillGroupsBefore    []string              `yaml:"refill_groups_before,omitempty"`
	RefillGroupsAfter     []string              `yaml:"refill_groups_after,omitempty"`
	MoonrakerMaterialType string                `yaml:"moonraker_material_type,omitempty"`
	MoonrakerColor        string                `yaml:"moonraker_color,omitempty"`
}

func filamentFrom(c *policy.FilamentChange) *filamentFront {
	if c == nil {
		return nil
	}
	return &filamentFront{
		Slot: c.Slot, Before: c.Before, After: c.After, RefillGroupsBefore: c.SameMaterialBefore,
		RefillGroupsAfter: c.SameMaterialAfter, MoonrakerMaterialType: c.MoonrakerMaterialType, MoonrakerColor: c.MoonrakerColor,
	}
}

func slotDefText(d policy.SlotDefinition) string {
	if d.RFID == "" && d.Name == "" {
		return "no definition"
	}
	return fmt.Sprintf("%s (%s, id %s, %s)", policy.DisplayName(d.Vendor, d.Name), d.Type, d.RFID, d.Color)
}

// mappingTable renders the mapping as a Markdown table for the AI to show the
// user. The canonicalisation and colour-distance warnings are in the effects
// list that follows it, so they are not repeated here.
func mappingTable(rows []policy.MappedFilament) string {
	var b strings.Builder
	b.WriteString("| filament | tool | file wants | uses slot | slot holds | colour distance |\n|---|---|---|---|---|---|\n")
	for _, m := range rows {
		fmt.Fprintf(&b, "| %d | %s | %s %s | %s | %s (%s %s) | %.1f |\n", m.Index, m.ToolID, m.FileType, m.FileColor, m.Slot,
			policy.DisplayName(m.SlotVendor, m.SlotName), m.SlotType, m.SlotColor, m.Distance)
	}
	return strings.TrimRight(b.String(), "\n")
}

// controlBody renders the Markdown body every control tool result shares:
// what happened (or what is proposed) and the exact next call, per
// dev_docs/safety-architecture.md section 5.
func controlBody(res policy.Result) string {
	var b strings.Builder
	if res.Proposed {
		fmt.Fprintf(&b, "Proposal for %s on %s: nothing has been sent to the printer yet.\n\n", res.Action, res.Printer.Name)
		if len(res.Mapping) > 0 {
			b.WriteString("Filament to slot mapping:\n\n" + mappingTable(res.Mapping) + "\n\n")
		}
		if len(res.Effects) > 0 {
			b.WriteString("This will:\n")
			for _, e := range res.Effects {
				fmt.Fprintf(&b, "- %s\n", e)
			}
			b.WriteString("\n")
		}
		if res.Action == policy.ActionStartPrint {
			b.WriteString("Before confirming: show the user this mapping including every warning, ask them to confirm that each " +
				"mapped slot really holds that spool (the server cannot see the spools) and that the bed is clear. Then call " +
				"start_print again with the SAME filename, source, slot_map and self_test, plus ")
			fmt.Fprintf(&b, "confirm_token: %q. The token expires at %s; if the printer, the file or any slot changes before then it is "+
				"invalidated and this call must be repeated for a fresh proposal.", res.Token, res.ExpiresAt.UTC().Format(time.RFC3339))
			return b.String()
		}
		fmt.Fprintf(&b, "Call %s again with confirm_token: %q to execute it. The token expires at %s; if the "+
			"printer's state changes before then it is invalidated and this call must be repeated for a fresh "+
			"proposal.", res.Action, res.Token, res.ExpiresAt.UTC().Format(time.RFC3339))
		return b.String()
	}

	switch res.Effect {
	case "no_change":
		fmt.Fprintf(&b, "%s: nothing was sent. The slot already holds this definition.", res.Action)
		if res.Filament != nil {
			fmt.Fprintf(&b, " %s: %s.", res.Filament.Slot, slotDefText(res.Filament.Before))
		}
		return b.String()
	case "refused_map_mismatch":
		fmt.Fprintf(&b, "%s was NOT started: after the mapping was sent, the printer's own map did not match it, so the start frame was not sent.", res.Action)
		appendEffects(&b, res.Effects)
		b.WriteString("\n\nDo not simply retry. The colour map was already sent, so the printer may keep it until the next start, and get_printer_status may then show preparing with no print running: if it does, the leftover map is the cause; clear it or start the print on the printer, then request a fresh proposal. If get_printer_status shows idle, a fresh proposal is safe.")
		return b.String()
	case "not_sent":
		fmt.Fprintf(&b, "%s was NOT started: the start frame could not be sent.", res.Action)
		appendEffects(&b, res.Effects)
		b.WriteString("\n\nCall get_printer_status to see the printer's state before trying again; if the colour map was sent the printer may keep it until the next start and show preparing with no print running.")
		return b.String()
	}

	if res.Effect == "unconfirmed" && res.Action == policy.ActionStartPrint && !res.Accepted {
		fmt.Fprintf(&b, "%s: writing the start frame reported an error, but the frame may have been delivered, so the print may be starting. Do not start it again: call get_printer_status to follow it (it may show preparing while the self-test runs).", res.Action)
		appendEffects(&b, res.Effects)
		return b.String()
	}

	if !res.Accepted {
		fmt.Fprintf(&b, "%s was sent to %s but the printer rejected or failed to accept it. Call "+
			"get_printer_status to see the current state before retrying.", res.Action, res.Printer.Name)
		return b.String()
	}
	switch res.Effect {
	case "confirmed":
		fmt.Fprintf(&b, "%s was sent to %s and the expected result was confirmed.", res.Action, res.Printer.Name)
	case "unconfirmed":
		if res.Filament != nil {
			fmt.Fprintf(&b, "%s was sent to %s but the new definition was not confirmed on both channels. Call get_filaments to see what the slot holds now before deciding whether to edit it again; the effects below show what each channel reported.", res.Action, res.Printer.Name)
			break
		}
		fmt.Fprintf(&b, "%s was sent to %s and accepted, but the expected result was not observed within the "+
			"settle timeout. Call get_printer_status to check the current state before deciding whether to "+
			"retry; a lost confirmation does not necessarily mean the write failed.", res.Action, res.Printer.Name)
	case "sent":
		fmt.Fprintf(&b, "%s was sent to %s: the map was verified and the start frame was written. The printer is not printing yet: "+
			"it runs a self-test for several minutes with the job still standby. Follow it with get_printer_status; it is not "+
			"confirmed that the print began.", res.Action, res.Printer.Name)
	default:
		fmt.Fprintf(&b, "%s was sent to %s.", res.Action, res.Printer.Name)
	}
	if len(res.Mapping) > 0 {
		b.WriteString("\n\nFilament to slot mapping used:\n\n" + mappingTable(res.Mapping))
	}
	if res.Filament != nil {
		fmt.Fprintf(&b, "\n\nSlot %s: %s -> %s.", res.Filament.Slot, slotDefText(res.Filament.Before), slotDefText(res.Filament.After))
		if len(res.Filament.SameMaterialBefore) > 0 || len(res.Filament.SameMaterialAfter) > 0 {
			fmt.Fprintf(&b, "\nRefill groups before: %s\nRefill groups after: %s",
				strings.Join(res.Filament.SameMaterialBefore, "; "), strings.Join(res.Filament.SameMaterialAfter, "; "))
		}
	}
	appendEffects(&b, res.Effects)
	if res.IdleHeatArm != nil {
		fmt.Fprintf(&b, "\nThe idle-heat watchdog was armed to turn this heater off automatically after %d "+
			"minutes unless a job starts or the target is changed first.", res.IdleHeatArm.ArmMinutes)
	}
	if res.StartPrintFlowRestored != nil {
		fmt.Fprintf(&b, "\nThe print did not reach the printing state; the previous flow factor (%.0f%%) was "+
			"restored.", *res.StartPrintFlowRestored)
	}
	return b.String()
}

func appendEffects(b *strings.Builder, effects []string) {
	if len(effects) == 0 {
		return
	}
	b.WriteString("\n\nDisclosed effects:\n")
	for _, e := range effects {
		fmt.Fprintf(b, "- %s\n", e)
	}
}

// controlFailure renders err from executeControl: a *policy.Error is
// rendered directly with a code- and action-specific hint
// (policyErrorHint), since internal/policy has no RenderError method of its
// own to satisfy failure()'s policyError seam (reply.go) - the smallest
// adapter for that gap lives here, entirely within this file, rather than as
// a change to internal/policy or to reply.go's shared classification.
// Anything else (a policyDeps type-assertion failure, a context error
// surfaced some other way) falls back to the generic failure() path.
func controlFailure(err error) *mcp.CallToolResult {
	var perr *policy.Error
	if errors.As(err, &perr) {
		return render.ErrorResult(render.Error{
			Code:    string(perr.Code),
			Message: perr.Message,
			Hint:    policyErrorHint(perr),
		})
	}
	return failure("execute the control action", err, "")
}

// policyErrorHint gives the exact next call or what the user must do for
// each policy.Code, per dev_docs/safety-architecture.md section 5 ("Errors
// carry the same state block, so a refused call still teaches the AI what
// it can do").
func policyErrorHint(err *policy.Error) string {
	if err.Action == policy.ActionSetFilamentDefinition {
		return filamentErrorHint(err)
	}
	switch err.Code {
	case policy.CodeForbidden:
		return "Only the user can allow control, and this server has no tool for it: ask them to run `creality_k2_mcp printers control on <printer id>` (or turn control on in the install wizard or TUI), then retry."
	case policy.CodeInvalidInput:
		return "Adjust the value to fit the stated range or configured band, or widen the band in settings, then call again."
	case policy.CodeConflict:
		return fmt.Sprintf("Call %s again with no confirm_token to request a fresh proposal.", err.Action)
	case policy.CodeNotFound:
		if err.Action != "" {
			return fmt.Sprintf("Call %s again with no confirm_token to request a fresh proposal, or check the identifier and try again.", err.Action)
		}
		return "Check the identifier and try again."
	case policy.CodeUnavailable:
		return "Call get_printer_status to see the current state and which actions are available now."
	default:
		return ""
	}
}

func tokenArg(t *string) string {
	if t == nil {
		return ""
	}
	return *t
}

// --- start_print ---

type startPrintInput struct {
	Printer      *string        `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Filename     string         `json:"filename" jsonschema:"the gcode file to print, exactly as listed by list_gcode_files"`
	Source       *string        `json:"source,omitempty" jsonschema:"with a CFS connected: cfs (default) or spool (side spool, not yet verified)"`
	SlotMap      []slotMapEntry `json:"slot_map,omitempty" jsonschema:"optional overrides of the automatic filament to slot mapping"`
	SelfTest     *bool          `json:"self_test,omitempty" jsonschema:"run the printer's pre-print self-test; default is the printer's own setting"`
	ConfirmToken *string        `json:"confirm_token,omitempty" jsonschema:"with a CFS connected: the token from the mapping proposal, passed with the same filename, source, slot_map and self_test"`
}

type slotMapEntry struct {
	Filament int    `json:"filament" jsonschema:"the filament's 0-based index in the file (0 is the first filament, tool T1A)"`
	Slot     string `json:"slot" jsonschema:"the slot to use for it: T1A to T4D"`
}

// canonicalSlotMap renders slot_map entries in the policy's canonical form,
// "T1A=T1C,T1B=T1D" (slicer tool = physical slot), sorted by filament index so
// the same overrides always bind the same token. Malformed entries are
// rejected here with a plain message.
func canonicalSlotMap(entries []slotMapEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	sorted := append([]slotMapEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Filament < sorted[j].Filament })
	parts := make([]string, 0, len(sorted))
	for i, e := range sorted {
		if e.Filament < 0 || e.Filament > 15 {
			return "", fmt.Errorf("slot_map filament %d is out of range (0 to 15)", e.Filament)
		}
		if i > 0 && sorted[i-1].Filament == e.Filament {
			return "", fmt.Errorf("slot_map names filament %d twice", e.Filament)
		}
		slot := strings.ToUpper(strings.TrimSpace(e.Slot))
		if len(slot) != 3 || slot[0] != 'T' || slot[1] < '1' || slot[1] > '4' || slot[2] < 'A' || slot[2] > 'D' {
			return "", fmt.Errorf("slot_map slot %q is not T1A to T4D", e.Slot)
		}
		parts = append(parts, fmt.Sprintf("T%d%c=%s", e.Filament/4+1, 'A'+e.Filament%4, slot))
	}
	return strings.Join(parts, ","), nil
}

func startPrintHandler(s *Server) func(context.Context, *mcp.CallToolRequest, startPrintInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in startPrintInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		slotMap, err := canonicalSlotMap(in.SlotMap)
		if err != nil {
			return render.ErrorResult(render.Error{
				Code: render.CodeInvalidInput, Message: err.Error(),
				Hint: "Give slot_map as a list of {filament, slot} entries, filament being the 0-based index in the file and slot T1A to T4D.",
			}), nil, nil
		}
		params := policy.Params{Filename: in.Filename, SlotMap: slotMap}
		if in.Source != nil {
			params.Source = strings.ToLower(strings.TrimSpace(*in.Source))
		}
		if in.SelfTest != nil {
			params.SelfTest, params.SelfTestExplicit = *in.SelfTest, true
		}
		res, err := s.executeControl(ctx, printer, policy.ActionStartPrint, params, tokenArg(in.ConfirmToken))
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- pause_print ---

type pausePrintInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
}

func pausePrintHandler(s *Server) func(context.Context, *mcp.CallToolRequest, pausePrintInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in pausePrintInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionPausePrint, policy.Params{}, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- resume_print ---

type resumePrintInput struct {
	Printer      *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	ConfirmToken *string `json:"confirm_token,omitempty" jsonschema:"the token from a prior call to this same tool; omit to get a fresh proposal"`
}

func resumePrintHandler(s *Server) func(context.Context, *mcp.CallToolRequest, resumePrintInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in resumePrintInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionResumePrint, policy.Params{}, tokenArg(in.ConfirmToken))
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- cancel_print ---

type cancelPrintInput struct {
	Printer      *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	ConfirmToken *string `json:"confirm_token,omitempty" jsonschema:"the token from a prior call to this same tool; omit to get a fresh proposal"`
}

func cancelPrintHandler(s *Server) func(context.Context, *mcp.CallToolRequest, cancelPrintInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in cancelPrintInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionCancelPrint, policy.Params{}, tokenArg(in.ConfirmToken))
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- set_nozzle_temperature ---

type setNozzleTemperatureInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	TargetC float64 `json:"target_c" jsonschema:"nozzle target temperature in Celsius"`
}

func setNozzleTemperatureHandler(s *Server) func(context.Context, *mcp.CallToolRequest, setNozzleTemperatureInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in setNozzleTemperatureInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionSetNozzleTemperature, policy.Params{TargetC: in.TargetC}, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- set_bed_temperature ---

type setBedTemperatureInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	TargetC float64 `json:"target_c" jsonschema:"bed target temperature in Celsius"`
}

func setBedTemperatureHandler(s *Server) func(context.Context, *mcp.CallToolRequest, setBedTemperatureInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in setBedTemperatureInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionSetBedTemperature, policy.Params{TargetC: in.TargetC}, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- set_fan_speed ---

// fanChannelValues lists domain.FanChannels (the canonical part/case/
// auxiliary order) as plain strings, for set_fan_speed's InputSchema enum.
func fanChannelValues() []string {
	out := make([]string, len(domain.FanChannels))
	for i, c := range domain.FanChannels {
		out[i] = string(c)
	}
	return out
}

type setFanSpeedInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Fan     string  `json:"fan" jsonschema:"which fan channel to set: part, case or auxiliary"`
	Percent float64 `json:"percent" jsonschema:"requested fan speed, 0-100"`
}

func setFanSpeedHandler(s *Server) func(context.Context, *mcp.CallToolRequest, setFanSpeedInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in setFanSpeedInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		params := policy.Params{Fan: domain.FanChannel(strings.ToLower(strings.TrimSpace(in.Fan))), FanPercent: in.Percent}
		res, err := s.executeControl(ctx, printer, policy.ActionSetFanSpeed, params, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- set_speed_factor ---

type setSpeedFactorInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Percent float64 `json:"percent" jsonschema:"requested speed factor as a percent, e.g. 100 for unchanged"`
}

func setSpeedFactorHandler(s *Server) func(context.Context, *mcp.CallToolRequest, setSpeedFactorInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in setSpeedFactorInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionSetSpeedFactor, policy.Params{Percent: in.Percent}, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- set_flow_factor ---

type setFlowFactorInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Percent float64 `json:"percent" jsonschema:"requested flow factor as a percent, e.g. 100 for unchanged"`
}

func setFlowFactorHandler(s *Server) func(context.Context, *mcp.CallToolRequest, setFlowFactorInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in setFlowFactorInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionSetFlowFactor, policy.Params{Percent: in.Percent}, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- set_light ---

type setLightInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	On      bool    `json:"on" jsonschema:"true to turn the chamber light on, false to turn it off"`
}

func setLightHandler(s *Server) func(context.Context, *mcp.CallToolRequest, setLightInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in setLightInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionSetLight, policy.Params{On: in.On}, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// --- exclude_object ---

type excludeObjectInput struct {
	Printer      *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	ObjectName   string  `json:"object_name" jsonschema:"the object to exclude, matched case insensitively against get_current_job's object list"`
	ConfirmToken *string `json:"confirm_token,omitempty" jsonschema:"the token from a prior call to this same tool; omit to get a fresh proposal"`
}

func excludeObjectHandler(s *Server) func(context.Context, *mcp.CallToolRequest, excludeObjectInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in excludeObjectInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		params := policy.Params{ObjectName: in.ObjectName}
		res, err := s.executeControl(ctx, printer, policy.ActionExcludeObject, params, tokenArg(in.ConfirmToken))
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}

// filamentErrorHint gives set_filament_definition hints that fit it: it has no
// proposal token and no configured bands, and the tools that explain a refusal
// are get_filaments (per-slot editable and why_not) and list_filament_catalog.
func filamentErrorHint(err *policy.Error) string {
	switch err.Code {
	case policy.CodeForbidden:
		return "Only the user can allow control, and this server has no tool for it: ask them to run `creality_k2_mcp printers control on <printer id>` (or turn control on in the install wizard or TUI), then retry."
	case policy.CodeInvalidInput:
		return "Fix the slot (T1A to T4D), the material (an id or exact name from list_filament_catalog) or the colour (#rrggbb), then call set_filament_definition again."
	case policy.CodeNotFound:
		return "Check the material with list_filament_catalog (an id or an exact name), then call set_filament_definition again."
	case policy.CodeConflict:
		return "Nothing was written. Call get_filaments to see the slots as they are now, then call set_filament_definition again if the edit still makes sense."
	case policy.CodeUnavailable:
		return "Call get_filaments: it shows each slot's editable and why_not, and edit_blocked says whether the printer's current state allows an edit at all; get_printer_status shows the state itself."
	default:
		return ""
	}
}
