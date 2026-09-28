package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
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

	cursor       int
	editing      bool
	input        string
	choiceCursor int

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
		help: "A heater set while idle turns off automatically after this many minutes."},
	{key: "nozzle_band_c", label: "Nozzle band (C)", kind: settingsRowFloat,
		help: "A mid-print nozzle temperature change may move at most this many degrees from the current target."},
	{key: "bed_band_c", label: "Bed band (C)", kind: settingsRowFloat,
		help: "A mid-print bed temperature change may move at most this many degrees from the current target."},
	{key: "part_fan_min_percent_of_current", label: "Part fan floor (% of current)", kind: settingsRowFloat,
		help: "A mid-print part fan change may not drop the fan below this percent of its current value."},
	{key: "speed_factor_min_percent", label: "Speed factor min (%)", kind: settingsRowFloat,
		help: "Lowest print speed factor a mid-print change may set."},
	{key: "speed_factor_max_percent", label: "Speed factor max (%)", kind: settingsRowFloat,
		help: "Highest print speed factor a mid-print change may set."},
	{key: "flow_factor_min_percent", label: "Flow factor min (%)", kind: settingsRowFloat,
		help: "Lowest flow factor a mid-print change may set."},
	{key: "flow_factor_max_percent", label: "Flow factor max (%)", kind: settingsRowFloat,
		help: "Highest flow factor a mid-print change may set."},
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
		s.message = "Recommended defaults restored; press s to save."
	case "enter":
		s.startEdit()
	case "s":
		s.saving = true
		s.message = ""
		return s.saveCmd()
	}
	return nil
}

func (s *settingsScreen) startEdit() {
	row := settingsScreenRows[s.cursor]
	s.editing = true
	s.message = row.help
	if row.kind == settingsRowChoice {
		s.choiceCursor = settingsPresetIndex(s.settings.Tools.Preset)
		return
	}
	s.input = settingsRowValue(s.settings, row.key)
}

func (s *settingsScreen) updateEditing(m tea.KeyMsg) tea.Cmd {
	row := settingsScreenRows[s.cursor]

	if row.kind == settingsRowChoice {
		switch m.String() {
		case "up", "k":
			s.choiceCursor = (s.choiceCursor - 1 + len(settingsPresetChoices)) % len(settingsPresetChoices)
		case "down", "j":
			s.choiceCursor = (s.choiceCursor + 1) % len(settingsPresetChoices)
		case "enter":
			cfg := s.settings
			cfg.Tools.Preset = settingsPresetChoices[s.choiceCursor]
			if err := domain.ValidateSettings(cfg); err != nil {
				s.message = err.Error()
				return nil
			}
			s.settings = cfg
			s.editing = false
			s.message = ""
		case "esc":
			s.editing = false
			s.message = ""
		}
		return nil
	}

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
		if s.editing && i == s.cursor {
			if row.kind == settingsRowChoice {
				value = "< " + string(settingsPresetChoices[s.choiceCursor]) + " >"
			} else {
				value = s.input + "_"
			}
		}
		fmt.Fprintf(&b, " %s %-34s %s\n", cursor, row.label, value)
	}

	if s.message != "" {
		b.WriteString("\n  " + s.message)
	}

	footer := "  ↑↓ move · enter edit · r restore defaults · s save · esc back"
	if s.editing {
		footer = "  enter confirm · esc cancel"
	}
	b.WriteString("\n\n" + tuiStyleDim.Render(footer))
	return b.String()
}
