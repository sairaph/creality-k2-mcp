package tui

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"
	"github.com/sairaph/mcp-wizard/app/list"
	"github.com/sairaph/mcp-wizard/app/menu"

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// snapshotJPEGQuality matches internal/clicmd/snapshot.go's own constant:
// no size cap applies here (unlike get_camera_snapshot's MCP result), so
// the full-resolution capture is kept for local use.
const snapshotJPEGQuality = 92

type cameraScreenMode int

const (
	cameraModePicker cameraScreenMode = iota
	cameraModeActions
)

// cameraScreen: pick an enabled printer, then open its live view in the
// default browser or start/stop a recording
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section). Every action goes
// through Deps.Daemon (the background camera daemon client) and
// Deps.Opener (the browser opener), both injected so a test never starts
// the real daemon or launches a real browser (AGENTS.md hard testing rule,
// task instructions).
type cameraScreen struct {
	mode cameraScreenMode

	printers []domain.Printer
	list     *list.Model
	loadErr  string

	selected domain.Printer
	actions  *menu.Model
	busy     bool
	message  string
}

func newCameraScreen() *cameraScreen { return &cameraScreen{} }

type cameraPrintersLoadedMsg struct {
	printers []domain.Printer
	err      error
}

type cameraResultMsg struct {
	text string
	err  error
}

func (s *cameraScreen) Init(ctx context.Context, deps Deps) tea.Cmd {
	dir := deps.Dir
	return func() tea.Msg {
		reg, _, _, err := domain.LoadRegistry(dir)
		if err != nil {
			return cameraPrintersLoadedMsg{err: err}
		}
		return cameraPrintersLoadedMsg{printers: reg.Enabled()}
	}
}

func (s *cameraScreen) Update(ctx context.Context, deps Deps, msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case cameraPrintersLoadedMsg:
		if m.err != nil {
			s.loadErr = m.err.Error()
			return nil
		}
		s.printers = m.printers
		items := make([]list.Item, len(m.printers))
		for i, p := range m.printers {
			items[i] = list.Item{Label: fmt.Sprintf("%s (%s)", p.Name, p.Host), Active: true}
		}
		s.list = list.New("Camera - choose a printer", items, 0, len(items), len(items))
		return s.list.Init()

	case cameraResultMsg:
		s.busy = false
		if m.err != nil {
			s.message = m.err.Error()
		} else {
			s.message = m.text
		}
		return nil

	case app.ActionMsg:
		return s.handleAction(ctx, deps, m)

	case tea.KeyMsg:
		if s.mode == cameraModeActions {
			if s.busy || s.actions == nil {
				return nil
			}
			return s.actions.Update(msg)
		}
		if m.String() == "q" {
			return app.Action("camera", "back")
		}
		if s.list != nil {
			return s.list.Update(msg)
		}
	}
	return nil
}

func (s *cameraScreen) handleAction(ctx context.Context, deps Deps, msg app.ActionMsg) tea.Cmd {
	switch msg.Source {
	case "list":
		switch msg.Value {
		case "select":
			idx, _ := msg.Data.(int)
			if idx < 0 || idx >= len(s.printers) {
				return nil
			}
			s.selected = s.printers[idx]
			s.mode = cameraModeActions
			s.message = ""
			s.actions = menu.New(fmt.Sprintf("Camera - %s", s.selected.Name), func() []menu.Item {
				return []menu.Item{
					{Label: "Open live view in browser", Action: "open"},
					{Label: "Take a snapshot", Action: "snapshot"},
					{Label: "Start recording", Action: "start"},
					{Label: "Stop recording", Action: "stop"},
					{Label: "Back", Action: "back"},
				}
			})
			return s.actions.Init()
		case "back":
			return app.Action("camera", "back")
		}
	case "menu":
		// "quit" is the action submenu's own q/ctrl+c: here it means "back
		// to the printer picker", never "quit the application" (ctrl+c
		// still quits the whole app first, via AppModel.HandleGlobalKeys).
		if msg.Value == "quit" {
			s.mode = cameraModePicker
			s.message = ""
			return nil
		}
		if msg.Value != "select" {
			return nil
		}
		action, _ := msg.Data.(string)
		switch action {
		case "back":
			s.mode = cameraModePicker
			s.message = ""
		case "open":
			if deps.Daemon == nil {
				s.message = "the camera daemon is not available"
				return nil
			}
			s.busy = true
			s.message = "Opening the live view..."
			return s.openCmd(ctx, deps)
		case "snapshot":
			if deps.Daemon == nil {
				s.message = "the camera daemon is not available"
				return nil
			}
			s.busy = true
			s.message = "Capturing a snapshot..."
			return s.snapshotCmd(ctx, deps)
		case "start":
			if deps.Daemon == nil {
				s.message = "the camera daemon is not available"
				return nil
			}
			s.busy = true
			s.message = "Starting a recording..."
			return s.startCmd(ctx, deps)
		case "stop":
			if deps.Daemon == nil {
				s.message = "the camera daemon is not available"
				return nil
			}
			s.busy = true
			s.message = "Stopping the recording..."
			return s.stopCmd(ctx, deps)
		}
	}
	return nil
}

func (s *cameraScreen) openCmd(ctx context.Context, deps Deps) tea.Cmd {
	printer := s.selected
	daemonClient := deps.Daemon
	opener := deps.Opener
	return func() tea.Msg {
		url, err := daemonClient.ViewerURL(ctx, printer.ID)
		if err != nil {
			return cameraResultMsg{err: fmt.Errorf("open the live view: %w", err)}
		}
		if err := opener(url); err != nil {
			return cameraResultMsg{err: fmt.Errorf("opened %s but could not launch a browser: %w", url, err)}
		}
		return cameraResultMsg{text: "Opened " + url + " in your browser."}
	}
}

// snapshotCmd captures one still frame from the selected printer's camera
// through Deps.Daemon.Snapshot (review backlog item 51: the background
// daemon's hub, autostarting it like openCmd's ViewerURL already does) and
// saves it as a timestamped JPEG file in the registry directory, matching
// the CLI "snapshot" command's own default output naming.
func (s *cameraScreen) snapshotCmd(ctx context.Context, deps Deps) tea.Cmd {
	printer := s.selected
	daemonClient := deps.Daemon
	dir := deps.Dir
	return func() tea.Msg {
		result, err := daemonClient.Snapshot(ctx, printer.Host)
		if err != nil {
			return cameraResultMsg{err: fmt.Errorf("capture a snapshot: %w", err)}
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%s.jpg", printer.ID, time.Now().Format("20060102-150405")))
		if err := writeSnapshotJPEG(path, result.Image); err != nil {
			return cameraResultMsg{err: fmt.Errorf("save the snapshot: %w", err)}
		}
		return cameraResultMsg{text: "Saved snapshot to " + path}
	}
}

// writeSnapshotJPEG encodes img as a JPEG and writes it to path, refusing
// to silently overwrite an existing file at that path (matching
// internal/clicmd/snapshot.go's own default behaviour, without --force): a
// fresh timestamped name from snapshotCmd above should never collide in
// practice, but this stays safe if it ever does (e.g. two snapshots taken
// within the same second).
func writeSnapshotJPEG(path string, img image.Image) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	encErr := jpeg.Encode(f, img, &jpeg.Options{Quality: snapshotJPEGQuality})
	closeErr := f.Close()
	if encErr != nil {
		return encErr
	}
	return closeErr
}

func (s *cameraScreen) startCmd(ctx context.Context, deps Deps) tea.Cmd {
	printer := s.selected
	daemonClient := deps.Daemon
	return func() tea.Msg {
		info, err := daemonClient.StartRecording(ctx, daemon.RecordingStartParams{
			PrinterID: printer.ID,
			Host:      printer.Host,
			Mode:      string(daemon.RecordModeVideo),
			Until:     string(daemon.RecordUntilStopped),
		})
		if err != nil {
			return cameraResultMsg{err: fmt.Errorf("start recording: %w", err)}
		}
		return cameraResultMsg{text: "Recording started (" + info.ID + ")."}
	}
}

func (s *cameraScreen) stopCmd(ctx context.Context, deps Deps) tea.Cmd {
	printer := s.selected
	daemonClient := deps.Daemon
	return func() tea.Msg {
		list, err := daemonClient.ListRecordings(ctx)
		if err != nil {
			return cameraResultMsg{err: fmt.Errorf("stop recording: %w", err)}
		}
		var activeID string
		for _, r := range list.Recordings {
			if r.Active && r.PrinterID == printer.ID {
				activeID = r.ID
				break
			}
		}
		if activeID == "" {
			return cameraResultMsg{err: fmt.Errorf("%s has no active recording", printer.Name)}
		}
		info, err := daemonClient.StopRecording(ctx, activeID)
		if err != nil {
			return cameraResultMsg{err: fmt.Errorf("stop recording: %w", err)}
		}
		return cameraResultMsg{text: fmt.Sprintf("Recording %s stopped (%s).", info.ID, info.StopReason)}
	}
}

func (s *cameraScreen) View() string {
	if s.loadErr != "" {
		return tuiStyleTitle.Render("  Camera") + "\n\n  " + tuiStyleError.Render(s.loadErr)
	}
	if s.mode == cameraModeActions {
		var out string
		if s.actions != nil {
			out = s.actions.View()
		}
		if s.busy {
			out += "\n\n  " + tuiStyleDim.Render("Working...")
		} else if s.message != "" {
			out += "\n\n  " + s.message
		}
		return out
	}
	if s.list == nil {
		return tuiStyleTitle.Render("  Camera") + "\n\n  " + tuiStyleDim.Render("Loading printers...")
	}
	if len(s.printers) == 0 {
		return tuiStyleTitle.Render("  Camera") + "\n\n  " +
			tuiStyleDim.Render("No printer is enabled. Enable one from Printers first.") + "\n\n" +
			tuiStyleDim.Render("  q back")
	}
	return s.list.View()
}
