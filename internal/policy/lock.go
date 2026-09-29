package policy

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// crossProcessLockTimeout bounds how long Execute waits for the
// cross-process file lock (domain.LockPath) before reporting a conflict,
// per dev_docs/safety-architecture.md section 3.4: two processes contending
// for the same printer must fail fast with a named conflict, not hang.
const crossProcessLockTimeout = 3 * time.Second

// crossProcessLockRetry is how often TryLockContext re-polls the file lock
// while waiting.
const crossProcessLockRetry = 50 * time.Millisecond

// printerLock is the in-process half of dev_docs/safety-architecture.md
// section 3.4's serialisation: one mutex per printer identity around the
// whole check-act-verify-poll sequence, and the pendingAction record
// DeriveActivityState needs to evaluate the transition rows
// (11-state-model.md section 1.1 rows 7, 8, 10) while that sequence is in
// flight. pendingMu is a separate, always-uncontended mutex guarding only
// the pending field, so a caller that lost the race for mu can still read
// what is currently pending without blocking on mu itself.
type printerLock struct {
	mu sync.Mutex

	pendingMu sync.Mutex
	pending   *printerstate.PendingAction

	// startRec is the start-window record (startwindow.go) and pauseRec the
	// resume record (resume.go); both are guarded by pendingMu and, unlike
	// pending, are NOT cleared when a call returns.
	startRec *startInFlight
	pauseRec *pauseRecord

	// resumeFlight and pauseFlight track a resume or pause whose POST is still
	// running or whose macro has not finished (flight.go).
	resumeFlight *flightRec
	pauseFlight  *flightRec

	// preemptCancel is set while a pre-emptible setpoint action holds the lock
	// (plan-v0.3.0.md 2a.4); pause_print and the confirming cancel_print call it
	// to take the lock instead of failing with a conflict. preempted records, for
	// the CURRENT hold only, that they did (it is reset whenever a new holder
	// takes the lock), so only the action that was actually pre-empted reports it.
	// silentRec is the Silent record (speedpreset.go). Every transition of the
	// lock (take, pre-empt request, release) happens under pendingMu, so the
	// pre-empt decision and the lock acquisition are one critical section.
	preemptCancel context.CancelFunc
	// preemptWaiters counts pause/cancel callers waiting for the lock; while any
	// waits, a non-pre-empting caller cannot take it (they get the normal
	// conflict), so a queued setpoint can never win the lock ahead of a stop.
	preemptWaiters int
	preempted      bool
	silentRec      *silentRecord
}

func (l *printerLock) setPending(p *printerstate.PendingAction) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.pending = p
}

// acquireOutcome is the result of one atomic attempt on the in-process lock.
type acquireOutcome int

const (
	// lockTaken: the caller now holds the mutex.
	lockTaken acquireOutcome = iota
	// heldByPreemptible: a setpoint action holds the lock and was asked to stop
	// (only when the caller is pre-empting).
	heldByPreemptible
	// heldByOther: someone else holds it and it will not be pre-empted (a
	// non-setpoint action, or the caller does not pre-empt).
	heldByOther
)

// tryAcquire is the one atomic step: under pendingMu it either takes the mutex
// (registering the new holder's cancel func, nil for an action that cannot be
// pre-empted, and resetting the per-hold preempted flag), or, for a pre-empting
// caller finding a setpoint holder, marks that hold pre-empted and cancels it.
// There is no gap between deciding and acting, and none between a holder's
// release and the cancel func it registered (see unlockHolder).
func (l *printerLock) tryAcquire(cancel context.CancelFunc, preempting bool) acquireOutcome {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if !preempting && l.preemptWaiters > 0 {
		return heldByOther
	}
	if l.mu.TryLock() {
		l.preemptCancel, l.preempted = cancel, false
		return lockTaken
	}
	if preempting && l.preemptCancel != nil {
		l.preempted = true
		l.preemptCancel() // idempotent: safe to repeat while the holder winds down
		return heldByPreemptible
	}
	return heldByOther
}

func (l *printerLock) addPreemptWaiter(delta int) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.preemptWaiters += delta
}

// unlockHolder drops the holder's cancel func and the mutex in ONE critical
// section, so a pre-empting caller never sees the lock held without its cancel
// func nor a cancel func after the holder is gone.
func (l *printerLock) unlockHolder() {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.preemptCancel = nil
	l.mu.Unlock()
}

// takePreempted reports and clears whether the holder was pre-empted.
func (l *printerLock) takePreempted() bool {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	p := l.preempted
	l.preempted = false
	return p
}

func (l *printerLock) getPending() *printerstate.PendingAction {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return l.pending
}

// lockRegistry hands out one *printerLock per printer identity, creating it
// on first use. It is safe for concurrent use and is normally embedded once
// in a *Policy.
type lockRegistry struct {
	mu    sync.Mutex
	locks map[string]*printerLock
}

func newLockRegistry() *lockRegistry {
	return &lockRegistry{locks: map[string]*printerLock{}}
}

func (r *lockRegistry) get(identity string) *printerLock {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.locks[identity]
	if !ok {
		l = &printerLock{}
		r.locks[identity] = l
	}
	return l
}

// acquiredLocks holds both halves of a held lock (in-process and
// cross-process) so a single deferred release call can drop both in the
// right order.
type acquiredLocks struct {
	pl       *printerLock
	flock    *flock.Flock
	released bool
}

// release drops the cross-process lock first, then the in-process mutex,
// and clears any pendingAction this call left set (belt-and-suspenders: the
// caller is expected to have already cleared it once settle/timeout
// resolved).
func (a *acquiredLocks) release() {
	if a == nil || a.released {
		return
	}
	a.released = true
	if a.flock != nil {
		_ = a.flock.Unlock()
		_ = a.flock.Close()
	}
	if a.pl != nil {
		a.pl.setPending(nil)
		a.pl.unlockHolder()
	}
}

// acquireLocks takes the in-process mutex (non-blocking: a second call
// already in flight fails fast with a conflict naming it, rather than
// queuing behind it, per 11-state-model.md section 3.2 step 6) and then the
// cross-process file lock (bounded, per crossProcessLockTimeout).
//
// preempt is true for pause_print and cancel_print, which take the lock from an
// in-flight setpoint action instead of failing (plan-v0.3.0.md 2a.4).
// holderCancel is non-nil for a pre-emptible setpoint action: it is registered
// in the same critical section that takes the in-process mutex, so a pause that
// finds the lock held always finds the cancel func too (no window between
// taking the lock and registering it).
func acquireLocks(ctx context.Context, reg *lockRegistry, identity string, preempt bool, holderCancel context.CancelFunc) (*acquiredLocks, *Error) {
	pl := reg.get(identity)
	// pause_print and the confirming cancel_print (preempt) take the lock from an
	// in-flight setpoint action instead of failing (plan-v0.3.0.md 2a.4): stopping
	// must never wait behind a speed, fan, temperature or light change. Only
	// setpoint holders are pre-empted; any other holder gives the normal conflict.
	outcome := pl.tryAcquire(holderCancel, preempt)
	if outcome == heldByPreemptible && waitPreempting(ctx, pl) {
		outcome = lockTaken
	}
	if outcome != lockTaken {
		if pending := pl.getPending(); pending != nil {
			return nil, &Error{Code: CodeConflict, Message: "another action (" + string(pending.Kind) + ") is already in progress on this printer"}
		}
		return nil, &Error{Code: CodeConflict, Message: "another action is already in progress on this printer"}
	}

	path, err := domain.LockPath(identity)
	if err != nil {
		pl.unlockHolder()
		return nil, &Error{Code: CodeInternal, Message: "resolve lock path: " + err.Error()}
	}
	// domain.LockPath only names the path; the locks/ directory is created
	// here, the one place in this package that needs it to exist, mirroring
	// domain's own writeFileAtomic pattern (mode 0700: only this user).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		pl.unlockHolder()
		return nil, &Error{Code: CodeInternal, Message: "create lock directory: " + err.Error()}
	}
	fl := flock.New(path)
	lockCtx, cancel := context.WithTimeout(ctx, crossProcessLockTimeout)
	defer cancel()
	ok, err := fl.TryLockContext(lockCtx, crossProcessLockRetry)
	if !ok {
		pl.unlockHolder()
		if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
			// A real filesystem failure (not just "still held"), e.g. the
			// lock file's directory disappeared underneath us.
			return nil, &Error{Code: CodeInternal, Message: "acquire cross-process lock: " + err.Error()}
		}
		return nil, &Error{Code: CodeConflict, Message: "printer is locked by another " + domain.ServerName + " process"}
	}

	return &acquiredLocks{pl: pl, flock: fl}, nil
}

// preemptWait bounds how long pause_print or cancel_print waits for a
// pre-empted action to release the lock (plan-v0.3.0.md 2a.4).
var preemptWait = 3 * time.Second

// waitPreempting takes the lock for a pause or cancel once the pre-empted
// holder is gone, within preemptWait. Every iteration is one atomic tryAcquire
// that re-requests pre-emption, so a setpoint that queued up and won the lock in
// the gap is cancelled too, and the caller can never end up waiting behind it;
// a holder that is not a setpoint action ends the wait with a conflict. On
// success the caller holds the lock.
func waitPreempting(ctx context.Context, pl *printerLock) bool {
	pl.addPreemptWaiter(1)
	defer pl.addPreemptWaiter(-1)
	deadline := time.Now().Add(preemptWait)
	for {
		switch pl.tryAcquire(nil, true) {
		case lockTaken:
			return true
		case heldByOther:
			return false
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// preemptible lists the setpoint-type actions pause_print and cancel_print may
// pre-empt while they hold the lock (plan-v0.3.0.md 2a.4).
func preemptible(name ActionName) bool {
	switch name {
	case ActionSetSpeedPreset, ActionSetSpeedFactor, ActionSetFlowFactor, ActionSetFanSpeed,
		ActionSetNozzleTemperature, ActionSetBedTemperature, ActionSetLight:
		return true
	}
	return false
}
