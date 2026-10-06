package wizard

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/userhome"
)

// Tone is how a client's outcome is coloured: ok is green, warn amber (a
// conflict or a skipped client), error red.
type Tone int

const (
	ToneOK Tone = iota
	ToneWarn
	ToneError
)

// ClientLine is one client's outcome as the finish screen shows it.
type ClientLine struct {
	ID     harness.ID
	Name   string
	Status string
	Tone   Tone
}

// ClientLines is the one place the finish screen's status words come from. It
// matches the wording of the library's unattended output exactly: for a real
// run, installer.formatResultStatus (what installer.PrintResults prints with
// enabling=true, dryRun=false); for a dry run, what installer.PrintChanges
// prints (`already registered`, `would <action>`, `<state>: <reason>`). A test
// feeds the same results and changes to the library printers and compares.
func ClientLines(dryRun bool, results []harness.Result, changes []harness.Change) []ClientLine {
	var out []ClientLine
	if dryRun {
		for _, c := range changes {
			name := c.Name
			if name == "" {
				name = string(c.HarnessID)
			}
			line := ClientLine{ID: c.HarnessID, Name: name}
			switch {
			case c.State == harness.ApplyNoop:
				line.Status = "already registered"
			case c.Action != "":
				line.Status = "would " + c.Action
			case c.Reason != "":
				line.Status = string(c.State) + ": " + c.Reason
				line.Tone = ToneWarn
			default:
				line.Status = string(c.State)
				line.Tone = ToneWarn
			}
			if c.State == harness.ApplyFailed {
				line.Tone = ToneError
			}
			out = append(out, line)
		}
		return out
	}
	for _, r := range results {
		line := ClientLine{ID: r.HarnessID, Name: r.Name, Status: "registered"}
		switch r.State {
		case harness.ApplyNoop:
			line.Status = "already registered"
		case harness.ApplyConflict:
			line.Status, line.Tone = "conflict", ToneWarn
		case harness.ApplySkipped:
			line.Status, line.Tone = "skipped: "+r.Reason, ToneWarn
		case harness.ApplyFailed:
			line.Status, line.Tone = "failed: "+r.Reason, ToneError
		}
		out = append(out, line)
	}
	return out
}

func (t Tone) style() lipgloss.Style {
	switch t {
	case ToneWarn:
		return frame.StyleWarn
	case ToneError:
		return frame.StyleError
	}
	return frame.StyleOK
}

// FinishInput is everything the finish screen and the summary printed after
// the program ends are built from.
type FinishInput struct {
	DryRun bool
	Scope  harness.Scope
	// Failed is true when registration failed (the flow carries a Failure).
	Failed     bool
	Printers   []PrinterRow
	Preset     domain.ToolPreset
	Detections []harness.Harness
	Results    []harness.Result
	Changes    []harness.Change
}

// NewFinishInput gathers the finish data from the wizard's step states.
func NewFinishInput(scope harness.Scope, dryRun bool, failure error, ps *PrinterState, ss *SettingsState, hs *installer.HarnessState, rs *installer.ResultsState) FinishInput {
	in := FinishInput{DryRun: dryRun, Scope: scope, Failed: failure != nil}
	if ps != nil {
		in.Printers = ps.Rows
	}
	if ss != nil {
		in.Preset = ss.Form.Values().Tools.Preset
	}
	if hs != nil {
		in.Detections = hs.Detections
	}
	if rs != nil {
		in.Results, in.Changes = rs.Results, rs.Changes
	}
	return in
}

// command is the subcommand that repeats this setup: add for a project, install
// for the global one.
func (in FinishInput) command() string {
	if in.Scope.IsProject() {
		return "add"
	}
	return "install"
}

// printersSummary is the Printers row: how many printers are enabled, by name,
// with whether the AI may control each.
func printersSummary(rows []PrinterRow) string {
	var parts []string
	for _, r := range rows {
		if !r.Addable || !r.Printer.Enabled {
			continue
		}
		control := "control off"
		if r.Printer.AllowControl {
			control = "control on"
		}
		parts = append(parts, displayName(r)+", "+control)
	}
	switch len(parts) {
	case 0:
		return "none enabled"
	case 1:
		return "1 enabled (" + parts[0] + ")"
	}
	return fmt.Sprintf("%d enabled (%s)", len(parts), strings.Join(parts, "; "))
}

// FinishLines is the finish screen's body (and, printed after the program
// ends, the summary that stays in the scrollback), wrapped to a terminal w
// columns wide: the heading, the summary rows, one row per client, a restart
// line per client whose configuration changed, and what to do next.
func FinishLines(w int, in FinishInput) []string {
	lines := ClientLines(in.DryRun, in.Results, in.Changes)

	registered := 0
	for _, l := range lines {
		if l.Tone == ToneOK {
			registered++
		}
	}
	clients := "none selected"
	switch {
	case len(lines) == 0:
	case in.DryRun:
		clients = fmt.Sprintf("%d would be registered", registered)
	default:
		clients = fmt.Sprintf("%d registered", registered)
	}

	var heading string
	switch {
	case in.Failed:
		heading = frame.StyleError.Render("Registration failed.")
	case in.DryRun:
		heading = frame.StyleWarn.Render("Dry run: nothing was written.")
	default:
		heading = frame.StyleOK.Render("Setup complete.")
	}
	out := []string{frame.Gutter + heading, ""}

	labelW := 16
	for _, l := range lines {
		labelW = max(labelW, ansi.StringWidth(l.Name)+2)
	}
	summary := []frame.KV{}
	if in.Scope.IsProject() && in.Scope.Dir != "" {
		summary = append(summary, frame.KV{Label: "Project", Value: userhome.Shorten(in.Scope.Dir)})
	}
	summary = append(summary,
		frame.KV{Label: "Printers", Value: printersSummary(in.Printers)},
		frame.KV{Label: "Tool preset", Value: string(in.Preset)},
		frame.KV{Label: "AI clients", Value: clients},
	)
	out = append(out, frame.Rows(w, labelW, summary...)...)

	if len(lines) > 0 {
		var kvs []frame.KV
		kvs = append(kvs, frame.KV{})
		for _, l := range lines {
			kvs = append(kvs, frame.KV{Label: l.Name, Value: l.Status, Style: l.Tone.style()})
		}
		out = append(out, frame.Rows(w, labelW, kvs...)...)
	}

	shown := make(map[harness.ID]bool, len(lines))
	for _, l := range lines {
		shown[l.ID] = true
	}
	others := 0
	for _, h := range in.Detections {
		if h.Relevant() && !shown[h.ID] {
			others++
		}
	}
	if others > 0 {
		noun := "other clients were"
		if others == 1 {
			noun = "other client was"
		}
		out = append(out, "", frame.Gutter+frame.StyleDim.Render(fmt.Sprintf("%d %s not selected.", others, noun)))
	}

	if reloads := reloadLines(in); len(reloads) > 0 {
		out = append(out, "")
		for _, r := range reloads {
			out = append(out, frame.Wrap(w, frame.Gutter, r)...)
		}
	}

	out = append(out, "")
	bin := domain.BinaryName
	switch {
	case in.Failed:
		out = append(out, frame.Wrap(w, frame.Gutter, fmt.Sprintf("Fix the problem above and run `%s %s` again.", bin, in.command()))...)
	case in.DryRun:
		out = append(out, frame.Wrap(w, frame.Gutter, fmt.Sprintf("Run `%s %s` without --dry-run to apply this.", bin, in.command()))...)
	default:
		out = append(out, frame.Wrap(w, frame.Gutter, fmt.Sprintf("Next: run `%s` to open the app, or ask your AI \"what is my printer doing?\".", bin))...)
		out = append(out, frame.Wrap(w, frame.Gutter, fmt.Sprintf("Change any of this later with `%s %s`.", bin, in.command()))...)
	}
	return out
}

// reloadLines is one "<Client>: <reload hint>" line per client whose
// configuration this run changed (an Applied result), with the hint taken from
// the detection of that client. A dry run changed nothing, so it has none.
func reloadLines(in FinishInput) []string {
	if in.DryRun {
		return nil
	}
	byID := make(map[harness.ID]harness.Harness, len(in.Detections))
	for _, h := range in.Detections {
		byID[h.ID] = h
	}
	var out []string
	for _, r := range in.Results {
		if r.State != harness.Applied {
			continue
		}
		h, ok := byID[r.HarnessID]
		if !ok || h.ReloadHint == "" {
			continue
		}
		out = append(out, capitalize(r.Name+": "+h.ReloadHint))
	}
	return out
}

func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// CancelLine is the line printed after the program ends when setup did not
// finish. interrupted means a real run was cut off while registering the AI
// clients; wrote means step 1 or 2 saved something. command is "install" or
// "add".
func CancelLine(dryRun, wrote, interrupted bool, command string) string {
	switch {
	case dryRun:
		return "  Dry run cancelled; nothing was written."
	case interrupted:
		return fmt.Sprintf("  Registration was interrupted; run %s %s again.", domain.BinaryName, command)
	case wrote:
		return "  Setup cancelled. The printers and settings you saved were kept; no AI client was changed."
	}
	return "  Setup cancelled; nothing was written."
}

// Outcome is what the wizard ended with, for PrintOutcome.
type Outcome struct {
	DryRun bool
	Scope  harness.Scope
	// Width is the last terminal width the program saw (0 means 80).
	Width int
	// Failure is the flow's terminal error, if any.
	Failure error
	// OnApplyStep is true when the program ended on the registration step.
	OnApplyStep bool
	Printers    *PrinterState
	Settings    *SettingsState
	Clients     *installer.HarnessState
	Results     *installer.ResultsState
}

// PrintOutcome prints, after the program has ended, what the wizard ended
// with: the finish summary when registration ran to the end (also when a
// client failed, so the per-client rows are not lost), otherwise one line
// saying what was or was not written (dev_docs/tui-design-v0.4.0.md R2.6). A
// failure with nothing to show prints nothing here; the caller reports it.
// Finished means Results.Done and no Failure.
func PrintOutcome(w io.Writer, o Outcome) {
	rs := o.Results
	if rs != nil && rs.Done {
		if o.Failure != nil && len(rs.Results)+len(rs.Changes) == 0 {
			return
		}
		width := o.Width
		if width <= 0 {
			width = 80
		}
		in := NewFinishInput(o.Scope, o.DryRun, o.Failure, o.Printers, o.Settings, o.Clients, rs)
		fmt.Fprintln(w)
		for _, line := range FinishLines(width, in) {
			fmt.Fprintln(w, line)
		}
		return
	}
	if o.Failure != nil {
		return
	}
	command := "install"
	if o.Scope.IsProject() {
		command = "add"
	}
	wrote := (o.Printers != nil && o.Printers.Wrote) || (o.Settings != nil && o.Settings.Wrote)
	fmt.Fprintln(w)
	fmt.Fprintln(w, CancelLine(o.DryRun, wrote, o.OnApplyStep && !o.DryRun, command))
}
