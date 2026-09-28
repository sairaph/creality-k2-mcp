// Package fmp4 muxes an H.264 Annex-B access unit stream (as produced by
// this project's WebRTC receive path, see internal/camera and
// dev_docs/plan-v0.1.0.md decision 11) into fragmented MP4: one init
// segment (ftyp + moov, with an avc1/avcC sample entry built from the
// stream's SPS/PPS) followed by a series of media fragments (moof + mdat),
// one per GOP or per Config.FragmentDuration, whichever is reached first,
// always starting at a keyframe.
//
// Library choice: box construction (ftyp/moov/avcC on the init segment,
// moof/mdat on every fragment, including the two-pass trun data-offset
// patch-up a fragment's trun box needs) is delegated to
// github.com/bluenviron/mediacommon/v2's pkg/formats/fmp4 and
// pkg/codecs/h264 packages rather than written by hand here. That library
// is maintained by the mediamtx project (a widely deployed Go media
// server) specifically for this exact use case, muxing a live H.264
// Annex-B stream into fMP4 for both HLS-style segment output and MSE
// playback, so it already handles the fiddly parts correctly: emulation
// prevention bytes in the SPS/PPS copied into avcC, the trun data-offset
// back-patch (Part.Marshal writes moof then mdat, then seeks back into the
// already-written trun box to fill in the offset, which is why fragments
// are marshaled into an in-memory io.WriteSeeker, see
// pkg/formats/fmp4/seekablebuffer, before being copied to this package's
// sink), and box structure that ffmpeg and browsers actually accept. It is
// pure Go (no cgo, so CGO_ENABLED=0 release builds keep working), under
// active maintenance (github.com/bluenviron/mediacommon/v2, releases
// through 2026), and already a transitive concern of this project's
// problem domain (camera streaming), so depending on it here is a much
// smaller risk than re-deriving ISO/IEC 14496-12 box encoding by hand. The
// original (v1) module path is deprecated upstream; this package uses the
// v2 module, which is where the fmp4 and h264 packages actually live.
//
// Everything above the box level is this package's own code: GOP- and
// duration-based fragmentation policy, decode-timestamp bookkeeping from
// the camera's 90 kHz RTP clock, Annex-B-to-AVCC conversion per access
// unit, the streaming-vs-recording sink policy (flush/fsync semantics,
// Close), and the MSE codec-string helper.
//
// No-reordering assumption: WriteAccessUnit takes a single timestamp (pts)
// per access unit and uses it directly as both the sample's decode and
// presentation time. This is correct only for a stream written in decode
// order with pts equal to each access unit's own decode timestamp, which is
// what this project's K2 camera path always produces: it is a live,
// low-latency WebRTC feed (dev_docs/plan-v0.1.0.md) whose encoder is not
// expected to emit B-frames, so there is no presentation-order reordering
// to undo and no reordering delay to buffer before a sample's final
// duration is known. WriteAccessUnit only detects the cheap symptom of a
// stream that violates this assumption: a pts that fails to strictly
// increase from call to call (see its doc comment) is rejected outright.
// It cannot detect, and does not try to detect, a stream that is
// internally reordered but still happens to present strictly increasing
// timestamps to this package, for example B-frames whose presentation
// order runs ahead of decode order while both individually keep
// increasing call to call; a caller that hands such a stream to
// WriteAccessUnit gets a structurally valid but visually wrong (frames
// played back out of order) fMP4 output, not an error.
//
// WriteAccessUnit also bounds two live-stream failure modes with typed,
// exported errors rather than doing the wrong thing silently:
// ErrParametersChanged, returned when a keyframe's SPS/PPS differs from
// the SPS/PPS already embedded in this Writer's init segment (the caller
// must Close this Writer and open a new one, with a new init segment, for
// the new parameters), and ErrNoKeyframe, returned when no keyframe
// arrives to close the current fragment within Config.MaxKeyframeWait or
// Config.MaxBufferedBytes of buffered sample payload, whichever comes
// first (the caller is expected to request a keyframe, e.g. a PLI on the
// WebRTC receive path, and keep writing to the same Writer once one
// arrives). See both errors' doc comments for the exact behavior.
package fmp4
