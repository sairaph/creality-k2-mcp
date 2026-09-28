package daemon

import "time"

// The methods this package's Server registers on its socket.Server, and the
// only JSON-RPC surface the daemon exposes (T11a). internal/daemon/client
// is the sole caller in production; tests dial the socket directly.
const (
	MethodPing           = "ping"
	MethodWatchdogArm    = "watchdog.arm"
	MethodWatchdogDisarm = "watchdog.disarm"
	MethodWatchdogStatus = "watchdog.status"
	MethodViewerURL      = "viewer.url"
	MethodCameraStatus   = "camera.status"
	MethodCameraSnapshot = "camera.snapshot"
)

// PingResult answers MethodPing: proof the daemon is alive and reachable,
// for policy.Watchdog.Alive over IPC.
type PingResult struct {
	OK        bool      `json:"ok"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// ArmParams is MethodWatchdogArm's request, the wire shape of
// internal/policy.IdleHeatArmRequest. This package never imports
// internal/policy (internal/daemon/client does that translation at the IPC
// boundary) so the daemon's wire contract stays independent of policy's own
// in-process type.
type ArmParams struct {
	Identity   string  `json:"identity"`
	Heater     string  `json:"heater"`
	TargetC    float64 `json:"target_c"`
	ArmMinutes int     `json:"arm_minutes"`
	// Host, MoonrakerPort and APIKey are the live connection details the
	// watchdog needs to reach the printer directly at expiry, with no
	// registry lookup (dev_docs/safety-architecture.md section 10 D2,
	// review backlog item 36): a printer known only through the
	// K2_MCP_HOST environment override is never saved to the registry
	// file, so a registry-backed lookup by identity could never find it.
	// APIKey travels over this local, loopback-only socket only; the
	// daemon keeps it in process memory for as long as the heater stays
	// armed and never logs it (see Server.handleArm).
	Host          string `json:"host"`
	MoonrakerPort int    `json:"moonraker_port"`
	APIKey        string `json:"api_key,omitempty"`
}

// ArmResult answers MethodWatchdogArm.
type ArmResult struct {
	OK bool `json:"ok"`
}

// DisarmParams is MethodWatchdogDisarm's request.
type DisarmParams struct {
	Identity string `json:"identity"`
}

// DisarmResult answers MethodWatchdogDisarm. Disarming is always accepted
// (a no-op when nothing is armed), so this carries no error case of its own.
type DisarmResult struct {
	OK bool `json:"ok"`
}

// StatusParams is MethodWatchdogStatus's request.
type StatusParams struct {
	Identity string `json:"identity"`
}

// StatusResult answers MethodWatchdogStatus: every heater this daemon has
// ever armed for Identity, armed or not, for get_printer_status.
type StatusResult struct {
	Heaters []HeaterStatus `json:"heaters"`
}

// ViewerURLParams is MethodViewerURL's request (T11b). PrinterID, when
// non-empty, preselects that printer's stream on the returned page (the
// URL's "printer" query parameter); empty shows every enabled printer.
type ViewerURLParams struct {
	PrinterID string `json:"printer_id,omitempty"`
}

// ViewerURLResult answers MethodViewerURL: the local browser viewer's base
// URL, already carrying the per-daemon access token (and the printer query
// parameter, if ViewerURLParams.PrinterID was set). The viewer HTTP server
// is started lazily on the first call to this method.
type ViewerURLResult struct {
	URL string `json:"url"`
}

// CameraStatusParams is MethodCameraStatus's request (review backlog item
// 47): Host is the printer's camera host (Hub.Subscribe's own key), not an
// identity - the hub has never had any concept of a verified Klipper
// hostname, only the host a viewer or recording actually connects to.
type CameraStatusParams struct {
	Host string `json:"host"`
}

// CameraStatusResult answers MethodCameraStatus: the daemon hub's live
// camera connection state for one printer, for get_printer_status to
// surface alongside 9999's own camera_video_flag (review backlog item 47).
// This never starts a connection just to answer the query: Connected is
// simply false when the hub has no open entry for Host right now (no viewer
// or recording currently subscribed).
type CameraStatusResult struct {
	Connected   bool      `json:"connected"`
	HasMedia    bool      `json:"has_media"`
	LastMediaAt time.Time `json:"last_media_at,omitempty"`
}

// CameraSnapshotParams is MethodCameraSnapshot's request (review backlog
// item 51): Host is the printer's camera host (Hub.Subscribe's own key),
// matching CameraStatusParams.
type CameraSnapshotParams struct {
	Host string `json:"host"`
}

// CameraSnapshotResult answers MethodCameraSnapshot: one decoded camera
// frame, carried over the socket as JPEG bytes (ImageJPEG) so the wire
// contract stays plain JSON. internal/daemon/client.Client.Snapshot decodes
// ImageJPEG back into an image.Image for its caller, which is what lets
// internal/mcpserver/tools_camera.go's fitJPEG (downscale-then-recompress
// to fit the MCP result size cap) keep working unchanged: it operates on a
// decoded image, not a fixed pre-encoded JPEG.
type CameraSnapshotResult struct {
	ImageJPEG  []byte    `json:"image_jpeg"`
	Width      int       `json:"width"`
	Height     int       `json:"height"`
	CapturedAt time.Time `json:"captured_at"`
}
