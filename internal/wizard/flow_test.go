package wizard

import (
	"context"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// pump runs the commands the model returns the way the program's event loop
// does (batches expanded, results fed back through Update), dropping spinner
// ticks so it terminates.
func pump(m tea.Model, cmd tea.Cmd) (done bool) {
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch msg := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tea.QuitMsg:
			done = true
		default:
			if tui.IsSpinMsg(msg) {
				continue
			}
			var next tea.Cmd
			m, next = m.Update(msg)
			queue = append(queue, next)
		}
	}
	return done
}

func press(m tea.Model, key tea.KeyMsg) bool {
	_, cmd := m.Update(key)
	return pump(m, cmd)
}

// The four steps driven together through the flow, with fakes for discovery
// and for the AI clients and a temp home: enter on every step reaches the
// finish screen, and the registry and settings files are written.
func TestWizardEndToEnd(t *testing.T) {
	home := isolateHome(t)

	det := &fakeDetector{detections: sampleClients(), results: realResults()}
	discover := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{Results: []discovery.Result{k2Result("192.168.1.10", "K2-5885", "K2")}}, nil
	}
	state := &wizTestState{}
	steps := []flow.Step[wizTestState]{
		PrintersStep(context.Background(), wizPrinters, PrintersStepOptions{Discover: discover}),
		SettingsStep(wizSettings, SettingsStepOptions{}),
		ClientsStep(context.Background(), det, wizHarness, ClientsStepOptions{AllDetected: true}),
		ApplyStep(context.Background(), det, wizHarness, wizResults, wizPrinters, wizSettings, ApplyStepOptions{}),
	}
	f := flow.New(steps, state)
	m := f.Model()

	pump(m, m.Init())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if state.Width != 100 || state.Height != 30 {
		t.Fatalf("the flow should record the size: %dx%d", state.Width, state.Height)
	}
	if !state.Printers.Ready || state.Printers.Scanning || len(state.Printers.Rows) != 1 {
		t.Fatalf("step 1 did not finish loading and scanning: %+v", state.Printers)
	}
	enter := tea.KeyMsg{Type: tea.KeyEnter}

	// Step 1 -> 2 -> 3 -> 4.
	for i, want := range []string{"settings", "harnesses", "apply"} {
		if press(m, enter) {
			t.Fatalf("the program quit early at step %d", i+1)
		}
		if got := steps[f.Current()].ID(); got != want {
			t.Fatalf("after enter %d on step: current = %q, want %q", i+1, got, want)
		}
		plain := ansi.Strip(m.View())
		wantStep := []string{"step 2 of 4", "step 3 of 4", "step 4 of 4"}[i]
		if !strings.Contains(plain, wantStep) {
			t.Errorf("header should say %q:\n%s", wantStep, plain)
		}
	}

	if !state.Results.Done || !state.Settled || state.Failure != nil {
		t.Fatalf("registration should be done: %+v settled=%v failure=%v", state.Results, state.Settled, state.Failure)
	}
	if det.applyCalls != 1 {
		t.Errorf("ApplyIn calls = %d, want 1", det.applyCalls)
	}
	finish := ansi.Strip(m.View())
	for _, want := range []string{"Setup complete.", "1 enabled (K2-5885, control off)", "2 registered", "Next: run"} {
		if !strings.Contains(finish, want) {
			t.Errorf("finish screen missing %q:\n%s", want, finish)
		}
	}
	assertFrame(t, m.View(), 100, 30)

	if !state.Printers.Wrote || !state.Settings.Wrote {
		t.Errorf("the saves should be recorded: printers=%v settings=%v", state.Printers.Wrote, state.Settings.Wrote)
	}
	for _, path := range []string{state.Printers.Path, state.Settings.Path} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to exist: %v", path, err)
		}
		if !strings.HasPrefix(path, home) {
			t.Errorf("%s is outside the temp home %s", path, home)
		}
	}
	reg, _, _, err := domain.LoadRegistry("")
	if err != nil || len(reg.Printers) != 1 || !reg.Printers[0].Enabled {
		t.Fatalf("registry = %+v err=%v", reg, err)
	}

	// Enter on the finish screen ends the program with exit code 0.
	if !press(m, enter) {
		t.Error("enter on the finish screen should end the program")
	}
	if f.ExitCode() != 0 {
		t.Errorf("exit code = %d, want 0", f.ExitCode())
	}
}

// A dry run walks the same steps and writes nothing at all.
func TestWizardDryRunWritesNothing(t *testing.T) {
	home := isolateHome(t)

	det := &fakeDetector{detections: sampleClients(), changes: []harness.Change{
		{HarnessID: "claude-desktop", Name: "Claude Desktop", State: harness.Applied, Action: "add"},
	}}
	discover := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{Results: []discovery.Result{k2Result("192.168.1.10", "K2-5885", "K2")}}, nil
	}
	state := &wizTestState{}
	steps := []flow.Step[wizTestState]{
		PrintersStep(context.Background(), wizPrinters, PrintersStepOptions{Discover: discover, DryRun: true}),
		SettingsStep(wizSettings, SettingsStepOptions{DryRun: true}),
		ClientsStep(context.Background(), det, wizHarness, ClientsStepOptions{AllDetected: true, DryRun: true}),
		ApplyStep(context.Background(), det, wizHarness, wizResults, wizPrinters, wizSettings, ApplyStepOptions{DryRun: true}),
	}
	f := flow.New(steps, state)
	m := f.Model()
	pump(m, m.Init())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if !strings.Contains(ansi.Strip(m.View()), "dry run") {
		t.Errorf("the header should carry the dry run marker:\n%s", ansi.Strip(m.View()))
	}
	for range 3 {
		press(m, tea.KeyMsg{Type: tea.KeyEnter})
	}
	if det.applyCalls != 0 || det.planCalls != 1 {
		t.Errorf("dry run: apply=%d plan=%d", det.applyCalls, det.planCalls)
	}
	if !state.Results.Done || state.Printers.Wrote || state.Settings.Wrote {
		t.Errorf("done=%v printersWrote=%v settingsWrote=%v", state.Results.Done, state.Printers.Wrote, state.Settings.Wrote)
	}
	if !strings.Contains(ansi.Strip(m.View()), "Dry run: nothing was written.") {
		t.Errorf("finish screen:\n%s", ansi.Strip(m.View()))
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a dry run must not touch the filesystem, found %v", entries)
	}
}

// Cancelling on step 3 after step 1 saved leaves the saves in place and the
// outcome line says so.
func TestWizardCancelAfterSavesKeepsThemAndSaysSo(t *testing.T) {
	isolateHome(t)

	det := &fakeDetector{detections: sampleClients()}
	discover := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{Results: []discovery.Result{k2Result("192.168.1.10", "K2-5885", "K2")}}, nil
	}
	state := &wizTestState{}
	steps := []flow.Step[wizTestState]{
		PrintersStep(context.Background(), wizPrinters, PrintersStepOptions{Discover: discover}),
		SettingsStep(wizSettings, SettingsStepOptions{}),
		ClientsStep(context.Background(), det, wizHarness, ClientsStepOptions{AllDetected: true}),
		ApplyStep(context.Background(), det, wizHarness, wizResults, wizPrinters, wizSettings, ApplyStepOptions{}),
	}
	f := flow.New(steps, state)
	m := f.Model()
	pump(m, m.Init())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := steps[f.Current()].ID(); got != "harnesses" {
		t.Fatalf("current = %q, want the AI clients step", got)
	}
	if !press(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}) {
		t.Fatal("q should cancel")
	}
	if det.applyCalls != 0 {
		t.Error("no AI client may be touched after a cancel before registration")
	}

	var out strings.Builder
	PrintOutcome(&out, Outcome{Scope: harness.Scope{}, Printers: &state.Printers, Settings: &state.Settings, Clients: &state.Harness, Results: &state.Results,
		OnApplyStep: steps[f.Current()].ID() == "apply"})
	if !strings.Contains(out.String(), "The printers and settings you saved were kept; no AI client was changed.") {
		t.Errorf("outcome = %q", out.String())
	}
}
