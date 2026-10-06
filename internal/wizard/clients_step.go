package wizard

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/userhome"
)

// ClientDetector is the part of *harness.Detector the AI clients step and the
// registration step use. Tests pass a fake, so no test reads or writes a real
// AI client configuration.
type ClientDetector interface {
	Detect(ctx context.Context) []harness.Harness
	DetectIn(ctx context.Context, scope harness.Scope) []harness.Harness
	PlanResultsIn(ctx context.Context, scope harness.Scope, ids []harness.ID, desired harness.DesiredState, policy harness.ConflictPolicy) ([]harness.Change, error)
	ApplyIn(ctx context.Context, scope harness.Scope, ids []harness.ID, desired harness.DesiredState, policy harness.ConflictPolicy) []harness.Result
}

// ClientsStepOptions controls the AI clients step.
type ClientsStepOptions struct {
	// AllDetected pre-selects every detected client, not only the configured
	// ones.
	AllDetected bool
	// Scope selects where detection and registration happen (the zero value is
	// global scope).
	Scope harness.Scope
	// DryRun only marks the header; selection works the same.
	DryRun bool
	// App is the header's first part; empty means "creality-k2-mcp setup".
	App string
}

// ClientsStep returns the AI clients step: it detects the clients and lets the
// user pick which ones to register with. It is built on the exported pieces of
// installer.HarnessStep (VisibleIndices, MoveCursor, ToggleHarness, ToggleAll,
// FirstSelectable, HarnessState) and reproduces that step's detection and
// selection behaviour, drawn in the shared frame instead of the library
// chrome (dev_docs/tui-design-v0.4.0.md 3.3, R2.7).
func ClientsStep[T any](ctx context.Context, detector ClientDetector, stateFn func(*T) *installer.HarnessState, opts ClientsStepOptions) flow.Step[T] {
	if detector == nil {
		panic("wizard: ClientsStep requires a non-nil detector")
	}
	return &clientsStep[T]{ctx: ctx, detector: detector, stateFn: stateFn, opts: opts, chrome: newChrome(opts.App, opts.DryRun)}
}

type clientsStep[T any] struct {
	ctx      context.Context
	detector ClientDetector
	stateFn  func(*T) *installer.HarnessState
	opts     ClientsStepOptions
	chrome   chrome
	ready    bool
	// message is the one-line note under the list (enter with nothing
	// selected); the next key clears it.
	message string
	tick    ticker
}

type clientsDetectedMsg struct {
	harnesses []harness.Harness
}

func (s *clientsStep[T]) ID() string { return "harnesses" }

func (s *clientsStep[T]) Title(state *T) string {
	if s.opts.Scope.IsProject() {
		return fmt.Sprintf("Register in this project (%s): which AI clients should be able to use this server?", userhome.Shorten(s.opts.Scope.Dir))
	}
	return "Which AI clients should be able to use this server?"
}

func (s *clientsStep[T]) Hints(state *T) []struct{ Key, Label string } {
	hs := s.get(state)
	if hs == nil {
		return nil
	}
	return legacyHints(s.hints(hs))
}

// hints is the one key list both Hints and the footer use: the show-all key
// reads "show all" or "hide" by the current state, and the list keys only exist
// once detection has finished.
func (s *clientsStep[T]) hints(hs *installer.HarnessState) []frame.Hint {
	if !s.ready {
		return []frame.Hint{frame.Back(), frame.Cancel()}
	}
	v := frame.Hint{Keys: "v", Label: "show all", Priority: 50}
	if hs.ShowAll {
		v.Label = "hide"
	}
	return []frame.Hint{
		{Keys: "↑↓", Label: "move", Priority: 60},
		{Keys: "space", Label: "toggle", Priority: 90},
		{Keys: "a", Label: "all/none", Priority: 70},
		v,
		{Keys: "enter", Label: "continue", Priority: 99},
		frame.Back(),
		frame.Cancel(),
	}
}

func (s *clientsStep[T]) get(state *T) *installer.HarnessState {
	if s.stateFn == nil {
		return nil
	}
	return s.stateFn(state)
}

func (s *clientsStep[T]) Init(state *T) tea.Cmd {
	hs := s.get(state)
	if hs == nil {
		return nil
	}
	s.ready = false
	s.tick.reset()
	s.message = ""
	hs.Scope = s.opts.Scope
	scope := s.opts.Scope
	return tea.Batch(
		s.tick.start(),
		func() tea.Msg {
			if scope.IsProject() {
				return clientsDetectedMsg{harnesses: s.detector.DetectIn(s.ctx, scope)}
			}
			return clientsDetectedMsg{harnesses: s.detector.Detect(s.ctx)}
		},
	)
}

func (s *clientsStep[T]) Update(msg tea.Msg, state *T) (flow.Directive, tea.Cmd) {
	hs := s.get(state)
	if hs == nil {
		return flow.Fail, nil
	}

	switch m := msg.(type) {
	case clientsDetectedMsg:
		hs.Detections = m.harnesses
		hs.Selected = make(map[harness.ID]bool)
		for _, h := range m.harnesses {
			// Pre-select configured clients; in project scope every harness
			// with project support is selectable, but only the ones the user
			// actually has start selected.
			if h.Configured || (s.opts.AllDetected && h.Selectable() && h.Relevant()) {
				hs.Selected[h.ID] = true
			}
		}
		hs.Cursor = installer.FirstSelectable(m.harnesses)
		if hs.Cursor < 0 {
			if indices := installer.VisibleIndices(m.harnesses, false); len(indices) > 0 {
				hs.Cursor = indices[0]
			}
		}
		s.ready = true
		return flow.Continue, nil

	case tea.KeyMsg:
		key := m.String()
		if smallDrops(baseStateOf(state), key, false) {
			return flow.Continue, nil
		}
		if key == "ctrl+c" {
			return flow.Quit, nil
		}
		if !s.ready {
			switch key {
			case "q":
				return flow.Quit, nil
			case "esc":
				return flow.Back, nil
			}
			return flow.Continue, nil
		}
		s.message = ""
		switch key {
		case "q":
			return flow.Quit, nil
		case "up", "k":
			installer.MoveCursor(hs, hs.Detections, -1)
		case "down", "j":
			installer.MoveCursor(hs, hs.Detections, 1)
		case " ":
			installer.ToggleHarness(hs)
		case "a":
			installer.ToggleAll(hs)
		case "v":
			hs.ShowAll = !hs.ShowAll
			if !hs.ShowAll {
				keepCursorVisible(hs)
			}
		case "enter":
			if selectedCount(hs) == 0 {
				s.message = "Select at least one client, or press q to cancel."
				return flow.Continue, nil
			}
			return flow.Next, nil
		case "esc":
			return flow.Back, nil
		}
		return flow.Continue, nil
	}

	if tui.IsSpinMsg(msg) && !s.ready {
		if base := baseStateOf(state); base != nil {
			base.Spinner.Frame++
		}
		return flow.Continue, s.tick.next(!s.ready)
	}
	return flow.Continue, nil
}

// keepCursorVisible moves the cursor to the first visible row when hiding the
// not-installed clients would leave it on a hidden one.
func keepCursorVisible(hs *installer.HarnessState) {
	indices := installer.VisibleIndices(hs.Detections, false)
	if len(indices) == 0 {
		return
	}
	for _, idx := range indices {
		if idx == hs.Cursor {
			return
		}
	}
	hs.Cursor = indices[0]
}

// selectedCount counts the clients whose Selected entry is true. Readers test
// the value: ToggleHarness leaves a false entry behind for a deselected client.
func selectedCount(hs *installer.HarnessState) int {
	n := 0
	for _, on := range hs.Selected {
		if on {
			n++
		}
	}
	return n
}

func (s *clientsStep[T]) View(state *T) string {
	hs := s.get(state)
	if hs == nil {
		return ""
	}
	base := baseStateOf(state)
	return s.chrome.draw(base, "AI clients", 3, s.hints(hs), func(w, rows int) []string {
		out := questionLines(w, s.Title(state))
		if !s.ready {
			return append(out, frame.Gutter+frame.Spinner(spinnerFrame(base))+" Looking for AI clients...")
		}
		indices := installer.VisibleIndices(hs.Detections, hs.ShowAll)
		hidden := len(hs.Detections) - len(indices)

		var tail []string
		if len(indices) == 0 {
			text := "No AI clients were found on this computer."
			if hidden > 0 {
				text += " Press v to show every supported client."
			}
			tail = append(tail, frame.Wrap(w, frame.Gutter, text)...)
		}
		if hidden > 0 && !hs.ShowAll {
			noun := "clients that are not installed"
			if hidden == 1 {
				noun = "client that is not installed"
			}
			tail = append(tail, "", frame.Gutter+frame.StyleDim.Render(fmt.Sprintf("v show %d %s", hidden, noun)))
		}
		if s.message != "" {
			tail = append(tail, "")
			for _, line := range frame.Wrap(w, frame.Gutter, s.message) {
				tail = append(tail, frame.StyleWarn.Render(line))
			}
		}

		cursor, nameW := 0, 0
		for i, idx := range indices {
			nameW = max(nameW, ansi.StringWidth(hs.Detections[idx].Name))
			if idx == hs.Cursor {
				cursor = i
			}
		}
		blocks := make([][]string, len(indices))
		for i, idx := range indices {
			h := hs.Detections[idx]
			glyph := frame.StyleDim.Render("-")
			switch {
			case hs.Selected[h.ID]:
				glyph = frame.StyleOK.Render("●")
			case h.Selectable():
				glyph = frame.StyleDim.Render("○")
			}
			status := frame.StyleDim.Render(h.StatusText())
			if h.Configured {
				status = frame.StyleOK.Render(h.StatusText())
			}
			blocks[i] = []string{frame.Marker(i == cursor) + glyph + " " + frame.Pad(h.Name, nameW) + "  " + status}
		}
		avail := max(rows-len(out)-len(tail), 1)
		out = append(out, listWindow(blocks, cursor, avail)...)
		return append(out, tail...)
	})
}
