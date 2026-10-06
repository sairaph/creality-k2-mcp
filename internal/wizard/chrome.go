package wizard

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// stepTotal is the number of wizard steps the header counts: Printers,
// Settings, AI clients, Registration.
const stepTotal = 4

// chrome draws a wizard step inside the shared frame
// (dev_docs/tui-design-v0.4.0.md 3.2): the header
// "creality-k2-mcp setup  <Step>  step n of 4" (plus the dry-run marker), the
// body clipped to the rows that fit, and the adaptive footer on the last row.
type chrome struct {
	app string
	dry bool
}

func newChrome(app string, dry bool) chrome {
	if app == "" {
		app = frame.AppName + " setup"
	}
	return chrome{app: app, dry: dry}
}

// draw returns the whole screen for the step called name (number n of
// stepTotal). Before the first window size message it returns "" (nothing to
// draw yet), and below the minimum size it returns the too-small message
// instead of the frame; q and ctrl+c keep working there because the steps
// handle keys without looking at the size. body gets the terminal width and
// the number of body rows.
func (c chrome) draw(base *flow.BaseState, name string, n int, hints []frame.Hint, body func(w, rows int) []string) string {
	if base == nil || base.Width <= 0 || base.Height <= 0 {
		return ""
	}
	w, h := base.Width, base.Height
	if msg, small := frame.TooSmall(w, h); small {
		return msg
	}
	header := frame.Header{App: c.app, Name: name, Context: fmt.Sprintf("step %d of %d", n, stepTotal), DryRun: c.dry}
	return frame.Screen(w, h, header, body(w, h-3), frame.Footer(w, hints...))
}

// legacyHints converts frame hints to the key/label pairs flow.Step.Hints
// returns, so the footer and the interface list come from one list.
func legacyHints(hints []frame.Hint) []struct{ Key, Label string } {
	out := make([]struct{ Key, Label string }, len(hints))
	for i, h := range hints {
		out[i] = struct{ Key, Label string }{h.Keys, h.Label}
	}
	return out
}

// busyHints is the footer while a write or a probe is in flight: every key but
// ctrl+c is ignored (dev_docs/tui-design-v0.4.0.md R2.2).
func busyHints(what string) []frame.Hint {
	return []frame.Hint{
		{Label: what, Priority: 99},
		{Keys: "ctrl+c", Label: "quit", Priority: frame.PriorityQuit},
	}
}

// spinnerFrame is the step's spinner counter (0 without a base state).
func spinnerFrame(base *flow.BaseState) int {
	if base == nil {
		return 0
	}
	return base.Spinner.Frame
}

// questionLines is the step's question as the first body lines: wrapped under
// the gutter, then one blank row.
func questionLines(w int, text string) []string {
	return append(frame.Wrap(w, frame.Gutter, text), "")
}

// listWindow flattens blocks (one or more lines per list item) into at most
// avail lines, keeping the block at cursor fully visible and showing as much
// context around it as fits. The window is derived from the cursor alone, so
// it needs no stored scroll offset and follows a resize by itself.
func listWindow(blocks [][]string, cursor, avail int) []string {
	total := 0
	for _, b := range blocks {
		total += len(b)
	}
	flatten := func(lo, hi int) []string {
		var out []string
		for _, b := range blocks[lo:hi] {
			out = append(out, b...)
		}
		return out
	}
	if len(blocks) == 0 {
		return nil
	}
	if total <= avail {
		return flatten(0, len(blocks))
	}
	cursor = min(max(cursor, 0), len(blocks)-1)
	lo, hi, used := cursor, cursor+1, len(blocks[cursor])
	for {
		grew := false
		if hi < len(blocks) && used+len(blocks[hi]) <= avail {
			used += len(blocks[hi])
			hi++
			grew = true
		}
		if lo > 0 && used+len(blocks[lo-1]) <= avail {
			used += len(blocks[lo-1])
			lo--
			grew = true
		}
		if !grew {
			break
		}
	}
	return flatten(lo, hi)
}

// smallDrops reports whether a key must be ignored because the terminal is
// below frame.MinWidth x frame.MinHeight: the screen then shows only the
// too-small message, so only q (not while typing) and ctrl+c keep working
// (dev_docs/tui-design-v0.4.0.md R2.4). Before the first size message nothing
// is known, so nothing is dropped.
func smallDrops(base *flow.BaseState, key string, typing bool) bool {
	if base == nil || base.Width <= 0 || base.Height <= 0 {
		return false
	}
	if _, small := frame.TooSmall(base.Width, base.Height); !small {
		return false
	}
	if key == "ctrl+c" {
		return false
	}
	return key != "q" || typing
}

// ticker keeps at most one spinner tick chain running per step, so the
// spinner never advances two or three times per interval. Ticks carry no
// owner, so a step starts a chain only when it has none.
type ticker struct{ on bool }

// start returns a tick command unless a chain is already running.
func (t *ticker) start() tea.Cmd {
	if t.on {
		return nil
	}
	t.on = true
	return tui.Spinner()
}

// next is called on a tick: it keeps the chain going while active, else ends it.
func (t *ticker) next(active bool) tea.Cmd {
	if !active {
		t.on = false
		return nil
	}
	return tui.Spinner()
}

// reset forgets a chain; a step calls it from Init, which is a fresh start.
func (t *ticker) reset() { t.on = false }
