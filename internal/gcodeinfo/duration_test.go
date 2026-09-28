package gcodeinfo

import "testing"

func TestParseDurationSeconds(t *testing.T) {
	cases := []struct {
		text string
		want float64
		ok   bool
	}{
		{"14m 33s", 14*60 + 33, true},
		{"4h25m", 4*3600 + 25*60, true},
		{"1d 2h 3m 4s", 1*86400 + 2*3600 + 3*60 + 4, true},
		{"33s", 33, true},
		{"", 0, false},
		{"not a duration", 0, false},
	}
	for _, c := range cases {
		got := parseDurationSeconds(c.text)
		if !c.ok {
			if got != nil {
				t.Errorf("parseDurationSeconds(%q) = %v, want nil", c.text, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("parseDurationSeconds(%q) = nil, want %v", c.text, c.want)
			continue
		}
		if *got != c.want {
			t.Errorf("parseDurationSeconds(%q) = %v, want %v", c.text, *got, c.want)
		}
	}
}
