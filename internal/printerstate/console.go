package printerstate

import (
	"fmt"
	"regexp"

	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

// temperatureReport matches the periodic temperature lines Klipper prints while
// waiting on a heater, such as "B:71.0 /70.0 T0:208.3 /250.0" and
// "// cur_temp = 41.2". They arrive every second or so during a heat-up and
// drown every other console line (supervised session 2026-09-29).
var temperatureReport = regexp.MustCompile(`^(?:B:-?\d+(?:\.\d+)? /-?\d+(?:\.\d+)?(?: T\d*:-?\d+(?:\.\d+)? /-?\d+(?:\.\d+)?)*|// cur_temp = -?\d+(?:\.\d+)?)\s*$`)

// CollapseTemperatureReports replaces each run of two or more consecutive
// temperature reports with one entry, "(N temperature reports, latest: <line>)",
// keeping every other entry and the order. The latest report of a run is its
// last entry in the given order, or its first when newestFirst is true (the
// console tool lists newest first). The collapsed entry keeps the latest
// report's time and type.
func CollapseTemperatureReports(entries []moonraker.GCodeStoreEntry, newestFirst bool) []moonraker.GCodeStoreEntry {
	out := make([]moonraker.GCodeStoreEntry, 0, len(entries))
	for i := 0; i < len(entries); {
		if !temperatureReport.MatchString(entries[i].Message) {
			out = append(out, entries[i])
			i++
			continue
		}
		j := i
		for j < len(entries) && temperatureReport.MatchString(entries[j].Message) {
			j++
		}
		run := entries[i:j]
		if len(run) == 1 {
			out = append(out, run[0])
		} else {
			latest := run[len(run)-1]
			if newestFirst {
				latest = run[0]
			}
			latest.Message = fmt.Sprintf("(%d temperature reports, latest: %s)", len(run), latest.Message)
			out = append(out, latest)
		}
		i = j
	}
	return out
}
