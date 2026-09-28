package wizard

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// SettingsState is embedded in consumer state for the Settings step.
type SettingsState struct {
	Settings domain.Settings
	Path     string
	Ready    bool

	Cursor  int
	Editing bool
	Input   string

	// Saving is true while the save enter started is in flight, mirroring
	// installer.ApplyStep's Done gating so a save runs as a tea.Cmd instead
	// of blocking Update.
	Saving bool

	Message string
}

// settingsRowKind selects the editor a settingsRow uses.
type settingsRowKind int

const (
	rowFloat settingsRowKind = iota
	rowInt
	rowChoice
)

// settingsRow describes one editable row, in the style of
// interactive-terminal-mcp's SettingsRows and sana-mcp's configure screen.
type settingsRow struct {
	Key   string
	Label string
	Help  string
	Kind  settingsRowKind
}

// settingsRows is the ordered, concise list of D1/D2/D6 settings shown to
// the user, with sensible defaults preselected from domain.DefaultSettings.
var settingsRows = []settingsRow{
	{Key: "preset", Label: "Tool preset", Kind: rowChoice,
		Help: "Which tools the AI can use: monitor (read only), camera (+ camera), or control (+ printing controls)."},
	{Key: "idle_heat_minutes", Label: "Idle heat timeout (minutes)", Kind: rowInt,
		Help: "A heater the AI turns on while the printer is idle turns off automatically after this many minutes."},
	{Key: "nozzle_band_c", Label: "Nozzle band (C)", Kind: rowFloat,
		Help: bandHelp("nozzle temperature: each change may move the target at most this many degrees")},
	{Key: "bed_band_c", Label: "Bed band (C)", Kind: rowFloat,
		Help: bandHelp("bed temperature: each change may move the target at most this many degrees")},
	{Key: "part_fan_min_percent_of_current", Label: "Part fan floor (% of current)", Kind: rowFloat,
		Help: bandHelp("part fan: a change may not drop the fan below this percent of its current speed")},
	{Key: "speed_factor_min_percent", Label: "Speed factor min (%)", Kind: rowFloat,
		Help: bandHelp("print speed: lowest speed factor the AI may set")},
	{Key: "speed_factor_max_percent", Label: "Speed factor max (%)", Kind: rowFloat,
		Help: bandHelp("print speed: highest speed factor the AI may set")},
	{Key: "flow_factor_min_percent", Label: "Flow factor min (%)", Kind: rowFloat,
		Help: bandHelp("flow: lowest flow factor the AI may set")},
	{Key: "flow_factor_max_percent", Label: "Flow factor max (%)", Kind: rowFloat,
		Help: bandHelp("flow: highest flow factor the AI may set")},
}

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

// settingsValue renders one row's current value as text.
func settingsValue(cfg domain.Settings, key string) string {
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

func parseFloatField(raw string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("must be a number")
	}
	return v, nil
}

// setSettingsValue applies raw to the field named by key on a copy of cfg
// and validates the result with domain.ValidateSettings, so an edit is
// either fully applied or rejected with the reason shown inline, never
// half-saved.
func setSettingsValue(cfg domain.Settings, key, raw string) (domain.Settings, error) {
	switch key {
	case "idle_heat_minutes":
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return cfg, fmt.Errorf("must be a whole number")
		}
		cfg.IdleHeatMinutes = v
	case "nozzle_band_c":
		v, err := parseFloatField(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.NozzleBandC = v
	case "bed_band_c":
		v, err := parseFloatField(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.BedBandC = v
	case "part_fan_min_percent_of_current":
		v, err := parseFloatField(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.PartFanMinPercentOfCurrent = v
	case "speed_factor_min_percent":
		v, err := parseFloatField(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.SpeedFactorMinPercent = v
	case "speed_factor_max_percent":
		v, err := parseFloatField(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.SpeedFactorMaxPercent = v
	case "flow_factor_min_percent":
		v, err := parseFloatField(raw)
		if err != nil {
			return cfg, err
		}
		cfg.Bands.FlowFactorMinPercent = v
	case "flow_factor_max_percent":
		v, err := parseFloatField(raw)
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

// SettingsStepOptions controls the Settings step.
type SettingsStepOptions struct {
	// DryRun computes what enter would save and shows it instead of writing
	// config.toml, matching installer.ApplyStepOptions.DryRun.
	DryRun bool
}

// SettingsStep returns a flow.Step that lets the user choose the tool
// preset (dev_docs/safety-architecture.md D6), the mid-print setpoint bands
// (D1) and the idle heat timeout (D2), and saves them to the settings file
// on enter. Defaults come from domain.DefaultSettings (through
// domain.ReadSettings, which never writes; the file is created only by an
// explicit enter, or left untouched entirely in dry-run).
func SettingsStep[T any](stateFn func(*T) *SettingsState, opts SettingsStepOptions) flow.Step[T] {
	return &settingsStep[T]{stateFn: stateFn, opts: opts}
}

type settingsStep[T any] struct {
	stateFn func(*T) *SettingsState
	opts    SettingsStepOptions
}

func (s *settingsStep[T]) ID() string { return "settings" }

func (s *settingsStep[T]) Title(state *T) string {
	return "Settings - safety limits and which tools the AI can use"
}

func (s *settingsStep[T]) Hints(state *T) []struct{ Key, Label string } {
	st := s.get(state)
	if st == nil {
		return nil
	}
	if st.Saving {
		return nil // the save is in flight and cannot be cancelled
	}
	hints := settingsHints(st)
	out := make([]struct{ Key, Label string }, len(hints))
	for i, h := range hints {
		out[i] = struct{ Key, Label string }(h)
	}
	return out
}

// settingsHints is the one key list both Hints and the View footer show, so
// they cannot drift apart. Enter saves and continues, like every other
// wizard step; the highlighted row is changed in place.
func settingsHints(st *SettingsState) []tui.Hint {
	if st.Editing {
		return []tui.Hint{
			{Key: "enter", Label: "confirm"},
			{Key: "esc", Label: "cancel"},
		}
	}
	change := tui.Hint{Key: "e", Label: "edit"}
	if settingsRows[st.Cursor].Kind == rowChoice {
		change = tui.Hint{Key: "←→", Label: "change"}
	}
	return []tui.Hint{
		{Key: "↑↓", Label: "move"},
		change,
		{Key: "r", Label: "restore defaults"},
		{Key: "enter", Label: "save & continue"},
		{Key: "esc", Label: "back"},
		{Key: "q", Label: "cancel"},
	}
}

// cyclePreset moves the tool preset dir steps through presetChoices,
// wrapping at either end.
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

func (s *settingsStep[T]) get(state *T) *SettingsState {
	if s.stateFn == nil {
		return nil
	}
	return s.stateFn(state)
}

type settingsLoadedMsg struct {
	settings domain.Settings
	path     string
	err      error
}

// settingsSavedMsg is what saveCmd produces: a real write's outcome, or a
// dry run's preview, following installer.ApplyStep's appliedMsg pattern.
type settingsSavedMsg struct {
	dryRun bool
	err    error
}

// Init loads the settings the step edits with domain.ReadSettings, which
// never writes, in every mode: opening the Settings step, dry-run or not,
// must never create config.toml on its own. The file is created only by an
// explicit enter (and never in dry-run; see saveCmd).
func (s *settingsStep[T]) Init(state *T) tea.Cmd {
	st := s.get(state)
	if st == nil {
		return nil
	}
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

func (s *settingsStep[T]) Update(msg tea.Msg, state *T) (flow.Directive, tea.Cmd) {
	st := s.get(state)
	if st == nil {
		return flow.Fail, nil
	}

	switch m := msg.(type) {
	case settingsLoadedMsg:
		if m.err != nil {
			if base := baseStateOf(state); base != nil {
				base.Failure = m.err
			}
			return flow.Fail, nil
		}
		st.Settings = m.settings
		st.Path = m.path
		st.Ready = true
		return flow.Continue, nil

	case settingsSavedMsg:
		return s.handleSaved(st, m)

	case tea.KeyMsg:
		if !st.Ready {
			if m.String() == "q" || m.String() == "ctrl+c" {
				return flow.Quit, nil
			}
			return flow.Continue, nil
		}
		if st.Saving {
			if m.String() == "ctrl+c" {
				return flow.Quit, nil
			}
			return flow.Continue, nil
		}
		if st.Editing {
			return s.updateEditing(m, st)
		}
		return s.updateList(m, st)
	}
	return flow.Continue, nil
}

func (s *settingsStep[T]) updateList(m tea.KeyMsg, st *SettingsState) (flow.Directive, tea.Cmd) {
	switch m.String() {
	case "q", "ctrl+c":
		return flow.Quit, nil
	case "esc":
		return flow.Back, nil
	case "up", "k":
		st.Cursor = (st.Cursor - 1 + len(settingsRows)) % len(settingsRows)
	case "down", "j":
		st.Cursor = (st.Cursor + 1) % len(settingsRows)
	case "r":
		st.Settings = domain.DefaultSettings()
		st.Message = "Recommended defaults restored; press enter to save."
	case "enter":
		st.Saving = true
		st.Message = ""
		return flow.Continue, s.saveCmd(st)
	default:
		row := settingsRows[st.Cursor]
		if row.Kind == rowChoice {
			// The preset is drawn as a value to flip through, so it changes
			// in place with no separate edit mode.
			switch m.String() {
			case "left", "h":
				st.Settings.Tools.Preset = cyclePreset(st.Settings.Tools.Preset, -1)
			case "right", "l", " ":
				st.Settings.Tools.Preset = cyclePreset(st.Settings.Tools.Preset, 1)
			}
			return flow.Continue, nil
		}
		switch {
		case m.String() == "e":
			s.startEdit(st, settingsValue(st.Settings, row.Key))
		case isNumberKey(m):
			s.startEdit(st, string(m.Runes))
		}
	}
	return flow.Continue, nil
}

// handleSaved applies the result of saveCmd: a real write's outcome, or a
// dry run's preview, following installer.ApplyStep's appliedMsg handling.
func (s *settingsStep[T]) handleSaved(st *SettingsState, m settingsSavedMsg) (flow.Directive, tea.Cmd) {
	st.Saving = false
	if m.err != nil {
		st.Message = m.err.Error()
		return flow.Continue, nil
	}
	if m.dryRun {
		st.Message = fmt.Sprintf("Dry run: would write settings to %s", st.Path)
	} else {
		st.Message = ""
	}
	return flow.Next, nil
}

// saveCmd runs the enter save off Update: in dry-run it only validates the
// settings and never calls domain.SaveSettings, so config.toml is never
// created just by walking through the wizard with --dry-run; otherwise it
// saves exactly as Update used to.
func (s *settingsStep[T]) saveCmd(st *SettingsState) tea.Cmd {
	path := st.Path
	cfg := st.Settings
	dryRun := s.opts.DryRun
	return func() tea.Msg {
		if dryRun {
			if err := domain.ValidateSettings(cfg); err != nil {
				return settingsSavedMsg{err: err}
			}
			return settingsSavedMsg{dryRun: true}
		}
		if err := domain.SaveSettings(path, cfg); err != nil {
			return settingsSavedMsg{err: err}
		}
		return settingsSavedMsg{}
	}
}

// startEdit opens the numeric editor on the highlighted row with input as
// its starting text: the current value for "e", or the typed character.
func (s *settingsStep[T]) startEdit(st *SettingsState, input string) {
	st.Editing = true
	st.Message = ""
	st.Input = input
}

func (s *settingsStep[T]) updateEditing(m tea.KeyMsg, st *SettingsState) (flow.Directive, tea.Cmd) {
	row := settingsRows[st.Cursor]

	switch m.String() {
	case "enter":
		cfg, err := setSettingsValue(st.Settings, row.Key, st.Input)
		if err != nil {
			st.Message = err.Error()
			return flow.Continue, nil
		}
		st.Settings = cfg
		st.Editing = false
		st.Message = ""
	case "esc":
		st.Editing = false
		st.Message = ""
	case "backspace":
		if len(st.Input) > 0 {
			r := []rune(st.Input)
			st.Input = string(r[:len(r)-1])
		}
	default:
		if len(m.Runes) > 0 {
			st.Input += string(m.Runes)
		}
	}
	return flow.Continue, nil
}

func (s *settingsStep[T]) View(state *T) string {
	st := s.get(state)
	if st == nil {
		return ""
	}

	frame := 0
	if base := baseStateOf(state); base != nil {
		frame = base.Spinner.Frame
	}
	if !st.Ready {
		return tui.Section(tui.DefaultTheme, "", fmt.Sprintf("  %s Loading settings...\n", tui.SpinFrame(frame)))
	}

	styles := tui.DefaultTheme.Styles()
	var b strings.Builder

	if st.Saving {
		verb := "Saving settings..."
		if s.opts.DryRun {
			verb = "Computing what would be written..."
		}
		fmt.Fprintf(&b, "  %s %s\n\n", tui.SpinFrame(frame), verb)
	}

	for i, row := range settingsRows {
		cursor := " "
		if i == st.Cursor {
			cursor = styles.Cursor.Render(">")
		}
		value := settingsValue(st.Settings, row.Key)
		switch {
		case st.Editing && i == st.Cursor:
			value = st.Input + "_"
		case row.Kind == rowChoice:
			value = "< " + value + " >"
		}
		fmt.Fprintf(&b, " %s %-34s %s\n", cursor, row.Label, value)
	}

	// A status message (an error, a restore) wins; otherwise explain the
	// highlighted row, so what each band means is always on screen.
	message := st.Message
	if message == "" {
		message = settingsRows[st.Cursor].Help
	}
	b.WriteString("\n  " + message)

	var footer string
	if !st.Saving {
		footer = tui.Hints(tui.DefaultTheme, settingsHints(st)...)
	}
	b.WriteString("\n" + tui.Footer(tui.DefaultTheme, footer))
	return tui.Section(tui.DefaultTheme, s.Title(state), b.String())
}
