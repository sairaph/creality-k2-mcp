package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// errBoom is a fixed error TestWatchdog_Expiry_RecordsTurnOffFailure uses
// to simulate a failed turn-off command.
var errBoom = errors.New("simulated turn-off failure")

// This file exercises Watchdog in isolation, with a fake printer state
// source and a fake template sender (never a real socket or printer,
// AGENTS.md hard testing rule): expiry turns the heater off only when the
// printer is still idle and the target is unchanged, and stands down
// (recording why, never assuming) for every other outcome.
//
// Since review backlog item 36 (dev_docs/safety-architecture.md section 10
// D2), Watchdog reaches the printer directly from the connection details
// (host, Moonraker port, API key) each ArmRequest carries, never through a
// registry lookup by identity: fakeStateSource/fakeSender below are keyed
// by host, exactly like the real registryAccess.SnapshotDirect/
// TurnOffDirect, not by identity, so every test here already exercises the
// env-override shape (an identity with no corresponding registry entry) by
// construction - there is no registry in this file at all.

type snapshotEntry struct {
	snap    printerstate.Snapshot
	derived printerstate.Derived
	// hostname is the live Klipper hostname this entry reports back, which
	// Watchdog.expire compares against the identity it was armed for.
	hostname string
	ok       bool
}

// fakeStateSource is a test-only DirectStateSource: host -> canned result.
// It also implements the identity-keyed StateSource Recorder uses (via a
// second map), so the same fake serves both roles in tests that build a
// full daemon.Options (recorder_test.go's RPC round-trip tests).
type fakeStateSource struct {
	mu          sync.Mutex
	byHost      map[string]snapshotEntry
	byIdentity  map[string]snapshotEntry
	directCalls int
}

func newFakeStateSource() *fakeStateSource {
	return &fakeStateSource{byHost: make(map[string]snapshotEntry), byIdentity: make(map[string]snapshotEntry)}
}

// set registers entry under both identity and host keys, for a test that
// does not care which lookup path is used.
func (f *fakeStateSource) set(identity string, entry snapshotEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byIdentity[identity] = entry
	if entry.hostname == "" {
		entry.hostname = identity
	}
	f.byHost[identity] = entry
}

// setDirect registers entry under host only, for a test that specifically
// exercises the direct (non-registry) lookup Watchdog uses.
func (f *fakeStateSource) setDirect(host string, entry snapshotEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHost[host] = entry
}

// Snapshot implements StateSource (identity-keyed; Recorder's own use).
func (f *fakeStateSource) Snapshot(ctx context.Context, identity string) (printerstate.Snapshot, printerstate.Derived, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.byIdentity[identity]
	if !ok {
		return printerstate.Snapshot{}, printerstate.Derived{}, false
	}
	return entry.snap, entry.derived, entry.ok
}

// SnapshotDirect implements DirectStateSource (host-keyed; Watchdog's own
// use, review backlog item 36): no identity is consulted here at all,
// matching registryAccess.SnapshotDirect never doing a registry lookup.
func (f *fakeStateSource) SnapshotDirect(ctx context.Context, host string, moonrakerPort int, apiKey string) (printerstate.Snapshot, printerstate.Derived, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.directCalls++
	entry, ok := f.byHost[host]
	if !ok {
		return printerstate.Snapshot{}, printerstate.Derived{}, "", false
	}
	return entry.snap, entry.derived, entry.hostname, entry.ok
}

func (f *fakeStateSource) directCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.directCalls
}

type turnOffCall struct {
	host          string
	moonrakerPort int
	apiKey        string
	heater        string
}

// fakeSender is a test-only DirectTemplateSender.
type fakeSender struct {
	mu    sync.Mutex
	calls []turnOffCall
	err   error
}

func newFakeSender() *fakeSender { return &fakeSender{} }

func (f *fakeSender) TurnOffDirect(ctx context.Context, host string, moonrakerPort int, apiKey, heater string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, turnOffCall{host, moonrakerPort, apiKey, heater})
	return f.err
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSender) lastCall() (turnOffCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return turnOffCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

func (f *fakeSender) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func idleSnapshot(targetC float64) (printerstate.Snapshot, printerstate.Derived) {
	target := targetC
	return printerstate.Snapshot{Extruder: &moonraker.Extruder{Target: &target}}, printerstate.Derived{
		State: "idle", Bucket: printerstate.BucketI, Reasons: []string{"standby"},
	}
}

// waitFor polls until cond returns true or the deadline passes, failing the
// test otherwise. Used instead of a fixed sleep so the test is only ever as
// slow as the watchdog's own (test-shrunk) timer actually is.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func newTestWatchdog(direct DirectStateSource, send DirectTemplateSender) *Watchdog {
	w := NewWatchdog(direct, send)
	w.minute = time.Millisecond // 1 "minute" = 1ms, so expiry is fast in tests
	return w
}

// Still idle, target unchanged, live hostname matches the armed identity:
// the watchdog sends the turn-off command straight to the armed connection
// details and records why.
func TestWatchdog_Expiry_TurnsOffWhenStillIdleAndTargetUnchanged(t *testing.T) {
	snap, derived := idleSnapshot(200)
	state := newFakeStateSource()
	state.set("printer-a", snapshotEntry{snap: snap, derived: derived, ok: true})
	send := newFakeSender()
	w := newTestWatchdog(state, send)

	if err := w.Arm(ArmRequest{Identity: "printer-a", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: "printer-a", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool { return send.count() == 1 })

	status := w.Status("printer-a")
	if len(status) != 1 || status[0].Armed {
		t.Fatalf("status = %+v, want one disarmed (expired) entry", status)
	}
	if status[0].LastAction == "" {
		t.Fatal("expected a non-empty LastAction after expiry")
	}
	if w.ArmedCount() != 0 {
		t.Fatalf("ArmedCount = %d, want 0 after expiry", w.ArmedCount())
	}
}

// Env-override printer (review backlog item 36): armed with connection
// details that have no corresponding registry entry at all (this test never
// builds or touches a registry), the watchdog still reaches it directly at
// expiry and turns the heater off, with the exact host/port/api key Arm was
// given, never looked up by identity.
func TestWatchdog_Expiry_EnvOverridePrinter_TurnsOffUsingDirectConnDetails(t *testing.T) {
	const identity = "env-live-host.local" // the verified live hostname, not the host
	const host = "203.0.113.9"             // never saved to any registry
	snap, derived := idleSnapshot(210)
	state := newFakeStateSource()
	state.setDirect(host, snapshotEntry{snap: snap, derived: derived, hostname: identity, ok: true})
	send := newFakeSender()
	w := newTestWatchdog(state, send)

	if err := w.Arm(ArmRequest{Identity: identity, Heater: "extruder", TargetC: 210, ArmMinutes: 1, Host: host, MoonrakerPort: 7125, APIKey: "secret-key"}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool { return send.count() == 1 })

	call, ok := send.lastCall()
	if !ok {
		t.Fatal("TurnOffDirect was never called")
	}
	if call.host != host || call.moonrakerPort != 7125 || call.apiKey != "secret-key" || call.heater != "extruder" {
		t.Fatalf("TurnOffDirect call = %+v, want host=%s port=7125 apiKey=secret-key heater=extruder", call, host)
	}
	if state.directCallCount() == 0 {
		t.Fatal("SnapshotDirect was never called; the watchdog must reach the printer directly, not through a registry")
	}
	status := w.Status(identity)
	if len(status) != 1 || status[0].Armed {
		t.Fatalf("status = %+v, want one disarmed (expired) entry", status)
	}
}

// Stand-down: a job started before the deadline (bucket no longer idle).
func TestWatchdog_Expiry_StandsDownWhenNoLongerIdle(t *testing.T) {
	state := newFakeStateSource()
	state.set("printer-b", snapshotEntry{
		snap:    printerstate.Snapshot{},
		derived: printerstate.Derived{State: "printing", Bucket: printerstate.BucketP, Reasons: []string{"print_stats.state is printing"}},
		ok:      true,
	})
	send := newFakeSender()
	w := newTestWatchdog(state, send)

	if err := w.Arm(ArmRequest{Identity: "printer-b", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: "printer-b", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool { return len(w.Status("printer-b")) == 1 && !w.Status("printer-b")[0].Armed })

	if send.count() != 0 {
		t.Fatalf("TurnOff called %d times, want 0 (a job started, must never assume it is safe)", send.count())
	}
}

// Stand-down: the target was changed by someone else before the deadline.
func TestWatchdog_Expiry_StandsDownWhenTargetChanged(t *testing.T) {
	snap, derived := idleSnapshot(150) // armed for 200, now reads 150
	state := newFakeStateSource()
	state.set("printer-c", snapshotEntry{snap: snap, derived: derived, ok: true})
	send := newFakeSender()
	w := newTestWatchdog(state, send)

	if err := w.Arm(ArmRequest{Identity: "printer-c", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: "printer-c", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool { return len(w.Status("printer-c")) == 1 && !w.Status("printer-c")[0].Armed })

	if send.count() != 0 {
		t.Fatalf("TurnOff called %d times, want 0 (target changed by someone else)", send.count())
	}
}

// Stand-down: the printer is unreachable at expiry. The watchdog must never
// assume it is safe to send anything.
func TestWatchdog_Expiry_StandsDownWhenUnreachable(t *testing.T) {
	state := newFakeStateSource() // no entry for host "printer-d": SnapshotDirect returns ok=false
	send := newFakeSender()
	w := newTestWatchdog(state, send)

	if err := w.Arm(ArmRequest{Identity: "printer-d", Heater: "heater_bed", TargetC: 60, ArmMinutes: 1, Host: "printer-d", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool { return len(w.Status("printer-d")) == 1 && !w.Status("printer-d")[0].Armed })

	if send.count() != 0 {
		t.Fatalf("TurnOff called %d times, want 0 (printer unreachable, must never assume)", send.count())
	}
	status := w.Status("printer-d")
	if status[0].LastAction == "" {
		t.Fatal("expected a recorded reason for standing down")
	}
}

// Stand-down: the live hostname at expiry no longer matches the identity
// the watchdog was armed for (review backlog item 36's identity_mismatch
// case, e.g. the address was reassigned to a different physical printer
// between arming and expiry). The heater must never be turned off on an
// unverified identity.
func TestWatchdog_Expiry_StandsDownWhenIdentityMismatch(t *testing.T) {
	const armedIdentity = "printer-e.local"
	const host = "printer-e-host"
	snap, derived := idleSnapshot(200)
	state := newFakeStateSource()
	state.setDirect(host, snapshotEntry{snap: snap, derived: derived, hostname: "a-different-printer.local", ok: true})
	send := newFakeSender()
	w := newTestWatchdog(state, send)

	if err := w.Arm(ArmRequest{Identity: armedIdentity, Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: host, MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		s := w.Status(armedIdentity)
		return len(s) == 1 && !s[0].Armed
	})

	if send.count() != 0 {
		t.Fatalf("TurnOff called %d times, want 0 (identity mismatch, must never assume it is the same printer)", send.count())
	}
	status := w.Status(armedIdentity)
	if !containsAny(status[0].LastAction, "identity_mismatch") {
		t.Fatalf("LastAction = %q, want it to mention identity_mismatch", status[0].LastAction)
	}
}

// Disarm cancels an armed watchdog immediately: the timer never fires and
// TurnOff is never called, even after waiting past the original deadline.
func TestWatchdog_Disarm_CancelsBeforeExpiry(t *testing.T) {
	snap, derived := idleSnapshot(200)
	state := newFakeStateSource()
	state.set("printer-f", snapshotEntry{snap: snap, derived: derived, ok: true})
	send := newFakeSender()
	w := newTestWatchdog(state, send)
	w.minute = 50 * time.Millisecond // slow enough that Disarm reliably wins the race

	if err := w.Arm(ArmRequest{Identity: "printer-f", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: "printer-f", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	w.Disarm("printer-f")

	time.Sleep(100 * time.Millisecond) // past the original deadline

	if send.count() != 0 {
		t.Fatalf("TurnOff called %d times, want 0 (disarmed before the deadline)", send.count())
	}
	if w.ArmedCount() != 0 {
		t.Fatalf("ArmedCount = %d, want 0 after Disarm", w.ArmedCount())
	}
	status := w.Status("printer-f")
	if len(status) != 1 || status[0].Armed {
		t.Fatalf("status = %+v, want one disarmed entry", status)
	}
}

// Disarm on an identity with nothing armed is a harmless no-op.
func TestWatchdog_Disarm_NoopWhenNothingArmed(t *testing.T) {
	w := newTestWatchdog(newFakeStateSource(), newFakeSender())
	w.Disarm("never-armed")
	if w.ArmedCount() != 0 {
		t.Fatalf("ArmedCount = %d, want 0", w.ArmedCount())
	}
}

// Re-arming the same identity+heater before its deadline replaces the
// earlier arm: only the latest one ever fires.
func TestWatchdog_Arm_ReArmReplacesEarlierDeadline(t *testing.T) {
	snap, derived := idleSnapshot(220)
	state := newFakeStateSource()
	state.set("printer-g", snapshotEntry{snap: snap, derived: derived, ok: true})
	send := newFakeSender()
	w := newTestWatchdog(state, send)
	w.minute = 30 * time.Millisecond

	if err := w.Arm(ArmRequest{Identity: "printer-g", Heater: "extruder", TargetC: 999, ArmMinutes: 1, Host: "printer-g", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	// Re-arm for the target actually on the printer before the first would
	// have fired.
	if err := w.Arm(ArmRequest{Identity: "printer-g", Heater: "extruder", TargetC: 220, ArmMinutes: 1, Host: "printer-g", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("re-Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool { return len(w.Status("printer-g")) == 1 && !w.Status("printer-g")[0].Armed })

	if send.count() != 1 {
		t.Fatalf("TurnOff called %d times, want 1 (only the latest arm should ever fire)", send.count())
	}
}

// Arm validates its request and never touches the printer at arm time.
func TestWatchdog_Arm_ValidatesRequest(t *testing.T) {
	w := newTestWatchdog(newFakeStateSource(), newFakeSender())

	cases := []ArmRequest{
		{Identity: "", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: "h"},
		{Identity: "p", Heater: "bogus", TargetC: 200, ArmMinutes: 1, Host: "h"},
		{Identity: "p", Heater: "extruder", TargetC: 200, ArmMinutes: 0, Host: "h"},
		{Identity: "p", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: ""},
	}
	for _, req := range cases {
		if err := w.Arm(req); err == nil {
			t.Errorf("Arm(%+v): want error, got nil", req)
		}
	}
}

// A failed turn-off command is recorded, not silently dropped.
func TestWatchdog_Expiry_RecordsTurnOffFailure(t *testing.T) {
	snap, derived := idleSnapshot(200)
	state := newFakeStateSource()
	state.set("printer-h", snapshotEntry{snap: snap, derived: derived, ok: true})
	send := newFakeSender()
	send.setErr(errBoom)
	w := newTestWatchdog(state, send)

	if err := w.Arm(ArmRequest{Identity: "printer-h", Heater: "extruder", TargetC: 200, ArmMinutes: 1, Host: "printer-h", MoonrakerPort: 7125}); err != nil {
		t.Fatalf("Arm: %v", err)
	}

	waitFor(t, time.Second, func() bool { return send.count() == 1 })
	waitFor(t, time.Second, func() bool { return len(w.Status("printer-h")) == 1 && w.Status("printer-h")[0].LastAction != "" })
}
