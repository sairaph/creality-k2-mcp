package mcpserver

import (
	"context"
	"errors"
	"fmt"
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
			"the flow factor to 100% first if it was not already (the previous value is restored automatically " +
			"if the print does not reach the printing state). Because nothing the server can read confirms the " +
			"bed is actually clear, the AI must ask the user to confirm the bed is clear before calling this " +
			"tool, and only call it after that confirmation; this is not enforced by a token, it is the caller's " +
			"responsibility. Sends immediately with no confirm_token. Related: get_printer_status to confirm the " +
			"printer is idle first, pause_print and cancel_print once printing.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, startPrintHandler(s))

	registerTool(s, domain.ToolInfo{Name: "pause_print", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "pause_print",
		Description: "Pauses the current print. Only available while printing (not during START_PRINT's own " +
			"prepare/heat/home sequence, where pausing is unverified and blocked). The printer's PAUSE macro " +
			"drops the nozzle target to 140 C and, if the toolhead is homed, lifts it, moves to the purge/clean " +
			"position, wipes the nozzle and parks; the part and auxiliary fans turn off. Sends immediately with " +
			"no confirm_token. Related: resume_print to continue (its proposal shows what will be restored), " +
			"cancel_print to stop the job instead, get_printer_status to see the paused state.",
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
			"see the stored targets beforehand, pause_print, cancel_print to stop instead.",
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
			"check progress before deciding.",
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
			"for the current target and configured band, set_bed_temperature.",
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
			"Related: get_printer_status for the current fan speeds.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
		InputSchema: withEnum(inputSchema[setFanSpeedInput](), "fan", fanChannelValues()...),
	}, setFanSpeedHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_speed_factor", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_speed_factor",
		Description: "Sets the print speed factor as a percent (Moonraker M220), only available while printing, " +
			"bounded to the configured band (default 50-150%; widen it in settings for a larger change). This is " +
			"runtime only: both START_PRINT and END_PRINT reset it. Sends immediately with no confirm_token. " +
			"Related: set_flow_factor, get_printer_status for the current factor.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true},
	}, setSpeedFactorHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_flow_factor", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_flow_factor",
		Description: "Sets the extrusion flow factor as a percent (Moonraker M221), only available while " +
			"printing, bounded to the configured band (default 90-110%; widen it in settings for a larger " +
			"change). Unlike the speed factor, Creality's own macros do not reset this at the end of a print; " +
			"start_print resets it defensively to 100% before starting a new one, so a value left over from a " +
			"previous print never carries into the next. Sends immediately with no confirm_token. Related: " +
			"set_speed_factor, get_printer_status for the current factor.",
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
			"excluded.",
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
}

func (f *controlFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

// controlFrontFrom renders a policy.Result into controlFront. The embedded
// StateBlock is Before for a still-unsent proposal (After is empty on a
// Proposed Result per its own doc comment) and After once a write was
// actually attempted, so a reader always sees the most current state in the
// conventional top-level fields as well as both blocks explicitly.
func controlFrontFrom(res policy.Result) *controlFront {
	before := res.Before
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

// controlBody renders the Markdown body every control tool result shares:
// what happened (or what is proposed) and the exact next call, per
// dev_docs/safety-architecture.md section 5.
func controlBody(res policy.Result) string {
	var b strings.Builder
	if res.Proposed {
		fmt.Fprintf(&b, "Proposal for %s on %s: nothing has been sent to the printer yet.\n\n", res.Action, res.Printer.Name)
		if len(res.Effects) > 0 {
			b.WriteString("This will:\n")
			for _, e := range res.Effects {
				fmt.Fprintf(&b, "- %s\n", e)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Call %s again with confirm_token: %q to execute it. The token expires at %s; if the "+
			"printer's state changes before then it is invalidated and this call must be repeated for a fresh "+
			"proposal.", res.Action, res.Token, res.ExpiresAt.UTC().Format(time.RFC3339))
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
		fmt.Fprintf(&b, "%s was sent to %s and accepted, but the expected result was not observed within the "+
			"settle timeout. Call get_printer_status to check the current state before deciding whether to "+
			"retry; a lost confirmation does not necessarily mean the write failed.", res.Action, res.Printer.Name)
	default:
		fmt.Fprintf(&b, "%s was sent to %s.", res.Action, res.Printer.Name)
	}
	if len(res.Effects) > 0 {
		b.WriteString("\n\nDisclosed effects:\n")
		for _, e := range res.Effects {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}
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
	switch err.Code {
	case policy.CodeForbidden:
		return "Enable allow_control for this printer in the registry, or in the install wizard/TUI settings, then retry."
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
	Printer  *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Filename string  `json:"filename" jsonschema:"the gcode file to print, exactly as listed by list_gcode_files"`
}

func startPrintHandler(s *Server) func(context.Context, *mcp.CallToolRequest, startPrintInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in startPrintInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionStartPrint, policy.Params{Filename: in.Filename}, "")
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
