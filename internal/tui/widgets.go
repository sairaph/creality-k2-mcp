package tui

import (
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// spinInterval is how often a spinner frame advances; a variable so a test can
// shrink it instead of waiting out real ticks.
var spinInterval = frame.SpinInterval

// selectList is the cursor and scroll window of a list: the cursor stays
// inside the list, the window keeps it visible within the rows given to
// window, and a resize only re-clamps (no pager text anywhere).
type selectList struct {
	cursor int
	top    int
}

// key applies a movement key to a list of n rows with page rows per page and
// reports whether it was one.
func (l *selectList) key(k tea.KeyMsg, n, page int) bool {
	switch k.String() {
	case "up", "k":
		l.cursor--
	case "down", "j":
		l.cursor++
	case "pgup":
		l.cursor -= max(page, 1)
	case "pgdown":
		l.cursor += max(page, 1)
	case "home", "g":
		l.cursor = 0
	case "end", "G":
		l.cursor = n - 1
	default:
		return false
	}
	l.clamp(n)
	return true
}

func (l *selectList) clamp(n int) {
	l.cursor = max(min(l.cursor, n-1), 0)
}

// window clamps the cursor and the scroll offset for h visible rows of n and
// returns the half-open range of rows to draw.
func (l *selectList) window(n, h int) (from, to int) {
	h = max(h, 1)
	l.clamp(n)
	if l.cursor < l.top {
		l.top = l.cursor
	}
	if l.cursor >= l.top+h {
		l.top = l.cursor - h + 1
	}
	l.top = max(min(l.top, n-h), 0)
	return l.top, min(l.top+h, n)
}

// scrollView is a line offset into content longer than the body. The offset
// survives content refreshes and is clamped, never reset, when the content or
// the terminal shrinks.
type scrollView struct{ off int }

// key applies a scroll key to total lines shown h at a time and reports
// whether it was one.
func (s *scrollView) key(k tea.KeyMsg, total, h int) bool {
	switch k.String() {
	case "up", "k":
		s.off--
	case "down", "j":
		s.off++
	case "pgup":
		s.off -= max(h, 1)
	case "pgdown", " ":
		s.off += max(h, 1)
	case "home", "g":
		s.off = 0
	case "end", "G":
		s.off = total
	default:
		return false
	}
	s.clamp(total, h)
	return true
}

func (s *scrollView) clamp(total, h int) {
	s.off = max(min(s.off, total-h), 0)
}

// view returns the lines visible in h rows, clamping the offset first.
func (s *scrollView) view(lines []string, h int) []string {
	s.clamp(len(lines), h)
	return lines[s.off:min(s.off+max(h, 0), len(lines))]
}

var nextSpinnerID atomic.Int64

type spinTickMsg struct{ id int64 }

// spinner animates an ASCII glyph. Each screen owns one: ticks carry its id,
// so a tick that outlives the screen (or arrives twice) is ignored instead of
// speeding the next screen's spinner up.
type spinner struct {
	id      int64
	frame   int
	ticking bool
}

func newSpinner() spinner { return spinner{id: nextSpinnerID.Add(1)} }

func (s *spinner) glyph() string { return frame.Spinner(s.frame) }

// ensure starts the tick chain when something is in progress and none runs.
func (s *spinner) ensure(active bool) tea.Cmd {
	if !active || s.ticking {
		return nil
	}
	s.ticking = true
	id := s.id
	return tea.Tick(spinInterval, func(time.Time) tea.Msg { return spinTickMsg{id} })
}

// update advances the glyph on this spinner's tick and keeps the chain going
// while active; handled is false for any other message.
func (s *spinner) update(msg tea.Msg, active bool) (handled bool, cmd tea.Cmd) {
	t, ok := msg.(spinTickMsg)
	if !ok || t.id != s.id {
		return false, nil
	}
	s.frame++
	s.ticking = false
	return true, s.ensure(active)
}
