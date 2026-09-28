package camera

// rtpTimestampUnwrapper turns a track's 32-bit RTP timestamps (which wrap
// around roughly every 13.25 hours at H.264's 90 kHz clock rate, and start
// at a random value per RFC 3550) into a monotonically increasing uint64
// series of 90 kHz ticks starting at 0 for the first timestamp seen. A
// Session owns one of these, so its zero value is exactly "no timestamp
// seen yet for this session", which is what "reset the unwrap base on a new
// session" means: each new Session gets a fresh, zeroed unwrapper.
//
// Not safe for concurrent use; Session only ever calls unwrap from its
// single readTrack goroutine.
type rtpTimestampUnwrapper struct {
	have     bool
	lastRaw  uint32
	extended int64
}

// unwrap returns the extended, session-relative presentation timestamp for
// raw. It handles both directions of 32-bit wraparound (and ordinary minor
// RTP timestamp jitter/reordering) by taking the difference between
// consecutive raw timestamps as a signed 32-bit value: for real-time video,
// consecutive access units are never more than 2^31 ticks apart, so
// interpreting the modular difference as signed always recovers the correct
// forward (or, rarely, backward) step, including across a wrap from
// 0xFFFFFFFF back to 0.
func (u *rtpTimestampUnwrapper) unwrap(raw uint32) uint64 {
	if !u.have {
		u.have = true
		u.lastRaw = raw
		u.extended = 0
		return 0
	}

	diff := int32(raw - u.lastRaw)
	u.lastRaw = raw
	u.extended += int64(diff)
	if u.extended < 0 {
		// Only reachable from an out-of-order first packet or similar
		// pathological input; clamp rather than hand back a timestamp a
		// muxer would reject.
		u.extended = 0
	}
	return uint64(u.extended)
}
