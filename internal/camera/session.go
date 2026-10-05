package camera

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// signalingTimeout and iceConnectTimeout are the two timeouts decision 11
// calls for: signaling (offer POST, including local ICE gathering) and
// waiting for the resulting PeerConnection to actually connect.
const (
	signalingTimeout  = 5 * time.Second
	iceConnectTimeout = 5 * time.Second
)

// accessUnitBufferSize is how many decoded access units Session buffers
// internally before a slow consumer starts causing drops. A consumer that
// wants to fan out to many subscribers without ever dropping at this layer
// should drain AccessUnits() promptly, e.g. into a Broadcaster.
const accessUnitBufferSize = 32

// samplebuilderMaxLate bounds how many RTP packets samplebuilder buffers
// while waiting for a sample to become complete (see its New docs). It is
// measured in packets, and samplebuilder force-purges its buffer once more
// than this many are held, so a sample that spans more packets can never
// complete, even with no loss at all. With the chamber light on the K2's
// 1280x720 IDR spans 117-118 RTP packets (about 65 kB; unlit about 13), so the
// old bound of 100 made a lit keyframe impossible to assemble
// (dev_docs/field-camera-transport-analysis.md, field feedback item 3). 512
// keeps more than 4x headroom over the largest observed IDR while bounding
// how long one packet that NACK cannot repair stalls assembly. It is a
// keyframe-size anti-break bound, not a tuning knob. The snapshot, live view
// and recording paths share it through Session.
const samplebuilderMaxLate = 512

// pliRetryInterval is how often Session re-sends an RTCP PLI while it has
// not yet delivered any complete keyframe (SPS+PPS+IDR) on a freshly opened
// track. A package variable, not a constant, so tests can shrink it instead
// of waiting out a real multi-second retry budget (the same pattern used
// throughout internal/daemon, e.g. camera_hub.go's reconnectBackoffMin).
//
// This exists because of dev_docs/t11e-soak-report.md item 3: across a
// ~14-minute live soak, a fresh Session sent at most two PLIs total (one on
// track start, one best-effort nudge from Snapshot) and then simply waited
// out its 10-15s budget with nothing further sent. RTCP travels over UDP
// with no delivery guarantee, and the K2's camera was observed to need
// several seconds, sometimes much longer, before honoring a PLI; a single
// lost or ignored request left the whole wait budget empty. The live
// proof-of-concept this package is built from
// (references/printer-snapshot/extra/webrtc_pion_viewer_poc.go.txt) already
// retried every 2s rather than once, which is why it reliably captured
// video where the product did not. Retrying at 1s cadence costs the printer
// one small RTCP packet per second at most, and only until the first usable
// keyframe of this Session's lifetime arrives.
var pliRetryInterval = 1 * time.Second

// EventLogger receives structured lifecycle events from a Session: open,
// close, each PLI sent, each complete keyframe received, and any other
// noteworthy operational condition (a signaling/ICE failure, or an IDR
// arriving with no way yet to attach SPS/PPS). Every method may be called
// concurrently, from Session's internal goroutines, and must not block or
// panic. A nil EventLogger is never passed to an implementation's own
// callers; Open substitutes noopEventLogger when none is supplied via
// WithLogger, so every call site in this package can call s.logger.X(...)
// unconditionally.
//
// This is a hook only: nothing in this package writes to a daemon log
// itself. A caller (e.g. internal/daemon) wires an EventLogger that routes
// these calls into its own logger; until that wiring exists, WithLogger is
// simply not used and every event is a no-op.
type EventLogger interface {
	// SessionOpened is called once Open has a fully connected Session ready
	// to return.
	SessionOpened(host string)
	// SessionClosed is called at the end of Close, err being whatever the
	// underlying PeerConnection.Close returned (nil on the common path).
	SessionClosed(host string, err error)
	// KeyframeRequested is called every time Session writes an RTCP PLI,
	// including the very first one and every periodic retry.
	KeyframeRequested(host string)
	// KeyframeReceived is called once per Session, the first time a
	// complete keyframe access unit (SPS+PPS+IDR, in-band or reconstructed
	// from cached parameter sets) is produced. waited is how long that took
	// since Open returned control to negotiate the connection (roughly,
	// time since the track started); packets is how many RTP packets carried
	// it (compare with samplebuilderMaxLate when a keyframe never arrives).
	KeyframeReceived(host string, waited time.Duration, packets int)
	// Error is called for a session-level failure or a noteworthy but
	// non-fatal diagnostic (e.g. an IDR arriving with neither in-band nor
	// cached SPS/PPS available yet, so it cannot be delivered as a usable
	// keyframe).
	Error(host string, err error)
}

// noopEventLogger is the default EventLogger: every call is a no-op. Used
// whenever Open is called without WithLogger.
type noopEventLogger struct{}

func (noopEventLogger) SessionOpened(string)                        {}
func (noopEventLogger) SessionClosed(string, error)                 {}
func (noopEventLogger) KeyframeRequested(string)                    {}
func (noopEventLogger) KeyframeReceived(string, time.Duration, int) {}
func (noopEventLogger) Error(string, error)                         {}

// Option configures Open. See WithLogger.
type Option func(*sessionOptions)

type sessionOptions struct {
	logger EventLogger
}

// WithLogger routes Session's lifecycle events (open/close, PLI sent,
// keyframe received, errors) to logger instead of discarding them. Passing
// a nil logger is a no-op (Open keeps its default no-op logger).
func WithLogger(logger EventLogger) Option {
	return func(o *sessionOptions) {
		if logger != nil {
			o.logger = logger
		}
	}
}

// Session is one WebRTC connection to a K2's camera: recvonly, video only.
// Create one with Open. Access units are delivered on the channel from
// AccessUnits, oldest first. A Session is safe for concurrent use from
// multiple goroutines (RequestKeyframe, CodecInfo, Close can all be called
// while AccessUnits is being drained).
type Session struct {
	pc *webrtc.PeerConnection

	host     string
	logger   EventLogger
	openedAt time.Time

	aus     chan AccessUnit
	dropped uint64

	mu             sync.Mutex
	lastSPS        []byte
	lastPPS        []byte
	width          int
	height         int
	profileLevelID string
	ssrc           webrtc.SSRC
	trackReady     bool

	// keyframeCh is closed exactly once, by processSample via keyframeOnce,
	// the first time this Session produces a complete keyframe access unit.
	// retryPLIUntilKeyframe watches it to know when to stop resending PLIs.
	keyframeCh   chan struct{}
	keyframeOnce sync.Once

	// ptsUnwrap is only ever touched from the single readTrack goroutine
	// (via processSample), so it needs no locking of its own.
	ptsUnwrap rtpTimestampUnwrapper

	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closeMu sync.Mutex
	closed  bool
}

// Open negotiates a recvonly, video-only WebRTC session with the printer's
// camera at host (a bare hostname/IP, or "host:port" for a non-default
// signaling port such as a test fixture) and returns a Session once RTP is
// flowing. The context bounds signaling and the initial ICE connect only
// (each internally capped at 5 seconds); once Open returns, the Session
// runs independently of ctx until Close is called.
//
// opts configures optional behavior; WithLogger routes lifecycle events to
// an EventLogger. Every existing caller of Open(ctx, host) keeps compiling
// and behaving exactly as before (opts defaults to no logging).
func Open(ctx context.Context, host string, opts ...Option) (*Session, error) {
	cfg := sessionOptions{logger: noopEventLogger{}}
	for _, opt := range opts {
		opt(&cfg)
	}

	api, err := newSessionAPI(host)
	if err != nil {
		cfg.logger.Error(host, err)
		return nil, err
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		err = fmt.Errorf("camera: create peer connection: %w", err)
		cfg.logger.Error(host, err)
		return nil, err
	}

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		_ = pc.Close()
		err = fmt.Errorf("camera: add video transceiver: %w", err)
		cfg.logger.Error(host, err)
		return nil, err
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	s := &Session{
		pc:         pc,
		host:       host,
		logger:     cfg.logger,
		openedAt:   time.Now(),
		aus:        make(chan AccessUnit, accessUnitBufferSize),
		keyframeCh: make(chan struct{}),
		cancel:     cancel,
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		s.mu.Lock()
		s.ssrc = track.SSRC()
		s.trackReady = true
		s.mu.Unlock()

		// Request an immediate keyframe as soon as the track starts, same
		// as any real viewer joining mid-stream (section 3: a PLI reliably
		// produces a fresh IDR), then keep retrying periodically until this
		// Session's first complete keyframe arrives (pliRetryInterval's doc
		// comment: a single PLI is not reliable enough against this
		// printer, per the T11e soak report).
		s.sendPLI(track.SSRC())

		s.wg.Add(1)
		go s.readTrack(sessionCtx, track)

		s.wg.Add(1)
		go s.retryPLIUntilKeyframe(sessionCtx, track.SSRC())
	})

	signalCtx, signalCancel := context.WithTimeout(ctx, signalingTimeout)
	defer signalCancel()

	answerSDP, err := s.negotiate(signalCtx, host)
	if err != nil {
		// Close (not just cancel+pc.Close) so that if OnTrack already fired
		// and started readTrack, Open does not return until that goroutine
		// has actually exited: otherwise a caller that treats a non-nil
		// error as "nothing left running" would be wrong.
		_ = s.Close()
		s.logger.Error(host, err)
		return nil, err
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}); err != nil {
		_ = s.Close()
		err = fmt.Errorf("camera: set remote description: %w", err)
		s.logger.Error(host, err)
		return nil, err
	}

	connectCtx, connectCancel := context.WithTimeout(ctx, iceConnectTimeout)
	defer connectCancel()
	if err := waitConnected(connectCtx, pc, iceConnectTimeout); err != nil {
		_ = s.Close()
		s.logger.Error(host, err)
		return nil, err
	}

	s.logger.SessionOpened(host)
	return s, nil
}

// sendPLI writes one RTCP Picture Loss Indication for ssrc and reports it to
// the EventLogger. Errors from WriteRTCP are not fatal (RTCP is
// best-effort); retryPLIUntilKeyframe's periodic retries are what make a
// single lost PLI harmless.
func (s *Session) sendPLI(ssrc webrtc.SSRC) {
	_ = s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
	s.logger.KeyframeRequested(s.host)
}

// retryPLIUntilKeyframe resends a PLI every pliRetryInterval until s.keyframeCh
// closes (this Session's first complete keyframe has been delivered) or ctx
// is done (Close was called, or Open failed before ever reaching this
// point). It only ever concerns a Session's very first keyframe: once one
// has arrived, s.lastSPS/lastPPS are populated, so any future IDR can be
// completed from that cache without needing a resend loop (see
// processSample and RequestKeyframe for on-demand, one-shot requests after
// that point).
func (s *Session) retryPLIUntilKeyframe(ctx context.Context, ssrc webrtc.SSRC) {
	defer s.wg.Done()

	ticker := time.NewTicker(pliRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.keyframeCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sendPLI(ssrc)
		}
	}
}

// newSessionAPI builds the pion API (media engine and network setup) that
// Open uses to create its PeerConnection. It is a package variable, not a
// hardcoded call, so this package's own tests (see the vnet setup in
// fakeprinter_test.go) can substitute pion's virtual network for the real
// one: a real UDP socket, even one restricted to a single loopback address,
// still made a freshly built test binary trigger a Windows Firewall prompt
// (Windows treats each new, unsigned build as an unrecognized application
// regardless of which address it binds to). pion's vnet performs no real OS
// networking at all, which removes the possibility entirely. Production
// code (a real binary talking to a real printer) always uses this default,
// which restricts ICE to the one real local interface that can reach host.
var newSessionAPI = func(host string) (*webrtc.API, error) {
	settingEngine := webrtc.SettingEngine{}
	if err := restrictICEToRoute(&settingEngine, host); err != nil {
		return nil, fmt.Errorf("camera: %w", err)
	}
	// Disable pion's mDNS candidate gathering/querying. The SettingEngine
	// zero value leaves this unset, which pion treats as
	// ice.MulticastDNSModeQueryOnly, opening a real 224.0.0.0:5353
	// multicast UDP socket on every real Open - pointless exposure for a
	// printer that restrictICEToRoute above already pins to one known
	// local address and route.
	disableMulticastDNS(&settingEngine)

	mediaEngine, interceptorRegistry, err := newMediaEngineWithNACK()
	if err != nil {
		return nil, err
	}

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(mediaEngine),
		webrtc.WithSettingEngine(settingEngine),
		webrtc.WithInterceptorRegistry(interceptorRegistry),
	), nil
}

// newMediaEngineWithNACK builds a MediaEngine with the default codecs plus a
// NACK generator/responder interceptor (webrtc.ConfigureNack), returning the
// interceptor.Registry that must be passed to webrtc.NewAPI alongside it.
//
// This is dev_docs/camera-keyframe-rca.md's corrected root cause fix. The
// original proof of concept (references/printer-snapshot/extra/
// webrtc_pion_viewer_poc.go.txt) reliably captured video with no interceptor
// at all, but it depacketized one RTP packet at a time with no
// timestamp-based reassembly, so a single lost packet only corrupted the one
// small NAL unit it belonged to - never fatal for its file-dump use case. A
// live probe with the real printer at 192.168.1.102 found the product's own
// pipeline receiving RTP continuously (3,896 access units in 5 minutes, no
// transport gap) yet completing zero keyframes across roughly 300 PLI
// requests: samplebuilder (readTrack, below) only emits a sample once every
// packet between its first and last has arrived, and a 1280x720 IDR spans
// many FU-A fragments, so one lost packet anywhere inside it silently
// starves that whole access unit forever, while a single-packet P-frame
// keeps surviving by luck. The K2's own SDP answer already advertises "nack"
// and "nack pli" support (it responds to a NACK by retransmitting), so the
// only missing piece was the receive side ever asking: ConfigureNack adds
// pion's nack.GeneratorInterceptor, which watches the incoming RTP sequence
// for gaps and sends an RTCP NACK for the missing sequence number(s), and
// registers the matching "nack"/"nack pli" RTCPFeedback on the MediaEngine's
// codecs so the interceptor's own stream filter (nack.streamSupportNack)
// actually activates for this negotiated codec. Exported at package level
// (rather than inlined in newSessionAPI) so this package's own tests
// (vnet_test.go's TestMain, fakeprinter_test.go) build their PeerConnections
// with the exact same interceptor configuration production code uses,
// instead of a hand-duplicated approximation that could silently drift from
// it.
func newMediaEngineWithNACK() (*webrtc.MediaEngine, *interceptor.Registry, error) {
	mediaEngine := &webrtc.MediaEngine{}
	if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
		return nil, nil, fmt.Errorf("camera: register codecs: %w", err)
	}

	interceptorRegistry := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(mediaEngine, interceptorRegistry); err != nil {
		return nil, nil, fmt.Errorf("camera: configure nack interceptor: %w", err)
	}

	return mediaEngine, interceptorRegistry, nil
}

// disableMulticastDNS turns off pion/ice's mDNS candidate gathering and
// querying on se. It is factored out of newSessionAPI, rather than inlined,
// so this package's tests can apply this exact production setting to a bare
// SettingEngine and inspect the result directly, without going through
// restrictICEToRoute's real UDP dial (which needs a real route to probe and
// so cannot run in a socket-free test).
func disableMulticastDNS(se *webrtc.SettingEngine) {
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
}

// restrictICEToRoute configures se so ICE candidate gathering only uses the
// one local network address the OS routing table would pick to reach host,
// instead of pion's default of every local interface (Wi-Fi, Ethernet,
// Docker, a VPN, Tailscale, IPv6, and loopback all at once). A printer is
// only ever reachable on one local subnet, so gathering candidates
// elsewhere is both pointless and needlessly exposes the process on
// networks it has no reason to touch.
//
// The local address is found with the standard "connect" a UDP socket to
// the destination and read back its local address" trick: this performs no
// handshake and sends no packet (UDP has no connection to establish), so it
// works even before the printer has answered anything.
func restrictICEToRoute(se *webrtc.SettingEngine, host string) error {
	probeAddr := normalizeHostPort(host)

	conn, err := net.Dial("udp4", probeAddr)
	if err != nil {
		return fmt.Errorf("determine local network route to %s: %w", host, err)
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localAddr.IP == nil {
		return fmt.Errorf("determine local network route to %s: no usable local address", host)
	}
	localIP := localAddr.IP

	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	// This also covers the loopback case (a test's fake printer on
	// 127.0.0.1): pion excludes loopback addresses from gathering unless
	// this is set, and the IPFilter below then narrows it down to exactly
	// that one loopback address anyway.
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.Equal(localIP) })

	return nil
}

// negotiate builds a local offer, waits for ICE gathering to complete
// (this package uses non-trickle signaling, matching the printer's own
// single-shot HTTP signaling endpoint), posts it, and returns the printer's
// answer SDP.
func (s *Session) negotiate(ctx context.Context, host string) (string, error) {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("camera: create offer: %w", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("camera: set local description: %w", err)
	}

	select {
	case <-gatherComplete:
	case <-ctx.Done():
		return "", fmt.Errorf("camera: ice gathering: %w", ctx.Err())
	}

	localSDP := s.pc.LocalDescription().SDP
	answerSDP, err := postOffer(ctx, signalingURL(host), localSDP)
	if err != nil {
		return "", err
	}
	return answerSDP, nil
}

// waitConnected polls pc's connection state until it reaches Connected, a
// terminal failure state, ctx is done, or timeout elapses. Polling (rather
// than a one-shot OnConnectionStateChange callback) avoids missing a state
// transition that lands between the callback firing and this function
// reading it.
func waitConnected(ctx context.Context, pc *webrtc.PeerConnection, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		switch pc.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			return nil
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			return fmt.Errorf("camera: peer connection reached state %s", pc.ConnectionState())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("camera: timed out after %s waiting for peer connection to connect", timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("camera: waiting for peer connection: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// readTrack depacketizes RTP from track into access units via samplebuilder
// and delivers them on s.aus, until track reading ends (including because
// Close closed the underlying PeerConnection) or ctx is done.
func (s *Session) readTrack(ctx context.Context, track *webrtc.TrackRemote) {
	defer s.wg.Done()

	depacketizer := &codecs.H264Packet{}
	builder := samplebuilder.New(samplebuilderMaxLate, depacketizer, track.Codec().ClockRate)
	var counts packetCounter

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}

		counts.add(pkt.Timestamp)
		builder.Push(pkt)
		for {
			sample := builder.Pop()
			if sample == nil {
				break
			}
			au := s.processSample(sample.Data, sample.PacketTimestamp, counts.take(sample.PacketTimestamp))
			select {
			case s.aus <- au:
			default:
				atomic.AddUint64(&s.dropped, 1)
			}
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// packetCounter counts RTP packets per RTP timestamp (one timestamp is one
// access unit), so a popped sample can report how many packets carried it.
// Samples samplebuilder drops never pop, so their entries are cleared once the
// map holds more timestamps than the builder could still be assembling.
type packetCounter map[uint32]int

func (c *packetCounter) add(ts uint32) {
	if *c == nil || len(*c) > samplebuilderMaxLate {
		*c = make(packetCounter)
	}
	(*c)[ts]++
}

// take returns the count for ts and forgets it.
func (c *packetCounter) take(ts uint32) int {
	n := (*c)[ts]
	delete(*c, ts)
	return n
}

// processSample turns one samplebuilder-assembled Annex-B sample into an
// AccessUnit, updating the cached SPS/PPS and codec info, and prepending
// the most recently seen SPS/PPS to a keyframe that did not carry its own
// (see the AccessUnit doc comment). rtpTimestamp is the sample's RTP
// timestamp (media.Sample.PacketTimestamp), unwrapped into AccessUnit.PTS.
//
// Keyframe is only ever true when the returned Data is actually
// self-contained (SPS+PPS+IDR, whichever of SPS/PPS arrived in-band versus
// were prepended from cache): an IDR NAL unit whose SPS and/or PPS is
// neither in this sample nor cached yet (the very first sample of a brand
// new Session, before any parameter set has ever been seen) is reported
// with Keyframe false, not true. This is dev_docs/t11e-soak-report.md item
// 2's fix: the soak's one snapshot that received an IDR within budget still
// failed with "decode keyframe: ... sps=false pps=false idr=true" because
// the old code returned Keyframe:true here regardless, and both
// waitForKeyframe (snapshot.go) and the daemon's own waitForKeyframe
// (internal/daemon/viewer_stream.go, recorder.go) trust that flag per
// AccessUnit's own doc comment. Returning false here instead makes every
// caller correctly keep waiting - which retryPLIUntilKeyframe backs up by
// continuing to ask the printer for a fresh, hopefully complete IDR.
func (s *Session) processSample(data []byte, rtpTimestamp uint32, packets int) AccessUnit {
	units := splitAnnexB(data)

	hasIDR := false
	hasSPS := false
	hasPPS := false

	for _, u := range units {
		switch nalType(u) {
		case nalTypeSPS:
			hasSPS = true
			sps := append([]byte(nil), u...)
			s.mu.Lock()
			s.lastSPS = sps
			s.mu.Unlock()
			if info, err := parseSPS(u); err == nil {
				s.mu.Lock()
				s.width, s.height = info.Width, info.Height
				s.profileLevelID = info.ProfileLevelID
				s.mu.Unlock()
			}
		case nalTypePPS:
			hasPPS = true
			pps := append([]byte(nil), u...)
			s.mu.Lock()
			s.lastPPS = pps
			s.mu.Unlock()
		case nalTypeIDR:
			hasIDR = true
		}
	}

	// complete tracks whether this access unit, once any cached SPS/PPS is
	// prepended below, will actually carry both parameter sets alongside
	// its IDR. It starts optimistic (true) and is only ever pulled false by
	// a parameter set this sample needs but does not have in-band and has
	// nothing cached for either (see the doc comment above).
	complete := hasIDR
	if hasIDR && (!hasSPS || !hasPPS) {
		s.mu.Lock()
		sps, pps := s.lastSPS, s.lastPPS
		s.mu.Unlock()

		var prefix []byte
		if !hasSPS {
			if sps == nil {
				complete = false
			} else {
				prefix = append(prefix, sps...)
			}
		}
		if !hasPPS {
			if pps == nil {
				complete = false
			} else {
				prefix = append(prefix, pps...)
			}
		}
		if len(prefix) > 0 {
			data = append(append([]byte(nil), prefix...), data...)
		}
	}

	if complete {
		s.keyframeOnce.Do(func() {
			close(s.keyframeCh)
			s.logger.KeyframeReceived(s.host, time.Since(s.openedAt), packets)
		})
	} else if hasIDR {
		// A real IDR arrived, but this Session has never seen a usable
		// SPS/PPS to attach to it yet (dev_docs/t11e-soak-report.md item
		// 2). Not fatal: retryPLIUntilKeyframe (still running, since
		// s.keyframeCh has not closed) keeps asking for a fresh one, and
		// whichever GOP eventually arrives with its own in-band SPS/PPS
		// (typically a STAP-A aggregate, per RFC 6184) will complete
		// normally and unblock every waiter.
		s.logger.Error(s.host, fmt.Errorf("camera: received IDR with no SPS/PPS available (in-band or cached) yet; waiting for parameter sets"))
	}

	return AccessUnit{
		Data:       data,
		Keyframe:   complete,
		PTS:        s.ptsUnwrap.unwrap(rtpTimestamp),
		CapturedAt: time.Now(),
	}
}

// AccessUnits returns the channel access units are delivered on. It is
// closed once Close has finished tearing down the Session.
func (s *Session) AccessUnits() <-chan AccessUnit {
	return s.aus
}

// RequestKeyframe sends an RTCP Picture Loss Indication, which reliably
// makes the printer's encoder emit a fresh IDR (confirmed live, section 3).
// It returns an error if no video track has started yet. This is the
// on-demand, one-shot request a consumer uses to ask for a fresh keyframe
// after the session's first one (e.g. fmp4.ErrNoKeyframe); the automatic,
// periodic retry for a Session's very first keyframe is
// retryPLIUntilKeyframe, started internally by Open.
func (s *Session) RequestKeyframe() error {
	s.mu.Lock()
	ready := s.trackReady
	ssrc := s.ssrc
	s.mu.Unlock()

	if !ready {
		return fmt.Errorf("camera: no active video track yet")
	}
	err := s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
	s.logger.KeyframeRequested(s.host)
	return err
}

// CodecInfo returns what is currently known about the track's codec.
// Width and Height are zero until an SPS NAL unit has been seen.
func (s *Session) CodecInfo() CodecInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return CodecInfo{
		ProfileLevelID: s.profileLevelID,
		Width:          s.width,
		Height:         s.height,
	}
}

// ParameterSets returns the most recently seen SPS and PPS NAL units (each
// including its Annex-B start code), or nil, nil if neither has been seen
// yet. A muxer (e.g. internal/camera/fmp4) uses these to initialize its
// track and to detect an in-stream parameter change: a new pair with
// different bytes means the stream's codec configuration changed and a new
// segment is needed.
func (s *Session) ParameterSets() (sps, pps []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.lastSPS...), append([]byte(nil), s.lastPPS...)
}

// Dropped returns how many access units this Session has discarded because
// AccessUnits was not being drained fast enough.
func (s *Session) Dropped() uint64 {
	return atomic.LoadUint64(&s.dropped)
}

// Close tears down the PeerConnection and waits for the internal RTP
// reading goroutine to exit before returning, so Close leaves no goroutine
// running. Safe to call more than once; only the first call has effect.
func (s *Session) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	s.cancel()
	err := s.pc.Close()
	s.wg.Wait()
	close(s.aus)
	s.logger.SessionClosed(s.host, err)

	if err != nil {
		return fmt.Errorf("camera: close peer connection: %w", err)
	}
	return nil
}
