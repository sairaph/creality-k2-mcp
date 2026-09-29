package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// settingsScreen edits the persisted settings document (preset, mid-print
// bands, idle heat timeout; dev_docs/safety-architecture.md D1/D2/D6),
// reusing domain.Settings/ReadSettings/SaveSettings/ValidateSettings/
// DefaultSettings directly, exactly as internal/wizard's own Settings step
// does. It does not reuse that step's bubbletea plumbing (a flow.Step is
// built for the linear install wizard, not a standing screen the user
// revisits from the main menu), but it edits and validates the same
// document the same way, so the two surfaces can never disagree about what
// a saved settings file means.
type settingsScreen struct {
	path     string
	settings domain.Settings
	ready    bool
	loadErr  string

	cursor  int
	editing bool
	input   string

	saving  bool
	message string
}

func newSettingsScreen() *settingsScreen { return &settingsScreen{} }

type settingsRowKind int

const (
	settingsRowFloat settingsRowKind = iota
	settingsRowInt
	settingsRowChoice
)

type settingsRow struct {
	key   string
	label string
	help  string
	kind  settingsRowKind
}

var settingsScreenRows = []settingsRow{
	{key: "preset", label: "Tool preset", kind: settingsRowChoice,
		help: "Which tools the AI can use: monitor (read only), camera (+ camera), or control (+ printing controls)."},
	{key: "idle_heat_minutes", label: "Idle heat timeout (minutes)", kind: settingsRowInt,
		help: "A heater the AI turns on while the printer is idle turns off automatically after this many minutes."},
	{key: "nozzle_band_c", label: "Nozzle band (C)", kind: settingsRowFloat,
		help: settingsBandHelp("nozzle temperature: each change may move the target at most this many degrees")},
	{key: "bed_band_c", label: "Bed band (C)", kind: settingsRowFloat,
		help: settingsBandHelp("bed temperature: each change may move the target at most this many degrees")},
	{key: "part_fan_min_percent_of_current", label: "Part fan floor (% of current)", kind: settingsRowFloat,
		help: settingsBandHelp("part fan: a change may not drop the fan below this percent of its current speed")},
	{key: "speed_factor_min_percent", label: "Speed factor min (%)", kind: settingsRowFloat,
		help: settingsBandHelp("print speed: lowest speed factor the AI may set")},
	{key: "speed_factor_max_percent", label: "Speed factor max (%)", kind: settingsRowFloat,
		help: settingsBandHelp("print speed: highest speed factor the AI may set")},
	{key: "flow_factor_min_percent", label: "Flow factor min (%)", kind: settingsRowFloat,
		help: settingsBandHelp("flow: lowest flow factor the AI may set")},
	{key: "flow_factor_max_percent", label: "Flow factor max (%)", kind: settingsRowFloat,
		help: settingsBandHelp("flow: highest flow factor the AI may set")},
}

// settingsBandHelp prefixes a band's own rule with what every band shares:
// it only limits the AI, only during a print, and never the file's own
// settings.
func settingsBandHelp(rule string) string {
	return "AI limit during a print only; your gcode is never limited. " +
		strings.ToUpper(rule[:1]) + rule[1:] + "."
}

var settingsPresetChoices = []domain.ToolPreset{domain.PresetMonitor, domain.PresetCamera, domain.PresetControl}

func settingsPresetIndex(p domain.ToolPreset) int {
	for i, c := range settingsPresetChoices {
		if c == p {
			return i
		}
	}
	return 0
}

// cycleSettingsPreset moves the tool preset dir steps through
// settingsPresetChoices, wrapping at either end.
func cycleSettingsPreset(p domain.ToolPreset, dir int) domain.ToolPreset {
	n := len(settingsPresetChoices)
	return settingsPresetChoices[(settingsPresetIndex(p)+dir+n)%n]
}

// isSettingsNumberKey reports whether a key press can start a number:
// typing on a numeric row starts editing it with that character.
func isSettingsNumberKey(m tea.KeyMsg) bool {
	if m.Type != tea.KeyRunes || len(m.Runes) != 1 {
		return false
	}
	r := m.Runes[0]
	return (r >= '0' && r <= '9') || r == '.'
}

func settingsRowValue(cfg domain.Settings, key string) string {
	switch key {
	case "preset":
		return string(cfg.Tools.Preset)
	case "idle_heat_minutes":
		return strconv.Itoa(cfg.IdleHeatMinutes)
	case "nozzle_band_c":
		return formatSettingsFloat(cfg.Bands.NozzleBandC)
	case "bed_band_c":
		return formatSettingsFloat(cfg.Bands.BedBandC)
	case "part_fan_min_percent_of_current":
		return formatSettingsFloat(cfg.Bands.PartFanMinPercentOfCurrent)
	case "speed_factor_min_percent":
		return formatSettingsFloat(cfg.Bands.SpeedFactorMinPercent)
	case "speed_factor_max_percent":
		return formatSettingsFloat(cfg.Bands.SpeedFactorMaxPercent)
	case "flow_factor_min_percent":
		return formatSettingsFloat(cfg.Bands.FlowFactorMinPercent)
	case "flow_factor_max_percent":
		return formatSettingsFloat(cfg.Bands.FlowFactorMaxPercent)
	default:
		return ""
	}
}

func formatSettingsFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func parseSettingsFloat(raw string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("must be a number")
	}
	return v, nil
}

// setSettingsRowValue applies raw to the field named by key on a copy of
// cfg and validates the result with domain.ValidateSettings, so an edit is
// either fully applied or rejected with the reason shown inline, never
// half-saved.
func setSettingsRowValue(cfg domain.Settings, key, raw string) (domain.Settings, error) {
	switch key {
	case "idle_heat_minutes":
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("must be a whole number")
		}
		cfg.IdleHeatMinutes = v
	case "nozzle_band_c":
		v, err := parseSettingsFloat(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.NozzleBandC = v
	case "bed_band_c":
		v, err := parseSettingsFloat(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.BedBandC = v
	case "part_fan_min_percent_of_current":
		v, err := parseSettingsFloat(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.PartFanMinPercentOfCurrent = v
	case "speed_factor_min_percent":
		v, err := parseSettingsFloat(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.SpeedFactorMinPercent = v
	case "speed_factor_max_percent":
		v, err := parseSettingsFloat(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.SpeedFactorMaxPercent = v
	case "flow_factor_min_percent":
		v, err := parseSettingsFloat(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.FlowFactorMinPercent = v
	case "flow_factor_max_percent":
		v, err := parseSettingsFloat(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.FlowFactorMaxPercent = v
	default:
		return cfg, fmt.Errorf("unknown setting %q", key)
	}
	if err := domain.ValidateSettings(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

type settingsLoadedMsg struct {
	settings domain.Settings
	path     string
	err      error
}

type settingsSavedMsg struct {
	err error
}

func (s *settingsScreen) Init() tea.Cmd {
	return func() tea.Msg {
		path, err := domain.SettingsPath()
		if err != nil {
			return settingsLoadedMsg{err: err}
		}
		cfg, err := domain.ReadSettings(path)
		if err != nil {
			return settingsLoadedMsg{err: err}
		}
		return settingsLoadedMsg{settings: cfg, path: path}
	}
}

func (s *settingsScreen) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case settingsLoadedMsg:
		if m.err != nil {
			s.loadErr = m.err.Error()
			return nil
		}
		s.settings, s.path, s.ready = m.settings, m.path, true
		return nil

	case settingsSavedMsg:
		s.saving = false
		if m.err != nil {
			s.message = m.err.Error()
			return nil
		}
		s.message = "Saved."
		return nil

	case tea.KeyMsg:
		if !s.ready {
			return nil
		}
		if s.saving {
			return nil
		}
		if s.editing {
			return s.updateEditing(m)
		}
		return s.updateList(m)
	}
	return nil
}

func (s *settingsScreen) updateList(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "q", "esc":
		return app.Action("settings", "back")
	case "up", "k":
		s.cursor = (s.cursor - 1 + len(settingsScreenRows)) % len(settingsScreenRows)
	case "down", "j":
		s.cursor = (s.cursor + 1) % len(settingsScreenRows)
	case "r":
		s.settings = domain.DefaultSettings()
		s.message = "Recommended defaults restored; press enter to save."
	case "enter":
		s.saving = true
		s.message = ""
		return s.saveCmd()
	default:
		row := settingsScreenRows[s.cursor]
		if row.kind == settingsRowChoice {
			// The preset is drawn as a value to flip through, so it changes
			// in place with no separate edit mode.
			switch m.String() {
			case "left", "h":
				s.settings.Tools.Preset = cycleSettingsPreset(s.settings.Tools.Preset, -1)
			case "right", "l", " ":
				s.settings.Tools.Preset = cycleSettingsPreset(s.settings.Tools.Preset, 1)
			}
			return nil
		}
		switch {
		case m.String() == "e":
			s.startEdit(settingsRowValue(s.settings, row.key))
		case isSettingsNumberKey(m):
			s.startEdit(string(m.Runes))
		}
	}
	return nil
}

// startEdit opens the numeric editor on the highlighted row with input as
// its starting text: the current value for "e", or the typed character.
func (s *settingsScreen) startEdit(input string) {
	s.editing = true
	s.message = ""
	s.input = input
}

func (s *settingsScreen) updateEditing(m tea.KeyMsg) tea.Cmd {
	row := settingsScreenRows[s.cursor]

	switch m.String() {
	case "enter":
		cfg, err := setSettingsRowValue(s.settings, row.key, s.input)
		if err != nil {
			s.message = err.Error()
			return nil
		}
		s.settings = cfg
		s.editing = false
		s.message = ""
	case "esc":
		s.editing = false
		s.message = ""
	case "backspace":
		if len(s.input) > 0 {
			r := []rune(s.input)
			s.input = string(r[:len(r)-1])
		}
	default:
		if len(m.Runes) > 0 {
			s.input += string(m.Runes)
		}
	}
	return nil
}

func (s *settingsScreen) saveCmd() tea.Cmd {
	path, cfg := s.path, s.settings
	return func() tea.Msg {
		if err := domain.SaveSettings(path, cfg); err != nil {
			return settingsSavedMsg{err: err}
		}
		return settingsSavedMsg{}
	}
}

func (s *settingsScreen) View() string {
	if s.loadErr != "" {
		return tuiStyleTitle.Render("  Settings") + "\n\n  " + tuiStyleError.Render(s.loadErr)
	}
	if !s.ready {
		return tuiStyleTitle.Render("  Settings") + "\n\n  " + tuiStyleDim.Render("Loading settings...")
	}

	var b strings.Builder
	b.WriteString(tuiStyleTitle.Render("  Settings") + "\n\n")

	if s.saving {
		b.WriteString("  " + tuiStyleDim.Render("Saving...") + "\n\n")
	}

	for i, row := range settingsScreenRows {
		cursor := " "
		if i == s.cursor {
			cursor = tuiStyleCursor.Render(">")
		}
		value := settingsRowValue(s.settings, row.key)
		switch {
		case s.editing && i == s.cursor:
			value = s.input + "_"
		case row.kind == settingsRowChoice:
			value = "< " + value + " >"
		}
		fmt.Fprintf(&b, " %s %-34s %s\n", cursor, row.label, value)
	}

	// A status message (an error, a save) wins; otherwise explain the
	// highlighted row, so what each band means is always on screen.
	message := s.message
	if message == "" {
		message = settingsScreenRows[s.cursor].help
	}
	b.WriteString("\n  " + message)

	change := "e edit"
	if settingsScreenRows[s.cursor].kind == settingsRowChoice {
		change = "←→ change"
	}
	footer := "  ↑↓ move · " + change + " · r restore defaults · enter save · esc back"
	if s.editing {
		footer = "  enter confirm · esc cancel"
	}
	b.WriteString("\n\n" + tuiStyleDim.Render(footer))
	return b.String()
}
