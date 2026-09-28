package camera

import "testing"

func TestRTPTimestampUnwrapperStartsAtZero(t *testing.T) {
	var u rtpTimestampUnwrapper
	// The first raw timestamp is whatever RFC 3550 random value the track
	// started at; the unwrapper always reports 0 for it.
	if got := u.unwrap(0xF0000000); got != 0 {
		t.Fatalf("first unwrap() = %d, want 0", got)
	}
}

func TestRTPTimestampUnwrapperMonotonicIncrease(t *testing.T) {
	var u rtpTimestampUnwrapper
	const step = 90000 / 15 // ~one video frame at 15fps, 90 kHz clock

	start := uint32(1000)
	u.unwrap(start)

	var prev uint64
	for i := 1; i <= 10; i++ {
		got := u.unwrap(start + uint32(i)*step)
		want := uint64(i) * step
		if got != want {
			t.Fatalf("step %d: unwrap() = %d, want %d", i, got, want)
		}
		if got <= prev {
			t.Fatalf("step %d: unwrap() = %d did not increase from %d", i, got, prev)
		}
		prev = got
	}
}

// TestRTPTimestampUnwrapperWraparound confirms the extended timestamp keeps
// increasing correctly when the raw 32-bit RTP timestamp wraps from near
// the uint32 maximum back around to a small value.
func TestRTPTimestampUnwrapperWraparound(t *testing.T) {
	var u rtpTimestampUnwrapper
	const step = 3000

	// raw0 is chosen so that raw0+step overflows uint32 back to exactly 0,
	// i.e. one step below the maximum. It must be a variable, not a
	// constant: Go's constant arithmetic rejects overflow at compile time,
	// but the whole point here is the runtime uint32 wraparound.
	raw0 := ^uint32(0) - step + 1
	if got := u.unwrap(raw0); got != 0 {
		t.Fatalf("first unwrap() = %d, want 0", got)
	}

	raw1 := raw0 + step // wraps: this is Go's well-defined uint32 overflow
	if raw1 != 0 {
		t.Fatalf("test setup error: raw0+step = %d, want 0 (wrapped)", raw1)
	}
	if got := u.unwrap(raw1); got != step {
		t.Fatalf("unwrap() across wraparound = %d, want %d", got, step)
	}

	raw2 := raw1 + step
	if got := u.unwrap(raw2); got != 2*step {
		t.Fatalf("unwrap() after wraparound = %d, want %d", got, 2*step)
	}
}

func TestRTPTimestampUnwrapperResetPerSession(t *testing.T) {
	var a rtpTimestampUnwrapper
	a.unwrap(5000)
	a.unwrap(5000 + 3000)

	// A brand new unwrapper (as a new Session gets) starts fresh at 0
	// regardless of what any other session's raw timestamps were.
	var b rtpTimestampUnwrapper
	if got := b.unwrap(999999); got != 0 {
		t.Fatalf("new unwrapper's first unwrap() = %d, want 0", got)
	}
}
