package tui

import (
	"errors"
	"github.com/charmbracelet/lipgloss"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// The palette lives in internal/tui/frame (one place, shared with the install
// wizard); these are the short names every screen here uses.
var (
	styleDim   = frame.StyleDim
	styleOK    = frame.StyleOK
	styleWarn  = frame.StyleWarn
	styleError = frame.StyleError
)

// confirmChoice is the answer a key gave to a yes/no dialog.
type confirmChoice int

const (
	confirmPending confirmChoice = iota
	confirmYes
	confirmNo
)

// confirmKey applies one key to a dialog whose highlighted answer is *yes
// (default: no): arrows and tab flip it, y and n answer, enter takes the
// highlighted answer, esc and q cancel.
func confirmKey(m tea.KeyMsg, yes *bool) confirmChoice {
	switch m.String() {
	case "up", "down", "k", "j", "tab", "left", "right", "h", "l":
		*yes = !*yes
	case "esc", "n", "q":
		return confirmNo
	case "y":
		return confirmYes
	case "enter":
		if *yes {
			return confirmYes
		}
		return confirmNo
	}
	return confirmPending
}

// confirmBody renders a dialog: the question in the warning colour, the
// explanation wrapped under it and the two answers (cancel first).
func confirmBody(w int, question, explanation string, yes bool, yesLabel, noLabel string) []string {
	out := []string{frame.Gutter + styleWarn.Render(question), ""}
	for _, line := range frame.Wrap(w, frame.Gutter, explanation) {
		out = append(out, styleDim.Render(line))
	}
	yesText := yesLabel
	if yes {
		yesText = styleError.Render(yesLabel)
	}
	return append(out, "",
		frame.Marker(!yes)+noLabel,
		frame.Marker(yes)+yesText)
}

// confirmHints is the footer of every dialog.
func confirmHints() []frame.Hint {
	return []frame.Hint{
		{Keys: "y", Label: "yes", Priority: 80},
		{Keys: "n", Label: "no", Priority: 70},
		{Keys: "esc", Label: "cancel", Priority: frame.PriorityBack},
	}
}

// busyHints is the footer while a write is in flight: only ctrl+c works.
func busyHints(what string) []frame.Hint {
	return []frame.Hint{
		{Label: what, Priority: 50},
		{Keys: "ctrl+c", Label: "quit", Priority: frame.PriorityQuit},
	}
}

// joinNonEmpty joins the non-empty parts with sep.
func joinNonEmpty(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

// wrapStyled wraps text to the gutter-indented body width and colours each
// line (wrapping the plain text first keeps the colour codes out of the width
// maths).
func wrapStyled(w int, style lipgloss.Style, text string) []string {
	lines := frame.Wrap(w, frame.Gutter, text)
	for i, l := range lines {
		lines[i] = style.Render(l)
	}
	return lines
}

// errDaemonUnavailable is what Camera and Recordings say when no daemon client
// is wired up.
var errDaemonUnavailable = errors.New("The camera daemon is not available.")
