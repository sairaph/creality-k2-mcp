// Package camera talks WebRTC to a Creality K2's onboard camera and turns
// its H.264 video into access units the rest of the server can use: a
// single decoded snapshot, a live fan-out for browser viewers, or a
// recording. See references/analysis/04-creality-ws-camera.md section 3 for
// the protocol this package implements against.
package camera

import (
	"context"
	"time"
)

// AccessUnit is one H.264 access unit (typically one video frame) as an
// Annex-B byte stream: one or more NAL units, each prefixed with a start
// code. When Keyframe is true, Data is self-contained: it starts with an
// SPS and PPS NAL unit followed by the IDR slice, even if the printer sent
// the SPS/PPS earlier and out of band from this particular IDR (see
// Session for how that is arranged).
type AccessUnit struct {
	Data     []byte
	Keyframe bool

	// PTS is this access unit's presentation timestamp in 90 kHz ticks
	// (the H.264 RTP clock rate), unwrapped from the track's 32-bit RTP
	// timestamp into a monotonically increasing value and reset to 0 at
	// the start of each Session (see Session for the unwrap logic). This
	// is the printer's own camera clock, not wall-clock time, and is what
	// a muxer (e.g. internal/camera/fmp4) should use for a sample's
	// presentation time.
	PTS uint64

	// CapturedAt is the wall-clock time this access unit was assembled,
	// for display purposes only (it is not derived from the camera's
	// clock and is not suitable as a muxer timestamp).
	CapturedAt time.Time
}

// CodecInfo describes the video codec of a Session's track. ProfileLevelID
// comes from the SDP answer's fmtp line (e.g. "42e01f"). Width and Height
// are zero until an SPS NAL unit has been seen and successfully parsed.
type CodecInfo struct {
	ProfileLevelID string
	Width          int
	Height         int
}

// NoKeyframeText is the fixed part of NoKeyframeError's message. The daemon's
// errors cross its socket as plain text, so a client recognises this failure
// by it.
const NoKeyframeText = "the camera stream did not deliver a complete keyframe"

// NoKeyframeError is a snapshot that ran out of time before the camera
// stream delivered a complete keyframe. Wait is how long was waited. It
// matches context.DeadlineExceeded under errors.Is, because it is that
// deadline, expired.
type NoKeyframeError struct {
	Wait time.Duration
}

func (e *NoKeyframeError) Error() string {
	if e.Wait <= 0 {
		return NoKeyframeText
	}
	return NoKeyframeText + " within " + e.Wait.Round(time.Second).String()
}

// Is reports whether target is context.DeadlineExceeded.
func (e *NoKeyframeError) Is(target error) bool {
	return target == context.DeadlineExceeded
}

// NoKeyframeCause is what is known about the cause of a NoKeyframeError, for
// every message that reports one: the field cause (a keyframe larger than the
// receive window) is fixed, and no other cause has been observed.
const NoKeyframeCause = "the cause is not known to this server"
