package wizard

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

type settingsTestState struct {
	flow.BaseState
	Settings SettingsState
}

func settingsTestStateFn(s *settingsTestState) *SettingsState { return &s.Settings }

func newSettingsStep() *settingsStep[settingsTestState] {
	return &settingsStep[settingsTestState]{stateFn: settingsTestStateFn, chrome: newChrome("", false)}
}

func newDryRunSettingsStep() *settingsStep[settingsTestState] {
	return &settingsStep[settingsTestState]{stateFn: settingsTestStateFn, opts: SettingsStepOptions{DryRun: true}, chrome: newChrome("", true)}
}

// readySettingsState is a loaded step state holding the default settings.
func readySettingsState() *settingsTestState {
	st := &settingsTestState{}
	st.Width, st.Height = 120, 36
	st.Settings.Ready = true
	st.Settings.Form.Load(domain.DefaultSettings())
	st.Settings.Form.EnterLabel = "continue"
	return st
}

func keyRune(r rune) tea.KeyMsg        { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func keyType(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func TestSettingsStepIDAndTitle(t *testing.T) {
	s := newSettingsStep()
	if s.ID() != "settings" {
		t.Errorf("ID() = %q, want %q", s.ID(), "settings")
	}
}

func TestSettingsStepInitLoadsDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := newSettingsStep()
	st := &settingsTestState{}
	cmd := s.Init(st)
	if cmd == nil {
		t.Fatal("Init returned a nil cmd")
	}
	msg := cmd()
	loaded, ok := msg.(settingsLoadedMsg)
	if !ok {
		t.Fatalf("Init produced %T, want settingsLoadedMsg", msg)
	}
	if loaded.err != nil {
		t.Fatalf("settingsLoadedMsg.err = %v", loaded.err)
	}

	s.Update(loaded, st)
	if !st.Settings.Ready {
		t.Error("Ready should be true after settingsLoadedMsg")
	}
	got := st.Settings.Form.Values()
	if got.Tools.Preset != domain.PresetCamera {
		t.Errorf("default preset = %q, want %q (sensible default preselected)", got.Tools.Preset, domain.PresetCamera)
	}
	if got.IdleHeatMinutes != 15 {
		t.Errorf("default idle_heat_minutes = %d, want 15", got.IdleHeatMinutes)
	}
	if _, err := os.Stat(loaded.path); !os.IsNotExist(err) {
		t.Errorf("Init must never create %s on its own, stat err = %v", loaded.path, err)
	}
}

// The step edits through the shared settingsform.Form: the number editor, its
// validation and the preset cycling are exercised in that package's own tests;
// here one pass proves the step passes keys to it and renders its body.
func TestSettingsStepDelegatesEditingToTheSharedForm(t *testing.T) {
	s := newSettingsStep()
	st := readySettingsState()

	s.Update(keyType(tea.KeyDown), st) // idle_heat_minutes
	s.Update(keyRune('4'), st)
	s.Update(keyRune('0'), st)
	if !st.Settings.Form.Typing() {
		t.Fatal("typing a number should open the shared form's editor")
	}
	s.Update(keyType(tea.KeyEnter), st) // confirm the edit, not a save
	if st.Settings.Form.Typing() || st.Settings.Saving {
		t.Fatalf("typing=%v saving=%v, want the edit confirmed without saving", st.Settings.Form.Typing(), st.Settings.Saving)
	}
	if got := st.Settings.Form.Values().IdleHeatMinutes; got != 40 {
		t.Fatalf("IdleHeatMinutes = %d, want 40", got)
	}

	view := s.View(st)
	for _, want := range []string{"Tool preset", "< camera >", "Idle heat timeout", "40"} {
		if !strings.Contains(view, want) {
			t.Errorf("View output missing %q:\n%s", want, view)
		}
	}
}

func TestSettingsStepQIsACharacterWhileTyping(t *testing.T) {
	s := newSettingsStep()
	st := readySettingsState()
	s.Update(keyType(tea.KeyDown), st)
	s.Update(keyRune('e'), st)

	if d, _ := s.Update(keyRune('q'), st); d != flow.Continue {
		t.Fatalf("q while typing gave directive %v, want Continue (it is a character)", d)
	}
	if d, _ := s.Update(keyType(tea.KeyEsc), st); d != flow.Continue || st.Settings.Form.Typing() {
		t.Fatalf("esc while typing gave directive %v typing=%v, want the field cancelled", d, st.Settings.Form.Typing())
	}
	if d, _ := s.Update(keyRune('q'), st); d != flow.Quit {
		t.Errorf("q on the list gave directive %v, want Quit", d)
	}
}

func TestSettingsStepHintsFollowTheForm(t *testing.T) {
	s := newSettingsStep()
	st := readySettingsState()

	var keys []string
	for _, h := range s.Hints(st) {
		keys = append(keys, h.Key+" "+h.Label)
	}
	joined := strings.Join(keys, "|")
	for _, want := range []string{"←→ change", "enter continue", "esc back", "q cancel"} {
		if !strings.Contains(joined, want) {
			t.Errorf("hints %q missing %q", joined, want)
		}
	}

	s.Update(keyType(tea.KeyDown), st)
	s.Update(keyRune('e'), st)
	for _, h := range s.Hints(st) {
		if h.Key == "q" {
			t.Error("q must not be a hint while a field is being typed in")
		}
	}
}

func TestSettingsStepSaveAdvancesAndPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}

	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	cfg := domain.DefaultSettings()
	cfg.IdleHeatMinutes = 42
	st.Settings.Form.Load(cfg)
	st.Settings.Path = path

	directive, cmd := s.Update(keyType(tea.KeyEnter), st)
	if directive != flow.Continue {
		t.Fatalf("directive = %v, want Continue (the save runs as a tea.Cmd)", directive)
	}
	if cmd == nil {
		t.Fatal("enter should return a non-nil cmd that performs the save")
	}
	if !st.Settings.Saving {
		t.Error("Saving should be true while the save cmd is in flight")
	}
	// While saving, keys other than ctrl+c are ignored.
	if d, c := s.Update(keyRune('r'), st); d != flow.Continue || c != nil {
		t.Error("keys must be ignored while Saving")
	}
	if d, _ := s.Update(keyRune('q'), st); d != flow.Continue {
		t.Error("q must not cancel a write in flight")
	}
	if d, _ := s.Update(keyType(tea.KeyCtrlC), st); d != flow.Quit {
		t.Error("ctrl+c must still quit while saving")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the file must not exist before the save cmd has run")
	}

	msg := cmd()
	saved, ok := msg.(settingsSavedMsg)
	if !ok {
		t.Fatalf("save cmd produced %T, want settingsSavedMsg", msg)
	}
	if saved.err != nil {
		t.Fatalf("settingsSavedMsg.err = %v", saved.err)
	}

	directive, cmd = s.Update(saved, st)
	if directive != flow.Next {
		t.Fatalf("directive after settingsSavedMsg = %v, want Next (enter continues in the wizard)", directive)
	}
	if cmd != nil {
		t.Error("handling settingsSavedMsg should not chain another command")
	}
	if st.Settings.Saving {
		t.Error("Saving should be false once settingsSavedMsg is handled")
	}
	onDisk, err := domain.LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.IdleHeatMinutes != 42 {
		t.Errorf("saved IdleHeatMinutes = %d, want 42", onDisk.IdleHeatMinutes)
	}
}

func TestSettingsStepDryRunNeverWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}

	s := newDryRunSettingsStep()
	st := readySettingsState()
	st.Settings.Path = path

	_, cmd := s.Update(keyType(tea.KeyEnter), st)
	if cmd == nil {
		t.Fatal("expected a non-nil save cmd")
	}
	if view := s.View(st); !strings.Contains(view, "Computing what would be written...") {
		t.Errorf("a dry-run save should say what it is doing:\n%s", view)
	}
	msg := cmd()
	saved, ok := msg.(settingsSavedMsg)
	if !ok {
		t.Fatalf("save cmd produced %T, want settingsSavedMsg", msg)
	}
	if saved.err != nil {
		t.Fatalf("settingsSavedMsg.err = %v", saved.err)
	}
	if !saved.dryRun {
		t.Error("settingsSavedMsg.dryRun should be true in dry-run mode")
	}

	directive, _ := s.Update(saved, st)
	if directive != flow.Next {
		t.Fatalf("directive after dry-run save = %v, want Next", directive)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run must never touch the filesystem, found %v under %s", entries, home)
	}
}

func TestSettingsStepSaveErrorStaysOnTheStep(t *testing.T) {
	s := newSettingsStep()
	st := readySettingsState()
	st.Settings.Saving = true

	directive, _ := s.Update(settingsSavedMsg{err: os.ErrPermission}, st)
	if directive != flow.Continue || st.Settings.Saving {
		t.Fatalf("directive=%v saving=%v, want the step kept with the save finished", directive, st.Settings.Saving)
	}
	if !strings.Contains(s.View(st), os.ErrPermission.Error()) {
		t.Errorf("the failure should be shown:\n%s", s.View(st))
	}
}

func TestSettingsStepEscGoesBack(t *testing.T) {
	s := newSettingsStep()
	st := readySettingsState()

	directive, _ := s.Update(keyType(tea.KeyEsc), st)
	if directive != flow.Back {
		t.Errorf("directive = %v, want Back", directive)
	}
}
