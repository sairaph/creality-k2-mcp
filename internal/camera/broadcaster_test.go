package camera

import (
	"context"
	"testing"
	"time"
)

func openTestSession(t *testing.T, autoRepeat bool) *Session {
	t.Helper()
	host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: autoRepeat})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func recvWithTimeout(t *testing.T, ch <-chan AccessUnit, timeout time.Duration) (AccessUnit, bool) {
	t.Helper()
	select {
	case au, ok := <-ch:
		return au, ok
	case <-time.After(timeout):
		return AccessUnit{}, false
	}
}

func TestBroadcasterFansOutToMultipleSubscribers(t *testing.T) {
	session := openTestSession(t, true)

	b := NewBroadcaster(session)
	defer b.Close()

	id1, ch1 := b.Subscribe()
	id2, ch2 := b.Subscribe()
	defer b.Unsubscribe(id1)
	defer b.Unsubscribe(id2)

	if _, ok := recvWithTimeout(t, ch1, 3*time.Second); !ok {
		t.Fatal("subscriber 1 received nothing within 3s")
	}
	if _, ok := recvWithTimeout(t, ch2, 3*time.Second); !ok {
		t.Fatal("subscriber 2 received nothing within 3s")
	}
}

func TestBroadcasterNewSubscriberGetsLatestKeyframeFirst(t *testing.T) {
	session := openTestSession(t, true)

	b := NewBroadcaster(session)
	defer b.Close()

	id1, ch1 := b.Subscribe()
	defer b.Unsubscribe(id1)

	// Wait for at least one keyframe to have gone through the broadcaster.
	var sawKeyframe AccessUnit
	deadline := time.After(4 * time.Second)
waitKeyframe:
	for {
		select {
		case au, ok := <-ch1:
			if !ok {
				t.Fatal("subscriber 1 channel closed early")
			}
			if au.Keyframe {
				sawKeyframe = au
				break waitKeyframe
			}
		case <-deadline:
			t.Fatal("no keyframe observed by subscriber 1 within 4s")
		}
	}

	// A newly subscribed, late-joining consumer must get that same latest
	// keyframe immediately, without waiting for the next one.
	id2, ch2 := b.Subscribe()
	defer b.Unsubscribe(id2)

	first, ok := recvWithTimeout(t, ch2, 1*time.Second)
	if !ok {
		t.Fatal("late subscriber received nothing within 1s")
	}
	if !first.Keyframe {
		t.Fatal("late subscriber's first access unit is not a keyframe")
	}
	if string(first.Data) != string(sawKeyframe.Data) {
		t.Error("late subscriber's priming keyframe does not match the last keyframe seen on the broadcaster")
	}
}

func TestBroadcasterDropsForSlowSubscriberOnly(t *testing.T) {
	session := openTestSession(t, true)

	b := NewBroadcaster(session)
	defer b.Close()

	slowID, slowCh := b.Subscribe()
	fastID, fastCh := b.Subscribe()
	defer b.Unsubscribe(slowID)
	defer b.Unsubscribe(fastID)

	// Actively drain the fast subscriber in the background; never drain
	// the slow one, so its buffer fills and Broadcaster starts dropping
	// for it specifically.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-fastCh:
			case <-stop:
				return
			}
		}
	}()

	deadline := time.After(6 * time.Second)
	for {
		if b.Dropped(slowID) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("slow subscriber never accumulated a drop count within 6s")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Drain whatever is sitting in the slow channel so it stops blocking
	// further sends, then confirm the fast subscriber is still healthy
	// (received something recently, i.e. was never held up by the slow one).
	go func() {
		for range slowCh {
		}
	}()

	if _, ok := recvWithTimeout(t, fastCh, 3*time.Second); !ok {
		t.Fatal("fast subscriber stopped receiving access units")
	}
}

// drainUntilClosed reads from ch, discarding any buffered values, until it
// observes the channel closed or deadline elapses (in which case it fails
// the test). A channel can only ever yield a value or be closed, so this
// cannot hang indefinitely as long as the producer side is well-behaved;
// the deadline guards against a regression that stops closing it.
func drainUntilClosed(t *testing.T, ch <-chan AccessUnit, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("channel was never observed closed within %s", timeout)
		}
	}
}

func TestBroadcasterUnsubscribeClosesChannel(t *testing.T) {
	session := openTestSession(t, true)

	b := NewBroadcaster(session)
	defer b.Close()

	id, ch := b.Subscribe()
	b.Unsubscribe(id)

	drainUntilClosed(t, ch, 2*time.Second)
}

func TestBroadcasterCloseDoesNotCloseSession(t *testing.T) {
	session := openTestSession(t, true)

	b := NewBroadcaster(session)
	_, ch := b.Subscribe()

	// Wait for at least one access unit before closing: openTestSession
	// only waits for Open to return (ICE/DTLS connected), not for the
	// OnTrack callback that sets Session.trackReady - closing immediately
	// races RequestKeyframe below against that callback.
	if _, ok := recvWithTimeout(t, ch, 3*time.Second); !ok {
		t.Fatal("subscriber received nothing within 3s")
	}

	b.Close()

	// The subscriber channel must be closed by Broadcaster.Close.
	drainUntilClosed(t, ch, 2*time.Second)

	// But the underlying Session must still be usable.
	if err := session.RequestKeyframe(); err != nil {
		t.Fatalf("session.RequestKeyframe after Broadcaster.Close: %v", err)
	}
}
