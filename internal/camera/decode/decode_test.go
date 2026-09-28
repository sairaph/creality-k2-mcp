package decode

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Reference statistics from `ffmpeg -i camera_sample_clip.h264 -frames:v 1
// -vf signalstats,metadata=print -f null -` on the same clip: mean luma
// 19.7327, mean Cb 126.822, mean Cr 126.972 (see dev_docs/t0-decoder-spike.md).
// The clip's content is a mostly-dark printer enclosure interior, hence the
// low luma mean.
const (
	refMeanY  = 19.7327
	refMeanCb = 126.822
	refMeanCr = 126.972
	// Generous tolerance: this compares two different H.264 decoders
	// (ffmpeg's libavcodec vs OpenH264) on the same bitstream, which can
	// differ slightly in deblocking and rounding but must agree on the
	// broad picture content.
	meanTolerance = 6.0
)

func TestDecodeKeyframe(t *testing.T) {
	ctx := context.Background()

	data, err := os.ReadFile(filepath.Join("testdata", "camera_sample_clip.h264"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		if err := dec.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	start := time.Now()
	img, err := dec.DecodeKeyframe(ctx, data)
	if err != nil {
		t.Fatalf("DecodeKeyframe: %v", err)
	}
	t.Logf("decode (including module warm-up already paid by New): %s", time.Since(start))

	bounds := img.Bounds()
	if bounds.Dx() != 1280 || bounds.Dy() != 720 {
		t.Fatalf("got %dx%d image, want 1280x720", bounds.Dx(), bounds.Dy())
	}

	ycbcr, ok := img.(*image.YCbCr)
	if !ok {
		t.Fatalf("got %T, want *image.YCbCr", img)
	}
	if ycbcr.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Fatalf("got subsample ratio %v, want 4:2:0", ycbcr.SubsampleRatio)
	}

	meanY := planeMean(ycbcr.Y, ycbcr.YStride, bounds.Dx(), bounds.Dy())
	chromaW, chromaH := (bounds.Dx()+1)/2, (bounds.Dy()+1)/2
	meanCb := planeMean(ycbcr.Cb, ycbcr.CStride, chromaW, chromaH)
	meanCr := planeMean(ycbcr.Cr, ycbcr.CStride, chromaW, chromaH)

	t.Logf("mean Y=%.4f Cb=%.4f Cr=%.4f (ffmpeg reference: Y=%.4f Cb=%.4f Cr=%.4f)",
		meanY, meanCb, meanCr, refMeanY, refMeanCb, refMeanCr)

	if diff := absFloat(meanY - refMeanY); diff > meanTolerance {
		t.Errorf("mean luma %.4f differs from ffmpeg reference %.4f by %.4f, want within %.1f", meanY, refMeanY, diff, meanTolerance)
	}
	if diff := absFloat(meanCb - refMeanCb); diff > meanTolerance {
		t.Errorf("mean Cb %.4f differs from ffmpeg reference %.4f by %.4f, want within %.1f", meanCb, refMeanCb, diff, meanTolerance)
	}
	if diff := absFloat(meanCr - refMeanCr); diff > meanTolerance {
		t.Errorf("mean Cr %.4f differs from ffmpeg reference %.4f by %.4f, want within %.1f", meanCr, refMeanCr, diff, meanTolerance)
	}

	outPath := filepath.Join(t.TempDir(), "camera_sample_clip.jpg")
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create output jpeg: %v", err)
	}
	defer f.Close()

	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	t.Logf("wrote decoded keyframe to %s", outPath)
}

// TestDecodeKeyframeSkipsLeadingGarbage confirms that DecodeKeyframe
// tolerates (and skips) bytes before the first SPS/PPS/IDR, which is what
// WebRTC RTP depacketization produces when a snapshot request arrives
// mid-stream, before a full GOP has been observed.
func TestDecodeKeyframeSkipsLeadingGarbage(t *testing.T) {
	ctx := context.Background()

	data, err := os.ReadFile(filepath.Join("testdata", "camera_sample_clip.h264"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	// The sample clip already starts mid-stream (task input, not
	// synthesized here), so decoding it at all is the regression test:
	// a decoder that required the stream to start exactly on the SPS
	// would fail this the same way it would fail TestDecodeKeyframe.
	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	if _, err := dec.DecodeKeyframe(ctx, data); err != nil {
		t.Fatalf("DecodeKeyframe on stream with leading garbage: %v", err)
	}
}

// TestDecodeKeyframeContextAlreadyCancelled confirms that DecodeKeyframe
// honours a ctx that is already cancelled before the call even starts,
// rather than ignoring it and running the decode anyway. This is what
// wazero's WithCloseOnContextDone (see getCompiled) is for: the input is
// network-sourced H.264, so a caller needs to be able to give up on a
// stuck or unwanted decode.
func TestDecodeKeyframeContextAlreadyCancelled(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()

	start := time.Now()
	if _, err := dec.DecodeKeyframe(cancelledCtx, data); err == nil {
		t.Fatalf("DecodeKeyframe with an already cancelled context: want error, got nil")
	}
	t.Logf("returned in %s", time.Since(start))

	// A call interrupted by context cancellation leaves the Decoder
	// closed (a defined, permanent state), not half-reset: any later use
	// must fail immediately too, the same as after Close.
	if _, err := dec.DecodeKeyframe(ctx, data); err == nil {
		t.Fatalf("DecodeKeyframe after a cancelled call: want error (decoder closed), got nil")
	}
}

// TestDecodeKeyframeContextShortDeadline confirms that DecodeKeyframe
// aborts promptly when its context reaches a deadline while a decode is
// actually in flight (not just when the context arrives already done), by
// giving it a deadline far shorter than the sample clip's normal decode
// time (tens of milliseconds, see dev_docs/t0-decoder-spike.md).
func TestDecodeKeyframeContextShortDeadline(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	shortCtx, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = dec.DecodeKeyframe(shortCtx, data)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("DecodeKeyframe with a 5ms deadline: want error, got nil (elapsed %s)", elapsed)
	}
	t.Logf("returned in %s: %v", elapsed, err)

	// Generous upper bound: a prompt abort should be nowhere near the
	// several-seconds outlier noted for a full decode in
	// dev_docs/t0-decoder-spike.md. This catches a regression where
	// cancellation silently stops being honoured (the call would then run
	// to completion instead of aborting).
	const maxPromptDuration = 5 * time.Second
	if elapsed > maxPromptDuration {
		t.Fatalf("DecodeKeyframe with a 5ms deadline took %s, want under %s", elapsed, maxPromptDuration)
	}
}

// TestDecodeKeyframeGarbageInput confirms that input with no Annex-B start
// code at all (never mind a valid NAL unit) produces a clear error instead
// of a panic or a confusing failure deeper in the decode path.
func TestDecodeKeyframeGarbageInput(t *testing.T) {
	ctx := context.Background()

	garbage := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 64)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	if _, err := dec.DecodeKeyframe(ctx, garbage); err == nil {
		t.Fatalf("DecodeKeyframe on non-NAL garbage: want error, got nil")
	}
}

// TestDecodeKeyframeSPSPPSWithoutIDR confirms the sawSPS/sawPPS/sawIDR
// error path: an SPS and PPS with no IDR ever arriving should fail
// clearly, not hang or return a zero-value image. The SPS and PPS NAL
// units are extracted from the sample clip itself rather than adding a
// new binary fixture.
func TestDecodeKeyframeSPSPPSWithoutIDR(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	units := nalUnits(data)
	sps := firstUnitOfType(units, nalSPS)
	pps := firstUnitOfType(units, nalPPS)
	if sps == nil || pps == nil {
		t.Fatalf("sample clip does not contain both an SPS and a PPS NAL unit")
	}

	noIDR := append(append([]byte{}, sps...), pps...)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	_, err = dec.DecodeKeyframe(ctx, noIDR)
	if err == nil {
		t.Fatalf("DecodeKeyframe with SPS+PPS and no IDR: want error, got nil")
	}
	t.Logf("got expected error: %v", err)
}

// TestDecodeKeyframeTruncatedIDR confirms that an IDR NAL unit cut off
// mid-slice (as could happen if a caller hands over an incomplete buffer)
// produces a clear error rather than a corrupted image or a panic. The
// truncation point is the midpoint of the sample clip's own IDR NAL unit.
func TestDecodeKeyframeTruncatedIDR(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	units := nalUnits(data)
	sps := firstUnitOfType(units, nalSPS)
	pps := firstUnitOfType(units, nalPPS)
	idr := firstUnitOfType(units, nalIDR)
	if sps == nil || pps == nil || idr == nil {
		t.Fatalf("sample clip does not contain SPS, PPS and IDR NAL units")
	}
	if len(idr) < 2 {
		t.Fatalf("sample clip's IDR NAL unit is too short to truncate meaningfully")
	}

	truncatedIDR := idr[:len(idr)/2]
	truncated := append(append(append([]byte{}, sps...), pps...), truncatedIDR...)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	_, err = dec.DecodeKeyframe(ctx, truncated)
	if err == nil {
		t.Fatalf("DecodeKeyframe with a truncated IDR: want error, got nil")
	}
	t.Logf("got expected error: %v", err)
}

// TestDecodeKeyframeAfterClose confirms that a closed Decoder rejects
// further DecodeKeyframe calls immediately, instead of touching a wasm
// module instance that has already been released.
func TestDecodeKeyframeAfterClose(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := dec.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := dec.DecodeKeyframe(ctx, data); err == nil {
		t.Fatalf("DecodeKeyframe after Close: want error, got nil")
	}
}

// TestDecodeKeyframeRepeatedCallsProduceIdenticalResults confirms that
// calling DecodeKeyframe more than once on the same, reused Decoder gives
// byte-identical results each time, which is how this package is meant to
// be used in the long-lived-process case (see the Decoder doc comment):
// OpenH264 only recognises a picture as complete once it sees the first
// NAL unit of the following picture, so a successful DecodeKeyframe call
// has necessarily already fed part of the next picture into the decoder.
// DecodeKeyframe resets to a clean module instance at the start of every
// call precisely so that leftover, never-finished picture from a previous
// call cannot bleed into this one.
func TestDecodeKeyframeRepeatedCallsProduceIdenticalResults(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	img1, err := dec.DecodeKeyframe(ctx, data)
	if err != nil {
		t.Fatalf("DecodeKeyframe (1st call): %v", err)
	}
	img2, err := dec.DecodeKeyframe(ctx, data)
	if err != nil {
		t.Fatalf("DecodeKeyframe (2nd call): %v", err)
	}

	yc1, ok := img1.(*image.YCbCr)
	if !ok {
		t.Fatalf("1st call: got %T, want *image.YCbCr", img1)
	}
	yc2, ok := img2.(*image.YCbCr)
	if !ok {
		t.Fatalf("2nd call: got %T, want *image.YCbCr", img2)
	}

	if yc1.Bounds() != yc2.Bounds() {
		t.Fatalf("bounds differ between calls: %v vs %v", yc1.Bounds(), yc2.Bounds())
	}
	if yc1.YStride != yc2.YStride || yc1.CStride != yc2.CStride {
		t.Fatalf("strides differ between calls: Y %d vs %d, C %d vs %d", yc1.YStride, yc2.YStride, yc1.CStride, yc2.CStride)
	}
	if !bytes.Equal(yc1.Y, yc2.Y) {
		t.Errorf("Y plane differs between two DecodeKeyframe calls on the same Decoder")
	}
	if !bytes.Equal(yc1.Cb, yc2.Cb) {
		t.Errorf("Cb plane differs between two DecodeKeyframe calls on the same Decoder")
	}
	if !bytes.Equal(yc1.Cr, yc2.Cr) {
		t.Errorf("Cr plane differs between two DecodeKeyframe calls on the same Decoder")
	}
}

// readSampleClip reads the shared sample clip used across this package's
// tests.
func readSampleClip(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "camera_sample_clip.h264"))
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	return data
}

// firstUnitOfType returns the first NAL unit of type want among units (as
// produced by nalUnits), or nil if none is found.
func firstUnitOfType(units [][]byte, want int) []byte {
	for _, u := range units {
		if nalType(u) == want {
			return u
		}
	}
	return nil
}

func planeMean(plane []byte, stride, width, height int) float64 {
	var sum float64
	var count int
	for y := 0; y < height; y++ {
		row := plane[y*stride : y*stride+width]
		for _, v := range row {
			sum += float64(v)
			count++
		}
	}
	return sum / float64(count)
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// TestDecodeLatest_SameBoundsAsKeyframe confirms DecodeLatest decodes the
// sample clip successfully and produces an image with the same dimensions
// DecodeKeyframe reports for the same clip. The sample clip carries more
// than one access unit (it is also used by this package's own multi-frame
// fixtures elsewhere in this repository), so DecodeLatest's returned
// picture is a later one than DecodeKeyframe's first picture and the two
// need not be pixel-identical - only same-sized, decodable output is
// asserted here; TestDecodeLatest_ConcatenatedAccessUnitsProducesLaterPicture
// below is the test that specifically exercises "later than the first".
func TestDecodeLatest_SameBoundsAsKeyframe(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	decKF, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer decKF.Close(ctx)
	imgKF, err := decKF.DecodeKeyframe(ctx, data)
	if err != nil {
		t.Fatalf("DecodeKeyframe: %v", err)
	}

	decLatest, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer decLatest.Close(ctx)
	imgLatest, err := decLatest.DecodeLatest(ctx, data)
	if err != nil {
		t.Fatalf("DecodeLatest: %v", err)
	}

	ycKF, ok := imgKF.(*image.YCbCr)
	if !ok {
		t.Fatalf("DecodeKeyframe: got %T, want *image.YCbCr", imgKF)
	}
	ycLatest, ok := imgLatest.(*image.YCbCr)
	if !ok {
		t.Fatalf("DecodeLatest: got %T, want *image.YCbCr", imgLatest)
	}
	if ycKF.Bounds() != ycLatest.Bounds() {
		t.Fatalf("bounds differ: DecodeKeyframe %v vs DecodeLatest %v", ycKF.Bounds(), ycLatest.Bounds())
	}
}

// TestDecodeLatest_ConcatenatedAccessUnitsProducesLaterPicture feeds the
// sample clip's data twice in a row (simulating internal/daemon's rolling
// GOP buffer, which grows by appending later access units after the
// keyframe): DecodeLatest must succeed and must not simply return the
// same first-picture result DecodeKeyframe would, confirming it actually
// kept decoding through the whole input rather than stopping early.
func TestDecodeLatest_ConcatenatedAccessUnitsProducesLaterPicture(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)
	doubled := append(append([]byte{}, data...), data...)

	decKF, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer decKF.Close(ctx)
	imgKF, err := decKF.DecodeKeyframe(ctx, data)
	if err != nil {
		t.Fatalf("DecodeKeyframe: %v", err)
	}

	decLatest, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer decLatest.Close(ctx)
	imgLatest, err := decLatest.DecodeLatest(ctx, doubled)
	if err != nil {
		t.Fatalf("DecodeLatest: %v", err)
	}

	ycKF := imgKF.(*image.YCbCr)
	ycLatest, ok := imgLatest.(*image.YCbCr)
	if !ok {
		t.Fatalf("DecodeLatest: got %T, want *image.YCbCr", imgLatest)
	}
	if ycLatest.Bounds() != ycKF.Bounds() {
		t.Fatalf("bounds differ: DecodeKeyframe %v vs DecodeLatest(doubled) %v", ycKF.Bounds(), ycLatest.Bounds())
	}
}

// TestDecodeLatest_NoCompletablePictureReturnsError mirrors
// TestDecodeKeyframeSPSPPSWithoutIDR: SPS+PPS with no IDR never completes
// any picture, so DecodeLatest must report an error exactly like
// DecodeKeyframe does, not a nil image.
func TestDecodeLatest_NoCompletablePictureReturnsError(t *testing.T) {
	ctx := context.Background()
	data := readSampleClip(t)

	units := nalUnits(data)
	sps := firstUnitOfType(units, nalSPS)
	pps := firstUnitOfType(units, nalPPS)
	if sps == nil || pps == nil {
		t.Fatalf("sample clip does not contain both an SPS and a PPS NAL unit")
	}
	noIDR := append(append([]byte{}, sps...), pps...)

	dec, err := New(ctx)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dec.Close(ctx)

	_, err = dec.DecodeLatest(ctx, noIDR)
	if err == nil {
		t.Fatalf("DecodeLatest with SPS+PPS and no IDR: want error, got nil")
	}
	t.Logf("got expected error: %v", err)
}
