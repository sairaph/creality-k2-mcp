package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"
)

// selectMenuItem moves the main menu's cursor to label and presses enter,
// then drains every resulting command (including the one extra Update
// cycle a menu selection needs to turn its app.Action into a Step change,
// exactly as bubbletea's own event loop would deliver it).
func selectMenuItem(t *testing.T, m *Model, label string) *Model {
	t.Helper()
	for m.menu.Items[m.menu.Cursor].Label != label {
		m.menu.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	newModelIface, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModelIface.(*Model)
	for _, msg := range drainCmd(cmd) {
		newModelIface, cmd = m.Update(msg)
		m = newModelIface.(*Model)
		for _, inner := range drainCmd(cmd) {
			newModelIface, _ = m.Update(inner)
			m = newModelIface.(*Model)
		}
	}
	return m
}

func TestModelMenuOpensEachScreen(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	cases := []struct {
		label string
		step  app.Step
	}{
		{"Printers", stepPrinters},
		{"Status", stepStatus},
		{"Camera", stepCamera},
		{"Recordings", stepRecordings},
		{"Settings", stepSettings},
		{"Run doctor", stepDoctor},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := newModel(ctx, "test", deps)
			m.Init()
			m = selectMenuItem(t, m, tc.label)
			if m.Step != tc.step {
				t.Fatalf("Step = %v, want %v", m.Step, tc.step)
			}
			if m.View() == "" {
				t.Error("View() is empty after opening the screen")
			}
		})
	}
}

func TestModelDoctorScreenAndBack(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{
		PrinterClients: fakePrinterClients(nil),
		RunDoctor:      func(context.Context) string { return "all checks passed" },
	}.withDefaults()

	m := newModel(ctx, "test", deps)
	m.Init()
	m = selectMenuItem(t, m, "Run doctor")
	if m.Step != stepDoctor {
		t.Fatalf("Step = %v, want stepDoctor", m.Step)
	}
	if !strings.Contains(m.View(), "all checks passed") {
		t.Errorf("View() = %q, want the doctor report", m.View())
	}

	newModelIface, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = newModelIface.(*Model)
	for _, msg := range drainCmd(cmd) {
		newModelIface, _ = m.Update(msg)
		m = newModelIface.(*Model)
	}
	if m.Step != stepMenu {
		t.Fatalf("Step = %v, want stepMenu after esc from the doctor report", m.Step)
	}
}

func TestModelQuit(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{}.withDefaults()
	m := newModel(ctx, "test", deps)
	m.Init()
	newModelIface, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = newModelIface.(*Model)
	for _, msg := range drainCmd(cmd) {
		newModelIface, _ := m.Update(msg)
		m = newModelIface.(*Model)
	}
	if !m.Quit {
		t.Error("expected Quit=true after q on the main menu")
	}
}
