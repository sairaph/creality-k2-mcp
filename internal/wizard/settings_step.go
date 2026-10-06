package wizard

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/settingsform"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// SettingsState is embedded in consumer state for the Settings step. The rows,
// values, editing and validation live in the shared settingsform.Form (the
// app's Settings screen renders the same one); this state only adds the load
// and save bookkeeping of a flow step.
type SettingsState struct {
	Form  settingsform.Form
	Path  string
	Ready bool

	// Saving is true while the save enter started is in flight, mirroring
	// installer.ApplyStep's Done gating so a save runs as a tea.Cmd instead
	// of blocking Update.
	Saving bool
	// Wrote is true once a real (not dry-run) save reached disk, so the cancel
	// line after the program can say what was kept.
	Wrote bool
}

// SettingsStepOptions controls the Settings step.
type SettingsStepOptions struct {
	// DryRun computes what enter would save and shows it instead of writing
	// config.toml, matching installer.ApplyStepOptions.DryRun.
	DryRun bool
	// App is the header's first part ("creality-k2-mcp setup", or
	// "creality-k2-mcp project setup" for add); empty means the install one.
	App string
}

// SettingsStep returns a flow.Step that lets the user choose the tool
// preset (dev_docs/safety-architecture.md D6), the mid-print setpoint bands
// (D1) and the idle heat timeout (D2), and saves them to the settings file
// on enter. Defaults come from domain.DefaultSettings (through
// domain.ReadSettings, which never writes; the file is created only by an
// explicit enter, or left untouched entirely in dry-run).
func SettingsStep[T any](stateFn func(*T) *SettingsState, opts SettingsStepOptions) flow.Step[T] {
	return &settingsStep[T]{stateFn: stateFn, opts: opts, chrome: newChrome(opts.App, opts.DryRun)}
}

type settingsStep[T any] struct {
	stateFn func(*T) *SettingsState
	opts    SettingsStepOptions
	chrome  chrome
}

func (s *settingsStep[T]) ID() string { return "settings" }

func (s *settingsStep[T]) Title(state *T) string {
	return "Safety limits and which tools the AI can use:"
}

func (s *settingsStep[T]) Hints(state *T) []struct{ Key, Label string } {
	st := s.get(state)
	if st == nil {
		return nil
	}
	return legacyHints(settingsHints(st))
}

// settingsHints is the one key list both Hints and the footer show, so they
// cannot drift apart: the shared form's own keys (enter saves and continues
// here), then back and cancel when no field is being typed in. Before the
// settings load only cancel works; while the save is in flight only ctrl+c.
func settingsHints(st *SettingsState) []frame.Hint {
	switch {
	case !st.Ready:
		return []frame.Hint{frame.Cancel()}
	case st.Saving:
		return busyHints("saving...")
	}
	out := st.Form.Hints()
	if !st.Form.Typing() {
		out = append(out, frame.Back(), frame.Cancel())
	}
	return out
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
		st.Form.Load(m.settings)
		st.Form.EnterLabel = "continue"
		st.Path = m.path
		st.Ready = true
		return flow.Continue, nil

	case settingsSavedMsg:
		return s.handleSaved(st, m)

	case tea.KeyMsg:
		if smallDrops(baseStateOf(state), m.String(), st.Ready && st.Form.Typing()) {
			return flow.Continue, nil
		}
		if m.String() == "ctrl+c" {
			return flow.Quit, nil
		}
		if !st.Ready || st.Saving {
			if !st.Ready && m.String() == "q" {
				return flow.Quit, nil
			}
			return flow.Continue, nil
		}
		if !st.Form.Typing() {
			switch m.String() {
			case "q":
				return flow.Quit, nil
			case "esc":
				return flow.Back, nil
			}
		}
		if _, ev := st.Form.Update(m); ev == settingsform.EventSave {
			st.Saving = true
			return flow.Continue, s.saveCmd(st)
		}
	}
	return flow.Continue, nil
}

// handleSaved applies the result of saveCmd: a real write's outcome, or a
// dry run's preview, following installer.ApplyStep's appliedMsg handling.
func (s *settingsStep[T]) handleSaved(st *SettingsState, m settingsSavedMsg) (flow.Directive, tea.Cmd) {
	st.Saving = false
	if m.err != nil {
		st.Form.SetError(m.err.Error())
		return flow.Continue, nil
	}
	st.Form.Saved()
	if !m.dryRun {
		st.Wrote = true
	}
	return flow.Next, nil
}

// saveCmd runs the enter save off Update: in dry-run it only validates the
// settings and never calls domain.SaveSettings, so config.toml is never
// created just by walking through the wizard with --dry-run; otherwise it
// saves exactly as Update used to.
func (s *settingsStep[T]) saveCmd(st *SettingsState) tea.Cmd {
	path := st.Path
	cfg := st.Form.Values()
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

func (s *settingsStep[T]) View(state *T) string {
	st := s.get(state)
	if st == nil {
		return ""
	}
	base := baseStateOf(state)
	return s.chrome.draw(base, "Settings", 2, settingsHints(st), func(w, rows int) []string {
		out := questionLines(w, s.Title(state))
		glyph := frame.Spinner(spinnerFrame(base))
		if !st.Ready {
			return append(out, frame.Gutter+glyph+" Loading settings...")
		}
		if st.Saving {
			verb := "Saving settings..."
			if s.opts.DryRun {
				verb = "Computing what would be written..."
			}
			out = append(out, frame.Gutter+glyph+" "+verb, "")
		}
		return append(out, st.Form.Body(w)...)
	})
}
