package doctorchecks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerclient"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// printersConcurrency and printerTimeout bound the per-printer gather this
// check runs, matching internal/clicmd's own "printers" list command
// (printersListConcurrency, printersListTimeout in internal/clicmd/printers.go)
// so a dead or slow printer never makes doctor noticeably slower than that
// command already is.
const (
	printersConcurrency = 5
	printerTimeout      = 5 * time.Second
)

// signalingPath and cameraDefaultPort mirror internal/camera/signaling.go's
// own unexported signalingPath and defaultCameraPort (the printer's WebRTC
// signaling endpoint, references/analysis/04-creality-ws-camera.md section
// 3): http://<host>:8000/call/webrtc_local. Kept as a local copy, not an
// import, because internal/camera exports no URL builder and this check
// only ever sends a plain GET - never an offer, never a WebRTC session - so
// it has no other reason to depend on that package.
const (
	signalingPath     = "/call/webrtc_local"
	cameraDefaultPort = "8000"
)

// cameraProbeTimeout bounds the signaling GET on its own, nested inside
// whatever budget the caller already gave checkOnePrinter.
const cameraProbeTimeout = 3 * time.Second

// PrinterClientsFunc builds the Moonraker/9999 clients for one printer,
// matching internal/printerclient.Default's shape (an unnamed function type,
// so it assigns here directly without a wrapper).
type PrinterClientsFunc func(domain.Printer) printerstate.Deps

// PrintersCheck probes every enabled printer: Moonraker reachability and
// derived activity state, port 9999 reachability and model identification,
// the camera signaling endpoint's reachability, and the registry's own
// allow_control flag. Different printers are probed concurrently (bounded),
// each within its own timeout, so one dead printer never blocks the rest;
// every probe failure carries a concrete, printer-specific hint.
type PrintersCheck struct {
	Printers []domain.Printer // enabled printers only

	// PrinterClients builds the clients for one printer. Nil defaults to
	// internal/printerclient.Default (real network clients); tests supply a
	// fake so no test ever opens a non-loopback socket (AGENTS.md hard
	// testing rule).
	PrinterClients PrinterClientsFunc

	// Concurrency and PerPrinterTimeout override the package defaults
	// (printersConcurrency, printerTimeout). A test shrinks
	// PerPrinterTimeout so an unreachable-host case stays fast.
	Concurrency       int
	PerPrinterTimeout time.Duration
}

func (PrintersCheck) Name() string { return "Printers" }

func (c PrintersCheck) Run(ctx context.Context) doctor.Result {
	if len(c.Printers) == 0 {
		return doctor.Result{Name: "Printers", Status: doctor.OK, Detail: "no enabled printers (see Registry)"}
	}

	pc := c.PrinterClients
	if pc == nil {
		pc = printerclient.Default
	}
	concurrency := c.Concurrency
	if concurrency <= 0 {
		concurrency = printersConcurrency
	}
	timeout := c.PerPrinterTimeout
	if timeout <= 0 {
		timeout = printerTimeout
	}

	blocks := make([]printerBlock, len(c.Printers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i, p := range c.Printers {
		i, p := i, p
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			blocks[i] = checkOnePrinter(pctx, pc, p)
		}()
	}
	wg.Wait()

	status := doctor.OK
	lines := make([]string, 0, len(blocks))
	for _, b := range blocks {
		lines = append(lines, b.text)
		if worse(b.status, status) {
			status = b.status
		}
	}
	return doctor.Result{Name: "Printers", Status: status, Detail: strings.Join(lines, "\n\n")}
}

// worse reports whether candidate outranks current on the fail > warn > ok
// severity order, for folding several probes' statuses into one overall
// Result.Status.
func worse(candidate, current doctor.Status) bool {
	return statusRank(candidate) > statusRank(current)
}

func statusRank(s doctor.Status) int {
	switch s {
	case doctor.Fail:
		return 2
	case doctor.Warn:
		return 1
	default:
		return 0
	}
}

type printerBlock struct {
	status doctor.Status
	text   string
}

// checkOnePrinter runs all four per-printer probes and folds them into one
// block: a header line naming the printer, then one indented line per
// probe.
func checkOnePrinter(ctx context.Context, pc PrinterClientsFunc, p domain.Printer) printerBlock {
	deps := pc(p)
	snap := printerstate.Take(ctx, deps, p)
	derived := printerstate.DeriveActivityState(snap, nil)

	moonrakerStatus, moonrakerLine := moonrakerProbeResult(snap, derived, p)
	wsStatus, wsLine := ws9999ProbeResult(snap)
	cameraStatus, cameraLine := cameraProbeResult(ctx, p)
	controlLine := "control: " + onOff(p.AllowControl)

	status := doctor.OK
	for _, s := range []doctor.Status{moonrakerStatus, wsStatus, cameraStatus} {
		if worse(s, status) {
			status = s
		}
	}

	header := fmt.Sprintf("%s (%s, %s):", p.ID, p.Name, p.Host)
	text := strings.Join([]string{
		header,
		"  " + moonrakerLine,
		"  " + wsLine,
		"  " + cameraLine,
		"  " + controlLine,
	}, "\n")
	return printerBlock{status: status, text: text}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// moonrakerProbeResult classifies snap's server/info result and reports the
// derived activity state and bucket alongside it. A connect failure is a
// Fail with a hint to check power, network and the registry's api_key; a
// reachable Moonraker whose Klippy is not ready is only a Warn, since
// Moonraker itself answered fine.
func moonrakerProbeResult(snap printerstate.Snapshot, derived printerstate.Derived, p domain.Printer) (doctor.Status, string) {
	if snap.ServerInfoErr != nil {
		hint := fmt.Sprintf("check that the printer is powered on and reachable at %s:%d, and the api_key in the registry if one is configured",
			p.Host, p.MoonrakerPort)
		var merr *moonraker.Error
		if errors.As(snap.ServerInfoErr, &merr) && merr.Code == moonraker.CodeAuthentication {
			hint = "Moonraker rejected the request with an authentication error; check the printer's api_key in the registry"
		}
		return doctor.Fail, fmt.Sprintf("moonraker: FAIL - %v (%s)", snap.ServerInfoErr, hint)
	}
	if !snap.ServerInfo.KlippyConnected || snap.ServerInfo.KlippyState != "ready" {
		return doctor.Warn, fmt.Sprintf(
			"moonraker: reachable, but klippy is not ready (klippy_connected=%v, klippy_state=%q); check the printer's screen for an error",
			snap.ServerInfo.KlippyConnected, snap.ServerInfo.KlippyState)
	}
	// checkIdentity (internal/printerstate/state.go, review backlog item 24)
	// corroborates the registry's persisted hostname against a fresh
	// printer/info read before anything else is trusted; a mismatch means the
	// printer now answering at this address may not be the one this registry
	// entry was set up for (e.g. DHCP moved the IP), so it is a Fail, not a
	// Warn - every write is refused until it is resolved. An unverified
	// identity (printer/info itself failed or came back empty) is a Warn:
	// Moonraker answered fine, only this one corroborating read did not.
	switch derived.State {
	case printerstate.StateIdentityMismatch:
		return doctor.Fail, fmt.Sprintf("moonraker: FAIL - %s", strings.Join(derived.Reasons, "; "))
	case printerstate.StateIdentityUnverified:
		return doctor.Warn, fmt.Sprintf("moonraker: %s", strings.Join(derived.Reasons, "; "))
	}
	return doctor.OK, fmt.Sprintf("moonraker: ok (klippy_state=%s) - state=%s bucket=%s",
		snap.ServerInfo.KlippyState, derived.State, derived.Bucket)
}

// ws9999ProbeResult classifies the port 9999 read printerstate.Take already
// performed (snap.WS9999*), so this never opens a second connection to the
// same printer.
func ws9999ProbeResult(snap printerstate.Snapshot) (doctor.Status, string) {
	if !snap.WS9999Reachable {
		return doctor.Fail, fmt.Sprintf(
			"port 9999: FAIL - %v (verify the printer's firmware exposes port 9999 and no firewall blocks it)", snap.WS9999Err)
	}
	if snap.WS9999.Model == "" {
		return doctor.Warn, "port 9999: reachable, but reported no model (this printer may not identify as a K2)"
	}
	return doctor.OK, fmt.Sprintf("port 9999: ok (model=%s)", snap.WS9999.Model)
}

// cameraProbeResult sends a bare GET to the printer's WebRTC signaling
// path, never an offer and never a WebRTC session: any HTTP response (even
// a 404/405 from a printer expecting only POST) proves the endpoint is up,
// so only a transport-level failure counts as unreachable.
func cameraProbeResult(ctx context.Context, p domain.Printer) (doctor.Status, string) {
	url := "http://" + cameraHostPort(p.Host) + signalingPath
	ctx, cancel := context.WithTimeout(ctx, cameraProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return doctor.Fail, fmt.Sprintf("camera: FAIL - %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return doctor.Fail, fmt.Sprintf(
			"camera: FAIL - %s: %v (verify the printer's camera/webrtc service is running and port %s is reachable)",
			url, err, cameraDefaultPort)
	}
	defer resp.Body.Close()
	return doctor.OK, fmt.Sprintf("camera: ok (%s answered HTTP %d)", url, resp.StatusCode)
}

// cameraHostPort appends cameraDefaultPort to host unless host already names
// one (a "host:port" pair, used by tests pointing at a fake signaling server
// on an arbitrary local port), matching internal/camera's own
// normalizeHostPort.
func cameraHostPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, cameraDefaultPort)
}
