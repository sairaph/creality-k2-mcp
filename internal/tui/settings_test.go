package tui

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func settingsHarness(t *testing.T, w, h int) *harness {
	t.Helper()
	isolateHome(t)
	hs := newHarness(t, Deps{}, w, h)
	hs.open("Settings")
	return hs
}

func savedSettings(t *testing.T) domain.Settings {
	t.Helper()
	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := domain.ReadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSettingsFrameAndRows(t *testing.T) {
	for _, sz := range sizes {
		h := settingsHarness(t, sz[0], sz[1])
		h.requireFrame("settings")
		ls := h.lines()
		if ls[0] != "creality-k2-mcp  Settings" {
			t.Errorf("header = %q", ls[0])
		}
		for _, want := range []string{"Tool preset", "< camera >", "Idle heat timeout (minutes)", "Flow factor max (%)"} {
			if !strings.Contains(h.text(), want) {
				t.Errorf("%dx%d: missing %q:\n%s", sz[0], sz[1], want, h.text())
			}
		}
		if !strings.HasPrefix(ls[2], "  > Tool preset") {
			t.Errorf("first row = %q, want the cursor on the preset", ls[2])
		}
		if !strings.Contains(h.text(), "Which tools the AI can use") {
			t.Errorf("the highlighted row is not explained:\n%s", h.text())
		}
	}
	h := settingsHarness(t, 120, 36)
	if got := strings.TrimSpace(h.footer()); got != "↑↓ move · ←→ change · r restore defaults · enter save · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
}

func TestSettingsEditSaveStaysOnTheScreen(t *testing.T) {
	h := settingsHarness(t, 120, 36)
	h.key("down", "e")
	h.requireFrame("settings while typing")
	if got := strings.TrimSpace(h.footer()); got != "enter confirm · esc cancel" {
		t.Errorf("typing footer = %q", got)
	}
	h.key("backspace", "backspace")
	h.typeText("30")
	h.key("enter")
	if got := h.lines()[0]; got != "creality-k2-mcp  Settings  unsaved changes" {
		t.Errorf("header = %q, want the unsaved marker", got)
	}
	if savedSettings(t).IdleHeatMinutes == 30 {
		t.Fatal("an edit must not save by itself")
	}

	h.key("enter") // save
	if !strings.Contains(h.text(), "Saved.") {
		t.Errorf("want Saved.:\n%s", h.text())
	}
	if h.lines()[0] != "creality-k2-mcp  Settings" {
		t.Errorf("after saving the screen must stay clean: %q", h.lines()[0])
	}
	if got := savedSettings(t).IdleHeatMinutes; got != 30 {
		t.Errorf("saved IdleHeatMinutes = %d, want 30", got)
	}
	h.requireFrame("settings after saving")
}

func TestSettingsRejectsAnInvalidNumberInline(t *testing.T) {
	h := settingsHarness(t, 80, 24)
	h.key("down", "e", "backspace", "backspace")
	h.typeText("1.5")
	h.key("enter")
	if !strings.Contains(h.text(), "must be a whole number") {
		t.Errorf("want the reason inline:\n%s", h.text())
	}
	if got := strings.TrimSpace(h.footer()); got != "enter confirm · esc cancel" {
		t.Errorf("the editor must stay open, footer = %q", got)
	}
	h.requireFrame("settings with an inline error")
}

func TestSettingsTypingQAndEsc(t *testing.T) {
	h := settingsHarness(t, 120, 36)
	h.key("down", "e")
	h.typeText("q")
	if h.quit || !strings.Contains(h.text(), "15q_") {
		t.Fatalf("q must be typed into the field (quit=%v):\n%s", h.quit, h.text())
	}
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Settings") || strings.Contains(h.text(), "15q") {
		t.Fatalf("esc must cancel the field and stay:\n%s", h.text())
	}
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Errorf("the second esc must leave: %q", h.lines()[0])
	}
}

func TestSettingsQQuitsAndDropsUnsavedEdits(t *testing.T) {
	h := settingsHarness(t, 120, 36)
	h.key("down", "e", "backspace", "backspace")
	h.typeText("99")
	h.key("enter")
	h.key("q")
	if !h.quit {
		t.Fatal("q did not quit")
	}
	if got := savedSettings(t).IdleHeatMinutes; got == 99 {
		t.Error("an unsaved edit was written")
	}
}

func TestSettingsPresetChangesInPlaceAndRestoreDefaults(t *testing.T) {
	h := settingsHarness(t, 120, 36)
	h.key("right")
	if !strings.Contains(h.text(), "< control >") {
		t.Errorf("right did not change the preset:\n%s", h.text())
	}
	h.key("left", "left")
	if !strings.Contains(h.text(), "< monitor >") {
		t.Errorf("left did not change the preset:\n%s", h.text())
	}
	h.key("r")
	if !strings.Contains(h.text(), "< camera >") || !strings.Contains(h.text(), "Recommended defaults restored") {
		t.Errorf("r did not restore the defaults:\n%s", h.text())
	}
}

func TestSettingsBusyWhileSaving(t *testing.T) {
	h := settingsHarness(t, 120, 36)
	s := h.m.screen.(*settingsScreen)
	s.saving = true
	h.requireFrame("settings while saving")
	if got := strings.TrimSpace(h.footer()); got != "saving... · ctrl+c quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("q", "esc", "down")
	if h.quit || !strings.Contains(h.lines()[0], "Settings") {
		t.Errorf("a key got through while saving (quit=%v)", h.quit)
	}
	h.key("ctrl+c")
	if !h.quit {
		t.Error("ctrl+c must quit at once")
	}
}

func TestSettingsSaveFailureIsShown(t *testing.T) {
	h := settingsHarness(t, 120, 36)
	s := h.m.screen.(*settingsScreen)
	s.saving = true
	h.send(settingsSavedMsg{err: errors.New("disk is full")})
	if !strings.Contains(h.text(), "disk is full") || s.saving {
		t.Errorf("the failure is not shown:\n%s", h.text())
	}
}

func TestSettingsLoadErrorOffersRetry(t *testing.T) {
	h := settingsHarness(t, 120, 36)
	h.send(settingsLoadedMsg{err: errors.New("settings are unreadable")})
	h.requireFrame("settings load error")
	if !strings.Contains(h.text(), "settings are unreadable") {
		t.Errorf("the error is not shown:\n%s", h.text())
	}
	if got := strings.TrimSpace(h.footer()); got != "r retry · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("r")
	if !strings.Contains(h.text(), "Tool preset") {
		t.Errorf("retry did not reload:\n%s", h.text())
	}
}

// Opening Settings never creates the file; only an explicit save does.
func TestSettingsOpeningDoesNotWrite(t *testing.T) {
	h := settingsHarness(t, 80, 24)
	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("opening Settings created %s (stat err %v)", path, err)
	}
	h.key("esc")
}
