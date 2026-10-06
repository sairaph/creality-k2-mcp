package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/wizard"
)

// printersReachTimeout bounds the per-printer reachability probe the
// Printers screen runs to show the REACHABLE/STATE columns, matching
// internal/clicmd's own "printers" list command (printersListTimeout).
const printersReachTimeout = 5 * time.Second

// printersReachConcurrency bounds how many printers are probed at once,
// matching internal/clicmd's printersListConcurrency.
const printersReachConcurrency = 4

// controlWarningText explains what allow_control grants, shown in the
// confirmation before turning it on. Matches internal/clicmd's own
// controlWarning wording (dev_docs/plan-v0.1.0.md's "Control" tool group).
const controlWarningText = "Control lets the AI change this printer through the MCP tools: start, pause, " +
	"resume and cancel prints; upload and delete gcode files; set nozzle and bed temperature; fans, speed " +
	"and flow factors; the chamber light; and exclude objects. Only allow this for a printer, and an AI " +
	"client, you trust with those actions."

type printersMode int

const (
	printersModeNormal printersMode = iota
	printersModeRenaming
	printersModeConfirmRemove
	printersModeConfirmControl
)

// reachInfo is one printer's reachability, gathered the same way
// internal/clicmd's "printers" list command does: printerstate.Take plus
// DeriveActivityState, reachable meaning snap.ServerInfoErr == nil.
type reachInfo struct {
	reachable bool
	state     string
}

// printersScreen is the Printers management screen
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section): list with enabled,
// control, reachability and state; rescan, add by host, enable/disable,
// allow/deny control, rename, remove. Every mutation is built on
// internal/wizard's exported registry merge/save functions, the same ones
// the install wizard and "printers" one-shot command use, so the three
// surfaces never disagree about what a save means; only the key handling
// and rendering here are specific to this screen.
type printersScreen struct {
	ctx  context.Context
	deps Deps

	ps         wizard.PrinterState
	loadErr    string
	cancelScan context.CancelFunc
	progressCh chan discovery.Progress

	// cancelProbe stops the add-host probe; probeGen tags each probe so the
	// result of a cancelled one is dropped when it arrives late.
	cancelProbe context.CancelFunc
	probeGen    int

	reach map[string]reachInfo

	mode        printersMode
	renameInput string
	confirmYes  bool

	list  selectList
	spin  spinner
	lastH int
}

func newPrintersScreen(ctx context.Context, deps Deps) *printersScreen {
	return &printersScreen{ctx: ctx, deps: deps, reach: map[string]reachInfo{}, spin: newSpinner()}
}

// --- messages ---

type printersRegistryLoadedMsg struct {
	reg  domain.Registry
	path string
	err  error
}

type printersScanProgressMsg discovery.Progress

type printersScanDoneMsg struct {
	report discovery.Report
	merged []discovery.MergeResult
	err    error
}

type printersProbeDoneMsg struct {
	gen    int
	merged discovery.MergeResult
	err    error
}

type printersSavedMsg struct {
	reg  domain.Registry
	path string
	err  error
}

type printersReachMsg map[string]reachInfo

// --- Init / Update ---

func (s *printersScreen) Init() tea.Cmd {
	return tea.Batch(s.loadCmd(), s.spin.ensure(true))
}

func (s *printersScreen) loadCmd() tea.Cmd {
	dir := s.deps.Dir
	return func() tea.Msg {
		reg, path, _, err := domain.LoadRegistry(dir)
		return printersRegistryLoadedMsg{reg: reg, path: path, err: err}
	}
}

// Close cancels a scan and an add-host probe still running, so q and esc work
// at any moment.
func (s *printersScreen) Close() {
	if s.cancelScan != nil {
		s.cancelScan()
	}
	if s.cancelProbe != nil {
		s.cancelProbe()
	}
}

func (s *printersScreen) spinning() bool {
	return !s.ps.Ready || s.ps.Scanning || s.ps.Saving || s.ps.Probing
}

func (s *printersScreen) Update(msg tea.Msg) (tea.Cmd, Nav) {
	cmd := s.update(msg)
	return tea.Batch(cmd, s.spin.ensure(s.spinning())), NavNone
}

func (s *printersScreen) update(msg tea.Msg) tea.Cmd {
	if handled, cmd := s.spin.update(msg, s.spinning()); handled {
		return cmd
	}
	switch m := msg.(type) {
	case printersRegistryLoadedMsg:
		s.ps.Ready = true
		if m.err != nil {
			s.loadErr = m.err.Error()
			return nil
		}
		s.loadErr = ""
		s.ps.Dir = s.deps.Dir
		s.ps.Registry = m.reg
		s.ps.Path = m.path
		s.ps.Rows = wizard.BuildRows(m.reg, nil, nil)
		return s.reachCmd()

	case printersScanProgressMsg:
		s.ps.Scanned, s.ps.Total, s.ps.Found = m.Scanned, m.Total, m.Found
		return s.watchProgress()

	case printersScanDoneMsg:
		s.ps.Scanning = false
		s.cancelScan = nil
		s.progressCh = nil
		if m.err != nil {
			s.ps.ScanErr = m.err.Error()
			return nil
		}
		s.ps.ScanErr = ""
		s.ps.Partial = m.report.Partial
		s.ps.Rows = wizard.BuildRows(s.ps.Registry, m.merged, s.ps.Rows)
		if s.ps.Cursor >= len(s.ps.Rows) {
			s.ps.Cursor = 0
		}
		return s.reachCmd()

	case printersProbeDoneMsg:
		return s.handleProbeDone(m)

	case printersSavedMsg:
		s.ps.Saving = false
		if m.err != nil {
			s.ps.Message = m.err.Error()
			return nil
		}
		s.ps.Registry = m.reg
		s.ps.Path = m.path
		return s.reachCmd()

	case printersReachMsg:
		for k, v := range m {
			s.reach[k] = v
		}
		return nil

	case tea.KeyMsg:
		return s.handleKey(m)
	}
	return nil
}

func (s *printersScreen) handleKey(m tea.KeyMsg) tea.Cmd {
	// A probe writes nothing and is not Busy: the root handles q and esc, every
	// other key is swallowed until it finishes or is cancelled.
	if !s.ps.Ready || s.ps.Saving || s.ps.Probing {
		return nil
	}
	if s.loadErr != "" {
		if m.String() == "r" {
			s.ps.Ready, s.loadErr = false, ""
			return s.loadCmd()
		}
		return nil
	}
	if s.ps.Adding {
		return s.updateAdding(m)
	}
	switch s.mode {
	case printersModeRenaming:
		return s.updateRenaming(m)
	case printersModeConfirmRemove:
		return s.updateConfirmRemove(m)
	case printersModeConfirmControl:
		return s.updateConfirmControl(m)
	default:
		return s.updateNormal(m)
	}
}

func (s *printersScreen) updateNormal(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "up", "k":
		wizard.MoveCursor(&s.ps, -1)
	case "down", "j":
		wizard.MoveCursor(&s.ps, 1)
	case " ":
		row := wizard.CurrentRow(&s.ps)
		if row == nil || !row.Addable {
			return nil
		}
		wizard.ToggleEnabled(&s.ps)
		s.ps.Saving = true
		s.ps.Message = ""
		return s.saveCmd()
	case "c":
		row := wizard.CurrentRow(&s.ps)
		if row == nil || !row.Addable {
			return nil
		}
		if !row.Printer.Enabled {
			s.ps.Message = "Enable the printer before allowing control."
			return nil
		}
		if !row.Printer.AllowControl {
			s.mode = printersModeConfirmControl
			s.confirmYes = false
			return nil
		}
		wizard.ToggleControl(&s.ps)
		s.ps.Saving = true
		s.ps.Message = ""
		return s.saveCmd()
	case "m":
		s.ps.Adding = true
		s.ps.Input = ""
		s.ps.Message = ""
	case "n":
		row := wizard.CurrentRow(&s.ps)
		if row == nil || !row.Addable {
			return nil
		}
		s.mode = printersModeRenaming
		s.renameInput = row.Printer.Name
	case "d", "backspace", "delete":
		row := wizard.CurrentRow(&s.ps)
		if row == nil {
			return nil
		}
		s.mode = printersModeConfirmRemove
		s.confirmYes = false
	case "r":
		if !s.ps.Scanning {
			return s.startScan()
		}
	}
	return nil
}

func (s *printersScreen) updateAdding(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "enter":
		host := strings.TrimSpace(s.ps.Input)
		if host == "" {
			s.ps.Message = "Enter a host name or IP address."
			return nil
		}
		if err := domain.ValidateHost(host); err != nil {
			s.ps.Message = err.Error()
			return nil
		}
		s.ps.Probing = true
		s.ps.Message = ""
		return s.probeCmd(host)
	case "backspace":
		if len(s.ps.Input) > 0 {
			r := []rune(s.ps.Input)
			s.ps.Input = string(r[:len(r)-1])
		}
	default:
		if len(m.Runes) > 0 {
			s.ps.Input += string(m.Runes)
		}
	}
	return nil
}

func (s *printersScreen) handleProbeDone(m printersProbeDoneMsg) tea.Cmd {
	if m.gen != s.probeGen || !s.ps.Probing {
		return nil
	}
	s.ps.Probing = false
	if s.cancelProbe != nil {
		s.cancelProbe()
		s.cancelProbe = nil
	}
	if m.err != nil {
		s.ps.Message = m.err.Error()
		return nil
	}
	s.ps.Adding = false
	s.ps.Input = ""
	s.ps.Rows = wizard.MergeRow(s.ps.Rows, m.merged)
	for i, r := range s.ps.Rows {
		if strings.EqualFold(r.Printer.Host, m.merged.Discovered.Host) {
			s.ps.Cursor = i
			break
		}
	}
	row := wizard.CurrentRow(&s.ps)
	if row == nil || !row.Addable {
		if row != nil {
			s.ps.Message = row.Reason
		}
		return nil
	}
	s.ps.Message = ""
	s.ps.Saving = true
	return s.saveCmd()
}

func (s *printersScreen) updateRenaming(m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "enter":
		name := strings.TrimSpace(s.renameInput)
		if name == "" {
			s.ps.Message = "The name must not be empty."
			s.mode = printersModeNormal
			return nil
		}
		row := wizard.CurrentRow(&s.ps)
		if row != nil {
			row.Printer.Name = name
		}
		s.mode = printersModeNormal
		s.ps.Saving = true
		s.ps.Message = ""
		return s.saveCmd()
	case "backspace":
		if len(s.renameInput) > 0 {
			r := []rune(s.renameInput)
			s.renameInput = string(r[:len(r)-1])
		}
	default:
		if len(m.Runes) > 0 {
			s.renameInput += string(m.Runes)
		}
	}
	return nil
}

func (s *printersScreen) updateConfirmRemove(m tea.KeyMsg) tea.Cmd {
	switch confirmKey(m, &s.confirmYes) {
	case confirmYes:
		return s.doRemove()
	case confirmNo:
		s.mode = printersModeNormal
	}
	return nil
}

func (s *printersScreen) doRemove() tea.Cmd {
	idx := s.ps.Cursor
	if idx < 0 || idx >= len(s.ps.Rows) {
		s.mode = printersModeNormal
		return nil
	}
	rows := make([]wizard.PrinterRow, 0, len(s.ps.Rows)-1)
	rows = append(rows, s.ps.Rows[:idx]...)
	rows = append(rows, s.ps.Rows[idx+1:]...)
	s.ps.Rows = rows
	if s.ps.Cursor >= len(s.ps.Rows) {
		s.ps.Cursor = len(s.ps.Rows) - 1
	}
	if s.ps.Cursor < 0 {
		s.ps.Cursor = 0
	}
	s.mode = printersModeNormal
	s.ps.Saving = true
	s.ps.Message = ""
	return s.saveCmd()
}

func (s *printersScreen) updateConfirmControl(m tea.KeyMsg) tea.Cmd {
	switch confirmKey(m, &s.confirmYes) {
	case confirmYes:
		return s.doAllowControl()
	case confirmNo:
		s.mode = printersModeNormal
	}
	return nil
}

func (s *printersScreen) doAllowControl() tea.Cmd {
	wizard.ToggleControl(&s.ps)
	s.mode = printersModeNormal
	s.ps.Saving = true
	s.ps.Message = ""
	return s.saveCmd()
}

// Back cancels a running probe, closes the add-host field, the rename field or
// a dialog; with none open the root leaves the screen.
func (s *printersScreen) Back() bool {
	switch {
	case s.ps.Probing:
		s.stopProbe()
		s.ps.Adding = false
		s.ps.Input = ""
		s.ps.Message = ""
		return true
	case s.ps.Adding:
		s.ps.Adding = false
		s.ps.Input = ""
		s.ps.Message = ""
		return true
	case s.mode != printersModeNormal:
		s.mode = printersModeNormal
		return true
	}
	return false
}

// stopProbe cancels the running add-host probe and invalidates its result.
func (s *printersScreen) stopProbe() {
	if s.cancelProbe != nil {
		s.cancelProbe()
		s.cancelProbe = nil
	}
	s.probeGen++
	s.ps.Probing = false
}

func (s *printersScreen) Mode() Mode {
	switch {
	case s.ps.Saving:
		return ModeBusy
	case s.ps.Probing:
		return ModeNormal
	case s.ps.Adding || s.mode == printersModeRenaming:
		return ModeTyping
	case s.mode == printersModeConfirmRemove || s.mode == printersModeConfirmControl:
		return ModeModal
	}
	return ModeNormal
}

// --- commands ---

func (s *printersScreen) saveCmd() tea.Cmd {
	dir := s.ps.Dir
	rows := s.ps.Rows
	return func() tea.Msg {
		reg, path, err := wizard.SaveSelection(dir, rows)
		if err != nil {
			return printersSavedMsg{err: err}
		}
		return printersSavedMsg{reg: reg, path: path}
	}
}

// startScan runs a scan asynchronously with live progress, matching
// internal/wizard's own Printers install step. It never saves the registry by
// itself (only an explicit action - enable, control, add, rename, remove -
// does), matching "printers scan" never changing the registry. Leaving the
// screen cancels it (Close), keeping nothing.
func (s *printersScreen) startScan() tea.Cmd {
	scanCtx, cancel := context.WithCancel(s.ctx)
	s.cancelScan = cancel
	s.ps.Scanning = true
	s.ps.Scanned, s.ps.Total, s.ps.Found = 0, 0, 0
	s.ps.ScanErr = ""
	s.ps.Message = ""

	ch := make(chan discovery.Progress, 32)
	s.progressCh = ch

	reg := s.ps.Registry
	discover := s.deps.Discover
	scanCmd := func() tea.Msg {
		defer close(ch)
		opts := discovery.Options{ScanOptions: discovery.ScanOptions{
			Progress: func(p discovery.Progress) {
				select {
				case ch <- p:
				default:
				}
			},
		}}
		report, merged, err := wizard.ScanAndMerge(scanCtx, discover, opts, reg, true)
		if err != nil {
			return printersScanDoneMsg{err: err}
		}
		return printersScanDoneMsg{report: report, merged: merged}
	}
	return tea.Batch(scanCmd, s.watchProgress())
}

func (s *printersScreen) watchProgress() tea.Cmd {
	ch := s.progressCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return nil
		}
		return printersScanProgressMsg(p)
	}
}

func (s *printersScreen) probeCmd(host string) tea.Cmd {
	if s.cancelProbe != nil {
		s.cancelProbe()
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.cancelProbe = cancel
	s.probeGen++
	gen, probe := s.probeGen, s.deps.Probe
	reg := s.ps.Registry
	return func() tea.Msg {
		_, merged, err := wizard.ProbeAndMerge(ctx, probe, host, 0, reg)
		if err != nil {
			return printersProbeDoneMsg{gen: gen, err: err}
		}
		return printersProbeDoneMsg{gen: gen, merged: merged}
	}
}

// reachCmd gathers reachability and derived state for every addable,
// registered row, the same printerstate.Take + DeriveActivityState pipeline
// internal/clicmd's "printers" list command uses (gatherPrinterViews),
// bounded to printersReachConcurrency in flight and printersReachTimeout
// per printer so one dead printer can never hang the whole screen.
func (s *printersScreen) reachCmd() tea.Cmd {
	rows := s.ps.Rows
	ctx := s.ctx
	printerClients := s.deps.PrinterClients
	return func() tea.Msg {
		result := make(map[string]reachInfo, len(rows))
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, printersReachConcurrency)
		for _, row := range rows {
			if !row.Addable || row.Printer.ID == "" {
				continue
			}
			p := row.Printer
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				pctx, cancel := context.WithTimeout(ctx, printersReachTimeout)
				defer cancel()
				snap := printerstate.Take(pctx, printerClients(p), p)
				derived := printerstate.DeriveActivityState(snap, nil)
				mu.Lock()
				result[p.ID] = reachInfo{reachable: snap.ServerInfoErr == nil, state: derived.State}
				mu.Unlock()
			}()
		}
		wg.Wait()
		return printersReachMsg(result)
	}
}

// --- view ---

func printerRowName(r wizard.PrinterRow) string {
	if r.Printer.Name != "" {
		return r.Printer.Name
	}
	return r.Printer.Host
}

func (s *printersScreen) Header() frame.Header { return frame.Header{Name: "Printers"} }

func (s *printersScreen) Hints(w, h int) []frame.Hint {
	if s.ps.Probing {
		return []frame.Hint{
			{Label: "probing...", Priority: 50},
			{Keys: "esc", Label: "cancel", Priority: frame.PriorityBack},
			frame.Quit(),
		}
	}
	switch s.Mode() {
	case ModeBusy:
		return busyHints("saving...")
	case ModeModal:
		return confirmHints()
	case ModeTyping:
		label := "confirm"
		if s.ps.Adding {
			label = "probe"
		}
		return []frame.Hint{
			{Keys: "enter", Label: label, Priority: 80},
			{Keys: "esc", Label: "cancel", Priority: frame.PriorityBack},
		}
	}
	if s.loadErr != "" {
		return []frame.Hint{{Keys: "r", Label: "retry", Priority: 80}, frame.Back(), frame.Quit()}
	}
	if !s.ps.Ready {
		return []frame.Hint{frame.Back(), frame.Quit()}
	}
	return []frame.Hint{
		{Keys: "↑↓", Label: "move", Priority: 90},
		{Keys: "space", Label: "enable", Priority: 80},
		{Keys: "c", Label: "control", Priority: 70},
		{Keys: "m", Label: "add host", Priority: 60},
		{Keys: "r", Label: "rescan", Priority: 30},
		{Keys: "n", Label: "rename", Priority: 20},
		{Keys: "d", Label: "remove", Priority: 20},
		frame.Back(), frame.Quit(),
	}
}

func (s *printersScreen) selectedName() string {
	if row := wizard.CurrentRow(&s.ps); row != nil {
		return printerRowName(*row)
	}
	return ""
}

func (s *printersScreen) Body(w, h int) []string {
	s.lastH = h
	switch {
	case s.loadErr != "":
		return wrapStyled(w, styleError, s.loadErr)
	case !s.ps.Ready:
		return []string{frame.Gutter + s.spin.glyph() + " Loading the printer registry..."}
	case s.ps.Adding:
		return s.addingBody(w)
	case s.mode == printersModeRenaming:
		return []string{frame.Gutter + "New name for " + s.selectedName() + ":", "", frame.Gutter + s.renameInput + "_"}
	case s.mode == printersModeConfirmRemove:
		return confirmBody(w, "Remove "+s.selectedName()+"?",
			"This only removes it from the registry; the printer itself is unaffected.", s.confirmYes, "Remove", "Cancel")
	case s.mode == printersModeConfirmControl:
		return confirmBody(w, "Allow control on "+s.selectedName()+"?", controlWarningText,
			s.confirmYes, "Allow control", "Cancel")
	}
	return s.listBody(w, h)
}

func (s *printersScreen) addingBody(w int) []string {
	out := []string{frame.Gutter + "Host name or IP address of the printer:", "", frame.Gutter + s.ps.Input + "_"}
	if s.ps.Probing {
		out = append(out, "", frame.Gutter+s.spin.glyph()+" Probing...")
	}
	if s.ps.Message != "" {
		out = append(out, "")
		out = append(out, wrapStyled(w, styleWarn, s.ps.Message)...)
	}
	return out
}

// printersColumn is one column of the list; drop is the order in which a
// narrow terminal gives columns up (1 first), 0 = never dropped.
type printersColumn struct {
	name  string
	width int
	drop  int
}

func (s *printersScreen) columns(w int) []printersColumn {
	nameW := 8
	for _, r := range s.ps.Rows {
		nameW = max(nameW, min(ansi.StringWidth(printerRowName(r)), 24))
	}
	cols := []printersColumn{
		{"NAME", nameW, 0}, {"HOST", 15, 3}, {"ENABLED", 7, 0},
		{"CONTROL", 7, 2}, {"REACHABLE", 9, 1}, {"STATE", 16, 0},
	}
	for drop := 1; drop <= 3; drop++ {
		total := len(frame.Marker(false)) - 1
		for _, c := range cols {
			total += c.width + 1
		}
		if total <= w-1 {
			break
		}
		kept := cols[:0:0]
		for _, c := range cols {
			if c.drop != drop {
				kept = append(kept, c)
			}
		}
		cols = kept
	}
	return cols
}

func (s *printersScreen) listBody(w, h int) []string {
	var top []string
	if s.ps.Scanning {
		text := "scanning the network..."
		if s.ps.Total > 0 {
			text = fmt.Sprintf("scanning: %d/%d hosts, %d found", s.ps.Scanned, s.ps.Total, s.ps.Found)
		}
		top = append(top, frame.Gutter+s.spin.glyph()+" "+text)
	}
	if s.ps.Saving {
		top = append(top, frame.Gutter+s.spin.glyph()+" saving...")
	}

	var bottom []string
	if s.ps.ScanErr != "" {
		bottom = append(bottom, wrapStyled(w, styleError, "Scan error: "+s.ps.ScanErr)...)
	}
	if s.ps.Partial && !s.ps.Scanning {
		bottom = append(bottom, wrapStyled(w, styleWarn,
			"The last scan did not finish within its time budget; press r to scan again.")...)
	}
	if s.ps.Message != "" {
		bottom = append(bottom, wrapStyled(w, styleWarn, s.ps.Message)...)
	}

	if len(s.ps.Rows) == 0 {
		out := append(top, frame.Wrap(w, frame.Gutter, "No printers found. Press m to add one by host, or r to rescan.")...)
		return append(out, bottom...)
	}

	cols := s.columns(w)
	var head []string
	for _, c := range cols {
		head = append(head, fixedCell(c.name, c.width))
	}
	if len(top) > 0 {
		top = append(top, "")
	}
	out := append(top, frame.Gutter+"  "+styleDim.Render(strings.TrimRight(strings.Join(head, " "), " ")))

	reserve := len(out) + len(bottom)
	if len(bottom) > 0 {
		reserve++
	}
	s.list.cursor = s.ps.Cursor
	from, to := s.list.window(len(s.ps.Rows), h-reserve)
	for i := from; i < to; i++ {
		out = append(out, s.rowLine(i, cols))
	}
	if len(bottom) > 0 {
		out = append(out, "")
		out = append(out, bottom...)
	}
	return out
}

func (s *printersScreen) rowLine(i int, cols []printersColumn) string {
	row := s.ps.Rows[i]
	marker := frame.Marker(i == s.ps.Cursor)
	name := fixedCell(printerRowName(row), cols[0].width)
	if !row.Addable {
		return marker + name + " " + styleDim.Render(row.Reason)
	}
	enabled, control, reach, state := "no", "off", "?", ""
	if row.Printer.Enabled {
		enabled = "yes"
	}
	if row.Printer.AllowControl {
		control = "on"
	}
	if ri, ok := s.reach[row.Printer.ID]; ok {
		reach = "no"
		if ri.reachable {
			reach = "yes"
		}
		state = ri.state
	}
	values := map[string]string{
		"NAME": printerRowName(row), "HOST": row.Printer.Host, "ENABLED": enabled,
		"CONTROL": control, "REACHABLE": reach, "STATE": state,
	}
	cells := make([]string, len(cols))
	for j, c := range cols {
		cells[j] = fixedCell(values[c.name], c.width)
	}
	return marker + strings.TrimRight(strings.Join(cells, " "), " ")
}
