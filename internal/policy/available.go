package policy

import (
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// Available implements dev_docs/safety-architecture.md section 3.2's
// Available(snapshot, settings): for every action, whether it is
// available, blocked (with a reason) or needs a confirmation call first.
// This is for display and guidance only (P6) and never gates a write:
// Execute always re-derives from its own fresh snapshot and never consults
// this function's output. The pending-action lookup below keys on
// printerstate.Identity(snap) - the same verified live hostname Execute's
// lock registry keys on - rather than any fallback derived from printer
// (review backlog item 31): a printer this snapshot could not verify has no
// pending lookup done for it at all, since printer.Host was never the key
// anything was ever locked or armed under.
func (p *Policy) Available(printer domain.Printer, snap printerstate.Snapshot, settings domain.Settings) []printerstate.ActionGate {
	var pending *printerstate.PendingAction
	if identity := printerstate.Identity(snap); identity != printerstate.UnverifiedIdentity {
		pending = p.locks.get(identity).getPending()
	}
	derived := printerstate.DeriveActivityState(snap, pending)
	return GatesFor(printer, derived, settings)
}

// GatesFor is AvailableFor plus the per-printer allow_control gate: Execute
// refuses every write for a printer without allow_control before looking at
// state at all, so the preview must say the same. Without this an idle
// printer with control off listed set_light as available and the AI learned
// the refusal only by trying. Every caller that shows actions for a specific
// printer uses this, not AvailableFor.
func GatesFor(printer domain.Printer, derived printerstate.Derived, settings domain.Settings) []printerstate.ActionGate {
	gates := AvailableFor(derived, settings)
	for i := range gates {
		if err := checkAllowControl(ActionName(gates[i].Name), printer); err != nil {
			gates[i].Status = "blocked"
			gates[i].Reason = err.Message
		}
	}
	return gates
}

// AvailableFor is Available's pure core: it needs only the derived state
// (not a live *Policy or printer identity), so tests and Available itself
// share exactly one implementation.
func AvailableFor(derived printerstate.Derived, settings domain.Settings) []printerstate.ActionGate {
	gates := make([]printerstate.ActionGate, 0, len(Actions))
	for _, name := range Actions {
		gates = append(gates, gateFor(specs[name], derived))
	}
	return gates
}

func gateFor(spec actionSpec, derived printerstate.Derived) printerstate.ActionGate {
	if err := checkGate(spec, derived); err != nil {
		return printerstate.ActionGate{Name: string(spec.Name), Status: "blocked", Reason: err.Message}
	}
	if spec.Confirmation == ConfirmationProposalToken {
		return printerstate.ActionGate{Name: string(spec.Name), Status: "needs_confirmation"}
	}
	// ConfirmationNone, and ConfirmationConditional's baseline (upload
	// needs a token only for an overwrite, which depends on the specific
	// filename a caller has not supplied yet; Available reports the
	// non-overwrite, always-possible case as available).
	return printerstate.ActionGate{Name: string(spec.Name), Status: "available"}
}
