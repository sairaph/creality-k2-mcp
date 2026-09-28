package wizard

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
)

type harnessTestState struct {
	flow.BaseState
	Harness installer.HarnessState
}

func harnessTestStateFn(s *harnessTestState) *installer.HarnessState { return &s.Harness }

// toggleStep stands in for installer.HarnessStep: space flips the entry
// under the cursor with installer.ToggleHarness, exactly as the real step
// does, leaving a false entry behind in Selected.
type toggleStep struct{}

func (toggleStep) ID() string                                            { return "harnesses" }
func (toggleStep) Title(*harnessTestState) string                        { return "" }
func (toggleStep) Hints(*harnessTestState) []struct{ Key, Label string } { return nil }
func (toggleStep) Init(*harnessTestState) tea.Cmd                        { return nil }
func (toggleStep) View(*harnessTestState) string                         { return "" }
func (toggleStep) Update(msg tea.Msg, st *harnessTestState) (flow.Directive, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == " " {
		installer.ToggleHarness(&st.Harness)
	}
	return flow.Continue, nil
}

// A client toggled off must disappear from Selected so mcp-wizard's View,
// which treats every key as checked, shows it unchecked.
func TestHarnessStepDropsDeselectedClients(t *testing.T) {
	st := &harnessTestState{}
	st.Harness.Detections = []harness.Harness{
		{ID: "claude-code", Name: "Claude Code", State: harness.Detected, Installed: true},
		{ID: "codex", Name: "Codex CLI", State: harness.Detected, Installed: true},
	}
	st.Harness.Selected = map[harness.ID]bool{"claude-code": true, "codex": true}
	st.Harness.Cursor = 1

	s := HarnessStep[harnessTestState](toggleStep{}, harnessTestStateFn)
	s.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, st)
	if _, present := st.Harness.Selected["codex"]; present {
		t.Fatalf("deselected client still in Selected: %v", st.Harness.Selected)
	}
	if !st.Harness.Selected["claude-code"] {
		t.Fatal("the other client must stay selected")
	}

	s.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, st)
	if !st.Harness.Selected["codex"] {
		t.Fatal("toggling again must select the client")
	}
}
