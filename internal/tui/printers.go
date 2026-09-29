package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
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
	ps         wizard.PrinterState
	cancelScan context.CancelFunc
	progressCh chan discovery.Progress

	reach map[string]reachInfo

	mode        printersMode
	renameInput string
	confirmYes  bool
}

func newPrintersScreen() *printersScreen {
	return &printersScreen{reach: map[string]reachInfo{}}
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

func (s *printersScreen) Init(ctx context.Context, deps Deps) tea.Cmd {
	dir := deps.Dir
	return func() tea.Msg {
		reg, path, _, err := domain.LoadRegistry(dir)
		return printersRegistryLoadedMsg{reg: reg, path: path, err: err}
	}
}

func (s *printersScreen) Update(ctx context.Context, deps Deps, msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case printersRegistryLoadedMsg:
		s.ps.Ready = true
		if m.err != nil {
			s.ps.Message = m.err.Error()
			return nil
		}
		s.ps.Dir = deps.Dir
		s.ps.Registry = m.reg
		s.ps.Path = m.path
		s.ps.Rows = wizard.BuildRows(m.reg, nil, nil)
		return s.reachCmd(ctx, deps)

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
		return s.reachCmd(ctx, deps)

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
		return s.reachCmd(ctx, deps)

	case printersReachMsg:
		for k, v := range m {
			s.reach[k] = v
		}
		return nil

	case tea.KeyMsg:
		return s.handleKey(m, ctx, deps)
	}
	return nil
}

func (s *printersScreen) handleKey(m tea.KeyMsg, ctx context.Context, deps Deps) tea.Cmd {
	if !s.ps.Ready || s.ps.Saving {
		return nil
	}
	if s.ps.Adding {
		return s.updateAdding(m, ctx, deps)
	}
	switch s.mode {
	case printersModeRenaming:
		return s.updateRenaming(m)
	case printersModeConfirmRemove:
		return s.updateConfirmRemove(m)
	case printersModeConfirmControl:
		return s.updateConfirmControl(m)
	default:
		return s.updateNormal(m, ctx, deps)
	}
}

func (s *printersScreen) updateNormal(m tea.KeyMsg, ctx context.Context, deps Deps) tea.Cmd {
	switch m.String() {
	case "q":
		return app.Action("printers", "back")
	case "esc":
		if s.ps.Scanning {
			if s.cancelScan != nil {
				s.cancelScan()
			}
			s.ps.Message = "Stopping scan..."
			return nil
		}
		return app.Action("printers", "back")
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
			s.ps.Message = "enable the printer before allowing control"
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
			return s.startScan(ctx, deps)
		}
	}
	return nil
}

func (s *printersScreen) updateAdding(m tea.KeyMsg, ctx context.Context, deps Deps) tea.Cmd {
	if s.ps.Probing {
		return nil
	}
	switch m.String() {
	case "esc":
		s.ps.Adding = false
		s.ps.Input = ""
		s.ps.Message = ""
	case "enter":
		host := strings.TrimSpace(s.ps.Input)
		if host == "" {
			s.ps.Message = "enter a host name or IP address"
			return nil
		}
		if err := domain.ValidateHost(host); err != nil {
			s.ps.Message = err.Error()
			return nil
		}
		s.ps.Probing = true
		s.ps.Message = ""
		return s.probeCmd(ctx, deps, host)
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
	s.ps.Probing = false
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
	case "esc":
		s.mode = printersModeNormal
	case "enter":
		name := strings.TrimSpace(s.renameInput)
		if name == "" {
			s.ps.Message = "name must not be empty"
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
	switch m.String() {
	case "up", "down", "k", "j", "tab", "left", "right", "h", "l":
		s.confirmYes = !s.confirmYes
	case "esc", "n", "q", "ctrl+c":
		s.mode = printersModeNormal
	case "y":
		return s.doRemove()
	case "enter":
		if !s.confirmYes {
			s.mode = printersModeNormal
			return nil
		}
		return s.doRemove()
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
	switch m.String() {
	case "up", "down", "k", "j", "tab", "left", "right", "h", "l":
		s.confirmYes = !s.confirmYes
	case "esc", "n", "q", "ctrl+c":
		s.mode = printersModeNormal
	case "y":
		return s.doAllowControl()
	case "enter":
		if !s.confirmYes {
			s.mode = printersModeNormal
			return nil
		}
		return s.doAllowControl()
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
// internal/wizard's own Printers install step: esc stops it, keeping
// whatever was found. It never saves the registry by itself (only an
// explicit action - enable, control, add, rename, remove - does), matching
// "printers scan" never changing the registry.
func (s *printersScreen) startScan(ctx context.Context, deps Deps) tea.Cmd {
	scanCtx, cancel := context.WithCancel(ctx)
	s.cancelScan = cancel
	s.ps.Scanning = true
	s.ps.Scanned, s.ps.Total, s.ps.Found = 0, 0, 0
	s.ps.ScanErr = ""
	s.ps.Message = ""

	ch := make(chan discovery.Progress, 32)
	s.progressCh = ch

	reg := s.ps.Registry
	discover := deps.Discover
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

func (s *printersScreen) probeCmd(ctx context.Context, deps Deps, host string) tea.Cmd {
	probe := deps.Probe
	reg := s.ps.Registry
	return func() tea.Msg {
		_, merged, err := wizard.ProbeAndMerge(ctx, probe, host, 0, reg)
		if err != nil {
			return printersProbeDoneMsg{err: err}
		}
		return printersProbeDoneMsg{merged: merged}
	}
}

// reachCmd gathers reachability and derived state for every addable,
// registered row, the same printerstate.Take + DeriveActivityState pipeline
// internal/clicmd's "printers" list command uses (gatherPrinterViews),
// bounded to printersReachConcurrency in flight and printersReachTimeout
// per printer so one dead printer can never hang the whole screen.
func (s *printersScreen) reachCmd(ctx context.Context, deps Deps) tea.Cmd {
	rows := s.ps.Rows
	printerClients := deps.PrinterClients
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

func (s *printersScreen) View() string {
	var b strings.Builder
	b.WriteString(tuiStyleTitle.Render("  Printers") + "\n\n")

	if !s.ps.Ready {
		b.WriteString("  " + tuiStyleDim.Render("Loading the printer registry...") + "\n")
		return b.String()
	}

	if s.ps.Adding {
		return s.viewAdding(&b)
	}
	switch s.mode {
	case printersModeRenaming:
		return s.viewRenaming(&b)
	case printersModeConfirmRemove:
		return s.viewConfirmRemove(&b)
	case printersModeConfirmControl:
		return s.viewConfirmControl(&b)
	}

	if s.ps.Scanning {
		if s.ps.Total > 0 {
			fmt.Fprintf(&b, "  %s scanning: %d/%d hosts, %d found\n\n", tuiStyleDim.Render("..."), s.ps.Scanned, s.ps.Total, s.ps.Found)
		} else {
			b.WriteString("  " + tuiStyleDim.Render("scanning the network...") + "\n\n")
		}
	}
	if s.ps.Saving {
		b.WriteString("  " + tuiStyleDim.Render("saving...") + "\n\n")
	}

	if len(s.ps.Rows) == 0 {
		b.WriteString("  " + tuiStyleDim.Render("No printers found. Press m to add one by host, or r to rescan.") + "\n")
	} else {
		fmt.Fprintf(&b, "  %-22s %-16s %-8s %-8s %-10s %s\n", "NAME", "HOST", "ENABLED", "CONTROL", "REACHABLE", "STATE")
		for i, row := range s.ps.Rows {
			cursor := " "
			if i == s.ps.Cursor {
				cursor = tuiStyleCursor.Render(">")
			}
			name := printerRowName(row)
			if !row.Addable {
				fmt.Fprintf(&b, "%s %-22s %s\n", cursor, name, tuiStyleDim.Render(row.Reason))
				continue
			}
			enabled, control, reach, state := "no", "off", "?", ""
			if row.Printer.Enabled {
				enabled = "yes"
			}
			if row.Printer.AllowControl {
				control = "on"
			}
			if ri, ok := s.reach[row.Printer.ID]; ok {
				if ri.reachable {
					reach = "yes"
				} else {
					reach = "no"
				}
				state = ri.state
			}
			fmt.Fprintf(&b, "%s %-22s %-16s %-8s %-8s %-10s %s\n", cursor, name, row.Printer.Host, enabled, control, reach, state)
		}
	}

	if s.ps.ScanErr != "" {
		b.WriteString("\n  " + tuiStyleError.Render("scan error: "+s.ps.ScanErr))
	}
	if s.ps.Partial && !s.ps.Scanning {
		b.WriteString("\n  " + tuiStyleWarn.Render("The last scan did not finish within its time budget; press r to scan again."))
	}
	if s.ps.Message != "" {
		b.WriteString("\n\n  " + s.ps.Message)
	}

	b.WriteString("\n\n" + tuiStyleDim.Render("  ↑↓ move · space enabled · c control · m add host · n rename · d remove · r rescan · esc back"))
	return b.String()
}

func (s *printersScreen) viewAdding(b *strings.Builder) string {
	b.WriteString("  Host name or IP address of the printer:\n\n  " + s.ps.Input + "_\n")
	if s.ps.Probing {
		b.WriteString("\n  " + tuiStyleDim.Render("Probing..."))
	}
	if s.ps.Message != "" {
		b.WriteString("\n\n  " + s.ps.Message)
	}
	b.WriteString("\n\n" + tuiStyleDim.Render("  enter probe · esc cancel"))
	return b.String()
}

func (s *printersScreen) viewRenaming(b *strings.Builder) string {
	row := wizard.CurrentRow(&s.ps)
	name := ""
	if row != nil {
		name = printerRowName(*row)
	}
	fmt.Fprintf(b, "  New name for %s:\n\n  %s_\n", name, s.renameInput)
	b.WriteString("\n" + tuiStyleDim.Render("  enter confirm · esc cancel"))
	return b.String()
}

func (s *printersScreen) viewConfirmRemove(b *strings.Builder) string {
	row := wizard.CurrentRow(&s.ps)
	name := ""
	if row != nil {
		name = printerRowName(*row)
	}
	b.WriteString("  " + tuiStyleWarn.Render("Remove "+name+"?") + "\n\n")
	b.WriteString("  " + tuiStyleDim.Render("This only removes it from the registry; the printer itself is unaffected.") + "\n\n")
	b.WriteString(tuiConfirmChoiceLines(s.confirmYes, "Remove", "Cancel"))
	return b.String()
}

func (s *printersScreen) viewConfirmControl(b *strings.Builder) string {
	row := wizard.CurrentRow(&s.ps)
	name := ""
	if row != nil {
		name = printerRowName(*row)
	}
	b.WriteString("  " + tuiStyleWarn.Render("Allow control on "+name+"?") + "\n\n")
	b.WriteString("  " + tuiStyleDim.Render(controlWarningText) + "\n\n")
	b.WriteString(tuiConfirmChoiceLines(s.confirmYes, "Allow control", "Cancel"))
	return b.String()
}
