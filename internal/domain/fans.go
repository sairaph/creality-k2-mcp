// The verified M106 fan mapping (plan decision 10, live test
// references/printer-snapshot/extra/fan_test_20260928.md). Written once here
// and referenced everywhere else that needs it.
package domain

import (
	"fmt"
	"math"
)

// FanChannel names one of the three independently controllable fans.
type FanChannel string

const (
	FanPart      FanChannel = "part"
	FanCase      FanChannel = "case"
	FanAuxiliary FanChannel = "auxiliary"
)

// FanSpec is everything needed to command and read back one fan channel.
type FanSpec struct {
	Channel FanChannel
	// PParameter is the M106 P index, e.g. "M106 P0".
	PParameter int
	// OutputPin is the Moonraker output_pin object name whose value reports
	// the fan's current PWM fraction (0 to 1).
	OutputPin string
	// MinValue is the PRINTER_PARAM floor the M106 macro maps a nonzero S
	// onto: the macro scales S from [1, 255] to [MinValue, 255], so a
	// nonzero fan never runs below this raw value even at S1.
	MinValue int
}

// Fans is the fixed part/case/auxiliary mapping, verified live 2026-09-28.
// Case shares its output pins with the temperature_fan chamber_fan
// thermostat on purpose (Creality's M141 drives both); a manual case fan
// speed can still be overridden by the thermostat when the chamber crosses
// its target, which tool descriptions built on this table must disclose.
var Fans = map[FanChannel]FanSpec{
	FanPart:      {Channel: FanPart, PParameter: 0, OutputPin: "fan0", MinValue: 25},
	FanCase:      {Channel: FanCase, PParameter: 1, OutputPin: "fan1", MinValue: 50},
	FanAuxiliary: {Channel: FanAuxiliary, PParameter: 2, OutputPin: "fan2", MinValue: 100},
}

// FanChannels lists the three channels in a stable order (part, case,
// auxiliary), for iteration and for schema enums.
var FanChannels = []FanChannel{FanPart, FanCase, FanAuxiliary}

// FanSpecFor looks up a fan channel's spec. ok is false for an unknown
// channel name.
func FanSpecFor(channel FanChannel) (FanSpec, bool) {
	spec, ok := Fans[channel]
	return spec, ok
}

// GCode renders the M106 command for a requested percent (0 to 100),
// e.g. "M106 P0 S128".
func (s FanSpec) GCode(percent float64) string {
	return fmt.Sprintf("M106 P%d S%d", s.PParameter, PercentToS(percent))
}

// PercentToS converts a requested fan percent (0 to 100) to the M106 S value
// (0 to 255): S = round(percent * 255 / 100). Rounding is half away from
// zero, matching the firmware macro's own behavior (verified live: a
// requested 50% sent as S128, not S127 or S128 from truncation).
func PercentToS(percent float64) int {
	return int(math.Round(percent * 255 / 100))
}

// ReportedFanPercent converts a Moonraker output_pin value (0 to 1) back to
// the user-facing percent, undoing the macro's floor mapping: the macro scales
// a nonzero S onto [min, 255], so the inverse is
// pct = (value*255 - min) / (255 - min) * 100. A pin value of exactly 0 means
// the fan is off regardless of min, and is reported as 0 to avoid a spurious
// negative from the floor subtraction.
func ReportedFanPercent(pinValue float64, min int) float64 {
	if pinValue <= 0 {
		return 0
	}
	raw := pinValue*255 - float64(min)
	return raw / (255 - float64(min)) * 100
}
