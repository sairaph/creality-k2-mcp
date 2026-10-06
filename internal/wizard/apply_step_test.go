package wizard

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func newApplyStep(det ClientDetector, opts ApplyStepOptions) *applyStep[wizTestState] {
	return ApplyStep[wizTestState](context.Background(), det, wizHarness, wizResults, wizPrinters, wizSettings, opts).(*applyStep[wizTestState])
}

// appliedState is a wizard state at the end of the flow: one enabled printer,
// the control preset, the sample clients with two selected and one false entry.
func appliedState(w, h int) *wizTestState {
	st := sized(w, h)
	p := domain.NewPrinter("K2-5885", "192.168.1.10")
	p.Enabled, p.AllowControl = true, true
	st.Printers.Rows = []PrinterRow{{Printer: p, Addable: true, Existing: true}}
	st.Settings.Ready = true
	cfg := domain.DefaultSettings()
	cfg.Tools.Preset = domain.PresetControl
	st.Settings.Form.Load(cfg)
	st.Harness.Detections = sampleClients()
	st.Harness.Selected = map[harness.ID]bool{"claude-code": true, "claude-desktop": true, "cursor": false}
	return st
}

// finishApply runs Init and feeds the result back like the program would.
func finishApply(t *testing.T, s *applyStep[wizTestState], st *wizTestState) {
	t.Helper()
	msgs := flattenCmd(s.Init(st))
	m, ok := firstOfType[appliedMsg](msgs)
	if !ok {
		t.Fatal("Init did not produce an appliedMsg")
	}
	s.Update(m, st)
}

func realResults() []harness.Result {
	return []harness.Result{
		{HarnessID: "claude-code", Name: "Claude Code", State: harness.ApplyNoop},
		{HarnessID: "claude-desktop", Name: "Claude Desktop", State: harness.Applied},
	}
}

func TestApplyStepRealRunAppliesOnlyTrueEntries(t *testing.T) {
	det := &fakeDetector{results: realResults()}
	s := newApplyStep(det, ApplyStepOptions{})
	st := appliedState(120, 36)
	st.Results.Results = []harness.Result{{Name: "stale"}}
	st.Results.Done = true

	cmd := s.Init(st)
	if st.Results.Done || st.Results.Results != nil || st.Results.Changes != nil {
		t.Fatalf("Init must clear the results: %+v", st.Results)
	}
	msgs := flattenCmd(cmd)
	m, _ := firstOfType[appliedMsg](msgs)
	if det.applyCalls != 1 || det.planCalls != 0 {
		t.Fatalf("real run: apply=%d plan=%d", det.applyCalls, det.planCalls)
	}
	if got := strings.Join(idStrings(det.applyIDs), ","); got != "claude-code,claude-desktop" {
		t.Errorf("applied ids = %q, want the true entries only, in detection order", got)
	}

	if d, _ := s.Update(m, st); d != flow.Continue {
		t.Fatalf("applied gave %v, want Continue", d)
	}
	if !st.Results.Done || len(st.Results.Results) != 2 {
		t.Errorf("results not stored: %+v", st.Results)
	}
	if !st.Settled || st.Failure != nil {
		t.Errorf("Settled=%v Failure=%v, want settled without failure", st.Settled, st.Failure)
	}
}

func idStrings(ids []harness.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func TestApplyStepDryRunPlansAndNeverApplies(t *testing.T) {
	det := &fakeDetector{changes: []harness.Change{{HarnessID: "claude-desktop", Name: "Claude Desktop", State: harness.Applied, Action: "add"}}}
	s := newApplyStep(det, ApplyStepOptions{DryRun: true})
	st := appliedState(120, 36)
	finishApply(t, s, st)

	if det.planCalls != 1 || det.applyCalls != 0 {
		t.Fatalf("dry run: plan=%d apply=%d", det.planCalls, det.applyCalls)
	}
	if !st.Results.Done || len(st.Results.Changes) != 1 || len(st.Results.Results) != 0 {
		t.Errorf("changes not stored: %+v", st.Results)
	}
}

func TestApplyStepPlanErrorFails(t *testing.T) {
	boom := errors.New("plan exploded")
	s := newApplyStep(&fakeDetector{planErr: boom}, ApplyStepOptions{DryRun: true})
	st := appliedState(120, 36)
	msgs := flattenCmd(s.Init(st))
	m, _ := firstOfType[appliedMsg](msgs)
	if d, _ := s.Update(m, st); d != flow.Fail {
		t.Fatalf("a plan error gave %v, want Fail", d)
	}
	if !errors.Is(st.Failure, boom) {
		t.Errorf("Failure = %v, want the plan error", st.Failure)
	}
}

func TestApplyStepFailedClientSetsFailure(t *testing.T) {
	det := &fakeDetector{results: []harness.Result{
		{HarnessID: "claude-code", Name: "Claude Code", State: harness.Applied},
		{HarnessID: "claude-desktop", Name: "Claude Desktop", State: harness.ApplyFailed, Reason: "config is read-only"},
	}}
	s := newApplyStep(det, ApplyStepOptions{})
	st := appliedState(120, 36)
	finishApply(t, s, st)

	if st.Failure == nil || !strings.Contains(st.Failure.Error(), "Claude Desktop") || !strings.Contains(st.Failure.Error(), "config is read-only") {
		t.Fatalf("Failure = %v, want the failed client named with its reason", st.Failure)
	}
	if !st.Settled {
		t.Error("the flow is settled once the writes ran, even when one failed")
	}
	out := ansi.Strip(s.View(st))
	if !strings.Contains(out, "Registration failed.") || !strings.Contains(out, "failed: config is read-only") {
		t.Errorf("the failure should show on the screen:\n%s", out)
	}
}

func TestApplyStepNothingSelectedFinishes(t *testing.T) {
	s := newApplyStep(&fakeDetector{}, ApplyStepOptions{})
	st := appliedState(120, 36)
	st.Harness.Selected = map[harness.ID]bool{}
	finishApply(t, s, st)
	if !st.Results.Done || !st.Settled {
		t.Fatalf("Done=%v Settled=%v, want an immediate empty finish", st.Results.Done, st.Settled)
	}
	out := ansi.Strip(s.View(st))
	if !strings.Contains(out, "none selected") {
		t.Errorf("AI clients row should say none selected:\n%s", out)
	}
}

func TestApplyStepKeys(t *testing.T) {
	key := func(r string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(r)} }

	// A real write in flight: only ctrl+c does anything.
	s := newApplyStep(&fakeDetector{results: realResults()}, ApplyStepOptions{})
	st := appliedState(120, 36)
	s.Init(st)
	for _, k := range []tea.KeyMsg{key("q"), {Type: tea.KeyEsc}, {Type: tea.KeyEnter}} {
		if d, _ := s.Update(k, st); d != flow.Continue {
			t.Errorf("%q while writing gave %v, want Continue", k.String(), d)
		}
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyCtrlC}, st); d != flow.Quit {
		t.Errorf("ctrl+c while writing gave %v, want Quit", d)
	}

	// A dry run being planned can be cancelled with q.
	dry := newApplyStep(&fakeDetector{}, ApplyStepOptions{DryRun: true})
	st = appliedState(120, 36)
	dry.Init(st)
	if d, _ := dry.Update(key("q"), st); d != flow.Quit {
		t.Errorf("q while planning gave %v, want Quit", d)
	}

	// Done: enter, q and esc all finish.
	s = newApplyStep(&fakeDetector{results: realResults()}, ApplyStepOptions{})
	st = appliedState(120, 36)
	finishApply(t, s, st)
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEnter}, key("q"), {Type: tea.KeyEsc}} {
		if d, _ := s.Update(k, st); d != flow.Next {
			t.Errorf("%q once done gave %v, want Next", k.String(), d)
		}
	}
}

func TestApplyStepSpinnerRunsWhileWriting(t *testing.T) {
	s := newApplyStep(&fakeDetector{results: realResults()}, ApplyStepOptions{})
	st := appliedState(120, 36)
	s.Init(st)
	before := st.Spinner.Frame
	if d, cmd := s.Update(spinTick(t), st); d != flow.Continue || cmd == nil {
		t.Fatalf("a tick while writing should reschedule: %v %v", d, cmd)
	}
	if st.Spinner.Frame != before+1 {
		t.Error("the tick should advance the spinner frame")
	}
	out := ansi.Strip(s.View(st))
	if !strings.Contains(out, "Registering with the selected clients...") {
		t.Errorf("writing line missing:\n%s", out)
	}
	if got := footerOf(s.View(st)); got != "registering... · ctrl+c quit" {
		t.Errorf("footer while writing = %q", got)
	}
}

// spinTick produces the spinner tick message the library's Spinner command
// sends, so a test can feed it without waiting.
func spinTick(t *testing.T) tea.Msg {
	t.Helper()
	msg := tui.Spinner()()
	if msg == nil {
		t.Fatal("no spin message")
	}
	return msg
}

func TestApplyStepFinishScreen(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		w, h := size[0], size[1]
		s := newApplyStep(&fakeDetector{results: realResults()}, ApplyStepOptions{})
		st := appliedState(w, h)
		finishApply(t, s, st)

		out := s.View(st)
		lines := assertFrame(t, out, w, h)
		if head := ansi.Strip(lines[0]); !strings.Contains(head, "step 4 of 4") {
			t.Errorf("%dx%d: header = %q", w, h, head)
		}
		plain := ansi.Strip(out)
		for _, want := range []string{
			"Setup complete.",
			"Printers", "1 enabled (K2-5885, control on)",
			"Tool preset", "control",
			"AI clients", "2 registered",
			"Claude Code", "already registered",
			"Claude Desktop", "registered",
			"Claude Desktop: restart Claude Desktop so it picks up the server",
			"Next: run `creality-k2-mcp` to open the app",
			"Change any of this later with `creality-k2-mcp install`.",
		} {
			if !strings.Contains(plain, want) {
				t.Errorf("%dx%d: finish screen missing %q:\n%s", w, h, want, plain)
			}
		}
		if got := footerOf(out); got != "enter finish" {
			t.Errorf("%dx%d: footer = %q, want enter finish", w, h, got)
		}
		if strings.Contains(plain, "\\") || strings.Contains(plain, ":/") {
			t.Errorf("%dx%d: the global finish screen must show no path:\n%s", w, h, plain)
		}
	}
}

func TestApplyStepDryRunFinishScreen(t *testing.T) {
	det := &fakeDetector{changes: []harness.Change{
		{HarnessID: "claude-code", Name: "Claude Code", State: harness.ApplyNoop},
		{HarnessID: "claude-desktop", Name: "Claude Desktop", State: harness.Applied, Action: "add"},
	}}
	s := newApplyStep(det, ApplyStepOptions{DryRun: true})
	s.chrome = newChrome("", true)
	st := appliedState(120, 36)
	finishApply(t, s, st)

	out := s.View(st)
	lines := assertFrame(t, out, 120, 36)
	if head := ansi.Strip(lines[0]); !strings.Contains(head, "dry run") {
		t.Errorf("header = %q, want the dry run marker", head)
	}
	plain := ansi.Strip(out)
	for _, want := range []string{"Dry run: nothing was written.", "2 would be registered", "already registered", "would add"} {
		if !strings.Contains(plain, want) {
			t.Errorf("dry-run finish screen missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "restart Claude Desktop") || strings.Contains(plain, "Next:") {
		t.Errorf("a dry run has no reload lines and no next steps:\n%s", plain)
	}
}

func TestApplyStepProjectScopeShowsDirOnceAndNoClientPaths(t *testing.T) {
	dir := t.TempDir()
	scope := harness.ProjectScopeDir(dir)
	det := &fakeDetector{results: []harness.Result{
		{HarnessID: "claude-code", Name: "Claude Code", State: harness.Applied, Path: dir + "/.mcp.json"},
	}}
	s := newApplyStep(det, ApplyStepOptions{Scope: scope, App: "creality-k2-mcp project setup"})
	st := appliedState(120, 36)
	finishApply(t, s, st)

	plain := ansi.Strip(s.View(st))
	if strings.Contains(plain, ".mcp.json") {
		t.Errorf("no client path may be shown:\n%s", plain)
	}
	n := 0
	for _, l := range strings.Split(plain, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "Project ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the project row appears %d times, want once:\n%s", n, plain)
	}
	if !strings.Contains(plain, "creality-k2-mcp project setup") {
		t.Errorf("header should name the project setup:\n%s", plain)
	}
	if !strings.Contains(plain, "`creality-k2-mcp add`") {
		t.Errorf("project scope repeats with add:\n%s", plain)
	}
}

func TestApplyStepHintsAreTheFooter(t *testing.T) {
	s := newApplyStep(&fakeDetector{results: realResults()}, ApplyStepOptions{})
	st := appliedState(120, 36)
	finishApply(t, s, st)
	hints := s.Hints(st)
	if len(hints) != 1 || hints[0].Key != "enter" || hints[0].Label != "finish" {
		t.Errorf("Hints = %v", hints)
	}
}
