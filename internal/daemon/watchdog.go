package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// expireCheckTimeout bounds the fresh snapshot-and-send Watchdog performs
// when a heater's deadline is reached, so an unreachable printer cannot hang
// the expiry goroutine forever.
const expireCheckTimeout = 10 * time.Second

// cfsRecheckInterval is how long the watchdog waits before re-checking after
// deferring an expiry because a connected CFS is busy (plan-v0.2.0.md V8, 2.6).
// There is deliberately no overall cap on deferrals: each check is bounded by
// expireCheckTimeout, and any other non-idle state, a changed target, an
// identity mismatch or an unreachable printer still stands down at once.
const cfsRecheckInterval = 30 * time.Second

// StateSource takes a fresh printerstate snapshot for a printer identity by
// resolving it against the printer registry (registryAccess.Snapshot).
// Recorder's until:"print_end" polling uses this (it only ever needs to
// resolve an already-registered printer's identity for a fresh read - it
// never turns anything off), and it remains registry-backed for that use.
// The idle-heat Watchdog below no longer uses this: see DirectStateSource.
type StateSource interface {
	Snapshot(ctx context.Context, identity string) (snap printerstate.Snapshot, derived printerstate.Derived, ok bool)
}

// DirectStateSource is how Watchdog reaches a printer at expiry: built
// straight from the connection details Arm recorded (host, Moonraker port,
// API key), never through a registry lookup by identity
// (dev_docs/safety-architecture.md section 10 D2, review backlog item 36).
// A printer known only through the K2_MCP_HOST environment override is
// never saved to the registry file, so a registry-backed lookup can never
// find it; before this fix the watchdog's TurnOff silently could not find
// such a printer at expiry and the heater was never turned off. ok is false
// only when the printer could not be reached at all (server/info or
// printer/info failed); hostname is the live Klipper hostname printer/info
// actually reported, for the caller to check against the identity Arm was
// given (SnapshotDirect never assumes that check itself; Watchdog.expire
// does it explicitly and stands down with reason "identity_mismatch" on a
// disagreement, never proceeding on an unverified identity, P1).
type DirectStateSource interface {
	SnapshotDirect(ctx context.Context, host string, moonrakerPort int, apiKey string) (snap printerstate.Snapshot, derived printerstate.Derived, hostname string, ok bool)
}

// DirectTemplateSender sends the one command the watchdog is ever allowed
// to send: SET_HEATER_TEMPERATURE HEATER=<heater> TARGET=0, straight to
// host/moonrakerPort/apiKey, no registry lookup (see DirectStateSource).
type DirectTemplateSender interface {
	TurnOffDirect(ctx context.Context, host string, moonrakerPort int, apiKey, heater string) error
}

// ArmRequest is Watchdog.Arm's request, decoupled from
// internal/policy.IdleHeatArmRequest so this package never imports policy.
// Host, MoonrakerPort and APIKey are the live connection details Arm needs
// to reach the printer directly at expiry (D2, review backlog item 36):
// APIKey is held only in this record, in process memory, for as long as the
// heater stays armed, and is never logged (see Server.handleArm) or
// included in Watchdog.Status.
type ArmRequest struct {
	Identity      string
	Heater        string // "extruder" or "heater_bed"
	TargetC       float64
	ArmMinutes    int
	Host          string
	MoonrakerPort int
	APIKey        string
}

// HeaterStatus is one heater's watchdog status, for Watchdog.Status and
// get_printer_status.
type HeaterStatus struct {
	Heater       string    `json:"heater"`
	Armed        bool      `json:"armed"`
	TargetC      float64   `json:"target_c"`
	DeadlineAt   time.Time `json:"deadline_at,omitempty"`
	LastAction   string    `json:"last_action,omitempty"`
	LastActionAt time.Time `json:"last_action_at,omitempty"`
}

// heaterRecord is one identity+heater's current or most recently resolved
// watchdog state. token guards against a stale timer (from a heater that
// was re-armed before its previous deadline) acting on a record it no
// longer owns.
type heaterRecord struct {
	armed         bool
	token         int64
	targetC       float64
	deadline      time.Time
	timer         *time.Timer
	lastAction    string
	lastActionAt  time.Time
	host          string
	moonrakerPort int
	apiKey        string
}

// Watchdog is D2's idle-heat watchdog component: entirely separate from the
// camera Hub (dev_docs/safety-architecture.md section 10 D2, "its own
// component in the daemon, not tied to camera code paths"). Arm records a
// heater, target and deadline; at expiry it takes a fresh snapshot and
// sends the turn-off command only if the printer is still idle (bucket I)
// and the heater's current target still equals the armed target, standing
// down and recording why otherwise. A connected CFS that is busy (or the
// signal-fed filament_operation state) defers the check by cfsRecheckInterval
// instead of standing down (plan-v0.2.0.md V8). Disarm cancels immediately. Every
// method is safe for concurrent use.
type Watchdog struct {
	mu        sync.Mutex
	byIdent   map[string]map[string]*heaterRecord // identity -> heater -> record
	nextToken int64

	direct     DirectStateSource
	directSend DirectTemplateSender

	// minute scales ArmRequest.ArmMinutes into a duration. Production
	// leaves this at time.Minute; tests shrink it so expiry does not need a
	// real multi-minute sleep.
	minute time.Duration

	// cfsRecheck is cfsRecheckInterval; tests shrink it.
	cfsRecheck time.Duration
}

// NewWatchdog builds a Watchdog backed by direct and directSend, both of
// which must be non-nil for Arm to ever do anything useful (a nil one makes
// Arm return an error rather than panic later at expiry). Both reach the
// printer straight from the connection details each ArmRequest carries,
// never through a registry lookup (D2, review backlog item 36).
func NewWatchdog(direct DirectStateSource, directSend DirectTemplateSender) *Watchdog {
	return &Watchdog{
		byIdent:    make(map[string]map[string]*heaterRecord),
		direct:     direct,
		directSend: directSend,
		minute:     time.Minute,
		cfsRecheck: cfsRecheckInterval,
	}
}

func validHeater(h string) bool { return h == "extruder" || h == "heater_bed" }

// floatEqual is a small epsilon compare for a heater target read back from
// the printer against the target Arm recorded, matching the C-degree
// granularity SET_HEATER_TEMPERATURE actually uses.
func floatEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}

// Arm records identity's heater target and deadline and starts its expiry
// timer, replacing any previous arm for the same identity+heater (the
// deadline resets: a fresh idle-bucket write while already armed extends
// the watchdog rather than stacking two of them). It returns an error only
// for an invalid request; it never contacts the printer itself (that only
// happens at expiry).
func (w *Watchdog) Arm(req ArmRequest) error {
	if req.Identity == "" {
		return fmt.Errorf("daemon: arm requires a non-empty identity")
	}
	if !validHeater(req.Heater) {
		return fmt.Errorf("daemon: heater must be \"extruder\" or \"heater_bed\", got %q", req.Heater)
	}
	if req.ArmMinutes <= 0 {
		return fmt.Errorf("daemon: arm_minutes must be positive, got %d", req.ArmMinutes)
	}
	if strings.TrimSpace(req.Host) == "" {
		return fmt.Errorf("daemon: arm requires a non-empty host; the watchdog reaches the printer directly at expiry and never through the registry")
	}
	if w.direct == nil || w.directSend == nil {
		return fmt.Errorf("daemon: watchdog has no printer access wired up")
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	heaters, ok := w.byIdent[req.Identity]
	if !ok {
		heaters = make(map[string]*heaterRecord)
		w.byIdent[req.Identity] = heaters
	}
	if existing, ok := heaters[req.Heater]; ok && existing.timer != nil {
		existing.timer.Stop()
	}

	w.nextToken++
	token := w.nextToken
	deadline := time.Now().Add(time.Duration(req.ArmMinutes) * w.minute)
	rec := &heaterRecord{
		armed: true, token: token, targetC: req.TargetC, deadline: deadline,
		host: req.Host, moonrakerPort: req.MoonrakerPort, apiKey: req.APIKey,
	}
	rec.timer = time.AfterFunc(time.Until(deadline), func() { w.expire(req.Identity, req.Heater, token) })
	heaters[req.Heater] = rec
	return nil
}

// Disarm cancels every currently armed heater watchdog for identity
// immediately (start_print's atomic cancel, D2). It is always a no-op
// success, never an error, when nothing is armed for identity.
func (w *Watchdog) Disarm(identity string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	heaters, ok := w.byIdent[identity]
	if !ok {
		return
	}
	for _, rec := range heaters {
		if !rec.armed {
			continue
		}
		if rec.timer != nil {
			rec.timer.Stop()
		}
		rec.armed = false
		rec.lastAction = "disarmed: a print started (or was cancelled by request) before the deadline"
		rec.lastActionAt = time.Now()
	}
}

// Status returns every heater this Watchdog has ever armed for identity,
// armed or not, sorted by heater name.
func (w *Watchdog) Status(identity string) []HeaterStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	heaters := w.byIdent[identity]
	out := make([]HeaterStatus, 0, len(heaters))
	for heater, rec := range heaters {
		out = append(out, HeaterStatus{
			Heater:       heater,
			Armed:        rec.armed,
			TargetC:      rec.targetC,
			DeadlineAt:   rec.deadline,
			LastAction:   rec.lastAction,
			LastActionAt: rec.lastActionAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Heater < out[j].Heater })
	return out
}

// ArmedCount returns how many heaters (across every printer) are currently
// armed, for the daemon's idle self-exit check.
func (w *Watchdog) ArmedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, heaters := range w.byIdent {
		for _, rec := range heaters {
			if rec.armed {
				n++
			}
		}
	}
	return n
}

// expire is the timer callback: it re-checks the printer itself, entirely
// independent of whatever armed it, and sends the turn-off command only if
// every one of D2's conditions still holds. token guards against acting on
// a record a newer Arm or a Disarm has already superseded.
func (w *Watchdog) expire(identity, heater string, token int64) {
	w.mu.Lock()
	heaters, ok := w.byIdent[identity]
	var rec *heaterRecord
	if ok {
		rec = heaters[heater]
	}
	if rec == nil || !rec.armed || rec.token != token {
		w.mu.Unlock()
		return // disarmed or superseded by a newer arm before the deadline
	}
	targetC := rec.targetC
	host := rec.host
	moonrakerPort := rec.moonrakerPort
	apiKey := rec.apiKey
	w.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), expireCheckTimeout)
	defer cancel()

	// Built straight from the connection details Arm recorded, never
	// through a registry lookup by identity (D2, review backlog item 36):
	// an env-override printer (K2_MCP_HOST) is never in the registry file,
	// so a registry-backed lookup could never find it here and the heater
	// would never be turned off.
	snap, derived, liveHostname, ok := w.direct.SnapshotDirect(ctx, host, moonrakerPort, apiKey)
	if !ok {
		w.settle(identity, heater, token, "stood down: printer is unreachable")
		return
	}
	if !printerstate.HostnamesEqual(liveHostname, identity) {
		w.settle(identity, heater, token, fmt.Sprintf(
			"stood down: identity_mismatch: armed for %q, now answering as %q; the printer now at this "+
				"address may not be the one that was armed (e.g. the address was reassigned)", identity, liveHostname))
		return
	}
	// CFS deferral (plan V8, 2.6, narrowed by safety review M1): a nozzle whose
	// CFS is positively busy is a filament operation in progress, not proof that
	// someone took over the printer, so re-check later instead of cutting the
	// nozzle out from under a load. Nozzle only, and only for a Known, error free
	// busy CFS (cfsDeferral); everything else, including a CFS that is unknown or
	// in error and the bed, takes the normal checks below and is turned off. The
	// target checks still apply first so a changed target stands down even while
	// deferring.
	if heater == "extruder" && cfsDeferral(derived) {
		current, known := heaterTarget(snap, heater)
		if !known {
			w.settle(identity, heater, token, "stood down: current heater target is unknown; refusing to assume it is unchanged")
			return
		}
		if !floatEqual(current, targetC) {
			w.settle(identity, heater, token, fmt.Sprintf("stood down: target changed by someone else (armed for %g, now %g)", targetC, current))
			return
		}
		w.deferExpiry(identity, heater, token, cfsBusyReason(derived))
		return
	}
	if derived.Bucket != printerstate.BucketI {
		reason := "printer is not idle"
		if len(derived.Reasons) > 0 && derived.Reasons[0] != "" {
			reason = derived.Reasons[0]
		}
		w.settle(identity, heater, token, fmt.Sprintf("stood down: no longer idle (state %s): %s", derived.State, reason))
		return
	}
	current, known := heaterTarget(snap, heater)
	if !known {
		w.settle(identity, heater, token, "stood down: current heater target is unknown; refusing to assume it is unchanged")
		return
	}
	if !floatEqual(current, targetC) {
		w.settle(identity, heater, token, fmt.Sprintf("stood down: target changed by someone else (armed for %g, now %g)", targetC, current))
		return
	}
	if err := w.directSend.TurnOffDirect(ctx, host, moonrakerPort, apiKey, heater); err != nil {
		w.settle(identity, heater, token, "turn-off command failed: "+err.Error())
		return
	}
	w.settle(identity, heater, token, fmt.Sprintf("turned %s off: still idle after the configured wait, target unchanged at %g", heater, targetC))
}

// settle records the outcome of an expiry check, unless a newer Arm has
// already replaced this record (token mismatch), in which case it leaves
// the newer record alone rather than clobbering it.
func (w *Watchdog) settle(identity, heater string, token int64, action string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	heaters, ok := w.byIdent[identity]
	if !ok {
		return
	}
	rec, ok := heaters[heater]
	if !ok || rec.token != token {
		return
	}
	rec.armed = false
	rec.lastAction = action
	rec.lastActionAt = time.Now()
}

// heaterTarget reads heater's current target off a fresh snapshot. ok is
// false when the target is not positively known (nil object or nil field),
// never treated as "unchanged" by the caller.
func heaterTarget(snap printerstate.Snapshot, heater string) (float64, bool) {
	switch heater {
	case "extruder":
		if snap.Extruder == nil || snap.Extruder.Target == nil {
			return 0, false
		}
		return *snap.Extruder.Target, true
	case "heater_bed":
		if snap.HeaterBed == nil || snap.HeaterBed.Target == nil {
			return 0, false
		}
		return *snap.HeaterBed.Target, true
	default:
		return 0, false
	}
}

// cfsDeferral reports whether an expiry should be deferred rather than
// settled. Only for the nozzle heater (the caller checks) and only when the CFS
// is POSITIVELY busy: Known, no error, and either the signal-fed
// filament_operation state or an idle-bucket printer whose CFS is not at rest
// (safety review M1). A CFS that is unknown (9999 unreachable, a field missing)
// or in error does not defer: turning a heater off at idle is the safe
// direction, and an unknown or error state can last indefinitely. The bed never
// defers: a CFS filament load does not need it.
func cfsDeferral(derived printerstate.Derived) bool {
	if !derived.CFSConnected || !derived.CFSKnown || derived.CFSError {
		return false
	}
	if derived.State == printerstate.StateFilamentOperation {
		return true
	}
	return derived.Bucket == printerstate.BucketI && !derived.CFSQuiescent
}

// cfsBusyReason names why the CFS is busy, for the deferred status line.
func cfsBusyReason(derived printerstate.Derived) string {
	if len(derived.CFSReasons) > 0 {
		return strings.Join(derived.CFSReasons, "; ")
	}
	for _, r := range derived.Reasons {
		if r != "" {
			return r
		}
	}
	return "state " + derived.State
}

// deferExpiry records the deferral and re-arms the same record's timer with
// the same token. It checks armed and the token under the lock first, so a
// Disarm (or a newer Arm) during the wait wins: a superseded or disarmed
// record is never re-armed (plan 8a.8).
func (w *Watchdog) deferExpiry(identity, heater string, token int64, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	heaters, ok := w.byIdent[identity]
	if !ok {
		return
	}
	rec, ok := heaters[heater]
	if !ok || !rec.armed || rec.token != token {
		return
	}
	now := time.Now()
	rec.lastAction = "deferred: CFS busy: " + reason
	rec.lastActionAt = now
	rec.deadline = now.Add(w.cfsRecheck)
	rec.timer = time.AfterFunc(w.cfsRecheck, func() { w.expire(identity, heater, token) })
}
