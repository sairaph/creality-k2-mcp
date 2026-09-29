package printerstate

import "time"

// PendingKind names the lifecycle write a pendingAction is tracking
// (dev_docs/safety-architecture.md section 3.4, references/analysis/11-state-model.md
// section 3). internal/policy owns the lifecycle of a PendingAction (when it
// is created, when it is cleared on settle or timeout); this package only
// reads it as an input to DeriveActivityState.
type PendingKind string

const (
	PendingNone   PendingKind = ""
	PendingStart  PendingKind = "start"
	PendingPause  PendingKind = "pause"
	PendingResume PendingKind = "resume"
	PendingCancel PendingKind = "cancel"
	PendingHome   PendingKind = "home"

	// PendingFilamentOperation and PendingCFSOperation back
	// 11-state-model.md row 14's conservative rule: since no printer-reported
	// signal reliably marks a filament-load or CFS operation as busy, a tool
	// that starts one must hold its own client-side "just started this, treat
	// as busy" lock until a generous, evidence-free timeout elapses. Since v0.2.0
	// set_filament_definition holds PendingCFSOperation while it runs, and the
	// printer-reported CFS feed signals (state.go row 14) cover the rest.
	PendingFilamentOperation PendingKind = "filament_operation"
	PendingCFSOperation      PendingKind = "cfs_operation"
)

// PendingAction is the caller-supplied (internal/policy-owned) record of a
// lifecycle write this server itself issued but has not yet seen settle.
// DeriveActivityState uses it only to evaluate the transition rows
// (11-state-model.md section 1.1 rows 7, 8, 10); it never creates, clears or
// times one out itself (safety-architecture.md 3.2, which is internal/policy's
// job in a later task).
type PendingAction struct {
	Kind     PendingKind
	IssuedAt time.Time
	Timeout  time.Duration
}

// Elapsed returns how long the action has been pending as of now. A nil
// receiver returns zero, so callers can call it unconditionally on a
// possibly-nil *PendingAction.
func (p *PendingAction) Elapsed(now time.Time) time.Duration {
	if p == nil {
		return 0
	}
	return now.Sub(p.IssuedAt)
}

// Active reports whether p names kind and has not yet exceeded its own
// timeout as of now. A nil receiver, or a kind mismatch, is never active.
// This implements the exact precondition 11-state-model.md section 1.1 rows
// 7, 8, 10 state for a transition row: "a <kind> action was issued less than
// T_<kind>_timeout ago". Once the timeout elapses this reports false and the
// transition row stops matching (11-state-model.md section 1.1's own
// wording); what to tell the caller about a timed-out pending action is
// internal/policy's concern (safety-architecture.md 3.2 step 5), not this
// package's.
func (p *PendingAction) Active(now time.Time, kind PendingKind) bool {
	if p == nil || p.Kind != kind || p.Kind == PendingNone {
		return false
	}
	return p.Elapsed(now) < p.Timeout
}
