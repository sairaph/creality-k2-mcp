package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// registryAccess is the production StateSource, plus the watchdog's
// DirectStateSource/DirectTemplateSender: StateSource resolves a printer
// identity against the global registry (domain.LoadRegistry, used only by
// Recorder's until:"print_end" polling, and by the viewer's printer
// listing); SnapshotDirect/TurnOffDirect (D2, review backlog item 36) never
// consult the registry at all, they build clients straight from the
// host/port/API key the caller supplies, so the idle-heat watchdog can
// still reach an env-override printer (K2_MCP_HOST) at expiry even though
// that printer is never saved to the registry file. Every method talks to
// the printer with a fresh connection: the daemon outlives any one MCP
// session, so it must never depend on a connection opened by whichever
// process happened to arm the watchdog.
type registryAccess struct{}

// resolve looks up identity (a Klipper hostname, falling back to host, the
// same rule internal/policy's printerIdentity uses) in the global registry.
func (registryAccess) resolve(identity string) (domain.Printer, bool) {
	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		return domain.Printer{}, false
	}
	for _, p := range reg.Printers {
		if matchesIdentity(p, identity) {
			return p, true
		}
	}
	return domain.Printer{}, false
}

func matchesIdentity(p domain.Printer, identity string) bool {
	if h := strings.TrimSpace(p.Hostname); h != "" {
		return h == identity
	}
	return strings.TrimSpace(p.Host) == identity
}

func clientsFor(p domain.Printer) printerstate.Deps {
	return printerstate.Deps{
		Moonraker: moonraker.New(fmt.Sprintf("http://%s:%d", p.Host, p.MoonrakerPort), p.APIKey),
		WS9999:    crealityws.New(p.Host, 0),
	}
}

// Snapshot implements StateSource.
func (r registryAccess) Snapshot(ctx context.Context, identity string) (printerstate.Snapshot, printerstate.Derived, bool) {
	p, ok := r.resolve(identity)
	if !ok {
		return printerstate.Snapshot{}, printerstate.Derived{}, false
	}
	snap := printerstate.Take(ctx, clientsFor(p), p)
	derived := printerstate.DeriveActivityState(snap, nil)
	return snap, derived, true
}

// SnapshotDirect implements DirectStateSource (D2, review backlog item 36):
// it builds clients straight from host/moonrakerPort/apiKey, with no
// registry lookup at all, so the watchdog can still reach a printer known
// only through the K2_MCP_HOST environment override (never saved to the
// registry file) when its idle-heat arm expires. The resulting
// domain.Printer carries no persisted Hostname (there is nothing to verify
// it against here; printerstate.DeriveActivityState's own identity check is
// a no-op for an empty Hostname), so the caller (Watchdog.expire) is the one
// that compares hostname against the identity it armed under.
func (r registryAccess) SnapshotDirect(ctx context.Context, host string, moonrakerPort int, apiKey string) (snap printerstate.Snapshot, derived printerstate.Derived, hostname string, ok bool) {
	p := domain.Printer{Host: host, MoonrakerPort: moonrakerPort, APIKey: apiKey}
	snap = printerstate.Take(ctx, clientsFor(p), p)
	if snap.PrinterInfoErr != nil {
		return snap, printerstate.Derived{}, "", false
	}
	derived = printerstate.DeriveActivityState(snap, nil)
	return snap, derived, strings.TrimSpace(snap.PrinterInfo.Hostname), true
}

// TurnOffDirect implements DirectTemplateSender: SET_HEATER_TEMPERATURE
// HEATER=<heater> TARGET=0, the only command the watchdog is ever allowed
// to send, sent straight to host/moonrakerPort/apiKey with no registry
// lookup (D2, review backlog item 36).
func (r registryAccess) TurnOffDirect(ctx context.Context, host string, moonrakerPort int, apiKey, heater string) error {
	client := moonraker.New(fmt.Sprintf("http://%s:%d", host, moonrakerPort), apiKey)
	return client.RunTemplate(ctx, moonraker.TemplateSetHeaterTemperature, map[string]string{"heater": heater, "target": "0"})
}

// NewProductionOptions builds Options wired to real printer registry
// lookups, real Moonraker/9999 clients and real camera.Open: T11a's
// production wiring, used by main.go's hidden `camera serve` command.
// Recorder is T11c's recording component, wired to the same Hub and to
// registryAccess as its StateSource (the same source the Watchdog uses).
// Its recordings directory defaults to DefaultRecordingsDir; if that cannot
// be resolved, recordings are written under paths.Dir's own parent instead
// so the daemon can still start.
func NewProductionOptions(paths Paths) Options {
	access := registryAccess{}
	hub := NewHub(nil)
	recDir, err := DefaultRecordingsDir()
	if err != nil {
		recDir = filepath.Join(filepath.Dir(paths.Dir), recordingsSubdir)
	}
	return Options{
		Paths:    paths,
		Hub:      hub,
		Watchdog: NewWatchdog(access, access),
		Recorder: NewRecorder(recDir, hub, access, nil),
		Printers: access,
	}
}
