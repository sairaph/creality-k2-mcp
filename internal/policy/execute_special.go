package policy

import (
	"context"
	"strconv"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// sendStartPrint is start_print's own send function: D7's flow-factor
// record/reset/restore does not fit the generic sendPolled shape (it can
// send a second command after the fact, on the failure path). identity is
// the already-verified identity Execute resolved via resolveExecuteIdentity,
// threaded through rather than recomputed here (review backlog item 31): it
// must be the exact identity armIdleHeatWatchdog armed the watchdog under,
// or Disarm below would cancel the wrong key (or none at all) and leave a
// heater's automatic turn-off silently unwatched.
func (p *Policy) sendStartPrint(ctx context.Context, deps Deps, printer domain.Printer, identity string, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)

	// D2: cancel any armed idle-heat watchdog for this printer immediately,
	// before this call sends anything, so a heater left armed while idle is
	// never turned off out from under a print that is now starting. This is
	// best-effort and its result is ignored on purpose: a print starting
	// must never be gated by the background daemon (the daemon being down
	// simply means nothing was armed on it to begin with, since Arm always
	// requires the daemon alive).
	if deps.Watchdog != nil {
		_ = deps.Watchdog.Disarm(ctx, identity)
	}

	var prevFlowPercent *float64
	if snap.GCodeMove != nil && snap.GCodeMove.ExtrudeFactor != nil {
		v := *snap.GCodeMove.ExtrudeFactor * 100
		prevFlowPercent = &v
	}
	needsReset := prevFlowPercent != nil && !floatEqual(*prevFlowPercent, 100)

	locks.pl.setPending(&printerstate.PendingAction{Kind: printerstate.PendingStart, IssuedAt: time.Now(), Timeout: spec.SettleTimeout})
	defer locks.pl.setPending(nil)

	p.setUploading(identity, params.Filename, false) // start implies the upload, if any, is done

	var commands []string
	var sendErr error
	if needsReset {
		sendErr = deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM221, map[string]string{"percent": "100"})
		commands = append(commands, "M221 S100")
	}
	if sendErr == nil {
		sendErr = deps.Moonraker.PrintStart(ctx, params.Filename)
	}
	commands = append(commands, "PrintStart")
	accepted := sendErr == nil

	settleFn := func(_ printerstate.Snapshot, d printerstate.Derived) bool {
		return d.State == printerstate.StatePrinting
	}
	afterSnap, afterDerived, confirmed := p.pollUntilSettle(ctx, deps, printer, locks, printerstate.PendingStart, spec.SettleTimeout, settleFn)

	result := Result{
		Action:   spec.Name,
		Accepted: accepted,
		Effect:   effectString(confirmed),
		Effects:  spec.Effects,
		Commands: commands,
		Before:   before,
		After:    printerstate.BuildStateBlock(afterSnap, afterDerived, nil),
		Printer:  printer,
		Job:      printerstate.JobIdentityFrom(afterSnap),
	}

	// D7: restore the previous flow factor if the print did not reach
	// printing. Best-effort: if the restore call itself fails, the caller
	// still sees Effect "unconfirmed" and can act on it (P6); a failed
	// restore never turns into a fabricated success.
	if !confirmed && needsReset {
		restoreErr := deps.Moonraker.RunTemplate(ctx, moonraker.TemplateM221, map[string]string{"percent": strconv.Itoa(int(*prevFlowPercent))})
		if restoreErr == nil {
			result.StartPrintFlowRestored = prevFlowPercent
		}
	}

	return result, nil
}

// sendSetLight is set_light's own send function: crealityws.SetLight
// already performs its own send-then-confirm round trip
// (crealityws.LightConfirmTimeout), so this does not use the generic
// snapshot-poll loop.
func (p *Policy) sendSetLight(ctx context.Context, deps Deps, printer domain.Printer, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)

	confirmed, err := deps.WS9999.SetLight(ctx, params.On)
	accepted := err == nil

	afterSnap := printerstate.Take(ctx, deps.stateDeps(), printer)
	afterDerived := printerstate.DeriveActivityState(afterSnap, nil)

	return Result{
		Action:   spec.Name,
		Accepted: accepted,
		Effect:   effectString(confirmed),
		Effects:  spec.Effects,
		Commands: spec.Commands,
		Before:   before,
		After:    printerstate.BuildStateBlock(afterSnap, afterDerived, nil),
		Printer:  printer,
		Job:      printerstate.JobIdentityFrom(afterSnap),
	}, nil
}

// sendUpload is upload_gcode_file's own send function: it settles by
// polling the gcodes-root listing, not a printerstate snapshot field.
// identity is the already-verified identity Execute resolved via
// resolveExecuteIdentity, threaded through rather than recomputed here
// (review backlog item 31), so isUploading's later lookup (evaluateParams,
// checked against the same identity Execute always resolves) actually finds
// what this call recorded.
func (p *Policy) sendUpload(ctx context.Context, deps Deps, printer domain.Printer, identity string, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)

	p.setUploading(identity, params.Filename, true)
	defer p.setUploading(identity, params.Filename, false)

	_, sendErr := deps.Moonraker.Upload(ctx, params.LocalPath, params.Filename)
	accepted := sendErr == nil

	confirmed := p.pollFileListing(ctx, deps, spec.SettleTimeout, func(files []moonraker.GCodeFile) bool {
		return listedContains(files, params.Filename)
	})

	afterSnap := printerstate.Take(ctx, deps.stateDeps(), printer)
	afterDerived := printerstate.DeriveActivityState(afterSnap, nil)

	return Result{
		Action:   spec.Name,
		Accepted: accepted,
		Effect:   effectString(confirmed),
		Effects:  spec.Effects,
		Commands: spec.Commands,
		Before:   before,
		After:    printerstate.BuildStateBlock(afterSnap, afterDerived, nil),
		Printer:  printer,
		Job:      printerstate.JobIdentityFrom(afterSnap),
	}, nil
}

// sendDelete is delete_gcode_file's own send function, settling the same
// way as sendUpload but on absence rather than presence.
func (p *Policy) sendDelete(ctx context.Context, deps Deps, printer domain.Printer, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	before := printerstate.BuildStateBlock(snap, derived, nil)

	_, sendErr := deps.Moonraker.Delete(ctx, params.Filename)
	accepted := sendErr == nil

	confirmed := p.pollFileListing(ctx, deps, spec.SettleTimeout, func(files []moonraker.GCodeFile) bool {
		return !listedContains(files, params.Filename)
	})

	afterSnap := printerstate.Take(ctx, deps.stateDeps(), printer)
	afterDerived := printerstate.DeriveActivityState(afterSnap, nil)

	return Result{
		Action:   spec.Name,
		Accepted: accepted,
		Effect:   effectString(confirmed),
		Effects:  spec.Effects,
		Commands: spec.Commands,
		Before:   before,
		After:    printerstate.BuildStateBlock(afterSnap, afterDerived, nil),
		Printer:  printer,
		Job:      printerstate.JobIdentityFrom(afterSnap),
	}, nil
}

// pollFileListing polls the gcodes-root listing at pollInterval until want
// reports true or timeout elapses.
func (p *Policy) pollFileListing(ctx context.Context, deps Deps, timeout time.Duration, want func([]moonraker.GCodeFile) bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		files, err := deps.Moonraker.List(ctx)
		if err == nil && want(files) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pollInterval):
		}
	}
}
