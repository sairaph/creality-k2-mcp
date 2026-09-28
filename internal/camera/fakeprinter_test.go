package camera

// This file implements a fake K2 camera used by every test in this
// package: an httptest.Server standing in for the printer's
// /call/webrtc_local signaling endpoint, backed by a real pion
// PeerConnection that answers, streams real H.264 (from
// decode/testdata/camera_sample_clip.h264, packetized as RTP) and honors
// RTCP PLI, exactly like the real printer does (see
// references/analysis/04-creality-ws-camera.md section 3 and the
// pion-based proof of concept it documents). No code from
// references/ha_creality_ws is used; this only exercises the wire
// protocol described in that analysis.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// answerEncoding selects which of the two encodings
// (references/analysis/04-creality-ws-camera.md section 3) the fake
// printer's signaling server replies with.
type answerEncoding int

const (
	answerRawSDP answerEncoding = iota
	answerBase64JSON
)

// testFrameInterval paces the fake printer's RTP output at roughly the
// live frame rate observed against the real printer (section 3: 15-20
// fps).
const testFrameInterval = time.Second / 15

// testNAL is one NAL unit (no Annex-B start code, matching
// h264reader.NAL.Data) from the sample clip, tagged with whether it is an
// SPS or PPS.
type testNAL struct {
	data     []byte
	spsOrPPS bool
}

// fakePrinterOptions configures startFakePrinter.
type fakePrinterOptions struct {
	encoding answerEncoding

	// autoRepeatKeyframes, if true, keeps looping the whole clip
	// (producing a fresh keyframe roughly once per loop, on its own,
	// without needing a PLI). If false, the fake printer sends the first
	// keyframe once and then idles on non-keyframe filler until it
	// receives an RTCP PLI, at which point it sends the next GOP's IDR
	// (stripped per pliResendComplete below) and idles again. This is what
	// session_test.go uses to prove RequestKeyframe actually causes a
	// fresh keyframe, and that Session reconstructs it from out-of-band
	// SPS/PPS.
	autoRepeatKeyframes bool

	// useStapA, when true, lets the RTP payloader aggregate small NAL
	// units (SPS, PPS) into a single STAP-A packet ahead of the IDR's own
	// FU-A fragments, matching how the real printer is free to packetize
	// under packetization-mode=1 (references/analysis/04-creality-ws-camera.md
	// section 3's SDP places no restriction on it). When false (every
	// existing test predating this option), the payloader is forced to
	// send one NAL unit per RTP packet (DisableStapA), which is simpler to
	// reason about for tests that deliberately strip SPS/PPS to exercise
	// Session's cache-based reconstruction path instead of in-band
	// delivery.
	useStapA bool

	// firstGOPMissingParams, when true, strips SPS/PPS from the very first
	// GOP this fake printer ever sends on a fresh connection, simulating
	// dev_docs/t11e-soak-report.md item 2's live observation: a session's
	// first-ever IDR sometimes arrives with no SPS/PPS at all, before
	// Session has cached anything to complete it from. Combined with
	// pliResendComplete, this reproduces "IDR without SPS/PPS until a
	// later complete GOP arrives".
	firstGOPMissingParams bool

	// pliResendComplete controls what a PLI-triggered resend (the idle
	// loop below) sends: the full GOP, SPS/PPS included, when true, or
	// (the long-standing default, false) that GOP with its leading
	// SPS/PPS stripped, forcing Session's out-of-band cache-prepend path.
	pliResendComplete bool

	// ignorePLICount, when greater than 0, makes the fake printer's idle
	// loop silently ignore this many received PLI signals before finally
	// acting on one, simulating dev_docs/t11e-soak-report.md item 3's "a
	// single lost or ignored PLI" (RTCP has no delivery guarantee, and the
	// real printer was observed to sometimes need more than one). This is
	// what proves a test actually needed Session's periodic
	// retryPLIUntilKeyframe (session.go), not just the one immediate PLI
	// Open sends when the track starts.
	ignorePLICount int
}

// fakePrinter is the state behind one httptest.Server standing in for the
// printer's signaling endpoint.
type fakePrinter struct {
	t    *testing.T
	opts fakePrinterOptions
	gops [][]testNAL

	stop chan struct{}

	mu  sync.Mutex
	pcs []*webrtc.PeerConnection
}

// startFakePrinter starts a fake printer and returns the "host:port" to
// pass as Open/Snapshot's host argument. Everything is torn down via
// t.Cleanup.
func startFakePrinter(t *testing.T, opts fakePrinterOptions) string {
	t.Helper()

	fp := &fakePrinter{
		t:    t,
		opts: opts,
		gops: loadTestGOPs(t),
		stop: make(chan struct{}),
	}

	server := httptest.NewServer(http.HandlerFunc(fp.handle))
	t.Cleanup(func() {
		close(fp.stop)
		server.Close()
		fp.mu.Lock()
		defer fp.mu.Unlock()
		for _, pc := range fp.pcs {
			_ = pc.Close()
		}
	})

	return server.Listener.Addr().String()
}

// handle answers one offer POST exactly like the printer's
// /call/webrtc_local: decode the base64-JSON offer, answer with a
// recvonly-compatible sendonly video track, and start streaming.
func (fp *fakePrinter) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	offerSDP, err := parseAnswer(body) // same base64-json envelope, reused
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	settingEngine := webrtc.SettingEngine{}
	// This fake printer's WebRTC transport (ICE/DTLS/SRTP) runs entirely
	// over pion's virtual network (see vnet_test.go's TestMain), not a real
	// socket: a real UDP socket, even one restricted to loopback, still
	// made a freshly built test binary trigger a Windows Firewall prompt.
	// testPrinterVNet is on the same virtual router as the client side
	// Session.Open uses (installed into newSessionAPI by TestMain), so the
	// two can reach each other purely in-process.
	settingEngine.SetNet(testPrinterVNet)
	// See vnet_test.go: disable mDNS candidate gathering, which otherwise
	// opens a real multicast UDP socket regardless of SetNet.
	settingEngine.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	// newMediaEngineWithNACK (session.go) registers the same NACK
	// generator/responder interceptor and "nack"/"nack pli" RTCPFeedback
	// production code uses. On this fake printer's send side, only the
	// responder ever does anything (it retransmits a lost RTP packet on
	// request), which is exactly what makes this fixture an accurate stand-in
	// for the real printer: its SDP answer already advertises "nack" and
	// "nack pli" support, and dev_docs/camera-keyframe-rca.md's live probe
	// confirmed it actually retransmits on request, not just advertises it.
	mediaEngine, interceptorRegistry, err := newMediaEngineWithNACK()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settingEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	)

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fp.mu.Lock()
	fp.pcs = append(fp.pcs, pc)
	fp.mu.Unlock()

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "profile-level-id=42e01f;packetization-mode=1;level-asymmetry-allowed=1",
		},
		"video", "pion",
		webrtc.WithPayloader(func(webrtc.RTPCodecCapability) (rtp.Payloader, error) {
			// DisableStapA is the long-standing default (opts.useStapA
			// false): the sample clip's own captured NAL sequence is sent
			// as independent NAL units, which lets most tests choose
			// exactly which GOPs carry their own SPS/PPS by construction.
			// opts.useStapA lets a test instead exercise real STAP-A
			// aggregation (SPS+PPS in one packet ahead of the IDR's FU-A
			// fragments), matching what the real printer's SDP allows.
			return &codecs.H264Payloader{DisableStapA: !fp.opts.useStapA}, nil
		}),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sender, err := pc.AddTrack(track)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Restrict the answer to exactly one H264 profile, matching the real
	// printer's own minimal answer (section 3: a single "96 96" H264
	// entry). Without this, pion's CreateAnswer lists every codec the
	// MediaEngine and the offer have in common (VP8, VP9, AV1, several
	// H264 profiles), which is not representative of the real printer and
	// would make CodecInfo.ProfileLevelID ambiguous to extract from SDP.
	for _, tr := range pc.GetTransceivers() {
		if tr.Kind() != webrtc.RTPCodecTypeVideo {
			continue
		}
		_ = tr.SetCodecPreferences([]webrtc.RTPCodecParameters{
			{
				RTPCodecCapability: webrtc.RTPCodecCapability{
					MimeType:     webrtc.MimeTypeH264,
					ClockRate:    90000,
					SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
					RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
				},
			},
		})
	}

	pliCh := make(chan struct{}, 1)
	go readRTCPForPLI(sender, pliCh)

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	<-gatherComplete

	// Wait for this PeerConnection to actually be connected (ICE+DTLS+SRTP)
	// before streaming: WriteSample succeeds even before that (it just
	// queues RTP), but those early packets have nowhere to go yet and are
	// lost, which used to silently drop the GOP's SPS/PPS/IDR. This must
	// not block the HTTP response below (the client cannot finish
	// connecting until it receives this answer), so it runs in its own
	// goroutine.
	go func() {
		if err := waitConnected(context.Background(), pc, 5*time.Second); err != nil {
			return
		}
		streamClip(track, fp.gops, pliCh, fp.stop, fp.opts)
	}()

	respondAnswer(w, pc.LocalDescription().SDP, fp.opts.encoding)
}

// readRTCPForPLI drains RTCP for sender (required so the underlying ICE
// transport keeps flowing) and signals pliCh whenever a Picture Loss
// Indication arrives, matching the real printer's advertised "nack pli"
// support (section 3).
func readRTCPForPLI(sender *webrtc.RTPSender, pliCh chan<- struct{}) {
	buf := make([]byte, 1500)
	for {
		n, _, err := sender.Read(buf)
		if err != nil {
			return
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil {
			continue
		}
		for _, p := range packets {
			if _, ok := p.(*rtcp.PictureLossIndication); ok {
				select {
				case pliCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

// streamClip writes gops to track: the first GOP immediately (so a session
// gets a keyframe as soon as it connects, matching the real printer's own
// behavior of producing a keyframe for a newly joined viewer, unless
// opts.firstGOPMissingParams simulates that first keyframe missing its
// SPS/PPS entirely), then either loops the whole clip (opts.autoRepeatKeyframes)
// or idles on filler frames until pliCh fires, at which point it sends the
// next GOP either stripped of its SPS/PPS (the default, to exercise
// Session's out-of-band cache-prepend path) or complete
// (opts.pliResendComplete, to exercise genuine in-band recovery from an
// initially incomplete first keyframe).
func streamClip(track *webrtc.TrackLocalStaticSample, gops [][]testNAL, pliCh <-chan struct{}, stop <-chan struct{}, opts fakePrinterOptions) {
	sendGOP := func(g []testNAL) bool {
		for _, n := range g {
			select {
			case <-stop:
				return false
			default:
			}
			dur := testFrameInterval
			if n.spsOrPPS {
				dur = 0
			}
			_ = track.WriteSample(media.Sample{Data: n.data, Duration: dur})
			time.Sleep(2 * time.Millisecond)
		}
		return true
	}

	first := gops[0]
	if opts.firstGOPMissingParams {
		first = stripSPSPPS(first)
	}
	if !sendGOP(first) {
		return
	}

	if opts.autoRepeatKeyframes {
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if !sendGOP(gops[i%len(gops)]) {
				return
			}
		}
	}

	filler := gops[0][len(gops[0])-1]
	pliSeen := 0
	for i := 1; ; {
		select {
		case <-stop:
			return
		case <-pliCh:
			pliSeen++
			if pliSeen <= opts.ignorePLICount {
				// Simulate this PLI being lost or ignored: no response,
				// keep idling on filler exactly as if nothing arrived.
				continue
			}
			gop := gops[i%len(gops)]
			if !opts.pliResendComplete {
				gop = stripSPSPPS(gop)
			}
			if !sendGOP(gop) {
				return
			}
			i++
		case <-time.After(20 * time.Millisecond):
			_ = track.WriteSample(media.Sample{Data: filler.data, Duration: testFrameInterval})
		}
	}
}

// stripSPSPPS drops the leading SPS/PPS NAL units from a GOP, leaving the
// IDR and following frames, to simulate a printer that does not repeat
// SPS/PPS before every keyframe.
func stripSPSPPS(gop []testNAL) []testNAL {
	out := make([]testNAL, 0, len(gop))
	for _, n := range gop {
		if n.spsOrPPS {
			continue
		}
		out = append(out, n)
	}
	return out
}

// loadTestGOPs reads the shared sample clip (owned by internal/camera/decode,
// read here, not copied) and splits it into groups of pictures at each SPS
// boundary.
func loadTestGOPs(t *testing.T) [][]testNAL {
	t.Helper()

	path := filepath.Join("decode", "testdata", "camera_sample_clip.h264")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}

	reader, err := h264reader.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("h264reader.NewReader: %v", err)
	}

	var gops [][]testNAL
	var current []testNAL
	started := false // true once the first SPS has been seen
	for {
		nal, err := reader.NextNAL()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextNAL: %v", err)
		}

		isSPS := nal.UnitType == h264reader.NalUnitTypeSPS
		if isSPS {
			started = true
			if len(current) > 0 {
				gops = append(gops, current)
				current = nil
			}
		}
		if !started {
			// The clip opens with a few P-frame NAL units left over from
			// before the capture's first SPS/PPS/IDR (no IDR ever arrived
			// for them, so they are not a usable GOP for this fixture);
			// drop them so every element of gops is a real, complete GOP
			// starting with its own SPS, PPS and IDR.
			continue
		}
		isPPS := nal.UnitType == h264reader.NalUnitTypePPS
		current = append(current, testNAL{
			data:     append([]byte(nil), nal.Data...),
			spsOrPPS: isSPS || isPPS,
		})
	}
	if len(current) > 0 {
		gops = append(gops, current)
	}
	if len(gops) < 2 {
		t.Fatalf("sample clip split into %d GOPs, want at least 2", len(gops))
	}
	return gops
}

// respondAnswer writes sdp as the HTTP response body, encoded per enc.
func respondAnswer(w http.ResponseWriter, sdp string, enc answerEncoding) {
	switch enc {
	case answerRawSDP:
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(sdp))
	default:
		payload, err := json.Marshal(sigMessage{Type: "answer", SDP: sdp})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(payload)))
	}
}
