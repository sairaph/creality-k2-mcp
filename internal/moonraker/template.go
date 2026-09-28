package moonraker

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// Template identifies one of the fixed, named gcode commands this package
// is allowed to emit (safety-architecture.md P4, "closed command
// vocabulary: the server can only emit a fixed, enumerated set of gcode
// templates and Moonraker endpoints, each with validated parameters").
//
// The five constants below are the entire vocabulary. There is no exported
// way to construct a Template outside this fixed set or to render one into
// a gcode string without going through RunTemplate: render, below, is
// unexported, so a caller in another package can never turn a Template plus
// some args into a command string except by asking RunTemplate to send it.
type Template int

const (
	// TemplateSetHeaterTemperature renders
	// "SET_HEATER_TEMPERATURE HEATER=<heater> TARGET=<target>".
	// args: "heater" ("extruder" or "heater_bed"), "target" (a finite
	// decimal number, as a string), bounded by this package's own absolute
	// limits regardless of what the policy layer allows: extruder
	// [0, domain.NozzleCeilingC] (0-320 C), heater_bed
	// [0, domain.BedCeilingC] (0-110 C). These are the hard machine-level
	// bounds render enforces on its own (safety-architecture.md P4); the
	// policy layer's live product_param cap and mid-print bands (D1) are
	// always at or inside these, never outside them.
	TemplateSetHeaterTemperature Template = iota
	// TemplateM106 renders "M106 P<fan> S<speed>".
	// args: "fan" ("0", "1" or "2"), "speed" (an integer 0-255, as a
	// string).
	TemplateM106
	// TemplateM220 renders "M220 S<percent>".
	// args: "percent" (an integer, as a string), bounded to this package's
	// own absolute limit of 10-200 regardless of the policy layer.
	TemplateM220
	// TemplateM221 renders "M221 S<percent>".
	// args: "percent" (an integer, as a string), bounded to this package's
	// own absolute limit of 80-120 regardless of the policy layer.
	TemplateM221
	// TemplateExcludeObject renders "EXCLUDE_OBJECT NAME=<name>".
	// args: "name" (must match objectNamePattern).
	TemplateExcludeObject
)

// objectNamePattern is the only shape of object name EXCLUDE_OBJECT may be
// sent, matching the names EXCLUDE_OBJECT_DEFINE produces
// (control_test_20260928.md: names are stored upper case, but matching is
// case-insensitive at the tool layer above this one; this package only
// checks the character set).
var objectNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// render validates args for t and returns the literal gcode line to send,
// or an error describing exactly what was wrong with args. It is
// unexported: RunTemplate is the only caller, and only for one of the five
// Template constants above.
func (t Template) render(args map[string]string) (string, error) {
	switch t {
	case TemplateSetHeaterTemperature:
		heater := args["heater"]
		var max float64
		switch heater {
		case "extruder":
			max = domain.NozzleCeilingC
		case "heater_bed":
			max = domain.BedCeilingC
		default:
			return "", fmt.Errorf("heater must be \"extruder\" or \"heater_bed\", got %q", heater)
		}
		target, err := strconv.ParseFloat(args["target"], 64)
		if err != nil {
			return "", fmt.Errorf("target must be a number: %w", err)
		}
		if math.IsNaN(target) || math.IsInf(target, 0) {
			return "", fmt.Errorf("target must be a finite number, got %v", target)
		}
		if target < 0 || target > max {
			return "", fmt.Errorf("target for %s must be 0-%v, got %v", heater, max, target)
		}
		return fmt.Sprintf("SET_HEATER_TEMPERATURE HEATER=%s TARGET=%s", heater, strconv.FormatFloat(target, 'f', -1, 64)), nil

	case TemplateM106:
		fan := args["fan"]
		if fan != "0" && fan != "1" && fan != "2" {
			return "", fmt.Errorf("fan must be \"0\", \"1\" or \"2\", got %q", fan)
		}
		speed, err := strconv.Atoi(args["speed"])
		if err != nil {
			return "", fmt.Errorf("speed must be an integer: %w", err)
		}
		if speed < 0 || speed > 255 {
			return "", fmt.Errorf("speed must be 0-255, got %d", speed)
		}
		return fmt.Sprintf("M106 P%s S%d", fan, speed), nil

	case TemplateM220:
		percent, err := strconv.Atoi(args["percent"])
		if err != nil {
			return "", fmt.Errorf("percent must be an integer: %w", err)
		}
		if percent < 10 || percent > 200 {
			return "", fmt.Errorf("percent must be 10-200, got %d", percent)
		}
		return fmt.Sprintf("M220 S%d", percent), nil

	case TemplateM221:
		percent, err := strconv.Atoi(args["percent"])
		if err != nil {
			return "", fmt.Errorf("percent must be an integer: %w", err)
		}
		if percent < 80 || percent > 120 {
			return "", fmt.Errorf("percent must be 80-120, got %d", percent)
		}
		return fmt.Sprintf("M221 S%d", percent), nil

	case TemplateExcludeObject:
		name := args["name"]
		if !objectNamePattern.MatchString(name) {
			return "", fmt.Errorf("name must match %s, got %q", objectNamePattern.String(), name)
		}
		return fmt.Sprintf("EXCLUDE_OBJECT NAME=%s", name), nil

	default:
		return "", fmt.Errorf("unknown template %d", int(t))
	}
}

// RunTemplate validates args for t and, if they are valid, sends the
// resulting gcode line via /printer/gcode/script. This is the only path by
// which this package ever emits a gcode command; there is no method that
// sends arbitrary gcode text.
func (c *Client) RunTemplate(ctx context.Context, t Template, args map[string]string) error {
	command, err := t.render(args)
	if err != nil {
		return &Error{Op: "RunTemplate", Code: CodeInvalidInput, Body: err.Error()}
	}
	payload := struct {
		Script string `json:"script"`
	}{Script: command}
	return c.postJSON(ctx, "RunTemplate", "/printer/gcode/script", payload, timeoutStatus, nil)
}
