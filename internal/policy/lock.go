package policy

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
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
}

func (l *printerLock) setPending(p *printerstate.PendingAction) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.pending = p
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
	pl    *printerLock
	flock *flock.Flock
}

// release drops the cross-process lock first, then the in-process mutex,
// and clears any pendingAction this call left set (belt-and-suspenders: the
// caller is expected to have already cleared it once settle/timeout
// resolved).
func (a *acquiredLocks) release() {
	if a == nil {
		return
	}
	if a.flock != nil {
		_ = a.flock.Unlock()
		_ = a.flock.Close()
	}
	if a.pl != nil {
		a.pl.setPending(nil)
		a.pl.mu.Unlock()
	}
}

// acquireLocks takes the in-process mutex (non-blocking: a second call
// already in flight fails fast with a conflict naming it, rather than
// queuing behind it, per 11-state-model.md section 3.2 step 6) and then the
// cross-process file lock (bounded, per crossProcessLockTimeout).
func acquireLocks(ctx context.Context, reg *lockRegistry, identity string) (*acquiredLocks, *Error) {
	pl := reg.get(identity)
	if !pl.mu.TryLock() {
		pending := pl.getPending()
		if pending != nil {
			return nil, &Error{Code: CodeConflict, Message: "another action (" + string(pending.Kind) + ") is already in progress on this printer"}
		}
		return nil, &Error{Code: CodeConflict, Message: "another action is already in progress on this printer"}
	}

	path, err := domain.LockPath(identity)
	if err != nil {
		pl.mu.Unlock()
		return nil, &Error{Code: CodeInternal, Message: "resolve lock path: " + err.Error()}
	}
	// domain.LockPath only names the path; the locks/ directory is created
	// here, the one place in this package that needs it to exist, mirroring
	// domain's own writeFileAtomic pattern (mode 0700: only this user).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		pl.mu.Unlock()
		return nil, &Error{Code: CodeInternal, Message: "create lock directory: " + err.Error()}
	}
	fl := flock.New(path)
	lockCtx, cancel := context.WithTimeout(ctx, crossProcessLockTimeout)
	defer cancel()
	ok, err := fl.TryLockContext(lockCtx, crossProcessLockRetry)
	if !ok {
		pl.mu.Unlock()
		if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
			// A real filesystem failure (not just "still held"), e.g. the
			// lock file's directory disappeared underneath us.
			return nil, &Error{Code: CodeInternal, Message: "acquire cross-process lock: " + err.Error()}
		}
		return nil, &Error{Code: CodeConflict, Message: "printer is locked by another " + domain.ServerName + " process"}
	}

	return &acquiredLocks{pl: pl, flock: fl}, nil
}
