package doctorchecks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// fakeMoonraker implements printerstate.MoonrakerClient with no network I/O
// at all (AGENTS.md hard testing rule): every call returns exactly what the
// test configured.
type fakeMoonraker struct {
	serverInfo    moonraker.ServerInfoResult
	serverInfoErr error
	// printerInfo/printerInfoErr back PrinterInfo, which printerstate.Take
	// (review backlog item 24) now also reads to corroborate a registry
	// entry's persisted hostname; a test that expects an OK moonraker line
	// must set printerInfo.Hostname to match the test printer's own
	// domain.Printer.Hostname, or DeriveActivityState reports
	// identity_unverified instead (see checkIdentity, internal/printerstate/state.go).
	printerInfo    moonraker.PrinterInfoResult
	printerInfoErr error
}

func (f *fakeMoonraker) ServerInfo(context.Context) (moonraker.ServerInfoResult, error) {
	return f.serverInfo, f.serverInfoErr
}
func (f *fakeMoonraker) PrinterInfo(context.Context) (moonraker.PrinterInfoResult, error) {
	return f.printerInfo, f.printerInfoErr
}
func (f *fakeMoonraker) QueryObjects(context.Context, map[string][]string) (map[string]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeMoonraker) HistoryList(context.Context, int, int) (moonraker.HistoryList, error) {
	return moonraker.HistoryList{}, nil
}
func (f *fakeMoonraker) GCodeStore(context.Context, int) ([]moonraker.GCodeStoreEntry, error) {
	return nil, nil
}

var _ printerstate.MoonrakerClient = (*fakeMoonraker)(nil)

// fakeWS9999 implements printerstate.WS9999Client with no network I/O.
type fakeWS9999 struct {
	status crealityws.Status
	err    error
}

func (f *fakeWS9999) ReadStatus(context.Context) (crealityws.Status, error) {
	return f.status, f.err
}

var _ printerstate.WS9999Client = (*fakeWS9999)(nil)

// readyServerInfo is a Moonraker server/info response with Klippy fully up,
// the precondition moonrakerProbeResult treats as OK rather than Warn.
func readyServerInfo() moonraker.ServerInfoResult {
	return moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"}
}

// healthyMoonraker is a fakeMoonraker that answers with Klippy ready and a
// printer/info hostname matching hostname, so checkIdentity
// (internal/printerstate/state.go) verifies cleanly and moonrakerProbeResult
// reports OK rather than identity_unverified.
func healthyMoonraker(hostname string) *fakeMoonraker {
	return &fakeMoonraker{
		serverInfo:  readyServerInfo(),
		printerInfo: moonraker.PrinterInfoResult{Hostname: hostname},
	}
}

// cameraTestServer starts an httptest server bound to 127.0.0.1 (AGENTS.md
// hard testing rule: tests never open a non-loopback socket) and returns the
// bare "host:port" cameraHostPort expects as a printer's Host field.
func cameraTestServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func testPrinter(host string, allowControl bool) domain.Printer {
	return domain.Printer{
		ID: "k2-1", Name: "k2-1", Host: host, MoonrakerPort: 7125,
		Hostname: "k2-1.local", Enabled: true, AllowControl: allowControl,
	}
}

func TestPrintersCheckNoEnabledPrinters(t *testing.T) {
	res := PrintersCheck{}.Run(context.Background())
	if res.Status != doctor.OK {
		t.Fatalf("Status = %v, want OK", res.Status)
	}
	if !strings.Contains(res.Detail, "no enabled printers") {
		t.Errorf("Detail = %q, want it to say no enabled printers", res.Detail)
	}
}

func TestPrintersCheckAllHealthy(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("camera probe method = %s, want GET (never an offer)", r.Method)
		}
		w.WriteHeader(http.StatusNotFound) // the real endpoint only accepts POST
	})
	p := testPrinter(camHost, false)

	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: healthyMoonraker("k2-1.local"),
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.OK {
		t.Fatalf("Status = %v, Detail = %q, want OK", res.Status, res.Detail)
	}
	for _, want := range []string{"moonraker: ok", "port 9999: ok (model=F021)", "camera: ok", "control: off"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("Detail = %q, want it to contain %q", res.Detail, want)
		}
	}
}

func TestPrintersCheckMoonrakerUnreachableIsFail(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := testPrinter(camHost, false)

	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: &fakeMoonraker{serverInfoErr: errors.New("dial tcp: connection refused")},
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.Fail {
		t.Fatalf("Status = %v, Detail = %q, want Fail", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "moonraker: FAIL") || !strings.Contains(res.Detail, "check that the printer is powered on") {
		t.Errorf("Detail = %q, want a moonraker FAIL line with a concrete hint", res.Detail)
	}
}

func TestPrintersCheckMoonrakerAuthErrorGetsAuthHint(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := testPrinter(camHost, false)

	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: &fakeMoonraker{serverInfoErr: &moonraker.Error{Op: "ServerInfo", Status: 401, Code: moonraker.CodeAuthentication}},
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.Fail {
		t.Fatalf("Status = %v, want Fail", res.Status)
	}
	if !strings.Contains(res.Detail, "api_key") {
		t.Errorf("Detail = %q, want the authentication hint naming api_key", res.Detail)
	}
}

func TestPrintersCheckKlippyNotReadyIsWarn(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := testPrinter(camHost, false)

	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: &fakeMoonraker{serverInfo: moonraker.ServerInfoResult{KlippyConnected: false, KlippyState: "startup"}},
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.Warn {
		t.Fatalf("Status = %v, Detail = %q, want Warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "klippy is not ready") {
		t.Errorf("Detail = %q, want a klippy-not-ready line", res.Detail)
	}
}

func TestPrintersCheckWS9999UnreachableIsFail(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := testPrinter(camHost, false)

	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: healthyMoonraker("k2-1.local"),
				WS9999:    &fakeWS9999{err: errors.New("no usable frame within budget")},
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.Fail {
		t.Fatalf("Status = %v, Detail = %q, want Fail", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "port 9999: FAIL") {
		t.Errorf("Detail = %q, want a port 9999 FAIL line", res.Detail)
	}
}

func TestPrintersCheckWS9999NoModelIsWarn(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := testPrinter(camHost, false)

	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: healthyMoonraker("k2-1.local"),
				WS9999:    &fakeWS9999{status: crealityws.Status{}}, // Model == ""
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.Warn {
		t.Fatalf("Status = %v, Detail = %q, want Warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "reported no model") {
		t.Errorf("Detail = %q, want a no-model line", res.Detail)
	}
}

// A closed loopback port proves the camera probe fails closed on a real
// transport error while never leaving the loopback interface.
func TestPrintersCheckCameraUnreachableIsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	camHost := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // now refuses connections, still 127.0.0.1 only

	p := testPrinter(camHost, false)
	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: healthyMoonraker("k2-1.local"),
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.Fail {
		t.Fatalf("Status = %v, Detail = %q, want Fail", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "camera: FAIL") {
		t.Errorf("Detail = %q, want a camera FAIL line", res.Detail)
	}
}

func TestPrintersCheckControlStateReported(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := testPrinter(camHost, true)

	check := PrintersCheck{
		Printers: []domain.Printer{p},
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: healthyMoonraker("k2-1.local"),
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}
	res := check.Run(context.Background())
	if !strings.Contains(res.Detail, "control: on") {
		t.Errorf("Detail = %q, want control: on", res.Detail)
	}
}

// Overall status is the worst across every enabled printer: one healthy
// printer must never hide another one's failure.
func TestPrintersCheckOverallStatusIsWorstAcrossPrinters(t *testing.T) {
	okCam := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	failCam := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })

	printers := []domain.Printer{
		{ID: "k2-ok", Name: "k2-ok", Host: okCam, MoonrakerPort: 7125, Hostname: "k2-ok.local", Enabled: true},
		{ID: "k2-bad", Name: "k2-bad", Host: failCam, MoonrakerPort: 7125, Hostname: "k2-bad.local", Enabled: true},
	}
	check := PrintersCheck{
		Printers: printers,
		PrinterClients: func(p domain.Printer) printerstate.Deps {
			if p.ID == "k2-bad" {
				return printerstate.Deps{
					Moonraker: &fakeMoonraker{serverInfoErr: errors.New("connection refused")},
					WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
				}
			}
			return printerstate.Deps{
				Moonraker: healthyMoonraker("k2-ok.local"),
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}
	res := check.Run(context.Background())
	if res.Status != doctor.Fail {
		t.Fatalf("Status = %v, want Fail (one printer failed)", res.Status)
	}
	if !strings.Contains(res.Detail, "k2-ok") || !strings.Contains(res.Detail, "k2-bad") {
		t.Errorf("Detail = %q, want both printers reported", res.Detail)
	}
}

// PerPrinterTimeout actually bounds the gather: a Moonraker fake that never
// returns must not hang the test.
func TestPrintersCheckRespectsPerPrinterTimeout(t *testing.T) {
	camHost := cameraTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := testPrinter(camHost, false)

	check := PrintersCheck{
		Printers:          []domain.Printer{p},
		PerPrinterTimeout: 50 * time.Millisecond,
		PrinterClients: func(domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: &blockingMoonraker{},
				WS9999:    &fakeWS9999{status: crealityws.Status{Model: "F021"}},
			}
		},
	}

	done := make(chan doctor.Result, 1)
	go func() { done <- check.Run(context.Background()) }()
	select {
	case res := <-done:
		if res.Status != doctor.Fail {
			t.Errorf("Status = %v, Detail = %q, want Fail (timed out)", res.Status, res.Detail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PrintersCheck.Run did not honour PerPrinterTimeout")
	}
}

// blockingMoonraker never returns until its context is cancelled, standing
// in for a printer that accepts the TCP connection but never answers.
type blockingMoonraker struct{}

func (blockingMoonraker) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	<-ctx.Done()
	return moonraker.ServerInfoResult{}, ctx.Err()
}
func (blockingMoonraker) PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error) {
	<-ctx.Done()
	return moonraker.PrinterInfoResult{}, ctx.Err()
}
func (blockingMoonraker) QueryObjects(ctx context.Context, _ map[string][]string) (map[string]json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingMoonraker) HistoryList(ctx context.Context, _, _ int) (moonraker.HistoryList, error) {
	<-ctx.Done()
	return moonraker.HistoryList{}, ctx.Err()
}
func (blockingMoonraker) GCodeStore(ctx context.Context, _ int) ([]moonraker.GCodeStoreEntry, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

var _ printerstate.MoonrakerClient = blockingMoonraker{}
