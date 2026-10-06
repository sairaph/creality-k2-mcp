package tui

import (
	"encoding/json"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

// addPrinter registers a printer in the isolated home's registry.
func addPrinter(t *testing.T, id, name, host string, enabled, control bool) domain.Printer {
	t.Helper()
	p := domain.Printer{
		ID: id, Name: name, Host: host, MoonrakerPort: 7125,
		Hostname: name, Enabled: enabled, AllowControl: control,
	}
	registerTestPrinter(t, "", p)
	return p
}

// idleMoonraker answers like a K2 at rest whose Klipper hostname is hostname,
// so the identity check passes and the derived state is idle.
func idleMoonraker(hostname string) *fakeMoonrakerClient {
	return &fakeMoonrakerClient{
		serverInfo:  fakeIdleServerInfo(),
		printerInfo: moonraker.PrinterInfoResult{Hostname: hostname},
		objects: map[string]json.RawMessage{
			"webhooks":       json.RawMessage(`{"state":"ready"}`),
			"print_stats":    json.RawMessage(`{"state":"standby"}`),
			"pause_resume":   json.RawMessage(`{"is_paused":false}`),
			"idle_timeout":   json.RawMessage(`{"state":"Ready"}`),
			"virtual_sdcard": json.RawMessage(`{"is_active":false}`),
		},
	}
}

// idleDeps is a Deps whose printers all read as idle K2s and whose daemon is
// the given fake (nil leaves it unwired).
func idleDeps(hostnames map[string]string, d *fakeDaemonClient) Deps {
	overrides := map[string]*fakeMoonrakerClient{}
	for id, hostname := range hostnames {
		overrides[id] = idleMoonraker(hostname)
	}
	deps := Deps{PrinterClients: fakePrinterClients(overrides)}
	if d != nil {
		deps.Daemon = d
	}
	return deps
}

// cleanIdleMoonraker is idleMoonraker plus everything a real idle K2 reports
// (heaters at temperature with target 0, fans, light, CFS box disconnected),
// so printerstate has nothing to fail closed on and Status shows no Details.
func cleanIdleMoonraker(hostname string) *fakeMoonrakerClient {
	f := idleMoonraker(hostname)
	f.objects["extruder"] = json.RawMessage(`{"temperature":24.5,"target":0}`)
	f.objects["heater_bed"] = json.RawMessage(`{"temperature":23.8,"target":0}`)
	f.objects["output_pin fan0"] = json.RawMessage(`{"value":0}`)
	f.objects["output_pin fan1"] = json.RawMessage(`{"value":0}`)
	f.objects["output_pin fan2"] = json.RawMessage(`{"value":0}`)
	f.objects["output_pin LED"] = json.RawMessage(`{"value":0}`)
	f.objects["box"] = json.RawMessage(`{"state":"disconnect"}`)
	return f
}
