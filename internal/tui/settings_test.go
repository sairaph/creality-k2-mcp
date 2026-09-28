package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
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
	s.Update(keyType(tea.KeyEnter)) // start editing
	if !s.editing {
		t.Fatal("expected editing after enter")
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

	cmd := s.Update(keyRune('s'))
	if !s.saving {
		t.Fatal("expected saving after s")
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
	s.Update(keyType(tea.KeyEnter))
	s.input = "not a number"
	s.Update(keyType(tea.KeyEnter))
	if !s.editing {
		t.Error("an invalid value must keep the editor open")
	}
	if s.message == "" {
		t.Error("expected an inline error message for the invalid value")
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
