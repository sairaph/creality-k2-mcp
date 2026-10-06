package wizard

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
)

// wizTestState is the four-step state main.go's AppState has, so the steps can
// be driven together.
type wizTestState struct {
	flow.BaseState
	Printers PrinterState
	Settings SettingsState
	Harness  installer.HarnessState
	Results  installer.ResultsState
}

func wizPrinters(s *wizTestState) *PrinterState          { return &s.Printers }
func wizSettings(s *wizTestState) *SettingsState         { return &s.Settings }
func wizHarness(s *wizTestState) *installer.HarnessState { return &s.Harness }
func wizResults(s *wizTestState) *installer.ResultsState { return &s.Results }

// sized is a wizard state with a terminal of w x h already reported.
func sized(w, h int) *wizTestState {
	st := &wizTestState{}
	st.Width, st.Height = w, h
	return st
}

// fakeDetector is a ClientDetector that touches nothing: it returns canned
// detections, plans and results and records what it was asked.
type fakeDetector struct {
	detections []harness.Harness
	changes    []harness.Change
	planErr    error
	results    []harness.Result

	detectCalls int
	scopes      []harness.Scope
	planIDs     []harness.ID
	applyIDs    []harness.ID
	planCalls   int
	applyCalls  int
}

func (f *fakeDetector) Detect(ctx context.Context) []harness.Harness {
	f.detectCalls++
	return f.detections
}

func (f *fakeDetector) DetectIn(ctx context.Context, scope harness.Scope) []harness.Harness {
	f.detectCalls++
	f.scopes = append(f.scopes, scope)
	return f.detections
}

func (f *fakeDetector) PlanResultsIn(ctx context.Context, scope harness.Scope, ids []harness.ID, desired harness.DesiredState, policy harness.ConflictPolicy) ([]harness.Change, error) {
	f.planCalls++
	f.planIDs = ids
	return f.changes, f.planErr
}

func (f *fakeDetector) ApplyIn(ctx context.Context, scope harness.Scope, ids []harness.ID, desired harness.DesiredState, policy harness.ConflictPolicy) []harness.Result {
	f.applyCalls++
	f.applyIDs = ids
	return f.results
}

// sampleClients is a detection list: two installed (one already configured),
// one config-present-only is not relevant, two not installed.
func sampleClients() []harness.Harness {
	return []harness.Harness{
		{ID: "claude-code", Name: "Claude Code", State: harness.Detected, Installed: true, Configured: true, ReloadHint: "restart Claude Code"},
		{ID: "claude-desktop", Name: "Claude Desktop", State: harness.Detected, Installed: true, ReloadHint: "restart Claude Desktop so it picks up the server"},
		{ID: "cursor", Name: "Cursor", State: harness.NotDetected},
		{ID: "windsurf", Name: "Windsurf", State: harness.NotDetected},
	}
}

// assertFrame checks the frame contract on a drawn screen: exactly h rows, the
// header on row 1, a blank row 2, the footer on the last row, and no row wider
// than w-1 display columns.
func assertFrame(t *testing.T, out string, w, h int) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) != h {
		t.Fatalf("screen has %d rows, want %d:\n%s", len(lines), h, out)
	}
	if !strings.Contains(ansi.Strip(lines[0]), "creality-k2-mcp") {
		t.Errorf("row 1 is not the header: %q", lines[0])
	}
	if strings.TrimSpace(ansi.Strip(lines[1])) != "" {
		t.Errorf("row 2 is not blank: %q", lines[1])
	}
	if strings.TrimSpace(ansi.Strip(lines[h-1])) == "" {
		t.Errorf("the last row (footer) is blank:\n%s", out)
	}
	for i, line := range lines {
		if got := ansi.StringWidth(line); got > w-1 {
			t.Errorf("row %d is %d columns wide, limit %d: %q", i+1, got, w-1, ansi.Strip(line))
		}
	}
	return lines
}

// footerOf is the footer row of a drawn screen without styling.
func footerOf(out string) string {
	lines := strings.Split(out, "\n")
	return strings.TrimSpace(ansi.Strip(lines[len(lines)-1]))
}
