// Temperature ceilings and other pure limit checks (plan decision 6,
// safety-architecture D1). No I/O: callers supply whatever live state they
// already read.
package domain

import (
	"errors"
	"math"
)

// Fixed, non-configurable code ceilings. Both sit above every known K2
// product_param value and below Klipper's firmware max_temp (extruder 390,
// bed 115), so a caller can never command a value the firmware itself would
// refuse, and the code ceiling can only ever tighten the effective limit,
// never loosen it.
const (
	NozzleCeilingC = 320.0
	BedCeilingC    = 110.0
)

// ErrLiveCapUnknown is returned by the effective-limit functions when the
// live cap has not been read from the printer's product_param. This makes
// the fail-closed rule of plan decision 6 enforceable in the API instead of
// documentation-only: there is no value a caller can pass for an unread live
// cap that still produces a usable limit, so a caller cannot proceed with a
// temperature write without first getting a successful live-cap read.
var ErrLiveCapUnknown = errors.New("live product_param cap not read")

// LiveCap is the live temperature cap read from the printer's product_param.
// Known is false when the read has not happened, or failed; that is distinct
// from a read that succeeded and returned a permissive value, so the zero
// value of LiveCap means "not read", never "no cap".
type LiveCap struct {
	Value float64
	Known bool
}

// EffectiveTemperatureLimit is the minimum of the code ceiling, the live cap
// read from the printer's product_param, and an optional user-configured
// soft cap. It returns ErrLiveCapUnknown when liveCap.Known is false, so a
// caller cannot obtain a limit at all without a successful live-cap read
// (plan decision 6). A nil softCap is ignored.
func EffectiveTemperatureLimit(ceiling float64, liveCap LiveCap, softCap *float64) (float64, error) {
	if !liveCap.Known {
		return 0, ErrLiveCapUnknown
	}
	limit := ceiling
	if liveCap.Value < limit {
		limit = liveCap.Value
	}
	if softCap != nil && *softCap < limit {
		limit = *softCap
	}
	return limit, nil
}

// EffectiveNozzleLimit applies EffectiveTemperatureLimit with the nozzle code
// ceiling.
func EffectiveNozzleLimit(liveCap LiveCap, softCap *float64) (float64, error) {
	return EffectiveTemperatureLimit(NozzleCeilingC, liveCap, softCap)
}

// EffectiveBedLimit applies EffectiveTemperatureLimit with the bed code
// ceiling.
func EffectiveBedLimit(liveCap LiveCap, softCap *float64) (float64, error) {
	return EffectiveTemperatureLimit(BedCeilingC, liveCap, softCap)
}

// WithinBand reports whether target is within +-band of current. Used for the
// mid-print setpoint bands (safety-architecture D1): nozzle +-10 C, bed +-5 C.
func WithinBand(current, target, band float64) bool {
	return math.Abs(target-current) <= band
}

// WithinRange reports whether value falls within [min, max] inclusive. Used
// for the speed (50-150%) and flow (90-110%) factor bands.
func WithinRange(value, min, max float64) bool {
	return value >= min && value <= max
}

// PartFanAboveFloor reports whether requestedPercent is at least
// minPercentOfCurrent percent of currentPercent (safety-architecture D1: the
// part fan must not be dropped below 50% of its current value in a single
// mid-print change).
func PartFanAboveFloor(currentPercent, requestedPercent, minPercentOfCurrent float64) bool {
	floor := currentPercent * minPercentOfCurrent / 100
	return requestedPercent >= floor
}
