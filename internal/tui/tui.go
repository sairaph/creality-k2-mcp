// Package tui implements creality-k2-mcp's interactive application
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section, T13b): the full-screen
// app a bare invocation opens on a terminal (main.go's runApp). It follows
// the mcp-wizard/app framework (menu, list, table, confirm, detail, async)
// already used by main.go's own menu/doctor screens, and reuses the same
// business logic the one-shot CLI commands (internal/clicmd) and the install
// wizard (internal/wizard) call: internal/wizard's exported registry
// merge/save functions for Printers, internal/printerstate's Take/
// DeriveActivityState/BuildStateBlock for Status, and internal/daemon's
// client for Camera and Recordings. No other package's exported API is
// changed to build this package.
package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/app"
	"github.com/sairaph/mcp-wizard/app/detail"
	"github.com/sairaph/mcp-wizard/app/menu"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerclient"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
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
// supply fakes for discovery, printer state and the daemon without ever
// touching a real registry file, printer or the network (AGENTS.md hard
// testing rule), and without ever launching a real browser.
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

	// RunDoctor runs every doctor check and renders the report as text, for
	// the Doctor screen. Set by main.go from the same doctor.Runner the
	// `doctor` command uses.
	RunDoctor func(ctx context.Context) string
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
	if d.RunDoctor == nil {
		d.RunDoctor = func(ctx context.Context) string { return "" }
	}
	return d
}

// step identifies the current top-level screen.
const (
	stepMenu app.Step = iota
	stepDoctor
	stepPrinters
	stepStatus
	stepCamera
	stepRecordings
	stepSettings
)

// Model is the whole application's tea.Model.
type Model struct {
	app.AppModel
	ctx     context.Context
	deps    Deps
	version string

	menu   *menu.Model
	detail *detail.Model // doctor report

	printers   *printersScreen
	status     *statusScreen
	camera     *cameraScreen
	recordings *recordingsScreen
	settings   *settingsScreen
}

// newModel builds the application model, shared by Run and by this
// package's own tests (which drive Init/Update/View directly instead of
// through app.Run's real tea.Program, so a test never opens a terminal).
func newModel(ctx context.Context, version string, deps Deps) *Model {
	m := &Model{ctx: ctx, version: version, deps: deps.withDefaults()}
	m.menu = menu.New(domain.ServerName+" "+version, func() []menu.Item {
		return []menu.Item{
			{Label: "Printers", Action: "printers"},
			{Label: "Status", Action: "status"},
			{Label: "Camera", Action: "camera"},
			{Label: "Recordings", Action: "recordings"},
			{Label: "Settings", Action: "settings"},
			{Label: "Run doctor", Action: "doctor"},
			{Label: "Quit", Action: "quit"},
		}
	})
	return m
}

// Run starts the interactive application. version is shown in the menu
// title, matching main.go's existing convention.
func Run(ctx context.Context, version string, deps Deps) int {
	return app.Run(ctx, newModel(ctx, version, deps), app.Options{Title: domain.ServerName, Version: version})
}

func (m *Model) Init() tea.Cmd { return m.menu.Init() }

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handled, cmd := m.HandleGlobalKeys(msg); handled {
		return m, cmd
	}

	if msg, ok := msg.(doctorLoadedMsg); ok {
		m.Status = ""
		if msg.err != nil {
			m.detail.SetContent("Doctor failed: " + msg.err.Error())
		} else {
			m.detail.SetContent(msg.report)
		}
		return m, nil
	}

	// app.ActionMsg is only handled specially here for the two top-level
	// concerns: the main menu (only while it is the active screen; every
	// other screen's own menu/list/confirm/table subcomponents share the
	// same Source strings and must reach that screen's own Update instead,
	// so this never intercepts them while a sub-screen is active) and any
	// screen signalling "I'm done, go back to the menu" via
	// app.Action(<screen name>, "back") - a convention every screen below
	// uses so leaving a screen always looks the same from here. Anything
	// else falls through to the per-step routing below.
	if am, ok := msg.(app.ActionMsg); ok {
		switch am.Source {
		case "menu":
			if m.Step == stepMenu {
				return m.handleMenuAction(am)
			}
		case "printers", "status", "camera", "recordings", "settings":
			if am.Value == "back" {
				m.Step = stepMenu
				return m, nil
			}
		case "detail":
			if m.Step == stepDoctor && am.Value == "back" {
				m.Step = stepMenu
				return m, nil
			}
		}
	}

	// Route every other message to the active screen. Each screen's own
	// Update ignores message types it does not care about.
	switch m.Step {
	case stepMenu:
		return m, m.menu.Update(msg)
	case stepDoctor:
		if m.detail != nil {
			return m, m.detail.Update(msg)
		}
	case stepPrinters:
		return m, m.printers.Update(m.ctx, m.deps, msg)
	case stepStatus:
		return m, m.status.Update(m.ctx, m.deps, msg)
	case stepCamera:
		return m, m.camera.Update(m.ctx, m.deps, msg)
	case stepRecordings:
		return m, m.recordings.Update(m.ctx, m.deps, msg)
	case stepSettings:
		return m, m.settings.Update(msg)
	}
	return m, nil
}

func (m *Model) handleMenuAction(msg app.ActionMsg) (tea.Model, tea.Cmd) {
	action, _ := msg.Data.(string)
	if msg.Value == "quit" || action == "quit" {
		m.Quit = true
		return m, tea.Quit
	}
	return m.openScreen(action)
}

// openScreen switches to the screen named by a menu action, constructing
// and initializing it fresh every time so a screen never shows stale data
// from a previous visit.
func (m *Model) openScreen(action string) (tea.Model, tea.Cmd) {
	switch action {
	case "doctor":
		m.Step = stepDoctor
		m.Status = "Running checks..."
		m.detail = detail.New("Doctor", m.Status)
		runDoctor := m.deps.RunDoctor
		return m, tea.Batch(m.detail.Init(), func() tea.Msg {
			return doctorLoadedMsg{report: runDoctor(m.ctx)}
		})
	case "printers":
		m.Step = stepPrinters
		m.printers = newPrintersScreen()
		return m, m.printers.Init(m.ctx, m.deps)
	case "status":
		m.Step = stepStatus
		m.status = newStatusScreen()
		return m, m.status.Init(m.ctx, m.deps)
	case "camera":
		m.Step = stepCamera
		m.camera = newCameraScreen()
		return m, m.camera.Init(m.ctx, m.deps)
	case "recordings":
		m.Step = stepRecordings
		m.recordings = newRecordingsScreen()
		return m, m.recordings.Init(m.ctx, m.deps)
	case "settings":
		m.Step = stepSettings
		m.settings = newSettingsScreen()
		return m, m.settings.Init()
	}
	return m, nil
}

func (m *Model) View() string {
	switch m.Step {
	case stepDoctor:
		if m.detail != nil {
			return m.detail.View()
		}
	case stepPrinters:
		return m.printers.View()
	case stepStatus:
		return m.status.View()
	case stepCamera:
		return m.camera.View()
	case stepRecordings:
		return m.recordings.View()
	case stepSettings:
		return m.settings.View()
	}
	return m.menu.View()
}

// doctorLoadedMsg is the doctor report, run off the UI loop.
type doctorLoadedMsg struct {
	report string
	err    error
}
