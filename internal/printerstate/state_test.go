package printerstate

import (
	"errors"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

func boolPtr(b bool) *bool        { return &b }
func floatPtr(f float64) *float64 { return &f }
func strPtr(s string) *string     { return &s }
func intPtr(i int) *int           { return &i }

// syntheticIdle is a fully-known, hand-built idle Snapshot: every field a
// row in references/analysis/11-state-model.md section 1.1 reads is
// explicitly set to its idle-baseline value, so individual tests can flip
// exactly one field without any risk of aliasing a shared pointer.
func syntheticIdle() Snapshot {
	return Snapshot{
		Taken:      fixedNow,
		ServerInfo: moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"},
		Webhooks:   &moonraker.Webhooks{State: strPtr("ready")},
		PrintStats: &moonraker.PrintStats{State: "standby"},
		PauseResume: &moonraker.PauseResume{
			IsPaused: boolPtr(false),
		},
		IdleTimeout: &moonraker.IdleTimeout{State: strPtr("Ready")},
		VirtualSDCard: &moonraker.VirtualSDCard{
			IsActive:              boolPtr(false),
			BedMeshCalibrateState: boolPtr(false),
		},
		MotorControl: &moonraker.MotorControl{IsHoming: boolPtr(false)},
		CustomMacro:  &moonraker.CustomMacro{LevelingCalibration: intPtr(0)},
		Box:          &moonraker.Box{State: strPtr("disconnect")},
		WS9999: crealityws.Status{
			State:         crealityws.Int{Value: 0, Present: true},
			DeviceState:   crealityws.Int{Value: 0, Present: true},
			UpgradeStatus: crealityws.Int{Value: 0, Present: true},
			RepoPlrStatus: crealityws.Int{Value: 0, Present: true},
			CfsConnect:    crealityws.Int{Value: 0, Present: true},
		},
		WS9999Reachable: true,
	}
}

func TestDeriveActivityState_RealIdleCapture(t *testing.T) {
	snap := idleSnapshot(t)
	d := DeriveActivityState(snap, nil)
	if d.State != StateIdle {
		t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StateIdle, d.Reasons)
	}
	if d.Bucket != BucketI || d.Class != ClassSafeToAct {
		t.Fatalf("Bucket/Class = %s/%s, want I/safe_to_act", d.Bucket, d.Class)
	}
	if d.CFSConnected {
		t.Fatal("CFSConnected = true on the real idle capture, want false (box.state is disconnect)")
	}
}

func TestDeriveActivityState_RealPrintingCapture(t *testing.T) {
	// This exact capture (control_test_20260928.log:31-33) is the one
	// 11-state-model.md section 1.1 row 5 cites as its own evidence: the
	// print flipped straight to print_stats.state "printing" with
	// print_duration still 0.0 moments after POST /printer/print/start, so
	// this snapshot lands in "preparing" (still inside START_PRINT), not
	// "printing", by that row's own definition. Both gate as busy; the
	// distinct display state and PP bucket are what this test pins.
	snap := printingSnapshot(t)
	d := DeriveActivityState(snap, nil)
	if d.State != StatePreparing {
		t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StatePreparing, d.Reasons)
	}
	if d.Bucket != BucketPP || d.Class != ClassBusy {
		t.Fatalf("Bucket/Class = %s/%s, want PP/busy", d.Bucket, d.Class)
	}
}

func TestDeriveActivityState_RealPausedCapture(t *testing.T) {
	snap := pausedSnapshot(t)
	d := DeriveActivityState(snap, nil)
	if d.State != StatePaused {
		t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StatePaused, d.Reasons)
	}
	if d.Bucket != BucketZ || d.Class != ClassBusy {
		t.Fatalf("Bucket/Class = %s/%s, want Z/busy", d.Bucket, d.Class)
	}
}

func TestDeriveActivityState_RealCancelledCapture(t *testing.T) {
	snap := cancelledSnapshot(t)
	d := DeriveActivityState(snap, nil)
	if d.State != StateCancelled {
		t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StateCancelled, d.Reasons)
	}
	if d.Bucket != BucketI || d.Class != ClassSafeToAct {
		t.Fatalf("Bucket/Class = %s/%s, want I/safe_to_act", d.Bucket, d.Class)
	}
}

// stateCase is one row of the state table: a snapshot builder, an optional
// pending action and the expected state/bucket/class. It is shared by the
// derivation test and by the CFS-flags-on-every-return-path test.
type stateCase struct {
	name    string
	snap    func() Snapshot
	pending *PendingAction
	state   string
	bucket  Bucket
	class   GatingClass
}

// stateTable is the table of synthetic rows (see the derivation test below).
func stateTable() []stateCase {
	return []stateCase{
		{
			name: "row1 offline",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.ServerInfoErr = errors.New("dial tcp 192.168.1.102:7125: connect: connection refused")
				return s
			},
			state: StateOffline, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name: "row2 klippy_not_ready (not connected)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.ServerInfo.KlippyConnected = false
				return s
			},
			state: StateKlippyNotReady, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name: "row2 klippy_not_ready (startup)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.ServerInfo.KlippyState = "startup"
				return s
			},
			state: StateKlippyNotReady, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name: "row3 error host-level",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.ServerInfo.KlippyState = "error"
				return s
			},
			state: StateError, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name: "row3 error job-level",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "error"
				return s
			},
			state: StateError, bucket: BucketE, class: ClassBusy,
		},
		{
			name: "row4 homing",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.MotorControl.IsHoming = boolPtr(true)
				return s
			},
			state: StateHoming, bucket: BucketB, class: ClassBusy,
		},
		{
			name: "row5 preparing",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "printing"
				s.PrintStats.PrintDuration = 0
				s.VirtualSDCard.IsActive = boolPtr(true)
				s.PauseResume.IsPaused = boolPtr(false)
				return s
			},
			state: StatePreparing, bucket: BucketPP, class: ClassBusy,
		},
		{
			name: "row6 calibrating",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.CustomMacro.LevelingCalibration = intPtr(1)
				return s
			},
			state: StateCalibrating, bucket: BucketB, class: ClassBusy,
		},
		{
			name: "row7 cancelling (transition)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "printing"
				s.PrintStats.PrintDuration = 10
				s.VirtualSDCard.IsActive = boolPtr(true)
				s.PauseResume.IsPaused = boolPtr(false)
				return s
			},
			pending: &PendingAction{Kind: PendingCancel, IssuedAt: fixedNow.Add(-1 * time.Second), Timeout: 60 * time.Second},
			state:   StateCancelling, bucket: BucketT, class: ClassTransitioning,
		},
		{
			name: "row8 pausing (transition)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "printing"
				s.PrintStats.PrintDuration = 10
				s.VirtualSDCard.IsActive = boolPtr(true)
				s.PauseResume.IsPaused = boolPtr(false)
				return s
			},
			pending: &PendingAction{Kind: PendingPause, IssuedAt: fixedNow.Add(-1 * time.Second), Timeout: 30 * time.Second},
			state:   StatePausing, bucket: BucketT, class: ClassTransitioning,
		},
		{
			name: "row9 paused",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "paused"
				s.PauseResume.IsPaused = boolPtr(true)
				return s
			},
			state: StatePaused, bucket: BucketZ, class: ClassBusy,
		},
		{
			name: "row10 resuming (transition)",
			snap: func() Snapshot {
				s := syntheticIdle()
				// is_paused already flipped false but virtual_sdcard has not
				// yet gone active: not settled-paused (row9, needs
				// is_paused true) and not settled-printing (row12, needs
				// is_active true), the narrow window row10 exists for.
				s.PrintStats.State = "paused"
				s.PauseResume.IsPaused = boolPtr(false)
				s.VirtualSDCard.IsActive = boolPtr(false)
				return s
			},
			pending: &PendingAction{Kind: PendingResume, IssuedAt: fixedNow.Add(-1 * time.Second), Timeout: 120 * time.Second},
			state:   StateResuming, bucket: BucketT, class: ClassTransitioning,
		},
		{
			name: "row11 cancelled settled",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "cancelled"
				s.VirtualSDCard.IsActive = boolPtr(false)
				s.IdleTimeout.State = strPtr("Ready")
				return s
			},
			state: StateCancelled, bucket: BucketI, class: ClassSafeToAct,
		},
		{
			name: "row12 printing",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "printing"
				s.PrintStats.PrintDuration = 10
				s.PauseResume.IsPaused = boolPtr(false)
				s.VirtualSDCard.IsActive = boolPtr(true)
				return s
			},
			state: StatePrinting, bucket: BucketP, class: ClassBusy,
		},
		{
			name: "row13 complete",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "complete"
				return s
			},
			state: StateComplete, bucket: BucketI, class: ClassSafeToAct,
		},
		{
			name: "row14 filament_operation",
			snap: func() Snapshot {
				return syntheticIdle()
			},
			pending: &PendingAction{Kind: PendingFilamentOperation, IssuedAt: fixedNow.Add(-1 * time.Second), Timeout: 60 * time.Second},
			state:   StateFilamentOperation, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name: "row14 cfs_operation",
			snap: func() Snapshot {
				return syntheticIdle()
			},
			pending: &PendingAction{Kind: PendingCFSOperation, IssuedAt: fixedNow.Add(-1 * time.Second), Timeout: 60 * time.Second},
			state:   StateCFSOperation, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name: "row15 upgrading",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.WS9999.UpgradeStatus = crealityws.Int{Value: 1, Present: true}
				return s
			},
			state: StateUpgrading, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name: "row16 recovery_pending",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.WS9999.RepoPlrStatus = crealityws.Int{Value: 1, Present: true}
				return s
			},
			state: StateRecoveryPending, bucket: BucketU, class: ClassUnknownFailClosed,
		},
		{
			name:  "row17 idle",
			snap:  syntheticIdle,
			state: StateIdle, bucket: BucketI, class: ClassSafeToAct,
		},
		{
			// The adversarial case the HIGH finding was about: a long
			// command started by another actor (touchscreen, Creality
			// Print, another MCP client) while print_stats and
			// virtual_sdcard look exactly like idle. Row 18 (busy_command)
			// is now evaluated before row 17 (idle), and isIdle itself also
			// requires idle_timeout to be known and not "Printing", so this
			// no longer derives as idle/safe_to_act.
			name: "row18 busy_command (standby, inactive, idle_timeout Printing)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.IdleTimeout.State = strPtr("Printing")
				return s
			},
			state: StateBusyCommand, bucket: BucketB, class: ClassBusy,
		},
		{
			name: "row18 busy_command (settled cancelled equivalent, idle_timeout Printing)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "cancelled"
				s.VirtualSDCard.IsActive = boolPtr(false)
				s.IdleTimeout.State = strPtr("Printing")
				return s
			},
			state: StateBusyCommand, bucket: BucketB, class: ClassBusy,
		},
		{
			name: "row18 busy_command (complete equivalent, idle_timeout Printing)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "complete"
				s.IdleTimeout.State = strPtr("Printing")
				return s
			},
			state: StateBusyCommand, bucket: BucketB, class: ClassBusy,
		},
		{
			name: "row19 unknown (unrecognised print_stats.state)",
			snap: func() Snapshot {
				s := syntheticIdle()
				s.PrintStats.State = "some_future_firmware_value"
				return s
			},
			state: StateUnknown, bucket: BucketU, class: ClassUnknownFailClosed,
		},
	}
}

// TestDeriveActivityState_AllNineteenStates exercises one synthetic Snapshot
// per row of references/analysis/11-state-model.md section 1.1, asserting
// the exact display state, bucket and gating class each one produces. This
// also doubles as the "one bucket per state" pinning test except for
// "error", whose two buckets are asserted explicitly by name.

func TestDeriveActivityState_AllNineteenStates(t *testing.T) {
	tests := stateTable()

	seenBucket := map[string]Bucket{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := DeriveActivityState(tc.snap(), tc.pending)
			if d.State != tc.state {
				t.Fatalf("State = %q, want %q (reasons: %v)", d.State, tc.state, d.Reasons)
			}
			if d.Bucket != tc.bucket {
				t.Fatalf("Bucket = %s, want %s", d.Bucket, tc.bucket)
			}
			if d.Class != tc.class {
				t.Fatalf("Class = %s, want %s", d.Class, tc.class)
			}
			// Pin one bucket per state, except "error" which is documented
			// to have two (host-level -> U, job-level -> E); tell those two
			// apart by class, which the table above already asserts is
			// exact.
			if tc.state == StateError {
				return
			}
			if prior, ok := seenBucket[tc.state]; ok && prior != d.Bucket {
				t.Fatalf("state %q previously mapped to bucket %s, now %s", tc.state, prior, d.Bucket)
			}
			seenBucket[tc.state] = d.Bucket
			if canon, ok := bucketOf[tc.state]; ok && canon != d.Bucket {
				t.Fatalf("state %q bucket %s does not match the canonical bucketOf table (%s)", tc.state, d.Bucket, canon)
			}
		})
	}
}

// TestDeriveActivityState_Precedence pins concrete conflicts between two
// simultaneously-true rows, asserting the earlier row in
// 11-state-model.md section 1.1 always wins.
func TestDeriveActivityState_Precedence(t *testing.T) {
	t.Run("homing beats printing (row4 before row12)", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "printing"
		s.PrintStats.PrintDuration = 10
		s.PauseResume.IsPaused = boolPtr(false)
		s.VirtualSDCard.IsActive = boolPtr(true)
		s.MotorControl.IsHoming = boolPtr(true)
		d := DeriveActivityState(s, nil)
		if d.State != StateHoming {
			t.Fatalf("State = %q, want %q", d.State, StateHoming)
		}
	})

	t.Run("calibrating beats paused (row6 before row9)", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "paused"
		s.PauseResume.IsPaused = boolPtr(true)
		s.CustomMacro.LevelingCalibration = intPtr(1)
		d := DeriveActivityState(s, nil)
		if d.State != StateCalibrating {
			t.Fatalf("State = %q, want %q", d.State, StateCalibrating)
		}
	})

	t.Run("paused beats a pending resume that has not yet flipped is_paused (row9 before row10)", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "paused"
		s.PauseResume.IsPaused = boolPtr(true)
		pending := &PendingAction{Kind: PendingResume, IssuedAt: fixedNow.Add(-1 * time.Second), Timeout: 120 * time.Second}
		d := DeriveActivityState(s, pending)
		if d.State != StatePaused {
			t.Fatalf("State = %q, want %q (a resume in flight must not override a still-genuinely-paused printer, 11-state-model.md row 9 before row 10)", d.State, StatePaused)
		}
	})

	t.Run("a settled cancel clears the cancelling transition even if pendingAction is still set (row7's own not-yet-settled precondition)", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "cancelled"
		s.VirtualSDCard.IsActive = boolPtr(false)
		s.IdleTimeout.State = strPtr("Ready")
		pending := &PendingAction{Kind: PendingCancel, IssuedAt: fixedNow.Add(-1 * time.Second), Timeout: 60 * time.Second}
		d := DeriveActivityState(s, pending)
		if d.State != StateCancelled {
			t.Fatalf("State = %q, want %q", d.State, StateCancelled)
		}
	})

	t.Run("a timed-out pending action stops matching its transition row", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "paused"
		s.PauseResume.IsPaused = boolPtr(true)
		pending := &PendingAction{Kind: PendingPause, IssuedAt: fixedNow.Add(-1 * time.Hour), Timeout: 30 * time.Second}
		d := DeriveActivityState(s, pending)
		if d.State != StatePaused {
			t.Fatalf("State = %q, want %q (timed-out pending pause must not force pausing)", d.State, StatePaused)
		}
	})

	t.Run("upgrading beats idle (row15 before row17)", func(t *testing.T) {
		s := syntheticIdle()
		s.WS9999.UpgradeStatus = crealityws.Int{Value: 3, Present: true}
		d := DeriveActivityState(s, nil)
		if d.State != StateUpgrading {
			t.Fatalf("State = %q, want %q", d.State, StateUpgrading)
		}
	})
}

// TestDeriveActivityState_FailClosedOnMissingFields checks every field this
// package documents as safety-critical: a nil pointer for it must never let
// the derivation conclude a safe or settled state.
func TestDeriveActivityState_FailClosedOnMissingFields(t *testing.T) {
	tests := []struct {
		name string
		snap func() Snapshot
	}{
		{"print_stats missing entirely", func() Snapshot {
			s := syntheticIdle()
			s.PrintStats = nil
			return s
		}},
		{"pause_resume missing while printing", func() Snapshot {
			s := syntheticIdle()
			s.PrintStats.State = "printing"
			s.PrintStats.PrintDuration = 10
			s.VirtualSDCard.IsActive = boolPtr(true)
			s.PauseResume = nil
			return s
		}},
		{"pause_resume.is_paused missing while printing", func() Snapshot {
			s := syntheticIdle()
			s.PrintStats.State = "printing"
			s.PrintStats.PrintDuration = 10
			s.VirtualSDCard.IsActive = boolPtr(true)
			s.PauseResume.IsPaused = nil
			return s
		}},
		{"virtual_sdcard missing while printing", func() Snapshot {
			s := syntheticIdle()
			s.PrintStats.State = "printing"
			s.PrintStats.PrintDuration = 10
			s.PauseResume.IsPaused = boolPtr(false)
			s.VirtualSDCard = nil
			return s
		}},
		{"webhooks missing at idle", func() Snapshot {
			s := syntheticIdle()
			s.Webhooks = nil
			return s
		}},
		{"webhooks.state missing at idle", func() Snapshot {
			s := syntheticIdle()
			s.Webhooks.State = nil
			return s
		}},
		{"idle_timeout missing while cancelled", func() Snapshot {
			s := syntheticIdle()
			s.PrintStats.State = "cancelled"
			s.VirtualSDCard.IsActive = boolPtr(false)
			s.IdleTimeout = nil
			return s
		}},
		{"idle_timeout missing at idle", func() Snapshot {
			s := syntheticIdle()
			s.IdleTimeout = nil
			return s
		}},
		{"idle_timeout.state missing at idle", func() Snapshot {
			s := syntheticIdle()
			s.IdleTimeout.State = nil
			return s
		}},
		{"idle_timeout missing while complete", func() Snapshot {
			s := syntheticIdle()
			s.PrintStats.State = "complete"
			s.IdleTimeout = nil
			return s
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := DeriveActivityState(tc.snap(), nil)
			if d.State != StateUnknown || d.Class != ClassUnknownFailClosed || d.Bucket != BucketU {
				t.Fatalf("got state=%s bucket=%s class=%s, want unknown/U/unknown_fail_closed (reasons: %v)",
					d.State, d.Bucket, d.Class, d.Reasons)
			}
		})
	}
}

// TestCFSConnectedMidPrint pins the rule that survives from safety-architecture.md D5: while the CFS
// reports connected, the bucket must stay whatever printing/paused naturally
// derive to (P/Z), not collapse to U, because pause and cancel of a running
// print must stay reachable. internal/policy is responsible for combining
// Bucket and CFSConnected to actually block start/resume/setpoints while
// still allowing pause/cancel; this package only guarantees the inputs to
// that decision are correct.
func TestCFSConnectedMidPrint(t *testing.T) {
	t.Run("printing with CFS connected", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "printing"
		s.PrintStats.PrintDuration = 10
		s.PauseResume.IsPaused = boolPtr(false)
		s.VirtualSDCard.IsActive = boolPtr(true)
		s.Box.State = strPtr("idle") // anything other than "disconnect"
		d := DeriveActivityState(s, nil)
		if d.State != StatePrinting || d.Bucket != BucketP {
			t.Fatalf("state/bucket = %s/%s, want printing/P (bucket must not collapse to U while CFS is connected, D5)", d.State, d.Bucket)
		}
		if !d.CFSConnected {
			t.Fatal("CFSConnected = false, want true")
		}
	})

	t.Run("paused with CFS connected", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "paused"
		s.PauseResume.IsPaused = boolPtr(true)
		s.Box.State = strPtr("idle")
		d := DeriveActivityState(s, nil)
		if d.State != StatePaused || d.Bucket != BucketZ {
			t.Fatalf("state/bucket = %s/%s, want paused/Z", d.State, d.Bucket)
		}
		if !d.CFSConnected {
			t.Fatal("CFSConnected = false, want true")
		}
	})

	t.Run("box.state missing fails closed to connected", func(t *testing.T) {
		s := syntheticIdle()
		s.Box.State = nil
		d := DeriveActivityState(s, nil)
		if !d.CFSConnected {
			t.Fatal("CFSConnected = false, want true (missing box.state must fail closed)")
		}
	})

	t.Run("box object missing entirely fails closed to connected", func(t *testing.T) {
		s := syntheticIdle()
		s.Box = nil
		d := DeriveActivityState(s, nil)
		if !d.CFSConnected {
			t.Fatal("CFSConnected = false, want true (missing box object must fail closed)")
		}
	})

	t.Run("9999 cfsConnect nonzero overrides a disconnected box.state", func(t *testing.T) {
		s := syntheticIdle()
		s.Box.State = strPtr("disconnect")
		s.WS9999.CfsConnect = crealityws.Int{Value: 1, Present: true}
		d := DeriveActivityState(s, nil)
		if !d.CFSConnected {
			t.Fatal("CFSConnected = false, want true (9999 cfsConnect corroboration)")
		}
	})

	t.Run("disconnected box and no 9999 corroboration is not connected", func(t *testing.T) {
		s := syntheticIdle()
		d := DeriveActivityState(s, nil)
		if d.CFSConnected {
			t.Fatal("CFSConnected = true, want false on the disconnected baseline")
		}
	})
}

// TestWS9999Unreachable pins 11-state-model.md section 2.3: 9999 being
// unreachable must only remove the 9999-only signals (upgrading,
// recovery_pending here), never fail closed an otherwise
// Moonraker-answerable decision.
func TestWS9999Unreachable(t *testing.T) {
	t.Run("printing is unaffected by an unreachable 9999", func(t *testing.T) {
		s := syntheticIdle()
		s.PrintStats.State = "printing"
		s.PrintStats.PrintDuration = 10
		s.PauseResume.IsPaused = boolPtr(false)
		s.VirtualSDCard.IsActive = boolPtr(true)
		s.WS9999 = crealityws.Status{}
		s.WS9999Reachable = false
		d := DeriveActivityState(s, nil)
		if d.State != StatePrinting || d.Bucket != BucketP || d.Class != ClassBusy {
			t.Fatalf("state/bucket/class = %s/%s/%s, want printing/P/busy", d.State, d.Bucket, d.Class)
		}
	})

	t.Run("idle is unaffected by an unreachable 9999", func(t *testing.T) {
		s := syntheticIdle()
		s.WS9999 = crealityws.Status{}
		s.WS9999Reachable = false
		d := DeriveActivityState(s, nil)
		if d.State != StateIdle {
			t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StateIdle, d.Reasons)
		}
	})

	t.Run("upgrading and recovery_pending never fire while 9999 is unreachable", func(t *testing.T) {
		s := syntheticIdle()
		s.WS9999 = crealityws.Status{} // Present false for everything
		s.WS9999Reachable = false
		d := DeriveActivityState(s, nil)
		if d.State == StateUpgrading || d.State == StateRecoveryPending {
			t.Fatalf("State = %q, want a Moonraker-derivable state, not a 9999-only one, when 9999 is unreachable", d.State)
		}
	})
}
