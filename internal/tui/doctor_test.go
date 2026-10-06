package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/userhome"
)

// fakeCheck is a doctor check with a canned result: no network, no registry.
type fakeCheck struct {
	name string
	res  doctor.Result
	ran  *runCounter
}

type runCounter struct {
	mu sync.Mutex
	n  int
}

func (c *runCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (f fakeCheck) Name() string { return f.name }

func (f fakeCheck) Run(ctx context.Context) doctor.Result {
	if f.ran != nil {
		f.ran.mu.Lock()
		f.ran.n++
		f.ran.mu.Unlock()
	}
	res := f.res
	res.Name = f.name
	return res
}

func fakeChecks(ran *runCounter) func() []doctor.Check {
	return func() []doctor.Check {
		home, _ := userhome.Dir() // resolved at run time, after the test isolated HOME
		return []doctor.Check{
			fakeCheck{name: "Executable", res: doctor.Result{Status: doctor.OK, Detail: home + "/bin/creality-k2-mcp"}, ran: ran},
			fakeCheck{name: "PATH", res: doctor.Result{Status: doctor.Warn, Detail: "/opt/creality is not on PATH"}, ran: ran},
			fakeCheck{name: "Settings", res: doctor.Result{Status: doctor.Fail, Detail: "first line\nsecond line\nthird line"}, ran: ran},
			fakeCheck{name: "AI clients", res: doctor.Result{Status: doctor.OK}, ran: ran},
			fakeCheck{name: "Version", res: doctor.Result{Status: doctor.OK, Detail: strings.Repeat("A long sentence that has to wrap. ", 8)}, ran: ran},
		}
	}
}

func doctorHarness(t *testing.T, w, h int, checks func() []doctor.Check) *harness {
	t.Helper()
	isolateHome(t)
	hs := newHarness(t, Deps{DoctorChecks: checks}, w, h)
	hs.open("Doctor")
	return hs
}

func TestDoctorRowsAreAlignedAndDetailsIndented(t *testing.T) {
	for _, sz := range sizes {
		h := doctorHarness(t, sz[0], sz[1], fakeChecks(nil))
		h.requireFrame("doctor")
		ls := h.lines()
		if ls[0] != "creality-k2-mcp  Doctor" {
			t.Errorf("header = %q", ls[0])
		}
		const valueCol = 26 // gutter 2 + tag 6 + gap 1 + name 16 + gap 1
		want := []struct{ prefix, value string }{
			{"  ok     Executable ", "~/bin/creality-k2-mcp"},
			{"  warn   PATH ", "/opt/creality is not on PATH"},
			{"  fail   Settings ", "first line"},
		}
		for i, w := range want {
			line := ls[2+i]
			if !strings.HasPrefix(line, w.prefix) {
				t.Errorf("%dx%d: row %d = %q, want it to start with %q", sz[0], sz[1], i+1, line, w.prefix)
			}
			if idx := strings.Index(line, w.value); idx != valueCol {
				t.Errorf("%dx%d: row %d value starts at column %d, want %d: %q", sz[0], sz[1], i+1, idx, valueCol, line)
			}
		}
		// A multi-line detail continues under the value column, not at column 0.
		pad := strings.Repeat(" ", valueCol)
		if ls[5] != pad+"second line" || ls[6] != pad+"third line" {
			t.Errorf("%dx%d: detail continuation = %q / %q", sz[0], sz[1], ls[5], ls[6])
		}
		// A check without detail is just its row; a long one wraps under the column.
		if !strings.HasPrefix(ls[7], "  ok     AI clients") || strings.TrimSpace(ls[7]) != "ok     AI clients" {
			t.Errorf("a check with no detail = %q", ls[7])
		}
		wrapped := 0
		for _, l := range ls[8 : len(ls)-1] {
			if l != "" {
				wrapped++
				if !strings.HasPrefix(l, pad) && !strings.HasPrefix(l, "  ok     Version") {
					t.Errorf("%dx%d: a wrapped line is not under the value column: %q", sz[0], sz[1], l)
				}
			}
		}
		if wrapped < 2 {
			t.Errorf("%dx%d: the long detail did not wrap:\n%s", sz[0], sz[1], h.text())
		}
		if strings.Contains(h.text(), userhomeForTest(t)) {
			t.Errorf("the home directory must be shortened to ~:\n%s", h.text())
		}
	}
}

func userhomeForTest(t *testing.T) string {
	home, err := userhome.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func TestDoctorFooter(t *testing.T) {
	h := doctorHarness(t, 120, 36, fakeChecks(nil))
	if got := strings.TrimSpace(h.footer()); got != "r run again · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") {
		t.Errorf("esc did not leave: %q", h.lines()[0])
	}
}

// The rows appear one at a time, as each check finishes, with a spinner row
// naming the check in flight.
func TestDoctorShowsRowsAsChecksFinish(t *testing.T) {
	isolateHome(t)
	ran := &runCounter{}
	s := newDoctorScreen(t.Context(), Deps{DoctorChecks: fakeChecks(ran)}.withDefaults())
	rows := func() string { return ansi.Strip(strings.Join(s.Body(120, 30), "\n")) }
	names := []string{"Executable", "PATH", "Settings", "AI clients", "Version"}

	cmd := s.start()
	if !strings.Contains(rows(), "preparing the checks...") {
		t.Errorf("before the list arrives:\n%s", rows())
	}

	// Run commands and deliver their messages the way the event loop would,
	// except the spinner's own ticks, and look at the screen after each result.
	pending, results := []tea.Cmd{cmd}, 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		for _, m := range drainCmd(c) {
			if _, tick := m.(spinTickMsg); tick {
				continue
			}
			if _, isList := m.(doctorListMsg); isList && ran.count() != 0 {
				t.Fatal("a check ran before the list arrived")
			}
			next, _ := s.Update(m)
			if next != nil {
				pending = append(pending, next)
			}
			switch m.(type) {
			case doctorListMsg:
				if ran.count() != 0 || !strings.Contains(rows(), "checking Executable...") {
					t.Errorf("after the list: %d runs\n%s", ran.count(), rows())
				}
			case doctorResultMsg:
				results++
				text := rows()
				for _, name := range names[:results] {
					if !strings.Contains(text, "  "+name) || strings.Contains(text, "checking "+name+"...") {
						t.Errorf("after %d result(s) the row for %s is wrong:\n%s", results, name, text)
					}
				}
				if results < len(names) && !strings.Contains(text, "checking "+names[results]+"...") {
					t.Errorf("after %d result(s) want the spinner row for %s:\n%s", results, names[results], text)
				}
			}
		}
	}
	if results != len(names) || s.running || strings.Contains(rows(), "checking") {
		t.Errorf("results=%d running=%v, want the run over:\n%s", results, s.running, rows())
	}
	if ran.count() != len(names) {
		t.Errorf("%d checks ran, want %d", ran.count(), len(names))
	}
}

func TestDoctorRunAgainOnlyWhenFinishedAndDropsStaleResults(t *testing.T) {
	ran := &runCounter{}
	h := doctorHarness(t, 120, 36, fakeChecks(ran))
	if ran.count() != 5 {
		t.Fatalf("%d checks ran, want 5", ran.count())
	}
	s := h.m.screen.(*doctorScreen)
	oldRun := s.run

	h.key("r")
	if ran.count() != 10 {
		t.Errorf("r did not run the checks again: %d runs", ran.count())
	}
	if s.run == oldRun {
		t.Error("a new run must get a new id")
	}
	h.send(doctorResultMsg{run: oldRun, index: len(s.results), result: doctor.Result{Name: "Stale", Status: doctor.Fail}})
	if strings.Contains(h.text(), "Stale") {
		t.Errorf("a result of an earlier run showed up:\n%s", h.text())
	}

	// While running, r does nothing and is not offered.
	s.running = true
	if hasHint(s.Hints(120, 33), "r run again") {
		t.Error("r run again is offered while running")
	}
	before := ran.count()
	h.key("r")
	if ran.count() != before {
		t.Error("r restarted a run in progress")
	}
}

func TestDoctorLeavingCancelsTheRun(t *testing.T) {
	h := doctorHarness(t, 120, 36, fakeChecks(nil))
	s := h.m.screen.(*doctorScreen)
	if s.ctx.Err() != nil {
		t.Fatal("the run's context is cancelled while the screen is open")
	}
	h.key("esc")
	if s.ctx.Err() == nil {
		t.Error("leaving the screen must cancel the checks' context")
	}
}

func TestDoctorScrollsALongReport(t *testing.T) {
	var checks []doctor.Check
	for i := 0; i < 40; i++ {
		checks = append(checks, fakeCheck{name: fmt.Sprintf("Check %02d", i), res: doctor.Result{Status: doctor.OK, Detail: "fine"}})
	}
	h := doctorHarness(t, 80, 24, func() []doctor.Check { return checks })
	h.requireFrame("doctor with a long report")
	if got := strings.TrimSpace(h.footer()); got != "↑↓ scroll · r run again · esc back · q quit" {
		t.Errorf("footer = %q", got)
	}
	if !strings.Contains(h.lines()[2], "Check 00") {
		t.Errorf("first row = %q", h.lines()[2])
	}
	h.key("down", "down", "down")
	if !strings.Contains(h.lines()[2], "Check 03") {
		t.Errorf("down did not scroll: %q", h.lines()[2])
	}
	h.key("end")
	if !strings.Contains(h.lines()[len(h.lines())-2], "Check 39") {
		t.Errorf("end did not reach the last row:\n%s", h.text())
	}
	h.requireFrame("doctor scrolled to the end")
}

func TestDoctorWithoutChecksSaysSo(t *testing.T) {
	h := doctorHarness(t, 80, 24, nil)
	h.requireFrame("doctor without checks")
	if !strings.Contains(h.text(), "There are no checks to run.") {
		t.Errorf("missing the sentence:\n%s", h.text())
	}
}

func TestDoctorTagsAreColouredByStatus(t *testing.T) {
	if !strings.Contains(styleOK.Render("x"), "\x1b[") {
		t.Skip("no colour profile in this environment")
	}
	s := newDoctorScreen(t.Context(), Deps{}.withDefaults())
	ok, warn, fail := s.tag(doctor.OK), s.tag(doctor.Warn), s.tag(doctor.Fail)
	if ok == warn || warn == fail || ok == fail {
		t.Errorf("ok, warn and fail must be three colours: %q %q %q", ok, warn, fail)
	}
	for _, tag := range []string{ok, warn, fail} {
		if ansi.StringWidth(tag) != doctorTagWidth {
			t.Errorf("tag %q is %d columns wide, want %d", tag, ansi.StringWidth(tag), doctorTagWidth)
		}
	}
}

// A check name longer than the name column widens the column for every row, and
// the detail wraps in the width that remains: nothing is cut or run together.
func TestDoctorLongCheckNameWidensTheColumnAndWrapsTheDetail(t *testing.T) {
	const name = "Camera/idle-heat daemon"
	const detail = "not running; idle heating is refused while it is down (a heater set while the printer is idle would stay on)"
	list := func() []doctor.Check {
		return []doctor.Check{
			fakeCheck{name: "PATH", res: doctor.Result{Status: doctor.OK, Detail: "fine"}},
			fakeCheck{name: name, res: doctor.Result{Status: doctor.Warn, Detail: detail}},
		}
	}
	for _, sz := range [][2]int{{80, 24}, {120, 36}} {
		h := doctorHarness(t, sz[0], sz[1], list)
		h.requireFrame("doctor with a long check name")
		var valueCol int
		var got []string
		for _, l := range h.lines()[2 : sz[1]-1] {
			if strings.HasPrefix(strings.TrimSpace(l), "warn") {
				valueCol = strings.Index(l, name) + len(name) + 2
				if !strings.HasPrefix(l[strings.Index(l, name)+len(name):], "  not running;") {
					t.Errorf("%dx%d: the name and the detail must be separated by two spaces: %q", sz[0], sz[1], l)
				}
				got = append(got, strings.TrimSpace(l[valueCol:]))
				continue
			}
			if len(got) > 0 && strings.TrimSpace(l) != "" {
				if !strings.HasPrefix(l, strings.Repeat(" ", valueCol)) {
					t.Errorf("%dx%d: continuation %q is not under the value column", sz[0], sz[1], l)
				}
				got = append(got, strings.TrimSpace(l))
			}
		}
		if joined := strings.Join(got, " "); joined != detail {
			t.Errorf("%dx%d: detail = %q, want %q", sz[0], sz[1], joined, detail)
		}
		// The short check lines up with the long one.
		for _, l := range h.lines() {
			if strings.HasPrefix(strings.TrimSpace(l), "ok") && strings.Index(l, "fine") != valueCol {
				t.Errorf("%dx%d: the short row's detail is not in the same column: %q", sz[0], sz[1], l)
			}
		}
	}
}
