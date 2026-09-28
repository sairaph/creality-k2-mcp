package policy

import (
	"context"
	"os"
	"testing"

	"github.com/sairaph/mcp-wizard/daemon/socket"

	"github.com/sairaph/creality_k2_mcp/internal/daemon"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// This file pins review backlog item 31 end to end: an env-override printer
// (K2_MCP_HOST, domain.EnvOverride - no persisted registry hostname at all)
// must have its idle-heat watchdog armed, queried and disarmed under the
// exact same identity, the live verified Klipper hostname
// resolveExecuteIdentity resolves from a fresh printer/info read, never the
// raw configured host. Before the fix, sendStartPrint's Disarm call and
// idleHeatArmFor's display both recomputed identity via the old
// printerIdentity "hostname, else host" fallback instead of using the
// identity Execute had already resolved and armed the watchdog under; for an
// env-override printer (Hostname always empty) that fallback returned the
// host, silently disarming (and displaying) the wrong key.

// socketWatchdog is a minimal policy.Watchdog wired to a real daemon.Server
// over its real socket, reimplementing the same round trip
// internal/daemon/client.Client makes (Arm/Alive/Disarm) plus a status query
// (Status), rather than importing that package: internal/daemon/client
// imports internal/policy, so an internal (package policy) test file here
// cannot import it back without an import cycle - exactly why
// internal/doctorchecks/daemon.go's own probePing/probeStatus reimplement
// the same round trip instead of importing internal/daemon/client.
type socketWatchdog struct {
	socketPath string
}

func (w socketWatchdog) Alive(ctx context.Context) bool {
	conn, err := socket.Dial(w.socketPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	var res daemon.PingResult
	return conn.Call(ctx, daemon.MethodPing, nil, &res) == nil && res.OK
}

func (w socketWatchdog) Arm(ctx context.Context, req IdleHeatArmRequest) error {
	conn, err := socket.Dial(w.socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	params := daemon.ArmParams{
		Identity: req.Identity, Heater: req.Heater, TargetC: req.TargetC, ArmMinutes: req.ArmMinutes,
		Host: req.Host, MoonrakerPort: req.MoonrakerPort, APIKey: req.APIKey,
	}
	var res daemon.ArmResult
	return conn.Call(ctx, daemon.MethodWatchdogArm, params, &res)
}

func (w socketWatchdog) Disarm(ctx context.Context, identity string) error {
	conn, err := socket.Dial(w.socketPath)
	if err != nil {
		return nil
	}
	defer conn.Close()
	var res daemon.DisarmResult
	return conn.Call(ctx, daemon.MethodWatchdogDisarm, daemon.DisarmParams{Identity: identity}, &res)
}

// status mirrors internal/daemon/client.Client.Status and get_printer_status's
// own watchdog query (internal/mcpserver/tools_status.go watchdogBlock): a
// read-only lookup of armed heaters for identity.
func (w socketWatchdog) status(ctx context.Context, identity string) (heaters []daemon.HeaterStatus, ok bool) {
	conn, err := socket.Dial(w.socketPath)
	if err != nil {
		return nil, false
	}
	defer conn.Close()
	var res daemon.StatusResult
	if err := conn.Call(ctx, daemon.MethodWatchdogStatus, daemon.StatusParams{Identity: identity}, &res); err != nil {
		return nil, false
	}
	return res.Heaters, true
}

var _ Watchdog = socketWatchdog{}

func startRealTestDaemon(t *testing.T) socketWatchdog {
	t.Helper()
	// A short, hand-rolled temp dir rather than t.TempDir(): that embeds the
	// full test name, which easily pushes "<dir>/daemon.sock" past the
	// AF_UNIX sun_path limit (daemon/paths.go's own maxSocketPathBytes,
	// review backlog item 21) for a long test name like this one's.
	dir, err := os.MkdirTemp("", "k2mcpd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	opts := daemon.NewProductionOptions(daemon.PathsIn(dir))
	srv, err := daemon.Open(opts)
	if err != nil {
		t.Fatalf("daemon.Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
	})
	return socketWatchdog{socketPath: opts.Paths.Socket}
}

func TestEnvOverridePrinter_ArmQueryDisarm_SameVerifiedIdentity(t *testing.T) {
	setTestHome(t)
	wd := startRealTestDaemon(t)

	f := newFakePrinter()
	f.addFile("model.gcode")
	f.setWatchdog(wd)
	// An env-override printer (domain.EnvOverride): no persisted Hostname at
	// all, and Host is an address, never an identity (dev_docs/safety-
	// architecture.md 3.4, domain/lock.go). PrinterInfo answers with the live
	// Klipper hostname resolveExecuteIdentity must resolve to and use as the
	// only identity - deliberately different from Host, so a test asserting
	// on Host alone would catch a regression back to the old fallback.
	const liveHostname = "env-live-host.local"
	f.setPrinterInfo(liveHostname, nil)
	printer := domain.Printer{
		ID: domain.EnvPrinterID, Name: "env override", Host: "203.0.113.5",
		MoonrakerPort: 7125, Enabled: true, AllowControl: true, // Hostname left empty
	}

	p := New()
	ctx := context.Background()

	// Arm: a heater set while idle (D2) arms the watchdog synchronously
	// inside Execute, before the heater command is sent.
	armRes := mustExecute(t, p, f, printer, ActionSetNozzleTemperature, Params{TargetC: 200}, "")
	if armRes.IdleHeatArm == nil {
		t.Fatal("expected an IdleHeatArmRequest for a heater set while idle (D2)")
	}
	if armRes.IdleHeatArm.Identity != liveHostname {
		t.Fatalf("IdleHeatArm.Identity = %q, want the verified live hostname %q, not the host", armRes.IdleHeatArm.Identity, liveHostname)
	}

	// Query status under the verified identity: this is the same key
	// get_printer_status's watchdogBlock queries (printerstate.Identity(snap)
	// against the same live printer/info read).
	heaters, ok := wd.status(ctx, liveHostname)
	if !ok {
		t.Fatal("status query under the verified identity = not ok, want ok")
	}
	if len(heaters) != 1 || !heaters[0].Armed || heaters[0].Heater != "extruder" {
		t.Fatalf("heaters under the verified identity = %+v, want one armed extruder heater", heaters)
	}

	// Nothing was ever armed under the raw host: proves Arm never fell back
	// to it.
	heatersUnderHost, ok := wd.status(ctx, printer.Host)
	if !ok {
		t.Fatal("status query under the host = not ok, want ok (daemon reachable)")
	}
	if len(heatersUnderHost) != 0 {
		t.Fatalf("heaters under the raw host = %+v, want none: nothing should ever be armed under the host", heatersUnderHost)
	}

	// Disarm: start_print disarms before it sends anything (D2 "cancelled
	// atomically by start_print"). It must disarm the exact identity Arm
	// used above, not the host.
	startRes := mustExecute(t, p, f, printer, ActionStartPrint, Params{Filename: "model.gcode"}, "")
	if !startRes.Accepted || startRes.Effect != "confirmed" {
		t.Fatalf("start_print result = %+v, want accepted+confirmed", startRes)
	}

	heatersAfterDisarm, ok := wd.status(ctx, liveHostname)
	if !ok {
		t.Fatal("status query after disarm = not ok, want ok")
	}
	for _, h := range heatersAfterDisarm {
		if h.Armed {
			t.Fatalf("heaters after start_print = %+v, want nothing armed under the verified identity (disarmed under the wrong key)", heatersAfterDisarm)
		}
	}
}
