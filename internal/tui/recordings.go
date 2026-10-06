package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/userhome"
)

// Terminal widths at which the recordings table gains or loses columns.
const (
	recordingsIDMinWidth   = 120 // the ID column is shown from here
	recordingsFullMinWidth = 80  // SIZE and STATUS are shown from here
)

// recordingsScreen lists every recording the background daemon knows about
// (active and completed), newest first, with duration, size and parts, and lets
// the user open a recording's containing folder or delete it
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section). Every action goes
// through Deps.Daemon and Deps.OpenFolder, both injected so a test never
// starts the real daemon or opens a real file manager window (AGENTS.md
// hard testing rule, task instructions).
type recordingsScreen struct {
	ctx  context.Context
	deps Deps

	loaded     bool
	loadErr    string
	recordings []daemon.RecordingInfo // newest first
	names      map[string]string      // printer id -> name, for display only

	list       selectList
	confirming bool
	confirmYes bool

	message string
	msgKind resultKind
	spin    spinner
	lastH   int
}

func newRecordingsScreen(ctx context.Context, deps Deps) *recordingsScreen {
	return &recordingsScreen{ctx: ctx, deps: deps, spin: newSpinner()}
}

type recordingsLoadedMsg struct {
	recordings []daemon.RecordingInfo
	names      map[string]string
	err        error
}

type recordingsActionMsg struct {
	text    string
	err     error
	deleted bool
}

func (s *recordingsScreen) Init() tea.Cmd {
	return tea.Batch(s.loadCmd(), s.spin.ensure(true))
}

func (s *recordingsScreen) loadCmd() tea.Cmd {
	ctx, dir, daemonClient := s.ctx, s.deps.Dir, s.deps.Daemon
	return func() tea.Msg {
		names := map[string]string{}
		if reg, _, _, err := domain.LoadRegistry(dir); err == nil {
			for _, p := range reg.Printers {
				names[p.ID] = p.Name
			}
		}
		if daemonClient == nil {
			return recordingsLoadedMsg{err: errDaemonUnavailable}
		}
		res, err := daemonClient.ListRecordings(ctx)
		if err != nil {
			return recordingsLoadedMsg{err: err}
		}
		return recordingsLoadedMsg{recordings: res.Recordings, names: names}
	}
}

func (s *recordingsScreen) selected() *daemon.RecordingInfo {
	if s.list.cursor < 0 || s.list.cursor >= len(s.recordings) {
		return nil
	}
	return &s.recordings[s.list.cursor]
}

func (s *recordingsScreen) Update(msg tea.Msg) (tea.Cmd, Nav) {
	cmd := s.update(msg)
	return tea.Batch(cmd, s.spin.ensure(!s.loaded && s.loadErr == "")), NavNone
}

func (s *recordingsScreen) update(msg tea.Msg) tea.Cmd {
	if handled, cmd := s.spin.update(msg, !s.loaded && s.loadErr == ""); handled {
		return cmd
	}
	switch m := msg.(type) {
	case recordingsLoadedMsg:
		if m.err != nil {
			s.loadErr = m.err.Error()
			return nil
		}
		s.loadErr, s.loaded = "", true
		s.recordings, s.names = m.recordings, m.names
		sort.SliceStable(s.recordings, func(i, j int) bool {
			return s.recordings[i].StartedAt.After(s.recordings[j].StartedAt)
		})
		s.list.clamp(len(s.recordings))
		return nil

	case recordingsActionMsg:
		if m.err != nil {
			s.message, s.msgKind = m.err.Error(), resultError
			return nil
		}
		s.message, s.msgKind = m.text, resultOK
		if m.deleted {
			return s.loadCmd()
		}
		return nil

	case tea.KeyMsg:
		if s.confirming {
			return s.confirmKey(m)
		}
		switch m.String() {
		case "r":
			s.message = ""
			s.loaded, s.loadErr = false, ""
			return s.loadCmd()
		case "enter":
			return s.openFolderCmd()
		case "d":
			if s.selected() != nil {
				s.confirming, s.confirmYes = true, false
			}
			return nil
		}
		if s.loaded {
			s.list.key(m, len(s.recordings), max(s.lastH-1, 1))
		}
	}
	return nil
}

func (s *recordingsScreen) openFolderCmd() tea.Cmd {
	rec := s.selected()
	if rec == nil || len(rec.Parts) == 0 {
		s.message, s.msgKind = "Nothing to open.", resultError
		return nil
	}
	dir := filepath.Dir(rec.Parts[0].Path)
	opener := s.deps.OpenFolder
	return func() tea.Msg {
		if err := opener(dir); err != nil {
			return recordingsActionMsg{err: fmt.Errorf("open %s: %w", userhome.Shorten(dir), err)}
		}
		return recordingsActionMsg{text: "Opened " + userhome.Shorten(dir)}
	}
}

func (s *recordingsScreen) confirmKey(m tea.KeyMsg) tea.Cmd {
	switch confirmKey(m, &s.confirmYes) {
	case confirmYes:
		return s.deleteCmd()
	case confirmNo:
		s.confirming = false
	}
	return nil
}

func (s *recordingsScreen) deleteCmd() tea.Cmd {
	s.confirming = false
	rec := s.selected()
	if rec == nil {
		return nil
	}
	id := rec.ID
	ctx, daemonClient := s.ctx, s.deps.Daemon
	return func() tea.Msg {
		if daemonClient == nil {
			return recordingsActionMsg{err: errDaemonUnavailable}
		}
		if err := daemonClient.DeleteRecording(ctx, id); err != nil {
			return recordingsActionMsg{err: fmt.Errorf("delete %s: %w", id, err)}
		}
		return recordingsActionMsg{text: "Deleted " + id + ".", deleted: true}
	}
}

func (s *recordingsScreen) Back() bool {
	if s.confirming {
		s.confirming = false
		return true
	}
	return false
}

func (s *recordingsScreen) Mode() Mode {
	if s.confirming {
		return ModeModal
	}
	return ModeNormal
}

func (s *recordingsScreen) Header() frame.Header { return frame.Header{Name: "Recordings"} }

func (s *recordingsScreen) Hints(w, h int) []frame.Hint {
	switch {
	case s.confirming:
		return confirmHints()
	case s.loadErr != "":
		return []frame.Hint{{Keys: "r", Label: "retry", Priority: 80}, frame.Back(), frame.Quit()}
	case !s.loaded:
		return []frame.Hint{frame.Back(), frame.Quit()}
	}
	out := []frame.Hint{{Keys: "r", Label: "refresh", Priority: 30}}
	if len(s.recordings) > 0 {
		out = []frame.Hint{
			{Keys: "↑↓", Label: "move", Priority: 70},
			{Keys: "enter", Label: "open folder", Priority: 80},
			{Keys: "d", Label: "delete", Priority: 60},
			{Keys: "r", Label: "refresh", Priority: 30},
		}
	}
	return append(out, frame.Back(), frame.Quit())
}

// recordingsColumns is the fixed column set for a terminal width: everything
// from 120 columns, no ID from 80, and under 80 no SIZE and STATUS.
func recordingsColumns(w int) []string {
	cols := []string{"STARTED", "PRINTER", "MODE", "DURATION"}
	if w >= recordingsFullMinWidth {
		cols = append(cols, "SIZE", "STATUS")
	}
	if w >= recordingsIDMinWidth {
		cols = append(cols, "ID")
	}
	return cols
}

var recordingsColumnWidths = map[string]int{
	"STARTED": 16, "PRINTER": 16, "MODE": 9, "DURATION": 9, "SIZE": 9, "STATUS": 9, "ID": 22,
}

func (s *recordingsScreen) cell(r daemon.RecordingInfo, col string) string {
	switch col {
	case "STARTED":
		return r.StartedAt.Local().Format("2006-01-02 15:04")
	case "PRINTER":
		if name := s.names[r.PrinterID]; name != "" {
			return name
		}
		return r.PrinterID
	case "MODE":
		return string(r.Mode)
	case "DURATION":
		return formatRecordingDuration(r.DurationSeconds)
	case "SIZE":
		return formatBytes(r.Bytes)
	case "STATUS":
		if r.Active {
			return "recording"
		}
		return "done"
	}
	return r.ID
}

func fixedCell(text string, width int) string {
	return frame.Pad(ansi.Truncate(text, width, ""), width)
}

func (s *recordingsScreen) Body(w, h int) []string {
	s.lastH = h
	switch {
	case s.loadErr != "":
		return wrapStyled(w, styleError, s.loadErr)
	case !s.loaded:
		return []string{frame.Gutter + s.spin.glyph() + " Reading recordings..."}
	case s.confirming:
		id := ""
		if rec := s.selected(); rec != nil {
			id = rec.ID
		}
		return confirmBody(w, "Delete recording "+id+"?", "This permanently deletes its files from disk.",
			s.confirmYes, "Delete", "Cancel")
	case len(s.recordings) == 0:
		out := frame.Wrap(w, frame.Gutter, "No recordings yet. Start one in Camera, or ask the AI to record a print.")
		return append(out, s.messageLines(w)...)
	}

	cols := recordingsColumns(w)
	var head []string
	for _, c := range cols {
		head = append(head, fixedCell(c, recordingsColumnWidths[c]))
	}
	out := []string{frame.Gutter + "  " + styleDim.Render(strings.TrimRight(strings.Join(head, " "), " "))}

	msg := s.messageLines(w)
	capacity := h - len(out) - len(msg)
	from, to := s.list.window(len(s.recordings), capacity)
	for i := from; i < to; i++ {
		r := s.recordings[i]
		var cells []string
		for _, c := range cols {
			cell := fixedCell(s.cell(r, c), recordingsColumnWidths[c])
			if c == "STATUS" && r.Active {
				cell = styleWarn.Render(cell)
			}
			cells = append(cells, cell)
		}
		out = append(out, frame.Marker(i == s.list.cursor)+strings.TrimRight(strings.Join(cells, " "), " "))
	}
	return append(out, msg...)
}

// messageLines is the result of the last action under the table, with a blank
// row above it, or nothing.
func (s *recordingsScreen) messageLines(w int) []string {
	if s.message == "" {
		return nil
	}
	style := styleOK
	if s.msgKind == resultError {
		style = styleError
	}
	return append([]string{""}, wrapStyled(w, style, s.message)...)
}

func formatRecordingDuration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, sec)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
