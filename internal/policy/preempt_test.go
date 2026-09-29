package policy

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Pre-emption and Silent-outcome tests added after the v0.3.0 review. The fakes
// are event-driven (a held frame or template is released by context
// cancellation), so nothing here depends on how fast the machine is.

// waitFor polls cond every few ms and fails after a generous deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// runAsync runs an Execute call in a goroutine and returns a func that waits for it.
func runAsync(p *Policy, f *fakePrinter, printer domain.Printer, name ActionName, params Params) func() (Result, error) {
	var wg sync.WaitGroup
	var res Result
	var err error
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err = exec(p, f, printer, name, params, "")
	}()
	return func() (Result, error) { wg.Wait(); return res, err }
}

// --- lock level (M-2, M-3) ---

// A holder that finishes exactly as the pause arrives must never turn into a
// conflict: the decision and the acquisition are one critical section.
func TestLock_FinishingHolderNeverConflictsWithAPreemptingCaller(t *testing.T) {
	setTestHome(t)
	reg := newLockRegistry()
	for i := 0; i < 300; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		holder, err := acquireLocks(ctx, reg, "race-finish", false, cancel)
		if err != nil {
			t.Fatalf("iteration %d: holder: %v", i, err)
		}
		go func(delay time.Duration) {
			time.Sleep(delay)
			holder.release()
			cancel()
		}(time.Duration(i%9) * 60 * time.Microsecond)
		req, err := acquireLocks(context.Background(), reg, "race-finish", true, nil)
		if err != nil {
			t.Fatalf("iteration %d: a pre-empting caller got %v", i, err)
		}
		req.release()
	}
}

// A setpoint that queues up while a pause is waiting must never take the lock
// ahead of it: every wait iteration re-requests pre-emption.
func TestLock_QueuedSetpointsNeverWinOverAWaitingPause(t *testing.T) {
	setTestHome(t)
	reg := newLockRegistry()
	for i := 0; i < 40; i++ {
		var stop atomic.Bool
		var wg sync.WaitGroup
		// The first holder, then a stream of queued setpoints hammering the lock.
		hold := func() {
			ctx, cancel := context.WithCancel(context.Background())
			l, err := acquireLocks(ctx, reg, "race-queue", false, cancel)
			if err != nil {
				return
			}
			select { // a setpoint that only ends when it is pre-empted or after a moment
			case <-ctx.Done():
			case <-time.After(30 * time.Millisecond):
			}
			l.release()
			cancel()
		}
		wg.Add(1)
		go func() { defer wg.Done(); hold() }()
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					hold()
				}
			}()
		}
		time.Sleep(time.Millisecond)
		start := time.Now()
		req, err := acquireLocks(context.Background(), reg, "race-queue", true, nil)
		if err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("iteration %d: pause lost to queued setpoints: %v", i, err)
		}
		if took := time.Since(start); took > preemptWait {
			t.Errorf("iteration %d: waited %v", i, took)
		}
		stop.Store(true)
		req.release()
		wg.Wait()
	}
}

// Pause and cancel never pre-empt a holder that is not a setpoint action, nor
// each other: they get the normal, immediate conflict.
func TestLock_NonSetpointHoldersAreNotPreempted(t *testing.T) {
	setTestHome(t)
	reg := newLockRegistry()
	holder, err := acquireLocks(context.Background(), reg, "non-setpoint", false, nil) // start, resume, upload, pause...
	if err != nil {
		t.Fatal(err)
	}
	defer holder.release()
	start := time.Now()
	if _, err := acquireLocks(context.Background(), reg, "non-setpoint", true, nil); err == nil || err.Code != CodeConflict {
		t.Fatalf("a pre-empting caller against a non-setpoint holder got %v, want a conflict", err)
	}
	if took := time.Since(start); took > preemptWait/2 {
		t.Errorf("the conflict took %v, want it immediate", took)
	}
	// A non-pre-empting caller never pre-empts a setpoint holder either.
	ctx, cancel := context.WithCancel(context.Background())
	sp, err := acquireLocks(ctx, reg, "setpoint-holder", false, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.release()
	if _, err := acquireLocks(context.Background(), reg, "setpoint-holder", false, nil); err == nil || err.Code != CodeConflict {
		t.Fatalf("a non-pre-empting caller got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("a non-pre-empting caller cancelled the setpoint holder")
	}
}

// M-3: a stale flag must never make an unrelated setpoint report preempted.
func TestPreemptedFlagDoesNotLeakToALaterSetpoint(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-stale")
	identity := printer.Hostname
	// A is a setpoint holder; R (a pause) pre-empts it and takes the lock.
	ctxA, cancelA := context.WithCancel(context.Background())
	a, aerr := acquireLocks(ctxA, p.locks, identity, false, cancelA)
	if aerr != nil {
		t.Fatal(aerr)
	}
	got := make(chan *acquiredLocks, 1)
	go func() {
		r, err := acquireLocks(context.Background(), p.locks, identity, true, nil)
		if err != nil {
			t.Errorf("pre-empting caller: %v", err)
		}
		got <- r
	}()
	<-ctxA.Done() // A is told to stop
	if !a.pl.takePreempted() {
		t.Fatal("the holder that was pre-empted did not see the flag")
	}
	a.release()
	r := <-got
	if r == nil {
		t.FailNow()
	}
	defer r.release()
	// Now an unrelated set_fan_speed B arrives while R holds the lock.
	_, err := exec(p, f, printer, ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 60}, "")
	wantErr(t, err, CodeConflict, "already in progress")
}

// --- Execute level: what a pre-empted Silent call reports ---

func preemptWith(t *testing.T, p *Policy, f *fakePrinter, printer domain.Printer, waitInFlight func() bool) {
	t.Helper()
	waitFor(t, "the setpoint action in flight", func() bool { return waitInFlight() && lockHeld(p, printer) })
	if _, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err != nil {
		t.Fatalf("pause_print: %v", err)
	}
}

// frameEntered reports that a speedMode call is inside the fake and held (the
// event that proves the setpoint is in flight, unlike a sleep).
func frameEntered(f *fakePrinter) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.speedModeHeld > 0 }

// A pre-empted Silent ENTRY (the frame held in flight) reports preempted, keeps
// the structured report, and says what a fresh read shows.
func TestPreempted_SilentEntryHoldingTheFrame(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-pre-entry")
	f.withLock(func() { f.blockSpeedMode = true })
	wait := runAsync(p, f, printer, ActionSetSpeedPreset, preset("silent"))
	waitFor(t, "the speedMode frame held in flight", func() bool { return frameEntered(f) })
	preemptWith(t, p, f, printer, func() bool { return true })
	res, err := wait()
	if err != nil || res.Effect != "preempted" {
		t.Fatalf("result = %v %+v", err, res.Effect)
	}
	if res.SpeedPreset == nil || res.SpeedPreset.MoonrakerSilent != "off" {
		t.Fatalf("report = %+v, want the fresh reading Silent off", res.SpeedPreset)
	}
	if !hasEffect(res, "a fresh read after the interruption shows Silent off") || res.After.PrinterID == "" {
		t.Errorf("effects = %v", res.Effects)
	}
	if silentNow(f) {
		t.Errorf("Silent was entered although the frame never went out")
	}
}

// A pre-empted Silent EXIT holding the speedMode:0 frame: no M220 is sent.
func TestPreempted_SilentExitHoldingTheFrame(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-pre-exit")
	f.setSilent(true)
	f.withLock(func() { f.blockSpeedMode = true })
	wait := runAsync(p, f, printer, ActionSetSpeedPreset, preset("standard"))
	waitFor(t, "the speedMode frame held in flight", func() bool { return frameEntered(f) })
	preemptWith(t, p, f, printer, func() bool { return true })
	res, err := wait()
	if err != nil || res.Effect != "preempted" {
		t.Fatalf("result = %v %+v", err, res.Effect)
	}
	if m220Count(f) != 0 {
		t.Errorf("an M220 was sent after the exit frame was held")
	}
	if res.SpeedPreset == nil || res.SpeedPreset.MoonrakerSilent != "on" || !hasEffect(res, "shows Silent on") {
		t.Errorf("report = %+v effects = %v", res.SpeedPreset, res.Effects)
	}
}

// Pre-empted between the exit frame and the M220 (the half-applied case): the
// fresh read names what is actually applied, from a read and not from the frame.
func TestPreempted_HalfAppliedExit(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-pre-half")
	f.withLock(func() { f.speedFactor = 1.25 })
	f.setSilent(true) // entered from ultrafast: the exit restores 125%
	f.withLock(func() { f.blockTemplate = true })
	wait := runAsync(p, f, printer, ActionSetSpeedPreset, preset("stable"))
	waitFor(t, "the exit frame written and the M220 in flight", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.templateCalls > 0 && len(f.speedFrames) == 1
	})
	preemptWith(t, p, f, printer, func() bool { return true })
	res, err := wait()
	if err != nil || res.Effect != "preempted" {
		t.Fatalf("result = %v %+v", err, res.Effect)
	}
	if !hasEffect(res, "a fresh read after the interruption shows Silent off and the speed factor at 125%") {
		t.Errorf("the fresh read is not stated: %v", res.Effects)
	}
	if res.SpeedPreset == nil || res.SpeedPreset.MoonrakerFactorPct == nil || int(*res.SpeedPreset.MoonrakerFactorPct+0.5) != 125 {
		t.Errorf("structured report lost or wrong: %+v", res.SpeedPreset)
	}
}

// A call that already confirmed keeps its result even if a pause arrived at the end.
func TestPreemptedResultKeepsAConfirmedOutcome(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-keep")
	confirmed := Result{Action: ActionSetSpeedFactor, Effect: "confirmed", Accepted: true}
	got, err := p.preemptedResult(context.Background(), f.deps(), printer, specs[ActionSetSpeedFactor], Params{}, p.locks.get(printer.Hostname), confirmed, nil)
	if err != nil || got.Effect != "confirmed" {
		t.Fatalf("got %+v %v", got, err)
	}
}

// The cancel PROPOSAL pre-empts nothing; only the confirming call does.
func TestCancelProposalDoesNotPreempt(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-cancel-prop")
	f.withLock(func() { f.blockTemplate = true })
	wait := runAsync(p, f, printer, ActionSetSpeedFactor, Params{Percent: 110})
	waitFor(t, "the setpoint in flight", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.templateCalls > 0
	})
	_, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	wantErr(t, err, CodeConflict, "already in progress")
	select {
	case <-time.After(50 * time.Millisecond):
	}
	// The setpoint is still in flight: release it by cancelling through a pause.
	if _, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if res, err := wait(); err != nil || res.Effect != "preempted" {
		t.Fatalf("setpoint = %v %+v", err, res.Effect)
	}
}

// Pause never pre-empts a real upload in flight (a non-setpoint action).
func TestPauseDoesNotPreemptAnUpload(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-upload")
	rel := make(chan struct{})
	f.withLock(func() { f.uploadRelease = rel })
	wait := runAsync(p, f, printer, ActionUploadGCodeFile, Params{Filename: "new.gcode", LocalPath: "x"})
	waitFor(t, "the upload in flight", func() bool { return lockHeld(p, printer) })
	start := time.Now()
	_, err := exec(p, f, printer, ActionPausePrint, Params{}, "")
	wantErr(t, err, CodeConflict, "already in progress")
	if took := time.Since(start); took > preemptWait/2 {
		t.Errorf("the conflict took %v, want it immediate", took)
	}
	close(rel)
	if res, err := wait(); err != nil || res.Effect == "preempted" {
		t.Fatalf("upload = %v %+v", err, res.Effect)
	}
}

// --- M-1: stuck_silent_possible only for a job that is neither printing nor paused ---

func TestSilentStuckCondition(t *testing.T) {
	on := printerstate.Derived{Qmode: printerstate.QmodeOn}
	for state, want := range map[string]bool{
		"printing": false, "paused": false, "standby": true, "complete": true, "cancelled": true, "error": true,
	} {
		snap := printerstate.Snapshot{PrintStats: &moonraker.PrintStats{State: state}}
		if got := silentStuck(snap, on); got != want {
			t.Errorf("print_stats %q: stuck = %v, want %v", state, got, want)
		}
	}
	if silentStuck(printerstate.Snapshot{}, on) {
		t.Error("an unreadable print_stats must not assert stuck")
	}
	if silentStuck(printerstate.Snapshot{PrintStats: &moonraker.PrintStats{State: "complete"}}, printerstate.Derived{Qmode: printerstate.QmodeOff}) {
		t.Error("Silent off is never stuck")
	}
}

// A pause lands right after the Silent frame: the call keeps its real outcome and
// says Silent stays on through the pause.
func TestSpeedPreset_PausedAfterTheFrameIsNotStuck(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-paused-after")
	f.withLock(func() { f.pauseAfterSpeed = true })
	res := mustPreset(t, p, f, printer, "silent")
	if res.Effect != "confirmed" {
		t.Fatalf("effect = %s, want confirmed", res.Effect)
	}
	if !hasEffect(res, "Silent stays active through the pause") {
		t.Errorf("no informational note: %v", res.Effects)
	}
	for _, e := range res.Effects {
		if strings.Contains(e, "STUCK") || strings.Contains(e, "Qmode_exit") {
			t.Errorf("a paused print reported as stuck: %q", e)
		}
	}
}

// m-5: Silent on with the factor changed at the printer screen is neither a
// no_change nor something speedMode:1 can fix.
func TestSpeedPreset_SilentWithAChangedFactorIsRefused(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-silent-80")
	f.setSilent(true)
	f.withLock(func() { f.speedFactor = 0.8 })
	_, err := exec(p, f, printer, ActionSetSpeedPreset, preset("silent"), "")
	wantErr(t, err, CodeUnavailable, "leave Silent with stable, standard or ultrafast")
	if len(f.speedFramesSnapshot()) != 0 {
		t.Errorf("a frame was sent")
	}
}

// m-9: with the Silent state unreadable, a fan request above 50% says why it may not read back.
func TestSetFanSpeed_UnknownSilentExplainsTheCap(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-fan-unknown")
	shortCap(t)
	f.withLock(func() { f.qmodeOmit = true })
	f.setPartFan(40)
	res, err := exec(p, f, printer, ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 80}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasEffect(res, "Silent mode state could not be read") {
		t.Errorf("no explanation: %v", res.Effects)
	}
}

// --- second review ---

// M-1: a pre-empted setpoint gives the lock back BEFORE its fresh read, so a
// slow port-9999 read cannot push the pause past preemptWait.
func TestPreempted_SlowFreshReadDoesNotDelayThePause(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-slow-read")
	old := preemptWait
	preemptWait = 400 * time.Millisecond
	t.Cleanup(func() { preemptWait = old })
	f.withLock(func() { f.blockTemplate = true })
	wait := runAsync(p, f, printer, ActionSetSpeedFactor, Params{Percent: 110})
	waitFor(t, "the setpoint in flight", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.templateCalls > 0
	})
	// From now on every 9999 read takes longer than preemptWait.
	f.withLock(func() { f.readDelay = 1500 * time.Millisecond })
	start := time.Now()
	if _, err := exec(p, f, printer, ActionPausePrint, Params{}, ""); err != nil {
		t.Fatalf("pause_print lost to the pre-empted call's slow fresh read: %v", err)
	}
	t.Logf("pause returned after %v", time.Since(start))
	res, err := wait()
	if err != nil || res.Effect != "preempted" {
		t.Fatalf("setpoint = %v %+v", err, res.Effect)
	}
}

// m-1: a call's own outcome or request-only refusal is not replaced by preempted;
// a refusal that read the printer may be an artifact and is reported as preempted;
// Accepted never comes from an interrupted write or from no write.
func TestPreemptedResult_KeepsOwnOutcomes(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-own")
	pl := p.locks.get(printer.Hostname)
	call := func(res Result, err error) (Result, error) {
		return p.preemptedResult(context.Background(), f.deps(), printer, specs[ActionSetSpeedPreset], preset("standard"), pl, res, err)
	}
	if got, err := call(Result{Effect: "no_change"}, nil); err != nil || got.Effect != "no_change" {
		t.Errorf("no_change replaced: %v %+v", err, got)
	}
	own := &Error{Code: CodeInvalidInput, Message: "band"}
	if _, err := call(Result{}, own); err != own {
		t.Errorf("an invalid-input refusal was replaced: %v", err)
	}
	if got, err := call(Result{}, &Error{Code: CodeUnavailable, Message: "offline"}); err != nil || got.Effect != "preempted" {
		t.Errorf("a printer-reading refusal = %v %+v, want preempted", err, got)
	}
	// Accepted: only with a write that was sent and returned nil.
	if got, _ := call(Result{Accepted: true}, nil); got.Accepted {
		t.Errorf("Accepted reported with no write")
	}
	if got, _ := call(Result{Accepted: true, Commands: []string{"M220 S100"}}, nil); !got.Accepted {
		t.Errorf("Accepted lost for a write that returned nil")
	}
}

// m-2: wording of a pre-empted Silent entry (frame held): not listed as fact, and
// the interruption is not called a failure.
func TestPreempted_SilentEntryWording(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-pre-words")
	f.withLock(func() { f.blockSpeedMode = true })
	wait := runAsync(p, f, printer, ActionSetSpeedPreset, preset("silent"))
	waitFor(t, "the frame held", func() bool { return frameEntered(f) })
	preemptWith(t, p, f, printer, func() bool { return true })
	res, _ := wait()
	if res.Accepted {
		t.Errorf("Accepted reported for an interrupted write")
	}
	if len(res.Effects) == 0 || strings.HasPrefix(res.Effects[0], "sets velocity") || !strings.Contains(res.Effects[0], "if Silent was applied") {
		t.Errorf("Silent effects are listed as fact: %v", res.Effects)
	}
	for _, e := range res.Effects {
		if strings.Contains(e, "context canceled") || strings.Contains(e, "failed twice") {
			t.Errorf("an interruption reads as a failure: %q", e)
		}
	}
	if !hasEffect(res, "interrupted by pause_print or cancel_print") {
		t.Errorf("no interruption note: %v", res.Effects)
	}
}

// m-2: an interrupted M220 does not read as "failed twice (context canceled)".
func TestPreempted_M220Wording(t *testing.T) {
	fastLifecycle(t)
	f, p, printer := speedSetup(t, "sp-pre-m220")
	f.withLock(func() { f.blockTemplate = true })
	wait := runAsync(p, f, printer, ActionSetSpeedPreset, preset("ultrafast"))
	waitFor(t, "the M220 in flight", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.templateCalls > 0
	})
	preemptWith(t, p, f, printer, func() bool { return true })
	res, _ := wait()
	for _, e := range res.Effects {
		if strings.Contains(e, "context canceled") || strings.Contains(e, "failed twice") {
			t.Errorf("an interruption reads as a failure: %q", e)
		}
	}
	if !hasEffect(res, "the M220 was interrupted by pause_print or cancel_print") {
		t.Errorf("effects = %v", res.Effects)
	}
}

// Two stops arriving together: one proceeds, the other gets the conflict, nothing
// hangs, and neither cancels the other.
func TestLock_TwoStopsTogether(t *testing.T) {
	setTestHome(t)
	reg := newLockRegistry()
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		sp, err := acquireLocks(ctx, reg, "two-stops", false, cancel)
		if err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			l   *acquiredLocks
			err *Error
		}
		out := make(chan outcome, 2)
		for s := 0; s < 2; s++ {
			go func() {
				l, err := acquireLocks(context.Background(), reg, "two-stops", true, nil)
				out <- outcome{l, err}
			}()
		}
		<-ctx.Done() // the setpoint was told to stop
		sp.release()
		var winners []*acquiredLocks
		conflicts := 0
		for s := 0; s < 2; s++ {
			select {
			case o := <-out:
				if o.err != nil {
					if o.err.Code != CodeConflict {
						t.Fatalf("iteration %d: %v", i, o.err)
					}
					conflicts++
				} else {
					winners = append(winners, o.l)
				}
			case <-time.After(preemptWait + 2*time.Second):
				t.Fatalf("iteration %d: a stop hung", i)
			}
		}
		if len(winners) != 1 || conflicts != 1 {
			t.Fatalf("iteration %d: %d winners and %d conflicts, want 1 and 1 (the winner holds the lock)", i, len(winners), conflicts)
		}
		winners[0].release()
	}
}

// While a stop waits, a non-pre-empting caller gets the conflict even if the
// lock is momentarily free (the waiter gate), deterministically.
func TestLock_WaiterGateBlocksNonPreemptingCallers(t *testing.T) {
	var pl printerLock
	pl.addPreemptWaiter(1)
	if got := pl.tryAcquire(nil, false); got != heldByOther {
		t.Fatalf("a non-pre-empting caller got %v while a stop waits, want heldByOther", got)
	}
	if got := pl.tryAcquire(nil, true); got != lockTaken {
		t.Fatalf("a stop got %v on the free lock", got)
	}
	pl.unlockHolder()
	pl.addPreemptWaiter(-1)
	if got := pl.tryAcquire(nil, false); got != lockTaken {
		t.Fatalf("a non-pre-empting caller got %v after the stop left", got)
	}
	pl.unlockHolder()
}

// Owner-ruled: a confirming cancel is refused while a pause holds the lock.
func TestCancelConfirmIsRefusedWhileAPauseHoldsTheLock(t *testing.T) {
	f, p, printer := speedSetup(t, "sp-cancel-vs-pause")
	prop, err := exec(p, f, printer, ActionCancelPrint, Params{}, "")
	if err != nil || !prop.Proposed {
		t.Fatalf("proposal: %v", err)
	}
	pause, aerr := acquireLocks(context.Background(), p.locks, printer.Hostname, true, nil) // stands in for a pause holding the lock
	if aerr != nil {
		t.Fatal(aerr)
	}
	defer pause.release()
	start := time.Now()
	_, err = exec(p, f, printer, ActionCancelPrint, Params{}, prop.Token)
	wantErr(t, err, CodeConflict, "already in progress")
	if took := time.Since(start); took > preemptWait/2 {
		t.Errorf("the conflict took %v, want it immediate", took)
	}
}
