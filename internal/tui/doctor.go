package tui

import (
	"context"
	"strings"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/userhome"
)

// Doctor row columns: the tag (ok, warn, fail), then the check's name; the
// first line of the detail starts in the value column and later lines wrap
// under it.
const (
	doctorTagWidth  = 6
	doctorNameWidth = 16
)

var nextDoctorRun atomic.Int64

// doctorListMsg carries the check list of one run, built off the UI loop
// (building it reads the registry); doctorResultMsg carries one finished
// check. Both carry the run id, so a result of a run the user has restarted or
// left is dropped.
type doctorListMsg struct {
	run    int64
	checks []doctor.Check
}

type doctorResultMsg struct {
	run    int64
	index  int
	result doctor.Result
}

// doctorScreen runs this server's own doctor checks (internal/doctorlist, the
// list the `doctor` command runs) one at a time and shows each row as it
// finishes, so a slow check (the update check, a printer that does not answer)
// never blanks the screen. Leaving the screen cancels the run. Doctor is the
// one screen that shows paths (R10's exception: they are what the user acts
// on), with the home directory shortened to ~.
type doctorScreen struct {
	ctx    context.Context
	cancel context.CancelFunc
	deps   Deps

	run     int64
	checks  []doctor.Check
	results []doctor.Result
	running bool

	scroll           scrollView
	spin             spinner
	lastH, lastTotal int
}

func newDoctorScreen(ctx context.Context, deps Deps) *doctorScreen {
	ctx, cancel := context.WithCancel(ctx)
	return &doctorScreen{ctx: ctx, cancel: cancel, deps: deps, spin: newSpinner()}
}

func (s *doctorScreen) Init() tea.Cmd {
	return tea.Batch(s.start(), s.spin.ensure(true))
}

// start begins (or restarts) a run.
func (s *doctorScreen) start() tea.Cmd {
	run := nextDoctorRun.Add(1)
	s.run, s.checks, s.results, s.running = run, nil, nil, true
	build := s.deps.DoctorChecks
	return func() tea.Msg { return doctorListMsg{run: run, checks: build()} }
}

// runCheck runs check i and reports it.
func (s *doctorScreen) runCheck(i int) tea.Cmd {
	ctx, run, check := s.ctx, s.run, s.checks[i]
	return func() tea.Msg {
		return doctorResultMsg{run: run, index: i, result: check.Run(ctx)}
	}
}

// Close stops the run: the check in flight sees its context cancelled.
func (s *doctorScreen) Close() { s.cancel() }

func (s *doctorScreen) Update(msg tea.Msg) (tea.Cmd, Nav) {
	cmd := s.update(msg)
	return tea.Batch(cmd, s.spin.ensure(s.running)), NavNone
}

func (s *doctorScreen) update(msg tea.Msg) tea.Cmd {
	if handled, cmd := s.spin.update(msg, s.running); handled {
		return cmd
	}
	switch m := msg.(type) {
	case doctorListMsg:
		if m.run != s.run {
			return nil
		}
		s.checks = m.checks
		if len(s.checks) == 0 {
			s.running = false
			return nil
		}
		return s.runCheck(0)

	case doctorResultMsg:
		if m.run != s.run || m.index != len(s.results) {
			return nil
		}
		s.results = append(s.results, m.result)
		if next := m.index + 1; next < len(s.checks) {
			return s.runCheck(next)
		}
		s.running = false

	case tea.KeyMsg:
		if m.String() == "r" && !s.running {
			return s.start()
		}
		s.scroll.key(m, s.lastTotal, s.lastH)
	}
	return nil
}

func (s *doctorScreen) Back() bool           { return false }
func (s *doctorScreen) Mode() Mode           { return ModeNormal }
func (s *doctorScreen) Header() frame.Header { return frame.Header{Name: "Doctor"} }

func (s *doctorScreen) Hints(w, h int) []frame.Hint {
	var out []frame.Hint
	if len(s.lines(w)) > h {
		out = append(out, frame.Hint{Keys: "↑↓", Label: "scroll", Priority: 70})
	}
	if !s.running {
		out = append(out, frame.Hint{Keys: "r", Label: "run again", Priority: 60})
	}
	return append(out, frame.Back(), frame.Quit())
}

func (s *doctorScreen) tag(status doctor.Status) string {
	text := frame.Pad(string(status), doctorTagWidth)
	switch status {
	case doctor.OK:
		return styleOK.Render(text)
	case doctor.Warn:
		return styleWarn.Render(text)
	case doctor.Fail:
		return styleError.Render(text)
	}
	return text
}

// nameWidth is the width of the longest check name in the list (or already
// reported). The name column is at least doctorNameWidth wide and, for a longer
// name, that name plus two spaces, so a long name never overflows.
func (s *doctorScreen) nameWidth() int {
	w := 0
	for _, c := range s.checks {
		w = max(w, ansi.StringWidth(c.Name()))
	}
	for _, r := range s.results {
		w = max(w, ansi.StringWidth(r.Name))
	}
	return w
}

// lines is the whole Doctor body at width w, before scrolling: one aligned row
// per finished check, then the check in flight.
func (s *doctorScreen) lines(w int) []string {
	longest := s.nameWidth()
	nameW := max(doctorNameWidth, longest)
	gapW := max(doctorNameWidth+1, longest+2) // the label column after the tag and its space
	var kvs []frame.KV
	for _, r := range s.results {
		kvs = append(kvs, frame.KV{
			Label: s.tag(r.Status) + " " + frame.Pad(r.Name, nameW),
			Value: strings.TrimRight(userhome.Shorten(r.Detail), "\n "),
		})
	}
	out := frame.Rows(w, doctorTagWidth+1+gapW, kvs...)
	switch {
	case s.running && len(s.results) < len(s.checks):
		out = append(out, frame.Gutter+s.spin.glyph()+" checking "+s.checks[len(s.results)].Name()+"...")
	case s.running:
		out = append(out, frame.Gutter+s.spin.glyph()+" preparing the checks...")
	case len(s.checks) == 0:
		out = append(out, frame.Gutter+"There are no checks to run.")
	}
	return out
}

func (s *doctorScreen) Body(w, h int) []string {
	lines := s.lines(w)
	s.lastH, s.lastTotal = h, len(lines)
	return s.scroll.view(lines, h)
}
