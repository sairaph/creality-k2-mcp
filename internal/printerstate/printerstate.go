// Package printerstate is the state reader (dev_docs/safety-architecture.md
// section 3.1): it gathers one consistent snapshot of a printer across
// Moonraker and the Creality port-9999 protocol, and derives from it the one
// authoritative activity state every tool and policy decision in this
// project must use (references/analysis/11-state-model.md section 1,
// P2 "one source of truth").
//
// Two rules shape every decision in this package:
//
//   - P1, fail closed: anything unknown, unmapped, unreadable or ambiguous
//     resolves to the "unknown" state with gating class unknown_fail_closed,
//     never silently to a permissive default. A nil pointer on a decoded
//     Moonraker field means "this package cannot read this field", never
//     "the field's zero value" (see internal/moonraker's own presence-rule
//     doc comment, which this package's derivation logic depends on).
//   - P3, Moonraker is authoritative, port 9999 corroborates only: no row in
//     DeriveActivityState's precedence table may be satisfied by a 9999
//     field alone unless 11-state-model.md section 1.1 explicitly says so
//     (upgrading, recovery_pending, and the homing/calibrating corroboration
//     signals). 9999 being unreachable therefore only ever removes those few
//     signals; it must never fail closed an otherwise Moonraker-answerable
//     decision (11-state-model.md section 2.3).
package printerstate

// The 19 display-state names from references/analysis/11-state-model.md
// section 1.1, in the same order and spelling as that table's # column so a
// reader can cross-reference directly. Row 14 names two possible display
// values (filament_operation, cfs_operation) sharing one precedence slot;
// every other row has exactly one name.
const (
	StateOffline           = "offline"
	StateKlippyNotReady    = "klippy_not_ready"
	StateError             = "error"
	StateHoming            = "homing"
	StatePreparing         = "preparing"
	StateCalibrating       = "calibrating"
	StateCancelling        = "cancelling"
	StatePausing           = "pausing"
	StatePaused            = "paused"
	StateResuming          = "resuming"
	StateCancelled         = "cancelled"
	StatePrinting          = "printing"
	StateComplete          = "complete"
	StateFilamentOperation = "filament_operation"
	StateCFSOperation      = "cfs_operation"
	StateUpgrading         = "upgrading"
	StateRecoveryPending   = "recovery_pending"
	StateIdle              = "idle"
	StateBusyCommand       = "busy_command"
	StateUnknown           = "unknown"

	// StateIdentityMismatch and StateIdentityUnverified are not part of
	// 11-state-model.md's original 19 rows: they were added for review
	// backlog item 24, evaluated ahead of every row above other than
	// offline/klippy_not_ready (state.go's checkIdentity, called right
	// after Klipper is confirmed ready). A registry-backed printer's
	// persisted hostname must be corroborated by a fresh printer/info read
	// before anything else in the snapshot is trusted, since the address a
	// registry entry points at can start answering for a different
	// physical printer (e.g. DHCP moving the IP).
	StateIdentityMismatch   = "identity_mismatch"
	StateIdentityUnverified = "identity_unverified"
)

// Bucket is the action-policy bucket a display state maps to
// (dev_docs/safety-architecture.md section 4.1). internal/policy uses Bucket,
// never the display state string, to decide which actions a tool may attempt
// (P2).
type Bucket string

const (
	BucketU  Bucket = "U"  // unknown/fail-closed: no writes except set_light when 9999 answers
	BucketE  Bucket = "E"  // job-level error: writes blocked, no error-dismiss tool in v0.1.0
	BucketB  Bucket = "B"  // busy (homing, calibrating, busy_command): writes blocked except set_light
	BucketPP Bucket = "PP" // preparing: cancel allowed, pause/setpoints blocked
	BucketP  Bucket = "P"  // printing
	BucketT  Bucket = "T"  // transitioning (our own pending pause/resume/cancel): writes blocked except set_light
	BucketZ  Bucket = "Z"  // paused: only resume is a legal gated action
	BucketI  Bucket = "I"  // idle/complete/cancelled(settled): safe to start
)

// GatingClass is the coarse class every tool precondition branches on
// (references/analysis/11-state-model.md section 1.0, point 2). Never use the
// display state string for a gating decision; always use GatingClass or
// Bucket.
type GatingClass string

const (
	ClassSafeToAct         GatingClass = "safe_to_act"
	ClassBusy              GatingClass = "busy"
	ClassTransitioning     GatingClass = "transitioning"
	ClassUnknownFailClosed GatingClass = "unknown_fail_closed"
)

// bucketOf maps every display state that has exactly one possible bucket.
// "error" is deliberately absent: DeriveActivityState assigns its bucket
// itself (BucketU for a host-level klippy error, BucketE for a job-level
// print_stats.state=="error"), since the same display name legitimately maps
// to two different buckets depending on which sub-condition fired
// (dev_docs/safety-architecture.md section 4.1's own table lists these as two
// separate rows sharing one display name). Every other state's bucket is
// determined solely by which precedence row matched, so a single table is
// enough for a test to pin "one bucket per state" against everything except
// error's documented exception.
var bucketOf = map[string]Bucket{
	StateOffline:            BucketU,
	StateKlippyNotReady:     BucketU,
	StateHoming:             BucketB,
	StatePreparing:          BucketPP,
	StateCalibrating:        BucketB,
	StateCancelling:         BucketT,
	StatePausing:            BucketT,
	StatePaused:             BucketZ,
	StateResuming:           BucketT,
	StateCancelled:          BucketI,
	StatePrinting:           BucketP,
	StateComplete:           BucketI,
	StateFilamentOperation:  BucketU,
	StateCFSOperation:       BucketU,
	StateUpgrading:          BucketU,
	StateRecoveryPending:    BucketU,
	StateIdle:               BucketI,
	StateBusyCommand:        BucketB,
	StateUnknown:            BucketU,
	StateIdentityMismatch:   BucketU,
	StateIdentityUnverified: BucketU,
}

// Derived is DeriveActivityState's result: the one authoritative activity
// state plus everything a caller needs to gate a write or explain a refusal
// (P2, P6). CFSConnected is reported alongside the bucket, never folded into
// it: safety-architecture.md D5 requires pause and cancel of a running print
// to stay reachable (bucket P/PP/Z unchanged) while the CFS reports
// connected, so internal/policy must combine Bucket and CFSConnected itself
// (block start/resume/setpoints, keep pause/cancel) rather than read a single
// collapsed "everything is blocked" signal from this package.
type Derived struct {
	State        string
	Bucket       Bucket
	Class        GatingClass
	CFSConnected bool
	Reasons      []string
}
