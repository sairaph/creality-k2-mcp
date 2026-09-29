// Package clicmd implements creality-k2-mcp's one-shot CLI commands
// (dev_docs/plan-v0.1.0.md's "TUI and CLI" section, T13a): printers, status
// and snapshot. Every command calls the same business-logic functions the
// MCP tools (internal/mcpserver) and, later, the TUI (T13b) call -
// internal/wizard, internal/discovery, internal/printerstate and
// internal/camera - so the three surfaces never disagree about what a
// printer's state or a scan result means (dev_docs/plan-v0.1.0.md decision
// 4's "the wizard step, the unattended install path, the TUI and the CLI all
// share one implementation").
package clicmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sairaph/mcp-wizard/tui"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	daemonclient "github.com/sairaph/creality-k2-mcp/internal/daemon/client"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerclient"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/creality-k2-mcp/internal/wizard"
)

// CameraSnapshotFunc matches internal/daemon/client.Client.Snapshot's
// signature, injected so a test never opens a real WebRTC session or talks
// to the real daemon (AGENTS.md hard testing rule).
type CameraSnapshotFunc func(ctx context.Context, host string) (*camera.SnapshotResult, error)

// CameraViewer resolves the local browser camera viewer URL for "camera
// open", matching internal/mcpserver.CameraViewer's shape exactly so
// internal/daemon/client.Client (the production implementation, wired in by
// withDefaults below) satisfies both interfaces structurally. This package
// must not import internal/mcpserver (deps.go's own doc comment on
// PrinterClientsFunc explains why), so the interface is declared again here
// rather than reused.
type CameraViewer interface {
	// ViewerURL returns the local browser viewer URL for printerID (empty
	// shows every enabled printer).
	ViewerURL(ctx context.Context, printerID string) (string, error)
}

// CameraRecorder starts, stops, lists and deletes recordings through the
// background daemon, for "camera record|stop|recordings|delete". It matches
// internal/mcpserver.CameraRecorder's shape exactly, for the same reason
// CameraViewer above does.
type CameraRecorder interface {
	StartRecording(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error)
	StopRecording(ctx context.Context, id string) (daemon.RecordingInfo, error)
	ListRecordings(ctx context.Context) (daemon.RecordingListResult, error)
	DeleteRecording(ctx context.Context, id string) error
}

// Opener opens url in the user's default browser, for "camera open". It is
// injectable so a test never actually launches a browser process (AGENTS.md
// hard testing rule): tests set deps.Opener to a fake that just records the
// URL it was called with.
type Opener func(url string) error

// PrinterClientsFunc builds the printerstate.Deps a command needs to read one
// printer, matching internal/mcpserver.PrinterClients's shape so status and
// snapshot use exactly the same seam the MCP tools do (they cannot share the
// type itself: this package must not import internal/mcpserver).
type PrinterClientsFunc func(p domain.Printer) printerstate.Deps

// Deps bundles everything a one-shot command needs beyond its own arguments,
// so a test can supply fakes for the registry directory, discovery, probing,
// printer clients and the camera without touching a real printer, the real
// registry file or the network (AGENTS.md hard testing rule).
type Deps struct {
	// Dir resolves which registry file a command reads and writes
	// (domain.RegistryPath): the project file when Dir names a directory
	// that already has one, otherwise the global file. Empty means the
	// global file only.
	Dir string

	// Discover runs a network scan for "printers scan". Nil defaults to
	// discovery.Discover (through wizard.ScanAndMerge).
	Discover wizard.DiscoverFunc
	// Probe identifies one manually entered host for "printers add". Nil
	// defaults to discovery.ProbeHost (through wizard.ProbeAndMerge).
	Probe wizard.ProbeFunc

	// PrinterClients builds per-printer Moonraker/9999 clients for "status"
	// (and the light state check "snapshot" runs first). Defaults to
	// internal/printerclient.Default.
	PrinterClients PrinterClientsFunc
	// CameraSnapshot captures a camera frame for "snapshot", through the
	// background daemon's hub (review backlog item 51: the rolling GOP
	// buffer, kept warm across repeated calls, not a fresh independent
	// WebRTC session per call). NewDefaultDeps wires this up to the same
	// internal/daemon/client.Client instance as CameraViewer/CameraRecorder
	// (left nil if that client could not be built); withDefaults
	// deliberately leaves a nil value alone rather than defaulting it to
	// any other capture path - see CameraViewer's own doc comment below for
	// why ("no direct fallback, one path").
	CameraSnapshot CameraSnapshotFunc

	// CameraViewer resolves "camera open"'s viewer URL, autostarting the
	// background daemon. NewDefaultDeps wires this up to a fresh
	// internal/daemon/client.Client (left nil if that client could not be
	// built, e.g. this process's own executable path could not be resolved,
	// in which case "camera open" reports the viewer as unavailable rather
	// than panicking); withDefaults deliberately leaves a nil value alone
	// rather than building one itself, matching mcpserver.Deps's own
	// CameraViewer/CameraRecorder contract (main.go is the only place that
	// wires a real client in, so a test that wants "unavailable" behavior can
	// leave this nil and trust it stays nil).
	CameraViewer CameraViewer
	// CameraRecorder backs "camera record|stop|recordings|delete". See
	// CameraViewer above: NewDefaultDeps wires both up to the same
	// internal/daemon/client.Client instance (one daemon client covers both
	// seams, matching main.go's own newWatchdogClient/CameraViewer/
	// CameraRecorder wiring); withDefaults never builds one on its own.
	CameraRecorder CameraRecorder
	// Opener opens "camera open"'s viewer URL in the user's default browser.
	// Defaults to defaultOpener (rundll32 on Windows, open on macOS,
	// xdg-open elsewhere).
	Opener Opener
	// IsInteractive reports whether this process can prompt for input, used
	// by "camera delete" to decide whether an interactive confirmation is
	// even possible. Defaults to github.com/sairaph/mcp-wizard/tui.IsInteractive.
	IsInteractive func() bool
	// Confirm prompts with the given text and reports whether the answer was
	// affirmative, used by "camera delete" without --yes. Defaults to
	// promptConfirm, which reads one line from os.Stdin; a test should
	// always supply its own fake rather than let a test process block on
	// stdin.
	Confirm func(prompt string) (bool, error)

	// Now returns the current time, used only for "snapshot"'s default,
	// timestamped output file name. Defaults to time.Now.
	Now func() time.Time

	Stdout io.Writer
	Stderr io.Writer
}

// withDefaults fills in every unset field with its production default. Every
// exported Run function calls this first, so a caller only needs to set the
// fields a test actually wants to fake.
func (d Deps) withDefaults() Deps {
	if d.PrinterClients == nil {
		d.PrinterClients = printerclient.Default
	}
	// CameraSnapshot/CameraViewer/CameraRecorder are deliberately not
	// defaulted here: see their own doc comments on Deps above. Only
	// NewDefaultDeps wires a real internal/daemon/client.Client in; a nil
	// value here (never overwritten) is exactly what every camera
	// subcommand's "unavailable" branch expects, whether that nil came from
	// a test's fake Deps or from NewDefaultDeps itself having failed to
	// build one.
	if d.Opener == nil {
		d.Opener = defaultOpener
	}
	if d.IsInteractive == nil {
		d.IsInteractive = tui.IsInteractive
	}
	if d.Confirm == nil {
		d.Confirm = promptConfirm
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Stdout == nil {
		d.Stdout = os.Stdout
	}
	if d.Stderr == nil {
		d.Stderr = os.Stderr
	}
	return d
}

// NewDefaultDeps builds the production Deps: the current working directory
// as the registry directory (matching main.go's own registry resolution for
// the MCP server, loadMCPRegistry), a real internal/daemon/client.Client
// wired to both CameraViewer and CameraRecorder (matching main.go's own
// newWatchdogClient/CameraViewer/CameraRecorder wiring for the MCP server;
// left unset, so every camera subcommand reports the daemon as unavailable,
// if the client could not be built), and every other real dependency via
// withDefaults.
func NewDefaultDeps() Deps {
	dir, _ := os.Getwd()
	deps := Deps{Dir: dir}
	if c, err := daemonclient.New(); err == nil {
		deps.CameraViewer = c
		deps.CameraRecorder = c
		deps.CameraSnapshot = c.Snapshot
	} else {
		fmt.Fprintf(os.Stderr, "warning: camera daemon client unavailable: %v\n", err)
	}
	return deps.withDefaults()
}
