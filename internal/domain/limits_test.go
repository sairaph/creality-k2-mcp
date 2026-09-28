package domain

import (
	"errors"
	"testing"
)

func f64(v float64) *float64 { return &v }

func known(v float64) LiveCap { return LiveCap{Value: v, Known: true} }

func TestEffectiveTemperatureLimit(t *testing.T) {
	cases := []struct {
		name    string
		ceiling float64
		liveCap LiveCap
		softCap *float64
		want    float64
	}{
		{"live cap looser than ceiling", NozzleCeilingC, known(NozzleCeilingC), nil, 320},
		{"live cap tighter", NozzleCeilingC, known(300), nil, 300},
		{"live cap looser than ceiling never loosens it", NozzleCeilingC, known(400), nil, 320},
		{"soft cap tighter than both", NozzleCeilingC, known(300), f64(250), 250},
		{"soft cap looser than live cap", NozzleCeilingC, known(300), f64(310), 300},
		{"bed ceiling with live cap", BedCeilingC, known(100), nil, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := EffectiveTemperatureLimit(c.ceiling, c.liveCap, c.softCap)
			if err != nil {
				t.Fatalf("EffectiveTemperatureLimit(%v, %v, %v) error = %v, want nil", c.ceiling, c.liveCap, c.softCap, err)
			}
			if got != c.want {
				t.Errorf("EffectiveTemperatureLimit(%v, %v, %v) = %v, want %v", c.ceiling, c.liveCap, c.softCap, got, c.want)
			}
		})
	}
}

// TestEffectiveTemperatureLimitFailsClosedWhenLiveCapUnknown checks that an
// unread live cap (the zero value of LiveCap) makes a limit unobtainable
// rather than silently falling back to the code ceiling: fail closed must be
// enforced by the API, not left to caller discipline.
func TestEffectiveTemperatureLimitFailsClosedWhenLiveCapUnknown(t *testing.T) {
	if _, err := EffectiveTemperatureLimit(NozzleCeilingC, LiveCap{}, nil); !errors.Is(err, ErrLiveCapUnknown) {
		t.Errorf("EffectiveTemperatureLimit() with unknown live cap error = %v, want ErrLiveCapUnknown", err)
	}
	if _, err := EffectiveTemperatureLimit(NozzleCeilingC, LiveCap{}, f64(250)); !errors.Is(err, ErrLiveCapUnknown) {
		t.Errorf("EffectiveTemperatureLimit() with unknown live cap and a soft cap error = %v, want ErrLiveCapUnknown", err)
	}
	if _, err := EffectiveNozzleLimit(LiveCap{}, nil); !errors.Is(err, ErrLiveCapUnknown) {
		t.Errorf("EffectiveNozzleLimit() with unknown live cap error = %v, want ErrLiveCapUnknown", err)
	}
	if _, err := EffectiveBedLimit(LiveCap{}, nil); !errors.Is(err, ErrLiveCapUnknown) {
		t.Errorf("EffectiveBedLimit() with unknown live cap error = %v, want ErrLiveCapUnknown", err)
	}
}

func TestEffectiveNozzleAndBedLimit(t *testing.T) {
	if got, err := EffectiveNozzleLimit(known(NozzleCeilingC), nil); err != nil || got != NozzleCeilingC {
		t.Errorf("EffectiveNozzleLimit(ceiling, nil) = (%v, %v), want (%v, nil)", got, err, NozzleCeilingC)
	}
	if got, err := EffectiveNozzleLimit(known(300), nil); err != nil || got != 300 {
		t.Errorf("EffectiveNozzleLimit(300, nil) = (%v, %v), want (300, nil)", got, err)
	}
	if got, err := EffectiveBedLimit(known(BedCeilingC), nil); err != nil || got != BedCeilingC {
		t.Errorf("EffectiveBedLimit(ceiling, nil) = (%v, %v), want (%v, nil)", got, err, BedCeilingC)
	}
	if got, err := EffectiveBedLimit(known(100), nil); err != nil || got != 100 {
		t.Errorf("EffectiveBedLimit(100, nil) = (%v, %v), want (100, nil)", got, err)
	}
}

func TestWithinBand(t *testing.T) {
	cases := []struct {
		name    string
		current float64
		target  float64
		band    float64
		want    bool
	}{
		{"exact", 210, 210, 10, true},
		{"at edge above", 210, 220, 10, true},
		{"at edge below", 210, 200, 10, true},
		{"just outside above", 210, 220.1, 10, false},
		{"just outside below", 210, 199.9, 10, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WithinBand(c.current, c.target, c.band); got != c.want {
				t.Errorf("WithinBand(%v, %v, %v) = %v, want %v", c.current, c.target, c.band, got, c.want)
			}
		})
	}
}

func TestWithinRange(t *testing.T) {
	cases := []struct {
		name     string
		value    float64
		min, max float64
		want     bool
	}{
		{"within", 100, 50, 150, true},
		{"at min", 50, 50, 150, true},
		{"at max", 150, 50, 150, true},
		{"below min", 49.9, 50, 150, false},
		{"above max", 150.1, 50, 150, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := WithinRange(c.value, c.min, c.max); got != c.want {
				t.Errorf("WithinRange(%v, %v, %v) = %v, want %v", c.value, c.min, c.max, got, c.want)
			}
		})
	}
}

func TestPartFanAboveFloor(t *testing.T) {
	cases := []struct {
		name       string
		current    float64
		requested  float64
		minPercent float64
		want       bool
	}{
		{"at floor", 80, 40, 50, true},
		{"above floor", 80, 60, 50, true},
		{"below floor", 80, 39.9, 50, false},
		{"zero current allows zero", 0, 0, 50, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PartFanAboveFloor(c.current, c.requested, c.minPercent); got != c.want {
				t.Errorf("PartFanAboveFloor(%v, %v, %v) = %v, want %v", c.current, c.requested, c.minPercent, got, c.want)
			}
		})
	}
}
