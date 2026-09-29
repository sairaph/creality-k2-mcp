package printerstate

import (
	"fmt"
	"strings"
)

// DeriveActivityState is the one authoritative activity-state derivation
// (P2). It evaluates references/analysis/11-state-model.md section 1.1's 19
// rows in exactly the listed order (plus the identity-verification check
// added for review backlog item 24, evaluated right after Klipper is
// confirmed ready and before any of those 19 rows), first match wins, and
// fails closed (state "unknown", class unknown_fail_closed) whenever a
// field needed to positively confirm a safe or settled state is missing,
// per P1. pending is
// the caller-owned (internal/policy) record of a lifecycle write this server
// itself issued but has not yet seen settle; a nil pending is always
// inactive, matching "no write in flight".
func DeriveActivityState(snap Snapshot, pending *PendingAction) Derived {
	d := deriveActivityState(snap, pending)
	// The CFS flags are attached here, once, for every return path of the
	// derivation below (plan-v0.2.0.md section 2.3): a return site that
	// forgot them would leave CFSError false, which is fail-open. d.CFSConnected
	// (not a fresh cfsConnected call) drives the flags because the early-return
	// rows report it true without inspecting the box at all.
	d.CFSKnown, d.CFSQuiescent, d.CFSError, d.CFSReasons = cfsFlagsFor(snap, d.CFSConnected)
	return d
}

// deriveActivityState is the row-by-row derivation DeriveActivityState wraps.
func deriveActivityState(snap Snapshot, pending *PendingAction) Derived {
	now := snap.Taken

	// Row 1: offline. server/info itself could not be reached.
	if snap.ServerInfoErr != nil {
		return unknown(StateOffline, BucketU, true,
			fmt.Sprintf("server/info unreachable: %v", snap.ServerInfoErr))
	}

	// Row 2: klippy_not_ready.
	if !snap.ServerInfo.KlippyConnected {
		return unknown(StateKlippyNotReady, BucketU, true,
			"server/info.klippy_connected is false")
	}
	switch snap.ServerInfo.KlippyState {
	case "ready":
		// continue to the object-query-based rows below.
	case "startup", "shutdown", "disconnected":
		return unknown(StateKlippyNotReady, BucketU, true,
			"server/info.klippy_state is "+snap.ServerInfo.KlippyState)
	case "error":
		// Row 3, host-level error.
		return unknown(StateError, BucketU, true,
			"server/info.klippy_state is error (host-level)")
	default:
		return unknown(StateKlippyNotReady, BucketU, true,
			fmt.Sprintf("server/info.klippy_state %q is not a recognised value", snap.ServerInfo.KlippyState))
	}

	// Identity verification (review backlog item 24), evaluated as soon as
	// Klipper is confirmed ready and before anything else below is trusted:
	// a registry-backed printer's persisted hostname must still be the one
	// actually answering at this address. A printer with no persisted
	// hostname (the K2_MCP_HOST environment override) has nothing to check
	// here; internal/policy resolves and verifies its identity itself
	// before ever locking or writing (execute.go resolveExecuteIdentity).
	if state, reason, ok := checkIdentity(snap); !ok {
		return unknown(state, BucketU, true, reason)
	}

	cfsOK, cfsReason := cfsConnected(snap)

	// Row 3, job-level error. The critical rule from 11-state-model.md
	// section 1.1 row 3: this uses only Moonraker's own print_stats.state,
	// never the 9999 err object (which is display/diagnostic-only, never a
	// gating input here, per P3 and 09-creality-state-machine.md section 1.7)
	// so a stale, uncleared 9999 error code can never make a running print
	// un-pausable through this derivation.
	if snap.PrintStats != nil && snap.PrintStats.State == "error" {
		return Derived{
			State:        StateError,
			Bucket:       BucketE,
			Class:        ClassBusy,
			CFSConnected: cfsOK,
			Reasons:      []string{"print_stats.state is error (job-level); clear it on the printer screen or in Creality Print", cfsReason},
		}
	}

	// Row 8a: resuming, signal-fed (supervised session 2026-09-29). The K2's
	// RESUME macro (reheat, purge, wipe) runs for 60 to 75 s with print_stats
	// still "paused" and pause_resume.is_paused still true; 9999 state 8 is that
	// routine. Without this row the printer derives paused for the whole time and
	// a second resume would be proposed under a running one. Bucket T, so every
	// write is refused except the explicit cancel exception (cancel queues behind
	// the macro; stopping must never be blocked). It is evaluated before the
	// homing and calibrating rows because RESUME homes X/Y first when they are
	// unhomed: cancel must stay allowed through that sub-phase too.
	if snap.PrintStats != nil && snap.PrintStats.State == "paused" && snap.WS9999.State.Present && snap.WS9999.State.Value == 8 {
		return transitioning(StateResuming, cfsOK, cfsReason,
			"9999 state is 8 (the RESUME routine: reheat, purge, wipe) while print_stats is still paused")
	}

	// Row 4: homing.
	if ok, reason := isHoming(snap); ok {
		return busy(StateHoming, BucketB, cfsOK, reason, cfsReason)
	}

	// Row 5: preparing.
	if ok, reason := isPreparing(snap); ok {
		return busy(StatePreparing, BucketPP, cfsOK, reason, cfsReason)
	}

	// Row 6: calibrating.
	if ok, reason := isCalibrating(snap); ok {
		return busy(StateCalibrating, BucketB, cfsOK, reason, cfsReason)
	}

	// Row 7: cancelling (transition).
	if pending.Active(now, PendingCancel) && !isSettledCancelled(snap) {
		return transitioning(StateCancelling, cfsOK, cfsReason,
			fmt.Sprintf("cancel pending since %s, not yet settled", pending.IssuedAt.Format(rfc3339)))
	}

	// Row 8: pausing (transition).
	if pending.Active(now, PendingPause) && !isSettledPaused(snap) {
		return transitioning(StatePausing, cfsOK, cfsReason,
			fmt.Sprintf("pause pending since %s, not yet settled", pending.IssuedAt.Format(rfc3339)))
	}

	// Row 9: paused.
	if isSettledPaused(snap) {
		return Derived{
			State:        StatePaused,
			Bucket:       BucketZ,
			Class:        ClassBusy,
			CFSConnected: cfsOK,
			Reasons:      []string{"print_stats.state is paused and pause_resume.is_paused is true", cfsReason},
		}
	}

	// Row 10: resuming (transition).
	if pending.Active(now, PendingResume) && !isSettledPrinting(snap) {
		return transitioning(StateResuming, cfsOK, cfsReason,
			fmt.Sprintf("resume pending since %s, not yet settled", pending.IssuedAt.Format(rfc3339)))
	}

	// Row 10a: start window (plan 8a.1, review-2 MF1, safety review M2). After
	// a CFS or spool start frame the printer runs a 3-4 minute self-test
	// before print_stats leaves its previous value, which would otherwise
	// derive as idle, complete or cancelled (bucket I) and let every write
	// through while blocking cancel. print_stats standby, complete or
	// cancelled (Klipper keeps the previous job's complete or cancelled until
	// the next job starts) plus a 9999 withSelfTest that is not 100, or a
	// non-identity box.map, is "preparing" (bucket PP: cancel allowed,
	// everything else refused). It sits before the cancelled and complete rows
	// so they cannot grant bucket I first, and after paused, printing's own
	// rows and the transitions, so it can never shadow printing or paused. The
	// policy layer adds its own in-flight record for the window before either
	// signal has appeared.
	if ok, reason := startWindowSignal(snap); ok {
		d := busy(StatePreparing, BucketPP, cfsOK, reason, cfsReason)
		d.StartWindow = true
		return d
	}

	// Row 11: cancelled (settled).
	if isSettledCancelled(snap) {
		return Derived{
			State:        StateCancelled,
			Bucket:       BucketI,
			Class:        ClassSafeToAct,
			CFSConnected: cfsOK,
			Reasons:      []string{"print_stats.state is cancelled, virtual_sdcard.is_active is false and idle_timeout.state is not Printing", cfsReason},
		}
	}

	// Row 12: printing.
	if isSettledPrinting(snap) {
		return Derived{
			State:        StatePrinting,
			Bucket:       BucketP,
			Class:        ClassBusy,
			CFSConnected: cfsOK,
			Reasons:      []string{"print_stats.state is printing, pause_resume.is_paused is false and virtual_sdcard.is_active is true", cfsReason},
		}
	}

	// Row 13: complete. Observed live (supervised print, 2026-09-29):
	// print_stats complete with idle_timeout Ready, 9999 state 2, deviceState 0
	// and the filament map reset to identity; gated like idle
	// (11-state-model.md section 1.1 row 13). Like row 11
	// (cancelled, settled), this also requires idle_timeout to be known and
	// not "Printing": a "complete" print_stats value while idle_timeout still
	// reports toolhead motion means another actor is running a manual command
	// right now, not that the printer is idle-equivalent (see isBusyCommand,
	// row 18, evaluated below). idle_timeout is used here purely as a
	// negative busy signal, never to positively detect printing itself
	// (11-state-model.md section 1.1 row 17's note).
	if isSettledComplete(snap) {
		return Derived{
			State:        StateComplete,
			Bucket:       BucketI,
			Class:        ClassSafeToAct,
			CFSConnected: cfsOK,
			Reasons:      []string{"print_stats.state is complete and idle_timeout.state is not Printing (observed live after a finished print: idle_timeout Ready, 9999 state 2, deviceState 0)", cfsReason},
		}
	}

	// Row 14: filament_operation / cfs_operation. Two sources: this server's
	// own pending lock on an operation it just started (the conservative
	// signal 11-state-model.md section 1.1 row 14 allows), and, since v0.2.0,
	// the 9999 CFS feed signals (cfsSignalOperation, plan 2.4). It sits after
	// printing, paused and complete on purpose (V6): a feed during a print is
	// ordinary and must not turn printing into bucket U, so a standby printer
	// that is feeding is filament_operation while cancelled/complete stay
	// bucket I with CFSQuiescent false and the policy layer refuses on that.
	if pending.Active(now, PendingFilamentOperation) {
		return unknown(StateFilamentOperation, BucketU, cfsOK,
			"server-initiated filament operation in progress with no reliable completion signal")
	}
	if pending.Active(now, PendingCFSOperation) {
		return unknown(StateCFSOperation, BucketU, cfsOK,
			"server-initiated CFS operation in progress with no reliable completion signal")
	}
	if ok, reason := cfsSignalOperation(snap, cfsOK); ok {
		return unknown(StateFilamentOperation, BucketU, cfsOK, reason)
	}

	// Row 15: upgrading. 9999-only signal; only fires when 9999 answered and
	// reported a non-baseline value (11-state-model.md section 2.3: 9999
	// being unreachable must not fail-close unrelated actions, so an absent
	// reading here simply does not match, it does not force "unknown").
	if snap.WS9999.UpgradeStatus.Present && snap.WS9999.UpgradeStatus.Value != 0 {
		return unknown(StateUpgrading, BucketU, cfsOK,
			fmt.Sprintf("9999 upgradeStatus is %d, not the observed baseline 0", snap.WS9999.UpgradeStatus.Value))
	}

	// Row 16: recovery_pending. Same 9999-only-signal treatment as row 15.
	if snap.WS9999.RepoPlrStatus.Present && snap.WS9999.RepoPlrStatus.Value != 0 {
		return unknown(StateRecoveryPending, BucketU, cfsOK,
			fmt.Sprintf("9999 repoPlrStatus is %d, not the observed baseline 0", snap.WS9999.RepoPlrStatus.Value))
	}

	// Row 18: busy_command, evaluated before row 17 (idle). This is the fix
	// for the HIGH finding that busy_command was unreachable: idle_timeout is
	// a negative busy signal only (11-state-model.md section 1.1 row 17's
	// note: never use it to positively detect printing), but it must still be
	// consulted before granting a safe_to_act bucket, or a long-running
	// manual command/console script started by another actor (touchscreen,
	// Creality Print, another MCP client) - which leaves print_stats in
	// standby/complete/settled-cancelled and virtual_sdcard inactive, exactly
	// like real idle - would be misclassified as idle/safe_to_act. Checking
	// this first, ahead of idle, closes that gap for every print_stats value
	// that would otherwise land in bucket I.
	if ok, reason := isBusyCommand(snap); ok {
		return busy(StateBusyCommand, BucketB, cfsOK, reason, cfsReason)
	}

	// Row 17: idle. isIdle itself also requires idle_timeout to be known and
	// not "Printing" (failing closed otherwise, see isIdle's doc comment), so
	// this row and row 18 above are mutually exclusive by construction; the
	// explicit ordering here is belt-and-suspenders for the busy bucket
	// winning over the safe-to-act one, per P1.
	if ok, reason := isIdle(snap); ok {
		return Derived{
			State:        StateIdle,
			Bucket:       BucketI,
			Class:        ClassSafeToAct,
			CFSConnected: cfsOK,
			Reasons:      []string{reason, cfsReason},
		}
	}

	// Row 19: unknown. Catch-all fail-closed outcome: nothing above matched,
	// which per P1 must never be read as "safe by default".
	return unknown(StateUnknown, BucketU, cfsOK, unknownReason(snap))
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

// unknown builds a Derived for any unknown_fail_closed row.
func unknown(state string, bucket Bucket, cfsOK bool, reason string) Derived {
	return Derived{State: state, Bucket: bucket, Class: ClassUnknownFailClosed, CFSConnected: cfsOK, Reasons: []string{reason}}
}

// busy builds a Derived for a straightforward busy row.
func busy(state string, bucket Bucket, cfsOK bool, reason, cfsReason string) Derived {
	return Derived{State: state, Bucket: bucket, Class: ClassBusy, CFSConnected: cfsOK, Reasons: []string{reason, cfsReason}}
}

// transitioning builds a Derived for one of the three MCP-server-tracked
// transition rows (11-state-model.md section 1.1 rows 7, 8, 10). All three
// gate as "busy" for every check except the operation being confirmed
// (section 1.1 row 7's note), which is exactly ClassTransitioning's meaning:
// internal/policy treats it the same as busy except when resolving the
// in-flight action itself.
func transitioning(state string, cfsOK bool, cfsReason, reason string) Derived {
	return Derived{State: state, Bucket: BucketT, Class: ClassTransitioning, CFSConnected: cfsOK, Reasons: []string{reason, cfsReason}}
}

// HostnamesEqual reports whether a and b name the same Klipper host for
// identity verification purposes (review backlog item 24): the comparison
// is case-insensitive and ignores one trailing dot, since a fully qualified
// name can be written with or without its root dot depending on whether it
// came from mDNS discovery or a printer/info read.
func HostnamesEqual(a, b string) bool {
	norm := func(s string) string {
		return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	}
	return norm(a) == norm(b)
}

// checkIdentity implements review backlog item 24: a registry-backed
// printer's persisted hostname (snap.Printer.Hostname) must be corroborated
// by this same snapshot pass's printer/info read before anything else here
// is trusted. A printer with no persisted hostname (the K2_MCP_HOST
// environment override, domain/env.go, never saved with one) has nothing to
// verify: ok is true and state/reason are unused. Otherwise ok is false
// whenever the live hostname could not be read (state
// StateIdentityUnverified: printer/info failed or came back empty - reads
// may still show this state, but internal/policy fails every write closed
// on it) or disagrees with the persisted one (StateIdentityMismatch: the
// printer now answering at this address may not be the one this registry
// entry was set up for, e.g. DHCP moved the IP).
func checkIdentity(snap Snapshot) (state string, reason string, ok bool) {
	persisted := strings.TrimSpace(snap.Printer.Hostname)
	if persisted == "" {
		return "", "", true
	}
	if snap.PrinterInfoErr != nil {
		return StateIdentityUnverified, fmt.Sprintf(
			"printer/info unreachable: %v; registry hostname %q could not be verified against the live "+
				"printer, so its identity is unverified: writes are refused until it can be confirmed",
			snap.PrinterInfoErr, persisted), false
	}
	live := strings.TrimSpace(snap.PrinterInfo.Hostname)
	if live == "" {
		return StateIdentityUnverified, fmt.Sprintf(
			"printer/info returned an empty hostname; registry hostname %q could not be verified against "+
				"the live printer, so its identity is unverified: writes are refused until it can be confirmed",
			persisted), false
	}
	if !HostnamesEqual(persisted, live) {
		return StateIdentityMismatch, fmt.Sprintf(
			"registry hostname %q does not match the live Klipper hostname %q; the printer now answering at "+
				"this address may not be the one this registry entry was set up for (e.g. DHCP moved the IP). "+
				"Re-run discovery (printers scan, the install wizard, or the TUI) to update the registry",
			persisted, live), false
	}
	return "", "", true
}

// cfsConnected implements dev_docs/safety-architecture.md section 3.1's CFS
// rule: box.state other than "disconnect" (including box.state missing, or
// the whole box object missing), OR 9999 cfsConnect present and non-zero, OR
// any per-unit state (Box.Units T1..T4) equal to "connect".
// Both box.State==nil and Box==nil fail closed to "connected" per
// moonraker.Box.Connected's own doc comment and P1: an unrecognised or
// missing state must never be silently treated as safe to control around.
func cfsConnected(snap Snapshot) (bool, string) {
	if snap.Box == nil {
		return true, "cfs_connected: box object missing from snapshot, failing closed"
	}
	if snap.Box.Connected() {
		if snap.Box.State == nil {
			return true, "cfs_connected: box.state missing from snapshot, failing closed"
		}
		return true, fmt.Sprintf("cfs_connected: box.state is %q", *snap.Box.State)
	}
	if snap.WS9999.CfsConnect.Present && snap.WS9999.CfsConnect.Value != 0 {
		return true, fmt.Sprintf("cfs_connected: box.state is disconnect but 9999 cfsConnect is %d", snap.WS9999.CfsConnect.Value)
	}
	// A brief bus drop can report box.state disconnect while a unit still
	// says connect (plan 2.2); the CFS rules must not lift on that.
	if snap.Box.AnyUnitConnected() {
		return true, "cfs_connected: box.state is disconnect but a unit (T1..T4) still reports connect"
	}
	return false, "cfs_connected: box.state is disconnect and 9999 cfsConnect is 0 or unreported"
}

// isHoming implements row 4: motor_control.is_homing, corroborated by 9999
// deviceState==7. A nil MotorControl or IsHoming, or an unreachable/silent
// 9999, simply means this row cannot positively confirm homing; it does not
// itself force "unknown" (homing is a positive-signal row, not a settled/safe
// one).
func isHoming(snap Snapshot) (bool, string) {
	if snap.MotorControl != nil && snap.MotorControl.IsHoming != nil && *snap.MotorControl.IsHoming {
		return true, "motor_control.is_homing is true"
	}
	if snap.WS9999.DeviceState.Present && snap.WS9999.DeviceState.Value == 7 {
		return true, "9999 deviceState is 7"
	}
	return false, ""
}

// isPreparing implements row 5: print_stats.state=="printing" with
// print_duration==0 while virtual_sdcard.is_active is true, i.e. still inside
// START_PRINT's own prepare/heat/home/clean sequence. Informational only
// (11-state-model.md section 1.1 row 5): both preparing and printing gate as
// busy, so a nil IsActive here still leaves this row simply not matching,
// falling through to row 12 (printing) instead of failing anything closed.
func isPreparing(snap Snapshot) (bool, string) {
	if snap.PrintStats == nil {
		return false, ""
	}
	if snap.PrintStats.State != "printing" || snap.PrintStats.PrintDuration != 0 {
		return false, ""
	}
	if snap.VirtualSDCard == nil || snap.VirtualSDCard.IsActive == nil || !*snap.VirtualSDCard.IsActive {
		return false, ""
	}
	return true, "print_stats.state is printing with print_duration 0 and virtual_sdcard.is_active true (START_PRINT still running)"
}

// isCalibrating implements row 6: any of three OR'd corroboration signals.
// None being available (all nil/absent) means this row cannot positively
// confirm calibrating; it does not fail anything closed by itself.
func isCalibrating(snap Snapshot) (bool, string) {
	if snap.CustomMacro != nil && snap.CustomMacro.LevelingCalibration != nil && *snap.CustomMacro.LevelingCalibration != 0 {
		return true, "custom_macro.leveling_calibration is nonzero"
	}
	// bed_mesh_calibate_state stays true for the whole print after the
	// pre-print self-test levels the bed (observed live 2026-09-29, CFS print
	// started with the self-test). A job that is printing or paused is
	// therefore never "calibrating" because of this flag alone: otherwise the
	// running print derives bucket B and pause/cancel become unavailable,
	// which "stopping must never be blocked" forbids.
	if snap.VirtualSDCard != nil && snap.VirtualSDCard.BedMeshCalibrateState != nil && *snap.VirtualSDCard.BedMeshCalibrateState &&
		!jobPrintingOrPaused(snap) {
		return true, "virtual_sdcard.bed_mesh_calibate_state is true"
	}
	if snap.WS9999.Raw != nil {
		if v, ok := snap.WS9999.Raw["bedTempAutoPid"]; ok && truthy(v) {
			return true, "9999 bedTempAutoPid is 1"
		}
		if v, ok := snap.WS9999.Raw["nozzleTempAutoPid"]; ok && truthy(v) {
			return true, "9999 nozzleTempAutoPid is 1"
		}
	}
	return false, ""
}

// jobPrintingOrPaused reports whether print_stats shows a job that is
// printing or paused.
func jobPrintingOrPaused(snap Snapshot) bool {
	return snap.PrintStats != nil && (snap.PrintStats.State == "printing" || snap.PrintStats.State == "paused")
}

// truthy reports whether a raw decoded JSON value (float64, string or bool,
// as crealityws.Status.Raw stores them) represents a nonzero/true flag.
func truthy(v any) bool {
	switch t := v.(type) {
	case float64:
		return t != 0
	case bool:
		return t
	case string:
		return t != "" && t != "0"
	default:
		return false
	}
}

// isSettledPaused implements the settled condition backing both row 9 and
// row 8's negation: print_stats.state=="paused" AND pause_resume.is_paused.
// A nil PrintStats/PauseResume/IsPaused means "not confirmed paused", never
// "confirmed not paused" (P1): the caller falls through to later rows, which
// eventually fail closed to unknown if nothing else matches either.
func isSettledPaused(snap Snapshot) bool {
	if snap.PrintStats == nil || snap.PrintStats.State != "paused" {
		return false
	}
	if snap.PauseResume == nil || snap.PauseResume.IsPaused == nil {
		return false
	}
	return *snap.PauseResume.IsPaused
}

// isSettledCancelled implements row 11's settle condition:
// print_stats.state=="cancelled" AND virtual_sdcard.is_active==false AND
// idle_timeout.state!="Printing".
func isSettledCancelled(snap Snapshot) bool {
	if snap.PrintStats == nil || snap.PrintStats.State != "cancelled" {
		return false
	}
	if snap.VirtualSDCard == nil || snap.VirtualSDCard.IsActive == nil || *snap.VirtualSDCard.IsActive {
		return false
	}
	if snap.IdleTimeout == nil || snap.IdleTimeout.State == nil {
		return false
	}
	return *snap.IdleTimeout.State != "Printing"
}

// isSettledComplete implements row 13's settle condition: print_stats.state
// == "complete" AND idle_timeout is known and not "Printing". idle_timeout is
// used here purely as a negative busy signal, exactly as row 11's settled-
// cancelled condition uses it: a missing idle_timeout or missing state cannot
// positively confirm the printer is not busy with someone else's command, so
// it does not settle here (P1); the caller falls through, eventually failing
// closed to unknown if nothing else matches.
func isSettledComplete(snap Snapshot) bool {
	if snap.PrintStats == nil || snap.PrintStats.State != "complete" {
		return false
	}
	if snap.IdleTimeout == nil || snap.IdleTimeout.State == nil {
		return false
	}
	return *snap.IdleTimeout.State != "Printing"
}

// isSettledPrinting implements row 12's condition:
// print_stats.state=="printing" AND pause_resume.is_paused==false AND
// virtual_sdcard.is_active==true.
func isSettledPrinting(snap Snapshot) bool {
	if snap.PrintStats == nil || snap.PrintStats.State != "printing" {
		return false
	}
	if snap.PauseResume == nil || snap.PauseResume.IsPaused == nil || *snap.PauseResume.IsPaused {
		return false
	}
	if snap.VirtualSDCard == nil || snap.VirtualSDCard.IsActive == nil || !*snap.VirtualSDCard.IsActive {
		return false
	}
	return true
}

// isIdle implements row 17. Every one of the five conditions must be
// positively known; a missing field means this row cannot fire, which
// (combined with idle being fail-closed's most common false-positive risk)
// is exactly why each check requires its pointer to be non-nil rather than
// treating a nil as false. The fifth condition, idle_timeout present and not
// "Printing", is the fix for the HIGH finding that busy_command (row 18) was
// unreachable: without it, a printer busy running a long command started by
// another actor (print_stats standby, virtual_sdcard inactive, but
// idle_timeout reporting real toolhead motion) used to derive as idle here.
// idle_timeout is used purely as a negative busy signal, never to positively
// detect printing (11-state-model.md section 1.1 row 17's note); a missing
// idle_timeout or missing state fails this row closed, same as every other
// missing field here, not just this one.
func isIdle(snap Snapshot) (bool, string) {
	if snap.PrintStats == nil || snap.PrintStats.State != "standby" {
		return false, ""
	}
	if snap.PauseResume == nil || snap.PauseResume.IsPaused == nil || *snap.PauseResume.IsPaused {
		return false, ""
	}
	if snap.VirtualSDCard == nil || snap.VirtualSDCard.IsActive == nil || *snap.VirtualSDCard.IsActive {
		return false, ""
	}
	if snap.Webhooks == nil || snap.Webhooks.State == nil || *snap.Webhooks.State != "ready" {
		return false, ""
	}
	if snap.IdleTimeout == nil || snap.IdleTimeout.State == nil || *snap.IdleTimeout.State == "Printing" {
		return false, ""
	}
	return true, "print_stats.state is standby, not paused, virtual_sdcard is not active, webhooks.state is ready and idle_timeout.state is not Printing"
}

// isBusyCommand implements row 18: idle_timeout.state=="Printing" while the
// job-level signal (print_stats.state) would otherwise place the printer in
// the safe-to-act bucket I - standby (idle), settled cancelled, or complete -
// meaning some other actor is moving the toolhead with a manual command or
// console script right now, not a job. idle_timeout is used here purely as a
// negative busy signal, never to positively detect printing itself
// (11-state-model.md section 1.1 row 17's note); this is its one legitimate
// use, shared with isIdle and isSettledComplete/isSettledCancelled's own
// negative checks. The cancelled case additionally requires virtual_sdcard to
// be settled inactive, matching row 11's own settle condition, so this does
// not fire mid-cancel (row 7 handles that transition).
func isBusyCommand(snap Snapshot) (bool, string) {
	if snap.IdleTimeout == nil || snap.IdleTimeout.State == nil || *snap.IdleTimeout.State != "Printing" {
		return false, ""
	}
	if snap.PrintStats == nil {
		return false, ""
	}
	switch snap.PrintStats.State {
	case "standby":
		return true, "idle_timeout.state is Printing while print_stats.state is standby (manual motion, not a job)"
	case "complete":
		return true, "idle_timeout.state is Printing while print_stats.state is complete (manual motion after completion, not a job)"
	case "cancelled":
		if snap.VirtualSDCard != nil && snap.VirtualSDCard.IsActive != nil && !*snap.VirtualSDCard.IsActive {
			return true, "idle_timeout.state is Printing while print_stats.state is cancelled and virtual_sdcard is not active (manual motion after cancel, not a job)"
		}
		return false, ""
	default:
		return false, ""
	}
}

// unknownReason builds a diagnostic reason for the row 19 catch-all, naming
// whichever central fields were unavailable so a caller sees why nothing
// above matched, per P1's "name the field" requirement and P6.
func unknownReason(snap Snapshot) string {
	var missing []string
	if snap.PrintStats == nil {
		missing = append(missing, "print_stats")
	} else if snap.PrintStats.State == "" {
		missing = append(missing, "print_stats.state (empty)")
	}
	if snap.PauseResume == nil || snap.PauseResume.IsPaused == nil {
		missing = append(missing, "pause_resume.is_paused")
	}
	if snap.VirtualSDCard == nil || snap.VirtualSDCard.IsActive == nil {
		missing = append(missing, "virtual_sdcard.is_active")
	}
	if snap.Webhooks == nil || snap.Webhooks.State == nil {
		missing = append(missing, "webhooks.state")
	}
	if snap.IdleTimeout == nil || snap.IdleTimeout.State == nil {
		missing = append(missing, "idle_timeout.state")
	}
	if len(missing) == 0 {
		if snap.PrintStats != nil {
			return fmt.Sprintf("no known state signature matched (print_stats.state %q did not match any recognised combination)", snap.PrintStats.State)
		}
		return "no known state signature matched"
	}
	return fmt.Sprintf("could not confirm a safe or settled state: missing %v", missing)
}
