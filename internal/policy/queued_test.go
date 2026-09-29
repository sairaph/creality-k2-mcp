package policy

import (
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// Live finding (supervised print 2026-09-29): Moonraker queues G-code behind a
// running macro. During a CFS purge two timed-out M220s were both queued and both
// ran later. A write that errored without an HTTP answer is "sent, possibly
// queued": never retried, the settle read decides, and a confirmed result after
// such an error is accepted.

func templateCallCount(f *fakePrinter) int { f.mu.Lock(); defer f.mu.Unlock(); return f.templateCalls }

func TestQueuedWrite_TimeoutThatRanIsConfirmedAndAcceptedWithoutADoubleSend(t *testing.T) {
	type tc struct {
		name   string
		action ActionName
		params Params
		setup  func(f *fakePrinter)
	}
	cases := []tc{
		{"speed factor", ActionSetSpeedFactor, Params{Percent: 120}, nil},
		{"flow factor", ActionSetFlowFactor, Params{Percent: 105}, nil},
		{"fan", ActionSetFanSpeed, Params{Fan: domain.FanPart, FanPercent: 60}, func(f *fakePrinter) { f.setPartFan(50) }},
		{"nozzle", ActionSetNozzleTemperature, Params{TargetC: 225}, nil},
		{"bed", ActionSetBedTemperature, Params{TargetC: 62}, func(f *fakePrinter) { f.withLock(func() { f.bedTarget = 60 }) }},
		{"speed preset", ActionSetSpeedPreset, preset("ultrafast"), nil},
	}
	for i, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f, p, printer := speedSetup(t, "queued-ran-"+string(rune('a'+i)))
			if c.setup != nil {
				c.setup(f)
			}
			f.withLock(func() { f.timeoutApplied = true })
			res, err := exec(p, f, printer, c.action, c.params, "")
			if err != nil {
				t.Fatalf("a timed-out write must not be an error: %v", err)
			}
			if res.Effect != "confirmed" || !res.Accepted {
				t.Fatalf("effect=%s accepted=%v, want confirmed and accepted", res.Effect, res.Accepted)
			}
			if !hasEffect(res, queuedRanNote) {
				t.Errorf("no busy note: %v", res.Effects)
			}
			if n := templateCallCount(f); n != 1 {
				t.Errorf("the template was sent %d times, want exactly once (no retry after a timeout)", n)
			}
		})
	}
}

func TestQueuedWrite_TimeoutThatNeverRanIsUnconfirmedWithTheQueuedNote(t *testing.T) {
	for i, c := range []struct {
		action ActionName
		params Params
	}{
		{ActionSetSpeedFactor, Params{Percent: 120}},
		{ActionSetSpeedPreset, preset("ultrafast")},
	} {
		f, p, printer := speedSetup(t, "queued-lost-"+string(rune('a'+i)))
		shortCap(t)
		f.withLock(func() { f.timeoutLost = true })
		res, err := exec(p, f, printer, c.action, c.params, "")
		if err != nil {
			t.Fatalf("%s: %v", c.action, err)
		}
		if res.Effect != "unconfirmed" || res.Accepted {
			t.Fatalf("%s: effect=%s accepted=%v", c.action, res.Effect, res.Accepted)
		}
		if !hasEffect(res, queuedPendingNote) {
			t.Errorf("%s: no queued note: %v", c.action, res.Effects)
		}
		if n := templateCallCount(f); n != 1 {
			t.Errorf("%s: sent %d times, want once", c.action, n)
		}
	}
}

// A value that already matched before the send: whether the timed-out command ran
// is not known, so it is not promoted to accepted.
func TestQueuedWrite_AlreadyMatchingIsNotPromoted(t *testing.T) {
	f, p, printer := speedSetup(t, "queued-same")
	f.withLock(func() { f.timeoutLost = true })
	res, err := exec(p, f, printer, ActionSetSpeedFactor, Params{Percent: 100}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Effect != "confirmed" || res.Accepted || !hasEffect(res, neutralConfirmNote) {
		t.Fatalf("effect=%s accepted=%v effects=%v", res.Effect, res.Accepted, res.Effects)
	}
}

// Silent exit: speedMode:0 then an M220 that times out and ran. Not a partial, not
// a failure, not sent twice.
func TestQueuedWrite_SilentExitWithAQueuedM220(t *testing.T) {
	f, p, printer := speedSetup(t, "queued-exit")
	f.setSilent(true)
	f.withLock(func() { f.timeoutApplied = true })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "confirmed" || !res.Accepted || !hasEffect(res, queuedRanNote) {
		t.Fatalf("effect=%s accepted=%v effects=%v", res.Effect, res.Accepted, res.Effects)
	}
	if m220Count(f) != 1 {
		t.Errorf("M220 sent %d times, want once", m220Count(f))
	}
	for _, e := range res.Effects {
		if strings.Contains(e, "failed") {
			t.Errorf("a queued command reads as a failure: %q", e)
		}
	}
	// And when the queued M220 has not run by the end of the window: unconfirmed, never partial.
	f2, p2, printer2 := speedSetup(t, "queued-exit2")
	shortCap(t)
	f2.setSilent(true)
	f2.withLock(func() { f2.timeoutLost = true })
	res = mustPreset(t, p2, f2, printer2, "ultrafast")
	if res.Effect != "unconfirmed" || !hasEffect(res, queuedPendingNote) || templateCallCount(f2) != 1 {
		t.Fatalf("effect=%s templates=%d effects=%v", res.Effect, templateCallCount(f2), res.Effects)
	}
}

// Only a definite HTTP error answer is retried (once).
func TestQueuedWrite_OnlyAnHTTPErrorIsRetried(t *testing.T) {
	f, p, printer := speedSetup(t, "queued-http")
	f.withLock(func() { f.failM220Times = 1 })
	res := mustPreset(t, p, f, printer, "ultrafast")
	if res.Effect != "confirmed" || m220Count(f) != 2 {
		t.Fatalf("effect=%s m220=%d, want confirmed after one retry of an HTTP error", res.Effect, m220Count(f))
	}
}
