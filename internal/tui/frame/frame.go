// Package frame is the one renderer every full-screen view of the app and the
// install wizard goes through (dev_docs/tui-design-v0.4.0.md section 1): row 1
// the header, row 2 blank, then the body clipped to the rows that fit and
// padded with blank rows, and the footer pinned to the last row. It holds no
// state; callers pass the real terminal size on every draw.
//
// Every function that takes a width w takes the terminal width and keeps one
// column free (w-1), so a full line never soft-wraps and pushes the footer off
// the screen (the mechanism sana-mcp's UI.Screen uses).
package frame

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// AppName is the product name shown first in every header.
const AppName = "creality-k2-mcp"

// MinWidth and MinHeight are the smallest terminal the frame draws in; below
// either, TooSmall replaces every view.
const (
	MinWidth  = 60
	MinHeight = 16
)

// SpinInterval is how often a spinner frame advances.
const SpinInterval = 110 * time.Millisecond

// Gutter is the indent every body line starts with.
const Gutter = "  "

// The one palette (256-colour, so it looks the same in a plain terminal as in
// a truecolor one). Semantic only: green is ok/ready/on, amber is attention,
// busy or changed, red is failed or blocked, dim is secondary text and hints.
var (
	StyleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	StyleName   = lipgloss.NewStyle().Bold(true)
	StyleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	StyleCursor = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	StyleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	StyleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	StyleError  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

// Header is the first row: the app name, the screen name and an optional
// context, e.g. "creality-k2-mcp  Status  K2-5885 (192.168.1.102)". The wizard
// sets App to "creality-k2-mcp setup" (or "... project setup") and DryRun.
type Header struct {
	App     string
	Name    string
	Context string
	DryRun  bool
}

func (h Header) render() string {
	app := h.App
	if app == "" {
		app = AppName
	}
	s := StyleTitle.Render(app)
	if h.Name != "" {
		s += "  " + StyleName.Render(h.Name)
	}
	if h.Context != "" {
		s += "  " + StyleDim.Render(h.Context)
	}
	if h.DryRun {
		s += "  " + StyleWarn.Render("dry run")
	}
	return s
}

// Screen lays out a full-height view of exactly h rows: the header on row 1,
// a blank row 2, the body (already indented by the caller) clipped to h-3 rows
// and padded with blank rows, and the footer on row h. Every row is cut to
// w-1 display columns, styled or not. The footer text is drawn dim.
func Screen(w, h int, header Header, body []string, footer string) string {
	if h < 1 {
		return ""
	}
	rows := make([]string, h)
	rows[0] = header.render()
	if h >= 3 {
		rows[h-1] = StyleDim.Render(Clip(footer, w))
	}
	for i, line := range body {
		row := i + 2
		if row >= h-1 {
			break
		}
		rows[row] = line
	}
	for i, row := range rows {
		rows[i] = Clip(row, w)
	}
	return strings.Join(rows, "\n")
}

// Marker is the start of a list row: the gutter, then a 2-column slot holding
// the "> " cursor (colour 81) on the selected row and blanks on the others.
func Marker(selected bool) string {
	if selected {
		return Gutter + StyleCursor.Render(">") + " "
	}
	return Gutter + "  "
}

// Clip cuts s to w-1 display columns, keeping any ANSI styling valid.
func Clip(s string, w int) string {
	return ansi.Truncate(s, max(w-1, 0), "")
}

// Pad right-pads s with spaces to width display columns (it never cuts).
func Pad(s string, width int) string {
	if n := width - ansi.StringWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// Hint is one footer entry. Hints with a higher Priority survive longer on a
// narrow terminal; a Hint with empty Keys is plain text (e.g. "saving...").
type Hint struct {
	Keys     string
	Label    string
	Priority int
}

const (
	// PriorityBack and PriorityQuit are above every ordinary hint, so the
	// footer keeps them until nothing else is left.
	PriorityBack = 100
	PriorityQuit = 101
)

// Back is the "esc back" hint.
func Back() Hint { return Hint{Keys: "esc", Label: "back", Priority: PriorityBack} }

// Quit is the "q quit" hint.
func Quit() Hint { return Hint{Keys: "q", Label: "quit", Priority: PriorityQuit} }

// Cancel is the wizard's "q cancel" hint.
func Cancel() Hint { return Hint{Keys: "q", Label: "cancel", Priority: PriorityQuit} }

func (h Hint) text() string {
	switch {
	case h.Keys == "":
		return h.Label
	case h.Label == "":
		return h.Keys
	}
	return h.Keys + " " + h.Label
}

const footerSep = " · "

// Footer returns the footer line for a terminal w columns wide: the longest
// candidate that fits w-1 columns, found by dropping the lowest-priority hint
// one at a time (equal priorities are dropped right to left). Back and quit
// (priority 100 and up) are always last in the line; the final fallback is the
// last hint with the highest priority in the list ("q quit").
func Footer(w int, hints ...Hint) string {
	if len(hints) == 0 {
		return ""
	}
	cur := make([]Hint, 0, len(hints))
	var tail []Hint
	for _, h := range hints {
		if h.Priority >= PriorityBack {
			tail = append(tail, h)
		} else {
			cur = append(cur, h)
		}
	}
	cur = append(cur, tail...)

	for len(cur) > 1 {
		if line := footerLine(cur); ansi.StringWidth(line) <= w-1 {
			return line
		}
		low := 0
		for i, h := range cur {
			if h.Priority <= cur[low].Priority {
				low = i
			}
		}
		cur = append(cur[:low:low], cur[low+1:]...)
	}

	best := hints[0]
	for _, h := range hints {
		if h.Priority >= best.Priority {
			best = h
		}
	}
	return footerLine([]Hint{best})
}

func footerLine(hints []Hint) string {
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = h.text()
	}
	return Gutter + strings.Join(parts, footerSep)
}

// KV is one label/value summary row; Style colours the value.
type KV struct {
	Label string
	Value string
	Style lipgloss.Style
}

// Rows renders summary rows on one label column: the gutter, the label padded
// to labelWidth display columns, then the value. A long value (or one with
// newlines) wraps under the value column, never under the label, and every
// line fits w-1 columns. A Label may carry ANSI styling.
func Rows(w, labelWidth int, rows ...KV) []string {
	valueW := max(w-1-len(Gutter)-labelWidth, 10)
	pad := Gutter + strings.Repeat(" ", labelWidth)
	var out []string
	for _, kv := range rows {
		if kv.Label == "" && kv.Value == "" {
			out = append(out, "") // a spacer row
			continue
		}
		label := Gutter + Pad(kv.Label, labelWidth)
		lines := strings.Split(ansi.Wrap(kv.Value, valueW, ""), "\n")
		for i, line := range lines {
			if line != "" {
				line = kv.Style.Render(line)
			}
			if i == 0 {
				out = append(out, label+line)
			} else {
				out = append(out, pad+line)
			}
		}
	}
	return out
}

// Wrap wraps text to w-1 display columns (breaking inside a word only when it
// is longer than a line) and prefixes every line with indent. Newlines in text
// start new lines.
func Wrap(w int, indent, text string) []string {
	avail := max(w-1-ansi.StringWidth(indent), 1)
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		for _, line := range strings.Split(ansi.Wrap(para, avail, ""), "\n") {
			out = append(out, indent+line)
		}
	}
	return out
}

// TooSmall reports whether the terminal is below MinWidth x MinHeight and, if
// so, the two-line message that replaces every view.
func TooSmall(w, h int) (string, bool) {
	if w >= MinWidth && h >= MinHeight {
		return "", false
	}
	msg := fmt.Sprintf("  Terminal is %dx%d.\n  %s needs at least %dx%d.", w, h, AppName, MinWidth, MinHeight)
	return msg, true
}

// Spinner returns the ASCII spinner glyph for a frame counter: ASCII on
// purpose, since the first thing a person sees may be a console without
// braille glyphs. Callers tick every SpinInterval.
func Spinner(frame int) string {
	const glyphs = `-\|/`
	if frame < 0 {
		frame = -frame
	}
	return string(glyphs[frame%len(glyphs)])
}
