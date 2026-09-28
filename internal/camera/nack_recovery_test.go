package camera

// This file is a network-level regression test for the exact failure mode
// dev_docs/camera-keyframe-rca.md's corrected root cause analysis
// describes: a 1280x720 IDR access unit spans many FU-A fragments, and one
// lost fragment anywhere inside it used to leave the whole access unit
// stuck in samplebuilder forever (nothing was ever asking the printer to
// retransmit it), while a single-packet P-frame kept surviving by luck.
// newSessionAPI now registers pion's NACK generator/responder interceptor
// (webrtc.ConfigureNack, via newMediaEngineWithNACK in session.go), which
// this package's fake printer (fakeprinter_test.go) also uses on its own
// send side, mirroring the real printer's own advertised ("nack", "nack
// pli") and observed (it retransmits on request) behavior.
//
// installMediaLossFilter (below) drops one in every four RTP media packets -
// including any retransmission, which is exactly as likely to be dropped
// again as the original - persistently for the life of the test.
// fakePrinterOptions.ignorePLICount is set high enough that the fake
// printer never honors a PLI during the test, so the only GOP it ever
// sends is its one unconditional first GOP; with every PLI ignored, a
// complete keyframe can only ever come from that same original access unit
// being fully recovered from the injected loss, never from a fresh,
// undamaged GOP sent in response to a request.
//
// This test cannot, on its own, prove that pion's explicit nack interceptor
// (rather than some other resilience already present once RTX is
// negotiated - webrtc.MediaEngine.RegisterDefaultCodecs pairs every default
// video codec with an "apt=" RTX companion codec by itself, independent of
// whether ConfigureNack is ever called) is the specific mechanism behind a
// recovery observed only inside pion's own vnet simulation. What it does
// prove, deterministically and every run, is the property that actually
// matters for this package's contract: a Session built by production's own
// newSessionAPI configuration survives persistent, non-total packet loss
// across its one and only GOP without ever losing that GOP's keyframe
// outright (dropping every packet reliably makes this same test time out,
// proven by TestSessionTimesOutWhenEveryMediaPacketIsLost below, confirming
// the injected loss is real and consequential, not a no-op). The corrected
// root cause's actual live confirmation - that a real K2 goes from zero
// keyframes across ~300 PLIs to reliable snapshots and a continuous stream
// once this fix ships - is the live verification recorded in
// dev_docs/camera-keyframe-rca.md, not this synthetic test.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/transport/v5/vnet"
)

// installMediaLossFilter installs a vnet.ChunkFilter on testWAN that drops
// every nth RTP media packet (RTP, not RTCP) it observes, counting every
// occurrence including a retransmission - it does not track sequence
// numbers or "already dropped once" state, so a retransmitted packet is
// exactly as likely to be dropped as its original. It passes every RTCP
// packet through unconditionally (so PLI and NACK requests themselves are
// never affected) and every non-RTP/RTCP chunk (STUN, DTLS) through
// unexamined.
//
// vnet.Router.AddChunkFilter has no corresponding remove method, and
// testWAN is shared by every test in this package's binary (vnet_test.go):
// a filter left attached after its own test ends would keep dropping
// traffic for every test that runs afterward, in the same process, forever
// - exactly the kind of cross-test poisoning this helper exists to prevent.
// t.Cleanup flips an atomic "done" flag the filter checks first, so once
// this test ends the filter becomes a permanent no-op (always keeps every
// chunk) instead of continuing to drop packets for unrelated, later tests.
//
// The classification is done directly on the raw chunk bytes, before SRTP
// decryption: SRTP only encrypts the RTP payload, never the 12-byte header
// (RFC 3711), so the version bits and second header byte are readable in
// the clear on every chunk this filter sees, for RTP/RTCP alike. RTCP's
// second byte is an unambiguous packet-type enum (200 SR, 201 RR, 202 SDES,
// 203 BYE, 204 APP, 205 RTPFB/NACK, 206 PSFB/PLI, 207 XR); an RTP packet's
// second byte is instead (marker<<7)|payloadType, and since this package's
// negotiated H264 payload type is always >=96 (RFC 3551's dynamic range),
// even a marker-bit-set RTP packet's byte lands at 224 or higher, well
// outside RTCP's 200-207 - so this switch never misclassifies either
// direction.
func installMediaLossFilter(t *testing.T, n int32) {
	t.Helper()

	var done int32
	var seen int32
	t.Cleanup(func() { atomic.StoreInt32(&done, 1) })

	testWAN.AddChunkFilter(func(c vnet.Chunk) bool {
		if atomic.LoadInt32(&done) != 0 {
			return true // this test has ended; never drop anything again
		}
		data := c.UserData()
		if len(data) < 12 || data[0]&0xC0 != 0x80 {
			return true // not RTP/RTCP-shaped (STUN, DTLS): keep
		}
		switch data[1] {
		case 200, 201, 202, 203, 204, 205, 206, 207:
			return true // RTCP (SR/RR/SDES/BYE/APP/RTPFB/PSFB/XR): keep, always
		}
		return atomic.AddInt32(&seen, 1)%n != 0
	})
}

func TestSessionRecoversKeyframeAfterPersistentPacketLoss(t *testing.T) {
	installMediaLossFilter(t, 4)

	host := startFakePrinter(t, fakePrinterOptions{ignorePLICount: 1000})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	deadline := time.After(8 * time.Second)
	for {
		select {
		case au, ok := <-session.AccessUnits():
			if !ok {
				t.Fatal("access unit stream ended before a keyframe arrived")
			}
			if au.Keyframe {
				return // recovered: a complete keyframe arrived despite persistent packet loss
			}
		case <-deadline:
			t.Fatal("no complete keyframe received within 8s despite persistent (not total) packet loss; " +
				"the one and only GOP this fake printer ever sends was apparently never recovered")
		}
	}
}

// TestSessionTimesOutWhenEveryMediaPacketIsLost is installMediaLossFilter's
// own sanity check: with every single media packet dropped (nothing to
// reassemble from, ever), Session must never fabricate a keyframe. This
// confirms TestSessionRecoversKeyframeAfterPersistentPacketLoss's injected
// loss is real and consequential (something a filter that silently failed
// to drop anything, or a Session that ignored it, would not be caught by).
func TestSessionTimesOutWhenEveryMediaPacketIsLost(t *testing.T) {
	installMediaLossFilter(t, 1)

	host := startFakePrinter(t, fakePrinterOptions{ignorePLICount: 1000})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case au, ok := <-session.AccessUnits():
			if !ok {
				return // access unit stream ended with no keyframe: expected
			}
			if au.Keyframe {
				t.Fatal("got a keyframe despite every media packet being dropped; the loss filter is not effective")
			}
		case <-deadline:
			return // expected: no keyframe ever, with zero media packets ever delivered
		}
	}
}
