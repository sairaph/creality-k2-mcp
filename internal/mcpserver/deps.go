package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/policy"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// RegistryLoader returns the current printer registry. Tool handlers call it
// fresh on every request rather than caching it on Server, so registry
// edits made through other means (the TUI, hand-editing printers.json, the
// K2_MCP_HOST env override) are always seen by the next call.
type RegistryLoader func() (domain.Registry, error)

// PrinterClients builds the printerstate.Deps (Moonraker and port 9999
// clients) a tool handler needs to read one printer, for printerstate.Take.
// Production code leaves this nil, which New replaces with
// internal/printerclient.Default (review backlog item 18: the single
// implementation shared with internal/clicmd and internal/doctorchecks);
// tests point it at fakes bound to httptest and loopback WebSocket servers
// so they never open a non-loopback socket or contact a real printer
// (AGENTS.md hard testing rules).
type PrinterClients func(p domain.Printer) printerstate.Deps

// PolicyExecutor is the seam this package depends on for per-action gating
// (dev_docs/safety-architecture.md section 5's "actions" list). internal/policy
// now backs it (T10) through policyAvailableAdapter below; the interface
// itself is left unchanged from the placeholder T7 tool groups were written
// against, so wiring the real package in needed no change to any existing
// caller.
//
// A nil PolicyExecutor (the zero value of Deps.Policy) is valid: callers
// that need action gating treat it as "no actions computed", leaving
// StateBlock.Actions empty, never as a reason to fail an otherwise-successful
// read.
type PolicyExecutor interface {
	// Actions reports the gated availability of every named action for one
	// printer's current derived state, for StateBlock.Actions, including the
	// printer's own allow_control gate. pending is the same value passed to
	// printerstate.DeriveActivityState for this snapshot.
	Actions(printer domain.Printer, derived printerstate.Derived, pending *printerstate.PendingAction) []printerstate.ActionGate
}

// policyAvailableAdapter adapts internal/policy.GatesFor (every action's gate
// for a given derived state and settings, dev_docs/safety-architecture.md
// section 3.2, plus the printer's allow_control gate) to the PolicyExecutor
// seam above, so get_printer_status's actions list comes from
// internal/policy itself rather than being left empty. pending is accepted
// (to satisfy the interface) but not used: every caller of
// PolicyExecutor.Actions today derives its Derived value with a nil pending
// already, and Derived's own Bucket/Class already reflect whatever pending
// transition was live when it was computed (P2, printerstate.Derived's own
// doc comment).
type policyAvailableAdapter struct{ settings domain.Settings }

func (a policyAvailableAdapter) Actions(printer domain.Printer, derived printerstate.Derived, _ *printerstate.PendingAction) []printerstate.ActionGate {
	return policy.GatesFor(printer, derived, a.settings)
}

// policyDeps builds the internal/policy.Deps a control-tool write needs for
// printer, from this server's existing PrinterClients seam: both
// internal/printerclient.Default and every test's own PrinterClients already
// return *moonraker.Client/*crealityws.Client concrete values (or an equivalent
// fake), which satisfy policy.MoonrakerClient/policy.WS9999Client's richer,
// superset interfaces structurally, so no separate client-construction seam
// is needed for control tools - the smallest possible adapter, two type
// assertions, matching the same pattern tools_status.go's own
// historyTotalsClient already uses to recover a richer interface from the
// same PrinterClients call. Deps.Watchdog is passed through as is: nil until
// T11a's background daemon lands, which internal/policy already treats the
// same as "not alive" (idle heating refused with a clear hint).
func (s *Server) policyDeps(printer domain.Printer) (policy.Deps, error) {
	base := s.deps.PrinterClients(printer)
	moon, ok := base.Moonraker.(policy.MoonrakerClient)
	if !ok {
		return policy.Deps{}, fmt.Errorf("printer client for %s does not implement policy.MoonrakerClient", printer.ID)
	}
	ws, ok := base.WS9999.(policy.WS9999Client)
	if !ok {
		return policy.Deps{}, fmt.Errorf("printer client for %s does not implement policy.WS9999Client", printer.ID)
	}
	return policy.Deps{Moonraker: moon, WS9999: ws, Watchdog: s.deps.Watchdog}, nil
}

// CameraSnapshotter captures one decoded frame from a printer's onboard
// camera, the seam get_camera_snapshot (tools_camera.go) calls through, so
// a test can supply a fake that returns a known image without ever opening
// a WebRTC session or a non-loopback socket (AGENTS.md hard testing
// rules). internal/daemon/client.Client.Snapshot is the production
// implementation (review backlog item 51: snapshots are taken from the
// background daemon's hub - its rolling GOP buffer, kept warm across
// repeated calls - not by opening a fresh, independent WebRTC session per
// call the way internal/camera.Snapshot used to), wired in from main.go on
// the same client instance as Deps.CameraViewer/Deps.CameraRecorder. A nil
// Deps.CameraSnapshot (until main.go wires one, or if it could not be)
// makes get_camera_snapshot report the camera as unavailable rather than
// falling back to any other capture path - there is deliberately only one
// path (owner design decision, review backlog item 51).
type CameraSnapshotter interface {
	// Snapshot captures one frame from host's camera (host only, no port:
	// the camera always serves on port 8000, matching internal/camera's own
	// normalizeHostPort default).
	Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error)
}

// CameraViewer resolves the local browser camera viewer URL for
// open_camera_view (tools_viewer.go, dev_docs/plan-v0.1.0.md T11b),
// autostarting the background daemon if it is not already running.
// internal/daemon/client.Client is the production implementation, wired in
// from main.go (the same client instance used for Deps.Watchdog): this
// package never talks to the daemon's socket directly, matching how
// Deps.Watchdog is wired. A nil Deps.CameraViewer, the only value available
// if this process's paths could not be resolved at startup, makes
// open_camera_view report the viewer as unavailable rather than panicking.
type CameraViewer interface {
	// ViewerURL returns the local browser viewer URL for printerID (empty
	// shows every enabled printer).
	ViewerURL(ctx context.Context, printerID string) (string, error)
}

// WatchdogStatusSource is the seam get_printer_status uses to surface D2's
// idle-heat watchdog liveness and armed heaters (dev_docs/safety-architecture.md
// section 10 D2, review backlog item 20): every heater the background daemon
// has armed for one printer identity, with its target and deadline.
// internal/daemon/client.Client.Status implements this exactly. It is wired
// in from main.go on its own client instance, built with
// client.WithoutAutostart (item 29), separate from the one behind
// Deps.Watchdog and Deps.CameraViewer: Status never autostarts the daemon,
// and this keeps that true by construction, not just by Status's own
// current implementation. A dial or call failure simply returns ok=false,
// which get_printer_status reports as "unknown", never as "not alive". A
// nil Deps.WatchdogStatus (until main.go wires one, or if it could not be)
// makes get_printer_status report the same "unknown" outcome.
type WatchdogStatusSource interface {
	// Status reports every heater the daemon has ever armed for identity,
	// armed or not. ok is false whenever the daemon could not be reached at
	// all (not running, or an IPC failure): the caller must bound ctx itself
	// (get_printer_status uses a short, fixed timeout) since this never
	// blocks past whatever deadline ctx already carries.
	Status(ctx context.Context, identity string) (heaters []daemon.HeaterStatus, ok bool)
}

// CameraStatusSource is the seam get_printer_status uses to surface the
// background daemon hub's live camera connection state (review backlog item
// 47): whether it currently has an open upstream connection for a printer's
// camera host, and when it last actually received video. internal/daemon/
// client.Client.CameraStatus implements this exactly, using the same
// client instance as Deps.WatchdogStatus (never autostarting, review
// backlog item 29's rule applied here too: a read-only status call must
// never start the daemon as a side effect). A nil Deps.CameraStatus (until
// main.go wires one, or if it could not be) makes get_printer_status leave
// this part of the state block out entirely, same as WatchdogStatus.
type CameraStatusSource interface {
	// CameraStatus reports host's camera connection state. ok is false
	// whenever the daemon could not be reached at all; connected is false
	// whenever the daemon answered but has no open connection for host right
	// now (no viewer or recording currently subscribed) - neither case is an
	// error, both are simply "nothing more is known right now".
	CameraStatus(ctx context.Context, host string) (lastMediaAt time.Time, hasMedia bool, connected bool, ok bool)
}

// CameraRecorder is the seam start_recording/stop_recording/list_recordings/
// delete_recording (tools_recording.go, dev_docs/plan-v0.1.0.md T11c) call
// through instead of talking to the background daemon's socket directly.
// internal/daemon/client.Client implements this exactly (structurally, the
// same pattern CameraViewer and WatchdogStatusSource already use), wired in
// from main.go alongside those. A nil Deps.CameraRecorder (until main.go
// wires one, or if it could not be) makes every recording tool report the
// recorder as unavailable rather than panicking.
type CameraRecorder interface {
	StartRecording(ctx context.Context, req daemon.RecordingStartParams) (daemon.RecordingInfo, error)
	StopRecording(ctx context.Context, id string) (daemon.RecordingInfo, error)
	ListRecordings(ctx context.Context) (daemon.RecordingListResult, error)
	DeleteRecording(ctx context.Context, id string) error
}

// Deps bundles everything a tool handler needs beyond its own arguments and
// the Server it is a method of, so tests can supply fakes for all of it
// without touching a real printer, the real registry file or the network.
type Deps struct {
	// Settings gates which tools New registers (registerTool) and carries
	// the bands/idle-heat configuration later control tools read.
	Settings domain.Settings
	// LoadRegistry loads the printer registry. Required: New panics if it is
	// nil, since every tool needs it to resolve a printer.
	LoadRegistry RegistryLoader
	// PrinterClients builds per-printer Moonraker/9999 clients. Defaults to
	// internal/printerclient.Default when nil.
	PrinterClients PrinterClients
	// Policy computes StateBlock.Actions. May be nil (see PolicyExecutor).
	// Defaults to policyAvailableAdapter (backed by PolicyEngine's settings)
	// when nil.
	Policy PolicyExecutor
	// PolicyEngine is the real internal/policy engine every control-tool
	// write goes through (dev_docs/safety-architecture.md section 3.2's
	// Execute). Defaults to a fresh policy.New() when nil: one Policy per
	// server, shared by every tool call, so its per-printer lock registry
	// and proposal-token store (section 3.3) behave as the package intends.
	// Production code and every test should normally leave this nil and let
	// New/newServer build the shared instance.
	PolicyEngine *policy.Policy
	// Watchdog is D2's idle-heat watchdog dependency (dev_docs/safety-architecture.md
	// section 10 D2), implemented by the background daemon a later task
	// (T11a) adds. A nil value, the only one available until T11a lands,
	// means idle heating is refused: internal/policy treats a nil Watchdog
	// exactly like "the daemon is not alive" and reports that in its error.
	Watchdog policy.Watchdog
	// CameraSnapshot captures a camera frame for get_camera_snapshot,
	// through the background daemon's hub (review backlog item 51). A nil
	// value (until main.go wires one, or if it could not be) makes
	// get_camera_snapshot report the camera as unavailable; there is no
	// other, direct fallback path.
	CameraSnapshot CameraSnapshotter
	// CameraViewer resolves open_camera_view's local browser URL. A nil
	// value (until main.go wires it, or if it could not be) makes
	// open_camera_view report the viewer as unavailable.
	CameraViewer CameraViewer
	// WatchdogStatus surfaces D2's idle-heat watchdog liveness and armed
	// heaters in get_printer_status (review backlog item 20). A nil value
	// (until main.go wires one, or if it could not be) makes
	// get_printer_status report the daemon's liveness as "unknown".
	WatchdogStatus WatchdogStatusSource
	// CameraStatus surfaces the background daemon hub's live camera
	// connection state in get_printer_status (review backlog item 47). A nil
	// value (until main.go wires one, or if it could not be) makes
	// get_printer_status leave that part of the state block out.
	CameraStatus CameraStatusSource
	// CameraRecorder backs start_recording/stop_recording/list_recordings/
	// delete_recording (T11c). A nil value (until main.go wires one, or if
	// it could not be) makes every recording tool report the recorder as
	// unavailable.
	CameraRecorder CameraRecorder
}

// derive is the one derivation every MCP tool that shows state or actions
// uses: the policy engine's (Policy.Derive), which adds this process's
// start-window record to printerstate's own derivation (plan-v0.2.0.md 8a.1),
// so the start window is reflected in every tool reply and never disagrees
// with the gates Execute enforces. Without a policy engine (a nil field in a
// hand-built Deps) it falls back to the plain derivation.
func (s *Server) derive(snap printerstate.Snapshot) printerstate.Derived {
	if s.deps.PolicyEngine != nil {
		return s.deps.PolicyEngine.Derive(snap)
	}
	return printerstate.DeriveActivityState(snap, nil)
}
