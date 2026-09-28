package camera

import (
	"sync"
	"sync/atomic"
)

// subscriberBufferSize is how many access units a slow subscriber can fall
// behind by before Broadcaster starts dropping for it.
const subscriberBufferSize = 16

// Broadcaster fans out one Session's access units to any number of
// subscribers (browser viewers, a recorder) without letting a slow
// consumer block the others or the upstream Session. A new subscriber
// immediately receives the most recent keyframe, if one has been seen, so
// it can start decoding right away instead of waiting for the next one.
//
// Broadcaster does not own the Session's lifecycle: creating a Broadcaster
// does not affect the Session, and closing a Broadcaster does not close the
// Session. The caller is responsible for closing the Session separately
// (typically after every Broadcaster over it has been closed).
type Broadcaster struct {
	session *Session

	mu           sync.Mutex
	subs         map[int]chan AccessUnit
	dropped      map[int]*uint64
	nextID       int
	lastKeyframe *AccessUnit

	stop     chan struct{}
	stopOnce sync.Once
	pumpDone chan struct{}
}

// NewBroadcaster starts fanning out session's access units. Call Close when
// done.
func NewBroadcaster(session *Session) *Broadcaster {
	b := &Broadcaster{
		session:  session,
		subs:     make(map[int]chan AccessUnit),
		dropped:  make(map[int]*uint64),
		stop:     make(chan struct{}),
		pumpDone: make(chan struct{}),
	}
	go b.pump()
	return b
}

// pump forwards access units from the session to every current subscriber
// until told to stop or the session's access unit stream ends.
func (b *Broadcaster) pump() {
	defer close(b.pumpDone)

	aus := b.session.AccessUnits()
	for {
		select {
		case <-b.stop:
			return
		case au, ok := <-aus:
			if !ok {
				return
			}
			b.dispatch(au)
		}
	}
}

// dispatch delivers au to every subscriber without blocking: a subscriber
// whose buffer is full has the unit dropped for it and its drop counter
// incremented, but every other subscriber still receives it.
func (b *Broadcaster) dispatch(au AccessUnit) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if au.Keyframe {
		kf := au
		b.lastKeyframe = &kf
	}

	for id, ch := range b.subs {
		select {
		case ch <- au:
		default:
			atomic.AddUint64(b.dropped[id], 1)
		}
	}
}

// Subscribe registers a new subscriber and returns its id and the channel
// it will receive access units on. If a keyframe has already been seen, it
// is enqueued for this subscriber immediately so a new viewer does not have
// to wait for the next one. The returned channel is closed by Unsubscribe
// or Close.
func (b *Broadcaster) Subscribe() (id int, ch <-chan AccessUnit) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id = b.nextID
	b.nextID++

	c := make(chan AccessUnit, subscriberBufferSize)
	b.subs[id] = c
	var d uint64
	b.dropped[id] = &d

	if b.lastKeyframe != nil {
		c <- *b.lastKeyframe
	}

	return id, c
}

// Unsubscribe removes a subscriber and closes its channel. Safe to call
// with an id that is already gone (a no-op).
func (b *Broadcaster) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ch, ok := b.subs[id]; ok {
		close(ch)
		delete(b.subs, id)
		delete(b.dropped, id)
	}
}

// Dropped returns how many access units have been dropped for the
// subscriber with the given id, or 0 if it is not (or no longer) a
// subscriber.
func (b *Broadcaster) Dropped(id int) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	if d, ok := b.dropped[id]; ok {
		return atomic.LoadUint64(d)
	}
	return 0
}

// Close stops fanning out and closes every subscriber's channel. It does
// not close the underlying Session (see the Broadcaster doc comment). Safe
// to call more than once.
func (b *Broadcaster) Close() {
	b.stopOnce.Do(func() { close(b.stop) })
	<-b.pumpDone

	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		close(ch)
		delete(b.subs, id)
		delete(b.dropped, id)
	}
}
