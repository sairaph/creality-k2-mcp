package tui

import "github.com/charmbracelet/lipgloss"

// Shared styles for every hand-rolled screen in this package (Printers,
// Status, Camera, Recordings), matching the 256-colour palette
// mcp-wizard/app's own menu/list/table/detail/confirm components already
// use (see e.g. references/mcp-wizard/app/menu/menu.go), so every screen in
// the application looks like one consistent program.
var (
	tuiStyleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	tuiStyleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	tuiStyleCursor = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	tuiStyleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	tuiStyleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	tuiStyleError  = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

// tuiConfirmChoiceLines renders a two-option yes/no prompt matching
// mcp-wizard/app/confirm's own layout (cancel first, preselected), for the
// hand-rolled confirmations this package needs beyond what confirm.Model
// covers (Printers' remove/allow-control prompts, which are embedded inside
// a larger screen rather than a standalone one). yes selects the
// (destructive or granting) confirmLabel option.
func tuiConfirmChoiceLines(yes bool, confirmLabel, cancelLabel string) string {
	cancel, confirmLine := "  "+cancelLabel, "  "+confirmLabel
	if yes {
		cancel = "  " + cancelLabel
		confirmLine = tuiStyleCursor.Render("> ") + tuiStyleError.Render(confirmLabel)
	} else {
		cancel = tuiStyleCursor.Render("> ") + cancelLabel
		confirmLine = "  " + confirmLabel
	}
	return "  " + cancel + "\n  " + confirmLine + "\n\n" + tuiStyleDim.Render("  ↑↓ choose · enter confirm · esc cancel")
}
