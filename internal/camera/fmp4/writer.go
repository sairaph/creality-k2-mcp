package fmp4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	h264c "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mfmp4 "github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// TimeScale is the media timescale used for the single video track this
// package writes, chosen to match the camera's RTP clock (90 kHz, see
// dev_docs/plan-v0.1.0.md) exactly. Every access unit timestamp given to
// WriteAccessUnit is therefore used as a track tick count directly, with no
// rate conversion and so no rounding error between RTP time and MP4 time.
const TimeScale = 90000

// videoTrackID is the only track this package ever writes.
const videoTrackID = 1

// DefaultFragmentDuration is used when Config.FragmentDuration is zero.
const DefaultFragmentDuration = 1 * time.Second

// defaultNominalDuration is the fallback sample duration for the very last
// access unit written to a Writer (used only by Close, since that sample
// has no following access unit to measure a real duration from). 90000/15
// matches the camera's approximate 15 fps floor (dev_docs/plan-v0.1.0.md);
// guessing slightly long here only affects how long the last frame of a
// recording appears to hold on screen, which is a cosmetic concern.
const defaultNominalDuration = TimeScale / 15

// DefaultMaxKeyframeWait is used when Config.MaxKeyframeWait is zero.
const DefaultMaxKeyframeWait = 10 * time.Second

// DefaultMaxBufferedBytes is used when Config.MaxBufferedBytes is zero.
const DefaultMaxBufferedBytes = 32 * 1024 * 1024

// ErrParametersChanged is returned by WriteAccessUnit when a keyframe's
// SPS or PPS is present and differs from the SPS/PPS embedded in this
// Writer's init segment, i.e. the sps/pps passed to NewWriter or
// NewRecordingWriter. A Writer's init segment (and its avc1/avcC sample
// entry) is written once, up front, and never rewritten, so there is no
// way to describe new parameters to a player's already-open MSE
// SourceBuffer or to a recording file's already-written moov box. The
// access unit that carried the changed parameters is not written when
// this error is returned. The caller must Close this Writer and open a
// new one (NewWriter or NewRecordingWriter) with the new SPS/PPS, which
// produces a new init segment for a player to append, or a new recording
// file to start.
var ErrParametersChanged = errors.New("fmp4: keyframe SPS/PPS differs from the init segment's SPS/PPS")

// ErrNoKeyframe is returned by WriteAccessUnit when no keyframe has
// arrived to close the current fragment within Config.MaxKeyframeWait
// ticks, or within Config.MaxBufferedBytes of sample payload buffered
// since the fragment's first sample, whichever limit is reached first.
// This bounds how much memory a Writer buffers while waiting for a
// keyframe: without it, a lost keyframe (e.g. a dropped PLI on the WebRTC
// receive path) would let the fragment buffer grow without limit for as
// long as the stream keeps sending non-keyframe access units.
//
// When this error is returned, every sample buffered since the last
// flushed fragment is dropped and the Writer goes back to requiring a
// keyframe before it accepts another access unit, exactly like a freshly
// created Writer (see WriteAccessUnit's doc comment on its first access
// unit). Non-keyframe access units written before that keyframe arrives
// are rejected. The caller is expected to request a keyframe from the
// encoder (a PLI, on this project's WebRTC receive path) and keep writing
// to the same Writer once one arrives; the Writer's init segment is
// unaffected, only the undelivered fragment's samples are lost. The
// recovering keyframe also rebases streamStartPTS, the same way a fresh
// Writer's own first sample does (see that field's doc comment): the dead
// time spent waiting for the keyframe is not counted as elapsed stream time
// in any later fragment's BaseTime.
var ErrNoKeyframe = errors.New("fmp4: no keyframe within the configured maximum wait; buffered samples dropped")

// Config configures a Writer.
type Config struct {
	// FragmentDuration is the minimum wall-clock duration a fragment must
	// span before the next keyframe access unit closes it. Fragments are
	// only ever closed at a keyframe, never mid-GOP, so a GOP longer than
	// FragmentDuration produces one fragment per GOP, and a GOP shorter
	// than FragmentDuration is merged with the following GOP(s) until the
	// threshold is reached. Zero uses DefaultFragmentDuration.
	FragmentDuration time.Duration

	// MaxKeyframeWait bounds how long WriteAccessUnit will buffer access
	// units while waiting for a keyframe to close the current fragment
	// (see ErrNoKeyframe). Zero uses DefaultMaxKeyframeWait.
	MaxKeyframeWait time.Duration

	// MaxBufferedBytes bounds how many bytes of sample payload
	// WriteAccessUnit will buffer while waiting for a keyframe (see
	// ErrNoKeyframe). Zero uses DefaultMaxBufferedBytes.
	MaxBufferedBytes int
}

// syncer is implemented by *os.File (and anything else with fsync
// semantics). Writer calls Sync after every chunk it writes when its sink
// implements this, which is what makes NewRecordingWriter's file playable
// up to the last complete fragment after the process is killed (see the
// Close doc comment, and dev_docs/plan-v0.1.0.md decision 11 / review
// finding F9).
type syncer interface {
	Sync() error
}

// flusher matches net/http.Flusher. Writer calls Flush after every chunk it
// writes when its sink implements this, so a browser MSE player fed by
// NewWriter(httpResponseWriter, ...) receives each fragment promptly
// instead of it sitting in a buffer.
type flusher interface {
	Flush()
}

// Writer muxes one H.264 video track into fragmented MP4, writing an init
// segment once (during NewWriter) and then one moof+mdat fragment per call
// to WriteAccessUnit that crosses a fragment boundary (see Config). It is
// the single muxing core behind both of this package's consumers:
// NewWriter (streaming to any io.Writer, e.g. a browser MSE player served
// over HTTP) and NewRecordingWriter (a file kept playable up to the last
// complete fragment if the process is killed). See the package doc comment
// for the underlying box-writing library this type is built on.
//
// A Writer is not safe for concurrent use.
type Writer struct {
	sink io.Writer

	// sps and pps are the parameters embedded in the init segment already
	// written by NewWriter, kept so WriteAccessUnit can detect a keyframe
	// that carries different ones (see ErrParametersChanged).
	sps, pps []byte

	fragmentTicks        uint64
	maxKeyframeWaitTicks uint64
	maxBufferedBytes     int

	started      bool
	closed       bool
	needKeyframe bool
	// streamStartPTS anchors every fragment's BaseTime (tfdt/
	// baseMediaDecodeTime, see flushFragment): BaseTime is always
	// fragStartPTS - streamStartPTS, never fragStartPTS itself, and never
	// relative to 0 on whatever clock pts itself came from - which matters
	// because pts, in this project, is a camera.Session's own RTP-derived
	// 90 kHz clock (internal/camera/pts.go), and internal/daemon's Hub fans
	// that same clock out to every viewer stream and recording part as a
	// shared subscription: a consumer that starts well after the session
	// opened (a late-joining viewer, or a recording begun while streams have
	// already been running) would otherwise see its very first fragment
	// start at some large, arbitrary tfdt instead of 0. This is
	// dev_docs/review-backlog.md item 50's normalisation, verified against
	// ffprobe by TestWriterNormalizesBaseTimeToZero: it already held before
	// that item (every Writer already anchors to its own first sample), and
	// that test exists to keep it that way.
	//
	// streamStartPTS is set once, in WriteAccessUnit's !w.started branch,
	// and rebased again every time WriteAccessUnit recovers from
	// ErrNoKeyframe (the needKeyframe branch): without that second rebase,
	// a fragment flushed after a recovery would compute BaseTime relative to
	// the Writer's original construction, so the "dead" time spent dropped-
	// and-waiting for the missing keyframe would count as real elapsed
	// stream time even though nothing was recorded during it - inflating
	// ffprobe's reported start_time/duration for that fragment by the whole
	// gap (dev_docs/review-backlog.md item 51 review, part 1's
	// writer.go:331-342 finding; TestWriterRebasesStreamStartAfterRecovery
	// locks this in). Rebasing here treats a post-recovery run exactly like
	// a fresh Writer's own first sample, which is the same semantics
	// TestWriterNormalizesBaseTimeToZero already locks in for the original
	// late-join case.
	streamStartPTS uint64
	fragStartPTS   uint64
	lastDuration   uint32
	pendingPTS     uint64
	pendingSync    bool
	pendingPayload []byte
	fragSamples    []*mfmp4.Sample
	bufferedBytes  int
	seq            uint32
}

// NewWriter creates a Writer that writes the init segment and every
// fragment to sink, in that order, as required by a browser's Media Source
// Extensions SourceBuffer (the init segment must be appended before any
// fragment). sps and pps are the stream's raw SPS/PPS NAL units: each
// includes its own one-byte NAL header (0x67/0x68) but no Annex-B start
// code, e.g. the SPS/PPS as split out of an access unit by the h264
// package used internally by WriteAccessUnit, or by the camera receive
// path that hands access units to this Writer.
//
// sink is never closed by this Writer unless it implements io.Closer and
// Close is later called on the Writer (see Close); a plain io.Writer such
// as an http.ResponseWriter is therefore left alone.
func NewWriter(sink io.Writer, sps, pps []byte, cfg Config) (*Writer, error) {
	if sink == nil {
		return nil, fmt.Errorf("fmp4: sink is nil")
	}
	if len(sps) == 0 {
		return nil, fmt.Errorf("fmp4: sps is empty")
	}
	if len(pps) == 0 {
		return nil, fmt.Errorf("fmp4: pps is empty")
	}

	fragmentDuration := cfg.FragmentDuration
	if fragmentDuration <= 0 {
		fragmentDuration = DefaultFragmentDuration
	}
	maxKeyframeWait := cfg.MaxKeyframeWait
	if maxKeyframeWait <= 0 {
		maxKeyframeWait = DefaultMaxKeyframeWait
	}
	maxBufferedBytes := cfg.MaxBufferedBytes
	if maxBufferedBytes <= 0 {
		maxBufferedBytes = DefaultMaxBufferedBytes
	}

	w := &Writer{
		sink:                 sink,
		sps:                  append([]byte(nil), sps...),
		pps:                  append([]byte(nil), pps...),
		fragmentTicks:        uint64(fragmentDuration.Nanoseconds()) * TimeScale / uint64(time.Second.Nanoseconds()),
		maxKeyframeWaitTicks: uint64(maxKeyframeWait.Nanoseconds()) * TimeScale / uint64(time.Second.Nanoseconds()),
		maxBufferedBytes:     maxBufferedBytes,
	}

	init := &mfmp4.Init{
		Tracks: []*mfmp4.InitTrack{
			{
				ID:        videoTrackID,
				TimeScale: TimeScale,
				Codec: &codecs.H264{
					SPS: append([]byte(nil), sps...),
					PPS: append([]byte(nil), pps...),
				},
			},
		},
	}

	var buf seekablebuffer.Buffer
	if err := init.Marshal(&buf); err != nil {
		return nil, fmt.Errorf("fmp4: marshal init segment: %w", err)
	}
	if err := w.writeChunk(buf.Bytes()); err != nil {
		return nil, fmt.Errorf("fmp4: write init segment: %w", err)
	}

	return w, nil
}

// NewRecordingWriter creates the file at path (truncating it if it already
// exists) and returns a Writer that records to it. The file is fsynced
// after the init segment and after every fragment (see Writer's syncer
// doc comment), and Close closes the file, so a recording made this way
// stays playable up to the last complete fragment even if the process is
// killed without calling Close (dev_docs/plan-v0.1.0.md decision 11, the
// T11c hard-kill requirement).
func NewRecordingWriter(path string, sps, pps []byte, cfg Config) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fmp4: create recording file: %w", err)
	}

	w, err := NewWriter(f, sps, pps, cfg)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

// WriteAccessUnit appends one H.264 access unit (Annex-B, i.e. its NAL
// units each prefixed with a 3- or 4-byte start code) at pts, a decode
// timestamp on the camera's 90 kHz RTP clock (matching TimeScale exactly,
// see its doc comment). pts must strictly increase from call to call, and
// samples must be given in decode order with pts equal to each access
// unit's own decode timestamp; see the package doc comment's
// no-reordering assumption for exactly what is and is not detected if
// that does not hold.
//
// The first access unit ever written to a Writer must be a keyframe (an
// IDR slice, optionally preceded by SPS/PPS/SEI in the same access unit):
// a fragment can only begin at a keyframe, and the first access unit
// always begins the first fragment. A camera stream that starts mid-GOP
// (as WebRTC depacketization typically does before a full GOP has
// arrived) should be trimmed to its first keyframe before being handed to
// a Writer; see internal/camera/decode's DecodeKeyframe doc comment for
// the same situation on the snapshot path. The same requirement applies
// again, on the fly, whenever WriteAccessUnit returns ErrNoKeyframe (see
// its doc comment): the next access unit accepted after that error must
// also be a keyframe.
//
// If a keyframe access unit carries an SPS and/or PPS NAL unit that
// differs from the SPS/PPS embedded in this Writer's init segment,
// WriteAccessUnit returns ErrParametersChanged and does not write the
// sample; see that error's doc comment for what the caller must do.
//
// If no keyframe arrives to close the current fragment within
// Config.MaxKeyframeWait or Config.MaxBufferedBytes of buffered sample
// data, whichever comes first, WriteAccessUnit returns ErrNoKeyframe and
// drops the buffered samples; see that error's doc comment.
func (w *Writer) WriteAccessUnit(pts uint64, annexB []byte) error {
	if w.closed {
		return fmt.Errorf("fmp4: writer is closed")
	}

	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(annexB); err != nil {
		return fmt.Errorf("fmp4: parse access unit: %w", err)
	}
	isKeyframe := h264c.IsRandomAccess(nalus)

	if isKeyframe {
		if keySPS, keyPPS := keyframeParameterSets(nalus); (keySPS != nil && !bytes.Equal(keySPS, w.sps)) ||
			(keyPPS != nil && !bytes.Equal(keyPPS, w.pps)) {
			return fmt.Errorf("fmp4: %w", ErrParametersChanged)
		}
	}

	payload, err := h264c.AVCC(nalus).Marshal()
	if err != nil {
		return fmt.Errorf("fmp4: convert access unit to AVCC: %w", err)
	}

	if !w.started {
		if !isKeyframe {
			return fmt.Errorf("fmp4: first access unit written to a Writer must be a keyframe")
		}
		w.started = true
		w.streamStartPTS = pts
		w.fragStartPTS = pts
		w.pendingPTS = pts
		w.pendingSync = true
		w.pendingPayload = payload
		w.bufferedBytes = len(payload)
		return nil
	}

	if pts <= w.pendingPTS {
		return fmt.Errorf("fmp4: access unit timestamp %d does not advance past previous timestamp %d", pts, w.pendingPTS)
	}

	if w.needKeyframe {
		if !isKeyframe {
			w.pendingPTS = pts
			return fmt.Errorf("fmp4: waiting for a keyframe after %w", ErrNoKeyframe)
		}
		w.needKeyframe = false
		// Rebase streamStartPTS here too, not just fragStartPTS: see the
		// streamStartPTS doc comment for why, and
		// TestWriterRebasesStreamStartAfterRecovery for the ffprobe-level
		// symptom this prevents.
		w.streamStartPTS = pts
		w.fragStartPTS = pts
		w.pendingPTS = pts
		w.pendingSync = true
		w.pendingPayload = payload
		w.bufferedBytes = len(payload)
		return nil
	}

	// The just-arrived access unit finally tells us how long the pending
	// one (the previous call's access unit) lasted, so it can now be
	// finalized as a sample with a known duration and added to the
	// fragment currently being built.
	duration := uint32(pts - w.pendingPTS)
	w.lastDuration = duration
	w.fragSamples = append(w.fragSamples, &mfmp4.Sample{
		Duration:        duration,
		IsNonSyncSample: !w.pendingSync,
		Payload:         w.pendingPayload,
	})

	if isKeyframe && pts-w.fragStartPTS >= w.fragmentTicks {
		if err := w.flushFragment(); err != nil {
			return err
		}
		w.fragStartPTS = pts
		w.bufferedBytes = 0
	}

	if !isKeyframe {
		elapsedTicks := pts - w.fragStartPTS
		if elapsedTicks >= w.maxKeyframeWaitTicks || w.bufferedBytes+len(payload) > w.maxBufferedBytes {
			w.fragSamples = nil
			w.bufferedBytes = 0
			w.needKeyframe = true
			w.pendingPTS = pts
			w.pendingPayload = nil
			return ErrNoKeyframe
		}
	}

	w.bufferedBytes += len(payload)
	w.pendingPTS = pts
	w.pendingSync = isKeyframe
	w.pendingPayload = payload
	return nil
}

// keyframeParameterSets returns the SPS and PPS NAL units found among
// nalus (nil for either one not present in this particular access unit),
// used by WriteAccessUnit to compare a keyframe's parameters against the
// ones already embedded in the init segment (see ErrParametersChanged).
func keyframeParameterSets(nalus h264c.AnnexB) (sps, pps []byte) {
	for _, n := range nalus {
		if len(n) == 0 {
			continue
		}
		switch h264c.NALUType(n[0] & 0x1f) {
		case h264c.NALUTypeSPS:
			sps = n
		case h264c.NALUTypePPS:
			pps = n
		}
	}
	return sps, pps
}

// flushFragment marshals and writes the samples accumulated since the last
// flush (or since the first access unit) as one moof+mdat fragment, then
// clears them. It does not touch pendingPTS/pendingSync/pendingPayload,
// which belong to the sample that starts the next fragment.
func (w *Writer) flushFragment() error {
	if len(w.fragSamples) == 0 {
		return nil
	}

	w.seq++
	part := &mfmp4.Part{
		SequenceNumber: w.seq,
		Tracks: []*mfmp4.PartTrack{
			{
				ID:       videoTrackID,
				BaseTime: w.fragStartPTS - w.streamStartPTS,
				Samples:  w.fragSamples,
			},
		},
	}

	var buf seekablebuffer.Buffer
	if err := part.Marshal(&buf); err != nil {
		return fmt.Errorf("fmp4: marshal fragment %d: %w", w.seq, err)
	}
	if err := w.writeChunk(buf.Bytes()); err != nil {
		return fmt.Errorf("fmp4: write fragment %d: %w", w.seq, err)
	}

	w.fragSamples = nil
	return nil
}

// writeChunk writes b (a complete, self-contained box structure: either
// the whole init segment or one whole moof+mdat fragment) to the sink in a
// single Write call, then flushes and fsyncs it if it supports either (see
// the flusher and syncer doc comments). Every on-disk or on-the-wire chunk
// this package ever produces is written this way, which is what lets
// NewRecordingWriter promise a file that is playable up to the last chunk
// it actually got to write.
func (w *Writer) writeChunk(b []byte) error {
	if _, err := w.sink.Write(b); err != nil {
		return err
	}
	if f, ok := w.sink.(flusher); ok {
		f.Flush()
	}
	if s, ok := w.sink.(syncer); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("sync: %w", err)
		}
	}
	return nil
}

// Close flushes the last, still-buffered fragment (using
// defaultNominalDuration for its final sample's duration, since no later
// access unit ever arrives to measure it from), then closes the sink if it
// implements io.Closer. Close is safe to call more than once; only the
// first call does anything.
//
// If the Writer is currently waiting for a keyframe after ErrNoKeyframe
// dropped its buffered samples (see that error's doc comment), there is
// nothing pending to flush: Close only closes the sink in that case.
//
// A Writer whose process is killed before Close is called simply never
// flushes this last, still-buffered fragment: everything written so far
// was already fsynced by WriteAccessUnit's calls to flushFragment (see the
// syncer doc comment), so the sink, e.g. a recording file from
// NewRecordingWriter, stays valid and playable up to the last fragment
// that did get flushed. This is the crash-safety property
// dev_docs/plan-v0.1.0.md decision 11 (review finding F9) requires of
// fragmented MP4 recordings.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	var ferr error
	if w.started && !w.needKeyframe {
		duration := w.lastDuration
		if duration == 0 {
			duration = defaultNominalDuration
		}
		w.fragSamples = append(w.fragSamples, &mfmp4.Sample{
			Duration:        duration,
			IsNonSyncSample: !w.pendingSync,
			Payload:         w.pendingPayload,
		})
		if err := w.flushFragment(); err != nil {
			ferr = err
		}
	}

	if c, ok := w.sink.(io.Closer); ok {
		if err := c.Close(); err != nil && ferr == nil {
			ferr = fmt.Errorf("fmp4: close sink: %w", err)
		}
	}

	return ferr
}
