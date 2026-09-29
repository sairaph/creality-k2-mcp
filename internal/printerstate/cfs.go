package printerstate

import (
	"fmt"
	"sort"
)

// CFS flags (dev_docs/plan-v0.2.0.md sections 2.3 and 8a.6; review B1/B2).
//
// A connected CFS changes what is safe to write, but no single printer field
// says "the CFS is busy". Instead of folding the CFS into the derived state
// (which would shadow printing/paused and make pause and cancel unreachable),
// DeriveActivityState reports four flags next to the bucket and
// internal/policy combines them per action:
//
//   - CFSKnown: every field needed to judge the CFS was positively read.
//   - CFSError: an error signal is present (only meaningful when read).
//   - CFSQuiescent: the IDLE-quiescence predicate. It requires deviceState 0,
//     which is presumably false during every print, so it is only meaningful
//     for bucket-I decisions (8a.6); print-time checks use Known and Error.
//   - CFSReasons: every failing field, with its value or "absent".
//
// The flags are computed once, by cfsFlagsFor, and attached to every Derived
// by the DeriveActivityState wrapper rather than at each return site: a
// Derived whose CFSError is left at its zero value would be fail-open.

// cfsIdleFeedStates are the feedState values that mean the feeder is at rest
// (plan 2.3: 0, 3, 14, 100; dev_docs/cfs-state-analysis.md section 2.2).
var cfsIdleFeedStates = map[int]bool{0: true, 3: true, 14: true, 100: true}

// cfsFlags computes the CFS flags for snap. When the CFS is not connected
// nothing about it matters: Known and Quiescent are true, Error is false and
// there are no reasons.
func cfsFlags(snap Snapshot) (known, quiescent, errFlag bool, reasons []string) {
	connected, _ := cfsConnected(snap)
	return cfsFlagsFor(snap, connected)
}

// cfsFlagsFor is cfsFlags with the connected decision supplied by the caller:
// DeriveActivityState's early-return rows (offline, klippy_not_ready,
// identity) report CFSConnected true without looking at the box at all (fail
// closed), and the flags must agree with that rather than re-derive
// "not connected" from the same snapshot.
func cfsFlagsFor(snap Snapshot, connected bool) (known, quiescent, errFlag bool, reasons []string) {
	if !connected {
		return true, true, false, nil
	}

	ws := snap.WS9999
	known = true
	fail := func(format string, args ...any) {
		known = false
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}

	// Known: every input present.
	if !snap.WS9999Reachable {
		fail("cfs unknown: port 9999 is unreachable")
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"deviceState", ws.DeviceState.Present},
		{"feedState", ws.FeedState.Present},
		{"materialStatus", ws.MaterialStatus.Present},
		{"err", ws.Err.Present},
		{"repoPlrStatus", ws.RepoPlrStatus.Present},
		{"upgradeStatus", ws.UpgradeStatus.Present},
		{"cfsConnect", ws.CfsConnect.Present},
	} {
		if snap.WS9999Reachable && !f.present {
			fail("cfs unknown: 9999 %s is absent", f.name)
		}
	}
	switch {
	case snap.Box == nil:
		fail("cfs unknown: box object is absent")
	case snap.Box.State == nil:
		fail("cfs unknown: box.state is absent")
	case *snap.Box.State != "connect":
		fail("cfs unknown: box.state is %q, not connect", *snap.Box.State)
	}
	if snap.Box != nil && snap.Box.FilamentUseup == nil {
		fail("cfs unknown: box.filament_useup is absent")
	}
	if snap.PauseResume == nil || snap.PauseResume.ResumeErr == nil {
		fail("cfs unknown: pause_resume.resume_err is absent")
	}
	if ws.CfsConnect.Present && ws.CfsConnect.Value != 1 {
		fail("cfs unknown: 9999 cfsConnect is %d but the CFS is treated as connected (box.state disagrees)", ws.CfsConnect.Value)
	}

	// Error: only meaningful for a field that is present.
	flagErr := func(format string, args ...any) {
		errFlag = true
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}
	if ws.Err.Present && (ws.Err.ErrCode != 0 || ws.Err.Key != 0) {
		flagErr("cfs error: 9999 err is errcode %d key %d", ws.Err.ErrCode, ws.Err.Key)
	}
	if ws.MaterialStatus.Present && ws.MaterialStatus.Value != 0 {
		flagErr("cfs error: 9999 materialStatus is %d", ws.MaterialStatus.Value)
	}
	if snap.PauseResume != nil && snap.PauseResume.ResumeErr != nil && *snap.PauseResume.ResumeErr {
		flagErr("cfs error: pause_resume.resume_err is true")
	}
	if snap.Box != nil && snap.Box.FilamentUseup != nil && *snap.Box.FilamentUseup != 0 {
		flagErr("cfs error: box.filament_useup is %d", *snap.Box.FilamentUseup)
	}

	// Quiescent: positive idle values only; everything else is busy.
	quiescent = known && !errFlag
	busyReason := func(format string, args ...any) {
		quiescent = false
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}
	if known && !errFlag {
		if ws.DeviceState.Value != 0 {
			busyReason("cfs busy: 9999 deviceState is %d, not 0", ws.DeviceState.Value)
		}
		if !cfsIdleFeedStates[ws.FeedState.Value] {
			busyReason("cfs busy: 9999 feedState is %d, not one of 0, 3, 14, 100", ws.FeedState.Value)
		}
		if ws.RepoPlrStatus.Value != 0 {
			busyReason("cfs busy: 9999 repoPlrStatus is %d, not 0", ws.RepoPlrStatus.Value)
		}
		if ws.UpgradeStatus.Value != 0 {
			busyReason("cfs busy: 9999 upgradeStatus is %d, not 0", ws.UpgradeStatus.Value)
		}
	}
	return known, quiescent, errFlag, reasons
}

// cfsSignalOperation implements the signal-fed filament_operation row (plan
// 2.4): with a CFS connected and 9999 answering, a feeder that is not at rest
// (feedState present and outside the idle set) or a load/unload in progress
// (deviceState 10 or 11) is a filament operation. Unlike every other 9999
// use in this package this is authoritative on its own, because no Moonraker
// field reports a CFS feed (dev_docs/cfs-state-analysis.md section 8.1); the
// row is evaluated after printing/paused/complete, so it can never shadow
// them.
func cfsSignalOperation(snap Snapshot, cfsOK bool) (bool, string) {
	if !cfsOK || !snap.WS9999Reachable {
		return false, ""
	}
	if fs := snap.WS9999.FeedState; fs.Present && !cfsIdleFeedStates[fs.Value] {
		return true, fmt.Sprintf("9999 feedState is %d with a CFS connected, not one of the idle values 0, 3, 14, 100", fs.Value)
	}
	if ds := snap.WS9999.DeviceState; ds.Present && (ds.Value == 10 || ds.Value == 11) {
		return true, fmt.Sprintf("9999 deviceState is %d (filament loading or unloading) with a CFS connected", ds.Value)
	}
	return false, ""
}

// startWindowSignal implements the printerstate half of plan 8a.1: after a
// CFS or spool start frame the printer runs a 3-4 minute self-test with
// print_stats still at its previous value (standby, or the previous job's
// complete or cancelled: dev_docs/cfs-print-start.md section 4.3, and Klipper
// keeps those until the next job starts), so one of those states plus either
// signal below means a start is in flight. 9999 withSelfTest present and not
// 100 is the self-test progress; a non-identity box.map is the map colorMatch
// wrote for the job (identity again at the end of the print). Printing,
// paused, error and everything else are never considered, so this never
// shadows them.
func startWindowSignal(snap Snapshot) (bool, string) {
	if snap.PrintStats == nil {
		return false, ""
	}
	prev := snap.PrintStats.State
	if prev != "standby" && prev != "complete" && prev != "cancelled" {
		return false, ""
	}
	if st := snap.WS9999.WithSelfTest; st.Present && st.Value != 100 {
		return true, fmt.Sprintf("print_stats.state is %s while 9999 withSelfTest is %d, not 100 (a print start's self-test is running)", prev, st.Value)
	}
	if snap.Box != nil {
		keys := make([]string, 0, len(snap.Box.Map))
		for k := range snap.Box.Map {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if v := snap.Box.Map[k]; v != k {
				return true, fmt.Sprintf("print_stats.state is %s while box.map is not the identity map (%s -> %s): a CFS print start wrote its filament map", prev, k, v)
			}
		}
	}
	return false, ""
}
