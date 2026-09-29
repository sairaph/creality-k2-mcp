package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func initSettingsScreen(t *testing.T) *settingsScreen {
	t.Helper()
	s := newSettingsScreen()
	for _, m := range drainCmd(s.Init()) {
		s.Update(m)
	}
	if !s.ready {
		t.Fatal("settings screen not ready after Init")
	}
	return s
}

func TestSettingsScreenLoadsDefaults(t *testing.T) {
	isolateHome(t)
	s := initSettingsScreen(t)
	if s.settings.IdleHeatMinutes != domain.DefaultSettings().IdleHeatMinutes {
		t.Errorf("IdleHeatMinutes = %d, want the default", s.settings.IdleHeatMinutes)
	}
	view := s.View()
	if view == "" {
		t.Fatal("View() is empty")
	}
}

func TestSettingsScreenEditAndSave(t *testing.T) {
	isolateHome(t)
	s := initSettingsScreen(t)

	// Cursor starts on "preset" (row 0); move to idle_heat_minutes (row 1).
	s.Update(keyType(tea.KeyDown))
	s.Update(keyRune('e')) // start editing
	if !s.editing {
		t.Fatal("expected editing after e")
	}
	s.input = ""
	for _, r := range "30" {
		s.Update(keyRune(r))
	}
	s.Update(keyType(tea.KeyEnter)) // confirm the edit
	if s.editing {
		t.Fatal("still editing after confirming")
	}
	if s.settings.IdleHeatMinutes != 30 {
		t.Fatalf("IdleHeatMinutes = %d, want 30", s.settings.IdleHeatMinutes)
	}

	cmd := s.Update(keyType(tea.KeyEnter))
	if !s.saving {
		t.Fatal("expected saving after enter")
	}
	for _, m := range drainCmd(cmd) {
		s.Update(m)
	}
	if s.saving {
		t.Error("still saving after save completed")
	}

	cfg, err := domain.ReadSettings(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleHeatMinutes != 30 {
		t.Errorf("saved IdleHeatMinutes = %d, want 30", cfg.IdleHeatMinutes)
	}
}

func TestSettingsScreenRejectsInvalidEdit(t *testing.T) {
	isolateHome(t)
	s := initSettingsScreen(t)
	s.Update(keyType(tea.KeyDown))
	s.Update(keyRune('e'))
	s.input = "not a number"
	s.Update(keyType(tea.KeyEnter))
	if !s.editing {
		t.Error("an invalid value must keep the editor open")
	}
	if s.message == "" {
		t.Error("expected an inline error message for the invalid value")
	}
}

// The preset is drawn as "< camera >" and changes in place with left/right,
// with no edit mode, and the footer says so.
func TestSettingsScreenPresetChangesInPlace(t *testing.T) {
	isolateHome(t)
	s := initSettingsScreen(t) // cursor starts on preset
	view := s.View()
	if !strings.Contains(view, "< camera >") || !strings.Contains(view, "←→ change") {
		t.Errorf("preset row should read < camera > with a ←→ change hint:\n%s", view)
	}
	s.Update(keyType(tea.KeyRight))
	if s.editing {
		t.Fatal("changing the preset must not open an edit mode")
	}
	if s.settings.Tools.Preset != domain.PresetControl {
		t.Fatalf("preset = %q after right from camera, want %q", s.settings.Tools.Preset, domain.PresetControl)
	}
	s.Update(keyType(tea.KeyLeft))
	s.Update(keyType(tea.KeyLeft))
	if s.settings.Tools.Preset != domain.PresetMonitor {
		t.Errorf("preset = %q after two lefts from control, want %q", s.settings.Tools.Preset, domain.PresetMonitor)
	}
}

// Typing a number on a numeric row starts editing it with that number.
func TestSettingsScreenTypingStartsNumericEdit(t *testing.T) {
	isolateHome(t)
	s := initSettingsScreen(t)
	s.Update(keyType(tea.KeyDown)) // idle_heat_minutes
	s.Update(keyRune('3'))
	s.Update(keyRune('0'))
	if !s.editing || s.input != "30" {
		t.Fatalf("editing=%v input=%q, want editing with 30", s.editing, s.input)
	}
	s.Update(keyType(tea.KeyEnter))
	if s.editing || s.settings.IdleHeatMinutes != 30 {
		t.Fatalf("editing=%v IdleHeatMinutes=%d, want 30 confirmed", s.editing, s.settings.IdleHeatMinutes)
	}
}

func TestSettingsScreenRestoreDefaults(t *testing.T) {
	isolateHome(t)
	s := initSettingsScreen(t)
	s.settings.IdleHeatMinutes = 200
	s.Update(keyRune('r'))
	if s.settings.IdleHeatMinutes != domain.DefaultSettings().IdleHeatMinutes {
		t.Errorf("IdleHeatMinutes = %d after restore, want the default", s.settings.IdleHeatMinutes)
	}
}
