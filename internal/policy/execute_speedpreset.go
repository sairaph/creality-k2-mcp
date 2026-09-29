package policy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// set_speed_preset (plan-v0.3.0.md sections 0-2 as revised by 2a): Creality's
// four speed presets. The factor is ALWAYS sent through the allowlisted
// Moonraker M220 template; port 9999 carries only speedMode (Silent on or off).
//
//	silent                        9999 {"speedMode":1}
//	stable/standard/ultrafast     Silent on: 9999 {"speedMode":0}, then M220 S<N>
//	                              Silent off: M220 S<N> only
//
// with N = 50, 100, 125. 9999 setFeedratePct is never sent.

var (
	// speedSettleCap bounds the whole read-back window and speedHold is how long
	// the target must keep holding inside it (plan 2a.5). Vars so tests can shrink
	// them; production never changes them.
	speedSettleCap = 3 * time.Second
	speedHold      = 1200 * time.Millisecond
)

// silentEffect is what entering Silent does and discloses, including the
// persistent-file write the owner decided to ship with (plan 2a, B2).
const silentEffect = "sets velocity 150 mm/s (a file that sets its own velocity overrides it) and clamps acceleration to 2500 mm/s2, " +
	"sets pressure advance 0.05, caps all three fans (part, case, auxiliary) to half their range and sets the speed factor to 50%, " +
	"until changed or the print ends. PERSISTENT WRITE: while printing, the firmware also writes creality/userdata/config/speed_mode.json " +
	"({\"speed_mode\":2}), a power-loss-resume hint that leaving Silent does not clear, so a later power-loss resume (even of another print) may come back in Silent"

// leaveSilentEffect is the honest description of leaving Silent (plan 2a.7).
const leaveSilentEffect = "leaving Silent restores the velocity, acceleration, corner velocity, pressure advance and fan values captured when Silent was entered; " +
	"after a CFS filament change these can be stale (for example the previous filament's pressure advance); " +
	"leaving Silent does not clear creality/userdata/config/speed_mode.json, which keeps its power-loss-resume hint"

const silentElsewhereNote = "Silent was entered elsewhere or is left over from an earlier print: this server has no record of entering it for this job, " +
	"so leaving it restores whatever values were captured then, which may belong to another print"

// StuckSilentText is the warning shown while Silent is on and the printer is
// not printing (get_printer_status guidance, start_print proposal and result;
// plan 2a.9). It is a warning, never a refusal.
const StuckSilentText = "Silent mode is still on while the printer is not printing (qmode_flag is 1): its acceleration clamp (2500 mm/s2, and velocity may be 150) " +
	"stays in force for the next print until Klipper restarts, and START_PRINT does not clear it. A firmware restart or a power cycle clears it. " +
	"This server cannot clear it: Silent's exit (Qmode_exit) does nothing outside a print"

// stuckSilentNotes returns the stuck-Silent warning when it applies: the
// printer is idle-like (bucket I) and the Silent flag is positively on.
func stuckSilentNotes(d printerstate.Derived) []string {
	if d.Qmode == printerstate.QmodeOn && d.Bucket == printerstate.BucketI {
		return []string{StuckSilentText}
	}
	return nil
}

// silentRecord is the policy session's record of entering Silent through this
// server: the job it was entered for (plan 2a.7). Not persisted.
type silentRecord struct {
	job *printerstate.JobIdentity
}

func (l *printerLock) setSilentRec(r *silentRecord) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.silentRec = r
}

func (l *printerLock) getSilentRec() *silentRecord {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return l.silentRec
}

// sameJob compares two job identities by filename, uuid and start time only:
// the history and 9999 print ids can appear later in the same job.
func sameJob(a, b *printerstate.JobIdentity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Filename == b.Filename && a.UUID == b.UUID && a.StartTime == b.StartTime
}

// speedPresetEffects lists the disclosed effects of one preset request.
func speedPresetEffects(preset string, silentOn bool) []string {
	if preset == printerstate.PresetSilent {
		return []string{silentEffect}
	}
	factor, _ := printerstate.PresetFactorPercent(preset)
	out := []string{fmt.Sprintf("the speed factor becomes %g%% (Moonraker M220, runtime only: START_PRINT and END_PRINT reset it)", factor)}
	if silentOn {
		out = append(out, leaveSilentEffect)
	}
	return out
}

// checkPresetName validates the requested preset.
func checkPresetName(name string) *Error {
	if _, ok := printerstate.PresetFactorPercent(name); !ok {
		return &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("preset %q is not one of silent, stable, standard, ultrafast", name)}
	}
	return nil
}

// checkPresetBand refuses a preset whose speed factor is outside the
// configured speed-factor band (plan S4, 2a.13). Silent and Stable are both
// 50%. Leaving Silent to any in-band non-Silent preset is always allowed; when
// Silent is on and the target is refused, the message says how to leave it.
func checkPresetBand(preset string, silentOn bool, settings domain.Settings) *Error {
	factor, _ := printerstate.PresetFactorPercent(preset)
	lo, hi := settings.Bands.SpeedFactorMinPercent, settings.Bands.SpeedFactorMaxPercent
	if domain.WithinRange(factor, lo, hi) {
		return nil
	}
	msg := fmt.Sprintf("preset %s sets the speed factor to %g%%, which is outside the configured band %g-%g%%; widen the band in settings", preset, factor, lo, hi)
	if silentOn {
		var ok []string
		for _, name := range []string{printerstate.PresetStable, printerstate.PresetStandard, printerstate.PresetUltrafast} {
			if f, _ := printerstate.PresetFactorPercent(name); domain.WithinRange(f, lo, hi) {
				ok = append(ok, name)
			}
		}
		if len(ok) > 0 {
			msg += ". Silent is on: leave it with " + strings.Join(ok, ", ")
		} else {
			msg += ". Silent is on and no other preset is inside the band: widen the band in settings, let the print finish, cancel it, or leave Silent on the printer screen"
		}
	}
	return &Error{Code: CodeInvalidInput, Message: msg}
}

// checkSilentGate is the Silent-related part of the gate (plan 2a.11 and M5):
// in bucket P, set_speed_factor is blocked while Silent is on or unknown,
// because leaving Silent restores the factor saved on entry and would silently
// discard the change; set_speed_preset needs a known Silent state to decide
// no_change and the entry condition. It lives in the gate so the actions list
// and Execute agree.
func checkSilentGate(spec actionSpec, derived printerstate.Derived) *Error {
	if derived.Bucket != printerstate.BucketP {
		return nil
	}
	switch spec.Name {
	case ActionSetSpeedFactor:
		switch derived.Qmode {
		case printerstate.QmodeOn:
			return &Error{Action: spec.Name, Code: CodeUnavailable, Message: "Silent mode is active: when it ends it restores the speed factor it saved on entry, which would silently discard this change; use set_speed_preset"}
		case printerstate.QmodeUnknown:
			return &Error{Action: spec.Name, Code: CodeUnavailable, Message: "Silent mode could not be read (custom_macro.qmode_flag and gcode_macro Qmode.flag are missing or disagree), and if it is active its end would silently discard a speed factor change; use set_speed_preset once the state can be read"}
		}
	case ActionSetSpeedPreset:
		if derived.Qmode == printerstate.QmodeUnknown {
			return &Error{Action: spec.Name, Code: CodeUnavailable, Message: "Silent mode could not be read (custom_macro.qmode_flag and gcode_macro Qmode.flag are missing or disagree), so the current preset and whether a change is needed cannot be decided"}
		}
	}
	return nil
}

// speedPresetMatches reports whether Moonraker shows the target: the Silent
// flag positively known and equal, and the speed factor at the preset's value.
func speedPresetMatches(snap printerstate.Snapshot, d printerstate.Derived, wantSilent bool, targetPct float64) bool {
	if d.Qmode == printerstate.QmodeUnknown || (d.Qmode == printerstate.QmodeOn) != wantSilent {
		return false
	}
	return snap.GCodeMove != nil && snap.GCodeMove.SpeedFactor != nil && floatEqual(*snap.GCodeMove.SpeedFactor*100, targetPct)
}

// presetReport builds the two-channel report from a snapshot.
func presetReport(preset string, snap printerstate.Snapshot, d printerstate.Derived) *SpeedPresetReport {
	r := &SpeedPresetReport{Preset: preset, MoonrakerSilent: d.Qmode.String()}
	if snap.GCodeMove != nil && snap.GCodeMove.SpeedFactor != nil {
		v := *snap.GCodeMove.SpeedFactor * 100
		r.MoonrakerFactorPct = &v
	}
	if snap.WS9999Reachable {
		if snap.WS9999.SpeedMode.Present {
			v := snap.WS9999.SpeedMode.Value
			r.SpeedMode9999 = &v
		}
		if snap.WS9999.CurFeedrate.Present {
			v := snap.WS9999.CurFeedrate.Value
			r.CurFeedratePct9999 = &v
		}
	}
	return r
}

// channelDisagreement describes how port 9999's readings differ from the
// target, or "" when it agrees or did not report (plan 2a.5).
func channelDisagreement(r *SpeedPresetReport, wantSilent bool, targetPct float64) string {
	var diffs []string
	if r.SpeedMode9999 != nil && (*r.SpeedMode9999 == 1) != wantSilent {
		diffs = append(diffs, fmt.Sprintf("speedMode=%d", *r.SpeedMode9999))
	}
	if r.CurFeedratePct9999 != nil && !floatEqual(float64(*r.CurFeedratePct9999), targetPct) {
		diffs = append(diffs, fmt.Sprintf("curFeedratePct=%d", *r.CurFeedratePct9999))
	}
	return strings.Join(diffs, ", ")
}

// recheckStillPrinting re-reads print_stats and pause_resume from Moonraker
// immediately before speedMode:1 is sent (plan 2a.6): a print that ended or
// paused since the snapshot could otherwise leave Silent stuck on.
func recheckStillPrinting(ctx context.Context, deps Deps) *Error {
	raw, err := deps.Moonraker.QueryObjects(ctx, map[string][]string{"print_stats": nil, "pause_resume": nil})
	if err != nil {
		return &Error{Code: CodeUnavailable, Message: "print_stats could not be re-read immediately before entering Silent: " + err.Error() + "; Silent was not entered"}
	}
	psRaw, ok1 := raw["print_stats"]
	prRaw, ok2 := raw["pause_resume"]
	if !ok1 || !ok2 {
		return &Error{Code: CodeUnavailable, Message: "print_stats or pause_resume was not reported when re-read immediately before entering Silent; Silent was not entered"}
	}
	ps, derr1 := moonraker.DecodePrintStats(psRaw)
	pr, derr2 := moonraker.DecodePauseResume(prRaw)
	if derr1 != nil || derr2 != nil || pr.IsPaused == nil {
		return &Error{Code: CodeUnavailable, Message: "print_stats or pause_resume could not be decoded when re-read immediately before entering Silent; Silent was not entered"}
	}
	if ps.State != "printing" || *pr.IsPaused {
		return &Error{Code: CodeUnavailable, Message: fmt.Sprintf("the job is no longer printing (print_stats %q, paused %v): Silent was not entered, because entering it outside a print could leave it stuck on", ps.State, *pr.IsPaused)}
	}
	return nil
}

// pollSpeedPreset re-snapshots (a fresh Take on a new 9999 connection, whose
// connect dump is the complete current state) until Moonraker has shown the
// target continuously for speedHold, all within speedSettleCap (plan 2a.5). The
// hold rule guards against the opposite race to the exit transient: the factor
// landing and then Qmode_exit's own M220 overwriting it.
func (p *Policy) pollSpeedPreset(ctx context.Context, deps Deps, printer domain.Printer, locks *acquiredLocks, wantSilent bool, targetPct float64) (printerstate.Snapshot, printerstate.Derived, bool) {
	deadline := time.Now().Add(speedSettleCap)
	var since time.Time
	for {
		snap := printerstate.Take(ctx, deps.stateDeps(), printer)
		derived := deriveFor(locks.pl, snap, nil)
		now := time.Now()
		if speedPresetMatches(snap, derived, wantSilent, targetPct) {
			if since.IsZero() {
				since = now
			}
			if now.Sub(since) >= speedHold {
				return snap, derived, true
			}
		} else {
			since = time.Time{}
		}
		if now.After(deadline) || ctx.Err() != nil {
			return snap, derived, false
		}
		select {
		case <-ctx.Done():
			return snap, derived, false
		case <-time.After(pollInterval):
		}
	}
}

// sendSpeedPreset is set_speed_preset's own send function.
func (p *Policy) sendSpeedPreset(ctx context.Context, deps Deps, printer domain.Printer, settings domain.Settings, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)
	targetPct, _ := printerstate.PresetFactorPercent(params.Preset)
	wantSilent := params.Preset == printerstate.PresetSilent
	silentOn := derived.Qmode == printerstate.QmodeOn
	fail := func(code Code, msg string) (Result, error) {
		return Result{}, &Error{Action: spec.Name, Code: code, Message: msg}
	}

	if derived.Qmode == printerstate.QmodeUnknown {
		return fail(CodeUnavailable, "the Silent mode state could not be read, so no preset is applied")
	}
	if snap.GCodeMove == nil || snap.GCodeMove.SpeedFactor == nil {
		return fail(CodeUnavailable, "the current speed factor could not be read (gcode_move.speed_factor), so no preset is applied")
	}

	// Silent is on but the speed factor was changed away from 50% (at the printer
	// screen): speedMode:1 would be a firmware no-op, so it cannot restore the
	// preset, and reporting no_change would be false. Refuse and say how to get back.
	if wantSilent && silentOn && !floatEqual(*snap.GCodeMove.SpeedFactor*100, targetPct) {
		return fail(CodeUnavailable, fmt.Sprintf("Silent is already on but the speed factor is %g%%, not 50%%, so the silent preset is not what the printer runs and sending speedMode:1 again does nothing: leave Silent with stable, standard or ultrafast, then enter it again", *snap.GCodeMove.SpeedFactor*100))
	}

	// no_change: nothing is sent when the derived current preset already equals
	// the target (plan S6, 2a.12), decided only with Silent and the factor known.
	if printerstate.SpeedPresetOf(snap, derived) == params.Preset {
		report := presetReport(params.Preset, snap, derived)
		return Result{
			Action: spec.Name, Accepted: false, Effect: "no_change",
			Effects: []string{"nothing was sent: the printer already runs the " + params.Preset + " preset"},
			Before:  before, After: before, Printer: printer, Job: printerstate.JobIdentityFrom(snap), SpeedPreset: report,
		}, nil
	}

	if err := checkPresetBand(params.Preset, silentOn, settings); err != nil {
		err.Action = spec.Name
		return Result{}, err
	}

	effects := speedPresetEffects(params.Preset, silentOn)
	if silentOn && !wantSilent {
		if rec := locks.pl.getSilentRec(); rec == nil || !sameJob(rec.job, printerstate.JobIdentityFrom(snap)) {
			effects = append(effects, silentElsewhereNote)
		}
	}

	var commands []string
	var notes []string
	accepted := true
	modeSent := false   // a speedMode frame was written
	modeFailed := false // the speedMode frame could not be written
	m220Failed := false // the M220 failed even after one retry
	m220Arg := map[string]string{"percent": strconv.Itoa(int(targetPct))}

	if wantSilent {
		if err := recheckStillPrinting(ctx, deps); err != nil {
			err.Action = spec.Name
			return Result{}, err
		}
		commands = append(commands, "speedMode:1 (port 9999)")
		sent, err := deps.WS9999.SetSpeedMode(ctx, true)
		modeSent, modeFailed = sent, err != nil
		if sent {
			locks.pl.setSilentRec(&silentRecord{job: printerstate.JobIdentityFrom(snap)})
		}
		if err != nil {
			accepted = false
			if ctx.Err() != nil {
				notes = append(notes, "the speedMode:1 frame was interrupted by pause_print or cancel_print; whether it reached the printer is not known")
			} else {
				notes = append(notes, "the speedMode frame reported an error ("+err.Error()+"); the settle read below shows whether Silent was entered")
			}
		}
	} else {
		if silentOn {
			commands = append(commands, "speedMode:0 (port 9999)")
			sent, err := deps.WS9999.SetSpeedMode(ctx, false)
			modeSent, modeFailed = sent, err != nil
			if err != nil {
				accepted = false
				if ctx.Err() != nil {
					notes = append(notes, "the speedMode:0 frame was interrupted by pause_print or cancel_print; whether it reached the printer is not known, and no speed factor was sent")
				} else {
					notes = append(notes, "the speedMode:0 frame could not be sent ("+err.Error()+"): Silent was not left and no speed factor was sent, because Silent's own exit would overwrite it")
				}
			}
		}
		if !modeFailed {
			commands = append(commands, "M220 S"+m220Arg["percent"])
			if err := deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM220, m220Arg); err != nil {
				// One retry: if speedMode:0 already ran, the printer is at the
				// pre-Silent factor, possibly faster than requested (plan 2a.3).
				commands = append(commands, "M220 S"+m220Arg["percent"]+" (retry)")
				if err2 := deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM220, m220Arg); err2 != nil {
					m220Failed = true
					accepted = false
					if ctx.Err() != nil {
						notes = append(notes, "the M220 was interrupted by pause_print or cancel_print; whether it reached the printer is not known")
					} else {
						notes = append(notes, "M220 failed twice ("+err2.Error()+")")
					}
				}
			}
		}
	}

	// cancelled reports what was written when the call was cancelled (pause_print
	// or cancel_print pre-empted it): the reads that follow would run on a dead
	// context and prove nothing, so only the write facts are reported here and
	// Execute adds a fresh read (preemptedResult).
	cancelled := func() (Result, error) {
		eff := append([]string(nil), effects...)
		if wantSilent && len(eff) > 0 {
			// Silent's effects are not facts here: the frame may never have gone out.
			eff[0] = "if Silent was applied (see the fresh read below): " + eff[0]
		}
		return Result{
			Action: spec.Name, Accepted: accepted && len(commands) > 0, Effect: "unconfirmed",
			Effects:  withCFSNote(append(eff, notes...), spec.Name, derived),
			Commands: commands, Before: before, Printer: printer,
		}, nil
	}
	if ctx.Err() != nil {
		return cancelled()
	}

	var afterSnap printerstate.Snapshot
	var afterDerived printerstate.Derived
	held := false
	if modeFailed {
		afterSnap = printerstate.Take(ctx, deps.stateDeps(), printer)
		afterDerived = deriveFor(locks.pl, afterSnap, nil)
	} else {
		afterSnap, afterDerived, held = p.pollSpeedPreset(ctx, deps, printer, locks, wantSilent, targetPct)
	}
	if ctx.Err() != nil {
		return cancelled()
	}
	report := presetReport(params.Preset, afterSnap, afterDerived)

	effect := "unconfirmed"
	disagree := channelDisagreement(report, wantSilent, targetPct)
	switch {
	case held && disagree == "":
		effect = "confirmed"
		if report.SpeedMode9999 == nil {
			notes = append(notes, "port 9999 did not report speedMode, so only Moonraker confirms this")
		}
		if m220Failed {
			notes = append(notes, "the M220 call reported an error but Moonraker reads the target speed factor")
		}
	case held:
		notes = append(notes, fmt.Sprintf("Moonraker shows the %s preset but port 9999 reports %s: the channels disagree, so the preset is treated as unknown", params.Preset, disagree))
	case modeSent && !wantSilent && m220Failed:
		effect = "partial"
		// What a fresh read shows, not what was inferred from a written frame.
		notes = append(notes, fmt.Sprintf("the speedMode:0 frame was sent; a fresh read shows Silent %s and the speed factor at %s, not %g%%",
			afterDerived.Qmode, percentText(report.MoonrakerFactorPct), targetPct))
	}
	// The record follows what a fresh read shows: Silent on means this server
	// entered it (or it was already on), off clears it.
	switch {
	case afterDerived.Qmode == printerstate.QmodeOff:
		locks.pl.setSilentRec(nil)
	case wantSilent && afterDerived.Qmode == printerstate.QmodeOn && modeSent:
		locks.pl.setSilentRec(&silentRecord{job: printerstate.JobIdentityFrom(snap)})
	}
	// stuck_silent_possible only when Silent is on and print_stats says the job
	// is neither printing nor paused: Silent then outlives the print (Qmode_exit
	// is a no-op outside a print). A paused or pausing print keeps its real
	// outcome; Silent legitimately stays on through a pause.
	if silentStuck(afterSnap, afterDerived) {
		effect = "stuck_silent_possible"
		notes = append(notes, "the printer is not printing or paused any more (print_stats "+afterSnap.PrintStats.State+") while Silent is on. "+StuckSilentText)
	} else if afterDerived.Qmode == printerstate.QmodeOn && (afterDerived.Bucket == printerstate.BucketZ || afterDerived.Bucket == printerstate.BucketT) {
		notes = append(notes, "the print is paused or pausing; Silent stays active through the pause until it ends or is changed")
	}
	if wantSilent && effect != "confirmed" && effect != "stuck_silent_possible" && len(effects) > 0 {
		// Do not list Silent's effects as fact when it may not have been entered.
		effects[0] = "if Silent was entered (see the notes and the state block): " + effects[0]
	}
	report.Notes = notes
	effects = append(effects, notes...)

	return Result{
		Action: spec.Name, Accepted: accepted, Effect: effect,
		Effects:  withCFSNote(effects, spec.Name, derived),
		Commands: commands,
		Before:   before,
		After:    printerstate.BuildStateBlock(afterSnap, afterDerived, nil),
		Printer:  printer,
		Job:      printerstate.JobIdentityFrom(afterSnap),

		SpeedPreset: report,
	}, nil
}

// percentText renders an optional percent.
func percentText(p *float64) string {
	if p == nil {
		return "unreadable"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

// silentStuck is the stuck-Silent condition after a preset call: Silent is on
// and the raw print_stats.state is neither printing nor paused.
func silentStuck(snap printerstate.Snapshot, d printerstate.Derived) bool {
	if d.Qmode != printerstate.QmodeOn || snap.PrintStats == nil {
		return false
	}
	s := snap.PrintStats.State
	return s != "printing" && s != "paused"
}

// StuckSilentWarning returns the stuck-Silent warning for a derived state, or
// "" when it does not apply (Silent off or unknown, or the printer is printing).
// get_printer_status shows it in its guidance (plan 2a.9).
func StuckSilentWarning(d printerstate.Derived) string {
	if notes := stuckSilentNotes(d); len(notes) > 0 {
		return notes[0]
	}
	return ""
}
