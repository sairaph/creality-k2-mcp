package gcodeinfo

import (
	"regexp"
	"strconv"
)

// durationPattern matches a slicer-style duration made of optional days,
// hours, minutes and seconds components, in any combination Creality Print
// / OrcaSlicer is known to emit: "14m 33s", "4h25m", "1d 2h 3m 4s". Every
// component is individually optional, so the pattern always finds a match
// somewhere in text, even a zero-length one with all four groups empty (for
// example against "" or "not a duration"); parseDurationSeconds relies on
// its own found flag, not on FindStringSubmatch returning nil, to tell a
// real duration apart from that empty match.
var durationPattern = regexp.MustCompile(`(?:\s*(\d+)\s*d)?(?:\s*(\d+)\s*h)?(?:\s*(\d+)\s*m)?(?:\s*(\d+)\s*s)?`)

// parseDurationSeconds converts a slicer duration string to seconds. Because
// durationPattern always matches, FindStringSubmatch here never returns nil;
// parseDurationSeconds instead returns nil when none of the four captured
// groups held a number (found stays false), never a fabricated zero.
func parseDurationSeconds(text string) *float64 {
	m := durationPattern.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	var total float64
	found := false
	units := [4]float64{86400, 3600, 60, 1}
	for i, group := range m[1:] {
		if group == "" {
			continue
		}
		n, err := strconv.ParseFloat(group, 64)
		if err != nil {
			continue
		}
		total += n * units[i]
		found = true
	}
	if !found {
		return nil
	}
	return &total
}
