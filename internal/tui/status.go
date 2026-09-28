package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"
	"github.com/sairaph/mcp-wizard/app/detail"
	"github.com/sairaph/mcp-wizard/app/list"

	"github.com/sairaph/creality_k2_mcp/internal/daemon"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// statusRefreshInterval is how often the live view reloads
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section: "per-printer live view
// refreshing every 2 s"). A package variable, not a constant, so a test can
// shrink it instead of waiting out a real 2 s tick (the same pattern
// internal/daemon's recorder.go uses for its own polling intervals).
var statusRefreshInterval = 2 * time.Second

type statusScreenMode int

const (
	statusModePicker statusScreenMode = iota
	statusModeLive
)

// statusScreen is the read-only Status screen: pick an enabled printer,
// then a live view of its derived state, temperatures, job progress, fans,
// light, CFS flag and idle-heat watchdog status, refreshing every 2 s. It
// has no control actions in v0.1.0 (dev_docs/plan-v0.1.0.md), and reuses
// exactly the printerstate.Take/DeriveActivityState/BuildStateBlock
// pipeline the "status" one-shot command and get_printer_status MCP tool
// use, so all three surfaces report the same thing.
type statusScreen struct {
	mode statusScreenMode

	printers []domain.Printer
	list     *list.Model
	loadErr  string

	selected domain.Printer
	detail   *detail.Model

	// loading and loadSeq guard the 2 s refresh loop against a slow snapshot
	// (printerstate.Take's own 10 s timeout) overlapping the next tick: a
	// tick skips starting a new load while one is still in flight, and every
	// load is tagged with the seq that was current when it started, so an
	// old load's result can never overwrite a newer one that finished first
	// (dev_docs/review-backlog.md item 40).
	loading bool
	loadSeq int
}

func newStatusScreen() *statusScreen { return &statusScreen{} }

type statusPrintersLoadedMsg struct {
	printers []domain.Printer
	err      error
}

type statusLoadedMsg struct {
	seq  int
	text string
	err  error
}

type statusTickMsg struct{}

func (s *statusScreen) Init(ctx context.Context, deps Deps) tea.Cmd {
	dir := deps.Dir
	return func() tea.Msg {
		reg, _, _, err := domain.LoadRegistry(dir)
		if err != nil {
			return statusPrintersLoadedMsg{err: err}
		}
		return statusPrintersLoadedMsg{printers: reg.Enabled()}
	}
}

func (s *statusScreen) Update(ctx context.Context, deps Deps, msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case statusPrintersLoadedMsg:
		if m.err != nil {
			s.loadErr = m.err.Error()
			return nil
		}
		s.printers = m.printers
		items := make([]list.Item, len(m.printers))
		for i, p := range m.printers {
			items[i] = list.Item{Label: fmt.Sprintf("%s (%s)", p.Name, p.Host), Active: true}
		}
		s.list = list.New("Status - choose a printer", items, 0, len(items), len(items))
		return s.list.Init()

	case statusLoadedMsg:
		if m.seq != s.loadSeq {
			// Stale: a newer load has since started (or, defensively, this
			// is some earlier load's late result racing a fresher one).
			// Only the result tagged with the current seq is ever applied.
			return nil
		}
		s.loading = false
		if s.detail == nil {
			return nil
		}
		if m.err != nil {
			s.detail.SetContent("Could not read status: " + m.err.Error())
		} else {
			s.detail.SetContent(m.text)
		}
		return nil

	case statusTickMsg:
		if s.mode != statusModeLive {
			return nil
		}
		if s.loading {
			// The previous tick's load has not returned yet (a slow
			// snapshot); skip starting another one so loads never pile up,
			// but keep the ticker itself alive so refreshing resumes as
			// soon as the in-flight load finishes.
			return s.tickCmd()
		}
		return tea.Batch(s.loadCmd(ctx, deps), s.tickCmd())

	case app.ActionMsg:
		if m.Source != "list" {
			return nil
		}
		switch m.Value {
		case "select":
			idx, _ := m.Data.(int)
			if idx < 0 || idx >= len(s.printers) {
				return nil
			}
			s.selected = s.printers[idx]
			s.mode = statusModeLive
			s.detail = detail.New(fmt.Sprintf("%s (%s)", s.selected.Name, s.selected.Host), "Loading...")
			return tea.Batch(s.detail.Init(), s.loadCmd(ctx, deps), s.tickCmd())
		case "back":
			return app.Action("status", "back")
		}
		return nil

	case tea.KeyMsg:
		if s.mode == statusModeLive {
			switch m.String() {
			case "esc", "q":
				s.mode = statusModePicker
				return nil
			}
			if s.detail != nil {
				return s.detail.Update(msg)
			}
			return nil
		}
		if m.String() == "q" {
			return app.Action("status", "back")
		}
		if s.list != nil {
			return s.list.Update(msg)
		}
	}
	return nil
}

func (s *statusScreen) tickCmd() tea.Cmd {
	return tea.Tick(statusRefreshInterval, func(time.Time) tea.Msg { return statusTickMsg{} })
}

// loadCmd takes a fresh snapshot for the selected printer and, when a
// daemon client is wired up, its idle-heat watchdog status, off the UI
// loop. It marks a load as in-flight (s.loading) and stamps the result with
// the seq current at the time it started, so statusTickMsg can skip
// starting an overlapping load and a stale statusLoadedMsg can be told apart
// from the current one (dev_docs/review-backlog.md item 40).
func (s *statusScreen) loadCmd(ctx context.Context, deps Deps) tea.Cmd {
	s.loading = true
	s.loadSeq++
	seq := s.loadSeq

	printer := s.selected
	pdeps := deps.PrinterClients(printer)
	daemonClient := deps.Daemon
	return func() tea.Msg {
		snap := printerstate.Take(ctx, pdeps, printer)
		derived := printerstate.DeriveActivityState(snap, nil)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		var heaters []daemon.HeaterStatus
		var watchdogKnown bool
		if daemonClient != nil {
			if hs, ok := daemonClient.Status(ctx, printerstate.Identity(snap)); ok {
				heaters, watchdogKnown = hs, true
			}
		}
		return statusLoadedMsg{seq: seq, text: formatStatusText(block, snap, heaters, watchdogKnown)}
	}
}

// formatStatusText renders block (and snap, for the job-progress fields
// StateBlock does not carry directly: layer and percent complete) plus the
// idle-heat watchdog status as human-readable text, matching
// internal/clicmd's "status" command formatting for temperatures/job/CFS,
// extended with fans, light and the watchdog (dev_docs/plan-v0.1.0.md's
// "TUI and CLI" section).
func formatStatusText(block printerstate.StateBlock, snap printerstate.Snapshot, heaters []daemon.HeaterStatus, watchdogKnown bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s (%s)\n", block.PrinterName, block.PrinterHost)
	fmt.Fprintf(&b, "State: %s   bucket: %s   class: %s\n", block.ActivityState, block.Bucket, block.GatingClass)
	if len(block.Reasons) > 0 {
		b.WriteString("Reasons:\n")
		for _, r := range block.Reasons {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}

	b.WriteString("\n")
	if block.NozzleTemperatureC != nil {
		fmt.Fprintf(&b, "Nozzle: %.1f", *block.NozzleTemperatureC)
		if block.NozzleTargetC != nil {
			fmt.Fprintf(&b, "/%.1f", *block.NozzleTargetC)
		}
		b.WriteString(" C\n")
	}
	if block.BedTemperatureC != nil {
		fmt.Fprintf(&b, "Bed: %.1f", *block.BedTemperatureC)
		if block.BedTargetC != nil {
			fmt.Fprintf(&b, "/%.1f", *block.BedTargetC)
		}
		b.WriteString(" C\n")
	}

	if block.Job != nil {
		fmt.Fprintf(&b, "\nJob: %s\n", block.Job.Filename)
		if snap.VirtualSDCard != nil {
			if snap.VirtualSDCard.Progress > 0 {
				fmt.Fprintf(&b, "  progress: %.1f%%\n", snap.VirtualSDCard.Progress*100)
			}
			if snap.VirtualSDCard.LayerCount > 0 {
				fmt.Fprintf(&b, "  layer: %d/%d\n", snap.VirtualSDCard.Layer, snap.VirtualSDCard.LayerCount)
			}
		}
	}

	b.WriteString("\n")
	fmt.Fprintf(&b, "Part fan: %s\n", statusPercent(block.PartFanPercent))
	fmt.Fprintf(&b, "Case fan: %s\n", statusPercent(block.CaseFanPercent))
	fmt.Fprintf(&b, "Auxiliary fan: %s\n", statusPercent(block.AuxiliaryFanPercent))
	fmt.Fprintf(&b, "Light: %s\n", statusOnOff(block.LightOn))

	b.WriteString("\n")
	fmt.Fprintf(&b, "CFS connected: %s\n", statusYesNo(block.CFSConnected))
	fmt.Fprintf(&b, "9999 reachable: %s\n", statusYesNo(block.Ws9999Reachable))

	b.WriteString("\nIdle-heat watchdog: ")
	switch {
	case !watchdogKnown:
		b.WriteString("unknown (daemon not reachable)\n")
	case len(heaters) == 0:
		b.WriteString("no heaters armed\n")
	default:
		b.WriteString("\n")
		for _, h := range heaters {
			state := "not armed"
			if h.Armed {
				state = fmt.Sprintf("armed, target %.1f C", h.TargetC)
				if !h.DeadlineAt.IsZero() {
					state += ", off at " + h.DeadlineAt.Format("15:04:05")
				}
			}
			fmt.Fprintf(&b, "  %s: %s\n", h.Heater, state)
		}
	}

	return b.String()
}

func statusPercent(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.0f%%", *v)
}

func statusOnOff(v *bool) string {
	if v == nil {
		return "unknown"
	}
	if *v {
		return "on"
	}
	return "off"
}

func statusYesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func (s *statusScreen) View() string {
	if s.loadErr != "" {
		return tuiStyleTitle.Render("  Status") + "\n\n  " + tuiStyleError.Render(s.loadErr)
	}
	if s.mode == statusModeLive {
		if s.detail != nil {
			return s.detail.View()
		}
		return tuiStyleDim.Render("  Loading...")
	}
	if s.list == nil {
		return tuiStyleTitle.Render("  Status") + "\n\n  " + tuiStyleDim.Render("Loading printers...")
	}
	if len(s.printers) == 0 {
		return tuiStyleTitle.Render("  Status") + "\n\n  " +
			tuiStyleDim.Render("No printer is enabled. Enable one from Printers first.") + "\n\n" +
			tuiStyleDim.Render("  q back")
	}
	return s.list.View()
}
