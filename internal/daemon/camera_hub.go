package daemon

import (
	"context"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/camera/decode"
)

// subscriberBufferSize is how many access units a slow subscriber can fall
// behind by before the hub starts dropping frames for it, matching
// internal/camera.Broadcaster's own buffer size.
const subscriberBufferSize = 16

// reconnectBackoffMin and reconnectBackoffMax bound how long the hub waits
// between reconnect attempts to a printer's camera after the upstream
// session ends while consumers are still subscribed. Package variables, not
// constants, so tests can shrink them rather than waiting out a real
// multi-second backoff.
var (
	reconnectBackoffMin = 500 * time.Millisecond
	reconnectBackoffMax = 10 * time.Second
)

// noMediaTimeout is how long pump waits for at least one access unit (of
// any kind, not just a keyframe) from the current upstream session before
// concluding it has gone silent and tearing it down so run's own reconnect
// loop reopens it (review backlog item 45). A camera.Session can stay
// ICE/DTLS "connected" indefinitely while the printer's own camera encoder
// has simply stopped producing frames - dev_docs/t11e-soak-report.md and
// dev_docs/camera-keyframe-rca.md both observed exactly this against the
// real K2 (long idle stretches with the transport still healthy) - and
// neither pion nor camera.Session itself currently detects that condition.
// A package variable, not a constant, so a test can shrink it.
var noMediaTimeout = 20 * time.Second

// --- Rolling GOP buffer (review backlog item 51) ---
//
// Owner design decision: stream at the camera's own rate when needed and
// take snapshots from the stream. Each printerCamera keeps a rolling
// buffer of the last complete keyframe access unit (with SPS/PPS) plus
// every access unit received since, bounded by goldenBufferMaxBytes or
// goldenBufferMaxDuration (whichever is hit first); exceeding either bound
// drops the whole buffer rather than trimming it, so the next keyframe
// starts a fresh one - a partial GOP with its head trimmed off is not
// decodable anyway (decode.Decoder.DecodeLatest needs the keyframe access
// unit at the start). Hub.Snapshot decodes this buffer directly, with no
// PLI needed when it is already non-empty ("fresh").
// goldenBufferMaxBytes bounds the rolling buffer's total access unit size.
// 8 MiB comfortably covers several seconds of 1280x720 H.264 at the K2's
// observed bitrate while staying far under the noMediaTimeout/
// goldenBufferMaxDuration bound in practice. A package variable, not a
// constant, so a test can shrink it instead of allocating megabytes of
// fixture data to exercise the bound.
var goldenBufferMaxBytes = 8 << 20

// goldenBufferMaxDuration bounds the rolling buffer's span, by capture
// time, from its keyframe to its newest access unit. A package variable,
// not a constant, so a test can shrink it instead of waiting out a real
// 10 second window.
var goldenBufferMaxDuration = 10 * time.Second

// pliCoalesceInterval bounds how often the hub actually sends an RTCP PLI
// for one upstream connection, regardless of how many separate reasons ask
// for one within that window: an explicit Hub.RequestKeyframe call from a
// viewer/recorder that hit fmp4.ErrNoKeyframe, a Hub.Snapshot decode
// failure, and pump's own automatic recovery retry (below) all share this
// same rate limit (review backlog item 51, item 3's "rate-limited to at
// most 1 per second per upstream across all consumers"). This is separate
// from, and in addition to, camera.Session's own bootstrap retry
// (pliRetryInterval) for a session's very first keyframe: pump's recovery
// retry only ever fires once haveEverKeyframe is true (see pump), so the
// two mechanisms never both fire for the same gap. A package variable, not
// a constant, so a test can shrink it.
var pliCoalesceInterval = 1 * time.Second

// hubKeepWarmDuration is how long Hub.Snapshot keeps a printer's upstream
// camera connection open after the last snapshot, so a caller taking
// several snapshots in a row (or a CLI/TUI user checking a printer
// periodically) does not pay the cold-connect keyframe-wait cost every
// time. It is independent of, and additional to, the existing "closed
// immediately once the last real subscriber (viewer/recording) leaves"
// behaviour: a connection with zero real subscribers and an expired (or
// never set) warm deadline still closes immediately, matching every
// pre-existing Subscribe/Unsubscribe test. A package variable, not a
// constant, so a test can shrink it.
var hubKeepWarmDuration = 60 * time.Second

// hubSnapshotBudget bounds Hub.Snapshot's wait for the buffer's first
// complete keyframe on a cold connection, regardless of the caller's own
// ctx deadline - matching internal/camera.Snapshot's own snapshotBudget
// rationale (dev_docs/camera-keyframe-rca.md, dev_docs/t11e-soak-report.md:
// live cold-connect latency observed up to 16s). A package variable, not a
// constant, so a test can shrink it.
var hubSnapshotBudget = 20 * time.Second

// hubSnapshotPollInterval is how often Hub.Snapshot re-checks the rolling
// buffer while waiting for it to become usable. A package variable, not a
// constant, so a test can shrink it.
var hubSnapshotPollInterval = 20 * time.Millisecond

// upstream is the slice of camera.Session's behaviour the hub needs from
// one open upstream camera connection. *camera.Session satisfies this
// directly; tests substitute a fake session source so no real WebRTC
// session or network I/O ever runs (AGENTS.md hard testing rule).
type upstream interface {
	AccessUnits() <-chan camera.AccessUnit
	RequestKeyframe() error
	Close() error
}

var _ upstream = (*camera.Session)(nil)

// SessionOpener opens the one upstream camera connection for a printer's
// camera host.
type SessionOpener interface {
	Open(ctx context.Context, host string) (upstream, error)
}

// realOpener is the production SessionOpener: real WebRTC to host over the
// network, relaying every camera.Session's lifecycle events into logger
// once one is available (review backlog item 46). Never used by a test.
//
// logger starts nil: NewProductionOptions builds the Hub (and so this
// realOpener) before the daemon's own log file exists, so daemon.Open wires
// a real logger in afterward via Hub.SetLogger. A pointer receiver plus its
// own mutex, rather than a plain struct field set once at construction, is
// what makes that later wiring safe: Open (in internal/camera) may already
// be running concurrently in another printerCamera's goroutine by the time
// SetLogger is called.
type realOpener struct {
	mu     sync.Mutex
	logger camera.EventLogger
}

func (ro *realOpener) Open(ctx context.Context, host string) (upstream, error) {
	ro.mu.Lock()
	logger := ro.logger
	ro.mu.Unlock()
	if logger == nil {
		return camera.Open(ctx, host)
	}
	return camera.Open(ctx, host, camera.WithLogger(logger))
}

// setLogger installs logger for every future Open call. Called only by
// Hub.SetLogger.
func (ro *realOpener) setLogger(logger camera.EventLogger) {
	ro.mu.Lock()
	ro.logger = logger
	ro.mu.Unlock()
}

// printerCamera is one printer's hub entry: a long-lived fan-out that
// outlives any single upstream connection, fed by a connect loop that
// reopens the upstream session with backoff whenever it ends while
// subscribers remain. This cannot be internal/camera.Broadcaster directly
// (Broadcaster is tied to the one *camera.Session it was built over and
// exits for good once that session's channel closes); this is the same
// dispatch shape, kept alive across reconnects instead.
type printerCamera struct {
	host string

	mu           sync.Mutex
	subs         map[int]chan camera.AccessUnit
	nextID       int
	lastKeyframe *camera.AccessUnit
	// current is the upstream session this printerCamera is presently
	// pumping from, or nil between reconnects (see run/pump below). Guarded
	// by mu so requestKeyframe (called from an HTTP viewer's goroutine, T11b)
	// can safely read it while run's goroutine sets and clears it.
	current upstream

	// logger is a snapshot of the owning Hub's logger, taken once at
	// creation (Hub.newPrinterCamera runs under h.mu, the same lock
	// SetLogger updates h.logger under, so this always reflects whatever
	// was wired in by the time this printerCamera was created). May be nil.
	logger camera.EventLogger

	// lastMediaAt is when dispatch last delivered an access unit (of any
	// kind, not just a keyframe) on this connection, or the zero Time if
	// none has arrived yet. Guarded by mu; surfaced via Hub.LastMediaAt for
	// get_printer_status (review backlog item 47).
	lastMediaAt time.Time

	// --- Rolling GOP buffer and snapshot support (review backlog item 51) ---

	// goldenBuffer holds the current rolling GOP: index 0 is the most
	// recent complete keyframe access unit, and every later element is an
	// access unit received since, in order. Empty whenever no usable
	// keyframe is currently buffered (before the first one ever arrives, or
	// after a bound-exceeded drop or a decode failure invalidates it).
	// goldenBytes is the running total of every element's Data length, kept
	// alongside the buffer purely so the bound check never has to re-sum it
	// on every access unit.
	goldenBuffer []camera.AccessUnit
	goldenBytes  int
	// haveEverKeyframe is set once, the first time this printerCamera's
	// current upstream connection ever produces a complete keyframe access
	// unit, and reset on every fresh upstream connection (see run). It is
	// what tells pump's automatic recovery-retry apart from
	// camera.Session's own bootstrap retry: before the first keyframe,
	// Session is already retrying on its own (pliRetryInterval), so pump
	// must stay out of the way; once haveEverKeyframe is true, Session's
	// own bootstrap loop has long since stopped, so a later gap (the
	// buffer going empty again) needs pump's own retry instead.
	haveEverKeyframe bool
	// lastPLIAt is when this printerCamera last actually sent a PLI to its
	// current upstream session, shared by every source that can trigger
	// one (pump's automatic retry, an explicit RequestKeyframe call, a
	// Snapshot decode failure) so they coalesce into at most one send per
	// pliCoalesceInterval (review backlog item 51, item 3).
	lastPLIAt time.Time

	// decoder is this printer's own reused decode.Decoder for Hub.Snapshot,
	// created lazily on first use and closed whenever the upstream
	// connection is torn down (teardownLocked): a fresh connection means a
	// fresh rolling buffer, so there is nothing left for a stale decoder
	// instance to usefully hold onto. decodeMu serializes every decode
	// through it (only one decode in flight per printer at a time, per the
	// task's "one reused Decoder per printer (serialized)"), independent of
	// mu (which guards short, non-blocking field access) so a slow decode
	// never blocks dispatch/subscribe/unsubscribe.
	decoder  *decode.Decoder
	decodeMu sync.Mutex

	// --- Demand tracking: when the upstream connection is open ---

	// refCount is how many callers currently want this connection open: one
	// per real subscriber (len(subs) tracks the same thing, refCount exists
	// so a Hub.Snapshot call can hold a reservation of its own without
	// registering a subscriber channel that would receive every access
	// unit it has no use for) plus one for each in-flight Hub.Snapshot
	// call.
	refCount int
	// warmUntil is the deadline (if any) a Hub.Snapshot call has asked the
	// connection to stay open until, even after refCount reaches zero
	// (hubKeepWarmDuration after that call). Zero means no such deadline is
	// pending - the pre-existing "close immediately once the last real
	// subscriber leaves" behaviour applies. Unsubscribe still respects an
	// already-pending warm deadline left over from a recent Snapshot call
	// (see release), matching "keep it warm for 60s after the last
	// snapshot/consumer" - whichever of those was more recent.
	warmUntil time.Time
	// closeTimer, when non-nil, is a pending call to checkIdle armed for
	// warmUntil; acquire cancels it (a new subscriber or snapshot call
	// supersedes any pending close).
	closeTimer *time.Timer

	// running is true exactly while cancel/done below describe a live run
	// goroutine (an open, or opening, upstream connection).
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// Hub owns one upstream camera connection per printer host, opened on
// demand by the first Subscribe or Snapshot call and closed once the last
// consumer leaves and, for a connection a Snapshot call kept warm, once
// hubKeepWarmDuration has passed since the last snapshot (component 1 of
// internal/daemon, kept entirely separate from the idle-heat Watchdog per
// dev_docs/safety-architecture.md section 10 D2). Viewer (T11b), recording
// (T11c) and snapshot (review backlog item 51) consumers all subscribe
// through this same API.
type Hub struct {
	opener SessionOpener

	mu     sync.Mutex
	byKey  map[string]*printerCamera
	logger camera.EventLogger // nil until SetLogger is called; see SetLogger
}

// NewHub builds a Hub. A nil opener defaults to the production
// implementation (real WebRTC via internal/camera.Open); tests always pass
// a fake.
func NewHub(opener SessionOpener) *Hub {
	if opener == nil {
		opener = &realOpener{}
	}
	return &Hub{opener: opener, byKey: make(map[string]*printerCamera)}
}

// SetLogger wires logger into every upstream camera session this Hub opens
// from here on (via realOpener, camera.WithLogger) and into the hub's own
// diagnostics, such as the no-media watchdog (review backlog items 45 and
// 46). Hub is built once, in NewProductionOptions, before the daemon's own
// log file exists; daemon.Open calls SetLogger right after opening it. A
// no-op for a test's fake opener (SetLogger only recognizes the production
// *realOpener), so every existing test's behaviour is unchanged. logger may
// be nil to go back to no-op logging.
func (h *Hub) SetLogger(logger camera.EventLogger) {
	h.mu.Lock()
	h.logger = logger
	h.mu.Unlock()
	if ro, ok := h.opener.(*realOpener); ok {
		ro.setLogger(logger)
	}
}

// getOrCreate returns host's printerCamera, creating an idle one (no open
// connection yet) if this is the first time host has ever been asked
// about.
func (h *Hub) getOrCreate(host string) *printerCamera {
	h.mu.Lock()
	defer h.mu.Unlock()
	pc, ok := h.byKey[host]
	if !ok {
		pc = &printerCamera{host: host, subs: make(map[int]chan camera.AccessUnit), logger: h.logger}
		h.byKey[host] = pc
	}
	return pc
}

// Subscribe opens host's upstream camera connection if it is not already
// open for this hub, and returns a subscription id (for Unsubscribe) and
// the channel access units are delivered on. If a keyframe has already been
// seen on this connection, it is delivered first, so a new consumer can
// start decoding immediately instead of waiting for the next one (matching
// internal/camera.Broadcaster.Subscribe's own behaviour).
func (h *Hub) Subscribe(host string) (id int, ch <-chan camera.AccessUnit) {
	pc := h.getOrCreate(host)
	pc.acquire(h)
	return pc.addSub()
}

// Unsubscribe removes a subscriber. Once a printer's last subscriber
// leaves (and no Hub.Snapshot call has left a warm-keep window still
// running), its upstream connection is closed.
func (h *Hub) Unsubscribe(host string, id int) {
	h.mu.Lock()
	pc, ok := h.byKey[host]
	h.mu.Unlock()
	if !ok {
		return
	}
	pc.removeSub(id)
	pc.release(h, 0)
}

// Snapshot returns the latest decodable picture from host's camera: if no
// upstream connection is currently open, one is opened; the call then
// waits (bounded by hubSnapshotBudget, regardless of ctx's own deadline)
// for the rolling GOP buffer to hold a usable keyframe, decodes it with
// this printer's own reused, serialized decode.Decoder, and returns the
// result. No PLI is sent when the buffer is already fresh (review backlog
// item 51). The connection is kept warm for hubKeepWarmDuration after this
// call returns, so a follow-up snapshot taken soon after reuses it instead
// of paying a fresh cold-connect cost.
func (h *Hub) Snapshot(ctx context.Context, host string) (*camera.SnapshotResult, error) {
	pc := h.getOrCreate(host)
	pc.acquire(h)
	defer pc.release(h, hubKeepWarmDuration)
	return pc.snapshot(ctx)
}

// RequestKeyframe asks host's upstream camera session for a fresh keyframe
// (an RTCP PLI, on the real camera.Session), for a viewer stream or
// recording that just hit fmp4.ErrNoKeyframe (T11b/T11c), coalesced with
// every other reason this printer's connection might need one right now to
// at most one actual PLI per pliCoalesceInterval (review backlog item 51).
// It returns an error if host has no printerCamera at all, or no upstream
// session is currently connected (e.g. mid-reconnect backoff); either case
// is expected to be transient and safe to ignore for a caller that will
// simply try again on the next access unit.
func (h *Hub) RequestKeyframe(host string) error {
	h.mu.Lock()
	pc, ok := h.byKey[host]
	h.mu.Unlock()
	if !ok {
		return fmt.Errorf("daemon: no active camera connection for %s", host)
	}
	return pc.requestKeyframe()
}

// ActiveCount returns how many printers currently have an open (or
// opening) upstream camera connection, for the daemon's idle self-exit
// check ("no viewers, recordings ... for 10 minutes"). A printerCamera
// kept warm only for a recent Hub.Snapshot call, with zero real
// subscribers, still counts here: its upstream connection really is open.
func (h *Hub) ActiveCount() int {
	h.mu.Lock()
	pcs := make([]*printerCamera, 0, len(h.byKey))
	for _, pc := range h.byKey {
		pcs = append(pcs, pc)
	}
	h.mu.Unlock()

	n := 0
	for _, pc := range pcs {
		pc.mu.Lock()
		if pc.running {
			n++
		}
		pc.mu.Unlock()
	}
	return n
}

// LastMediaAt reports host's camera connection state for get_printer_status
// (review backlog item 47): connected is true whenever this Hub currently
// has an open upstream connection for host, regardless of whether any
// access unit has actually arrived yet; hasMedia and at are only
// meaningful when connected is true, and report whether at least one access
// unit (of any kind, not just a keyframe) has ever been dispatched on the
// current connection and, if so, when the most recent one was. A host with
// no open connection (connected false) is not an error - get_printer_status
// never opens one just to check this - it simply means nothing is known
// right now.
func (h *Hub) LastMediaAt(host string) (at time.Time, hasMedia bool, connected bool) {
	h.mu.Lock()
	pc, ok := h.byKey[host]
	h.mu.Unlock()
	if !ok {
		return time.Time{}, false, false
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return pc.lastMediaAt, !pc.lastMediaAt.IsZero(), pc.running
}

// Close tears down every open upstream connection, for daemon shutdown.
func (h *Hub) Close() {
	h.mu.Lock()
	entries := make([]*printerCamera, 0, len(h.byKey))
	for _, pc := range h.byKey {
		entries = append(entries, pc)
	}
	h.byKey = make(map[string]*printerCamera)
	h.mu.Unlock()

	for _, pc := range entries {
		pc.forceClose()
	}
}

// addSub registers a new subscriber and returns its id and the channel it
// will receive access units on, priming it with the last known keyframe if
// one has been seen.
func (pc *printerCamera) addSub() (int, <-chan camera.AccessUnit) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	id := pc.nextID
	pc.nextID++
	ch := make(chan camera.AccessUnit, subscriberBufferSize)
	pc.subs[id] = ch
	if pc.lastKeyframe != nil {
		ch <- *pc.lastKeyframe
	}
	return id, ch
}

// removeSub removes id, closing its channel. Safe to call with an id that
// is already gone (a no-op).
func (pc *printerCamera) removeSub(id int) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if ch, ok := pc.subs[id]; ok {
		close(ch)
		delete(pc.subs, id)
	}
}

// acquire registers one more reason this connection must stay open,
// cancels any pending close and (re)opens the upstream connection if it is
// not already running.
func (pc *printerCamera) acquire(h *Hub) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.refCount++
	if pc.closeTimer != nil {
		pc.closeTimer.Stop()
		pc.closeTimer = nil
	}
	if !pc.running {
		pc.startLocked(h)
	}
}

// release undoes one acquire. If warm > 0, it also extends warmUntil to
// warm from now (used by Hub.Snapshot); a real subscriber's Unsubscribe
// passes warm 0, which leaves any warm deadline a recent Snapshot call may
// have set untouched (still respected - "60s after the last snapshot/
// consumer", whichever was later - but never extended by a plain
// unsubscribe). Once refCount reaches zero, the connection closes
// immediately unless warmUntil is still in the future, in which case a
// close is scheduled for then instead.
func (pc *printerCamera) release(h *Hub, warm time.Duration) {
	pc.mu.Lock()
	pc.refCount--
	if warm > 0 {
		pc.warmUntil = time.Now().Add(warm)
	}
	n := pc.refCount
	warmUntil := pc.warmUntil
	pc.mu.Unlock()

	if n > 0 {
		return
	}
	remaining := time.Until(warmUntil)
	if remaining > 0 {
		pc.scheduleClose(h, remaining)
		return
	}
	pc.teardown()
}

// scheduleClose arms (replacing any prior) a timer that re-checks whether
// this connection is still idle after d and, if so, tears it down.
func (pc *printerCamera) scheduleClose(h *Hub, d time.Duration) {
	pc.mu.Lock()
	if pc.closeTimer != nil {
		pc.closeTimer.Stop()
	}
	pc.closeTimer = time.AfterFunc(d, pc.checkIdle)
	pc.mu.Unlock()
}

// checkIdle re-validates that this connection is still unwanted (refCount
// still zero and any warm deadline has actually passed - either could have
// been renewed by a new acquire since the timer was armed) before actually
// tearing it down.
func (pc *printerCamera) checkIdle() {
	pc.mu.Lock()
	pc.closeTimer = nil
	if pc.refCount > 0 {
		pc.mu.Unlock()
		return
	}
	remaining := time.Until(pc.warmUntil)
	pc.mu.Unlock()
	if remaining > 0 {
		return // a concurrent release already rescheduled further out
	}
	pc.teardown()
}

// startLocked starts run for pc. pc.mu must already be held.
func (pc *printerCamera) startLocked(h *Hub) {
	ctx, cancel := context.WithCancel(context.Background())
	pc.cancel = cancel
	pc.done = make(chan struct{})
	pc.running = true
	pc.haveEverKeyframe = false
	go pc.run(ctx, h.opener, pc.done)
}

// teardown stops pc's run loop (if any) and clears every piece of state
// that belongs to the connection it was pumping: the rolling buffer, the
// last-seen keyframe, and the reused decoder (a fresh connection later
// means a fresh buffer, so there is nothing useful left for a stale
// decoder instance to hold onto).
func (pc *printerCamera) teardown() {
	pc.mu.Lock()
	cancel := pc.cancel
	done := pc.done
	running := pc.running
	pc.running = false
	pc.cancel = nil
	pc.done = nil
	pc.mu.Unlock()

	if running && cancel != nil {
		cancel()
		<-done
	}

	// Only clear the rolling buffer and last-seen keyframe once run/pump
	// have fully stopped (confirmed by <-done above), never before: pump
	// can still dispatch a few more already-in-flight access units between
	// cancel() being called and it actually observing ctx.Done(), which
	// would otherwise repopulate these fields after an earlier clear here,
	// leaving stale state from the now-closed connection sitting around
	// for whatever opens next (startLocked does not clear them either - a
	// fresh connection's own run loop does that itself, once it actually
	// has a new session open).
	pc.mu.Lock()
	pc.goldenBuffer = nil
	pc.goldenBytes = 0
	pc.lastKeyframe = nil
	pc.mu.Unlock()

	pc.decodeMu.Lock()
	if pc.decoder != nil {
		_ = pc.decoder.Close(context.Background())
		pc.decoder = nil
	}
	pc.decodeMu.Unlock()
}

// forceClose tears down pc unconditionally, for Hub.Close, regardless of
// refCount or any pending warm deadline.
func (pc *printerCamera) forceClose() {
	pc.mu.Lock()
	if pc.closeTimer != nil {
		pc.closeTimer.Stop()
		pc.closeTimer = nil
	}
	pc.refCount = 0
	pc.warmUntil = time.Time{}
	subs := pc.subs
	pc.subs = make(map[int]chan camera.AccessUnit)
	pc.mu.Unlock()
	for id, ch := range subs {
		close(ch)
		_ = id
	}
	pc.teardown()
}

// requestKeyframe forwards to the currently connected upstream session's
// own RequestKeyframe, rate-limited to at most one actual send per
// pliCoalesceInterval (see lastPLIAt's doc comment): a call arriving
// within that window of the last one is treated as already covered by it,
// not as an error. Reports an error only when no upstream session is
// currently connected right now (see Hub.RequestKeyframe's doc comment).
func (pc *printerCamera) requestKeyframe() error {
	pc.mu.Lock()
	sess := pc.current
	now := time.Now()
	if !pc.lastPLIAt.IsZero() && now.Sub(pc.lastPLIAt) < pliCoalesceInterval {
		pc.mu.Unlock()
		return nil
	}
	pc.lastPLIAt = now
	pc.mu.Unlock()

	if sess == nil {
		return fmt.Errorf("daemon: no upstream camera session currently connected for %s", pc.host)
	}
	return sess.RequestKeyframe()
}

// dispatch delivers au to every subscriber (best-effort, matching
// internal/camera.Broadcaster) and maintains the rolling GOP buffer: a
// keyframe access unit starts a fresh buffer; any other access unit is
// appended only while a buffer is already open (an access unit that
// arrives before this connection's first keyframe, or right after a bound-
// exceeded drop, is simply not bufferable yet). Exceeding
// goldenBufferMaxBytes or goldenBufferMaxDuration drops the whole buffer
// rather than trimming it, so the next keyframe starts a clean one.
func (pc *printerCamera) dispatch(au camera.AccessUnit) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.lastMediaAt = time.Now()
	if au.Keyframe {
		kf := au
		pc.lastKeyframe = &kf
		pc.haveEverKeyframe = true
		pc.goldenBuffer = []camera.AccessUnit{au}
		pc.goldenBytes = len(au.Data)
	} else if len(pc.goldenBuffer) > 0 {
		pc.goldenBuffer = append(pc.goldenBuffer, au)
		pc.goldenBytes += len(au.Data)
	}
	// Bound check applies uniformly, whether this access unit just started
	// a fresh buffer (an unrealistically huge single keyframe, in
	// practice) or extended one: either way, exceeding either bound drops
	// the whole buffer rather than trimming it, so the next keyframe
	// starts a clean one.
	if len(pc.goldenBuffer) > 0 {
		first := pc.goldenBuffer[0]
		if pc.goldenBytes > goldenBufferMaxBytes || au.CapturedAt.Sub(first.CapturedAt) > goldenBufferMaxDuration {
			pc.goldenBuffer = nil
			pc.goldenBytes = 0
		}
	}
	for _, ch := range pc.subs {
		select {
		case ch <- au:
		default:
		}
	}
}

// snapshot is Hub.Snapshot's implementation once the connection has been
// acquired. See Hub.Snapshot's doc comment for the overall contract.
func (pc *printerCamera) snapshot(ctx context.Context) (*camera.SnapshotResult, error) {
	ctx, cancel := context.WithTimeout(ctx, hubSnapshotBudget)
	defer cancel()

	ticker := time.NewTicker(hubSnapshotPollInterval)
	defer ticker.Stop()

	for {
		pc.mu.Lock()
		buf := append([]camera.AccessUnit(nil), pc.goldenBuffer...)
		pc.mu.Unlock()

		if len(buf) >= 2 {
			img, err := pc.decodeBuffer(ctx, buf)
			if err == nil {
				b := img.Bounds()
				return &camera.SnapshotResult{Image: img, CapturedAt: time.Now(), Width: b.Dx(), Height: b.Dy()}, nil
			}
			// A decode failure on a buffer that looked usable is treated as
			// an unrecoverable gap (review backlog item 51, item 3): drop it
			// and ask for a fresh keyframe rather than retrying the same bad
			// bytes.
			pc.mu.Lock()
			pc.goldenBuffer = nil
			pc.goldenBytes = 0
			pc.mu.Unlock()
			_ = pc.requestKeyframe()
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("daemon: snapshot: no keyframe received from %s within %s: %w", pc.host, hubSnapshotBudget, ctx.Err())
		case <-ticker.C:
		}
	}
}

// decodeBuffer decodes buf (a keyframe access unit followed by at least
// one more) with this printer's own reused decoder, serialized so at most
// one decode runs at a time per printer.
func (pc *printerCamera) decodeBuffer(ctx context.Context, buf []camera.AccessUnit) (image.Image, error) {
	pc.decodeMu.Lock()
	defer pc.decodeMu.Unlock()

	if pc.decoder == nil {
		dec, err := decode.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("daemon: create decoder for %s: %w", pc.host, err)
		}
		pc.decoder = dec
	}

	var total int
	for _, au := range buf {
		total += len(au.Data)
	}
	data := make([]byte, 0, total)
	for _, au := range buf {
		data = append(data, au.Data...)
	}

	return pc.decoder.DecodeLatest(ctx, data)
}

// run connects to host's camera and pumps access units into dispatch,
// reconnecting with capped exponential backoff whenever the upstream
// session ends while this printerCamera is still wanted (refCount > 0, or
// a warm deadline is still running), until teardown cancels ctx. done is
// closed when run returns, regardless of why.
func (pc *printerCamera) run(ctx context.Context, opener SessionOpener, done chan struct{}) {
	defer close(done)
	backoff := reconnectBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		sess, err := opener.Open(ctx, pc.host)
		if err != nil {
			if !sleepOrDone(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = reconnectBackoffMin
		pc.mu.Lock()
		pc.current = sess
		// A brand new upstream session starts its own PTS/keyframe state
		// over from scratch (and, in production, its own bootstrap PLI
		// retry - camera.Session.Open's OnTrack/retryPLIUntilKeyframe): the
		// rolling buffer and haveEverKeyframe must reset here too, on every
		// reconnect within this run loop, not only at full teardown,
		// otherwise pump's own recovery-retry ticker below would either
		// wrongly fire immediately (haveEverKeyframe still true from the
		// old session) or wrongly stay silent waiting for a buffer that
		// will never come from a session that no longer exists.
		pc.haveEverKeyframe = false
		pc.goldenBuffer = nil
		pc.goldenBytes = 0
		pc.mu.Unlock()
		pc.pump(ctx, sess)
		pc.mu.Lock()
		pc.current = nil
		pc.mu.Unlock()
		_ = sess.Close()
		if ctx.Err() != nil {
			return
		}
		// The upstream ended on its own (printer rebooted the camera, a
		// network blip): wait briefly before reconnecting rather than
		// hammering it.
		if !sleepOrDone(ctx, backoff) {
			return
		}
	}
}

// pump reads access units from sess and dispatches them to every
// subscriber, until ctx is done, sess's channel closes, or no access unit
// arrives for noMediaTimeout (the no-media watchdog, review backlog item
// 45): that last case returns just like the other two, so run's own
// reconnect loop closes sess and reopens a fresh upstream session with its
// existing capped backoff, exactly as if the upstream had ended on its own.
// Subscribers are never touched here (pc.subs is untouched by pump/run),
// so they stay subscribed through the gap: a viewer's stream handler simply
// sees no new access units for a while (its own single hub.RequestKeyframe
// call is a no-op with nothing connected to send it to) and a video
// recording's run sees the reopened session's PTS reset to 0 on its
// next access unit, which its existing "camera reconnected" handling
// already closes the current part and records as the gap's reason in the
// sidecar.
//
// pliTicker is this printerCamera's own automatic keyframe-recovery retry
// (review backlog item 51, item 3): while the rolling buffer is empty and
// this connection has already delivered at least one keyframe in its
// lifetime (haveEverKeyframe), a gap has opened up that camera.Session's
// own bootstrap retry (which only ever concerns a session's very first
// keyframe) will not resend for on its own, so pump nudges it here instead,
// through the same requestKeyframe coalescing every other PLI source
// shares.
func (pc *printerCamera) pump(ctx context.Context, sess upstream) {
	aus := sess.AccessUnits()
	noMedia := time.NewTimer(noMediaTimeout)
	defer noMedia.Stop()
	pliTicker := time.NewTicker(pliCoalesceInterval)
	defer pliTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-noMedia.C:
			if pc.logger != nil {
				pc.logger.Error(pc.host, fmt.Errorf("daemon: no media received for %s, reconnecting", noMediaTimeout))
			}
			return
		case <-pliTicker.C:
			pc.mu.Lock()
			needsKeyframe := pc.haveEverKeyframe && len(pc.goldenBuffer) == 0
			pc.mu.Unlock()
			if needsKeyframe {
				_ = pc.requestKeyframe()
			}
		case au, ok := <-aus:
			if !ok {
				return
			}
			if !noMedia.Stop() {
				<-noMedia.C
			}
			noMedia.Reset(noMediaTimeout)
			pc.dispatch(au)
		}
	}
}

// sleepOrDone waits for d or ctx to end, reporting false if ctx ended first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectBackoffMax {
		return reconnectBackoffMax
	}
	return d
}
