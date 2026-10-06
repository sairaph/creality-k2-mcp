package wizard

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// ApplyStepOptions controls the registration step.
type ApplyStepOptions struct {
	// Scope selects where registration happens; use the scope of the AI
	// clients step.
	Scope harness.Scope
	// DryRun computes the plan and shows it instead of writing anything.
	DryRun bool
	// ConflictPolicy decides what happens to a same-name entry that differs.
	// The zero value is harness.ConflictReplace.
	ConflictPolicy harness.ConflictPolicy
	// App is the header's first part; empty means "creality-k2-mcp setup".
	App string
}

// ApplyStep returns the last step: it registers the server with the clients
// selected in the AI clients step (or, in a dry run, computes the plan), then
// shows the finish screen (dev_docs/tui-design-v0.4.0.md 3.4, 3.5, R2.6). It
// reproduces installer.ApplyStep's contract: Init clears the results, Update
// marks them Done, sets BaseState.Settled and records a Failure for any failed
// client. The printers and settings states only feed the finish summary.
func ApplyStep[T any](ctx context.Context, detector ClientDetector, harnessFn func(*T) *installer.HarnessState, resultsFn func(*T) *installer.ResultsState, printersFn func(*T) *PrinterState, settingsFn func(*T) *SettingsState, opts ApplyStepOptions) flow.Step[T] {
	if detector == nil {
		panic("wizard: ApplyStep requires a non-nil detector")
	}
	if opts.ConflictPolicy == "" {
		opts.ConflictPolicy = harness.ConflictReplace
	}
	return &applyStep[T]{
		ctx: ctx, detector: detector, opts: opts, chrome: newChrome(opts.App, opts.DryRun),
		harnessFn: harnessFn, resultsFn: resultsFn, printersFn: printersFn, settingsFn: settingsFn,
	}
}

type applyStep[T any] struct {
	ctx        context.Context
	detector   ClientDetector
	opts       ApplyStepOptions
	chrome     chrome
	harnessFn  func(*T) *installer.HarnessState
	resultsFn  func(*T) *installer.ResultsState
	printersFn func(*T) *PrinterState
	settingsFn func(*T) *SettingsState
	tick       ticker
}

type appliedMsg struct {
	results []harness.Result
	changes []harness.Change
	err     error
}

func (s *applyStep[T]) ID() string { return "apply" }

func (s *applyStep[T]) Title(state *T) string {
	if s.opts.DryRun {
		return "Planned changes - nothing has been written"
	}
	return "Registration"
}

func (s *applyStep[T]) Hints(state *T) []struct{ Key, Label string } {
	return legacyHints(s.hints(s.results(state)))
}

// hints is the one key list both Hints and the footer use. While a real write
// is in flight only ctrl+c works (q would report a clean exit for a
// registration that may still complete); a dry run can be cancelled.
func (s *applyStep[T]) hints(rs *installer.ResultsState) []frame.Hint {
	switch {
	case rs != nil && rs.Done:
		return []frame.Hint{{Keys: "enter", Label: "finish", Priority: 99}}
	case s.opts.DryRun:
		return []frame.Hint{{Label: "planning...", Priority: 99}, frame.Cancel()}
	}
	return busyHints("registering...")
}

func (s *applyStep[T]) results(state *T) *installer.ResultsState {
	if s.resultsFn == nil {
		return nil
	}
	return s.resultsFn(state)
}

// selectedIDs returns the clients chosen in the AI clients step, in detection
// order. A client counts only when its Selected entry is true.
func (s *applyStep[T]) selectedIDs(state *T) []harness.ID {
	if s.harnessFn == nil {
		return nil
	}
	hs := s.harnessFn(state)
	if hs == nil {
		return nil
	}
	var ids []harness.ID
	for _, h := range hs.Detections {
		if hs.Selected[h.ID] {
			ids = append(ids, h.ID)
		}
	}
	return ids
}

func (s *applyStep[T]) Init(state *T) tea.Cmd {
	rs := s.results(state)
	if rs == nil {
		return nil
	}
	rs.Results = nil
	rs.Changes = nil
	rs.Done = false

	ids := s.selectedIDs(state)
	if len(ids) == 0 {
		return func() tea.Msg { return appliedMsg{} }
	}
	scope, policy, dryRun := s.opts.Scope, s.opts.ConflictPolicy, s.opts.DryRun
	s.tick.reset()
	return tea.Batch(s.tick.start(), func() tea.Msg {
		if dryRun {
			changes, err := s.detector.PlanResultsIn(s.ctx, scope, ids, harness.Present, policy)
			return appliedMsg{changes: changes, err: err}
		}
		return appliedMsg{results: s.detector.ApplyIn(s.ctx, scope, ids, harness.Present, policy)}
	})
}

func (s *applyStep[T]) Update(msg tea.Msg, state *T) (flow.Directive, tea.Cmd) {
	rs := s.results(state)
	if rs == nil {
		return flow.Fail, nil
	}

	switch m := msg.(type) {
	case appliedMsg:
		rs.Done = true
		rs.Results = m.results
		rs.Changes = m.changes
		if base := baseStateOf(state); base != nil {
			if m.err != nil {
				base.Failure = m.err
				return flow.Fail, nil
			}
			// The work is complete; cancelling from here on is a normal exit.
			base.Settled = true
			for _, r := range m.results {
				if r.State == harness.ApplyFailed {
					base.Failure = fmt.Errorf("registration failed for %s: %s", r.Name, r.Reason)
					break
				}
			}
		}
		return flow.Continue, nil

	case tea.KeyMsg:
		if smallDrops(baseStateOf(state), m.String(), false) {
			return flow.Continue, nil
		}
		switch m.String() {
		case "ctrl+c":
			return flow.Quit, nil
		case "enter", "q", "esc":
			if rs.Done {
				return flow.Next, nil
			}
			if m.String() == "q" && s.opts.DryRun {
				return flow.Quit, nil
			}
		}
		return flow.Continue, nil
	}

	if tui.IsSpinMsg(msg) {
		if base := baseStateOf(state); base != nil {
			base.Spinner.Frame++
		}
		return flow.Continue, s.tick.next(!rs.Done)
	}
	return flow.Continue, nil
}

func (s *applyStep[T]) View(state *T) string {
	rs := s.results(state)
	if rs == nil {
		return ""
	}
	base := baseStateOf(state)
	return s.chrome.draw(base, "Registration", 4, s.hints(rs), func(w, rows int) []string {
		if !rs.Done {
			verb := "Registering with"
			if s.opts.DryRun {
				verb = "Planning changes for"
			}
			return []string{frame.Gutter + frame.Spinner(spinnerFrame(base)) + " " + verb + " the selected clients..."}
		}
		var failure error
		if base != nil {
			failure = base.Failure
		}
		in := NewFinishInput(s.opts.Scope, s.opts.DryRun, failure, s.printers(state), s.settings(state), s.clients(state), rs)
		return FinishLines(w, in)
	})
}

func (s *applyStep[T]) printers(state *T) *PrinterState {
	if s.printersFn == nil {
		return nil
	}
	return s.printersFn(state)
}

func (s *applyStep[T]) settings(state *T) *SettingsState {
	if s.settingsFn == nil {
		return nil
	}
	return s.settingsFn(state)
}

func (s *applyStep[T]) clients(state *T) *installer.HarnessState {
	if s.harnessFn == nil {
		return nil
	}
	return s.harnessFn(state)
}
