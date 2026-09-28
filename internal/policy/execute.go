package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// Policy is the stateful half of this package: the per-printer lock
// registry (dev_docs/safety-architecture.md section 3.4) and the in-memory
// proposal_token store (section 3.3). One Policy is meant to be
// constructed once, at server startup, and shared by every tool call for
// every printer; its zero value is not usable, use New.
type Policy struct {
	locks  *lockRegistry
	tokens *tokenStore

	uploadingMu sync.Mutex
	uploading   map[string]map[string]bool // identity -> filename -> true
}

// New builds a ready-to-use Policy.
func New() *Policy {
	return &Policy{
		locks:     newLockRegistry(),
		tokens:    newTokenStore(),
		uploading: map[string]map[string]bool{},
	}
}

// testHookSkipGate lets a test disable Execute's gate check (checkGate) for
// one ActionName, so a canary test can drive a real mutation through
// Execute itself - proposal, token, send - rather than calling the fake
// printer's lifecycle methods directly (dev_docs/safety-architecture.md
// section 6's canary tests). It is nil in production and consulted only
// here; only a _test.go file in this package may assign it (it is
// unexported), and TestTestHookSkipGateNilByDefault (canary_test.go) pins
// that it starts nil.
var testHookSkipGate func(ActionName) bool

// skipGate reports whether testHookSkipGate wants Execute's gate check
// bypassed for name. It is false whenever nothing has set the hook, which
// is production behavior and every test in this package other than the
// through-Execute canary tests.
func skipGate(name ActionName) bool {
	return testHookSkipGate != nil && testHookSkipGate(name)
}

func (p *Policy) setUploading(identity, filename string, uploading bool) {
	p.uploadingMu.Lock()
	defer p.uploadingMu.Unlock()
	if uploading {
		if p.uploading[identity] == nil {
			p.uploading[identity] = map[string]bool{}
		}
		p.uploading[identity][filename] = true
		return
	}
	delete(p.uploading[identity], filename)
}

func (p *Policy) isUploading(identity, filename string) bool {
	p.uploadingMu.Lock()
	defer p.uploadingMu.Unlock()
	return p.uploading[identity][filename]
}

// Execute is the only path from a tool handler to a printer write
// (dev_docs/safety-architecture.md section 3.2). ctx bounds the whole call,
// including the settle poll. deps are this one printer's live clients.
// settings supplies the mid-print bands (D1) and idle-heat-minutes (D2).
// token is empty for a first call (or for any ConfirmationNone action) and
// the previously issued proposal token for a confirming second call.
//
// Execute never panics on a bad ActionName or Params; every rejection is a
// typed *Error a caller can map to a render code (errors.go).
func (p *Policy) Execute(ctx context.Context, deps Deps, printer domain.Printer, settings domain.Settings, name ActionName, params Params, token string) (Result, error) {
	spec, ok := specs[name]
	if !ok {
		return Result{}, &Error{Code: CodeInvalidInput, Message: fmt.Sprintf("unknown action %q", name)}
	}
	if err := checkAllowControl(name, printer); err != nil {
		return Result{}, err
	}

	identity, idErr := resolveExecuteIdentity(ctx, deps, printer)
	if idErr != nil {
		idErr.Action = name
		return Result{}, idErr
	}

	locks, lockErr := acquireLocks(ctx, p.locks, identity)
	if lockErr != nil {
		lockErr.Action = name
		return Result{}, lockErr
	}
	defer locks.release()

	now := time.Now()

	if token != "" {
		return p.executeWithToken(ctx, deps, printer, settings, spec, params, token, now, identity, locks)
	}

	// No token: take the fresh snapshot every path below needs. For a
	// ConfirmationNone action this is also the "fresh snapshot as the last
	// step before sending" snapshot (P5): nothing slow happens between this
	// read and the send call below.
	snap := printerstate.Take(ctx, deps.stateDeps(), printer)
	derived := printerstate.DeriveActivityState(snap, nil)

	if !skipGate(name) {
		if err := checkGate(spec, derived); err != nil {
			err.Action = name
			return Result{}, err
		}
	}

	confirmation, err := p.evaluateParams(ctx, deps, printer, identity, spec, snap, derived, params, settings)
	if err != nil {
		err.Action = name
		return Result{}, err
	}

	if confirmation == ConfirmationProposalToken {
		job := printerstate.JobIdentityFrom(snap)
		tok := p.tokens.issue(proposal{
			identity: identity, action: spec.Name, params: params, job: job,
			bucket: derived.Bucket, class: derived.Class, snapshotTime: snap.Taken, issuedAt: now,
		})
		return Result{
			Action:    spec.Name,
			Proposed:  true,
			Token:     tok,
			ExpiresAt: now.Add(tokenTTL),
			Effects:   spec.Effects,
			Commands:  spec.Commands,
			Before:    printerstate.BuildStateBlock(snap, derived, nil),
			Printer:   printer,
			Job:       job,
		}, nil
	}

	return p.send(ctx, deps, printer, identity, settings, spec, params, snap, derived, locks)
}

// resolveExecuteIdentity resolves the lock identity Execute takes its
// cross-process lock on (dev_docs/safety-architecture.md 3.4, review backlog
// item 24). It always takes its own fresh printer/info read before any lock
// is taken and uses the returned live Klipper hostname as identity, for a
// registry-backed printer exactly as much as for the K2_MCP_HOST
// environment override (domain.EnvOverride, whose Printer.Hostname is
// always empty since it is rebuilt fresh from the environment on every
// call): a registry-backed printer's persisted hostname is no longer
// trusted on its own (batch review C's HIGH finding - a registry entry's
// address can start answering for a different physical printer, e.g. DHCP
// moving the IP), it is only ever used to verify the live read agrees with
// it. If the read fails, comes back empty, or (for a registry-backed
// printer) disagrees with the persisted hostname, the write is refused
// (unavailable) rather than ever locking on the host or on a stale
// persisted hostname, so the lock key never switches identities out from
// under a concurrent process and a write is never sent to the wrong printer
// (domain/lock.go). This mirrors printerstate.DeriveActivityState's own
// checkIdentity (state.go), which performs the same verification for reads;
// Execute cannot simply reuse that check's Derived output because the lock
// must be resolved before the fresh snapshot Execute takes right after
// acquiring it (P5), so this makes its own printer/info read first.
func resolveExecuteIdentity(ctx context.Context, deps Deps, printer domain.Printer) (string, *Error) {
	persisted := strings.TrimSpace(printer.Hostname)

	info, err := deps.Moonraker.PrinterInfo(ctx)
	if err != nil {
		if persisted != "" {
			return "", &Error{
				Code: CodeUnavailable,
				Message: fmt.Sprintf(
					"this printer's identity could not be verified: registry hostname %q is persisted "+
						"but a fresh printer/info read failed: %v. Writes are refused until the live "+
						"hostname can be confirmed to still match; retry once the printer is reachable.",
					persisted, err),
			}
		}
		return "", &Error{
			Code: CodeUnavailable,
			Message: "the printer's Klipper hostname is not yet known: this printer has no " +
				"persisted hostname (environment-override printer) and a fresh printer/info " +
				"read to resolve one failed: " + err.Error() + ". Writes are refused until the " +
				"hostname is known, so the lock is never taken on the wrong identity; retry " +
				"once the printer is reachable.",
		}
	}
	live := strings.TrimSpace(info.Hostname)
	if live == "" {
		if persisted != "" {
			return "", &Error{
				Code: CodeUnavailable,
				Message: fmt.Sprintf(
					"this printer's identity could not be verified: registry hostname %q is persisted "+
						"but a fresh printer/info read returned an empty hostname. Writes are refused "+
						"until the live hostname can be confirmed to still match; retry once Klipper "+
						"reports a hostname.",
					persisted),
			}
		}
		return "", &Error{
			Code: CodeUnavailable,
			Message: "the printer's Klipper hostname is not yet known: this printer has no " +
				"persisted hostname (environment-override printer) and a fresh printer/info " +
				"read returned an empty hostname. Writes are refused until the hostname is " +
				"known, so the lock is never taken on the wrong identity; retry once Klipper " +
				"reports a hostname.",
		}
	}
	if persisted != "" && !printerstate.HostnamesEqual(persisted, live) {
		return "", &Error{
			Code: CodeUnavailable,
			Message: fmt.Sprintf(
				"registry hostname %q does not match the live Klipper hostname %q; the printer now "+
					"answering at this address may not be the one this registry entry was set up for "+
					"(e.g. DHCP moved the IP). Writes are refused; re-run discovery (printers scan, the "+
					"install wizard, or the TUI) to update the registry before writing to this printer.",
				persisted, live),
		}
	}
	return live, nil
}

// executeWithToken is the second, confirming call for a proposal_token (or
// conditional-token) action.
func (p *Policy) executeWithToken(ctx context.Context, deps Deps, printer domain.Printer, settings domain.Settings, spec actionSpec, params Params, token string, now time.Time, identity string, locks *acquiredLocks) (Result, error) {
	prop, ok := p.tokens.consume(token, now)
	if !ok || prop.action != spec.Name || prop.identity != identity {
		return Result{}, notFoundTokenError(spec.Name)
	}

	// Fresh snapshot, the last thing done before sending (P5) - never the
	// snapshot the proposal was built from.
	snap := printerstate.Take(ctx, deps.stateDeps(), printer)
	derived := printerstate.DeriveActivityState(snap, nil)
	job := printerstate.JobIdentityFrom(snap)

	if changed := proposalDiff(prop, identity, params, job, derived.Bucket, derived.Class); len(changed) > 0 {
		return Result{}, &Error{
			Action:  spec.Name,
			Code:    CodeConflict,
			Message: "the situation changed since this proposal was issued (" + joinChanged(changed) + "); request a new proposal",
			Changed: changed,
		}
	}

	if !skipGate(spec.Name) {
		if err := checkGate(spec, derived); err != nil {
			err.Action = spec.Name
			return Result{}, err
		}
	}
	if _, err := p.evaluateParams(ctx, deps, printer, identity, spec, snap, derived, params, settings); err != nil {
		err.Action = spec.Name
		return Result{}, err
	}

	return p.send(ctx, deps, printer, identity, settings, spec, params, snap, derived, locks)
}

// checkGate applies the generic bucket/CFS gate, except for set_light,
// which has its own rule (allowed in every state but offline). Identity
// mismatch/unverified (review backlog item 24) is an exception to
// set_light's own rule, not just to the generic bucket gate: unlike every
// other U state, here the printer's physical identity itself is uncertain,
// so even a "harmless" write like set_light could land on the wrong
// physical printer. Every write is refused, with no exception.
func checkGate(spec actionSpec, derived printerstate.Derived) *Error {
	if derived.State == printerstate.StateIdentityMismatch || derived.State == printerstate.StateIdentityUnverified {
		return &Error{
			Code:    CodeUnavailable,
			Message: strings.Join(nonEmpty(derived.Reasons), "; "),
		}
	}
	if spec.Name == ActionSetLight {
		if derived.State == printerstate.StateOffline {
			return &Error{Code: CodeUnavailable, Message: "printer is offline"}
		}
		return nil
	}
	return checkBucketAndCFS(spec, derived)
}

// send dispatches to the per-action sender. identity is the already-verified
// identity Execute resolved via resolveExecuteIdentity, threaded through to
// every sender that needs one (start_print's disarm and upload tracking,
// the idle-heat arm reported for display) rather than each recomputing it
// from printer (review backlog item 31): the same identity must be used for
// arming, querying and disarming the watchdog, or a status query can miss
// what Execute actually armed.
func (p *Policy) send(ctx context.Context, deps Deps, printer domain.Printer, identity string, settings domain.Settings, spec actionSpec, params Params, snap printerstate.Snapshot, derived printerstate.Derived, locks *acquiredLocks) (Result, error) {
	switch spec.Name {
	case ActionStartPrint:
		return p.sendStartPrint(ctx, deps, printer, identity, spec, params, snap, derived, locks)
	case ActionSetLight:
		return p.sendSetLight(ctx, deps, printer, spec, params, snap, derived)
	case ActionUploadGCodeFile:
		return p.sendUpload(ctx, deps, printer, identity, spec, params, snap, derived, locks)
	case ActionDeleteGCodeFile:
		return p.sendDelete(ctx, deps, printer, spec, params, snap, derived, locks)
	default:
		return p.sendPolled(ctx, deps, printer, identity, settings, spec, params, snap, derived, locks)
	}
}

// pollUntilSettle re-snapshots at pollInterval until settleFn reports
// settled or timeout elapses, per 11-state-model.md section 3.2. pending,
// when not PendingNone, is refreshed from locks on every iteration so
// DeriveActivityState's transition rows evaluate correctly while this
// call's own write is in flight.
func (p *Policy) pollUntilSettle(ctx context.Context, deps Deps, printer domain.Printer, locks *acquiredLocks, pendingKind printerstate.PendingKind, timeout time.Duration, settleFn func(printerstate.Snapshot, printerstate.Derived) bool) (printerstate.Snapshot, printerstate.Derived, bool) {
	deadline := time.Now().Add(timeout)
	var snap printerstate.Snapshot
	var derived printerstate.Derived
	for {
		var pending *printerstate.PendingAction
		if pendingKind != printerstate.PendingNone {
			pending = locks.pl.getPending()
		}
		snap = printerstate.Take(ctx, deps.stateDeps(), printer)
		derived = printerstate.DeriveActivityState(snap, pending)
		if settleFn(snap, derived) {
			return snap, derived, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return snap, derived, false
		}
		select {
		case <-ctx.Done():
			return snap, derived, false
		case <-time.After(pollInterval):
		}
	}
}

func effectString(confirmed bool) string {
	if confirmed {
		return "confirmed"
	}
	return "unconfirmed"
}

func joinChanged(changed []string) string {
	out := ""
	for i, c := range changed {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}
