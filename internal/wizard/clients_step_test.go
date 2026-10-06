package wizard

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
)

func newClientsStep(det ClientDetector, opts ClientsStepOptions) *clientsStep[wizTestState] {
	return ClientsStep[wizTestState](context.Background(), det, wizHarness, opts).(*clientsStep[wizTestState])
}

// detected runs the step's Init and feeds the detection result back, the way
// the program would, skipping the spinner tick.
func detected(t *testing.T, s *clientsStep[wizTestState], st *wizTestState) {
	t.Helper()
	msgs := flattenCmd(s.Init(st))
	m, ok := firstOfType[clientsDetectedMsg](msgs)
	if !ok {
		t.Fatal("Init did not produce a clientsDetectedMsg")
	}
	s.Update(m, st)
}

func TestClientsStepDetectionPreselectsAndRecordsScope(t *testing.T) {
	det := &fakeDetector{detections: sampleClients()}
	s := newClientsStep(det, ClientsStepOptions{AllDetected: true})
	st := sized(120, 36)
	detected(t, s, st)

	if s.ID() != "harnesses" {
		t.Errorf("ID = %q", s.ID())
	}
	if !st.Harness.Selected["claude-code"] || !st.Harness.Selected["claude-desktop"] {
		t.Errorf("Selected = %v, want the configured and the detected client", st.Harness.Selected)
	}
	if st.Harness.Selected["cursor"] || st.Harness.Selected["windsurf"] {
		t.Errorf("Selected = %v, a client that is not installed must not be selected", st.Harness.Selected)
	}
	if st.Harness.Cursor != 0 {
		t.Errorf("Cursor = %d, want the first selectable client", st.Harness.Cursor)
	}
	if det.detectCalls != 1 || len(det.scopes) != 0 {
		t.Errorf("global scope must use Detect: calls=%d scopes=%v", det.detectCalls, det.scopes)
	}
}

func TestClientsStepWithoutAllDetectedSelectsOnlyConfigured(t *testing.T) {
	s := newClientsStep(&fakeDetector{detections: sampleClients()}, ClientsStepOptions{})
	st := sized(120, 36)
	detected(t, s, st)
	if !st.Harness.Selected["claude-code"] || st.Harness.Selected["claude-desktop"] {
		t.Errorf("Selected = %v, want only the configured client", st.Harness.Selected)
	}
}

func TestClientsStepProjectScopeUsesDetectIn(t *testing.T) {
	det := &fakeDetector{detections: sampleClients()}
	scope := harness.ProjectScopeDir(t.TempDir())
	s := newClientsStep(det, ClientsStepOptions{AllDetected: true, Scope: scope})
	st := sized(120, 36)
	detected(t, s, st)

	if len(det.scopes) != 1 || det.scopes[0].Dir != scope.Dir {
		t.Errorf("DetectIn scopes = %v, want the project scope", det.scopes)
	}
	if st.Harness.Scope.Dir != scope.Dir {
		t.Errorf("HarnessState.Scope = %v, want the project scope recorded", st.Harness.Scope)
	}
}

func TestClientsStepToggleLeavesFalseEntryAndReadersTestTheValue(t *testing.T) {
	s := newClientsStep(&fakeDetector{detections: sampleClients()}, ClientsStepOptions{AllDetected: true})
	st := sized(120, 36)
	detected(t, s, st)

	// Deselect both selected clients: space on the cursor row, then move.
	s.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, st)
	s.Update(tea.KeyMsg{Type: tea.KeyDown}, st)
	s.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, st)
	if v, present := st.Harness.Selected["claude-code"]; !present || v {
		t.Fatalf("a deselected client keeps a false entry: present=%v value=%v", present, v)
	}
	if selectedCount(&st.Harness) != 0 {
		t.Fatalf("selectedCount = %d, want 0", selectedCount(&st.Harness))
	}

	d, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if d != flow.Continue {
		t.Fatalf("enter with nothing selected gave %v, want Continue", d)
	}
	out := ansi.Strip(s.View(st))
	if !strings.Contains(out, "Select at least one client, or press q to cancel.") {
		t.Errorf("the nothing-selected note is missing:\n%s", out)
	}
	// The note goes away with the next key.
	s.Update(tea.KeyMsg{Type: tea.KeyDown}, st)
	if strings.Contains(ansi.Strip(s.View(st)), "Select at least one client") {
		t.Error("the note must clear on the next key")
	}

	// Select one again: enter continues.
	s.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, st)
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st); d != flow.Next {
		t.Errorf("enter with a client selected gave %v, want Next", d)
	}
}

func TestClientsStepKeys(t *testing.T) {
	s := newClientsStep(&fakeDetector{detections: sampleClients()}, ClientsStepOptions{AllDetected: true})
	st := sized(120, 36)

	// Before detection: q cancels, esc goes back, others are ignored.
	s.Init(st)
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, st); d != flow.Continue {
		t.Errorf("a before detection gave %v", d)
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyEsc}, st); d != flow.Back {
		t.Errorf("esc before detection gave %v, want Back", d)
	}
	detected(t, s, st)

	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyEsc}, st); d != flow.Back {
		t.Errorf("esc gave %v, want Back", d)
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Quit {
		t.Errorf("q gave %v, want Quit", d)
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyCtrlC}, st); d != flow.Quit {
		t.Errorf("ctrl+c gave %v, want Quit", d)
	}

	// a clears the selection when everything selectable is selected.
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, st)
	if selectedCount(&st.Harness) != 0 {
		t.Errorf("a with everything selected should clear: %v", st.Harness.Selected)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, st)
	if selectedCount(&st.Harness) != 2 {
		t.Errorf("a should select every selectable client: %v", st.Harness.Selected)
	}
}

func TestClientsStepShowAllAndHiddenLine(t *testing.T) {
	s := newClientsStep(&fakeDetector{detections: sampleClients()}, ClientsStepOptions{AllDetected: true})
	st := sized(120, 36)
	detected(t, s, st)

	out := ansi.Strip(s.View(st))
	if !strings.Contains(out, "v show 2 clients that are not installed") {
		t.Errorf("hidden-clients line missing:\n%s", out)
	}
	if strings.Contains(out, "Cursor") || strings.Contains(out, "Windsurf") {
		t.Errorf("hidden clients must not be listed:\n%s", out)
	}
	if got := footerOf(s.View(st)); !strings.Contains(got, "v show all") {
		t.Errorf("footer = %q, want v show all", got)
	}

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")}, st)
	out = ansi.Strip(s.View(st))
	if strings.Contains(out, "that are not installed") {
		t.Errorf("the hidden line must go away while everything is shown:\n%s", out)
	}
	if !strings.Contains(out, "Windsurf") || !strings.Contains(out, "not installed") {
		t.Errorf("every client should be listed with its status:\n%s", out)
	}
	if got := footerOf(s.View(st)); !strings.Contains(got, "v hide") {
		t.Errorf("footer = %q, want v hide", got)
	}

	// Hiding again moves a cursor that sat on a hidden row.
	s.Update(tea.KeyMsg{Type: tea.KeyUp}, st) // wraps to the last (hidden) row
	if st.Harness.Cursor != 3 {
		t.Fatalf("Cursor = %d, want the last row", st.Harness.Cursor)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")}, st)
	if st.Harness.Cursor != 0 {
		t.Errorf("Cursor = %d after hiding, want the first visible row", st.Harness.Cursor)
	}
}

func TestClientsStepNoHiddenLineWhenNothingIsHidden(t *testing.T) {
	det := &fakeDetector{detections: sampleClients()[:2]}
	s := newClientsStep(det, ClientsStepOptions{AllDetected: true})
	st := sized(120, 36)
	detected(t, s, st)
	lines := strings.Split(ansi.Strip(s.View(st)), "\n")
	out := strings.Join(lines[:len(lines)-1], "\n") // the footer keeps its "v show all" key
	if strings.Contains(out, "v show") || strings.Contains(out, "No AI clients") {
		t.Errorf("no hidden-clients line and no stock text expected:\n%s", out)
	}
}

func TestClientsStepFrame(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		w, h := size[0], size[1]
		s := newClientsStep(&fakeDetector{detections: sampleClients()}, ClientsStepOptions{AllDetected: true})
		st := sized(w, h)

		s.Init(st)
		loading := s.View(st)
		assertFrame(t, loading, w, h)
		if !strings.Contains(ansi.Strip(loading), "Looking for AI clients...") {
			t.Errorf("%dx%d: loading line missing", w, h)
		}

		detected(t, s, st)
		out := s.View(st)
		lines := assertFrame(t, out, w, h)
		if head := ansi.Strip(lines[0]); !strings.Contains(head, "AI clients") || !strings.Contains(head, "step 3 of 4") {
			t.Errorf("%dx%d: header = %q", w, h, head)
		}
		plain := ansi.Strip(out)
		for _, want := range []string{"Claude Code", "configured", "Claude Desktop", "detected"} {
			if !strings.Contains(plain, want) {
				t.Errorf("%dx%d: screen missing %q:\n%s", w, h, want, plain)
			}
		}
		footer := footerOf(out)
		if !strings.Contains(footer, "enter continue") || !strings.Contains(footer, "q cancel") {
			t.Errorf("%dx%d: footer = %q", w, h, footer)
		}
	}
}

func TestClientsStepEveryFooterKeyDoesSomething(t *testing.T) {
	s := newClientsStep(&fakeDetector{detections: sampleClients()}, ClientsStepOptions{AllDetected: true})
	st := sized(120, 36)
	detected(t, s, st)
	for _, h := range s.Hints(st) {
		switch h.Key {
		case "↑↓", "space", "a", "v", "enter", "esc", "q":
		default:
			t.Errorf("footer lists key %q that Update does not handle", h.Key)
		}
	}
}

func TestClientsStepTooSmallAndNoSize(t *testing.T) {
	s := newClientsStep(&fakeDetector{detections: sampleClients()}, ClientsStepOptions{})
	st := &wizTestState{}
	detected(t, s, st)
	if out := s.View(st); out != "" {
		t.Errorf("View before the first size message = %q, want empty", out)
	}
	st.Width, st.Height = 50, 10
	out := s.View(st)
	if !strings.Contains(out, "Terminal is 50x10.") || !strings.Contains(out, "at least 60x16") {
		t.Errorf("too-small message = %q", out)
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Quit {
		t.Errorf("q must keep working when the terminal is too small, got %v", d)
	}
}
