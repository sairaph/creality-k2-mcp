package wizard

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

type settingsTestState struct {
	flow.BaseState
	Settings SettingsState
}

func settingsTestStateFn(s *settingsTestState) *SettingsState { return &s.Settings }

func newSettingsStep() *settingsStep[settingsTestState] {
	return &settingsStep[settingsTestState]{stateFn: settingsTestStateFn}
}

func newDryRunSettingsStep() *settingsStep[settingsTestState] {
	return &settingsStep[settingsTestState]{stateFn: settingsTestStateFn, opts: SettingsStepOptions{DryRun: true}}
}

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
	if st.Settings.Settings.Tools.Preset != domain.PresetCamera {
		t.Errorf("default preset = %q, want %q (sensible default preselected)", st.Settings.Settings.Tools.Preset, domain.PresetCamera)
	}
	if st.Settings.Settings.IdleHeatMinutes != 15 {
		t.Errorf("default idle_heat_minutes = %d, want 15", st.Settings.Settings.IdleHeatMinutes)
	}
	if _, err := os.Stat(loaded.path); !os.IsNotExist(err) {
		t.Errorf("Init must never create %s on its own, stat err = %v", loaded.path, err)
	}
}

func TestSettingsStepEditNumericField(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Cursor = 1 // idle_heat_minutes

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}, st) // start editing
	if !st.Settings.Editing {
		t.Fatal("e should start editing the selected row")
	}
	if st.Settings.Input != "15" {
		t.Fatalf("Input = %q, want the current value %q", st.Settings.Input, "15")
	}

	// Replace the value with 30.
	st.Settings.Input = ""
	for _, r := range "30" {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, st)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st) // confirm
	if st.Settings.Editing {
		t.Error("Editing should be false after confirming a valid value")
	}
	if st.Settings.Settings.IdleHeatMinutes != 30 {
		t.Errorf("IdleHeatMinutes = %d, want 30", st.Settings.Settings.IdleHeatMinutes)
	}
}

func TestSettingsStepRejectsInvalidNumericField(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Cursor = 1 // idle_heat_minutes
	st.Settings.Editing = true
	st.Settings.Input = "not a number"

	s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if !st.Settings.Editing {
		t.Error("an invalid value should keep the field open for editing")
	}
	if st.Settings.Message == "" {
		t.Error("an invalid value should leave a message explaining why")
	}
	if st.Settings.Settings.IdleHeatMinutes == 0 {
		// sanity: default is nonzero and must be untouched
		t.Error("the invalid edit must not have been applied")
	}
}

func TestSettingsStepRejectsOutOfRangeBand(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Cursor = 2 // nozzle_band_c
	st.Settings.Editing = true
	st.Settings.Input = "500" // out of domain.validateBands' 0-100 range

	s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if !st.Settings.Editing {
		t.Error("an out-of-range band should keep the field open for editing")
	}
	if st.Settings.Settings.Bands.NozzleBandC == 500 {
		t.Error("an out-of-range band must never be applied")
	}
}

// The preset is drawn as "< camera >" and changes in place with left/right
// (and space), with no edit mode, and the footer says so.
func TestSettingsStepPresetChangesInPlace(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings() // camera
	st.Settings.Cursor = 0                          // preset row

	view := s.View(st)
	if !strings.Contains(view, "< camera >") || !strings.Contains(view, "←→ change") {
		t.Errorf("preset row should read < camera > with a ←→ change hint:\n%s", view)
	}
	var hinted bool
	for _, h := range s.Hints(st) {
		if h.Key == "←→" {
			hinted = true
		}
	}
	if !hinted {
		t.Error("Hints() should include ←→ on the preset row")
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRight}, st)
	if st.Settings.Editing {
		t.Fatal("changing the preset must not open an edit mode")
	}
	if got := st.Settings.Settings.Tools.Preset; got != domain.PresetControl {
		t.Fatalf("after right: %q, want %q", got, domain.PresetControl)
	}
	s.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, st) // wraps to monitor
	if got := st.Settings.Settings.Tools.Preset; got != domain.PresetMonitor {
		t.Fatalf("after space: %q, want %q", got, domain.PresetMonitor)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyLeft}, st) // wraps back to control
	if got := st.Settings.Settings.Tools.Preset; got != domain.PresetControl {
		t.Fatalf("after left: %q, want %q", got, domain.PresetControl)
	}
}

// A numeric row is edited with e (starting from the current value) or by
// just typing a number (replacing it).
func TestSettingsStepTypingStartsNumericEdit(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Cursor = 2 // nozzle_band_c

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("8")}, st)
	if !st.Settings.Editing || st.Settings.Input != "8" {
		t.Fatalf("typing 8 should start editing with input 8, got editing=%v input=%q", st.Settings.Editing, st.Settings.Input)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if st.Settings.Editing || st.Settings.Settings.Bands.NozzleBandC != 8 {
		t.Fatalf("enter should confirm 8, got editing=%v band=%v", st.Settings.Editing, st.Settings.Settings.Bands.NozzleBandC)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")}, st)
	if !st.Settings.Editing || st.Settings.Input != "8" {
		t.Fatalf("e should edit the current value, got editing=%v input=%q", st.Settings.Editing, st.Settings.Input)
	}
}

// The highlighted row's help is always shown, so what a band means is on
// screen without opening anything.
func TestSettingsStepViewExplainsHighlightedRow(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Cursor = 2 // nozzle_band_c

	if view := s.View(st); !strings.Contains(view, "your gcode is never limited") {
		t.Errorf("View should explain the highlighted band:\n%s", view)
	}
}

func TestSettingsStepRestoreDefaults(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Settings.IdleHeatMinutes = 99
	st.Settings.Settings.Tools.Preset = domain.PresetControl

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}, st)
	if st.Settings.Settings.IdleHeatMinutes != 15 || st.Settings.Settings.Tools.Preset != domain.PresetCamera {
		t.Errorf("restore defaults did not reset to domain.DefaultSettings(): %+v", st.Settings.Settings)
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
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Settings.IdleHeatMinutes = 42
	st.Settings.Path = path

	directive, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
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
	if d, c := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}, st); d != flow.Continue || c != nil {
		t.Error("keys must be ignored while Saving")
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
		t.Fatalf("directive after settingsSavedMsg = %v, want Next", directive)
	}
	if cmd != nil {
		t.Error("handling settingsSavedMsg should not chain another command")
	}
	if st.Settings.Saving {
		t.Error("Saving should be false once settingsSavedMsg is handled")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected settings file to exist: %v", err)
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
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()
	st.Settings.Path = path

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if cmd == nil {
		t.Fatal("expected a non-nil save cmd")
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
	if st.Settings.Message == "" {
		t.Error("dry-run should leave a message describing what would be written")
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run must never touch the filesystem, found %v under %s", entries, home)
	}
}

func TestSettingsStepEscGoesBack(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true

	directive, _ := s.Update(tea.KeyMsg{Type: tea.KeyEsc}, st)
	if directive != flow.Back {
		t.Errorf("directive = %v, want Back", directive)
	}
}

func TestSettingsStepViewShowsRowsAndValues(t *testing.T) {
	s := newSettingsStep()
	st := &settingsTestState{}
	st.Settings.Ready = true
	st.Settings.Settings = domain.DefaultSettings()

	out := s.View(st)
	for _, want := range []string{"Tool preset", "camera", "Idle heat timeout", "15"} {
		if !strings.Contains(out, want) {
			t.Errorf("View output missing %q:\n%s", want, out)
		}
	}
}
