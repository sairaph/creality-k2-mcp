package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	h264c "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/camera/fmp4"
)

// streamFragmentDuration is the fmp4 fragment duration used for every
// viewer stream (fmp4.Config.FragmentDuration). It is a package variable,
// not a literal passed inline, purely so a test can shrink it and observe a
// fragment flush without waiting through many real camera GOPs at the 1
// second production default (fmp4.DefaultFragmentDuration).
var streamFragmentDuration = 500 * time.Millisecond

// streamMaxKeyframeWait and streamMaxBufferedBytes are the fmp4.Config
// bounds used for every viewer stream's wait for a keyframe (see
// fmp4.ErrNoKeyframe). Package variables, not literals, so a test can shrink
// streamMaxKeyframeWait and observe the handler request a fresh keyframe
// from the hub without waiting out the 10 second production default
// (fmp4.DefaultMaxKeyframeWait).
var (
	streamMaxKeyframeWait  = fmp4.DefaultMaxKeyframeWait
	streamMaxBufferedBytes = fmp4.DefaultMaxBufferedBytes
)

// streamWaitForKeyframeTimeout bounds how long handleStream waits for the
// first keyframe access unit after subscribing before giving up (a camera
// that never produces one, or a hub whose upstream is stuck reconnecting).
// 25s, not a shorter value: live testing against a real K2 observed cold-
// connect latency (session open to first usable keyframe) up to 16s, so a
// shorter budget cuts off attempts that would have succeeded (dev_docs/
// t11e-soak-report.md, dev_docs/camera-keyframe-rca.md).
var streamWaitForKeyframeTimeout = 25 * time.Second

// handleStream serves GET /stream/<printer-id>.mp4: a live fragmented MP4
// (an init segment, then one moof+mdat fragment per flushed GOP) muxed from
// the hub subscription for that printer, with the MSE codec string read
// straight from the stream's own first SPS (never hardcoded) sent back as
// the X-Video-Codec response header. If the stream's SPS/PPS ever changes
// mid-flight (fmp4.ErrParametersChanged), the response body simply ends, so
// the viewer page's fetch loop (viewer_page.go) sees the stream finish and
// reconnects for a fresh init segment; if the muxer runs out of buffered
// keyframe patience (fmp4.ErrNoKeyframe), a fresh keyframe is requested from
// the printer (a PLI, via the hub's upstream session) and the stream keeps
// running on the same connection.
func (v *viewerServer) handleStream(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".mp4")
	if !ok || id == "" {
		http.Error(w, "not found: stream path must be /stream/<printer-id>.mp4", http.StatusNotFound)
		return
	}

	printer, ok := v.findPrinter(id)
	if !ok {
		http.Error(w, fmt.Sprintf("not found: no enabled printer with id %q", id), http.StatusNotFound)
		return
	}

	// Bound how many browser tabs can have this printer's (or, in total,
	// any printer's) stream open at once: each open response holds a Hub
	// subscription and an fmp4 writer for as long as the connection lasts.
	if !v.acquireStreamSlot(printer.ID) {
		http.Error(w, fmt.Sprintf("too many concurrent streams (limit %d per printer, %d total)", maxStreamsPerPrinter, maxStreamsTotal), http.StatusTooManyRequests)
		return
	}
	defer v.releaseStreamSlot(printer.ID)

	subID, ch := v.hub.Subscribe(printer.Host)
	defer v.hub.Unsubscribe(printer.Host, subID)

	ctx := r.Context()
	keyframe, ok := waitForKeyframe(ctx, ch, streamWaitForKeyframeTimeout)
	if !ok {
		// review backlog item 47, corrected by dev_docs/camera-keyframe-
		// rca.md: an earlier claim that "the K2 pauses its camera stream
		// while idle" was unsupported and has been withdrawn - a live
		// proof-of-concept comparison found the camera transport can stay
		// continuously active while a complete keyframe still never
		// assembles, because one RTP packet lost inside an IDR (which spans
		// many FU-A fragments) left that access unit stuck forever with
		// nothing asking for a retransmit. internal/camera now registers a
		// NACK generator/responder to fix that, but this timeout can still
		// legitimately occur on a lossy or congested network, so a plain
		// "unavailable" message would still mislead a caller into thinking
		// something here is broken rather than a network condition.
		http.Error(w, "unavailable (code unavailable): the printer's camera did not produce a complete keyframe "+
			"within the wait budget; this is usually network packet loss on the camera stream, not the printer "+
			"being unreachable; try again", http.StatusServiceUnavailable)
		return
	}

	sps, pps, err := parameterSets(keyframe.Data)
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	codecStr, err := fmp4.CodecString(sps)
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "video/mp4")
	// The MSE codec string, derived from the stream's actual SPS (never
	// hardcoded): the page's inline JS reads this before creating its
	// SourceBuffer, since it must know the codec before the first fragment
	// arrives.
	w.Header().Set("X-Video-Codec", codecStr)
	w.WriteHeader(http.StatusOK)

	writer, err := fmp4.NewWriter(w, sps, pps, fmp4.Config{
		FragmentDuration: streamFragmentDuration,
		MaxKeyframeWait:  streamMaxKeyframeWait,
		MaxBufferedBytes: streamMaxBufferedBytes,
	})
	if err != nil {
		return
	}
	defer writer.Close()

	if err := writer.WriteAccessUnit(keyframe.PTS, keyframe.Data); err != nil {
		return
	}

	// requestedKeyframe avoids calling v.hub.RequestKeyframe more than once
	// per gap from this consumer: the hub itself now owns retrying while a
	// keyframe is still missing (review backlog item 51, item 3 - it
	// coalesces every consumer's request, plus its own automatic recovery
	// retry, to at most one actual PLI per second per upstream), so this
	// loop no longer runs its own periodic retry ticker (that was review
	// backlog item 44's fix, now superseded).
	requestedKeyframe := false
	for {
		select {
		case <-ctx.Done():
			return
		case au, ok := <-ch:
			if !ok {
				return
			}
			switch err := writer.WriteAccessUnit(au.PTS, au.Data); {
			case err == nil:
				requestedKeyframe = false
			case errors.Is(err, fmp4.ErrParametersChanged):
				return
			case errors.Is(err, fmp4.ErrNoKeyframe):
				if !requestedKeyframe {
					_ = v.hub.RequestKeyframe(printer.Host)
					requestedKeyframe = true
				}
			default:
				return
			}
		}
	}
}

// waitForKeyframe reads from ch until a keyframe access unit arrives, ctx is
// done, or timeout elapses, discarding any non-keyframe access unit along
// the way. In practice a fresh subscriber usually gets a keyframe
// immediately (Hub.Subscribe primes a new subscriber with the last known
// keyframe when one exists).
func waitForKeyframe(ctx context.Context, ch <-chan camera.AccessUnit, timeout time.Duration) (camera.AccessUnit, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return camera.AccessUnit{}, false
		case <-deadline.C:
			return camera.AccessUnit{}, false
		case au, ok := <-ch:
			if !ok {
				return camera.AccessUnit{}, false
			}
			if au.Keyframe {
				return au, true
			}
		}
	}
}

// parameterSets extracts the SPS and PPS NAL units (each with its own NAL
// header byte, no Annex-B start code) from a keyframe access unit's Annex-B
// data: the shape fmp4.NewWriter expects, matching camera.AccessUnit's own
// doc comment that a keyframe access unit always carries its SPS/PPS.
func parameterSets(annexB []byte) (sps, pps []byte, err error) {
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(annexB); err != nil {
		return nil, nil, fmt.Errorf("parse keyframe access unit: %w", err)
	}
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
	if sps == nil || pps == nil {
		return nil, nil, fmt.Errorf("keyframe access unit is missing SPS or PPS")
	}
	return sps, pps, nil
}

// findPrinter resolves id against the enabled printers this viewer server
// may show.
func (v *viewerServer) findPrinter(id string) (ViewerPrinter, bool) {
	if v.printers == nil {
		return ViewerPrinter{}, false
	}
	printers, err := v.printers.ListEnabled()
	if err != nil {
		return ViewerPrinter{}, false
	}
	for _, p := range printers {
		if p.ID == id {
			return p, true
		}
	}
	return ViewerPrinter{}, false
}
