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

	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/userhome"
)

// snapshotJPEGQuality matches internal/clicmd/snapshot.go's own constant:
// no size cap applies here (unlike get_camera_snapshot's MCP result), so
// the full-resolution capture is kept for local use.
const snapshotJPEGQuality = 92

type cameraAction struct {
	label string
	id    string
}

var cameraActions = []cameraAction{
	{"Open live view in the browser", "open"},
	{"Take a snapshot", "snapshot"},
	{"Start recording", "start"},
	{"Stop recording", "stop"},
}

type resultKind int

const (
	resultNone resultKind = iota
	resultWorking
	resultOK
	resultError
)

// cameraScreen: choose an enabled printer (skipped when only one is enabled),
// then open its live view in the default browser, take a snapshot or
// start/stop a recording (dev_docs/plan-v0.1.0.md's "TUI and CLI" section).
// Every action goes through Deps.Daemon (the background camera daemon client)
// and Deps.Opener (the browser opener), both injected so a test never starts
// the real daemon or launches a real browser (AGENTS.md hard testing rule,
// task instructions). An action is not a write the screen must wait for: q and
// esc work while it runs and the daemon finishes it.
type cameraScreen struct {
	ctx  context.Context
	deps Deps

	picker   printerPicker
	selected *domain.Printer

	actions selectList
	spin    spinner

	// opGen tags the action in flight; a result of an action started before the
	// user went back to the picker (or chose another printer) is dropped.
	opGen   int
	working bool
	result  string
	kind    resultKind

	lastH int
}

func newCameraScreen(ctx context.Context, deps Deps) *cameraScreen {
	return &cameraScreen{ctx: ctx, deps: deps, picker: newPrinterPicker(), spin: newSpinner()}
}

type cameraResultMsg struct {
	gen  int
	text string
	err  error
}

func (s *cameraScreen) Init() tea.Cmd {
	return tea.Batch(s.picker.loadCmd(s.deps), s.spin.ensure(true))
}

func (s *cameraScreen) open(p domain.Printer) {
	s.selected = &p
	s.opGen++
	s.working, s.result, s.kind = false, "", resultNone
	s.actions = selectList{}
}

func (s *cameraScreen) spinning() bool {
	return s.working || (s.selected == nil && !s.picker.loaded)
}

func (s *cameraScreen) Update(msg tea.Msg) (tea.Cmd, Nav) {
	cmd := s.update(msg)
	return tea.Batch(cmd, s.spin.ensure(s.spinning())), NavNone
}

func (s *cameraScreen) update(msg tea.Msg) tea.Cmd {
	if handled, cmd := s.spin.update(msg, s.spinning()); handled {
		return cmd
	}
	if s.picker.apply(msg) {
		if p, ok := s.picker.only(); ok && s.selected == nil {
			s.open(p)
		}
		return nil
	}
	switch m := msg.(type) {
	case cameraResultMsg:
		if m.gen != s.opGen {
			return nil
		}
		s.working = false
		if m.err != nil {
			s.result, s.kind = m.err.Error(), resultError
		} else {
			s.result, s.kind = m.text, resultOK
		}
	case tea.KeyMsg:
		return s.key(m)
	}
	return nil
}

func (s *cameraScreen) key(k tea.KeyMsg) tea.Cmd {
	if s.selected == nil {
		if k.String() == "r" && s.picker.err != "" {
			return s.picker.retry(s.deps)
		}
		if p, ok := s.picker.key(k, max(s.lastH, 1)); ok {
			s.open(p)
		}
		return nil
	}
	if s.actions.key(k, len(cameraActions), len(cameraActions)) {
		return nil
	}
	if k.String() != "enter" || s.working {
		return nil
	}
	if s.deps.Daemon == nil {
		s.result, s.kind = errDaemonUnavailable.Error(), resultError
		return nil
	}
	s.opGen++
	s.working, s.kind = true, resultWorking
	switch cameraActions[s.actions.cursor].id {
	case "open":
		s.result = "Opening the live view..."
		return s.openCmd()
	case "snapshot":
		s.result = "Capturing a snapshot..."
		return s.snapshotCmd()
	case "start":
		s.result = "Starting a recording..."
		return s.startCmd()
	default:
		s.result = "Stopping the recording..."
		return s.stopCmd()
	}
}

// Back returns from the actions to the picker when there is one; with a single
// enabled printer the root leaves for the menu.
func (s *cameraScreen) Back() bool {
	if s.selected != nil && len(s.picker.printers) > 1 {
		s.selected = nil
		s.opGen++ // the daemon finishes a running action; its result is dropped
		s.working, s.result, s.kind = false, "", resultNone
		return true
	}
	return false
}

func (s *cameraScreen) Mode() Mode { return ModeNormal }

func (s *cameraScreen) Header() frame.Header {
	h := frame.Header{Name: "Camera", Context: "choose a printer"}
	if s.selected != nil {
		h.Context = printerContext(*s.selected)
	}
	return h
}

func (s *cameraScreen) Body(w, h int) []string {
	s.lastH = h
	if s.selected == nil {
		return s.picker.body(w, h, s.spin.glyph())
	}
	from, to := s.actions.window(len(cameraActions), h)
	var out []string
	for i := from; i < to; i++ {
		out = append(out, frame.Marker(i == s.actions.cursor)+cameraActions[i].label)
	}
	if s.result == "" {
		return out
	}
	out = append(out, "")
	switch s.kind {
	case resultWorking:
		out = append(out, frame.Gutter+s.spin.glyph()+" "+s.result)
	case resultError:
		out = append(out, wrapStyled(w, styleError, s.result)...)
	default:
		out = append(out, wrapStyled(w, styleOK, s.result)...)
	}
	return out
}

func (s *cameraScreen) Hints(w, h int) []frame.Hint {
	if s.selected == nil {
		return s.picker.hints()
	}
	out := []frame.Hint{{Keys: "↑↓", Label: "move", Priority: 70}}
	if s.working {
		out = append(out, frame.Hint{Label: "working...", Priority: 80})
	} else {
		out = append(out, frame.Hint{Keys: "enter", Label: "run", Priority: 80})
	}
	return append(out, frame.Back(), frame.Quit())
}

func (s *cameraScreen) openCmd() tea.Cmd {
	ctx, gen := s.ctx, s.opGen
	printer := *s.selected
	daemonClient := s.deps.Daemon
	opener := s.deps.Opener
	return func() tea.Msg {
		url, err := daemonClient.ViewerURL(ctx, printer.ID)
		if err != nil {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("open the live view: %w", err)}
		}
		if err := opener(url); err != nil {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("opened %s but could not launch a browser: %w", url, err)}
		}
		return cameraResultMsg{gen: gen, text: "Opened " + url + " in your browser."}
	}
}

// snapshotCmd captures one still frame from the selected printer's camera
// through Deps.Daemon.Snapshot (review backlog item 51: the background
// daemon's hub, autostarting it like openCmd's ViewerURL already does) and
// saves it as a timestamped JPEG file in the registry directory, matching
// the CLI "snapshot" command's own default output naming. The path is shown
// (shortened with ~) because it is where the user must go to see the picture.
func (s *cameraScreen) snapshotCmd() tea.Cmd {
	ctx, gen := s.ctx, s.opGen
	printer := *s.selected
	daemonClient := s.deps.Daemon
	dir := s.deps.Dir
	return func() tea.Msg {
		result, err := daemonClient.Snapshot(ctx, printer.Host)
		if err != nil {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("capture a snapshot: %w", err)}
		}
		path := filepath.Join(dir, fmt.Sprintf("%s-%s.jpg", printer.ID, time.Now().Format("20060102-150405")))
		if err := writeSnapshotJPEG(path, result.Image); err != nil {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("save the snapshot: %w", err)}
		}
		return cameraResultMsg{gen: gen, text: "Snapshot saved to " + userhome.Shorten(path)}
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

func (s *cameraScreen) startCmd() tea.Cmd {
	ctx, gen := s.ctx, s.opGen
	printer := *s.selected
	daemonClient := s.deps.Daemon
	return func() tea.Msg {
		info, err := daemonClient.StartRecording(ctx, daemon.RecordingStartParams{
			PrinterID: printer.ID,
			Host:      printer.Host,
			Mode:      string(daemon.RecordModeVideo),
			Until:     string(daemon.RecordUntilStopped),
		})
		if err != nil {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("start recording: %w", err)}
		}
		return cameraResultMsg{gen: gen, text: "Recording started (" + info.ID + ")."}
	}
}

func (s *cameraScreen) stopCmd() tea.Cmd {
	ctx, gen := s.ctx, s.opGen
	printer := *s.selected
	daemonClient := s.deps.Daemon
	return func() tea.Msg {
		list, err := daemonClient.ListRecordings(ctx)
		if err != nil {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("stop recording: %w", err)}
		}
		var activeID string
		for _, r := range list.Recordings {
			if r.Active && r.PrinterID == printer.ID {
				activeID = r.ID
				break
			}
		}
		if activeID == "" {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("%s has no active recording", printer.Name)}
		}
		info, err := daemonClient.StopRecording(ctx, activeID)
		if err != nil {
			return cameraResultMsg{gen: gen, err: fmt.Errorf("stop recording: %w", err)}
		}
		return cameraResultMsg{gen: gen, text: fmt.Sprintf("Recording %s stopped (%s).", info.ID, info.StopReason)}
	}
}
