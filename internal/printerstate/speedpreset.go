package printerstate

import "math"

// QmodeState is the tri-state Silent-mode (Creality's Qmode macro) state
// derived once per snapshot (plan-v0.3.0.md 2a.1). The zero value is
// QmodeUnknown so a Derived built any way but DeriveActivityState fails
// closed. Every use of the Silent flag (gates, no_change, display, warnings)
// reads only this, never the raw fields.
type QmodeState int

const (
	// QmodeUnknown: the flag was not positively read, or the two sources
	// disagree.
	QmodeUnknown QmodeState = iota
	// QmodeOff: Silent is not active.
	QmodeOff
	// QmodeOn: Silent is active (velocity, acceleration, pressure advance and
	// fans are held by the Qmode macro; the speed factor was set to 50%).
	QmodeOn
)

func (q QmodeState) String() string {
	switch q {
	case QmodeOff:
		return "off"
	case QmodeOn:
		return "on"
	}
	return "unknown"
}

// qmodeFor derives the Silent-mode state: On or Off only when
// custom_macro.qmode_flag and gcode_macro Qmode.flag are both present, are
// each 0 or 1, and agree (the macro sets its own variable and calls
// SET_QMODE_FLAG as two separate lines, so an aborted macro can desync them);
// anything else is unknown.
func qmodeFor(snap Snapshot) QmodeState {
	if snap.CustomMacro == nil || snap.CustomMacro.QmodeFlag == nil || snap.QmodeMacro == nil || snap.QmodeMacro.Flag == nil {
		return QmodeUnknown
	}
	a, ok1 := flagValue(*snap.CustomMacro.QmodeFlag)
	b, ok2 := flagValue(*snap.QmodeMacro.Flag)
	if !ok1 || !ok2 || a != b {
		return QmodeUnknown
	}
	if a {
		return QmodeOn
	}
	return QmodeOff
}

func flagValue(v float64) (on bool, ok bool) {
	switch v {
	case 0:
		return false, true
	case 1:
		return true, true
	}
	return false, false
}

// The speed presets Creality's software offers (protocol doc 2.1). Silent and
// Stable both run at a 50% speed factor and are told apart only by the Silent
// flag.
const (
	PresetSilent     = "silent"
	PresetStable     = "stable"
	PresetStandard   = "standard"
	PresetUltrafast  = "ultrafast"
	PresetCustom     = "custom"
	PresetUnknownStr = "unknown"
)

// PresetFactorPercent is the speed factor a preset sets, and whether name is a
// preset that can be requested.
func PresetFactorPercent(name string) (float64, bool) {
	switch name {
	case PresetSilent, PresetStable:
		return 50, true
	case PresetStandard:
		return 100, true
	case PresetUltrafast:
		return 125, true
	}
	return 0, false
}

func presetNear(a, b float64) bool { return math.Abs(a-b) < 0.5 }

// SpeedPresetOf derives the speed preset shown in the state block: silent iff
// Silent is On, otherwise by the speed factor (50 stable, 100 standard, 125
// ultrafast, else custom). It is shown only in buckets P and Z (plan 2a.10): a
// factor of 100 while idle is just the default, and START_PRINT resets it. The
// result is "unknown" when the Silent flag is unknown, the factor is missing,
// or port 9999 reports a speedMode or curFeedratePct that disagrees with
// Moonraker (fail closed). It returns "" where it is not shown.
func SpeedPresetOf(snap Snapshot, d Derived) string {
	if d.Bucket != BucketP && d.Bucket != BucketZ {
		return ""
	}
	if d.Qmode == QmodeUnknown || snap.GCodeMove == nil || snap.GCodeMove.SpeedFactor == nil {
		return PresetUnknownStr
	}
	factor := *snap.GCodeMove.SpeedFactor * 100
	if snap.WS9999Reachable {
		if snap.WS9999.SpeedMode.Present {
			if (snap.WS9999.SpeedMode.Value == 1) != (d.Qmode == QmodeOn) {
				return PresetUnknownStr
			}
		}
		if snap.WS9999.CurFeedrate.Present && !presetNear(float64(snap.WS9999.CurFeedrate.Value), factor) {
			return PresetUnknownStr
		}
	}
	if d.Qmode == QmodeOn {
		return PresetSilent
	}
	switch {
	case presetNear(factor, 50):
		return PresetStable
	case presetNear(factor, 100):
		return PresetStandard
	case presetNear(factor, 125):
		return PresetUltrafast
	}
	return PresetCustom
}
