package wizard

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality_k2_mcp/internal/discovery"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// PrinterState is embedded in consumer state for the Printers step,
// following installer.HarnessState's pattern.
type PrinterState struct {
	// Dir is the directory SaveSelection resolves the registry against: ""
	// for the global install wizard, the project directory for "add".
	Dir      string
	Registry domain.Registry
	Path     string
	Rows     []PrinterRow
	Cursor   int
	// Ready is true once the registry has been loaded, so the step can
	// render the list and accept keys.
	Ready bool

	Scanning bool
	Scanned  int
	Total    int
	Found    int
	Partial  bool
	ScanErr  string

	Adding  bool
	Input   string
	Probing bool

	// Saving is true while the save (or, in dry-run, the write preview) that
	// "enter" started is in flight, mirroring installer.ApplyStep's Done
	// gating so a save runs as a tea.Cmd instead of blocking Update.
	Saving bool

	Message string

	cancelScan context.CancelFunc
	progressCh chan discovery.Progress
}

// PrintersStepOptions controls the Printers step.
type PrintersStepOptions struct {
	// Dir selects the registry scope: "" for global, a project directory
	// for a per-project registry (mirrors installer.HarnessStepOptions.Scope,
	// but the wizard package deliberately does not depend on the harness
	// package for this one string).
	Dir string
	// Discover runs a scan. Nil defaults to discovery.Discover.
	Discover DiscoverFunc
	// Probe identifies one manually entered host. Nil defaults to
	// discovery.ProbeHost.
	Probe ProbeFunc
	// DryRun computes what "enter" would save and shows it instead of writing
	// the registry, matching installer.ApplyStepOptions.DryRun.
	DryRun bool
}

// PrintersStep returns a flow.Step that scans the network for Creality K2
// printers, lets the user pick which are enabled and which may be
// controlled (not just monitored), and saves the registry on enter
// (dev_docs/plan-v0.1.0.md decision 4). Discovery runs asynchronously with a
// spinner and live progress; tui.CheckboxList is only a renderer, so this
// step owns the cursor and every key itself, the same way HarnessStep does.
func PrintersStep[T any](ctx context.Context, stateFn func(*T) *PrinterState, opts PrintersStepOptions) flow.Step[T] {
	if opts.Discover == nil {
		opts.Discover = discovery.Discover
	}
	if opts.Probe == nil {
		opts.Probe = discovery.ProbeHost
	}
	return &printersStep[T]{stateFn: stateFn, opts: opts, ctx: ctx}
}

type printersStep[T any] struct {
	stateFn func(*T) *PrinterState
	opts    PrintersStepOptions
	ctx     context.Context
}

func (s *printersStep[T]) ID() string { return "printers" }

func (s *printersStep[T]) Title(state *T) string {
	if s.opts.Dir != "" {
		return fmt.Sprintf("Printers - register in this project (%s), which Creality K2 printers should the AI use?", s.opts.Dir)
	}
	return "Printers - which Creality K2 printers should the AI be able to use?"
}

func (s *printersStep[T]) Hints(state *T) []struct{ Key, Label string } {
	ps := s.get(state)
	if ps == nil {
		return nil
	}
	if ps.Saving {
		return nil // the save is in flight and cannot be cancelled
	}
	if ps.Adding {
		return []struct{ Key, Label string }{
			{Key: "enter", Label: "probe"},
			{Key: "esc", Label: "cancel"},
		}
	}
	return []struct{ Key, Label string }{
		{Key: "↑↓", Label: "move"},
		{Key: "space", Label: "enabled"},
		{Key: "c", Label: "control"},
		{Key: "m", Label: "add host"},
		{Key: "r", Label: "rescan"},
		{Key: "enter", Label: "save & continue"},
		{Key: "esc", Label: "stop scan"},
		{Key: "q", Label: "cancel"},
	}
}

func (s *printersStep[T]) get(state *T) *PrinterState {
	if s.stateFn == nil {
		return nil
	}
	return s.stateFn(state)
}

// --- messages ---

type registryLoadedMsg struct {
	reg  domain.Registry
	path string
	err  error
}

type scanProgressMsg discovery.Progress

type scanDoneMsg struct {
	report discovery.Report
	merged []discovery.MergeResult
	err    error
}

type probeDoneMsg struct {
	merged discovery.MergeResult
	err    error
}

// printersSavedMsg is what the "enter" save (or, in dry-run, the write
// preview) produces, following installer.ApplyStep's appliedMsg pattern.
type printersSavedMsg struct {
	reg    domain.Registry
	path   string
	dryRun bool
	err    error
}

// --- Init / Update / View ---

func (s *printersStep[T]) Init(state *T) tea.Cmd {
	ps := s.get(state)
	if ps == nil {
		return nil
	}
	ps.Dir = s.opts.Dir
	dir := s.opts.Dir
	return tea.Batch(
		tui.Spinner(),
		func() tea.Msg {
			reg, path, _, err := domain.LoadRegistry(dir)
			return registryLoadedMsg{reg: reg, path: path, err: err}
		},
	)
}

func (s *printersStep[T]) Update(msg tea.Msg, state *T) (flow.Directive, tea.Cmd) {
	ps := s.get(state)
	if ps == nil {
		return flow.Fail, nil
	}

	switch m := msg.(type) {
	case registryLoadedMsg:
		if m.err != nil {
			if base := baseStateOf(state); base != nil {
				base.Failure = m.err
			}
			return flow.Fail, nil
		}
		ps.Registry = m.reg
		ps.Path = m.path
		ps.Rows = BuildRows(m.reg, nil, nil)
		ps.Ready = true
		return flow.Continue, s.startScan(ps)

	case scanProgressMsg:
		ps.Scanned, ps.Total, ps.Found = m.Scanned, m.Total, m.Found
		return flow.Continue, s.watchProgress(ps)

	case scanDoneMsg:
		ps.Scanning = false
		ps.cancelScan = nil
		ps.progressCh = nil
		if m.err != nil {
			ps.ScanErr = m.err.Error()
			return flow.Continue, nil
		}
		ps.ScanErr = ""
		ps.Partial = m.report.Partial
		ps.Rows = BuildRows(ps.Registry, m.merged, ps.Rows)
		if ps.Cursor >= len(ps.Rows) {
			ps.Cursor = 0
		}
		return flow.Continue, nil

	case probeDoneMsg:
		return s.handleProbeDone(ps, m)

	case printersSavedMsg:
		return s.handleSaved(ps, m)

	case tea.KeyMsg:
		if !ps.Ready {
			if m.String() == "q" || m.String() == "ctrl+c" {
				return flow.Quit, nil
			}
			return flow.Continue, nil
		}
		if ps.Saving {
			if m.String() == "ctrl+c" {
				return flow.Quit, nil
			}
			return flow.Continue, nil
		}
		if ps.Adding {
			return s.updateAdding(m, ps)
		}
		return s.updateList(m, ps)
	}

	if tui.IsSpinMsg(msg) {
		if base := baseStateOf(state); base != nil {
			base.Spinner.Frame++
		}
		if !ps.Ready || ps.Scanning || ps.Probing || ps.Saving {
			return flow.Continue, tui.Spinner()
		}
	}
	return flow.Continue, nil
}

func (s *printersStep[T]) handleProbeDone(ps *PrinterState, m probeDoneMsg) (flow.Directive, tea.Cmd) {
	ps.Probing = false
	if m.err != nil {
		ps.Message = m.err.Error()
		return flow.Continue, nil
	}
	ps.Adding = false
	ps.Input = ""
	ps.Rows = MergeRow(ps.Rows, m.merged)
	row := freshRowFromMerge(m.merged)
	for i := range ps.Rows {
		if ps.Rows[i].key() == row.key() {
			ps.Cursor = i
			break
		}
	}
	if row.Addable {
		ps.Message = ""
	} else {
		ps.Message = row.Reason
	}
	return flow.Continue, nil
}

// handleSaved applies the result of saveCmd: a real write's outcome, or a
// dry run's preview, following installer.ApplyStep's appliedMsg handling.
func (s *printersStep[T]) handleSaved(ps *PrinterState, m printersSavedMsg) (flow.Directive, tea.Cmd) {
	ps.Saving = false
	if m.err != nil {
		ps.Message = m.err.Error()
		return flow.Continue, nil
	}
	ps.Registry = m.reg
	ps.Path = m.path
	if m.dryRun {
		ps.Message = fmt.Sprintf("Dry run: would write %d printer(s) to %s", len(m.reg.Printers), m.path)
	} else {
		ps.Message = ""
	}
	return flow.Next, nil
}

func (s *printersStep[T]) updateList(m tea.KeyMsg, ps *PrinterState) (flow.Directive, tea.Cmd) {
	switch m.String() {
	case "q", "ctrl+c":
		if ps.cancelScan != nil {
			ps.cancelScan()
		}
		return flow.Quit, nil
	case "up", "k":
		MoveCursor(ps, -1)
	case "down", "j":
		MoveCursor(ps, 1)
	case " ":
		ToggleEnabled(ps)
	case "c":
		ToggleControl(ps)
	case "m":
		ps.Adding = true
		ps.Input = ""
		ps.Message = ""
	case "r":
		if !ps.Scanning {
			return flow.Continue, s.startScan(ps)
		}
	case "enter":
		ps.Saving = true
		ps.Message = ""
		return flow.Continue, s.saveCmd(ps)
	case "esc":
		if ps.Scanning {
			if ps.cancelScan != nil {
				ps.cancelScan()
			}
			ps.Message = "Stopping scan..."
			return flow.Continue, nil
		}
		return flow.Back, nil
	}
	return flow.Continue, nil
}

func (s *printersStep[T]) updateAdding(m tea.KeyMsg, ps *PrinterState) (flow.Directive, tea.Cmd) {
	switch m.String() {
	case "ctrl+c":
		return flow.Quit, nil
	case "esc":
		ps.Adding = false
		ps.Input = ""
		ps.Message = ""
	case "enter":
		host := strings.TrimSpace(ps.Input)
		if host == "" {
			ps.Message = "enter a host name or IP address"
			return flow.Continue, nil
		}
		if err := domain.ValidateHost(host); err != nil {
			ps.Message = err.Error()
			return flow.Continue, nil
		}
		ps.Probing = true
		ps.Message = ""
		return flow.Continue, tea.Batch(tui.Spinner(), s.probeCmd(ps, host))
	case "backspace":
		if len(ps.Input) > 0 {
			r := []rune(ps.Input)
			ps.Input = string(r[:len(r)-1])
		}
	default:
		if len(m.Runes) > 0 {
			ps.Input += string(m.Runes)
		}
	}
	return flow.Continue, nil
}

// startScan launches an async scan: opts.Discover runs off the main loop and
// reports live progress on a channel, which watchProgress turns into
// scanProgressMsg values so the spinner step shows a running count instead
// of just spinning blind. Esc cancels ctx, and discovery.Scan (through
// discovery.Discover) returns promptly with whatever it found so far
// (Report.Partial true), matching "esc stops a running scan keeping
// results".
func (s *printersStep[T]) startScan(ps *PrinterState) tea.Cmd {
	ctx, cancel := context.WithCancel(s.ctx)
	ps.cancelScan = cancel
	ps.Scanning = true
	ps.Scanned, ps.Total, ps.Found = 0, 0, 0
	ps.ScanErr = ""
	ps.Message = ""

	ch := make(chan discovery.Progress, 32)
	ps.progressCh = ch

	reg := ps.Registry
	discover := s.opts.Discover
	scanCmd := func() tea.Msg {
		defer close(ch)
		opts := discovery.Options{
			ScanOptions: discovery.ScanOptions{
				Progress: func(p discovery.Progress) {
					select {
					case ch <- p:
					default:
					}
				},
			},
		}
		report, merged, err := ScanAndMerge(ctx, discover, opts, reg, true)
		if err != nil {
			return scanDoneMsg{err: err}
		}
		return scanDoneMsg{report: report, merged: merged}
	}

	return tea.Batch(tui.Spinner(), scanCmd, s.watchProgress(ps))
}

func (s *printersStep[T]) watchProgress(ps *PrinterState) tea.Cmd {
	ch := ps.progressCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return nil
		}
		return scanProgressMsg(p)
	}
}

func (s *printersStep[T]) probeCmd(ps *PrinterState, host string) tea.Cmd {
	probe := s.opts.Probe
	reg := ps.Registry
	ctx := s.ctx
	return func() tea.Msg {
		_, merged, err := ProbeAndMerge(ctx, probe, host, 0, reg)
		if err != nil {
			return probeDoneMsg{err: err}
		}
		return probeDoneMsg{merged: merged}
	}
}

// saveCmd runs the "enter" save off Update: in dry-run it only builds the
// registry that would be written (registryFromRows) and resolves where, in
// the same shape installer.ApplyStep's Init uses PlanResultsIn instead of
// ApplyIn; otherwise it calls SaveSelection exactly as Update used to.
func (s *printersStep[T]) saveCmd(ps *PrinterState) tea.Cmd {
	dir := ps.Dir
	rows := ps.Rows
	dryRun := s.opts.DryRun
	return func() tea.Msg {
		if dryRun {
			path, _, err := domain.RegistryPath(dir)
			if err != nil {
				return printersSavedMsg{err: err}
			}
			return printersSavedMsg{reg: registryFromRows(rows), path: path, dryRun: true}
		}
		reg, path, err := SaveSelection(dir, rows)
		if err != nil {
			return printersSavedMsg{err: err}
		}
		return printersSavedMsg{reg: reg, path: path}
	}
}

func (s *printersStep[T]) View(state *T) string {
	ps := s.get(state)
	if ps == nil {
		return ""
	}

	frame := 0
	if base := baseStateOf(state); base != nil {
		frame = base.Spinner.Frame
	}

	if !ps.Ready {
		return tui.Section(tui.DefaultTheme, "", fmt.Sprintf("  %s Loading the printer registry...\n", tui.SpinFrame(frame)))
	}

	var b strings.Builder

	if ps.Adding {
		b.WriteString("  Host name or IP address of the printer:\n\n")
		b.WriteString(tui.TextInput(ps.Input, "e.g. 192.168.1.50", false))
		b.WriteString("\n")
		if ps.Probing {
			fmt.Fprintf(&b, "\n  %s Probing...\n", tui.SpinFrame(frame))
		}
		if ps.Message != "" {
			b.WriteString("\n  " + ps.Message)
		}
		b.WriteString("\n" + tui.Footer(tui.DefaultTheme, tui.Hints(tui.DefaultTheme,
			tui.Hint{Key: "enter", Label: "probe"},
			tui.Hint{Key: "esc", Label: "cancel"},
		)))
		return tui.Section(tui.DefaultTheme, s.Title(state), b.String())
	}

	if ps.Scanning {
		if ps.Total > 0 {
			fmt.Fprintf(&b, "  %s Scanning the network... %d/%d hosts, %d found\n\n", tui.SpinFrame(frame), ps.Scanned, ps.Total, ps.Found)
		} else {
			fmt.Fprintf(&b, "  %s Scanning the network...\n\n", tui.SpinFrame(frame))
		}
	}

	if ps.Saving {
		verb := "Saving the printer registry..."
		if s.opts.DryRun {
			verb = "Computing what would be written..."
		}
		fmt.Fprintf(&b, "  %s %s\n\n", tui.SpinFrame(frame), verb)
	}

	items := make([]tui.CheckboxItem, len(ps.Rows))
	selected := make(map[string]bool, len(ps.Rows))
	for i, row := range ps.Rows {
		items[i] = tui.CheckboxItem{ID: row.key(), Name: displayName(row)}
		if row.Addable {
			selected[row.key()] = row.Printer.Enabled
		}
	}
	rows := ps.Rows
	selectable := func(i int) bool {
		if i < 0 || i >= len(rows) {
			return false
		}
		return rows[i].Addable
	}
	statusFn := func(i int) string {
		if i < 0 || i >= len(rows) {
			return ""
		}
		row := rows[i]
		if !row.Addable {
			return row.Reason
		}
		control := "off"
		if row.Printer.AllowControl {
			control = "on"
		}
		origin := "registered"
		if !row.Existing {
			origin = "new"
		}
		return fmt.Sprintf("%s  control: %-3s  %s", row.Printer.Host, control, origin)
	}

	b.WriteString(tui.CheckboxList(tui.DefaultTheme, items, ps.Cursor, selected, selectable, statusFn, true, 0))

	if len(ps.Rows) == 0 && !ps.Scanning {
		b.WriteString("  No printers found. Press m to add one by host name or IP, or r to rescan.\n")
	}
	if ps.ScanErr != "" {
		b.WriteString("\n  scan error: " + ps.ScanErr)
	}
	if ps.Partial && !ps.Scanning {
		b.WriteString("\n  The last scan did not finish within its time budget; press r to scan again.")
	}
	if ps.Message != "" {
		b.WriteString("\n\n  " + ps.Message)
	}

	b.WriteString("\n" + tui.Footer(tui.DefaultTheme, tui.Hints(tui.DefaultTheme,
		tui.Hint{Key: "↑↓", Label: "move"},
		tui.Hint{Key: "space", Label: "enabled"},
		tui.Hint{Key: "c", Label: "control"},
		tui.Hint{Key: "m", Label: "add host"},
		tui.Hint{Key: "r", Label: "rescan"},
		tui.Hint{Key: "enter", Label: "save & continue"},
		tui.Hint{Key: "esc", Label: "stop scan"},
		tui.Hint{Key: "q", Label: "cancel"},
	)))
	return tui.Section(tui.DefaultTheme, s.Title(state), b.String())
}

// --- free helpers (own the cursor and keys; tui.CheckboxList only renders) ---

// CurrentRow returns a pointer to the row under the cursor, or nil.
func CurrentRow(ps *PrinterState) *PrinterRow {
	if ps == nil || ps.Cursor < 0 || ps.Cursor >= len(ps.Rows) {
		return nil
	}
	return &ps.Rows[ps.Cursor]
}

// MoveCursor moves the cursor dir rows through ps.Rows, wrapping at either
// end.
func MoveCursor(ps *PrinterState, dir int) {
	if ps == nil || len(ps.Rows) == 0 {
		return
	}
	ps.Cursor = ((ps.Cursor+dir)%len(ps.Rows) + len(ps.Rows)) % len(ps.Rows)
}

// ToggleEnabled flips Enabled on the row under the cursor. A not-addable row
// is never toggled. Disabling a row also clears AllowControl: control is
// only ever meaningful for a printer the AI is allowed to use at all
// (safety-architecture 3.4), so a disabled printer never keeps a stale
// control grant it would carry back if re-enabled without the user
// noticing.
func ToggleEnabled(ps *PrinterState) {
	row := CurrentRow(ps)
	if row == nil || !row.Addable {
		return
	}
	row.Printer.Enabled = !row.Printer.Enabled
	if !row.Printer.Enabled {
		row.Printer.AllowControl = false
	}
}

// ToggleControl flips AllowControl on the row under the cursor. A
// not-addable row is never toggled, and neither is a disabled one: allowing
// control on a printer the AI cannot even use yet would be invisible in the
// list (the control column only means something once the row is enabled) and
// SaveSelection would clear it again anyway, so refusing here and saying why
// is clearer than a toggle that silently does not stick.
func ToggleControl(ps *PrinterState) {
	row := CurrentRow(ps)
	if row == nil || !row.Addable {
		return
	}
	if !row.Printer.Enabled {
		ps.Message = "enable the printer before allowing control"
		return
	}
	row.Printer.AllowControl = !row.Printer.AllowControl
}

// baseStateOf returns the embedded *flow.BaseState when the consumer state
// exposes one, mirroring installer's own unexported helper of the same
// name.
func baseStateOf[T any](state *T) *flow.BaseState {
	if state == nil {
		return nil
	}
	if b, ok := any(state).(interface{ GetBaseState() *flow.BaseState }); ok {
		return b.GetBaseState()
	}
	return nil
}
