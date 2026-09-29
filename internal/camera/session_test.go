package camera

import (
	"context"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/camera/decode"
)

// testTimeout returns floor, unless the test binary's own -timeout deadline
// leaves more room than that once a safety margin is set aside for cleanup,
// in which case it returns that larger, load-tolerant budget instead. This
// keeps a test's internal timeout a fixed floor on a fast, idle machine
// (dev_docs/review-backlog.md item 26: TestSessionKeyframeCaptureAndCodecInfo
// was seen to time out at a fixed 5 s under heavy parallel load, though it
// was not reproducible with -count=5 on an idle machine) while still scaling
// up under `go test -timeout`, without touching any production timeout
// budget (session.go's signalingTimeout/iceConnectTimeout stay exactly as
// they are).
func testTimeout(t *testing.T, floor time.Duration) time.Duration {
	t.Helper()
	const cleanupMargin = 5 * time.Second
	dl, ok := t.Deadline()
	if !ok {
		return floor
	}
	if remaining := time.Until(dl) - cleanupMargin; remaining > floor {
		return remaining
	}
	return floor
}

// nextAU returns whatever access unit session delivers next. Used to give
// decode.DecodeKeyframe the "start of the following picture" boundary it
// needs to recognize a just-captured keyframe access unit as complete (see
// decode's own doc comment, and snapshot.go's waitForNextAccessUnit).
func nextAU(t *testing.T, ctx context.Context, session *Session) AccessUnit {
	t.Helper()
	select {
	case au, ok := <-session.AccessUnits():
		if !ok {
			t.Fatal("access unit stream ended before a trailing access unit arrived")
		}
		return au
	case <-ctx.Done():
		t.Fatalf("timed out waiting for a trailing access unit: %v", ctx.Err())
	}
	panic("unreachable")
}

// waitForAU drains session's access units until pred returns true for one
// of them, ctx is done, or the stream ends.
func waitForAU(t *testing.T, ctx context.Context, session *Session, pred func(AccessUnit) bool) AccessUnit {
	t.Helper()
	for {
		select {
		case au, ok := <-session.AccessUnits():
			if !ok {
				t.Fatal("access unit stream ended before the expected access unit arrived")
			}
			if pred(au) {
				return au
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for an access unit: %v", ctx.Err())
		}
	}
}

func TestOpenBothAnswerEncodings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		encoding answerEncoding
	}{
		{"raw SDP answer", answerRawSDP},
		{"base64-json answer", answerBase64JSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := startFakePrinter(t, fakePrinterOptions{encoding: tc.encoding, autoRepeatKeyframes: true})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			session, err := Open(ctx, host)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer session.Close()

			au := waitForAU(t, ctx, session, func(au AccessUnit) bool { return au.Keyframe })
			if !au.Keyframe {
				t.Fatal("first captured access unit is not a keyframe")
			}
		})
	}
}

func TestSessionKeyframeCaptureAndCodecInfo(t *testing.T) {
	host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: true})

	// This test's ctx bounds not just Open and the access-unit waits but
	// also decode.New and DecodeKeyframe below (the wasm H.264 decoder),
	// so a fixed 5 s floor is load-tolerant: testTimeout stretches it to
	// use most of the test binary's own -timeout budget instead, whenever
	// that leaves more room (item 26).
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout(t, 5*time.Second))
	defer cancel()

	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	au := waitForAU(t, ctx, session, func(au AccessUnit) bool { return au.Keyframe })

	info := session.CodecInfo()
	// The fake printer's SDP answer advertises profile-level-id=42e01f
	// (Baseline, see fakeprinter_test.go), matching the real K2's own SDP
	// quirk (references/analysis/04-creality-ws-camera.md section 3), but
	// the sample clip's actual SPS is Main profile. CodecInfo must report
	// what the SPS says, not what the SDP claims.
	if info.ProfileLevelID != "4d001f" {
		t.Errorf("CodecInfo.ProfileLevelID = %q, want %q (from the SPS, not the SDP's 42e01f)", info.ProfileLevelID, "4d001f")
	}
	if info.Width != 1280 || info.Height != 720 {
		t.Errorf("CodecInfo dimensions = %dx%d, want 1280x720", info.Width, info.Height)
	}

	sps, pps := session.ParameterSets()
	if len(sps) == 0 || len(pps) == 0 {
		t.Fatal("ParameterSets returned empty SPS and/or PPS after a keyframe was seen")
	}
	if nalType(sps) != nalTypeSPS {
		t.Errorf("ParameterSets sps has NAL type %d, want SPS (%d)", nalType(sps), nalTypeSPS)
	}
	if nalType(pps) != nalTypePPS {
		t.Errorf("ParameterSets pps has NAL type %d, want PPS (%d)", nalType(pps), nalTypePPS)
	}

	if au.CapturedAt.IsZero() {
		t.Error("CapturedAt was not set")
	}

	trailer := nextAU(t, ctx, session)
	if trailer.PTS <= au.PTS {
		t.Errorf("PTS did not increase from the keyframe (%d) to the next access unit (%d)", au.PTS, trailer.PTS)
	}

	// decode.DecodeKeyframe (via OpenH264) only recognizes a picture as
	// complete once it sees the start of the following one; Session
	// delivers one access unit per picture, so append the trailer already
	// fetched above (it works just as well for this as any other access
	// unit would).
	buf := append(append([]byte(nil), au.Data...), trailer.Data...)

	dec, err := decode.New(ctx)
	if err != nil {
		t.Fatalf("decode.New: %v", err)
	}
	defer dec.Close(ctx)

	img, err := dec.DecodeKeyframe(ctx, buf)
	if err != nil {
		t.Fatalf("DecodeKeyframe on captured keyframe: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != 1280 || bounds.Dy() != 720 {
		t.Fatalf("decoded image is %dx%d, want 1280x720", bounds.Dx(), bounds.Dy())
	}
}

// TestSessionRequestKeyframeOutOfBandSPSPPS drives the fake printer in
// PLI-only mode: it sends one keyframe on connect and then only sends
// another when asked, deliberately without repeating that GOP's SPS/PPS.
// This proves both that RequestKeyframe actually causes a fresh keyframe,
// and that Session reconstructs a self-contained access unit from the
// SPS/PPS it cached out of band.
func TestSessionRequestKeyframeOutOfBandSPSPPS(t *testing.T) {
	host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: false})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	first := waitForAU(t, ctx, session, func(au AccessUnit) bool { return au.Keyframe })

	if err := session.RequestKeyframe(); err != nil {
		t.Fatalf("RequestKeyframe: %v", err)
	}

	second := waitForAU(t, ctx, session, func(au AccessUnit) bool {
		return au.Keyframe && string(au.Data) != string(first.Data)
	})

	units := splitAnnexB(second.Data)
	if len(units) < 3 {
		t.Fatalf("reconstructed keyframe has %d NAL units, want at least 3 (SPS, PPS, IDR)", len(units))
	}
	if nalType(units[0]) != nalTypeSPS {
		t.Errorf("reconstructed keyframe's first NAL unit type = %d, want SPS (%d)", nalType(units[0]), nalTypeSPS)
	}
	if nalType(units[1]) != nalTypePPS {
		t.Errorf("reconstructed keyframe's second NAL unit type = %d, want PPS (%d)", nalType(units[1]), nalTypePPS)
	}

	trailer := nextAU(t, ctx, session)
	buf := append(append([]byte(nil), second.Data...), trailer.Data...)

	dec, err := decode.New(ctx)
	if err != nil {
		t.Fatalf("decode.New: %v", err)
	}
	defer dec.Close(ctx)

	img, err := dec.DecodeKeyframe(ctx, buf)
	if err != nil {
		t.Fatalf("DecodeKeyframe on out-of-band-reconstructed keyframe: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 1280 || b.Dy() != 720 {
		t.Fatalf("decoded image is %dx%d, want 1280x720", b.Dx(), b.Dy())
	}
}

func TestSessionCloseIsIdempotentAndLeavesNoGoroutine(t *testing.T) {
	host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	waitForAU(t, ctx, session, func(au AccessUnit) bool { return au.Keyframe })

	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s (readTrack goroutine leak?)")
	}

	// A second Close must be a harmless no-op.
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// The access unit channel must end up closed.
	drained := false
	for i := 0; i < 1000; i++ {
		select {
		case _, ok := <-session.AccessUnits():
			if !ok {
				drained = true
			}
		default:
		}
		if drained {
			break
		}
	}
	if !drained {
		t.Fatal("AccessUnits channel was never observed closed after Close")
	}
}

func TestOpenContextAlreadyCancelled(t *testing.T) {
	host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: true})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := Open(ctx, host)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Open with an already cancelled context: want error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Open with a cancelled context took %s, want a prompt failure", elapsed)
	}
}

func TestRequestKeyframeBeforeSessionExists(t *testing.T) {
	s := &Session{}
	if err := s.RequestKeyframe(); err == nil {
		t.Fatal("RequestKeyframe on a session with no track yet: want error, got nil")
	}
}
