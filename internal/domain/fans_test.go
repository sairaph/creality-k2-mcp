package domain

import (
	"math"
	"testing"
)

func TestPercentToS(t *testing.T) {
	cases := []struct {
		name    string
		percent float64
		want    int
	}{
		{"zero", 0, 0},
		{"fifty", 50, 128}, // round(50*255/100) = round(127.5) = 128, matching the live S128 test
		{"hundred", 100, 255},
		{"twentyfive", 25, 64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PercentToS(c.percent); got != c.want {
				t.Errorf("PercentToS(%v) = %d, want %d", c.percent, got, c.want)
			}
		})
	}
}

// TestReportedFanPercent uses the live-measured pin values from
// references/printer-snapshot/extra/fan_test_20260928.md: each channel was
// commanded S128 (50%), and the resulting output_pin value is reported here.
// The measured values are rounded to 4 decimals in the source document, so
// the recovered percent is checked within a small tolerance of 50, not
// exactly.
func TestReportedFanPercent(t *testing.T) {
	cases := []struct {
		name     string
		pinValue float64
		min      int
	}{
		{"part_fan0", 0.5508, 25},
		{"case_fan1", 0.5996, 50},
		{"auxiliary_fan2", 0.6973, 100},
	}
	const wantPercent = 50.0
	const tolerance = 0.5
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReportedFanPercent(c.pinValue, c.min)
			if math.Abs(got-wantPercent) > tolerance {
				t.Errorf("ReportedFanPercent(%v, %d) = %v, want within %v of %v", c.pinValue, c.min, got, tolerance, wantPercent)
			}
		})
	}
}

func TestReportedFanPercentZero(t *testing.T) {
	if got := ReportedFanPercent(0, 25); got != 0 {
		t.Errorf("ReportedFanPercent(0, 25) = %v, want 0", got)
	}
	if got := ReportedFanPercent(-0.01, 25); got != 0 {
		t.Errorf("ReportedFanPercent(-0.01, 25) = %v, want 0", got)
	}
}

func TestFanSpecMapping(t *testing.T) {
	cases := []struct {
		channel   FanChannel
		wantP     int
		wantPin   string
		wantMin   int
		wantGCode string
	}{
		{FanPart, 0, "fan0", 25, "M106 P0 S128"},
		{FanCase, 1, "fan1", 50, "M106 P1 S128"},
		{FanAuxiliary, 2, "fan2", 100, "M106 P2 S128"},
	}
	for _, c := range cases {
		t.Run(string(c.channel), func(t *testing.T) {
			spec, ok := FanSpecFor(c.channel)
			if !ok {
				t.Fatalf("FanSpecFor(%q) not found", c.channel)
			}
			if spec.PParameter != c.wantP {
				t.Errorf("PParameter = %d, want %d", spec.PParameter, c.wantP)
			}
			if spec.OutputPin != c.wantPin {
				t.Errorf("OutputPin = %q, want %q", spec.OutputPin, c.wantPin)
			}
			if spec.MinValue != c.wantMin {
				t.Errorf("MinValue = %d, want %d", spec.MinValue, c.wantMin)
			}
			if got := spec.GCode(50); got != c.wantGCode {
				t.Errorf("GCode(50) = %q, want %q", got, c.wantGCode)
			}
		})
	}
}

func TestFanSpecForUnknown(t *testing.T) {
	if _, ok := FanSpecFor("bogus"); ok {
		t.Error("FanSpecFor(\"bogus\") reported ok, want not found")
	}
}
