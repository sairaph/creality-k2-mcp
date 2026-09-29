package daemon

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/mcp-wizard/daemon/socket"
)

// This file exercises Hub against a fake session source, never a real
// camera.Session or WebRTC/network I/O (AGENTS.md hard testing rule).

// fakeSession is a test-only upstream: a plain channel plus a close count.
type fakeSession struct {
	mu             sync.Mutex
	aus            chan camera.AccessUnit
	closed         int
	keyframeReqs   int
	keyframeReqErr error
}

func newFakeSession() *fakeSession {
	return &fakeSession{aus: make(chan camera.AccessUnit, 4)}
}

func (s *fakeSession) AccessUnits() <-chan camera.AccessUnit { return s.aus }

func (s *fakeSession) RequestKeyframe() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyframeReqs++
	return s.keyframeReqErr
}

func (s *fakeSession) keyframeRequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keyframeReqs
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

func (s *fakeSession) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// send pushes au and, unlike closing aus directly, is always safe to call
// even after the hub has stopped reading (buffered channel, best-effort).
func (s *fakeSession) send(au camera.AccessUnit) {
	select {
	case s.aus <- au:
	default:
	}
}

func (s *fakeSession) end() { close(s.aus) }

// fakeOpener is a test-only SessionOpener: it hands out sessions in order,
// one per Open call, and records every host it was asked to open.
type fakeOpener struct {
	mu       sync.Mutex
	sessions []*fakeSession
	hosts    []string
	openErr  error
	openN    int
}

func (o *fakeOpener) queue(s *fakeSession) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sessions = append(o.sessions, s)
}

func (o *fakeOpener) Open(ctx context.Context, host string) (upstream, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hosts = append(o.hosts, host)
	o.openN++
	if o.openErr != nil {
		return nil, o.openErr
	}
	if len(o.sessions) == 0 {
		return nil, errors.New("fakeOpener: no session queued")
	}
	s := o.sessions[0]
	o.sessions = o.sessions[1:]
	return s, nil
}

func (o *fakeOpener) openCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.openN
}

func recvWithin(t *testing.T, ch <-chan camera.AccessUnit, timeout time.Duration) camera.AccessUnit {
	t.Helper()
	select {
	case au := <-ch:
		return au
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an access unit")
		return camera.AccessUnit{}
	}
}

// Subscribing opens the upstream session on demand and delivers frames.
func TestHub_Subscribe_OpensOnDemandAndDelivers(t *testing.T) {
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	id, ch := h.Subscribe("printer-a")
	sess.send(camera.AccessUnit{Data: []byte("frame1")})

	au := recvWithin(t, ch, time.Second)
	if string(au.Data) != "frame1" {
		t.Fatalf("got %q, want frame1", au.Data)
	}
	if opener.openCount() != 1 {
		t.Fatalf("Open called %d times, want 1", opener.openCount())
	}
	h.Unsubscribe("printer-a", id)
}

// A new subscriber immediately receives the most recent keyframe.
func TestHub_Subscribe_DeliversLastKeyframeToNewSubscriber(t *testing.T) {
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	id1, ch1 := h.Subscribe("printer-b")
	sess.send(camera.AccessUnit{Data: []byte("kf"), Keyframe: true})
	recvWithin(t, ch1, time.Second)

	id2, ch2 := h.Subscribe("printer-b")
	au := recvWithin(t, ch2, time.Second)
	if string(au.Data) != "kf" || !au.Keyframe {
		t.Fatalf("got %+v, want the last keyframe delivered first", au)
	}
	if opener.openCount() != 1 {
		t.Fatalf("Open called %d times, want 1 (second subscriber reuses the open connection)", opener.openCount())
	}
	h.Unsubscribe("printer-b", id1)
	h.Unsubscribe("printer-b", id2)
}

// The upstream connection is closed once the last subscriber leaves, and
// reopened fresh for a later subscriber.
func TestHub_ClosesUpstreamWhenLastConsumerLeaves(t *testing.T) {
	sess1 := newFakeSession()
	sess2 := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess1)
	opener.queue(sess2)
	h := NewHub(opener)

	id, _ := h.Subscribe("printer-c")
	waitFor(t, time.Second, func() bool { return opener.openCount() == 1 })
	h.Unsubscribe("printer-c", id)

	waitFor(t, time.Second, func() bool { return sess1.closeCount() == 1 })
	if h.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d, want 0 after the last consumer left", h.ActiveCount())
	}

	id2, ch2 := h.Subscribe("printer-c")
	sess2.send(camera.AccessUnit{Data: []byte("again")})
	recvWithin(t, ch2, time.Second)
	if opener.openCount() != 2 {
		t.Fatalf("Open called %d times, want 2 (reopened for the later subscriber)", opener.openCount())
	}
	h.Unsubscribe("printer-c", id2)
}

// RequestKeyframe forwards to the currently connected upstream session, and
// reports an error when there is no subscriber (nothing connected) for the
// host asked about.
func TestHub_RequestKeyframe_ForwardsToCurrentUpstream(t *testing.T) {
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	if err := h.RequestKeyframe("printer-e"); err == nil {
		t.Fatal("RequestKeyframe with no subscriber: want an error, got nil")
	}

	id, ch := h.Subscribe("printer-e")
	sess.send(camera.AccessUnit{Data: []byte("frame")})
	recvWithin(t, ch, time.Second)

	if err := h.RequestKeyframe("printer-e"); err != nil {
		t.Fatalf("RequestKeyframe with an active subscriber: %v", err)
	}
	if got := sess.keyframeRequestCount(); got != 1 {
		t.Fatalf("upstream session's RequestKeyframe called %d times, want 1", got)
	}

	h.Unsubscribe("printer-e", id)
}

// If the upstream session ends on its own while a subscriber remains, the
// hub reconnects (with backoff) and keeps delivering on the same channel.
func TestHub_ReconnectsWithBackoffWhenUpstreamEnds(t *testing.T) {
	reconnectBackoffMin = time.Millisecond
	reconnectBackoffMax = 5 * time.Millisecond
	t.Cleanup(func() {
		reconnectBackoffMin = 500 * time.Millisecond
		reconnectBackoffMax = 10 * time.Second
	})

	sess1 := newFakeSession()
	sess2 := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess1)
	opener.queue(sess2)
	h := NewHub(opener)

	id, ch := h.Subscribe("printer-d")
	sess1.send(camera.AccessUnit{Data: []byte("before")})
	recvWithin(t, ch, time.Second)

	sess1.end() // upstream ends unexpectedly; a subscriber is still here

	sess2.send(camera.AccessUnit{Data: []byte("after")})
	au := recvWithin(t, ch, 2*time.Second)
	if string(au.Data) != "after" {
		t.Fatalf("got %q, want frames to keep arriving on the same subscription after reconnect", au.Data)
	}
	if opener.openCount() != 2 {
		t.Fatalf("Open called %d times, want 2 (one reconnect)", opener.openCount())
	}

	h.Unsubscribe("printer-d", id)
}

// If the upstream session stops delivering any access unit at all for
// noMediaTimeout while a subscriber remains - a session that stays
// connected but whose printer has simply stopped producing frames, exactly
// what dev_docs/t11e-soak-report.md and dev_docs/camera-keyframe-rca.md
// observed against the real K2 - the hub tears it down and reopens with the
// existing capped backoff (review backlog item 45), without ever
// unsubscribing the caller: the same subscription id and channel keep
// working once the reopened session starts delivering again.
func TestHub_NoMediaWatchdog_ReconnectsWhenUpstreamGoesSilent(t *testing.T) {
	noMediaTimeout = 20 * time.Millisecond
	reconnectBackoffMin = time.Millisecond
	reconnectBackoffMax = 5 * time.Millisecond
	t.Cleanup(func() {
		noMediaTimeout = 20 * time.Second
		reconnectBackoffMin = 500 * time.Millisecond
		reconnectBackoffMax = 10 * time.Second
	})

	sess1 := newFakeSession()
	sess2 := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess1)
	opener.queue(sess2)
	h := NewHub(opener)

	id, ch := h.Subscribe("printer-f")
	sess1.send(camera.AccessUnit{Data: []byte("before")})
	recvWithin(t, ch, time.Second)

	// sess1 never sends anything else and is never closed by the printer
	// itself (unlike TestHub_ReconnectsWithBackoffWhenUpstreamEnds, its
	// channel stays open) - only the no-media watchdog's silence timeout
	// should end it.
	waitFor(t, 2*time.Second, func() bool { return opener.openCount() == 2 })
	waitFor(t, time.Second, func() bool { return sess1.closeCount() == 1 })

	// The subscriber was never unsubscribed: the same channel keeps
	// delivering once the reopened session (sess2) starts sending again.
	sess2.send(camera.AccessUnit{Data: []byte("after")})
	au := recvWithin(t, ch, 2*time.Second)
	if string(au.Data) != "after" {
		t.Fatalf("got %q, want frames to keep arriving on the same subscription after the no-media reconnect", au.Data)
	}

	h.Unsubscribe("printer-f", id)
}

// --- Rolling GOP buffer, Hub.Snapshot and PLI coalescing (review backlog
// item 51) ---
//
// These tests use internal/camera/decode/testdata/camera_sample_clip.h264
// via loadClipFixture (recorder_test.go, same package) for real, decodable
// H.264 access units, so Hub.Snapshot's decode path is genuinely exercised
// end to end, not just its buffering bookkeeping.

// goldenBufferLen and goldenBufferBytes read pc's rolling buffer state
// under its own lock, for tests in this package (same package as
// camera_hub.go, so its unexported fields are directly reachable).
func goldenBufferLen(pc *printerCamera) int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return len(pc.goldenBuffer)
}

func goldenBufferBytes(pc *printerCamera) int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.goldenBytes
}

// TestHub_GoldenBuffer_TracksKeyframeAndGrows confirms dispatch starts a
// fresh rolling buffer at each keyframe and appends every following access
// unit to it.
func TestHub_GoldenBuffer_TracksKeyframeAndGrows(t *testing.T) {
	fixture := loadClipFixture(t)
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	id, ch := h.Subscribe("printer-golden-a")
	defer h.Unsubscribe("printer-golden-a", id)

	now := time.Now()
	sess.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: now})
	recvWithin(t, ch, time.Second)
	waitFor(t, time.Second, func() bool { return goldenBufferLen(h.byKey["printer-golden-a"]) == 1 })

	sess.send(camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1), CapturedAt: now.Add(time.Millisecond)})
	recvWithin(t, ch, time.Second)
	waitFor(t, time.Second, func() bool { return goldenBufferLen(h.byKey["printer-golden-a"]) == 2 })

	// A fresh keyframe restarts the buffer rather than appending to it.
	sess.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(2), CapturedAt: now.Add(2 * time.Millisecond)})
	recvWithin(t, ch, time.Second)
	waitFor(t, time.Second, func() bool { return goldenBufferLen(h.byKey["printer-golden-a"]) == 1 })
}

// TestHub_GoldenBuffer_EvictsOnDurationBound confirms that exceeding
// goldenBufferMaxDuration drops the whole buffer (not just the oldest
// element), so a later, non-keyframe access unit is not appended to it
// until the next keyframe restarts it.
func TestHub_GoldenBuffer_EvictsOnDurationBound(t *testing.T) {
	origDuration := goldenBufferMaxDuration
	goldenBufferMaxDuration = 50 * time.Millisecond
	t.Cleanup(func() { goldenBufferMaxDuration = origDuration })

	fixture := loadClipFixture(t)
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	id, ch := h.Subscribe("printer-golden-b")
	defer h.Unsubscribe("printer-golden-b", id)

	base := time.Now()
	sess.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: base})
	recvWithin(t, ch, time.Second)
	waitFor(t, time.Second, func() bool { return goldenBufferLen(h.byKey["printer-golden-b"]) == 1 })

	// This access unit's CapturedAt is well beyond goldenBufferMaxDuration
	// after the keyframe's own CapturedAt: it must evict the whole buffer,
	// not extend it to 2 elements.
	sess.send(camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1), CapturedAt: base.Add(time.Second)})
	recvWithin(t, ch, time.Second)
	waitFor(t, time.Second, func() bool { return goldenBufferLen(h.byKey["printer-golden-b"]) == 0 })
}

// TestHub_GoldenBuffer_EvictsOnByteBound mirrors the duration test for
// goldenBufferMaxBytes: goldenBufferMaxBytes is shrunk below the sample
// clip's own keyframe access unit size, so the very first dispatch (the
// keyframe that would otherwise start the buffer) must evict it
// immediately within that same call - there is no stable intermediate
// state where an outside observer could see a 1-element buffer here, only
// the eventual 0.
func TestHub_GoldenBuffer_EvictsOnByteBound(t *testing.T) {
	fixture := loadClipFixture(t)
	if len(fixture.aus[0]) <= 100 {
		t.Fatalf("sample clip's keyframe access unit is %d bytes, want > 100 for this test's premise", len(fixture.aus[0]))
	}
	origBytes := goldenBufferMaxBytes
	goldenBufferMaxBytes = 100
	t.Cleanup(func() { goldenBufferMaxBytes = origBytes })

	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	id, ch := h.Subscribe("printer-golden-c")
	defer h.Unsubscribe("printer-golden-c", id)

	sess.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: time.Now()})
	recvWithin(t, ch, time.Second)

	waitFor(t, time.Second, func() bool { return goldenBufferLen(h.byKey["printer-golden-c"]) == 0 })
}

// TestHub_Snapshot_WarmBufferDecodesWithoutPLI confirms Hub.Snapshot
// decodes directly from an already-fresh rolling buffer (a keyframe plus
// at least one following access unit) without ever sending a PLI (owner
// design decision: "no PLI needed when the buffer is fresh").
func TestHub_Snapshot_WarmBufferDecodesWithoutPLI(t *testing.T) {
	fixture := loadClipFixture(t)
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	// A real subscriber (a viewer or recording) is what would normally
	// already have the connection open and the buffer warm in production;
	// keep it subscribed through the Snapshot call below (unsubscribing
	// beforehand would close the connection immediately per every other
	// Subscribe/Unsubscribe test's own contract, taking the buffer with
	// it).
	id, ch := h.Subscribe("printer-snap-a")
	defer h.Unsubscribe("printer-snap-a", id)
	now := time.Now()
	sess.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: now})
	recvWithin(t, ch, time.Second)
	sess.send(camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1), CapturedAt: now.Add(time.Millisecond)})
	recvWithin(t, ch, time.Second)
	waitFor(t, time.Second, func() bool { return goldenBufferLen(h.byKey["printer-snap-a"]) == 2 })

	result, err := h.Snapshot(context.Background(), "printer-snap-a")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if result.Width == 0 || result.Height == 0 {
		t.Fatalf("Snapshot result has zero dimensions: %+v", result)
	}
	if got := sess.keyframeRequestCount(); got != 0 {
		t.Fatalf("Snapshot from a fresh buffer sent %d PLIs, want 0", got)
	}
}

// TestHub_Snapshot_ColdStart_HubSendsNoPLIOfItsOwn confirms that while
// waiting for a connection's very first keyframe (haveEverKeyframe still
// false), the hub itself never sends a PLI: that bootstrap retry is
// camera.Session's own job (Session.Open's OnTrack/retryPLIUntilKeyframe,
// at pliRetryInterval, unrelated to this fake session double), and the
// hub deliberately stays out of the way rather than duplicating it (see
// pump's own doc comment). Snapshot must still return a clear timeout
// error when the budget elapses with nothing ever buffered.
func TestHub_Snapshot_ColdStart_HubSendsNoPLIOfItsOwn(t *testing.T) {
	origBudget := hubSnapshotBudget
	hubSnapshotBudget = 60 * time.Millisecond
	t.Cleanup(func() { hubSnapshotBudget = origBudget })
	origPLI := pliCoalesceInterval
	pliCoalesceInterval = 5 * time.Millisecond
	t.Cleanup(func() { pliCoalesceInterval = origPLI })

	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	_, err := h.Snapshot(context.Background(), "printer-snap-b")
	if err == nil {
		t.Fatal("Snapshot with no keyframe ever sent: want a timeout error, got nil")
	}
	if got := sess.keyframeRequestCount(); got != 0 {
		t.Fatalf("hub sent %d PLIs of its own while waiting for a cold connection's first keyframe, want 0 (bootstrap retry is camera.Session's own job)", got)
	}
}

// TestHub_RequestKeyframe_CoalescesAcrossConsumers confirms several
// concurrent RequestKeyframe callers for the same upstream collapse into
// at most one actual PLI per pliCoalesceInterval.
func TestHub_RequestKeyframe_CoalescesAcrossConsumers(t *testing.T) {
	origPLI := pliCoalesceInterval
	pliCoalesceInterval = 60 * time.Millisecond
	t.Cleanup(func() { pliCoalesceInterval = origPLI })

	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	id, ch := h.Subscribe("printer-coalesce")
	defer h.Unsubscribe("printer-coalesce", id)
	sess.send(camera.AccessUnit{Data: []byte("frame")})
	recvWithin(t, ch, time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.RequestKeyframe("printer-coalesce")
		}()
	}
	wg.Wait()

	if got := sess.keyframeRequestCount(); got != 1 {
		t.Fatalf("8 concurrent RequestKeyframe calls within one coalescing window sent %d PLIs, want exactly 1", got)
	}

	// After the coalescing window passes, a new request is allowed through.
	time.Sleep(2 * pliCoalesceInterval)
	if err := h.RequestKeyframe("printer-coalesce"); err != nil {
		t.Fatalf("RequestKeyframe after the coalescing window: %v", err)
	}
	if got := sess.keyframeRequestCount(); got != 2 {
		t.Fatalf("RequestKeyframe after the coalescing window sent %d total PLIs, want 2", got)
	}
}

// TestHub_Snapshot_KeepWarmExpiry_ClosesUpstream confirms Hub.Snapshot
// keeps the upstream connection open for hubKeepWarmDuration after the
// call returns (so a follow-up snapshot taken soon after reuses it), and
// that the connection actually closes once that window passes with no
// further activity.
func TestHub_Snapshot_KeepWarmExpiry_ClosesUpstream(t *testing.T) {
	origWarm := hubKeepWarmDuration
	hubKeepWarmDuration = 60 * time.Millisecond
	t.Cleanup(func() { hubKeepWarmDuration = origWarm })

	fixture := loadClipFixture(t)
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	// Prime the buffer via a throwaway subscription (Subscribe/Unsubscribe
	// closes immediately with no snapshot activity, matching every other
	// pre-existing Subscribe/Unsubscribe test - this is exactly why
	// Snapshot's own acquire, below, is what must be the thing keeping the
	// connection warm here, not this priming step).
	id, ch := h.Subscribe("printer-warm")
	now := time.Now()
	sess.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: now})
	recvWithin(t, ch, time.Second)
	sess.send(camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1), CapturedAt: now.Add(time.Millisecond)})
	recvWithin(t, ch, time.Second)
	h.Unsubscribe("printer-warm", id)
	waitFor(t, time.Second, func() bool { return sess.closeCount() == 1 })

	// Re-warm it with an actual Snapshot call against a second queued
	// session.
	sess2 := newFakeSession()
	opener.queue(sess2)
	subID, ch2 := h.Subscribe("printer-warm")
	now2 := time.Now()
	sess2.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: now2})
	recvWithin(t, ch2, time.Second)
	sess2.send(camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1), CapturedAt: now2.Add(time.Millisecond)})
	recvWithin(t, ch2, time.Second)
	h.Unsubscribe("printer-warm", subID)
	waitFor(t, time.Second, func() bool { return sess2.closeCount() == 1 })

	// Now take a real Snapshot against a third session: it must stay open
	// afterward (kept warm), with no real subscriber at all.
	sess3 := newFakeSession()
	opener.queue(sess3)
	// Prime sess3 immediately so Snapshot's own acquire-triggered Open can
	// find a fresh keyframe without waiting out the full budget.
	go func() {
		now3 := time.Now()
		sess3.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: now3})
		sess3.send(camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1), CapturedAt: now3.Add(time.Millisecond)})
	}()

	if _, err := h.Snapshot(context.Background(), "printer-warm"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if h.ActiveCount() != 1 {
		t.Fatalf("ActiveCount = %d right after Snapshot returned, want 1 (kept warm)", h.ActiveCount())
	}

	// Well before hubKeepWarmDuration elapses, it must still be open.
	time.Sleep(hubKeepWarmDuration / 3)
	if sess3.closeCount() != 0 {
		t.Fatalf("upstream closed before hubKeepWarmDuration elapsed")
	}

	waitFor(t, 2*time.Second, func() bool { return sess3.closeCount() == 1 })
	if h.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d after the keep-warm window expired, want 0", h.ActiveCount())
	}
}

// TestServer_CameraSnapshotRPC_RoundTrip drives MethodCameraSnapshot over a
// real AF_UNIX socket in a t.TempDir() (allowed under AGENTS.md's hard
// testing rule: this is not network I/O, and it never spawns the daemon or
// product binary - the Server runs in-process, same as every other
// round-trip test in this package), confirming the wire result decodes as
// a valid JPEG with sane, non-zero dimensions.
func TestServer_CameraSnapshotRPC_RoundTrip(t *testing.T) {
	fixture := loadClipFixture(t)
	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	hub := NewHub(opener)

	go func() {
		now := time.Now()
		sess.send(camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0), CapturedAt: now})
		sess.send(camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1), CapturedAt: now.Add(time.Millisecond)})
	}()

	dir := t.TempDir()
	opts := Options{
		Paths:    PathsIn(dir),
		Hub:      hub,
		Watchdog: newTestWatchdog(newFakeStateSource(), newFakeSender()),
		Recorder: NewRecorder(filepath.Join(dir, "recordings"), hub, newFakeStateSource(), nil),
	}
	s := openTestServer(t, opts)
	serveInBackground(t, s)

	conn, err := socket.Dial(opts.Paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var res CameraSnapshotResult
	if err := conn.Call(context.Background(), MethodCameraSnapshot, CameraSnapshotParams{Host: "printer-rpc-snap"}, &res); err != nil {
		t.Fatalf("camera.snapshot: %v", err)
	}
	if res.Width == 0 || res.Height == 0 {
		t.Fatalf("camera.snapshot result has zero dimensions: %+v", res)
	}
	img, err := jpeg.Decode(bytes.NewReader(res.ImageJPEG))
	if err != nil {
		t.Fatalf("decode ImageJPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != res.Width || b.Dy() != res.Height {
		t.Errorf("decoded JPEG is %dx%d, want %dx%d (matching the result's own Width/Height)", b.Dx(), b.Dy(), res.Width, res.Height)
	}
}
