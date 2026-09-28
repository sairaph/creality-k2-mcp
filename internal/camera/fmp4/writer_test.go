package fmp4

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	h264c "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mfmp4 "github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

// ffprobePath is where dev_docs/plan-v0.1.0.md says ffmpeg/ffprobe is
// installed on this machine. Tests that use it skip instead of failing when
// it is not there, per this task's instructions: ffmpeg is a verification
// aid, never a hard dependency of this package or its test suite.
const ffprobePath = `C:\ffmpeg\ffmpeg-7.1.1-essentials_build\bin\ffprobe.exe`

// sampleClipPath is the shared H.264 fixture used by internal/camera/decode
// (see that package's tests) and reused here rather than adding a second
// binary fixture. It is Annex-B, 1280x720, Baseline-compatible
// (profile-level-id 42e01f per this task), and starts mid-GOP: some leading
// access units appear before the first SPS/PPS/IDR (see splitAccessUnits).
const sampleClipPath = `../decode/testdata/camera_sample_clip.h264`

// splitAccessUnits groups annexB's NAL units (each already including its
// own Annex-B start code) into access units. A new access unit starts at
// every NAL unit that follows a VCL NAL unit (type 1 or 5) already
// collected into the current one: this stream carries at most one slice
// per picture, so seeing a second VCL NAL unit (or any NAL unit at all
// after one) means the previous picture's access unit is complete. This
// mirrors how an encoder emits SPS+PPS+IDR together as one access unit
// followed by single-NAL access units for the following P slices, which is
// exactly the shape of sampleClipPath (confirmed by inspection: NAL types
// SPS/PPS/IDR/non-IDR = 7/8/5/1, IDR always immediately preceded by SPS and
// PPS in the same access unit).
func splitAccessUnits(annexB []byte) [][]byte {
	var starts []int
	for i := 0; i+2 < len(annexB); i++ {
		if annexB[i] == 0 && annexB[i+1] == 0 && annexB[i+2] == 1 {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return nil
	}

	type nal struct {
		begin, end int
	}
	nals := make([]nal, 0, len(starts))
	for i, s := range starts {
		end := len(annexB)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		begin := s
		if begin > 0 && annexB[begin-1] == 0 {
			begin--
		}
		nals = append(nals, nal{begin, end})
	}

	naluType := func(n nal) int {
		i := n.begin + 3
		if annexB[n.begin+2] == 0 {
			i = n.begin + 4
		}
		return int(annexB[i] & 0x1f)
	}
	isVCL := func(t int) bool { return t == 1 || t == 5 }

	var aus [][]byte
	var curStart int
	curHasVCL := false
	curOpen := false
	for _, n := range nals {
		if curHasVCL {
			aus = append(aus, annexB[curStart:n.begin])
			curOpen = false
			curHasVCL = false
		}
		if !curOpen {
			curStart = n.begin
			curOpen = true
		}
		if isVCL(naluType(n)) {
			curHasVCL = true
		}
	}
	if curOpen {
		aus = append(aus, annexB[curStart:])
	}
	return aus
}

// nalType returns the H.264 NAL unit type of the first NAL unit found in an
// Annex-B buffer, tolerating either start code length.
func nalType(annexB []byte) int {
	i := 3
	if len(annexB) >= 4 && annexB[2] == 0 {
		i = 4
	}
	return int(annexB[i] & 0x1f)
}

// firstKeyframeIndex returns the index of the first access unit in aus that
// contains an IDR NAL unit.
func firstKeyframeIndex(t *testing.T, aus [][]byte) int {
	t.Helper()
	for i, au := range aus {
		var nalus h264c.AnnexB
		if err := nalus.Unmarshal(au); err != nil {
			t.Fatalf("splitAccessUnits produced an access unit AnnexB.Unmarshal rejects: %v", err)
		}
		if h264c.IsRandomAccess(nalus) {
			return i
		}
	}
	t.Fatalf("sample clip contains no keyframe access unit")
	return -1
}

// extractParameterSets returns the SPS and PPS raw NAL units (start code
// stripped) found in au.
func extractParameterSets(t *testing.T, au []byte) (sps, pps []byte) {
	t.Helper()
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(au); err != nil {
		t.Fatalf("AnnexB.Unmarshal: %v", err)
	}
	for _, n := range nalus {
		switch h264c.NALUType(n[0] & 0x1f) {
		case h264c.NALUTypeSPS:
			sps = n
		case h264c.NALUTypePPS:
			pps = n
		}
	}
	if sps == nil || pps == nil {
		t.Fatalf("keyframe access unit is missing SPS or PPS")
	}
	return sps, pps
}

// clipFixture holds the sample clip's access units (trimmed to start at the
// first keyframe, as WriteAccessUnit requires) plus its SPS/PPS, shared by
// every test below.
type clipFixture struct {
	sps, pps []byte
	aus      [][]byte // starts at the first keyframe access unit
}

func loadClipFixture(t *testing.T) clipFixture {
	t.Helper()
	data, err := os.ReadFile(sampleClipPath)
	if err != nil {
		t.Fatalf("read %s: %v", sampleClipPath, err)
	}

	all := splitAccessUnits(data)
	if len(all) < 2 {
		t.Fatalf("sample clip produced only %d access units", len(all))
	}

	first := firstKeyframeIndex(t, all)
	aus := all[first:]
	sps, pps := extractParameterSets(t, aus[0])

	return clipFixture{sps: sps, pps: pps, aus: aus}
}

// ptsAt returns the synthesized 90 kHz decode timestamp for the i-th access
// unit fed to a Writer, at a nominal 15 fps (matching the low end of the
// camera's 15-20 fps range, see dev_docs/plan-v0.1.0.md).
func ptsAt(i int) uint64 {
	const ticksPerFrame = TimeScale / 15
	return uint64(i) * ticksPerFrame
}

// feedAll writes every access unit in fixture to w with synthesized 15 fps
// timestamps, failing the test on the first error.
func feedAll(t *testing.T, w *Writer, fixture clipFixture) {
	t.Helper()
	for i, au := range fixture.aus {
		if err := w.WriteAccessUnit(ptsAt(i), au); err != nil {
			t.Fatalf("WriteAccessUnit(%d): %v", i, err)
		}
	}
}

// countKeyframes returns how many of fixture's access units are keyframes,
// i.e. the number of GOPs, which is also the number of fragments a Writer
// produces when FragmentDuration is small enough that every GOP closes its
// own fragment (used below to size expectations without hard-coding the
// fixture's exact GOP count).
func countKeyframes(t *testing.T, fixture clipFixture) int {
	t.Helper()
	n := 0
	for _, au := range fixture.aus {
		var nalus h264c.AnnexB
		if err := nalus.Unmarshal(au); err != nil {
			t.Fatalf("AnnexB.Unmarshal: %v", err)
		}
		if h264c.IsRandomAccess(nalus) {
			n++
		}
	}
	return n
}

// TestWriterStreamingBoxStructure feeds the whole sample clip through a
// Writer into an in-memory sink (as NewWriter is meant to be used to stream
// to a browser's MSE SourceBuffer over HTTP), then parses the result back
// with the same mediacommon/v2 library the Writer is built on and checks
// its box structure: one init segment with an H264 avc1/avcC track at
// TimeScale, followed by one fragment per GOP, each starting at a keyframe,
// with strictly increasing sequence numbers and non-decreasing base times.
func TestWriterStreamingBoxStructure(t *testing.T) {
	fixture := loadClipFixture(t)
	wantFragments := countKeyframes(t, fixture) // one fragment closes per keyframe after the first, plus the final one at Close

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{FragmentDuration: time.Millisecond})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	feedAll(t, w, fixture)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	full := sink.Bytes()

	var init mfmp4.Init
	if err := init.Unmarshal(bytes.NewReader(full)); err != nil {
		t.Fatalf("parse init segment: %v", err)
	}
	if len(init.Tracks) != 1 {
		t.Fatalf("got %d tracks, want 1", len(init.Tracks))
	}
	track := init.Tracks[0]
	if track.ID != videoTrackID {
		t.Errorf("track ID = %d, want %d", track.ID, videoTrackID)
	}
	if track.TimeScale != TimeScale {
		t.Errorf("track TimeScale = %d, want %d", track.TimeScale, TimeScale)
	}
	h264Codec, ok := track.Codec.(*codecs.H264)
	if !ok {
		t.Fatalf("track codec = %T, want *codecs.H264", track.Codec)
	}
	if !bytes.Equal(h264Codec.SPS, fixture.sps) {
		t.Errorf("init segment SPS does not match the SPS passed to NewWriter")
	}
	if !bytes.Equal(h264Codec.PPS, fixture.pps) {
		t.Errorf("init segment PPS does not match the PPS passed to NewWriter")
	}

	// Re-marshal the parsed init segment to find where it ends in full, so
	// Parts.Unmarshal is only handed the fragment bytes that follow it
	// (mirrors how a real reader knows the init segment's length from its
	// own box sizes rather than needing an out-of-band split point).
	var reInit seekablebuffer.Buffer
	if err := init.Marshal(&reInit); err != nil {
		t.Fatalf("re-marshal init segment: %v", err)
	}
	initLen := reInit.Len()

	var parts mfmp4.Parts
	if err := parts.Unmarshal(full[initLen:]); err != nil {
		t.Fatalf("parse fragments: %v", err)
	}
	if len(parts) != wantFragments {
		t.Fatalf("got %d fragments, want %d (one per GOP, %d keyframes in the fixture)", len(parts), wantFragments, wantFragments)
	}

	totalSamples := 0
	var lastBaseTime uint64
	for i, part := range parts {
		wantSeq := uint32(i + 1)
		if part.SequenceNumber != wantSeq {
			t.Errorf("fragment %d: SequenceNumber = %d, want %d", i, part.SequenceNumber, wantSeq)
		}
		if len(part.Tracks) != 1 {
			t.Fatalf("fragment %d: got %d tracks, want 1", i, len(part.Tracks))
		}
		pt := part.Tracks[0]
		if pt.ID != videoTrackID {
			t.Errorf("fragment %d: track ID = %d, want %d", i, pt.ID, videoTrackID)
		}
		if i > 0 && pt.BaseTime < lastBaseTime {
			t.Errorf("fragment %d: BaseTime %d is before previous fragment's BaseTime %d", i, pt.BaseTime, lastBaseTime)
		}
		lastBaseTime = pt.BaseTime
		if len(pt.Samples) == 0 {
			t.Fatalf("fragment %d has no samples", i)
		}
		if pt.Samples[0].IsNonSyncSample {
			t.Errorf("fragment %d: first sample is not a sync sample, want every fragment to start at a keyframe", i)
		}
		for _, s := range pt.Samples[1:] {
			if !s.IsNonSyncSample {
				t.Errorf("fragment %d: a non-first sample is marked as a sync sample, want only the first", i)
			}
		}
		for _, s := range pt.Samples {
			if s.Duration == 0 {
				t.Errorf("fragment %d: a sample has zero duration", i)
			}
		}
		totalSamples += len(pt.Samples)
	}

	if totalSamples != len(fixture.aus) {
		t.Errorf("got %d total samples across all fragments, want %d (one per access unit written)", totalSamples, len(fixture.aus))
	}
}

// TestWriterRecordingSurvivesHardKill records the whole sample clip to a
// file via NewRecordingWriter but never calls Close, simulating the daemon
// process being killed (Windows daemon stop is proc.Kill(), see
// dev_docs/plan-v0.1.0.md decision 11). It confirms the file on disk is
// exactly the fragments that were fsynced before the "kill" (one fewer
// than the number of GOPs, since the last GOP's fragment only ever gets
// flushed by a following keyframe or by Close, neither of which happens
// here) and that it still parses cleanly: the crash-safety property this
// package exists for (T11c hard-kill requirement).
func TestWriterRecordingSurvivesHardKill(t *testing.T) {
	fixture := loadClipFixture(t)
	keyframes := countKeyframes(t, fixture)
	if keyframes < 2 {
		t.Fatalf("fixture has only %d keyframe(s), need at least 2 for this test", keyframes)
	}

	// Opened directly (rather than through NewRecordingWriter) so this test
	// can hold on to the *os.File itself: a real killed process has its
	// file handles released by the OS without running any of its own code,
	// which is simulated below by closing the raw file handle directly
	// instead of calling Writer.Close (which would flush and finalize the
	// still-buffered last fragment, defeating the point of this test).
	path := filepath.Join(t.TempDir(), "recording.mp4")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("open recording file: %v", err)
	}
	w, err := NewWriter(f, fixture.sps, fixture.pps, Config{FragmentDuration: time.Millisecond})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	feedAll(t, w, fixture)
	// Deliberately not calling w.Close(): this is the point of the test.
	// Every fragment already written was fsynced by WriteAccessUnit
	// already, so releasing the raw OS handle (as the OS itself would do to
	// a killed process's file descriptors) is all that is needed to
	// simulate the crash.
	if err := f.Close(); err != nil {
		t.Fatalf("close raw file handle (simulated OS cleanup after kill): %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recording after simulated hard kill: %v", err)
	}

	var init mfmp4.Init
	if err := init.Unmarshal(bytes.NewReader(data)); err != nil {
		t.Fatalf("parse init segment from killed recording: %v", err)
	}

	var reInit seekablebuffer.Buffer
	if err := init.Marshal(&reInit); err != nil {
		t.Fatalf("re-marshal init segment: %v", err)
	}

	var parts mfmp4.Parts
	if err := parts.Unmarshal(data[reInit.Len():]); err != nil {
		t.Fatalf("parse fragments from killed recording: %v", err)
	}

	wantFragments := keyframes - 1
	if len(parts) != wantFragments {
		t.Fatalf("killed recording has %d fragments, want %d (the last GOP's fragment should be lost, never flushed)", len(parts), wantFragments)
	}
	for i, part := range parts {
		if part.SequenceNumber != uint32(i+1) {
			t.Errorf("fragment %d: SequenceNumber = %d, want %d", i, part.SequenceNumber, i+1)
		}
		if len(part.Tracks) != 1 || len(part.Tracks[0].Samples) == 0 {
			t.Fatalf("fragment %d is empty or malformed", i)
		}
	}
}

// TestWriterTruncationAfterNFragmentsStillParses truncates a clean,
// fully-closed recording immediately after its second fragment (discarding
// everything from the third fragment onward, as if a hard kill had torn a
// later write) and confirms the init segment plus the first two fragments
// still parse correctly on their own. This is the literal "truncate after N
// fragments" check this task asks for, complementing
// TestWriterRecordingSurvivesHardKill (which exercises the same property
// via an actual unflushed Writer instead of a manual byte cut).
func TestWriterTruncationAfterNFragmentsStillParses(t *testing.T) {
	fixture := loadClipFixture(t)
	if countKeyframes(t, fixture) < 3 {
		t.Fatalf("fixture needs at least 3 GOPs for this test")
	}

	rec := &offsetRecordingWriter{}
	w, err := NewWriter(rec, fixture.sps, fixture.pps, Config{FragmentDuration: time.Millisecond})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	feedAll(t, w, fixture)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// offsets[0] is the end of the init segment; offsets[1], offsets[2], ...
	// are the end of fragment 1, 2, .... Truncate right after fragment 2.
	if len(rec.offsets) < 3 {
		t.Fatalf("got %d recorded chunks, want at least 3 (init + 2 fragments)", len(rec.offsets))
	}
	truncated := rec.buf.Bytes()[:rec.offsets[2]]

	var init mfmp4.Init
	if err := init.Unmarshal(bytes.NewReader(truncated)); err != nil {
		t.Fatalf("parse init segment from truncated file: %v", err)
	}

	var reInit seekablebuffer.Buffer
	if err := init.Marshal(&reInit); err != nil {
		t.Fatalf("re-marshal init segment: %v", err)
	}

	var parts mfmp4.Parts
	if err := parts.Unmarshal(truncated[reInit.Len():]); err != nil {
		t.Fatalf("parse fragments from truncated file: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d fragments after truncating to 2, want 2", len(parts))
	}
}

// offsetRecordingWriter wraps a byte buffer and records, after each Write
// call, the buffer's length so far, letting a test truncate to a byte
// offset that lands exactly on a chunk boundary (init segment or a
// fragment) without needing to know each chunk's size up front.
type offsetRecordingWriter struct {
	buf     bytes.Buffer
	offsets []int
}

func (o *offsetRecordingWriter) Write(p []byte) (int, error) {
	n, err := o.buf.Write(p)
	o.offsets = append(o.offsets, o.buf.Len())
	return n, err
}

// TestWriterFirstAccessUnitMustBeKeyframe confirms WriteAccessUnit rejects
// a non-keyframe as the very first access unit, rather than silently
// producing a fragment with no keyframe at its start.
func TestWriterFirstAccessUnitMustBeKeyframe(t *testing.T) {
	fixture := loadClipFixture(t)

	// aus[1] is guaranteed not to be a keyframe: aus[0] is the fixture's
	// first keyframe (by construction, see loadClipFixture) and this
	// stream carries no two consecutive keyframes.
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteAccessUnit(ptsAt(1), fixture.aus[1]); err == nil {
		t.Fatalf("WriteAccessUnit with a non-keyframe first access unit: want error, got nil")
	}
}

// TestWriterRejectsNonIncreasingTimestamps confirms WriteAccessUnit refuses
// a pts that does not strictly advance, instead of silently producing a
// zero or negative sample duration.
func TestWriterRejectsNonIncreasingTimestamps(t *testing.T) {
	fixture := loadClipFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteAccessUnit(1000, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(0): %v", err)
	}
	if err := w.WriteAccessUnit(1000, fixture.aus[1]); err == nil {
		t.Fatalf("WriteAccessUnit with a non-increasing timestamp: want error, got nil")
	}
	if err := w.WriteAccessUnit(999, fixture.aus[1]); err == nil {
		t.Fatalf("WriteAccessUnit with a decreasing timestamp: want error, got nil")
	}
}

// TestWriterClosedRejectsFurtherWrites confirms a closed Writer rejects
// further WriteAccessUnit calls immediately, matching this project's
// pattern for other camera types (see internal/camera/decode.Decoder).
func TestWriterClosedRejectsFurtherWrites(t *testing.T) {
	fixture := loadClipFixture(t)

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteAccessUnit(ptsAt(0), fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: want nil (safe to call twice), got %v", err)
	}
	if err := w.WriteAccessUnit(ptsAt(1), fixture.aus[1]); err == nil {
		t.Fatalf("WriteAccessUnit after Close: want error, got nil")
	}
}

// avccSize returns the length of au's AVCC-converted payload, computed the
// same way WriteAccessUnit computes it internally, so a test can pick a
// Config.MaxBufferedBytes threshold precisely relative to real fixture
// access units instead of guessing a byte count.
func avccSize(t *testing.T, au []byte) int {
	t.Helper()
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(au); err != nil {
		t.Fatalf("AnnexB.Unmarshal: %v", err)
	}
	payload, err := h264c.AVCC(nalus).Marshal()
	if err != nil {
		t.Fatalf("AVCC.Marshal: %v", err)
	}
	return len(payload)
}

// mutateSPS returns a copy of au (a keyframe access unit that must contain
// an SPS NAL unit) with one byte inside the SPS payload flipped, producing
// an access unit whose SPS differs from au's original SPS while remaining
// a structurally valid Annex-B access unit with the same NAL types and
// start codes. AnnexB.Unmarshal's NAL unit slices alias the buffer they
// were parsed from, so re-unmarshaling the copy and mutating through the
// returned slice mutates the copy directly.
func mutateSPS(t *testing.T, au []byte) []byte {
	t.Helper()
	mutated := append([]byte(nil), au...)
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(mutated); err != nil {
		t.Fatalf("AnnexB.Unmarshal: %v", err)
	}
	for _, n := range nalus {
		if len(n) > 4 && h264c.NALUType(n[0]&0x1f) == h264c.NALUTypeSPS {
			n[len(n)-1] ^= 0xff
			return mutated
		}
	}
	t.Fatalf("mutateSPS: access unit has no SPS NAL unit")
	return nil
}

// TestWriterRejectsChangedSPSMidStream confirms WriteAccessUnit rejects a
// keyframe whose SPS differs from the SPS embedded in the init segment
// (ErrParametersChanged), leaves the rejected access unit's sample out of
// the output entirely, and leaves the Writer usable for further calls
// afterward (the caller is expected to Close and reopen with a new init
// segment instead, but a Writer that mistakenly keeps writing to the same
// one should not corrupt it further).
func TestWriterRejectsChangedSPSMidStream(t *testing.T) {
	fixture := loadClipFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{FragmentDuration: time.Hour})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.WriteAccessUnit(ptsAt(0), fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(0): %v", err)
	}
	if err := w.WriteAccessUnit(ptsAt(1), fixture.aus[1]); err != nil {
		t.Fatalf("WriteAccessUnit(1): %v", err)
	}

	mutated := mutateSPS(t, fixture.aus[0])
	if err := w.WriteAccessUnit(ptsAt(2), mutated); !errors.Is(err, ErrParametersChanged) {
		t.Fatalf("WriteAccessUnit with a changed SPS: got %v, want ErrParametersChanged", err)
	}

	// The Writer must still accept further access units against the
	// original, unchanged parameters.
	if err := w.WriteAccessUnit(ptsAt(3), fixture.aus[1]); err != nil {
		t.Fatalf("WriteAccessUnit after ErrParametersChanged: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var init mfmp4.Init
	if err := init.Unmarshal(bytes.NewReader(sink.Bytes())); err != nil {
		t.Fatalf("parse init segment: %v", err)
	}
	var reInit seekablebuffer.Buffer
	if err := init.Marshal(&reInit); err != nil {
		t.Fatalf("re-marshal init segment: %v", err)
	}

	var parts mfmp4.Parts
	if err := parts.Unmarshal(sink.Bytes()[reInit.Len():]); err != nil {
		t.Fatalf("parse fragments: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("got %d fragments, want 1 (FragmentDuration is an hour, so nothing else closes it)", len(parts))
	}
	gotSamples := len(parts[0].Tracks[0].Samples)
	if gotSamples != 3 {
		t.Fatalf("got %d samples in the fragment, want 3 (the rejected access unit must not be written)", gotSamples)
	}
}

// TestWriterNoKeyframeWithinMaxWaitDropsBuffer confirms WriteAccessUnit
// returns ErrNoKeyframe and drops the buffered samples once
// Config.MaxKeyframeWait ticks elapse since the current fragment's first
// sample without a keyframe arriving, that a non-keyframe access unit is
// rejected while the Writer is waiting for the next keyframe, and that a
// keyframe resumes normal operation afterward.
func TestWriterNoKeyframeWithinMaxWaitDropsBuffer(t *testing.T) {
	fixture := loadClipFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{
		FragmentDuration: time.Hour,
		MaxKeyframeWait:  5 * time.Millisecond, // 450 ticks at TimeScale
		MaxBufferedBytes: 1 << 30,              // effectively unlimited, isolates the time bound
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.WriteAccessUnit(0, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(keyframe): %v", err)
	}

	var pts uint64
	var gotErr error
	for i := 0; i < 50; i++ {
		pts += 100
		gotErr = w.WriteAccessUnit(pts, fixture.aus[1])
		if gotErr != nil {
			break
		}
	}
	if !errors.Is(gotErr, ErrNoKeyframe) {
		t.Fatalf("got %v, want ErrNoKeyframe", gotErr)
	}

	pts += 100
	if err := w.WriteAccessUnit(pts, fixture.aus[1]); err == nil {
		t.Fatalf("WriteAccessUnit with a non-keyframe while awaiting a keyframe: want error, got nil")
	}

	pts += 100
	if err := w.WriteAccessUnit(pts, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(keyframe) to resume after ErrNoKeyframe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWriterMaxBufferedBytesDropsBuffer confirms WriteAccessUnit returns
// ErrNoKeyframe and drops the buffered samples once Config.MaxBufferedBytes
// of sample payload accumulate without a keyframe arriving, even when
// Config.MaxKeyframeWait is far from being reached, and that a keyframe
// resumes normal operation afterward.
func TestWriterMaxBufferedBytesDropsBuffer(t *testing.T) {
	fixture := loadClipFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	size0 := avccSize(t, fixture.aus[0])
	size1 := avccSize(t, fixture.aus[1])

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{
		FragmentDuration: time.Hour,
		MaxKeyframeWait:  time.Hour, // effectively unlimited, isolates the byte bound
		MaxBufferedBytes: size0 + size1 + 1,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.WriteAccessUnit(0, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(keyframe): %v", err)
	}
	// size0 + size1 <= MaxBufferedBytes: must still fit.
	if err := w.WriteAccessUnit(1000, fixture.aus[1]); err != nil {
		t.Fatalf("WriteAccessUnit(1): %v", err)
	}
	// size0 + size1 + size1 > MaxBufferedBytes: must be rejected.
	if err := w.WriteAccessUnit(2000, fixture.aus[1]); !errors.Is(err, ErrNoKeyframe) {
		t.Fatalf("got %v, want ErrNoKeyframe", err)
	}

	if err := w.WriteAccessUnit(3000, fixture.aus[1]); err == nil {
		t.Fatalf("WriteAccessUnit with a non-keyframe while awaiting a keyframe: want error, got nil")
	}
	if err := w.WriteAccessUnit(4000, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(keyframe) to resume after ErrNoKeyframe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestWriterCloseRightAfterErrNoKeyframeLeavesFileValid confirms calling
// Close() directly while the Writer is in the needKeyframe state, right
// after ErrNoKeyframe dropped the buffered fragment (nothing pending to
// flush), leaves the recording file valid and flushes nothing extra
// (dev_docs/review-backlog.md item 15, Close's own doc comment: "If the
// Writer is currently waiting for a keyframe after ErrNoKeyframe dropped its
// buffered samples, there is nothing pending to flush: Close only closes
// the sink in that case").
func TestWriterCloseRightAfterErrNoKeyframeLeavesFileValid(t *testing.T) {
	fixture := loadClipFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	path := filepath.Join(t.TempDir(), "recording.mp4")
	w, err := NewRecordingWriter(path, fixture.sps, fixture.pps, Config{
		FragmentDuration: time.Hour,
		MaxKeyframeWait:  5 * time.Millisecond, // 450 ticks at TimeScale
		MaxBufferedBytes: 1 << 30,              // effectively unlimited, isolates the time bound
	})
	if err != nil {
		t.Fatalf("NewRecordingWriter: %v", err)
	}

	if err := w.WriteAccessUnit(0, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(keyframe): %v", err)
	}

	var pts uint64
	var gotErr error
	for i := 0; i < 50; i++ {
		pts += 100
		gotErr = w.WriteAccessUnit(pts, fixture.aus[1])
		if gotErr != nil {
			break
		}
	}
	if !errors.Is(gotErr, ErrNoKeyframe) {
		t.Fatalf("got %v, want ErrNoKeyframe", gotErr)
	}

	// Close immediately, with the Writer in the needKeyframe state: this is
	// the point of the test. No further access unit is fed.
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: want nil (safe to call twice), got %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recording: %v", err)
	}

	var init mfmp4.Init
	if err := init.Unmarshal(bytes.NewReader(data)); err != nil {
		t.Fatalf("parse init segment: %v", err)
	}
	var reInit seekablebuffer.Buffer
	if err := init.Marshal(&reInit); err != nil {
		t.Fatalf("re-marshal init segment: %v", err)
	}

	// The keyframe's sample was dropped by ErrNoKeyframe before ever being
	// flushed, and Close must not flush anything after that: the file must
	// be exactly the init segment, with no fragments at all.
	if len(data) != reInit.Len() {
		t.Errorf("recording is %d bytes, want exactly the init segment's %d bytes (Close must flush nothing extra after ErrNoKeyframe)", len(data), reInit.Len())
	}

	var parts mfmp4.Parts
	if err := parts.Unmarshal(data[reInit.Len():]); err != nil {
		t.Fatalf("parse fragments: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("got %d fragments, want 0 (the only buffered sample was dropped by ErrNoKeyframe, and Close must not flush anything after it)", len(parts))
	}
}

// TestWriterRebasesStreamStartAfterRecovery reproduces
// dev_docs/item51-review.md part 1's writer.go:331-342 finding: without
// rebasing streamStartPTS at an ErrNoKeyframe recovery, a fragment flushed
// after that recovery computes BaseTime (tfdt/baseMediaDecodeTime) relative
// to the Writer's original construction, so the dead time spent
// dropped-and-waiting for the missing keyframe counts as real elapsed
// stream time even though nothing was recorded during it - the exact
// mechanism the review traced to ffprobe reporting a wildly inflated
// start_time/duration (dev_docs/item51-review.md part 2c). This test
// forces that recovery directly (a short MaxKeyframeWait, same as
// TestWriterNoKeyframeWithinMaxWaitDropsBuffer) with a large, explicit gap
// between the dropped fragment's first sample and the recovering
// keyframe's pts, then parses the resulting fragment's own BaseTime and
// confirms it is 0 - anchored to the recovering keyframe, not inflated by
// the gap.
func TestWriterRebasesStreamStartAfterRecovery(t *testing.T) {
	fixture := loadClipFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	var sink bytes.Buffer
	w, err := NewWriter(&sink, fixture.sps, fixture.pps, Config{
		FragmentDuration: time.Hour, // isolates this test to the one fragment flushed by Close
		MaxKeyframeWait:  5 * time.Millisecond,
		MaxBufferedBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.WriteAccessUnit(0, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(keyframe): %v", err)
	}

	var pts uint64
	var gotErr error
	for i := 0; i < 50; i++ {
		pts += 100
		gotErr = w.WriteAccessUnit(pts, fixture.aus[1])
		if gotErr != nil {
			break
		}
	}
	if !errors.Is(gotErr, ErrNoKeyframe) {
		t.Fatalf("got %v, want ErrNoKeyframe", gotErr)
	}

	// A large, explicit dead gap (well over an hour at TimeScale, the same
	// order of magnitude as item50/51's own observed inflated figures)
	// between the dropped fragment and the recovering keyframe: real dead
	// time on the camera's own clock while no usable keyframe was being
	// delivered (e.g. a lost PLI response), not anything this Writer
	// recorded a sample for.
	const deadGapTicks = 400_000_000
	recoveryPTS := pts + deadGapTicks
	if err := w.WriteAccessUnit(recoveryPTS, fixture.aus[0]); err != nil {
		t.Fatalf("WriteAccessUnit(keyframe) to resume after ErrNoKeyframe: %v", err)
	}
	// A second sample after the recovering keyframe, so the recovered
	// fragment has more than the one sample Close's own synthesized
	// duration applies to.
	if err := w.WriteAccessUnit(recoveryPTS+100, fixture.aus[1]); err != nil {
		t.Fatalf("WriteAccessUnit after recovery: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var init mfmp4.Init
	if err := init.Unmarshal(bytes.NewReader(sink.Bytes())); err != nil {
		t.Fatalf("parse init segment: %v", err)
	}
	var reInit seekablebuffer.Buffer
	if err := init.Marshal(&reInit); err != nil {
		t.Fatalf("re-marshal init segment: %v", err)
	}

	var parts mfmp4.Parts
	if err := parts.Unmarshal(sink.Bytes()[reInit.Len():]); err != nil {
		t.Fatalf("parse fragments: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("got %d fragments, want 1 (the pre-recovery fragment was dropped by ErrNoKeyframe, "+
			"FragmentDuration is an hour so nothing else closes one, and Close flushes the last)", len(parts))
	}

	baseTime := parts[0].Tracks[0].BaseTime
	if baseTime != 0 {
		t.Errorf("fragment BaseTime = %d, want 0 (anchored to the recovering keyframe at pts %d, "+
			"not inflated by the %d-tick dead gap before it)", baseTime, recoveryPTS, deadGapTicks)
	}
	gotSamples := len(parts[0].Tracks[0].Samples)
	if gotSamples != 2 {
		t.Fatalf("got %d samples in the recovered fragment, want 2", gotSamples)
	}
}

// TestCodecString checks the MSE codec string derived from the sample
// clip's own SPS against ffprobe's independent read of the same clip
// ("ffprobe -show_entries stream=codec_name,profile,level" reports
// "h264"/"Main"/31, i.e. profile_idc 0x4d, level_idc 0x1f): this sample
// fixture (shared with internal/camera/decode) happens to be Main profile,
// not the Baseline-compatible 42e01f this task describes for the live
// camera, but CodecString's byte layout (profile_idc, the constraint-flags
// byte, level_idc, per RFC 6381 and the AVCDecoderConfigurationRecord this
// package's Writer embeds) is exactly the same regardless of which profile
// is present, so this still fully exercises the helper. Main profile here
// also has every constraint flag clear, giving "00" as the middle byte; a
// Baseline stream like the live camera's would show "e0" there instead
// (constraint_set0/1/2_flag all set), which is what makes the live stream's
// string "avc1.42e01f".
func TestCodecString(t *testing.T) {
	fixture := loadClipFixture(t)

	got, err := CodecString(fixture.sps)
	if err != nil {
		t.Fatalf("CodecString: %v", err)
	}
	const want = "avc1.4d001f"
	if got != want {
		t.Errorf("CodecString = %q, want %q", got, want)
	}
}

// TestWriterOutputDecodableByFFprobe writes a full recording and, if
// ffprobe is available at ffprobePath, asks it to identify the video
// stream. This is a verification aid only: per this task's instructions,
// ffmpeg is never a hard dependency, so the test skips (not fails) when
// ffprobe is not present.
func TestWriterOutputDecodableByFFprobe(t *testing.T) {
	if _, err := os.Stat(ffprobePath); err != nil {
		t.Skipf("ffprobe not found at %s, skipping (not a hard dependency): %v", ffprobePath, err)
	}

	fixture := loadClipFixture(t)
	path := filepath.Join(t.TempDir(), "clip.mp4")
	w, err := NewRecordingWriter(path, fixture.sps, fixture.pps, Config{FragmentDuration: time.Millisecond})
	if err != nil {
		t.Fatalf("NewRecordingWriter: %v", err)
	}
	feedAll(t, w, fixture)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(ffprobePath,
		"-v", "error",
		"-print_format", "json",
		"-show_entries", "stream=codec_name,codec_type,width,height",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("ffprobe failed: %v, stderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("run ffprobe: %v", err)
	}

	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parse ffprobe output: %v\noutput: %s", err, out)
	}

	if len(probe.Streams) != 1 {
		t.Fatalf("ffprobe found %d streams, want 1\noutput: %s", len(probe.Streams), out)
	}
	s := probe.Streams[0]
	if s.CodecType != "video" || s.CodecName != "h264" {
		t.Errorf("ffprobe stream = %s/%s, want video/h264", s.CodecType, s.CodecName)
	}
	if s.Width != 1280 || s.Height != 720 {
		t.Errorf("ffprobe reports %dx%d, want 1280x720", s.Width, s.Height)
	}
}

// TestWriterNormalizesBaseTimeToZero is dev_docs/review-backlog.md item 50's
// verification: a Writer's baseMediaDecodeTime (tfdt, mfmp4.PartTrack.
// BaseTime) must start at 0 relative to that Writer's own first sample, not
// relative to the camera's session-wide RTP-derived clock. This matters
// because internal/daemon's Hub fans out one upstream camera.Session's
// access units (whose PTS only ever resets to 0 when a brand new Session is
// opened, internal/camera/pts.go) to every viewer stream and recording part,
// each of which gets its own fmp4.Writer (internal/daemon/viewer_stream.go's
// NewWriter per HTTP request, internal/daemon/recorder.go's
// NewRecordingWriter per part): a consumer that subscribes well after the
// session opened (a late-joining viewer, or a recording started while
// streams have been running for a while) would otherwise start writing
// access units whose PTS is already tens of minutes into the session's own
// clock, and dev_docs/t11e-soak-report-2.md anomaly 3 observed exactly the
// symptom this produces downstream: ffprobe reporting an implausible
// start_time/duration far larger than the real capture window.
//
// This test simulates that late-subscribe case directly: it feeds the same
// clip fixture used elsewhere in this file, but with a large constant
// offset added to every timestamp (as if this Writer's first sample arrived
// long after the upstream session's own t=0), then confirms via ffprobe
// that the resulting file's start_time is ~0 and its duration matches the
// clip's own real span, not the offset. Writer.WriteAccessUnit's
// streamStartPTS (set from the first access unit ever written to a Writer)
// and flushFragment's BaseTime (fragStartPTS - streamStartPTS) are what
// make this hold; this test locks that behavior in.
func TestWriterNormalizesBaseTimeToZero(t *testing.T) {
	if _, err := os.Stat(ffprobePath); err != nil {
		t.Skipf("ffprobe not found at %s, skipping (not a hard dependency): %v", ffprobePath, err)
	}

	fixture := loadClipFixture(t)

	// Large enough to be unmistakable if it leaked into the output (roughly
	// 5,555 seconds, i.e. well over an hour into a session's own clock), yet
	// far below the 32-bit RTP timestamp space this package's PTS values are
	// already unwrapped past (internal/camera/pts.go).
	const lateSubscribeOffset = 500_000_000

	path := filepath.Join(t.TempDir(), "late_subscriber.mp4")
	w, err := NewRecordingWriter(path, fixture.sps, fixture.pps, Config{FragmentDuration: time.Millisecond})
	if err != nil {
		t.Fatalf("NewRecordingWriter: %v", err)
	}
	for i, au := range fixture.aus {
		if err := w.WriteAccessUnit(lateSubscribeOffset+ptsAt(i), au); err != nil {
			t.Fatalf("WriteAccessUnit(%d): %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cmd := exec.Command(ffprobePath,
		"-v", "error",
		"-print_format", "json",
		"-show_entries", "format=start_time,duration",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Fatalf("ffprobe failed: %v, stderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("run ffprobe: %v", err)
	}

	var probe struct {
		Format struct {
			StartTime string `json:"start_time"`
			Duration  string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parse ffprobe output: %v\noutput: %s", err, out)
	}

	var startTime, duration float64
	if _, err := fmt.Sscanf(probe.Format.StartTime, "%f", &startTime); err != nil {
		t.Fatalf("parse start_time %q: %v", probe.Format.StartTime, err)
	}
	if _, err := fmt.Sscanf(probe.Format.Duration, "%f", &duration); err != nil {
		t.Fatalf("parse duration %q: %v", probe.Format.Duration, err)
	}

	// start_time must be ~0 (never anywhere near the offset, which is over
	// 5,555 seconds), tolerating only float/rounding noise.
	if startTime < -0.1 || startTime > 0.1 {
		t.Errorf("start_time = %v, want ~0 (not the %v-tick late-subscribe offset)", startTime, lateSubscribeOffset)
	}

	// The clip is fed at a nominal 15 fps (ptsAt), and Close's fallback
	// duration for the final sample uses the same 15 fps assumption
	// (defaultNominalDuration = TimeScale/15), so the whole file's real
	// span is exactly len(fixture.aus)/15 seconds - independent of
	// lateSubscribeOffset, which is the whole point of this test.
	wantDuration := float64(len(fixture.aus)) / 15.0
	if duration < wantDuration-0.1 || duration > wantDuration+0.1 {
		t.Errorf("duration = %v, want ~%v (the sample span, not inflated by the %v-tick late-subscribe offset)",
			duration, wantDuration, lateSubscribeOffset)
	}
}

// TestSplitAccessUnitsSanity is a guard on this test file's own fixture
// helper, not on the fmp4 package: if it ever stops finding a reasonable
// access unit and keyframe count in the shared sample clip, every other
// test in this file would be exercising the wrong thing without an
// obviously related failure, so this fails loudly and specifically instead.
func TestSplitAccessUnitsSanity(t *testing.T) {
	data, err := os.ReadFile(sampleClipPath)
	if err != nil {
		t.Fatalf("read %s: %v", sampleClipPath, err)
	}
	aus := splitAccessUnits(data)
	if len(aus) < 10 {
		t.Fatalf("got %d access units, want at least 10", len(aus))
	}

	keyframes := 0
	for _, au := range aus {
		if nalType(au) != 7 && nalType(au) != 8 && nalType(au) != 1 && nalType(au) != 5 {
			continue
		}
		var nalus h264c.AnnexB
		if err := nalus.Unmarshal(au); err != nil {
			t.Fatalf("AnnexB.Unmarshal on a split access unit: %v", err)
		}
		if h264c.IsRandomAccess(nalus) {
			keyframes++
		}
	}
	if keyframes < 2 {
		t.Fatalf("got %d keyframe access units, want at least 2", keyframes)
	}
}

var _ io.Writer = (*offsetRecordingWriter)(nil)
