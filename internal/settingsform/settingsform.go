// Package settingsform is the one Settings editor the app's Settings screen
// and the install wizard's Settings step both render: the rows (tool preset,
// idle heat timeout and the mid-print bands), their values, the edit keys, the
// validation (domain.ValidateSettings) and the body. The callers own loading,
// saving and the footer keys around it (dev_docs/tui-design-v0.4.0.md 2.7,
// R2.10). The settings document is the same domain.Settings both surfaces
// persist, so the two can never disagree about what a saved file means.
package settingsform

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// Event is what Update tells the caller happened beyond editing the form.
type Event int

const (
	// EventNone: the key edited the form (or did nothing).
	EventNone Event = iota
	// EventSave: enter was pressed on the list; the caller saves Values().
	EventSave
)

type rowKind int

const (
	rowFloat rowKind = iota
	rowInt
	rowChoice
)

type row struct {
	key   string
	label string
	help  string
	kind  rowKind
}

// rows is the ordered list of D1/D2/D6 settings shown to the user.
var rows = []row{
	{key: "preset", label: "Tool preset", kind: rowChoice,
		help: "Which tools the AI can use: monitor (read only), camera (+ camera), or control (+ printing controls)."},
	{key: "idle_heat_minutes", label: "Idle heat timeout (minutes)", kind: rowInt,
		help: "A heater the AI turns on while the printer is idle turns off automatically after this many minutes."},
	{key: "nozzle_band_c", label: "Nozzle band (C)", kind: rowFloat,
		help: bandHelp("nozzle temperature: each change may move the target at most this many degrees")},
	{key: "bed_band_c", label: "Bed band (C)", kind: rowFloat,
		help: bandHelp("bed temperature: each change may move the target at most this many degrees")},
	{key: "part_fan_min_percent_of_current", label: "Part fan floor (% of current)", kind: rowFloat,
		help: bandHelp("part fan: a change may not drop the fan below this percent of its current speed")},
	{key: "speed_factor_min_percent", label: "Speed factor min (%)", kind: rowFloat,
		help: bandHelp("print speed: lowest speed factor the AI may set")},
	{key: "speed_factor_max_percent", label: "Speed factor max (%)", kind: rowFloat,
		help: bandHelp("print speed: highest speed factor the AI may set")},
	{key: "flow_factor_min_percent", label: "Flow factor min (%)", kind: rowFloat,
		help: bandHelp("flow: lowest flow factor the AI may set")},
	{key: "flow_factor_max_percent", label: "Flow factor max (%)", kind: rowFloat,
		help: bandHelp("flow: highest flow factor the AI may set")},
}

// labelWidth fits the longest label plus a gap.
const labelWidth = 32

// bandHelp prefixes a band's own rule with what every band shares: it only
// limits the AI, only during a print, and never the file's own settings.
func bandHelp(rule string) string {
	return "AI limit during a print only; your gcode is never limited. " +
		strings.ToUpper(rule[:1]) + rule[1:] + "."
}

// presetChoices is the cycle order for the tool preset row.
var presetChoices = []domain.ToolPreset{domain.PresetMonitor, domain.PresetCamera, domain.PresetControl}

func presetIndex(p domain.ToolPreset) int {
	for i, c := range presetChoices {
		if c == p {
			return i
		}
	}
	return 0
}

// cyclePreset moves the tool preset dir steps through presetChoices, wrapping
// at either end.
func cyclePreset(p domain.ToolPreset, dir int) domain.ToolPreset {
	n := len(presetChoices)
	return presetChoices[(presetIndex(p)+dir+n)%n]
}

// isNumberKey reports whether a key press can start a number: typing on a
// numeric row starts editing it with that character.
func isNumberKey(m tea.KeyMsg) bool {
	if m.Type != tea.KeyRunes || len(m.Runes) != 1 {
		return false
	}
	r := m.Runes[0]
	return (r >= '0' && r <= '9') || r == '.'
}

func value(cfg domain.Settings, key string) string {
	switch key {
	case "preset":
		return string(cfg.Tools.Preset)
	case "idle_heat_minutes":
		return strconv.Itoa(cfg.IdleHeatMinutes)
	case "nozzle_band_c":
		return formatFloat(cfg.Bands.NozzleBandC)
	case "bed_band_c":
		return formatFloat(cfg.Bands.BedBandC)
	case "part_fan_min_percent_of_current":
		return formatFloat(cfg.Bands.PartFanMinPercentOfCurrent)
	case "speed_factor_min_percent":
		return formatFloat(cfg.Bands.SpeedFactorMinPercent)
	case "speed_factor_max_percent":
		return formatFloat(cfg.Bands.SpeedFactorMaxPercent)
	case "flow_factor_min_percent":
		return formatFloat(cfg.Bands.FlowFactorMinPercent)
	case "flow_factor_max_percent":
		return formatFloat(cfg.Bands.FlowFactorMaxPercent)
	default:
		return ""
	}
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func parseFloat(raw string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("must be a number")
	}
	return v, nil
}

// setValue applies raw to the field named by key on a copy of cfg and
// validates the result with domain.ValidateSettings, so an edit is either
// fully applied or rejected with the reason shown inline, never half-saved.
func setValue(cfg domain.Settings, key, raw string) (domain.Settings, error) {
	if key == "idle_heat_minutes" {
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("must be a whole number")
		}
		cfg.IdleHeatMinutes = v
	} else {
		var field *float64
		switch key {
		case "nozzle_band_c":
			field = &cfg.Bands.NozzleBandC
		case "bed_band_c":
			field = &cfg.Bands.BedBandC
		case "part_fan_min_percent_of_current":
			field = &cfg.Bands.PartFanMinPercentOfCurrent
		case "speed_factor_min_percent":
			field = &cfg.Bands.SpeedFactorMinPercent
		case "speed_factor_max_percent":
			field = &cfg.Bands.SpeedFactorMaxPercent
		case "flow_factor_min_percent":
			field = &cfg.Bands.FlowFactorMinPercent
		case "flow_factor_max_percent":
			field = &cfg.Bands.FlowFactorMaxPercent
		default:
			return cfg, fmt.Errorf("unknown setting %q", key)
		}
		v, err := parseFloat(raw)
		if err != nil {
			return cfg, err
		}
		*field = v
	}
	if err := domain.ValidateSettings(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

type noticeKind int

const (
	noticeNone noticeKind = iota
	noticeInfo
	noticeOK
	noticeError
)

// Form is the editor state. The zero value is usable after Load.
type Form struct {
	// EnterLabel is what the enter hint says: "save" in the app, "continue" in
	// the wizard (where enter saves and moves on). Empty means "save".
	EnterLabel string

	cfg      domain.Settings
	saved    domain.Settings
	cursor   int
	editing  bool
	input    string
	notice   string
	noticeAs noticeKind
}

// Load replaces the document being edited (and the baseline Dirty compares
// against) and resets the cursor and any edit in progress.
func (f *Form) Load(cfg domain.Settings) {
	f.cfg, f.saved = cfg, cfg
	f.cursor, f.editing, f.input = 0, false, ""
	f.notice, f.noticeAs = "", noticeNone
}

// Values is the document as currently edited.
func (f *Form) Values() domain.Settings { return f.cfg }

// Dirty reports whether the edited document differs from the last loaded or
// saved one.
func (f *Form) Dirty() bool { return !reflect.DeepEqual(f.cfg, f.saved) }

// Typing reports whether a number is being typed: the caller must then pass
// every key (q included) to Update.
func (f *Form) Typing() bool { return f.editing }

// Saved records that Values() reached disk: it is the new baseline and the
// notice reads "Saved.".
func (f *Form) Saved() {
	f.saved = f.cfg
	f.notice, f.noticeAs = "Saved.", noticeOK
}

// SetError shows a failure (a rejected save) under the rows.
func (f *Form) SetError(msg string) { f.notice, f.noticeAs = msg, noticeError }

// CancelEdit closes the number editor, if open, and reports whether it was.
func (f *Form) CancelEdit() bool {
	if !f.editing {
		return false
	}
	f.editing = false
	f.notice, f.noticeAs = "", noticeNone
	return true
}

// Update applies one message. Only key presses matter. In list mode q and esc
// are left to the caller (they mean different things in the app and the
// wizard); every other key either edits the form or, for enter, asks the
// caller to save.
func (f *Form) Update(msg tea.Msg) (tea.Cmd, Event) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil, EventNone
	}
	if f.editing {
		f.updateEditing(key)
		return nil, EventNone
	}
	return nil, f.updateList(key)
}

func (f *Form) updateList(m tea.KeyMsg) Event {
	switch m.String() {
	case "up", "k":
		f.cursor = (f.cursor - 1 + len(rows)) % len(rows)
	case "down", "j":
		f.cursor = (f.cursor + 1) % len(rows)
	case "r":
		f.cfg = domain.DefaultSettings()
		f.notice, f.noticeAs = "Recommended defaults restored; enter saves them.", noticeInfo
	case "enter":
		f.notice, f.noticeAs = "", noticeNone
		return EventSave
	default:
		r := rows[f.cursor]
		if r.kind == rowChoice {
			// The preset is drawn as a value to flip through, so it changes in
			// place with no separate edit mode.
			switch m.String() {
			case "left", "h":
				f.cfg.Tools.Preset = cyclePreset(f.cfg.Tools.Preset, -1)
			case "right", "l", " ":
				f.cfg.Tools.Preset = cyclePreset(f.cfg.Tools.Preset, 1)
			}
			return EventNone
		}
		switch {
		case m.String() == "e":
			f.startEdit(value(f.cfg, r.key))
		case isNumberKey(m):
			f.startEdit(string(m.Runes))
		}
	}
	return EventNone
}

// startEdit opens the numeric editor on the highlighted row with input as its
// starting text: the current value for "e", or the typed character.
func (f *Form) startEdit(input string) {
	f.editing = true
	f.notice, f.noticeAs = "", noticeNone
	f.input = input
}

func (f *Form) updateEditing(m tea.KeyMsg) {
	switch m.String() {
	case "enter":
		cfg, err := setValue(f.cfg, rows[f.cursor].key, f.input)
		if err != nil {
			f.SetError(err.Error())
			return
		}
		f.cfg = cfg
		f.editing = false
		f.notice, f.noticeAs = "", noticeNone
	case "esc":
		f.CancelEdit()
	case "backspace":
		if r := []rune(f.input); len(r) > 0 {
			f.input = string(r[:len(r)-1])
		}
	default:
		if len(m.Runes) > 0 {
			f.input += string(m.Runes)
		}
	}
}

// Hints is the form's own footer keys, the same list Update handles; the
// caller appends its back and quit hints (and nothing at all while editing,
// where q is a character).
func (f *Form) Hints() []frame.Hint {
	if f.editing {
		return []frame.Hint{
			{Keys: "enter", Label: "confirm", Priority: 90},
			{Keys: "esc", Label: "cancel", Priority: frame.PriorityBack},
		}
	}
	change := frame.Hint{Keys: "e", Label: "edit", Priority: 60}
	if rows[f.cursor].kind == rowChoice {
		change = frame.Hint{Keys: "←→", Label: "change", Priority: 60}
	}
	enter := f.EnterLabel
	if enter == "" {
		enter = "save"
	}
	return []frame.Hint{
		{Keys: "↑↓", Label: "move", Priority: 70},
		change,
		{Keys: "r", Label: "restore defaults", Priority: 20},
		{Keys: "enter", Label: enter, Priority: 80},
	}
}

// Body renders the rows and, under them, either the status notice (an error,
// a restore, "Saved.") or the help for the highlighted row, wrapped to a
// terminal w columns wide. Lines carry the frame gutter already.
func (f *Form) Body(w int) []string {
	out := make([]string, 0, len(rows)+4)
	for i, r := range rows {
		val := value(f.cfg, r.key)
		switch {
		case f.editing && i == f.cursor:
			val = f.input + "_"
		case r.kind == rowChoice:
			val = "< " + val + " >"
		}
		out = append(out, frame.Marker(i == f.cursor)+frame.Pad(r.label, labelWidth-2)+val)
	}
	out = append(out, "")
	text, style := rows[f.cursor].help, frame.StyleDim
	switch f.noticeAs {
	case noticeInfo:
		text, style = f.notice, frame.StyleWarn
	case noticeOK:
		text, style = f.notice, frame.StyleOK
	case noticeError:
		text, style = f.notice, frame.StyleError
	}
	for _, line := range frame.Wrap(w, frame.Gutter, text) {
		out = append(out, style.Render(line))
	}
	return out
}
