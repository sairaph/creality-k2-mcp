package mcpserver

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/discovery"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// --- list_printers ---

func TestListPrintersHappyPath(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "list_printers", nil)
	if res.IsError {
		t.Fatalf("list_printers failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{
		"id: k2", "name: k2", "host: 127.0.0.1", "allow_control: true",
		"reachable: true", "activity_state: idle", "summary:",
		"wizard", "TUI",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("list_printers reply missing %q:\n%s", want, text)
		}
	}
}

func TestListPrintersUnreachableIsNotAnError(t *testing.T) {
	moon := fakeMoonraker(t, nil)
	wsHost, wsPort := fake9999(t)
	printer := testPrinter("k2", moon, wsHost, wsPort)
	moon.Close() // nothing listens at moon.URL any more

	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
	}
	cs := testSession(t, deps)
	res := call(t, cs, "list_printers", nil)
	if res.IsError {
		t.Fatalf("list_printers reported an error for an unreachable printer: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "reachable: false") {
		t.Fatalf("list_printers reply missing reachable: false:\n%s", text)
	}
	if !strings.Contains(text, "activity_state: offline") {
		t.Fatalf("list_printers reply missing activity_state: offline:\n%s", text)
	}
}

func TestListPrintersNoneEnabled(t *testing.T) {
	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1}, nil
		},
	}
	cs := testSession(t, deps)
	res := call(t, cs, "list_printers", nil)
	if res.IsError {
		t.Fatalf("list_printers failed with no enabled printers: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "printers: []") && !strings.Contains(text, "No printer is enabled") {
		t.Fatalf("list_printers reply for no enabled printers = %s", text)
	}
}

// --- discover_printers ---

// discoverFakeMoonraker runs a minimal loopback Moonraker fake answering
// exactly the two endpoints discovery's identify() calls
// (/printer/info, /server/info), mirroring internal/discovery's own
// fakeserver_test.go helper (unexported there, so duplicated here rather
// than shared across packages).
func discoverFakeMoonraker(t *testing.T, hostname string) (host string, port int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/printer/info":
			w.Write([]byte(`{"result": {"state": "ready", "hostname": "` + hostname + `"}}`))
		case "/server/info":
			w.Write([]byte(`{"result": {"klippy_connected": true, "klippy_state": "ready", "components": [], "api_version_string": "1.5.0"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse discover fake moonraker url %q: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse discover fake moonraker port from %q: %v", srv.URL, err)
	}
	return u.Hostname(), p
}

// withDiscoverPrinters overrides the discoverPrinters package var for the
// duration of the test, restoring the production default afterward.
func withDiscoverPrinters(t *testing.T, fn func(ctx context.Context) (discovery.Report, error)) {
	t.Helper()
	orig := discoverPrinters
	discoverPrinters = fn
	t.Cleanup(func() { discoverPrinters = orig })
}

func TestDiscoverPrintersFoundAndRegistered(t *testing.T) {
	moonHost, moonPort := discoverFakeMoonraker(t, "k2-disco")
	wsHost, wsPort := fake9999(t)
	_ = wsHost

	// Real discovery.Scan against a real, loopback-only listener (the same
	// pattern internal/discovery's own TestScanFindsAndIdentifiesRealListener
	// uses): a real dialer connecting only to 127.0.0.1, never a fake LAN
	// address, since identify() itself does not honour ScanOptions.Dialer
	// for its HTTP/WS calls, only for the initial TCP port check.
	withDiscoverPrinters(t, func(ctx context.Context) (discovery.Report, error) {
		rep := discovery.Scan(ctx, []string{moonHost}, discovery.ScanOptions{
			Port:        moonPort,
			WSPort:      wsPort,
			DialTimeout: time.Second,
			Budget:      5 * time.Second,
		})
		return rep, nil
	})

	existing := domain.Printer{ID: "existing", Name: "existing", Host: "10.0.0.9", Hostname: "k2-disco", Enabled: true}
	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{existing}}, nil
		},
	}
	cs := testSession(t, deps)
	res := call(t, cs, "discover_printers", nil)
	if res.IsError {
		t.Fatalf("discover_printers failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{"hostname: k2-disco", "identified_k2: true", "registered: true", "registered_as: existing (existing)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("discover_printers reply missing %q:\n%s", want, text)
		}
	}
}

func TestDiscoverPrintersFoundAndNotRegistered(t *testing.T) {
	moonHost, moonPort := discoverFakeMoonraker(t, "k2-new")
	wsHost, wsPort := fake9999(t)
	_ = wsHost

	withDiscoverPrinters(t, func(ctx context.Context) (discovery.Report, error) {
		rep := discovery.Scan(ctx, []string{moonHost}, discovery.ScanOptions{
			Port:        moonPort,
			WSPort:      wsPort,
			DialTimeout: time.Second,
			Budget:      5 * time.Second,
		})
		return rep, nil
	})

	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1}, nil
		},
	}
	cs := testSession(t, deps)
	res := call(t, cs, "discover_printers", nil)
	if res.IsError {
		t.Fatalf("discover_printers failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{"hostname: k2-new", "identified_k2: true", "registered: false"} {
		if !strings.Contains(text, want) {
			t.Fatalf("discover_printers reply missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "registered_as:") {
		t.Fatalf("discover_printers reply should not carry registered_as for an unregistered printer:\n%s", text)
	}
}

func TestDiscoverPrintersReportsPartial(t *testing.T) {
	addr := &net.IPNet{IP: net.ParseIP("10.77.0.5").To4(), Mask: net.CIDRMask(24, 32)}
	blockingDialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	withDiscoverPrinters(t, func(ctx context.Context) (discovery.Report, error) {
		return discovery.Discover(ctx, discovery.Options{
			Interfaces: func() ([]discovery.InterfaceInfo, error) {
				return []discovery.InterfaceInfo{{Name: "eth-test", Up: true, Addrs: []net.Addr{addr}}}, nil
			},
			ScanOptions: discovery.ScanOptions{
				Dialer:         blockingDialer,
				DialTimeout:    2 * time.Second,
				MaxConcurrency: 4,
				Budget:         80 * time.Millisecond,
			},
		})
	})

	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "discover_printers", nil)
	if res.IsError {
		t.Fatalf("discover_printers failed on a partial scan: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "partial: true") {
		t.Fatalf("discover_printers reply missing partial: true:\n%s", text)
	}
	if !strings.Contains(text, "did not finish") {
		t.Fatalf("discover_printers body does not explain the partial scan:\n%s", text)
	}
}

// --- preset filtering across both registry and status tool groups ---

func TestReadOnlyToolsPresentInEveryPreset(t *testing.T) {
	names := []string{
		"list_printers", "discover_printers", "get_printer_status",
		"get_current_job", "list_job_history", "list_console_messages",
	}
	for _, preset := range []domain.ToolPreset{domain.PresetMonitor, domain.PresetCamera, domain.PresetControl} {
		deps, _ := singlePrinterDeps(t, preset, nil)
		cs := testSession(t, deps)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("ListTools under preset %s: %v", preset, err)
		}
		present := map[string]bool{}
		for _, tool := range res.Tools {
			present[tool.Name] = true
			if tool.Name == "list_printers" || tool.Name == "get_printer_status" {
				if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
					t.Fatalf("%s under preset %s: not marked read-only", tool.Name, preset)
				}
			}
		}
		for _, name := range names {
			if !present[name] {
				t.Fatalf("tool %s missing under preset %s (monitor tools must be in every preset)", name, preset)
			}
		}
	}
}
