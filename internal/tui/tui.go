// Package tui implements creality-k2-mcp's interactive application
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section, T13b, reworked in v0.4.0 per
// dev_docs/tui-design-v0.4.0.md): the full-screen app a bare invocation opens
// on a terminal (main.go's runApp). Every screen is an own type behind the
// Screen interface and every view is drawn by internal/tui/frame (header, blank
// row, body, footer pinned to the last row), so the app has one back/quit rule
// (esc goes back one level, q quits, ctrl+c quits at once) and footers that are
// built from the same state the keys are. It reuses the business logic the
// one-shot CLI commands (internal/clicmd) and the install wizard
// (internal/wizard) call: internal/wizard's exported registry merge/save
// functions for Printers, internal/printerstate's Take/DeriveActivityState/
// BuildStateBlock for Status, internal/daemon's client for Camera and
// Recordings, internal/settingsform for Settings and internal/doctorlist's
// checks for Doctor.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerclient"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
	"github.com/sairaph/creality-k2-mcp/internal/wizard"
)

// DaemonClient is the subset of *daemon/client.Client the app needs for
// Camera and Recordings, and for Status's watchdog display. It matches that
// package's exported methods exactly, so the production *client.Client
// satisfies it with no adapter; a test supplies a fake instead of ever
// dialing the real background daemon (AGENTS.md hard testing rule).
// Snapshot (review backlog item 51) captures one decoded frame through the
// daemon's hub - the same rolling-GOP-buffer path get_camera_snapshot and
// the CLI's "snapshot" command use, kept warm across repeated calls - and
// is the Camera screen's only capture path; there is no other, direct
// fallback.
type DaemonClient interface {
	ViewerURL(ctx context.Context, printerID string) (string, error)
	StartRecording(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error)
	StopRecording(ctx context.Context, id string) (daemon.RecordingInfo, error)
	ListRecordings(ctx context.Context) (daemon.RecordingListResult, error)
	DeleteRecording(ctx context.Context, id string) error
	Status(ctx context.Context, identity string) (heaters []daemon.HeaterStatus, ok bool)
	Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error)
}

// PrinterClientsFunc builds the printerstate.Deps used to read one printer,
// matching internal/clicmd.PrinterClientsFunc's shape (this package does not
// import internal/clicmd, so it keeps its own copy of the type).
type PrinterClientsFunc func(p domain.Printer) printerstate.Deps

// Deps bundles everything the app needs beyond a context, so a test can
// supply fakes for discovery, printer state, the daemon and the doctor checks
// without ever touching a real registry file, printer or the network (AGENTS.md
// hard testing rule), and without ever launching a real browser.
type Deps struct {
	// Dir resolves which registry file Printers/Status/Camera read and
	// (for Printers) write, matching domain.RegistryPath: the project file
	// when Dir names a directory that already has one, the global file
	// otherwise. Empty means the global file only.
	Dir string

	// Discover runs a network scan for the Printers screen's rescan action.
	// Nil defaults to discovery.Discover (through wizard.ScanAndMerge).
	Discover wizard.DiscoverFunc
	// Probe identifies one manually entered host for "add host". Nil
	// defaults to discovery.ProbeHost (through wizard.ProbeAndMerge).
	Probe wizard.ProbeFunc

	// PrinterClients builds per-printer Moonraker/9999 clients for Status
	// (and the reachability column on Printers). Defaults to
	// internal/printerclient.Default.
	PrinterClients PrinterClientsFunc

	// Daemon is the background camera/idle-heat daemon client used by
	// Camera, Recordings and Status's watchdog line. Nil means the daemon
	// is not wired up: those screens say so instead of failing.
	Daemon DaemonClient

	// Opener opens a URL in the user's default browser (Camera's "open live
	// view"). Defaults to a real OS opener; a test always injects a fake so
	// no test ever launches a real browser (AGENTS.md hard testing rule,
	// task instructions).
	Opener func(url string) error
	// OpenFolder opens a directory in the OS file manager (Recordings'
	// "open containing folder"). Defaults to a real OS opener; same testing
	// rule as Opener.
	OpenFolder func(path string) error

	// DoctorChecks builds the doctor check list the Doctor screen runs one by
	// one (internal/doctorlist.Checks, the same list the `doctor` command
	// runs). Nil means no checks.
	DoctorChecks func() []doctor.Check
}

func (d Deps) withDefaults() Deps {
	if d.PrinterClients == nil {
		d.PrinterClients = printerclient.Default
	}
	if d.Opener == nil {
		d.Opener = openURL
	}
	if d.OpenFolder == nil {
		d.OpenFolder = openFolder
	}
	if d.DoctorChecks == nil {
		d.DoctorChecks = func() []doctor.Check { return nil }
	}
	return d
}

// Mode tells the root how a screen wants keys handled right now.
type Mode int

const (
	// ModeNormal: q quits, esc goes back, every other key goes to the screen.
	ModeNormal Mode = iota
	// ModeTyping: a text field has focus; q is a character, esc cancels the
	// field and enter confirms it.
	ModeTyping
	// ModeModal: a confirm dialog is open; the screen reads y, n, q, enter.
	ModeModal
	// ModeBusy: a write is in flight; every key is ignored except ctrl+c.
	ModeBusy
)

// Nav is what a screen asks the root to do after an Update.
type Nav int

const (
	NavNone Nav = iota
	// NavLeave returns to the menu.
	NavLeave
	// NavQuit ends the program.
	NavQuit
)

// Screen is one app screen. The root owns the size and the frame: it passes
// the terminal width and the body row count (H-3) on every View, so a screen
// keeps no copy of the size and clamps its cursor and scroll inside Body.
type Screen interface {
	Init() tea.Cmd
	// Update mutates the screen in place. The root has already handled q, esc
	// and ctrl+c according to Mode, so keys arriving here are the screen's own.
	Update(msg tea.Msg) (tea.Cmd, Nav)
	// Back is called for esc: true means the screen handled it itself (closed
	// a field, a dialog or a sub-view), false makes the root leave the screen.
	Back() bool
	Body(w, h int) []string
	// Hints is the footer key list, built from the same state Update reads.
	Hints(w, h int) []frame.Hint
	Header() frame.Header
	Mode() Mode
}

// closer is implemented by screens that hold background work (a scan, the
// doctor goroutine) to cancel when the screen is left or the app quits.
type closer interface{ Close() }

// Model is the whole application's tea.Model: the root that owns the terminal
// size, the keys common to every screen, and the frame.
type Model struct {
	ctx     context.Context
	deps    Deps
	version string

	w, h int // from the last tea.WindowSizeMsg; 0 until the first one

	menu   *menuScreen
	screen Screen
}

// newModel builds the application model, shared by Run and by this
// package's own tests (which drive Init/Update/View directly instead of
// through a real tea.Program, so a test never opens a terminal).
func newModel(ctx context.Context, version string, deps Deps) *Model {
	m := &Model{ctx: ctx, version: version, deps: deps.withDefaults()}
	m.menu = newMenuScreen(version)
	m.screen = m.menu
	return m
}

// Run starts the interactive application on its own full-screen program and
// returns the process exit code: 0 after a quit, 1 if the program fails.
func Run(ctx context.Context, version string, deps Deps) int {
	p := tea.NewProgram(newModel(ctx, version, deps), tea.WithContext(ctx), tea.WithAltScreen())
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		fmt.Fprintf(os.Stderr, "  %s: %v\n", domain.ServerName, err)
		return 1
	}
	return 0
}

func (m *Model) Init() tea.Cmd { return m.screen.Init() }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, m.apply(m.screen.Update(msg))
}

// handleKey is the one place q, esc and ctrl+c are interpreted (R5 and R2.2
// of the design): ctrl+c quits at once in every mode; while a write is in
// flight nothing else works; a text field or a dialog gets every key except
// esc; otherwise q quits and esc goes back one level.
func (m *Model) handleKey(k tea.KeyMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		return m.quit()
	}
	mode := m.screen.Mode()
	if _, small := frame.TooSmall(m.w, m.h); small {
		if key == "q" && mode == ModeNormal {
			return m.quit()
		}
		return nil
	}
	if mode == ModeBusy {
		return nil
	}
	if key == "esc" {
		if !m.screen.Back() {
			m.leave()
		}
		return nil
	}
	if key == "q" && mode == ModeNormal {
		return m.quit()
	}
	return m.apply(m.screen.Update(k))
}

// apply turns a screen's Nav into root behaviour and, from the menu, opens the
// screen the user picked.
func (m *Model) apply(cmd tea.Cmd, nav Nav) tea.Cmd {
	switch nav {
	case NavQuit:
		return tea.Batch(cmd, m.quit())
	case NavLeave:
		m.leave()
		return cmd
	}
	if m.screen == Screen(m.menu) {
		if choice := m.menu.take(); choice != "" {
			return tea.Batch(cmd, m.open(choice))
		}
	}
	return cmd
}

func (m *Model) quit() tea.Cmd {
	m.closeScreen()
	return tea.Quit
}

// leave returns to the menu, which keeps its cursor.
func (m *Model) leave() {
	m.closeScreen()
	m.screen = m.menu
}

func (m *Model) closeScreen() {
	if c, ok := m.screen.(closer); ok {
		c.Close()
	}
}

// open switches to the screen named by a menu action, constructing it fresh
// every time so a screen never shows stale data from a previous visit.
func (m *Model) open(action string) tea.Cmd {
	switch action {
	case "printers":
		m.screen = newPrintersScreen(m.ctx, m.deps)
	case "status":
		m.screen = newStatusScreen(m.ctx, m.deps)
	case "camera":
		m.screen = newCameraScreen(m.ctx, m.deps)
	case "recordings":
		m.screen = newRecordingsScreen(m.ctx, m.deps)
	case "settings":
		m.screen = newSettingsScreen()
	case "doctor":
		m.screen = newDoctorScreen(m.ctx, m.deps)
	default:
		return nil
	}
	return m.screen.Init()
}

// View returns nothing until the first size message, then exactly H rows.
func (m *Model) View() string {
	if m.w == 0 || m.h == 0 {
		return ""
	}
	if msg, small := frame.TooSmall(m.w, m.h); small {
		return msg
	}
	body := m.h - 3
	return frame.Screen(m.w, m.h, m.screen.Header(), m.screen.Body(m.w, body),
		frame.Footer(m.w, m.screen.Hints(m.w, body)...))
}
