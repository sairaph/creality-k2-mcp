package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	h264c "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mfmp4 "github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/camera/decode"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/daemon/socket"
)

// This file exercises Recorder (T11c) against a fake hub and a fake printer
// state source, never a real camera.Session, WebRTC session or non-loopback
// socket (AGENTS.md hard testing rule); the AF_UNIX round-trip test below
// dials a real socket in a t.TempDir(), which is allowed under the same rule
// (see daemon_test.go's own doc comment).

// sampleClipPath is the shared H.264 fixture also used by
// internal/camera/fmp4's own tests (see that package's writer_test.go for
// the identical rationale): Annex-B, 1280x720, starts mid-GOP.
const sampleClipPath = `../camera/decode/testdata/camera_sample_clip.h264`

// splitAccessUnits, firstKeyframeIndex, extractParameterSets and
// loadClipFixture below are a trimmed duplicate of
// internal/camera/fmp4/writer_test.go's own fixture helpers of the same
// name: that file's are unexported test helpers in a different package, so
// they cannot be imported from here. See that file for the full reasoning
// behind the grouping rule.
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

	type nal struct{ begin, end int }
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

// ptsAt returns a synthesized 90 kHz decode timestamp at a nominal 15 fps,
// matching fmp4's own writer_test.go.
func ptsAt(i int) uint64 {
	const ticksPerFrame = 90000 / 15
	return uint64(i) * ticksPerFrame
}

// fakeRecorderHub is a minimal, deterministic recorderHub: one buffered,
// never-dropping channel per host, fed directly by the test via send. This
// is a purpose-built fake rather than a reuse of camera_hub_test.go's
// fakeOpener/fakeSession/Hub chain, because that chain's dispatch and
// subscriber channels are both best-effort, non-blocking sends (correct for
// a live fan-out, but a source of nondeterministic dropped frames for a
// test that needs to feed an exact, ordered sequence of access units).
type fakeRecorderHub struct {
	mu           sync.Mutex
	channels     map[string]chan camera.AccessUnit
	keyframeReqs map[string]int
	keyframeErr  error
}

func newFakeRecorderHub() *fakeRecorderHub {
	return &fakeRecorderHub{
		channels:     make(map[string]chan camera.AccessUnit),
		keyframeReqs: make(map[string]int),
	}
}

func (h *fakeRecorderHub) chFor(host string) chan camera.AccessUnit {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.channels[host]
	if !ok {
		ch = make(chan camera.AccessUnit, 256)
		h.channels[host] = ch
	}
	return ch
}

func (h *fakeRecorderHub) Subscribe(host string) (int, <-chan camera.AccessUnit) {
	return 1, h.chFor(host)
}

func (h *fakeRecorderHub) Unsubscribe(host string, id int) {}

func (h *fakeRecorderHub) RequestKeyframe(host string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.keyframeReqs[host]++
	return h.keyframeErr
}

func (h *fakeRecorderHub) keyframeRequestCount(host string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.keyframeReqs[host]
}

func (h *fakeRecorderHub) send(host string, au camera.AccessUnit) {
	h.chFor(host) <- au
}

// Snapshot is a deterministic test double for Hub.Snapshot (review backlog
// item 51): it requests a keyframe (so tests that assert on
// keyframeRequestCount still observe exactly the calls runTimelapse makes)
// and then reads directly from host's channel until it has a keyframe
// access unit followed by a trailing one, the same "keyframe plus the next
// access unit" boundary internal/camera.Snapshot and the real
// Hub.Snapshot's rolling-buffer decode both rely on, decoding the pair with
// a throwaway decode.Decoder. This is a purpose-built fake rather than a
// reuse of the real Hub's golden-buffer logic for the same reason
// fakeRecorderHub exists at all (see its own doc comment): deterministic,
// ordered delivery from an exact test-fed sequence, not a live fan-out.
// Nothing else may read from this same channel while a Snapshot call is in
// flight (runTimelapse, since review backlog item 51, no longer drains the
// subscription channel itself for exactly this reason).
func (h *fakeRecorderHub) Snapshot(ctx context.Context, host string, _ time.Duration) (*camera.SnapshotResult, error) {
	if err := h.RequestKeyframe(host); err != nil {
		return nil, err
	}
	ch := h.chFor(host)
	var kf *camera.AccessUnit
	for {
		select {
		case au, ok := <-ch:
			if !ok {
				return nil, errors.New("fakeRecorderHub: channel closed")
			}
			if au.Keyframe {
				a := au
				kf = &a
				continue
			}
			if kf == nil {
				continue
			}
			buf := append(append([]byte(nil), kf.Data...), au.Data...)
			dec, err := decode.New(ctx)
			if err != nil {
				return nil, err
			}
			defer dec.Close(ctx)
			img, err := dec.DecodeKeyframe(ctx, buf)
			if err != nil {
				return nil, err
			}
			b := img.Bounds()
			return &camera.SnapshotResult{Image: img, CapturedAt: time.Now(), Width: b.Dx(), Height: b.Dy()}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// feed sends fixture's access units (starting at index from) to host, one
// at a time, with synthesized 15 fps timestamps.
func feed(h *fakeRecorderHub, host string, fixture clipFixture, from, to int) {
	for i := from; i < to; i++ {
		h.send(host, camera.AccessUnit{Data: fixture.aus[i], Keyframe: isKeyframeAU(fixture.aus[i]), PTS: ptsAt(i)})
	}
}

func isKeyframeAU(au []byte) bool {
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(au); err != nil {
		return false
	}
	return h264c.IsRandomAccess(nalus)
}

func newTestRecorder(t *testing.T, hub recorderHub, state StateSource) *Recorder {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "recordings")
	rec := NewRecorder(dir, hub, state, nil)
	rec.freeBytes = func(string) (uint64, error) { return 10 << 30, nil } // 10 GiB, comfortably above both thresholds
	rec.now = time.Now
	return rec
}

// waitForRecorderStop polls until id is no longer active, for a fixed
// budget, failing the test otherwise.
func waitForRecorderStop(t *testing.T, r *Recorder, id string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, func() bool {
		r.mu.Lock()
		_, active := r.active[id]
		r.mu.Unlock()
		return !active
	})
}

// parseRecording parses path as an fmp4 file (init segment plus zero or
// more fragments), matching internal/camera/fmp4's own writer_test.go
// pattern, and returns the total number of samples across every fragment.
func parseRecording(t *testing.T, path string) (init mfmp4.Init, totalSamples int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := init.Unmarshal(bytes.NewReader(data)); err != nil {
		t.Fatalf("parse init segment of %s: %v", path, err)
	}
	var reInit seekablebuffer.Buffer
	if err := init.Marshal(&reInit); err != nil {
		t.Fatalf("re-marshal init segment: %v", err)
	}
	var parts mfmp4.Parts
	if err := parts.Unmarshal(data[reInit.Len():]); err != nil {
		t.Fatalf("parse fragments of %s: %v", path, err)
	}
	for _, part := range parts {
		for _, tr := range part.Tracks {
			totalSamples += len(tr.Samples)
		}
	}
	return init, totalSamples
}

// --- Start/Stop happy path: clean stop produces a valid, parseable file ---

func TestRecorder_StartStop_ProducesValidFile(t *testing.T) {
	shrinkRecorderTimings(t)
	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)

	const host = "printer-a-host"
	go feed(hub, host, fixture, 0, len(fixture.aus))

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-a",
		Host:      host,
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !info.Active {
		t.Fatalf("Start result Active = false, want true")
	}
	if len(info.Parts) != 1 {
		t.Fatalf("Start result has %d parts, want 1", len(info.Parts))
	}

	// Give the run loop a moment to drain the rest of the fixture before
	// stopping, so the recording holds more than just the first keyframe.
	waitFor(t, 2*time.Second, func() bool {
		return fileSize(info.Parts[0].Path) > 0
	})
	time.Sleep(50 * time.Millisecond)

	stopped, err := rec.Stop(info.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Active {
		t.Fatalf("Stop result Active = true, want false")
	}
	if stopped.StopReason == "" {
		t.Fatalf("Stop result has no StopReason")
	}
	if len(stopped.Parts) != 1 {
		t.Fatalf("Stop result has %d parts, want 1", len(stopped.Parts))
	}
	if stopped.Bytes <= 0 {
		t.Fatalf("Stop result Bytes = %d, want > 0", stopped.Bytes)
	}

	init, totalSamples := parseRecording(t, stopped.Parts[0].Path)
	if len(init.Tracks) != 1 {
		t.Fatalf("parsed recording has %d tracks, want 1", len(init.Tracks))
	}
	h264Codec, ok := init.Tracks[0].Codec.(*codecs.H264)
	if !ok {
		t.Fatalf("track codec = %T, want *codecs.H264", init.Tracks[0].Codec)
	}
	if !bytes.Equal(h264Codec.SPS, fixture.sps) {
		t.Errorf("recorded SPS does not match the fixture's SPS")
	}
	if totalSamples == 0 {
		t.Fatalf("parsed recording has 0 samples across every fragment")
	}

	// A sidecar JSON file was written next to the part.
	sidecar := filepath.Join(filepath.Dir(stopped.Parts[0].Path), filepath.Base(stopped.ID)+".json")
	data, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var fromDisk RecordingInfo
	if err := json.Unmarshal(data, &fromDisk); err != nil {
		t.Fatalf("parse sidecar: %v", err)
	}
	if fromDisk.PrinterID != "printer-a" || fromDisk.ID != stopped.ID {
		t.Errorf("sidecar = %+v, want printer_id printer-a and id %s", fromDisk, stopped.ID)
	}

	waitForRecorderStop(t, rec, info.ID, 2*time.Second)
}

// --- Parameter change creates a new part file ---

func TestRecorder_ParametersChanged_CreatesNewPart(t *testing.T) {
	shrinkRecorderTimings(t)
	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	const host = "printer-b-host"

	// Start needs the first keyframe (fixture.aus[0]) already available on
	// the hub channel before it can return.
	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0)})

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-b",
		Host:      host,
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Feed a couple of ordinary frames on the original parameters first.
	feed(hub, host, fixture, 1, min(4, len(fixture.aus)))

	// Build a synthetic keyframe carrying a different SPS: reuse the real
	// keyframe access unit's bytes but flip the last byte of its SPS NAL,
	// which the fmp4 Writer detects by raw byte comparison only (it never
	// needs the SPS to be semantically valid H.264, only structurally
	// present with its own NAL header).
	changed := mutateSPS(t, fixture.aus[0])
	nextPTS := ptsAt(len(fixture.aus) + 10)
	hub.send(host, camera.AccessUnit{Data: changed, Keyframe: true, PTS: nextPTS})

	waitFor(t, 3*time.Second, func() bool {
		r := rec
		r.mu.Lock()
		ar, ok := r.active[info.ID]
		r.mu.Unlock()
		if !ok {
			return false
		}
		ar.mu.Lock()
		defer ar.mu.Unlock()
		return len(ar.parts) >= 1
	})

	stopped, err := rec.Stop(info.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(stopped.Parts) != 2 {
		t.Fatalf("got %d parts, want 2 (one per SPS/PPS generation)", len(stopped.Parts))
	}
	if stopped.Parts[0].EndReason == "" {
		t.Errorf("first part has no EndReason, want one naming the parameter change")
	}
	if _, err := os.Stat(stopped.Parts[1].Path); err != nil {
		t.Errorf("second part file missing: %v", err)
	}
}

// mutateSPS returns a copy of au with its SPS NAL unit's level_idc byte
// (offset 3 within the NAL: header, profile_idc, constraint_flags,
// level_idc) flipped, so the SPS differs by raw bytes from the original
// while staying a well-formed, parseable SPS: level_idc is a plain 8-bit
// field with no effect on the exp-golomb-coded bitstream that follows it,
// unlike almost every other SPS byte, so mutating it is safe for
// mediacommon's own SPS parser (used when opening a brand new fmp4.Writer
// for the resulting part, which fully parses the SPS to build the avcC
// box, unlike the raw byte comparison fmp4.Writer.WriteAccessUnit itself
// uses to detect a parameter change).
func mutateSPS(t *testing.T, au []byte) []byte {
	t.Helper()
	out := append([]byte(nil), au...)
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(au); err != nil {
		t.Fatalf("AnnexB.Unmarshal: %v", err)
	}
	// Find the SPS NAL's offset within au by searching for its raw bytes
	// (nalus entries do not carry start codes, but they are contiguous
	// subslices of the original buffer's payload).
	for _, n := range nalus {
		if len(n) < 4 || h264c.NALUType(n[0]&0x1f) != h264c.NALUTypeSPS {
			continue
		}
		idx := bytes.Index(out, n)
		if idx < 0 {
			t.Fatalf("could not locate SPS NAL bytes within the access unit")
		}
		out[idx+3] ^= 0x02 // level_idc: flip a low bit, still a valid level value
		return out
	}
	t.Fatalf("access unit has no SPS NAL unit long enough to hold level_idc")
	return nil
}

// --- Disk safety ---

func TestRecorder_Start_RefusesBelowFreeSpaceThreshold(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	rec.freeBytes = func(string) (uint64, error) { return 100 << 20, nil } // 100 MiB, below 1 GiB

	_, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-c",
		Host:      "printer-c-host",
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err == nil {
		t.Fatal("Start succeeded with free space below the 1 GiB safety threshold, want an error")
	}

	// The printer must not be left reserved after a refused start (a
	// subsequent Start with enough free space must succeed).
	rec.freeBytes = func(string) (uint64, error) { return 10 << 30, nil }
	fixture := loadClipFixture(t)
	go feed(hub, "printer-c-host", fixture, 0, 1)
	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-c",
		Host:      "printer-c-host",
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start after the reservation was released: %v", err)
	}
	// Stop before the test ends so t.TempDir()'s cleanup can remove the
	// recording file (Windows refuses to delete a file that is still open).
	if _, err := rec.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestRecorder_StopsWhenFreeSpaceFallsBelowThreshold(t *testing.T) {
	shrinkRecorderTimings(t)
	origInterval := recorderDiskCheckInterval
	recorderDiskCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { recorderDiskCheckInterval = origInterval })

	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	const host = "printer-d-host"

	var mu sync.Mutex
	free := uint64(10 << 30)
	rec.freeBytes = func(string) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		return free, nil
	}

	go feed(hub, host, fixture, 0, len(fixture.aus))

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-d",
		Host:      host,
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	free = 100 << 20 // 100 MiB, below the 512 MiB stop threshold
	mu.Unlock()

	waitForRecorderStop(t, rec, info.ID, 3*time.Second)

	res, err := rec.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, r := range res.Recordings {
		if r.ID == info.ID {
			found = true
			if r.StopReason == "" || !containsAny(r.StopReason, "disk", "512") {
				t.Errorf("StopReason = %q, want it to mention the disk safety threshold", r.StopReason)
			}
		}
	}
	if !found {
		t.Fatalf("recording %s not found in List after it stopped", info.ID)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// --- One active recording per printer ---

func TestRecorder_Start_RefusesSecondConcurrentRecordingForSamePrinter(t *testing.T) {
	shrinkRecorderTimings(t)
	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	const host = "printer-e-host"
	go feed(hub, host, fixture, 0, len(fixture.aus))

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-e",
		Host:      host,
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err = rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-e",
		Host:      host,
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err == nil {
		t.Fatal("second concurrent Start for the same printer succeeded, want an error")
	}

	if _, err := rec.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// --- Timelapse mode (T11d) ---

// snapshotWithLayer builds a printerstate.Snapshot reporting
// print_stats.info.current_layer = layer, the primary layer source
// (dev_docs plan-v0.1.0.md decision 11).
func snapshotWithLayer(layer int) printerstate.Snapshot {
	l := layer
	return printerstate.Snapshot{PrintStats: &moonraker.PrintStats{Info: moonraker.PrintStatsInfo{CurrentLayer: &l}}}
}

// timelapseSendFrame sends a fresh keyframe/trailer access unit pair on
// host, the technique runTimelapse expects to complete a decode (matching
// internal/camera.Snapshot's own "keyframe plus the next access unit"
// requirement): fixture.aus[0] is always a real keyframe (loadClipFixture
// slices the clip to start at one), and any second access unit serves as
// the trailer that lets the decoder recognize the keyframe's picture as
// complete.
func timelapseSendFrame(hub *fakeRecorderHub, host string, fixture clipFixture, seq int) {
	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(seq * 2)})
	hub.send(host, camera.AccessUnit{Data: fixture.aus[1%len(fixture.aus)], Keyframe: false, PTS: ptsAt(seq*2 + 1)})
}

func timelapseFrameCount(r *Recorder, id string) int {
	r.mu.Lock()
	ar, ok := r.active[id]
	r.mu.Unlock()
	if !ok {
		return -1
	}
	ar.mu.Lock()
	defer ar.mu.Unlock()
	return len(ar.frames)
}

// TestRecorder_Timelapse_LayersOneToFiveThenPrintEnd steps a fake state
// source's reported layer 1..5, feeding a fresh keyframe (exercising the
// real decode path, internal/camera/decode's wasm decoder) for each
// detected layer change, then reports the print complete: it asserts one
// JPEG frame per layer, correctly numbered, decode actually producing
// readable JPEG bytes, interval_mode "layer" in the sidecar, and the
// recording stopping on its own with a stop reason naming the print end.
func TestRecorder_Timelapse_LayersOneToFiveThenPrintEnd(t *testing.T) {
	shrinkRecorderTimings(t)
	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	state := newFakeStateSource()
	rec := newTestRecorder(t, hub, state)
	const host = "printer-t1-host"
	const identity = "printer-t1.local"

	state.set(identity, snapshotEntry{
		snap:    snapshotWithLayer(1),
		derived: printerstate.Derived{State: printerstate.StatePrinting, Bucket: printerstate.BucketP},
		ok:      true,
	})
	// Start's own connectivity check consumes one keyframe before the run
	// loop's poll ticker ever starts; supply it up front.
	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0)})

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-t1",
		Host:      host,
		Identity:  identity,
		Mode:      RecordModeTimelapse,
		Until:     RecordUntilPrintEnd,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Mode != RecordModeTimelapse {
		t.Fatalf("Start result Mode = %q, want timelapse", info.Mode)
	}

	for layer := 1; layer <= 5; layer++ {
		state.set(identity, snapshotEntry{
			snap:    snapshotWithLayer(layer),
			derived: printerstate.Derived{State: printerstate.StatePrinting, Bucket: printerstate.BucketP},
			ok:      true,
		})
		waitFor(t, 2*time.Second, func() bool { return hub.keyframeRequestCount(host) >= layer })
		timelapseSendFrame(hub, host, fixture, layer)
		waitFor(t, 2*time.Second, func() bool { return timelapseFrameCount(rec, info.ID) >= layer })
	}

	state.set(identity, snapshotEntry{
		snap:    snapshotWithLayer(5),
		derived: printerstate.Derived{State: printerstate.StateComplete, Bucket: printerstate.BucketI},
		ok:      true,
	})
	waitForRecorderStop(t, rec, info.ID, 3*time.Second)

	res, err := rec.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var final RecordingInfo
	found := false
	for _, r := range res.Recordings {
		if r.ID == info.ID {
			final = r
			found = true
		}
	}
	if !found {
		t.Fatalf("recording %s not found in List after it stopped", info.ID)
	}
	if !containsAny(final.StopReason, "print ended", "complete") {
		t.Errorf("StopReason = %q, want it to mention the print ended", final.StopReason)
	}
	if final.IntervalMode != "layer" {
		t.Errorf("IntervalMode = %q, want \"layer\"", final.IntervalMode)
	}
	if len(final.Frames) != 5 {
		t.Fatalf("got %d frames, want 5 (one per layer)", len(final.Frames))
	}
	for i, f := range final.Frames {
		wantLayer := i + 1
		if f.Layer == nil || *f.Layer != wantLayer {
			t.Errorf("frame %d Layer = %v, want %d", i, f.Layer, wantLayer)
		}
		wantName := filepath.Join(rec.dir, "printer-t1", filepath.Base(info.ID), timelapseFrameName(0, &wantLayer))
		if f.Path != wantName {
			t.Errorf("frame %d Path = %q, want %q", i, f.Path, wantName)
		}
		img, err := decodeJPEGFile(f.Path)
		if err != nil {
			t.Errorf("frame %d: not a valid JPEG: %v", i, err)
			continue
		}
		if b := img.Bounds(); b.Dx() == 0 || b.Dy() == 0 {
			t.Errorf("frame %d: decoded image has zero size", i)
		}
		if f.Bytes <= 0 {
			t.Errorf("frame %d Bytes = %d, want > 0", i, f.Bytes)
		}
	}
}

// TestRecorder_Timelapse_TimeIntervalFallback exercises the fallback path:
// when the fake state source reports ok=true but neither
// print_stats.info.current_layer nor virtual_sdcard.layer is known (an
// empty Snapshot), captures happen on a fixed time interval instead, and
// the sidecar records interval_mode "time".
func TestRecorder_Timelapse_TimeIntervalFallback(t *testing.T) {
	shrinkRecorderTimings(t)
	origFallback := recorderTimelapseFallbackInterval
	recorderTimelapseFallbackInterval = 30 * time.Millisecond
	t.Cleanup(func() { recorderTimelapseFallbackInterval = origFallback })

	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	state := newFakeStateSource()
	rec := newTestRecorder(t, hub, state)
	const host = "printer-t2-host"
	const identity = "printer-t2.local"

	// ok=true but an empty Snapshot: neither layer source is known.
	state.set(identity, snapshotEntry{snap: printerstate.Snapshot{}, derived: printerstate.Derived{State: printerstate.StatePrinting}, ok: true})
	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0)})

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-t2",
		Host:      host,
		Identity:  identity,
		Mode:      RecordModeTimelapse,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Serve every keyframe request with a fresh frame pair as it comes in,
	// until at least two captures have landed (proving the fallback fires
	// more than once, not just at start).
	seq := 0
	waitFor(t, 3*time.Second, func() bool {
		if hub.keyframeRequestCount(host) > seq {
			seq++
			timelapseSendFrame(hub, host, fixture, seq)
		}
		return timelapseFrameCount(rec, info.ID) >= 2
	})

	stopped, err := rec.Stop(info.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.IntervalMode != "time" {
		t.Errorf("IntervalMode = %q, want \"time\"", stopped.IntervalMode)
	}
	if stopped.FallbackIntervalSeconds != int(recorderTimelapseFallbackInterval.Seconds()) {
		t.Errorf("FallbackIntervalSeconds = %d, want %d", stopped.FallbackIntervalSeconds, int(recorderTimelapseFallbackInterval.Seconds()))
	}
	if len(stopped.Frames) < 2 {
		t.Fatalf("got %d frames, want at least 2", len(stopped.Frames))
	}
	for _, f := range stopped.Frames {
		if f.Layer != nil {
			t.Errorf("frame %+v has a Layer set, want nil for a time-interval capture", f)
		}
	}
}

// TestRecorder_Timelapse_StopsWhenFreeSpaceFallsBelowThreshold mirrors
// video's own disk-safety test for timelapse mode.
func TestRecorder_Timelapse_StopsWhenFreeSpaceFallsBelowThreshold(t *testing.T) {
	shrinkRecorderTimings(t)
	origInterval := recorderDiskCheckInterval
	recorderDiskCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { recorderDiskCheckInterval = origInterval })

	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	// A timelapse always needs a state source (layer-change polling is
	// unconditional, not just for until:"print_end"); this test never sets
	// an entry for the identity it uses, so every poll's Snapshot call just
	// returns ok=false, which is handled the same as an unreachable printer.
	rec := newTestRecorder(t, hub, newFakeStateSource())
	const host = "printer-t3-host"

	var mu sync.Mutex
	free := uint64(10 << 30)
	rec.freeBytes = func(string) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		return free, nil
	}

	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0)})

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-t3",
		Host:      host,
		Mode:      RecordModeTimelapse,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	free = 100 << 20 // 100 MiB, below the 512 MiB stop threshold
	mu.Unlock()

	waitForRecorderStop(t, rec, info.ID, 3*time.Second)

	res, err := rec.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, r := range res.Recordings {
		if r.ID == info.ID {
			found = true
			if !containsAny(r.StopReason, "disk", "512") {
				t.Errorf("StopReason = %q, want it to mention the disk safety threshold", r.StopReason)
			}
		}
	}
	if !found {
		t.Fatalf("recording %s not found in List after it stopped", info.ID)
	}
}

// TestRecorder_Timelapse_Delete_RemovesFramesDirectory captures a couple of
// frames, stops the recording, then deletes it: every frame file, the
// frames directory itself, and the sidecar must all be gone.
func TestRecorder_Timelapse_Delete_RemovesFramesDirectory(t *testing.T) {
	shrinkRecorderTimings(t)
	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	state := newFakeStateSource()
	rec := newTestRecorder(t, hub, state)
	const host = "printer-t4-host"
	const identity = "printer-t4.local"

	state.set(identity, snapshotEntry{
		snap:    snapshotWithLayer(1),
		derived: printerstate.Derived{State: printerstate.StatePrinting},
		ok:      true,
	})
	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0)})

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-t4",
		Host:      host,
		Identity:  identity,
		Mode:      RecordModeTimelapse,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return hub.keyframeRequestCount(host) >= 1 })
	timelapseSendFrame(hub, host, fixture, 1)
	waitFor(t, 2*time.Second, func() bool { return timelapseFrameCount(rec, info.ID) >= 1 })

	stopped, err := rec.Stop(info.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(stopped.Frames) == 0 {
		t.Fatal("stopped timelapse has 0 frames, nothing to verify deletion of")
	}
	framesDir := filepath.Dir(stopped.Frames[0].Path)

	if err := rec.Delete(stopped.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, f := range stopped.Frames {
		if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
			t.Errorf("frame file %s still exists after Delete", f.Path)
		}
	}
	if _, err := os.Stat(framesDir); !os.IsNotExist(err) {
		t.Errorf("frames directory %s still exists after Delete", framesDir)
	}
	sidecar := filepath.Join(filepath.Dir(framesDir), filepath.Base(stopped.ID)+".json")
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("sidecar %s still exists after Delete", sidecar)
	}
}

// decodeJPEGFile reads and decodes path as a JPEG, for a test to confirm a
// timelapse frame is actually a valid, readable image.
func decodeJPEGFile(path string) (image.Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return jpeg.Decode(bytes.NewReader(data))
}

// --- until: print_end ---

func TestRecorder_UntilPrintEnd_StopsWhenPrintCompletes(t *testing.T) {
	shrinkRecorderTimings(t)
	recorderStatePollInterval = 10 * time.Millisecond
	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	state := newFakeStateSource()
	rec := newTestRecorder(t, hub, state)
	const host = "printer-g-host"
	const identity = "printer-g.local"

	state.set(identity, snapshotEntry{derived: printerstate.Derived{State: printerstate.StatePrinting, Bucket: printerstate.BucketP}, ok: true})

	go feed(hub, host, fixture, 0, len(fixture.aus))

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-g",
		Host:      host,
		Identity:  identity,
		Mode:      RecordModeVideo,
		Until:     RecordUntilPrintEnd,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	state.set(identity, snapshotEntry{derived: printerstate.Derived{State: printerstate.StateComplete, Bucket: printerstate.BucketI}, ok: true})

	waitForRecorderStop(t, rec, info.ID, 3*time.Second)

	res, err := rec.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Recordings {
		if r.ID == info.ID && !containsAny(r.StopReason, "complete") {
			t.Errorf("StopReason = %q, want it to mention the print completed", r.StopReason)
		}
	}
}

// --- delete refuses an active recording ---

func TestRecorder_Delete_RefusesActiveRecording(t *testing.T) {
	shrinkRecorderTimings(t)
	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	const host = "printer-h-host"
	go feed(hub, host, fixture, 0, len(fixture.aus))

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-h",
		Host:      host,
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := rec.Delete(info.ID); err == nil {
		t.Fatal("Delete succeeded on an active recording, want an error")
	}

	stopped, err := rec.Stop(info.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if err := rec.Delete(stopped.ID); err != nil {
		t.Fatalf("Delete after stop: %v", err)
	}
	for _, p := range stopped.Parts {
		if _, err := os.Stat(p.Path); !os.IsNotExist(err) {
			t.Errorf("part file %s still exists after Delete", p.Path)
		}
	}

	res, err := rec.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Recordings {
		if r.ID == stopped.ID {
			t.Fatalf("deleted recording %s still appears in List", stopped.ID)
		}
	}
}

// --- RPC round trip over AF_UNIX in t.TempDir ---

func TestRecordingRPC_RoundTrip(t *testing.T) {
	shrinkRecorderTimings(t)
	fixture := loadClipFixture(t)
	recHub := newFakeRecorderHub()
	const host = "printer-i-host"
	go feed(recHub, host, fixture, 0, len(fixture.aus))

	dir := t.TempDir()
	opts := Options{
		Paths:    PathsIn(dir),
		Hub:      NewHub(&fakeOpener{}),
		Watchdog: newTestWatchdog(newFakeStateSource(), newFakeSender()),
		Recorder: NewRecorder(filepath.Join(dir, "recordings"), recHub, newFakeStateSource(), nil),
	}
	s := openTestServer(t, opts)
	serveInBackground(t, s)

	conn, err := socket.Dial(opts.Paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	ctx := context.Background()

	var startRes RecordingStartResult
	startParams := RecordingStartParams{PrinterID: "printer-i", Host: host, Mode: "video", Until: "stopped"}
	if err := conn.Call(ctx, MethodRecordingStart, startParams, &startRes); err != nil {
		t.Fatalf("recording.start: %v", err)
	}
	if !startRes.Recording.Active {
		t.Fatalf("recording.start result Active = false, want true")
	}
	id := startRes.Recording.ID

	var listRes RecordingListResult
	if err := conn.Call(ctx, MethodRecordingList, RecordingListParams{}, &listRes); err != nil {
		t.Fatalf("recording.list: %v", err)
	}
	found := false
	for _, r := range listRes.Recordings {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("recording.list did not include %s: %+v", id, listRes.Recordings)
	}

	time.Sleep(50 * time.Millisecond)

	var stopRes RecordingStopResult
	if err := conn.Call(ctx, MethodRecordingStop, RecordingStopParams{ID: id}, &stopRes); err != nil {
		t.Fatalf("recording.stop: %v", err)
	}
	if stopRes.Recording.Active {
		t.Fatalf("recording.stop result Active = true, want false")
	}

	var deleteRes RecordingDeleteResult
	if err := conn.Call(ctx, MethodRecordingDelete, RecordingDeleteParams{ID: id}, &deleteRes); err != nil {
		t.Fatalf("recording.delete: %v", err)
	}
	if !deleteRes.OK {
		t.Fatalf("recording.delete OK = false")
	}
}

// An invalid recording.start request (an unknown printer host, empty) is
// rejected as an error, not silently accepted.
func TestRecordingRPC_StartRejectsInvalidRequest(t *testing.T) {
	dir := t.TempDir()
	recHub := newFakeRecorderHub()
	opts := Options{
		Paths:    PathsIn(dir),
		Hub:      NewHub(&fakeOpener{}),
		Watchdog: newTestWatchdog(newFakeStateSource(), newFakeSender()),
		Recorder: NewRecorder(filepath.Join(dir, "recordings"), recHub, newFakeStateSource(), nil),
	}
	s := openTestServer(t, opts)
	serveInBackground(t, s)

	conn, err := socket.Dial(opts.Paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var res RecordingStartResult
	err = conn.Call(context.Background(), MethodRecordingStart, RecordingStartParams{Mode: "video", Until: "stopped"}, &res)
	if err == nil {
		t.Fatal("want an error for a recording.start request with no printer_id/host, got nil")
	}
}

// --- Path traversal (review backlog item 33) ---

// splitRecordingID refuses every shape of a malicious or malformed id
// before either half is ever joined into a filesystem path: literal "..",
// an extra separator (forward or back slash) inside a segment that would
// smuggle a second path component through, more or fewer than two "/"
// separated segments, an absolute path, a drive reference, an empty
// segment, and a lone "printer id" segment of "..".
func TestSplitRecordingID_RejectsPathTraversal(t *testing.T) {
	cases := []string{
		"../etc/passwd",
		"printer/../secret",
		"printer/..",
		"../printer/base",
		"printer/base/extra",
		"/etc/passwd",
		"printer/",
		"/printer/base",
		"printer/base/",
		"..\\..\\etc\\passwd",        // backslash traversal, no forward slash at all
		"C:\\Windows\\System32/base", // drive reference as the printer-id half
		"printer/%2e%2e%2fetc",       // literal percent-encoded text, never decoded
		"printer",                    // no separator at all
		"",                           // empty id
		"printer/base/../../x",       // too many separators
		".",                          // lone dot, no separator
		"printer/.",                  // recording-name half is a lone dot
	}
	for _, id := range cases {
		if _, _, err := splitRecordingID(id); err == nil {
			t.Errorf("splitRecordingID(%q): want error, got nil", id)
		}
	}
}

func TestSplitRecordingID_AcceptsValidID(t *testing.T) {
	printerID, base, err := splitRecordingID("printer-a/20240101T000000Z")
	if err != nil {
		t.Fatalf("splitRecordingID: %v", err)
	}
	if printerID != "printer-a" || base != "20240101T000000Z" {
		t.Fatalf("splitRecordingID = (%q, %q), want (printer-a, 20240101T000000Z)", printerID, base)
	}
}

// validateRecordingIDSegment rejects a segment over 64 characters, a
// Windows reserved device name (with or without an extension, case
// insensitive), and a trailing dot or space (review backlog item 38).
func TestValidateRecordingIDSegment_RejectsItem38Cases(t *testing.T) {
	cases := []string{
		strings.Repeat("a", 65),
		"con", "CON", "Con.mp4", "prn", "aux", "nul",
		"com1", "COM9", "lpt1", "LPT9",
		"printer.",
		"printer ",
	}
	for _, s := range cases {
		if err := validateRecordingIDSegment(s); err == nil {
			t.Errorf("validateRecordingIDSegment(%q): want error, got nil", s)
		}
	}
	// A segment at exactly the 64-character cap, and names that merely look
	// reserved, must still be accepted.
	ok := []string{strings.Repeat("a", 64), "console", "com10", "lpt10"}
	for _, s := range ok {
		if err := validateRecordingIDSegment(s); err != nil {
			t.Errorf("validateRecordingIDSegment(%q): want nil, got %v", s, err)
		}
	}
}

// Recorder.Start refuses a printer_id that is a Windows reserved device
// name or exceeds the length cap, before ever touching disk (review backlog
// item 38).
func TestRecorder_Start_RejectsItem38MaliciousPrinterIDs(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	for _, id := range []string{"con", "PRN.txt", strings.Repeat("a", 65), "printer."} {
		_, err := rec.Start(context.Background(), StartRecordingRequest{
			PrinterID: id, Host: "host", Mode: RecordModeVideo, Until: RecordUntilStopped,
		})
		if err == nil {
			t.Errorf("Start with printer_id %q: want error, got nil", id)
		}
	}
}

// --- Planted symlinks are refused (review backlog item 38) ---

// A recording directory that turns out to be a symlink is refused before
// Delete ever reads a sidecar through it or removes anything.
func TestRecorder_Delete_RefusesSymlinkedPrinterDirectory(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	if err := os.MkdirAll(rec.dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	outsideDir := t.TempDir()
	info := RecordingInfo{ID: "printer-sym/20240101T000000Z", PrinterID: "printer-sym", Mode: RecordModeVideo}
	if err := writeSidecar(outsideDir, "20240101T000000Z", info); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}

	printerDir := filepath.Join(rec.dir, "printer-sym")
	if err := os.Symlink(outsideDir, printerDir); err != nil {
		t.Skipf("os.Symlink not supported in this environment: %v", err)
	}

	if err := rec.Delete("printer-sym/20240101T000000Z"); err == nil {
		t.Fatal("Delete through a symlinked printer directory succeeded, want an error")
	}
}

// A part path that resolves to a symlink is refused before Remove is ever
// called on it, and neither the symlink nor the file it points at is
// touched.
func TestRecorder_Delete_RefusesSymlinkedPartFile(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	dir := filepath.Join(rec.dir, "printer-sym2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	outsideFile := filepath.Join(t.TempDir(), "secret.mp4")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	linkPath := filepath.Join(dir, "20240101T000000Z.mp4")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Skipf("os.Symlink not supported in this environment: %v", err)
	}

	info := RecordingInfo{
		ID: "printer-sym2/20240101T000000Z", PrinterID: "printer-sym2", Mode: RecordModeVideo,
		Parts: []RecordingPart{{Path: linkPath}},
	}
	if err := writeSidecar(dir, "20240101T000000Z", info); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}

	if err := rec.Delete("printer-sym2/20240101T000000Z"); err == nil {
		t.Fatal("Delete with a symlinked part path succeeded, want an error")
	}
	if _, err := os.Lstat(linkPath); err != nil {
		t.Errorf("symlink itself was removed despite the refusal: %v", err)
	}
	data, err := os.ReadFile(outsideFile)
	if err != nil || string(data) != "secret" {
		t.Errorf("outside file altered or missing: data=%q err=%v", data, err)
	}
}

// A timelapse frames directory that turns out to be a symlink is refused
// the same way.
func TestRecorder_Delete_RefusesSymlinkedTimelapseFramesDirectory(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	dir := filepath.Join(rec.dir, "printer-sym3")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "frame_00001.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	framesLink := filepath.Join(dir, "20240101T000000Z-timelapse")
	if err := os.Symlink(outsideDir, framesLink); err != nil {
		t.Skipf("os.Symlink not supported in this environment: %v", err)
	}

	info := RecordingInfo{
		ID: "printer-sym3/20240101T000000Z-timelapse", PrinterID: "printer-sym3", Mode: RecordModeTimelapse,
	}
	if err := writeSidecar(dir, "20240101T000000Z-timelapse", info); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}

	if err := rec.Delete("printer-sym3/20240101T000000Z-timelapse"); err == nil {
		t.Fatal("Delete with a symlinked timelapse frames directory succeeded, want an error")
	}
	if _, err := os.Lstat(filepath.Join(outsideDir, "frame_00001.jpg")); err != nil {
		t.Errorf("file behind the symlink was removed despite the refusal: %v", err)
	}
}

// Delete refuses every malicious id outright, before ever touching disk,
// and never reaches a file outside the recordings directory.
func TestRecorder_Delete_RejectsPathTraversalIDs(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)

	// A real file the recordings directory must never be able to reach,
	// placed as a sibling of the recordings root.
	outside := filepath.Join(filepath.Dir(rec.dir), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	malicious := []string{
		"../outside-secret",
		"printer-x/../../outside-secret",
		"printer-x/..",
		"..",
		"/outside-secret",
		"printer-x/base/extra",
		"printer-x/..\\..\\outside-secret",
	}
	for _, id := range malicious {
		if err := rec.Delete(id); err == nil {
			t.Errorf("Delete(%q): want error, got nil", id)
		}
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("outside file is gone or inaccessible: %v", err)
	}
	if string(data) != "secret" {
		t.Fatalf("outside file contents changed: %q", data)
	}
}

// Stop also refuses a malformed id, even though it never builds a
// filesystem path from it (it only ever looks it up in the active map).
func TestRecorder_Stop_RejectsMalformedID(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	if _, err := rec.Stop("../etc/passwd"); err == nil {
		t.Fatal("Stop(\"../etc/passwd\"): want error, got nil")
	}
}

// --- Sidecar atomic writes (review backlog item 34) ---

// writeSidecar never leaves a leftover temp file behind, and a later
// overwrite fully replaces the previous content (never merges or
// truncates), matching domain.WriteFileAtomic's crash-safety contract.
func TestWriteSidecar_AtomicNoLeftoverTempFile(t *testing.T) {
	dir := t.TempDir()
	info := RecordingInfo{ID: "printer-z/20240101T000000Z", PrinterID: "printer-z"}
	if err := writeSidecar(dir, "20240101T000000Z", info); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	sawSidecar := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file %s after writeSidecar", e.Name())
		}
		if e.Name() == "20240101T000000Z.json" {
			sawSidecar = true
		}
	}
	if !sawSidecar {
		t.Fatal("sidecar file was not created")
	}

	info2 := info
	info2.StopReason = "stopped: reached the maximum recording duration (12h0m0s)"
	if err := writeSidecar(dir, "20240101T000000Z", info2); err != nil {
		t.Fatalf("writeSidecar (overwrite): %v", err)
	}
	got, err := readSidecar(dir, "20240101T000000Z")
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if got.StopReason != info2.StopReason {
		t.Fatalf("StopReason = %q, want %q (overwrite must fully replace, not merge)", got.StopReason, info2.StopReason)
	}
}

// List tolerates a missing sidecar (the instant during rotation between a
// part file appearing and its sidecar write landing): it must skip such an
// entry, never error or panic.
func TestRecorder_List_ToleratesMissingSidecar(t *testing.T) {
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	printerDir := filepath.Join(rec.dir, "printer-w")
	if err := os.MkdirAll(printerDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(printerDir, "20240101T000000Z.mp4"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write part file: %v", err)
	}

	res, err := rec.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range res.Recordings {
		if r.PrinterID == "printer-w" {
			t.Fatalf("List returned a recording with no sidecar: %+v", r)
		}
	}
}

// --- ErrNoKeyframe resumes on the same writer, no leaked part (review backlog item 35) ---
// --- and retries its PLI until a keyframe arrives (review backlog item 44) ---

// After ErrNoKeyframe, the recorder must keep the same open writer/part,
// resend a keyframe request (PLI) every recorderKeyframeRetryInterval for
// as long as one is still owed (review backlog item 44: a single lost or
// ignored PLI must not leave the rest of the gap with nothing further
// sent), stop asking again once a keyframe resumes writing, and resume on
// that exact same writer - never close the part or open a new one (the
// pre-item-35-fix bug: it silently opened a second part without closing the
// first, leaking its file handle and dropping it from the sidecar).
// TestRecorder_ErrNoKeyframe_ResumesOnSameWriter_NoLeakedPart confirms
// run's ErrNoKeyframe handling (review backlog item 35: the writer stays
// open and resumes on the very next keyframe, never leaking a part) and,
// since review backlog item 51 removed run's own per-consumer PLI retry
// loop (item 44's earlier fix), that run asks the hub for a keyframe
// exactly once per gap - repeated retrying while none arrives is now the
// hub's own responsibility (internal/daemon/camera_hub_test.go covers its
// coalescing and automatic recovery retry directly), not something this
// fake, uncoalescing hub double is meant to exercise.
func TestRecorder_ErrNoKeyframe_ResumesOnSameWriter_NoLeakedPart(t *testing.T) {
	shrinkRecorderTimings(t)
	origWait := recorderMaxKeyframeWait
	recorderMaxKeyframeWait = time.Millisecond // ErrNoKeyframe on the very next non-keyframe frame
	t.Cleanup(func() { recorderMaxKeyframeWait = origWait })

	fixture := loadClipFixture(t)
	hub := newFakeRecorderHub()
	rec := newTestRecorder(t, hub, nil)
	const host = "printer-j-host"

	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(0)})

	info, err := rec.Start(context.Background(), StartRecordingRequest{
		PrinterID: "printer-j",
		Host:      host,
		Mode:      RecordModeVideo,
		Until:     RecordUntilStopped,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A non-keyframe access unit past the (shrunk) MaxKeyframeWait triggers
	// ErrNoKeyframe, which must ask the hub for a fresh keyframe exactly
	// once.
	hub.send(host, camera.AccessUnit{Data: fixture.aus[1], Keyframe: false, PTS: ptsAt(1)})
	waitFor(t, 2*time.Second, func() bool { return hub.keyframeRequestCount(host) >= 1 })
	time.Sleep(20 * time.Millisecond)
	if got := hub.keyframeRequestCount(host); got != 1 {
		t.Fatalf("RequestKeyframe called %d times for one gap, want exactly 1 (run must not retry on its own any more)", got)
	}

	// The current part must still be open and untouched throughout the gap:
	// ar.parts (closed parts) must stay empty.
	rec.mu.Lock()
	ar := rec.active[info.ID]
	rec.mu.Unlock()
	ar.mu.Lock()
	partsClosedDuringGap := len(ar.parts)
	ar.mu.Unlock()
	if partsClosedDuringGap != 0 {
		t.Fatalf("parts closed during the keyframe gap = %d, want 0 (the writer must stay open, never replaced)", partsClosedDuringGap)
	}

	// A fresh keyframe access unit resumes writing on the same writer.
	hub.send(host, camera.AccessUnit{Data: fixture.aus[0], Keyframe: true, PTS: ptsAt(20)})
	waitFor(t, 2*time.Second, func() bool { return fileSize(info.Parts[0].Path) > 0 })

	// Once a keyframe has resumed writing, no further request is made.
	time.Sleep(50 * time.Millisecond)
	if got := hub.keyframeRequestCount(host); got != 1 {
		t.Errorf("RequestKeyframe kept firing after a keyframe resumed writing: got %d, want 1 (unchanged)", got)
	}

	stopped, err := rec.Stop(info.ID)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(stopped.Parts) != 1 {
		t.Fatalf("got %d parts, want exactly 1 (ErrNoKeyframe must never open a new part or leak the writer)", len(stopped.Parts))
	}
	_, totalSamples := parseRecording(t, stopped.Parts[0].Path)
	if totalSamples == 0 {
		t.Fatal("parsed recording has 0 samples: the writer never actually resumed")
	}

	// The single part is the one recorded in the sidecar, and
	// delete_recording removes it completely.
	sidecar := filepath.Join(filepath.Dir(stopped.Parts[0].Path), filepath.Base(stopped.ID)+".json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	if err := rec.Delete(stopped.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(stopped.Parts[0].Path); !os.IsNotExist(err) {
		t.Error("part file still exists after Delete")
	}
}

// shrinkRecorderTimings shrinks every Recorder package-level timing knob to
// test-friendly durations, restoring the originals on test cleanup, so
// tests never wait out the real multi-second/hour production defaults.
func shrinkRecorderTimings(t *testing.T) {
	t.Helper()
	origPoll := recorderStatePollInterval
	origDisk := recorderDiskCheckInterval
	origKeyframe := recorderKeyframeWait
	origFragment := recorderFragmentDuration
	recorderStatePollInterval = 20 * time.Millisecond
	recorderDiskCheckInterval = 50 * time.Millisecond
	recorderKeyframeWait = 5 * time.Second
	recorderFragmentDuration = time.Millisecond
	t.Cleanup(func() {
		recorderStatePollInterval = origPoll
		recorderDiskCheckInterval = origDisk
		recorderKeyframeWait = origKeyframe
		recorderFragmentDuration = origFragment
	})
}
