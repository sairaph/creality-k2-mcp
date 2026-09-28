package doctorchecks

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sairaph/mcp-wizard/doctor"

	daemonclient "github.com/sairaph/creality_k2_mcp/internal/daemon/client"
)

// daemonProbeTimeout bounds the viewer.url round trip (Client.ViewerURL has
// no timeout of its own beyond ctx) independently of the Runner's own
// context, so a hung or half-dead daemon cannot make doctor block.
// Client.Ping already bounds its own round trip with its own aliveTimeout.
const daemonProbeTimeout = 2 * time.Second

// viewerHTTPTimeout bounds the follow-up plain HTTP GET to the viewer page
// itself, once its URL is known, separately from daemonProbeTimeout (which
// only bounds the socket call that returns that URL).
const viewerHTTPTimeout = 3 * time.Second

// notRunningHint explains what a stopped daemon means for this server's two
// daemon-backed features (dev_docs/plan-v0.1.0.md T11a, dev_docs/safety-
// architecture.md section 10 D2): idle heating cannot be armed without it (a
// heater set while the printer is idle is refused outright, never silently
// left unwatched), and opening the live camera view or starting a recording
// starts it automatically the next time either is used.
const notRunningHint = "not running; idle heating is refused while it is down (a heater set while the printer is " +
	"idle needs the watchdog armed to turn it off automatically), and opening the live camera view or starting a " +
	"recording starts it automatically on demand"

// DaemonCheck reports whether the background camera/idle-heat daemon
// (internal/daemon, run as the hidden `creality_k2_mcp camera serve`) is
// currently running; when it is, how many heaters are armed across the
// registry's enabled printers, and whether the local browser camera viewer
// page (dev_docs/plan-v0.1.0.md T11b, MethodViewerURL) answers.
//
// It never starts the daemon to check it: it uses internal/daemon/client
// built with client.WithoutAutostart, so every probe below (Ping, Status,
// ViewerURL) is read-only by construction, never the hand-rolled
// socket.Dial/conn.Call copies this check used before
// dev_docs/review-backlog.md item 29. The viewer.url call is only ever made
// once the Ping probe has already confirmed the daemon is alive on its own,
// so this never causes the daemon process itself to start - MethodViewerURL
// only lazily starts that already-running daemon's own internal HTTP
// listener, the same thing a real open_camera_view call would trigger.
type DaemonCheck struct {
	// Identities is every enabled printer's identity (registry Hostname, or
	// printerstate.UnverifiedIdentity when none is on file - see
	// identityForDoctor, doctorchecks.go), used to sum armed heaters across
	// the whole registry: the daemon has no "every armed identity" method,
	// only watchdog.status per identity.
	Identities []string
}

func (DaemonCheck) Name() string { return "Camera/idle-heat daemon" }

func (c DaemonCheck) Run(ctx context.Context) doctor.Result {
	cl, err := daemonclient.New(daemonclient.WithoutAutostart())
	if err != nil {
		return doctor.Result{Name: "Camera/idle-heat daemon", Status: doctor.Fail, Detail: err.Error()}
	}

	ping, alive := cl.Ping(ctx)
	if !alive {
		return doctor.Result{Name: "Camera/idle-heat daemon", Status: doctor.Warn, Detail: notRunningHint}
	}

	armed := 0
	for _, identity := range c.Identities {
		heaters, ok := cl.Status(ctx, identity)
		if !ok {
			continue
		}
		for _, h := range heaters {
			if h.Armed {
				armed++
			}
		}
	}

	viewerLine, viewerStatus := viewerURLLine(ctx, cl)

	detail := fmt.Sprintf("running (pid %d, up %s); %d watchdog(s) armed across %d enabled printer(s)",
		ping.PID, time.Since(ping.StartedAt).Round(time.Second), armed, len(c.Identities))
	detail += "\n" + viewerLine
	return doctor.Result{Name: "Camera/idle-heat daemon", Status: viewerStatus, Detail: detail}
}

// viewerURLLine fetches the local browser camera viewer's URL (T11b,
// MethodViewerURL, via the non-autostarting cl.ViewerURL) and sends it one
// plain GET, so doctor confirms the page a person would actually open really
// answers, not just that the daemon itself is up. The URL carries the
// daemon's per-session access token (ViewerURLResult's own doc comment);
// that token is stripped from the query string before it goes into doctor's
// output (terminal, or the TUI's own detail pane), so it never leaks into a
// screenshot or a saved log, while the GET itself still uses the real,
// unmasked URL.
func viewerURLLine(ctx context.Context, cl *daemonclient.Client) (string, doctor.Status) {
	urlCtx, cancel := context.WithTimeout(ctx, daemonProbeTimeout)
	rawURL, err := cl.ViewerURL(urlCtx, "")
	cancel()
	if err != nil || rawURL == "" {
		return "viewer URL: FAIL - could not fetch it from the daemon (viewer.url did not answer)", doctor.Warn
	}

	getCtx, cancel := context.WithTimeout(ctx, viewerHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(getCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Sprintf("viewer URL: FAIL - %v", err), doctor.Warn
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("viewer URL: FAIL - %s: %v", maskViewerURL(rawURL), err), doctor.Warn
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Sprintf("viewer URL: FAIL - %s answered HTTP %d", maskViewerURL(rawURL), resp.StatusCode), doctor.Warn
	}
	return fmt.Sprintf("viewer URL: ok (%s, HTTP %d)", maskViewerURL(rawURL), resp.StatusCode), doctor.OK
}

// maskViewerURL drops the query string (which carries the access token, and
// the optional printer id) from a viewer URL before it is ever printed, kept
// or shown. An unparseable URL is returned as is, since there is then
// nothing structured to strip.
func maskViewerURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
