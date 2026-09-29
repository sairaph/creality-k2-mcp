package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"
	"github.com/sairaph/mcp-wizard/app/table"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

type recordingsMode int

const (
	recordingsModeNormal recordingsMode = iota
	recordingsModeConfirmDelete
)

// recordingsScreen lists every recording the background daemon knows about
// (active and completed), with size, duration and parts, and lets the user
// open a recording's containing folder or delete it
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section). Every action goes
// through Deps.Daemon and Deps.OpenFolder, both injected so a test never
// starts the real daemon or opens a real file manager window (AGENTS.md
// hard testing rule, task instructions).
type recordingsScreen struct {
	recordings []daemon.RecordingInfo
	names      map[string]string // printer id -> name, for display only
	table      *table.Model
	loadErr    string

	mode       recordingsMode
	confirmYes bool

	message string
}

func newRecordingsScreen() *recordingsScreen { return &recordingsScreen{} }

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

func (s *recordingsScreen) Init(ctx context.Context, deps Deps) tea.Cmd {
	return s.loadCmd(ctx, deps)
}

func (s *recordingsScreen) loadCmd(ctx context.Context, deps Deps) tea.Cmd {
	dir := deps.Dir
	daemonClient := deps.Daemon
	return func() tea.Msg {
		names := map[string]string{}
		if reg, _, _, err := domain.LoadRegistry(dir); err == nil {
			for _, p := range reg.Printers {
				names[p.ID] = p.Name
			}
		}
		if daemonClient == nil {
			return recordingsLoadedMsg{err: fmt.Errorf("the camera daemon is not available")}
		}
		res, err := daemonClient.ListRecordings(ctx)
		if err != nil {
			return recordingsLoadedMsg{err: err}
		}
		return recordingsLoadedMsg{recordings: res.Recordings, names: names}
	}
}

func (s *recordingsScreen) buildTable() {
	cols := []table.Column{
		{Name: "ID", Width: 22},
		{Name: "PRINTER", Width: 16},
		{Name: "MODE", Width: 9},
		{Name: "ACTIVE", Width: 6},
		{Name: "STARTED", Width: 19},
		{Name: "DURATION", Width: 9},
		{Name: "SIZE", Width: 9},
	}
	rows := make([]table.Row, len(s.recordings))
	for i, r := range s.recordings {
		name := s.names[r.PrinterID]
		if name == "" {
			name = r.PrinterID
		}
		active := "no"
		if r.Active {
			active = "yes"
		}
		rows[i] = table.Row{
			r.ID, name, string(r.Mode), active,
			r.StartedAt.Format("2006-01-02 15:04:05"),
			formatRecordingDuration(r.DurationSeconds),
			formatBytes(r.Bytes),
		}
	}
	s.table = table.New("Recordings", cols, rows)
}

// selectedRecording looks up the recording backing the table's currently
// highlighted row by ID (the table's first column) rather than by
// s.table.Cursor's raw index into s.recordings, so this stays correct after
// the table's own "s" sort key has reordered its rows.
func (s *recordingsScreen) selectedRecording() *daemon.RecordingInfo {
	if s.table == nil || s.table.Cursor < 0 || s.table.Cursor >= len(s.table.Rows) {
		return nil
	}
	id := s.table.Rows[s.table.Cursor][0]
	for i := range s.recordings {
		if s.recordings[i].ID == id {
			return &s.recordings[i]
		}
	}
	return nil
}

func (s *recordingsScreen) Update(ctx context.Context, deps Deps, msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case recordingsLoadedMsg:
		if m.err != nil {
			s.loadErr = m.err.Error()
			return nil
		}
		s.loadErr = ""
		s.recordings = m.recordings
		s.names = m.names
		s.buildTable()
		return nil

	case recordingsActionMsg:
		if m.err != nil {
			s.message = m.err.Error()
			return nil
		}
		s.message = m.text
		if m.deleted {
			return s.loadCmd(ctx, deps)
		}
		return nil

	case app.ActionMsg:
		if m.Source != "table" {
			return nil
		}
		switch m.Value {
		case "back":
			return app.Action("recordings", "back")
		case "select":
			return s.openFolderCmd(deps)
		}
		return nil

	case tea.KeyMsg:
		if s.mode == recordingsModeConfirmDelete {
			return s.updateConfirmDelete(ctx, deps, m)
		}
		switch m.String() {
		case "q":
			return app.Action("recordings", "back")
		case "r":
			s.message = ""
			return s.loadCmd(ctx, deps)
		case "o":
			return s.openFolderCmd(deps)
		case "d":
			if s.selectedRecording() == nil {
				return nil
			}
			s.mode = recordingsModeConfirmDelete
			s.confirmYes = false
			return nil
		}
		if s.table != nil {
			return s.table.Update(msg)
		}
	}
	return nil
}

func (s *recordingsScreen) openFolderCmd(deps Deps) tea.Cmd {
	rec := s.selectedRecording()
	if rec == nil || len(rec.Parts) == 0 {
		s.message = "nothing to open"
		return nil
	}
	dir := filepath.Dir(rec.Parts[0].Path)
	opener := deps.OpenFolder
	return func() tea.Msg {
		if err := opener(dir); err != nil {
			return recordingsActionMsg{err: fmt.Errorf("open %s: %w", dir, err)}
		}
		return recordingsActionMsg{text: "Opened " + dir}
	}
}

func (s *recordingsScreen) updateConfirmDelete(ctx context.Context, deps Deps, m tea.KeyMsg) tea.Cmd {
	switch m.String() {
	case "up", "down", "k", "j", "tab", "left", "right", "h", "l":
		s.confirmYes = !s.confirmYes
	case "esc", "n", "q", "ctrl+c":
		s.mode = recordingsModeNormal
	case "y":
		return s.deleteCmd(ctx, deps)
	case "enter":
		if !s.confirmYes {
			s.mode = recordingsModeNormal
			return nil
		}
		return s.deleteCmd(ctx, deps)
	}
	return nil
}

func (s *recordingsScreen) deleteCmd(ctx context.Context, deps Deps) tea.Cmd {
	s.mode = recordingsModeNormal
	rec := s.selectedRecording()
	if rec == nil {
		return nil
	}
	id := rec.ID
	daemonClient := deps.Daemon
	return func() tea.Msg {
		if daemonClient == nil {
			return recordingsActionMsg{err: fmt.Errorf("the camera daemon is not available")}
		}
		if err := daemonClient.DeleteRecording(ctx, id); err != nil {
			return recordingsActionMsg{err: fmt.Errorf("delete %s: %w", id, err)}
		}
		return recordingsActionMsg{text: "Deleted " + id + ".", deleted: true}
	}
}

func (s *recordingsScreen) View() string {
	if s.loadErr != "" {
		return tuiStyleTitle.Render("  Recordings") + "\n\n  " + tuiStyleError.Render(s.loadErr) +
			"\n\n" + tuiStyleDim.Render("  r retry · q back")
	}
	if s.table == nil {
		return tuiStyleTitle.Render("  Recordings") + "\n\n  " + tuiStyleDim.Render("Loading recordings...")
	}
	if s.mode == recordingsModeConfirmDelete {
		rec := s.selectedRecording()
		id := ""
		if rec != nil {
			id = rec.ID
		}
		var b strings.Builder
		b.WriteString(tuiStyleTitle.Render("  Recordings") + "\n\n")
		b.WriteString("  " + tuiStyleWarn.Render("Delete recording "+id+"?") + "\n\n")
		b.WriteString("  " + tuiStyleDim.Render("This permanently deletes its files from disk.") + "\n\n")
		b.WriteString(tuiConfirmChoiceLines(s.confirmYes, "Delete", "Cancel"))
		return b.String()
	}

	out := s.table.View()
	out += "\n" + tuiStyleDim.Render("  o open folder · d delete · r refresh · q back")
	if s.message != "" {
		out += "\n  " + s.message
	}
	return out
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
