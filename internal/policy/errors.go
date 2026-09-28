package policy

import "fmt"

// Code is a render error code, matching the vocabulary the task names
// explicitly (conflict, invalid_input, unavailable, not_found, forbidden)
// plus internal_error for anything this package cannot otherwise classify.
// It intentionally mirrors moonraker.Code's spelling without importing that
// package's type, so this package's own errors never depend on what the
// command layer happened to return.
type Code string

const (
	// CodeConflict: a TOCTOU-class refusal - the printer state, job
	// identity or bound proposal field changed since it was last read or
	// since the proposal was issued, or another write is already in flight
	// on this printer (in-process or cross-process lock contention).
	CodeConflict Code = "conflict"
	// CodeInvalidInput: a parameter is out of range, malformed, or outside
	// a configured band (D1).
	CodeInvalidInput Code = "invalid_input"
	// CodeUnavailable: the action is not allowed in the printer's current
	// bucket/gating class, or CFS is connected and blocks it (D5), or a
	// live value this action's limit depends on (e.g. product_param) could
	// not be read.
	CodeUnavailable Code = "unavailable"
	// CodeNotFound: a referenced object, file or proposal token does not
	// exist.
	CodeNotFound Code = "not_found"
	// CodeForbidden: the printer's allow_control is false.
	CodeForbidden Code = "forbidden"
	// CodeInternal: an unexpected failure this package cannot classify more
	// specifically (e.g. a lock file could not be created).
	CodeInternal Code = "internal_error"
)

// Error is what Execute and Available return for every gating, parameter,
// lock, token or transport-classification failure. Message is meant to be
// shown to the calling AI directly (P6: guide, do not just refuse).
type Error struct {
	Action  ActionName
	Code    Code
	Message string
	// Changed lists which bound fields differed, for CodeConflict raised by
	// a proposal_token re-verification (dev_docs/safety-architecture.md
	// section 3.3).
	Changed []string
}

func (e *Error) Error() string {
	if e.Action != "" {
		return fmt.Sprintf("policy: %s: %s: %s", e.Action, e.Code, e.Message)
	}
	return fmt.Sprintf("policy: %s: %s", e.Code, e.Message)
}
