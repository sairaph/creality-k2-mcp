package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/settingsform"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// settingsScreen edits the persisted settings document (preset, mid-print
// bands, idle heat timeout; dev_docs/safety-architecture.md D1/D2/D6) through
// internal/settingsform, the same editor the install wizard's Settings step
// renders, so the two can never disagree about what a saved settings file
// means. Enter saves and stays on the screen; q or esc leave and drop unsaved
// edits (a save is always an explicit enter).
type settingsScreen struct {
	form    settingsform.Form
	path    string
	ready   bool
	loadErr string
	saving  bool
	spin    spinner
}

func newSettingsScreen() *settingsScreen { return &settingsScreen{spin: newSpinner()} }

type settingsLoadedMsg struct {
	settings domain.Settings
	path     string
	err      error
}

type settingsSavedMsg struct {
	err error
}

func (s *settingsScreen) Init() tea.Cmd {
	return tea.Batch(s.loadCmd(), s.spin.ensure(true))
}

func (s *settingsScreen) loadCmd() tea.Cmd {
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

func (s *settingsScreen) spinning() bool { return (!s.ready && s.loadErr == "") || s.saving }

func (s *settingsScreen) Update(msg tea.Msg) (tea.Cmd, Nav) {
	cmd := s.update(msg)
	return tea.Batch(cmd, s.spin.ensure(s.spinning())), NavNone
}

func (s *settingsScreen) update(msg tea.Msg) tea.Cmd {
	if handled, cmd := s.spin.update(msg, s.spinning()); handled {
		return cmd
	}
	switch m := msg.(type) {
	case settingsLoadedMsg:
		if m.err != nil {
			s.loadErr = m.err.Error()
			return nil
		}
		s.form.Load(m.settings)
		s.path, s.ready, s.loadErr = m.path, true, ""

	case settingsSavedMsg:
		s.saving = false
		if m.err != nil {
			s.form.SetError(m.err.Error())
			return nil
		}
		s.form.Saved()

	case tea.KeyMsg:
		if s.loadErr != "" {
			if m.String() == "r" {
				s.loadErr = ""
				return s.loadCmd()
			}
			return nil
		}
		if !s.ready || s.saving {
			return nil
		}
		if _, ev := s.form.Update(m); ev == settingsform.EventSave {
			s.saving = true
			return s.saveCmd()
		}
	}
	return nil
}

func (s *settingsScreen) saveCmd() tea.Cmd {
	path, cfg := s.path, s.form.Values()
	return func() tea.Msg {
		return settingsSavedMsg{err: domain.SaveSettings(path, cfg)}
	}
}

// Back closes the number editor; with none open the root leaves the screen.
func (s *settingsScreen) Back() bool { return s.ready && s.form.CancelEdit() }

func (s *settingsScreen) Mode() Mode {
	switch {
	case s.saving:
		return ModeBusy
	case s.ready && s.form.Typing():
		return ModeTyping
	}
	return ModeNormal
}

func (s *settingsScreen) Header() frame.Header {
	h := frame.Header{Name: "Settings"}
	if s.ready && s.form.Dirty() {
		h.Context = "unsaved changes"
	}
	return h
}

func (s *settingsScreen) Body(w, h int) []string {
	switch {
	case s.loadErr != "":
		return wrapStyled(w, styleError, s.loadErr)
	case !s.ready:
		return []string{frame.Gutter + s.spin.glyph() + " Loading settings..."}
	}
	return s.form.Body(w)
}

func (s *settingsScreen) Hints(w, h int) []frame.Hint {
	switch {
	case s.saving:
		return busyHints("saving...")
	case s.loadErr != "":
		return []frame.Hint{{Keys: "r", Label: "retry", Priority: 80}, frame.Back(), frame.Quit()}
	case !s.ready:
		return []frame.Hint{frame.Back(), frame.Quit()}
	case s.form.Typing():
		return s.form.Hints()
	}
	return append(s.form.Hints(), frame.Back(), frame.Quit())
}
