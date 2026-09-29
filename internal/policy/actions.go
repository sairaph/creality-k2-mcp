package policy

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// pendingKindFor maps an action to the printerstate.PendingKind its write
// occupies while in flight. Only the four lifecycle actions have one:
// 11-state-model.md section 1.1's transition rows (pausing, resuming,
// cancelling) and start_print's own "printing within N s" wait are the only
// writes DeriveActivityState needs to see as a pending transition; a
// setpoint change has no transition sub-state of its own.
func pendingKindFor(name ActionName) printerstate.PendingKind {
	switch name {
	case ActionStartPrint:
		return printerstate.PendingStart
	case ActionPausePrint:
		return printerstate.PendingPause
	case ActionResumePrint:
		return printerstate.PendingResume
	case ActionCancelPrint:
		return printerstate.PendingCancel
	default:
		return printerstate.PendingNone
	}
}

// floatEqual compares two Celsius/percent values with a small tolerance,
// since a value round-tripped through Klipper's own float formatting is not
// always bit-identical.
func floatEqual(a, b float64) bool { return math.Abs(a-b) < 0.5 }

// floatWithin reports whether a and b are within tolerance of each other,
// used for the fan percent read-back (M106's [1,255] scaling rounds).
func floatWithin(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }

func formatTemp(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// fanPin returns the OutputPin a fan channel reads back from, or nil for an
// unrecognised channel.
func fanPin(snap printerstate.Snapshot, channel domain.FanChannel) *moonraker.OutputPin {
	switch channel {
	case domain.FanPart:
		return snap.Fan0
	case domain.FanCase:
		return snap.Fan1
	case domain.FanAuxiliary:
		return snap.Fan2
	default:
		return nil
	}
}

// listedContains reports whether name is in a gcodes-root listing.
func listedContains(files []moonraker.GCodeFile, name string) bool {
	for _, f := range files {
		if f.Path == name {
			return true
		}
	}
	return false
}

// idleHeatArmFor builds the D2 idle-heat arm request to report on Result
// for a successful temperature write made while idle with a nonzero target:
// the actual arm call already happened, synchronously, inside
// armIdleHeatWatchdog before the heater command was sent
// (dev_docs/safety-architecture.md section 10 D2); this only reconstructs
// what was armed, for display. It is nil for every other action or bucket,
// and for a target of 0 (turning the heater off needs no watchdog).
func idleHeatArmFor(name ActionName, params Params, derived printerstate.Derived, settings domain.Settings, identity string, printer domain.Printer) *IdleHeatArmRequest {
	if derived.Bucket != printerstate.BucketI || params.TargetC == 0 {
		return nil
	}
	switch name {
	case ActionSetNozzleTemperature:
		return &IdleHeatArmRequest{Identity: identity, Heater: "extruder", TargetC: params.TargetC, ArmMinutes: settings.IdleHeatMinutes,
			Host: printer.Host, MoonrakerPort: printer.MoonrakerPort, APIKey: printer.APIKey}
	case ActionSetBedTemperature:
		return &IdleHeatArmRequest{Identity: identity, Heater: "heater_bed", TargetC: params.TargetC, ArmMinutes: settings.IdleHeatMinutes,
			Host: printer.Host, MoonrakerPort: printer.MoonrakerPort, APIKey: printer.APIKey}
	default:
		return nil
	}
}

// armIdleHeatWatchdog is D2's enforcement point, called from checkTemperature
// before Execute ever sends the heater command: a temperature write in the
// idle bucket with a nonzero target must arm Deps.Watchdog synchronously
// first, refusing outright if the daemon is not wired up, not alive, or
// rejects the arm request. A target of 0 needs no watchdog (there is
// nothing left to turn off automatically), and a mid-print write never
// calls this at all (dev_docs/safety-architecture.md section 10 D2).
func armIdleHeatWatchdog(ctx context.Context, deps Deps, identity string, heater string, targetC float64, settings domain.Settings, printer domain.Printer) *Error {
	if targetC == 0 {
		return nil
	}
	if deps.Watchdog == nil || !deps.Watchdog.Alive(ctx) {
		return &Error{
			Code: CodeUnavailable,
			Message: "idle-heat watchdog is not running; the background daemon (`" + domain.BinaryName +
				" camera serve`) is normally started automatically - run doctor to check it before heating while idle",
		}
	}
	req := IdleHeatArmRequest{
		Identity: identity, Heater: heater, TargetC: targetC, ArmMinutes: settings.IdleHeatMinutes,
		Host: printer.Host, MoonrakerPort: printer.MoonrakerPort, APIKey: printer.APIKey,
	}
	if err := deps.Watchdog.Arm(ctx, req); err != nil {
		return &Error{Code: CodeUnavailable, Message: "idle-heat watchdog refused to arm: " + err.Error() + "; refusing to heat without it"}
	}
	return nil
}

// evaluateParams is the dynamic half of the Action table: parameter rules
// and extra fresh preconditions that depend on live state
// (dev_docs/safety-architecture.md section 3.2's ParamRules(state)). It
// returns the effective Confirmation for this call (only
// upload_gcode_file's ConfirmationConditional ever differs from the static
// spec.Confirmation) or a typed *Error explaining exactly what was wrong.
func (p *Policy) evaluateParams(ctx context.Context, deps Deps, printer domain.Printer, identity string, spec actionSpec, snap printerstate.Snapshot, derived printerstate.Derived, params Params, settings domain.Settings) (Confirmation, *Error) {
	switch spec.Name {

	case ActionStartPrint:
		if params.Filename == "" {
			return spec.Confirmation, &Error{Code: CodeInvalidInput, Message: "filename must not be empty"}
		}
		if p.isUploading(identity, params.Filename) {
			return spec.Confirmation, &Error{Code: CodeConflict, Message: "this file is still being uploaded by this server"}
		}
		files, err := deps.Moonraker.List(ctx)
		if err != nil {
			return spec.Confirmation, &Error{Code: CodeUnavailable, Message: "could not list gcodes files: " + err.Error()}
		}
		if !listedContains(files, params.Filename) {
			return spec.Confirmation, &Error{Code: CodeNotFound, Message: "file " + params.Filename + " does not exist in the gcodes root"}
		}
		if derived.CFSConnected {
			// Any CFS-connected start goes through the mapping proposal (V3).
			return ConfirmationProposalToken, nil
		}
		return spec.Confirmation, nil

	case ActionPausePrint:
		return spec.Confirmation, nil

	case ActionCancelPrint:
		// Cancel during the print-start self-test: print_stats has no job yet, so
		// Moonraker's cancel is not known to stop it. Refused until the 9999
		// stop is verified on a real printer (plan 8a.1).
		if inStartWindow(snap, derived) && !stopDuringStartVerified {
			return spec.Confirmation, &Error{Code: CodeUnavailable, Message: startWindowCancelRefusal}
		}
		return spec.Confirmation, nil

	case ActionResumePrint:
		return spec.Confirmation, nil

	case ActionSetNozzleTemperature:
		return spec.Confirmation, checkTemperature(ctx, deps, printer, identity, snap, derived, params, settings, "extruder")

	case ActionSetBedTemperature:
		return spec.Confirmation, checkTemperature(ctx, deps, printer, identity, snap, derived, params, settings, "heater_bed")

	case ActionSetFanSpeed:
		return spec.Confirmation, checkFan(snap, derived, params, settings)

	case ActionSetSpeedPreset:
		return spec.Confirmation, checkPresetName(params.Preset)

	case ActionSetSpeedFactor:
		if !domain.WithinRange(params.Percent, settings.Bands.SpeedFactorMinPercent, settings.Bands.SpeedFactorMaxPercent) {
			return spec.Confirmation, &Error{Code: CodeInvalidInput, Message: bandMessage("speed factor", params.Percent, settings.Bands.SpeedFactorMinPercent, settings.Bands.SpeedFactorMaxPercent)}
		}
		return spec.Confirmation, nil

	case ActionSetFlowFactor:
		if !domain.WithinRange(params.Percent, settings.Bands.FlowFactorMinPercent, settings.Bands.FlowFactorMaxPercent) {
			return spec.Confirmation, &Error{Code: CodeInvalidInput, Message: bandMessage("flow factor", params.Percent, settings.Bands.FlowFactorMinPercent, settings.Bands.FlowFactorMaxPercent)}
		}
		return spec.Confirmation, nil

	case ActionSetLight:
		return spec.Confirmation, nil

	case ActionExcludeObject:
		return spec.Confirmation, checkExcludeObject(snap, params)

	case ActionUploadGCodeFile:
		if err := p.startWindowFileGuard(identity, snap, derived, params.Filename, "upload or overwrite"); err != nil {
			return spec.Confirmation, err
		}
		return p.evaluateUpload(ctx, deps, spec, snap, params)

	case ActionDeleteGCodeFile:
		if err := p.startWindowFileGuard(identity, snap, derived, params.Filename, "delete"); err != nil {
			return spec.Confirmation, err
		}
		return spec.Confirmation, checkDelete(snap, params)

	default:
		return spec.Confirmation, &Error{Code: CodeInvalidInput, Message: "unknown action"}
	}
}

// checkTemperature implements D1's two-tier ceiling and mid-print band, and
// D2/D3.6's idle-vs-paused split. heater is "extruder" or "heater_bed". In
// the idle bucket it also enforces D2's watchdog (armIdleHeatWatchdog)
// before returning, so a caller that gets a nil error back from this
// function for an idle-bucket write has already armed the watchdog.
func checkTemperature(ctx context.Context, deps Deps, printer domain.Printer, identity string, snap printerstate.Snapshot, derived printerstate.Derived, params Params, settings domain.Settings, heater string) *Error {
	var liveCap domain.LiveCap
	var currentTarget *float64
	var band float64
	if heater == "extruder" {
		if snap.ProductParam != nil {
			liveCap = liveTempCap(snap.ProductParam.NozzleTemp)
		}
		if snap.Extruder != nil {
			currentTarget = snap.Extruder.Target
		}
		band = settings.Bands.NozzleBandC
	} else {
		if snap.ProductParam != nil {
			liveCap = liveTempCap(snap.ProductParam.BedTemp)
		}
		if snap.HeaterBed != nil {
			currentTarget = snap.HeaterBed.Target
		}
		band = settings.Bands.BedBandC
	}

	limit, err := domain.EffectiveTemperatureLimit(ceilingFor(heater), liveCap, nil)
	if err != nil {
		return &Error{Code: CodeUnavailable, Message: "live temperature cap (product_param) has not been read; refusing to set " + heater + " temperature"}
	}
	if params.TargetC < 0 || params.TargetC > limit {
		return &Error{Code: CodeInvalidInput, Message: "target must be between 0 and " + formatTemp(limit) + " C (effective limit for " + heater + ")"}
	}

	switch derived.Bucket {
	case printerstate.BucketP:
		if currentTarget == nil {
			return &Error{Code: CodeUnavailable, Message: "current " + heater + " target could not be read; cannot apply the mid-print band"}
		}
		if !domain.WithinBand(*currentTarget, params.TargetC, band) {
			return &Error{Code: CodeInvalidInput, Message: bandMessage(heater+" temperature", params.TargetC, *currentTarget-band, *currentTarget+band)}
		}
	case printerstate.BucketI:
		// D2: idle heating allowed, no band (there is no "current print" to
		// protect); the watchdog is enforced here, synchronously, before
		// Execute ever sends the heater command.
		if err := armIdleHeatWatchdog(ctx, deps, identity, heater, params.TargetC, settings, printer); err != nil {
			return err
		}
	}
	return nil
}

func ceilingFor(heater string) float64 {
	if heater == "extruder" {
		return domain.NozzleCeilingC
	}
	return domain.BedCeilingC
}

func bandMessage(what string, value, min, max float64) string {
	return what + " " + strconv.FormatFloat(value, 'f', -1, 64) +
		" is outside the configured band " + strconv.FormatFloat(min, 'f', -1, 64) +
		"-" + strconv.FormatFloat(max, 'f', -1, 64) +
		"; widen the band in settings or make a smaller change"
}

// checkFan implements D1's part-fan floor (mid-print only) and 0-100 range.
func checkFan(snap printerstate.Snapshot, derived printerstate.Derived, params Params, settings domain.Settings) *Error {
	if _, ok := domain.FanSpecFor(params.Fan); !ok {
		return &Error{Code: CodeInvalidInput, Message: "unknown fan channel"}
	}
	if params.FanPercent < 0 || params.FanPercent > 100 {
		return &Error{Code: CodeInvalidInput, Message: "fan percent must be 0-100"}
	}
	if derived.Bucket == printerstate.BucketP && params.Fan == domain.FanPart {
		pin := fanPin(snap, params.Fan)
		spec, _ := domain.FanSpecFor(params.Fan)
		if pin == nil || pin.Value == nil {
			return &Error{Code: CodeUnavailable, Message: "current part fan speed could not be read; cannot apply the mid-print floor"}
		}
		current := domain.ReportedFanPercent(*pin.Value, spec.MinValue)
		if !domain.PartFanAboveFloor(current, params.FanPercent, settings.Bands.PartFanMinPercentOfCurrent) {
			floor := current * settings.Bands.PartFanMinPercentOfCurrent / 100
			return &Error{Code: CodeInvalidInput, Message: "part fan " + strconv.FormatFloat(params.FanPercent, 'f', -1, 64) +
				"% is below the configured floor of " + strconv.FormatFloat(settings.Bands.PartFanMinPercentOfCurrent, 'f', -1, 64) +
				"% of the current " + strconv.FormatFloat(current, 'f', -1, 64) + "% (floor " + strconv.FormatFloat(floor, 'f', -1, 64) + "%); widen the band in settings or make a smaller change"}
		}
	}
	return nil
}

// checkExcludeObject implements 10-hazard-analysis.md 2.5/3.10: case
// insensitive lookup, refuse an already-excluded object, refuse excluding
// the last remaining non-excluded object.
func checkExcludeObject(snap printerstate.Snapshot, params Params) *Error {
	if params.ObjectName == "" {
		return &Error{Code: CodeInvalidInput, Message: "object name must not be empty"}
	}
	if snap.ExcludeObject == nil {
		return &Error{Code: CodeUnavailable, Message: "exclude_object was not reported by this snapshot"}
	}
	stored, ok := findObject(snap.ExcludeObject.Objects, params.ObjectName)
	if !ok {
		return &Error{Code: CodeNotFound, Message: "no object named " + params.ObjectName}
	}
	if isExcluded(snap.ExcludeObject.ExcludedObjects, stored) {
		return &Error{Code: CodeInvalidInput, Message: "object " + stored + " is already excluded"}
	}
	remaining := 0
	for _, obj := range snap.ExcludeObject.Objects {
		name, _ := obj["name"].(string)
		if name == "" {
			continue
		}
		if !isExcluded(snap.ExcludeObject.ExcludedObjects, name) {
			remaining++
		}
	}
	if remaining <= 1 {
		return &Error{Code: CodeInvalidInput, Message: "excluding " + stored + " would leave no non-excluded objects printing; refused"}
	}
	return nil
}

// evaluateUpload implements upload's two rules: refuse outright when the
// target is the current print file (10-hazard-analysis.md 2.3), and require
// a proposal_token only when the target already exists on disk (D3's
// "upload overwrite").
func (p *Policy) evaluateUpload(ctx context.Context, deps Deps, spec actionSpec, snap printerstate.Snapshot, params Params) (Confirmation, *Error) {
	if params.Filename == "" || params.LocalPath == "" {
		return spec.Confirmation, &Error{Code: CodeInvalidInput, Message: "filename and local path must not be empty"}
	}
	if snap.PrintStats != nil && snap.PrintStats.Filename == params.Filename {
		return spec.Confirmation, &Error{Code: CodeConflict, Message: "cannot upload over " + params.Filename + ": it is the current print file"}
	}
	files, err := deps.Moonraker.List(ctx)
	if err != nil {
		return spec.Confirmation, &Error{Code: CodeUnavailable, Message: "could not list gcodes files: " + err.Error()}
	}
	if listedContains(files, params.Filename) {
		return ConfirmationProposalToken, nil
	}
	return ConfirmationNone, nil
}

// checkDelete implements 10-hazard-analysis.md 2.3: never delete the
// current print file, pre-checked here rather than relying solely on
// Moonraker's own 403.
func checkDelete(snap printerstate.Snapshot, params Params) *Error {
	if params.Filename == "" {
		return &Error{Code: CodeInvalidInput, Message: "filename must not be empty"}
	}
	if snap.PrintStats != nil && snap.PrintStats.Filename == params.Filename {
		return &Error{Code: CodeConflict, Message: "cannot delete " + params.Filename + ": it is the current print file"}
	}
	return nil
}

// settleFuncFor returns the settle condition for every action whose settle
// check is a plain function of the polled snapshot (everything except
// set_light, upload and delete, which have their own send functions with
// inline settle logic since they do not settle via the printerstate
// snapshot alone).
func settleFuncFor(name ActionName, params Params) func(printerstate.Snapshot, printerstate.Derived) bool {
	switch name {
	case ActionPausePrint:
		return func(_ printerstate.Snapshot, d printerstate.Derived) bool { return d.State == printerstate.StatePaused }
	case ActionResumePrint:
		return func(_ printerstate.Snapshot, d printerstate.Derived) bool {
			return d.State == printerstate.StatePrinting
		}
	case ActionCancelPrint:
		return func(_ printerstate.Snapshot, d printerstate.Derived) bool {
			return d.State == printerstate.StateCancelled
		}
	case ActionSetNozzleTemperature:
		return func(s printerstate.Snapshot, _ printerstate.Derived) bool {
			return s.Extruder != nil && s.Extruder.Target != nil && floatEqual(*s.Extruder.Target, params.TargetC)
		}
	case ActionSetBedTemperature:
		return func(s printerstate.Snapshot, _ printerstate.Derived) bool {
			return s.HeaterBed != nil && s.HeaterBed.Target != nil && floatEqual(*s.HeaterBed.Target, params.TargetC)
		}
	case ActionSetFanSpeed:
		return func(s printerstate.Snapshot, _ printerstate.Derived) bool {
			pin := fanPin(s, params.Fan)
			spec, ok := domain.FanSpecFor(params.Fan)
			if pin == nil || pin.Value == nil || !ok {
				return false
			}
			got := domain.ReportedFanPercent(*pin.Value, spec.MinValue)
			return floatWithin(got, params.FanPercent, 2)
		}
	case ActionSetSpeedFactor:
		return func(s printerstate.Snapshot, _ printerstate.Derived) bool {
			return s.GCodeMove != nil && s.GCodeMove.SpeedFactor != nil && floatEqual(*s.GCodeMove.SpeedFactor*100, params.Percent)
		}
	case ActionSetFlowFactor:
		return func(s printerstate.Snapshot, _ printerstate.Derived) bool {
			return s.GCodeMove != nil && s.GCodeMove.ExtrudeFactor != nil && floatEqual(*s.GCodeMove.ExtrudeFactor*100, params.Percent)
		}
	case ActionExcludeObject:
		return func(s printerstate.Snapshot, _ printerstate.Derived) bool {
			return s.ExcludeObject != nil && isExcluded(s.ExcludeObject.ExcludedObjects, params.ObjectName)
		}
	default:
		return func(printerstate.Snapshot, printerstate.Derived) bool { return true }
	}
}

// sendPolled handles every action whose settle condition is a snapshot poll
// (pause, resume, cancel, the four setpoint actions, exclude_object).
// identity is the already-verified identity Execute resolved
// (resolveExecuteIdentity), threaded through rather than recomputed here
// (review backlog item 31), since it is what idleHeatArmFor must report to
// match what armIdleHeatWatchdog actually armed the watchdog under.
func (p *Policy) sendPolled(ctx context.Context, deps Deps, printer domain.Printer, identity string, settings domain.Settings, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)

	pendingKind := pendingKindFor(spec.Name)
	if pendingKind != printerstate.PendingNone {
		locks.pl.setPending(&printerstate.PendingAction{Kind: pendingKind, IssuedAt: time.Now(), Timeout: spec.SettleTimeout})
		defer locks.pl.setPending(nil)
	}

	// While Silent is on the firmware caps every fan at half its range (M106,
	// gcode_macro.cfg), so a fan request above 50% is applied as 50%: settle
	// against the capped value and say so, instead of a false unconfirmed
	// (plan 2a.8).
	settleParams := params
	var extraEffects []string
	if spec.Name == ActionSetFanSpeed && derived.Qmode == printerstate.QmodeOn {
		extraEffects = append(extraEffects, "Silent mode is on: it caps all fans (part, case, auxiliary) at half their range (50%), so a request above 50% is applied as 50%")
		if params.FanPercent > 50 {
			settleParams.FanPercent = 50
		}
	} else if spec.Name == ActionSetFanSpeed && derived.Qmode == printerstate.QmodeUnknown && params.FanPercent > 50 {
		extraEffects = append(extraEffects, "the Silent mode state could not be read: if Silent is on, the firmware caps every fan at 50%, and this request above 50% will not read back as requested")
	}

	// A possibly-queued earlier write of the same setting (queued.go) is still
	// ahead of this one in Moonraker's queue: say so (this command runs after it).
	kind := settingKind(spec.Name, params)
	if isTemplateAction(spec.Name) {
		if q := locks.pl.outstandingQueued(kind, snap); q != nil {
			extraEffects = append(extraEffects, queuedNote(q))
		}
	}

	sendErr := dispatchSend(ctx, deps, spec.Name, params)
	accepted := sendErr == nil
	if sendErr == nil && isTemplateAction(spec.Name) {
		locks.pl.clearQueuedKind(kind) // FIFO: an answered write proves the earlier one ran
	}
	if spec.Name == ActionResumePrint || spec.Name == ActionCancelPrint {
		// The pause record is single-use: once a resume or cancel was sent it
		// must never match a later pause (plan 8a.5).
		locks.pl.setPauseRec(nil)
	}

	settleFn := settleFuncFor(spec.Name, settleParams)
	afterSnap, afterDerived, confirmed := p.pollUntilSettle(ctx, deps, printer, locks, pendingKind, spec.SettleTimeout, settleFn)
	// A template write that errored without an HTTP answer (a timeout) may be
	// queued behind a running macro and run later. It is never retried; the settle
	// poll decides. Confirmed after such an error means the printer ran it when it
	// got to it: accepted, unless the value already matched before the send (then
	// whether it ran is not known).
	if isTemplateAction(spec.Name) && deliveryUnknown(sendErr) {
		switch {
		case confirmed && !settleFn(snap, derived):
			accepted = true
			extraEffects = append(extraEffects, queuedRanNote)
		case confirmed:
			// The value already held before the send: the read cannot show that this
			// command ran.
			extraEffects = append(extraEffects, neutralConfirmNote)
		default:
			extraEffects = append(extraEffects, queuedPendingNote)
			// Remember it: a later call must not skip a write while this one is queued.
			settleCopy := settleParams
			locks.pl.setQueuedWrite(&queuedWrite{
				kind: kind, action: spec.Name, target: writeTarget(spec.Name, params), issuedAt: time.Now(),
				job: printerstate.JobIdentityFrom(snap),
				applied: func(s printerstate.Snapshot) bool {
					return settleFuncFor(spec.Name, settleCopy)(s, printerstate.Derived{})
				},
			})
		}
	}
	// A cancel whose HTTP call failed with a transport error or timeout (Moonraker
	// answers only after the macro) but whose effect the settle poll then confirmed
	// did reach the printer: count it as accepted. Never for an HTTP status
	// rejection, and never for the setpoint actions, whose target may already hold
	// before the send (final review M3).
	if spec.Name == ActionCancelPrint && confirmed && isTransportError(sendErr) {
		accepted = true
	}

	result := Result{
		Action:   spec.Name,
		Accepted: accepted,
		Effect:   effectString(confirmed),
		Effects:  append(withCFSNote(spec.Effects, spec.Name, derived), extraEffects...),
		Commands: spec.Commands,
		Before:   before,
		After:    printerstate.BuildStateBlock(afterSnap, afterDerived, nil),
		Printer:  printer,
		Job:      printerstate.JobIdentityFrom(afterSnap),
	}
	if accepted {
		if arm := idleHeatArmFor(spec.Name, params, derived, settings, identity, printer); arm != nil {
			result.IdleHeatArm = arm
		}
	}
	return result, nil
}

// isTemplateAction lists the actions sent as one Moonraker gcode-script template.
func isTemplateAction(name ActionName) bool {
	switch name {
	case ActionSetNozzleTemperature, ActionSetBedTemperature, ActionSetFanSpeed, ActionSetSpeedFactor,
		ActionSetFlowFactor, ActionExcludeObject:
		return true
	}
	return false
}

// dispatchSend performs the one write call an action makes, for every
// action other than start_print, set_light, upload and delete (which have
// their own send functions in execute_special.go).
func dispatchSend(ctx context.Context, deps Deps, name ActionName, params Params) error {
	switch name {
	case ActionPausePrint:
		return deps.Moonraker.PrintPause(ctx)
	case ActionResumePrint:
		return deps.Moonraker.PrintResume(ctx)
	case ActionCancelPrint:
		return deps.Moonraker.PrintCancel(ctx)
	case ActionSetNozzleTemperature:
		return deps.Moonraker.RunTemplate(ctx, moonraker.TemplateSetHeaterTemperature, map[string]string{"heater": "extruder", "target": formatTemp(params.TargetC)})
	case ActionSetBedTemperature:
		return deps.Moonraker.RunTemplate(ctx, moonraker.TemplateSetHeaterTemperature, map[string]string{"heater": "heater_bed", "target": formatTemp(params.TargetC)})
	case ActionSetFanSpeed:
		spec, _ := domain.FanSpecFor(params.Fan)
		return deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM106, map[string]string{"fan": strconv.Itoa(spec.PParameter), "speed": strconv.Itoa(domain.PercentToS(params.FanPercent))})
	case ActionSetSpeedFactor:
		return deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM220, map[string]string{"percent": strconv.Itoa(int(params.Percent))})
	case ActionSetFlowFactor:
		return deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM221, map[string]string{"percent": strconv.Itoa(int(params.Percent))})
	case ActionExcludeObject:
		return deps.Moonraker.RunTemplate(ctx, moonraker.TemplateExcludeObject, map[string]string{"name": params.ObjectName})
	default:
		return nil
	}
}
