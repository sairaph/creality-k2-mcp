package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

// statusRefreshInterval is how often the live view reloads
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section: "per-printer live view
// refreshing every 2 s"). A package variable, not a constant, so a test can
// shrink it instead of waiting out a real 2 s tick (the same pattern
// internal/daemon's recorder.go uses for its own polling intervals).
var statusRefreshInterval = 2 * time.Second

// statusLabelWidth is the one label column of the Status summary.
const statusLabelWidth = 16

// statusMaxDetails caps the reasons the Details row shows; the CLI has them all.
const statusMaxDetails = 3

// statusData is everything one snapshot gives the screen: the same
// printerstate.StateBlock the status command and get_printer_status build,
// plus the job progress StateBlock does not carry and the idle-heat watchdog.
type statusData struct {
	block         printerstate.StateBlock
	progress      float64 // percent complete; negative when unknown
	layer, layers int
	moonrakerOK   bool
	heaters       []daemon.HeaterStatus
	watchdogKnown bool
}

// statusScreen is the read-only Status screen: a live view of one printer's
// derived state, temperatures, job progress, fans, light, CFS and idle-heat
// watchdog, refreshing every 2 s without losing the scroll position. With
// several enabled printers a picker comes first. It has no control actions
// (dev_docs/plan-v0.1.0.md) and reuses exactly the printerstate.Take/
// DeriveActivityState/BuildStateBlock pipeline the "status" one-shot command
// and get_printer_status MCP tool use, so all three surfaces report the same
// thing.
type statusScreen struct {
	ctx  context.Context
	deps Deps

	picker   printerPicker
	selected *domain.Printer

	data    *statusData
	loadErr string
	scroll  scrollView
	spin    spinner

	// lastH and lastTotal are the body rows and content lines of the last draw,
	// which page jumps and the end key measure against.
	lastH, lastTotal int

	// loading and loadSeq guard the 2 s refresh loop against a slow snapshot
	// (printerstate.Take's own 10 s timeout) overlapping the next tick: a
	// tick skips starting a new load while one is still in flight, and every
	// load is tagged with the seq that was current when it started, so an
	// old load's result can never overwrite a newer one that finished first
	// (dev_docs/review-backlog.md item 40). gen tags the tick chain, so
	// leaving the live view and opening a printer again never runs two.
	loading bool
	loadSeq int
	gen     int
}

func newStatusScreen(ctx context.Context, deps Deps) *statusScreen {
	return &statusScreen{ctx: ctx, deps: deps, picker: newPrinterPicker(), spin: newSpinner()}
}

type statusLoadedMsg struct {
	seq  int
	data statusData
	err  error
}

type statusTickMsg struct{ gen int }

func (s *statusScreen) Init() tea.Cmd {
	return tea.Batch(s.picker.loadCmd(s.deps), s.spin.ensure(true))
}

// open starts the live view of p.
func (s *statusScreen) open(p domain.Printer) tea.Cmd {
	s.selected = &p
	s.data, s.loadErr = nil, ""
	s.scroll = scrollView{}
	s.gen++
	s.loading = false
	return tea.Batch(s.loadCmd(), s.tickCmd(), s.spin.ensure(true))
}

func (s *statusScreen) tickCmd() tea.Cmd {
	gen := s.gen
	return tea.Tick(statusRefreshInterval, func(time.Time) tea.Msg { return statusTickMsg{gen} })
}

// loadCmd takes a fresh snapshot for the selected printer and, when a
// daemon client is wired up, its idle-heat watchdog status, off the UI
// loop. It marks a load as in-flight (s.loading) and stamps the result with
// the seq current at the time it started, so statusTickMsg can skip
// starting an overlapping load and a stale statusLoadedMsg can be told apart
// from the current one (dev_docs/review-backlog.md item 40).
func (s *statusScreen) loadCmd() tea.Cmd {
	s.loading = true
	s.loadSeq++
	seq := s.loadSeq

	ctx := s.ctx
	printer := *s.selected
	pdeps := s.deps.PrinterClients(printer)
	daemonClient := s.deps.Daemon
	return func() tea.Msg {
		snap := printerstate.Take(ctx, pdeps, printer)
		derived := printerstate.DeriveActivityState(snap, nil)
		d := statusData{
			block:       printerstate.BuildStateBlock(snap, derived, nil),
			progress:    -1,
			moonrakerOK: snap.ServerInfoErr == nil,
		}
		if v := snap.VirtualSDCard; v != nil {
			if v.Progress > 0 {
				d.progress = v.Progress * 100
			}
			d.layer, d.layers = v.Layer, v.LayerCount
		}
		if daemonClient != nil {
			if hs, ok := daemonClient.Status(ctx, printerstate.Identity(snap)); ok {
				d.heaters, d.watchdogKnown = hs, true
			}
		}
		return statusLoadedMsg{seq: seq, data: d}
	}
}

func (s *statusScreen) spinning() bool {
	if s.selected == nil {
		return !s.picker.loaded
	}
	return s.data == nil && s.loadErr == ""
}

func (s *statusScreen) Update(msg tea.Msg) (tea.Cmd, Nav) {
	cmd := s.update(msg)
	return tea.Batch(cmd, s.spin.ensure(s.spinning())), NavNone
}

func (s *statusScreen) update(msg tea.Msg) tea.Cmd {
	if handled, cmd := s.spin.update(msg, s.spinning()); handled {
		return cmd
	}
	if s.picker.apply(msg) {
		if p, ok := s.picker.only(); ok && s.selected == nil {
			return s.open(p)
		}
		return nil
	}
	switch m := msg.(type) {
	case statusLoadedMsg:
		if m.seq != s.loadSeq || s.selected == nil {
			// Stale: a newer load has since started (or, defensively, this
			// is some earlier load's late result racing a fresher one).
			// Only the result tagged with the current seq is ever applied.
			return nil
		}
		s.loading = false
		if m.err != nil {
			s.loadErr = "Could not read status: " + m.err.Error()
			return nil
		}
		s.loadErr = ""
		s.data = &m.data
		return nil

	case statusTickMsg:
		if s.selected == nil || m.gen != s.gen {
			return nil
		}
		if s.loading {
			// The previous tick's load has not returned yet (a slow
			// snapshot); skip starting another one so loads never pile up,
			// but keep the ticker itself alive so refreshing resumes as
			// soon as the in-flight load finishes.
			return s.tickCmd()
		}
		return tea.Batch(s.loadCmd(), s.tickCmd())

	case tea.KeyMsg:
		return s.key(m)
	}
	return nil
}

func (s *statusScreen) key(k tea.KeyMsg) tea.Cmd {
	if s.selected == nil {
		if k.String() == "r" && s.picker.err != "" {
			return s.picker.retry(s.deps)
		}
		if p, ok := s.picker.key(k, max(s.lastH, 1)); ok {
			return s.open(p)
		}
		return nil
	}
	if k.String() == "r" && s.data == nil && s.loadErr != "" {
		s.loadErr = ""
		return s.loadCmd()
	}
	s.scroll.key(k, s.lastTotal, s.lastH)
	return nil
}

// Back returns from a printer to the picker when there is one; with a single
// enabled printer the root leaves for the menu.
func (s *statusScreen) Back() bool {
	if s.selected != nil && len(s.picker.printers) > 1 {
		s.selected = nil
		s.data, s.loadErr = nil, ""
		s.gen++ // retires the tick chain of the live view
		return true
	}
	return false
}

func (s *statusScreen) Header() frame.Header {
	h := frame.Header{Name: "Status"}
	if s.selected != nil {
		h.Context = printerContext(*s.selected)
	} else {
		h.Context = "choose a printer"
	}
	return h
}

func (s *statusScreen) Mode() Mode { return ModeNormal }

func (s *statusScreen) Body(w, h int) []string {
	if s.selected == nil {
		s.lastH = h
		return s.picker.body(w, h, s.spin.glyph())
	}
	lines := s.lines(w)
	s.lastH, s.lastTotal = h, len(lines)
	return s.scroll.view(lines, h)
}

func (s *statusScreen) Hints(w, h int) []frame.Hint {
	if s.selected == nil {
		return s.picker.hints()
	}
	var out []frame.Hint
	if s.data == nil && s.loadErr != "" {
		out = append(out, frame.Hint{Keys: "r", Label: "retry", Priority: 80})
	}
	if len(s.lines(w)) > h {
		out = append(out, frame.Hint{Keys: "↑↓", Label: "scroll", Priority: 70})
	}
	return append(out, frame.Back(), frame.Quit())
}

// lines is the whole Status body at width w, before scrolling.
func (s *statusScreen) lines(w int) []string {
	if s.data == nil {
		if s.loadErr != "" {
			return wrapStyled(w, styleError, s.loadErr)
		}
		return []string{frame.Gutter + s.spin.glyph() + " Reading " + s.selected.Name + "..."}
	}
	var out []string
	if s.loadErr != "" {
		out = append(out, wrapStyled(w, styleError, s.loadErr)...)
		out = append(out, "")
	}
	return append(out, frame.Rows(w, statusLabelWidth, statusRows(*s.data)...)...)
}

// --- state words ---

type stateTone int

const (
	toneOK      stateTone = iota // green: the printer is at rest and fine
	toneBusy                     // amber: working, or in a transition
	toneBlocked                  // red: failed, unreachable or not trusted
)

type stateWord struct {
	word string
	tone stateTone
}

// statusStateWords maps every activity state to the short phrase Status shows
// and its colour; the test iterates the printerstate.State* constants against
// it, so a new state cannot ship without a word.
var statusStateWords = map[string]stateWord{
	printerstate.StateIdle:      {"Idle", toneOK},
	printerstate.StateComplete:  {"Complete", toneOK},
	printerstate.StateCancelled: {"Cancelled", toneOK},

	printerstate.StatePrinting:          {"Printing", toneBusy},
	printerstate.StatePreparing:         {"Starting", toneBusy},
	printerstate.StatePaused:            {"Paused", toneBusy},
	printerstate.StatePausing:           {"Pausing", toneBusy},
	printerstate.StateResuming:          {"Resuming", toneBusy},
	printerstate.StateCancelling:        {"Cancelling", toneBusy},
	printerstate.StateHoming:            {"Homing", toneBusy},
	printerstate.StateCalibrating:       {"Calibrating", toneBusy},
	printerstate.StateBusyCommand:       {"Busy (manual command)", toneBusy},
	printerstate.StateFilamentOperation: {"Moving filament", toneBusy},
	printerstate.StateCFSOperation:      {"CFS operation", toneBusy},
	printerstate.StateUpgrading:         {"Updating firmware", toneBusy},
	printerstate.StateRecoveryPending:   {"Power-loss recovery pending", toneBusy},

	printerstate.StateError:              {"Error", toneBlocked},
	printerstate.StateOffline:            {"Offline", toneBlocked},
	printerstate.StateKlippyNotReady:     {"Klipper not ready", toneBlocked},
	printerstate.StateUnknown:            {"Unknown (writes blocked)", toneBlocked},
	printerstate.StateIdentityMismatch:   {"Different printer at this address", toneBlocked},
	printerstate.StateIdentityUnverified: {"Identity not verified", toneBlocked},
}

// stateWordOf is the phrase and tone for a block; a state this table does not
// know is shown as is, in red.
func stateWordOf(b printerstate.StateBlock) stateWord {
	sw, ok := statusStateWords[b.ActivityState]
	if !ok {
		return stateWord{b.ActivityState, toneBlocked}
	}
	if b.ActivityState == printerstate.StatePreparing && b.StartWindow {
		sw.word = "Starting (printer self-test)"
	}
	return sw
}

func toneStyle(t stateTone) func(...string) string {
	switch t {
	case toneOK:
		return styleOK.Render
	case toneBusy:
		return styleWarn.Render
	}
	return styleError.Render
}

// --- rows ---

func dimUnknown() string { return styleDim.Render("unknown") }

// temperatureText is "38 C   (off)" or "205 C  -> 210 C".
func temperatureText(cur, target *float64) string {
	if cur == nil {
		return dimUnknown()
	}
	text := fmt.Sprintf("%.0f C", *cur)
	if target != nil && *target > 0 {
		return text + "  " + styleDim.Render(fmt.Sprintf("-> %.0f C", *target))
	}
	return text + "   " + styleDim.Render("(off)")
}

func fanText(label string, v *float64) string {
	if v == nil {
		return label + " " + dimUnknown()
	}
	return fmt.Sprintf("%s %.0f%%", label, *v)
}

// heaterName is the plain word for a daemon watchdog heater id.
func heaterName(id string) string {
	switch id {
	case "extruder":
		return "nozzle"
	case "heater_bed":
		return "bed"
	}
	return id
}

// statusRows turns one snapshot into the labelled rows of the Status screen
// (the design's 2.3 and R2.8).
func statusRows(d statusData) []frame.KV {
	b := d.block
	sw := stateWordOf(b)
	rows := []frame.KV{
		{Label: "State", Value: toneStyle(sw.tone)("● " + sw.word)},
		{Label: "Nozzle", Value: temperatureText(b.NozzleTemperatureC, b.NozzleTargetC)},
		{Label: "Bed", Value: temperatureText(b.BedTemperatureC, b.BedTargetC)},
		{Label: "Job", Value: jobText(d)},
	}
	if text := speedText(b); text != "" {
		rows = append(rows, frame.KV{Label: "Speed", Value: text})
	}

	light := dimUnknown()
	if b.LightOn != nil {
		light = "off"
		if *b.LightOn {
			light = "on"
		}
	}
	rows = append(rows,
		frame.KV{Label: "Fans", Value: fanText("part", b.PartFanPercent) + "   " +
			fanText("case", b.CaseFanPercent) + "   " + fanText("aux", b.AuxiliaryFanPercent)},
		frame.KV{Label: "Light", Value: light},
	)

	cfsProblem := false
	if b.CFS != nil {
		cfsProblem = b.CFS.State == printerstate.CFSStateError || b.CFS.State == printerstate.CFSStateUnknown
		rows = append(rows, cfsRow(*b.CFS))
	}
	rows = append(rows,
		frame.KV{Label: "Watchdog", Value: watchdogText(d)},
		frame.KV{Label: "Links", Value: linksText(d)},
	)

	if details := detailLines(d, sw, cfsProblem); len(details) > 0 {
		tone := sw.tone
		if tone == toneOK {
			tone = toneBusy // an ok state, but port 9999 or the CFS needs attention
		}
		style := toneStyle(tone)
		for i, line := range details {
			details[i] = style(line)
		}
		rows = append(rows, frame.KV{})
		rows = append(rows, frame.KV{Label: "Details", Value: strings.Join(details, "\n")})
	}
	return rows
}

func jobText(d statusData) string {
	job := d.block.Job
	if job == nil {
		return styleDim.Render("none")
	}
	name := job.Filename
	if name == "" {
		name = "unnamed job"
	}
	parts := []string{name}
	if d.progress >= 0 {
		parts = append(parts, fmt.Sprintf("%.0f%%", d.progress))
	}
	if d.layers > 0 {
		parts = append(parts, fmt.Sprintf("layer %d/%d", d.layer, d.layers))
	}
	return strings.Join(parts, "   ")
}

// speedText is the speed preset with its factor, "silent" for Silent, and
// empty when nothing is known (the row is then left out).
func speedText(b printerstate.StateBlock) string {
	factor := ""
	if b.SpeedFactorPercent != nil {
		factor = fmt.Sprintf("%.0f%%", *b.SpeedFactorPercent)
	}
	switch b.SpeedPreset {
	case "", printerstate.PresetUnknownStr:
		return factor
	case printerstate.PresetSilent:
		return "silent"
	}
	if factor == "" {
		return b.SpeedPreset
	}
	return b.SpeedPreset + " (" + factor + ")"
}

func cfsRow(c printerstate.CFSBlock) frame.KV {
	first := ""
	if len(c.Reasons) > 0 {
		first = c.Reasons[0]
	}
	switch c.State {
	case printerstate.CFSStateIdle:
		return frame.KV{Label: "CFS", Value: "idle"}
	case printerstate.CFSStateInPrint:
		return frame.KV{Label: "CFS", Value: "in print"}
	case printerstate.CFSStateBusy:
		text := "busy"
		if first != "" {
			text += " (" + first + ")"
		}
		return frame.KV{Label: "CFS", Value: styleWarn.Render(text)}
	case printerstate.CFSStateError:
		text := "error"
		if first != "" {
			text += ": " + first
		}
		return frame.KV{Label: "CFS", Value: styleError.Render(text)}
	}
	return frame.KV{Label: "CFS", Value: dimUnknown()}
}

// watchdogText is the idle-heat watchdog line; the "daemon not running" case
// is amber only while a heater target is set (then nothing would turn it off).
func watchdogText(d statusData) string {
	switch {
	case !d.watchdogKnown:
		b := d.block
		heating := (b.NozzleTargetC != nil && *b.NozzleTargetC > 0) || (b.BedTargetC != nil && *b.BedTargetC > 0)
		if heating {
			return styleWarn.Render("daemon not running")
		}
		return styleDim.Render("daemon not running")
	case len(d.heaters) == 0:
		return "no heaters armed"
	}
	var armed []string
	for _, h := range d.heaters {
		if !h.Armed {
			continue
		}
		text := heaterName(h.Heater)
		if !h.DeadlineAt.IsZero() {
			text += " until " + h.DeadlineAt.Format("15:04")
		}
		armed = append(armed, text)
	}
	if len(armed) == 0 {
		return "no heaters armed"
	}
	return "armed: " + strings.Join(armed, ", ")
}

func linksText(d statusData) string {
	moon := "Moonraker ok"
	if !d.moonrakerOK {
		moon = styleWarn.Render("Moonraker unreachable")
	}
	ws := "port 9999 ok"
	if !d.block.Ws9999Reachable {
		ws = styleWarn.Render("port 9999 unreachable")
	}
	return moon + " · " + ws
}

// detailLines are the reasons worth showing: only when the state is in the red
// (blocked) group, the CFS block is error or unknown, or port 9999 is
// unreachable; never for a clean idle, complete or cancelled, nor for an amber
// busy state such as printing or paused. The informational "cfs_connected: ..."
// lines are never shown (the CFS row says it).
func detailLines(d statusData, sw stateWord, cfsProblem bool) []string {
	if sw.tone != toneBlocked && !cfsProblem && d.block.Ws9999Reachable {
		return nil
	}
	var out []string
	for _, r := range d.block.Reasons {
		r = strings.TrimSpace(r)
		if r == "" || strings.HasPrefix(r, "cfs_connected:") {
			continue
		}
		out = append(out, r)
		if len(out) == statusMaxDetails {
			break
		}
	}
	return out
}
