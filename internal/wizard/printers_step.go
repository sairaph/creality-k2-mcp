package wizard

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/userhome"
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
	// Wrote is true once a real (not dry-run) save reached disk, so the cancel
	// line after the program can say what was kept.
	Wrote bool

	Message string

	cancelScan context.CancelFunc
	progressCh chan discovery.Progress

	// cancelProbe stops the add-host probe; probeGen tags each probe so the
	// result of a cancelled one is dropped when it arrives late.
	cancelProbe context.CancelFunc
	probeGen    int

	tick ticker
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
	// App is the header's first part ("creality-k2-mcp setup", or
	// "creality-k2-mcp project setup" for add); empty means the install one.
	App string
}

// PrintersStep returns a flow.Step that scans the network for Creality K2
// printers, lets the user pick which are enabled and which may be
// controlled (not just monitored), and saves the registry on enter
// (dev_docs/plan-v0.1.0.md decision 4). Discovery runs asynchronously with a
// spinner and live progress; the step owns the cursor, every key and its own
// row renderer, and draws inside the shared frame.
func PrintersStep[T any](ctx context.Context, stateFn func(*T) *PrinterState, opts PrintersStepOptions) flow.Step[T] {
	if opts.Discover == nil {
		opts.Discover = discovery.Discover
	}
	if opts.Probe == nil {
		opts.Probe = discovery.ProbeHost
	}
	return &printersStep[T]{stateFn: stateFn, opts: opts, ctx: ctx, chrome: newChrome(opts.App, opts.DryRun)}
}

type printersStep[T any] struct {
	stateFn func(*T) *PrinterState
	opts    PrintersStepOptions
	ctx     context.Context
	chrome  chrome
}

func (s *printersStep[T]) ID() string { return "printers" }

func (s *printersStep[T]) Title(state *T) string {
	if s.opts.Dir != "" {
		return fmt.Sprintf("Register in this project (%s): which Creality K2 printers should the AI use?", userhome.Shorten(s.opts.Dir))
	}
	return "Which Creality K2 printers should the AI be able to use?"
}

func (s *printersStep[T]) Hints(state *T) []struct{ Key, Label string } {
	ps := s.get(state)
	if ps == nil {
		return nil
	}
	return legacyHints(printersHints(ps))
}

// printersHints is the one key list both Hints and the footer use, so they
// cannot drift apart. Priorities decide what a narrow footer drops first:
// rescan, the scan stop, move, add, control; "enter continue" and "q cancel"
// stay longest. The first step has no back hint, and "esc stop scan" shows
// only while a scan runs.
func printersHints(ps *PrinterState) []frame.Hint {
	switch {
	case !ps.Ready:
		return []frame.Hint{frame.Cancel()}
	case ps.Saving:
		return busyHints("saving...")
	case ps.Probing:
		return []frame.Hint{
			{Label: "probing...", Priority: 99},
			{Keys: "esc", Label: "cancel", Priority: frame.PriorityBack},
			frame.Cancel(),
		}
	case ps.Adding:
		return []frame.Hint{
			{Keys: "enter", Label: "probe", Priority: 99},
			{Keys: "esc", Label: "cancel", Priority: frame.PriorityBack},
		}
	}
	hints := []frame.Hint{
		{Keys: "↑↓", Label: "move", Priority: 60},
		{Keys: "space", Label: "enable", Priority: 90},
		{Keys: "c", Label: "control", Priority: 80},
		{Keys: "m", Label: "add host", Priority: 70},
	}
	if !ps.Scanning {
		hints = append(hints, frame.Hint{Keys: "r", Label: "rescan", Priority: 50})
	}
	hints = append(hints, frame.Hint{Keys: "enter", Label: "continue", Priority: 99})
	if ps.Scanning {
		hints = append(hints, frame.Hint{Keys: "esc", Label: "stop scan", Priority: 55})
	}
	return append(hints, frame.Cancel())
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
	gen    int
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
	ps.tick.reset()
	return tea.Batch(
		ps.tick.start(),
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
		if smallDrops(baseStateOf(state), m.String(), ps.Adding && !ps.Probing) {
			return flow.Continue, nil
		}
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
		if ps.Probing {
			return s.updateProbing(m, ps)
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
		return flow.Continue, ps.tick.next(!ps.Ready || ps.Scanning || ps.Probing || ps.Saving)
	}
	return flow.Continue, nil
}

// updateProbing handles a key while the add-host probe runs. The probe writes
// nothing, so it is never Busy: esc cancels it and stays in the field, q
// cancels the probe and the setup, ctrl+c quits; every other key is ignored.
func (s *printersStep[T]) updateProbing(m tea.KeyMsg, ps *PrinterState) (flow.Directive, tea.Cmd) {
	switch m.String() {
	case "esc":
		s.stopProbe(ps)
		ps.Message = ""
	case "q", "ctrl+c":
		s.stopProbe(ps)
		if ps.cancelScan != nil {
			ps.cancelScan()
		}
		return flow.Quit, nil
	}
	return flow.Continue, nil
}

// stopProbe cancels the running probe and invalidates its result.
func (s *printersStep[T]) stopProbe(ps *PrinterState) {
	if ps.cancelProbe != nil {
		ps.cancelProbe()
		ps.cancelProbe = nil
	}
	ps.probeGen++
	ps.Probing = false
}

func (s *printersStep[T]) handleProbeDone(ps *PrinterState, m probeDoneMsg) (flow.Directive, tea.Cmd) {
	if m.gen != ps.probeGen || !ps.Probing {
		return flow.Continue, nil
	}
	ps.Probing = false
	if ps.cancelProbe != nil {
		ps.cancelProbe()
		ps.cancelProbe = nil
	}
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
		ps.Message = fmt.Sprintf("Dry run: would write %d printer(s) to %s", len(m.reg.Printers), userhome.Shorten(m.path))
	} else {
		ps.Message = ""
		ps.Wrote = true
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
		if ps.cancelScan != nil {
			ps.cancelScan()
		}
		ps.Saving = true
		ps.Message = ""
		return flow.Continue, tea.Batch(ps.tick.start(), s.saveCmd(ps))
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
		return flow.Continue, tea.Batch(ps.tick.start(), s.probeCmd(ps, host))
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

	return tea.Batch(ps.tick.start(), scanCmd, s.watchProgress(ps))
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
	if ps.cancelProbe != nil {
		ps.cancelProbe()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	ps.cancelProbe = cancel
	ps.probeGen++
	gen := ps.probeGen
	return func() tea.Msg {
		_, merged, err := ProbeAndMerge(ctx, probe, host, 0, reg)
		if err != nil {
			return probeDoneMsg{gen: gen, err: err}
		}
		return probeDoneMsg{gen: gen, merged: merged}
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
	base := baseStateOf(state)
	return s.chrome.draw(base, "Printers", 1, printersHints(ps), func(w, rows int) []string {
		return s.body(ps, spinnerFrame(base), w, rows)
	})
}

// body is the step's screen between the header and the footer: the question,
// what is happening (loading, scanning, saving), the printer rows and the
// notes under them. Only the rows scroll, so the notes stay visible.
func (s *printersStep[T]) body(ps *PrinterState, spin, w, rows int) []string {
	glyph := frame.Spinner(spin)

	if !ps.Ready {
		return append(questionLines(w, s.Title(nil)), frame.Gutter+glyph+" Loading the printer registry...")
	}

	if ps.Adding {
		out := questionLines(w, "Host name or IP address of the printer:")
		if ps.Input == "" {
			out = append(out, frame.Gutter+frame.StyleDim.Render("e.g. 192.168.1.50")+"_")
		} else {
			out = append(out, frame.Gutter+ps.Input+"_")
		}
		if ps.Probing {
			out = append(out, "", frame.Gutter+glyph+" Probing...")
		}
		return append(out, noteLines(w, ps.Message)...)
	}

	out := questionLines(w, s.Title(nil))
	switch {
	case ps.Saving:
		verb := "Saving the printer registry..."
		if s.opts.DryRun {
			verb = "Computing what would be written..."
		}
		out = append(out, frame.Gutter+glyph+" "+verb, "")
	case ps.Scanning && ps.Total > 0:
		out = append(out, fmt.Sprintf("%s%s Scanning the network... %d/%d hosts, %d found", frame.Gutter, glyph, ps.Scanned, ps.Total, ps.Found), "")
	case ps.Scanning:
		out = append(out, frame.Gutter+glyph+" Scanning the network...", "")
	}

	var tail []string
	if len(ps.Rows) == 0 && !ps.Scanning {
		tail = append(tail, frame.Wrap(w, frame.Gutter, "No printers found yet. Press m to add one by its address, or r to scan again.")...)
	}
	if ps.ScanErr != "" {
		tail = append(tail, "")
		for _, line := range frame.Wrap(w, frame.Gutter, "Scan error: "+ps.ScanErr) {
			tail = append(tail, frame.StyleError.Render(line))
		}
	}
	if ps.Partial && !ps.Scanning {
		tail = append(tail, "")
		for _, line := range frame.Wrap(w, frame.Gutter, "The last scan did not finish within its time budget; press r to scan again.") {
			tail = append(tail, frame.StyleWarn.Render(line))
		}
	}
	tail = append(tail, noteLines(w, ps.Message)...)

	avail := max(rows-len(out)-len(tail), 1)
	out = append(out, listWindow(printerBlocks(ps, w), ps.Cursor, avail)...)
	return append(out, tail...)
}

// noteLines is a blank row and then msg wrapped in the attention colour; it
// is empty when there is no message.
func noteLines(w int, msg string) []string {
	if msg == "" {
		return nil
	}
	out := []string{""}
	for _, line := range frame.Wrap(w, frame.Gutter, msg) {
		out = append(out, frame.StyleWarn.Render(line))
	}
	return out
}

// printerBlocks renders one block of lines per row: the marker, the enabled
// glyph, the name, the host, the control state and where the entry came from;
// on a narrow terminal the origin goes first, then the control state. A row
// that cannot be added gets its reason on a second, dim line.
func printerBlocks(ps *PrinterState, w int) [][]string {
	nameW, hostW := 0, 0
	for _, row := range ps.Rows {
		nameW = max(nameW, ansi.StringWidth(displayName(row)))
		hostW = max(hostW, ansi.StringWidth(row.Printer.Host))
	}
	nameW = min(nameW, 28)

	blocks := make([][]string, len(ps.Rows))
	for i, row := range ps.Rows {
		marker := frame.Marker(i == ps.Cursor)
		name := frame.Pad(ansi.Truncate(displayName(row), nameW, "~"), nameW)
		if !row.Addable {
			blocks[i] = []string{marker + frame.StyleDim.Render("-") + " " + name}
			for _, line := range frame.Wrap(w, frame.Gutter+"    ", row.Reason) {
				blocks[i] = append(blocks[i], frame.StyleDim.Render(line))
			}
			continue
		}
		glyph := frame.StyleDim.Render("○")
		if row.Printer.Enabled {
			glyph = frame.StyleOK.Render("●")
		}
		control := frame.StyleDim.Render("control off")
		if row.Printer.AllowControl {
			control = frame.StyleWarn.Render("control on ")
		}
		origin := frame.StyleDim.Render("registered")
		if !row.Existing {
			origin = frame.StyleDim.Render("new")
		}
		line := marker + glyph + " " + name + "  " + frame.Pad(row.Printer.Host, hostW)
		for _, part := range []string{control, origin} {
			if ansi.StringWidth(line+"  "+part) > w-1 {
				break
			}
			line += "  " + part
		}
		blocks[i] = []string{line}
	}
	return blocks
}

// --- free helpers (own the cursor and keys; printerBlocks only renders) ---

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
