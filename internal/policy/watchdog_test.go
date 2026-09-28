package policy

import (
	"context"
	"errors"
	"sync"
)

// fakeWatchdog is a test-only policy.Watchdog double (D2): in-memory only,
// no socket, every call recorded so a test can assert exactly what happened
// and when (AGENTS.md hard testing rules). alive defaults to true, matching
// a healthy background daemon; a test flips it or sets an arm error to
// exercise the refusal paths.
type fakeWatchdog struct {
	mu sync.Mutex

	alive  bool
	armErr error

	armCalls    []IdleHeatArmRequest
	disarmCalls []string

	// record, when set, is called synchronously inside Arm and Disarm,
	// before they return, so a test sharing it with fakePrinter.recordEvent
	// can prove ordering against the fake's own lifecycle/command events
	// (e.g. "the watchdog was armed before the heater command was sent", or
	// "disarmed before start_print sent PrintStart").
	record func(string)
}

// newFakeWatchdog returns a fake that is alive and arms successfully, the
// baseline for every idle-heat test that expects the write to succeed.
func newFakeWatchdog() *fakeWatchdog {
	return &fakeWatchdog{alive: true}
}

func (w *fakeWatchdog) setAlive(alive bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.alive = alive
}

// setArmFails makes every subsequent Arm call fail with a fixed error,
// simulating a daemon that is alive (reachable) but rejects the arm
// request itself.
func (w *fakeWatchdog) setArmFails(fail bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if fail {
		w.armErr = errors.New("simulated arm failure")
	} else {
		w.armErr = nil
	}
}

func (w *fakeWatchdog) Alive(ctx context.Context) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.alive
}

func (w *fakeWatchdog) Arm(ctx context.Context, req IdleHeatArmRequest) error {
	w.mu.Lock()
	armErr := w.armErr
	record := w.record
	if armErr == nil {
		w.armCalls = append(w.armCalls, req)
	}
	w.mu.Unlock()
	if armErr != nil {
		return armErr
	}
	if record != nil {
		record("watchdog_armed:" + req.Heater)
	}
	return nil
}

// Disarm implements Watchdog: it always succeeds (matching production's
// "no-op when nothing is armed") and records identity so a test can assert
// it was called, and, via record, when it happened relative to other
// events.
func (w *fakeWatchdog) Disarm(ctx context.Context, identity string) error {
	w.mu.Lock()
	w.disarmCalls = append(w.disarmCalls, identity)
	record := w.record
	w.mu.Unlock()
	if record != nil {
		record("watchdog_disarmed:" + identity)
	}
	return nil
}

func (w *fakeWatchdog) armCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.armCalls)
}

func (w *fakeWatchdog) disarmCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.disarmCalls)
}

func (w *fakeWatchdog) lastDisarm() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.disarmCalls) == 0 {
		return "", false
	}
	return w.disarmCalls[len(w.disarmCalls)-1], true
}

func (w *fakeWatchdog) lastArm() (IdleHeatArmRequest, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.armCalls) == 0 {
		return IdleHeatArmRequest{}, false
	}
	return w.armCalls[len(w.armCalls)-1], true
}
