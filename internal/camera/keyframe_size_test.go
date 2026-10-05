package camera

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// Field feedback item 3 (dev_docs/field-camera-transport-analysis.md): with
// the chamber light on, the K2's IDR spans 117-118 RTP packets, and the old
// samplebuilder bound of 100 packets could never complete it. These tests
// drive the same samplebuilder and H.264 depacketizer Session uses, with the
// same bound, over a synthetic 15 fps stream.

// rtpStream builds packets for a run of access units: an IDR of idrPackets
// FU-A fragments (after a separate SPS and PPS packet, as the printer sends
// them) followed by small P-frames, one RTP timestamp per access unit.
type rtpStream struct {
	seq uint16
	ts  uint32
}

func (s *rtpStream) packet(payload []byte, marker bool) *rtp.Packet {
	p := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: s.seq, Timestamp: s.ts, Marker: marker},
		Payload: payload,
	}
	s.seq++
	return p
}

// fuA returns the FU-A fragments of one NAL unit of type nalType split into n
// fragments.
func fuA(nalType byte, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		header := nalType
		if i == 0 {
			header |= 0x80 // start
		}
		if i == n-1 {
			header |= 0x40 // end
		}
		out[i] = append([]byte{0x60 | 28, header}, make([]byte, 500)...)
	}
	return out
}

func (s *rtpStream) idr(n int) []*rtp.Packet {
	pkts := []*rtp.Packet{
		s.packet([]byte{0x67, 0x42, 0xe0, 0x1f}, false), // SPS
		s.packet([]byte{0x68, 0xce, 0x3c, 0x80}, false), // PPS
	}
	frags := fuA(5, n-2)
	for i, f := range frags {
		pkts = append(pkts, s.packet(f, i == len(frags)-1))
	}
	s.ts += 90000 / 15
	return pkts
}

func (s *rtpStream) pFrame() []*rtp.Packet {
	frags := fuA(1, 3)
	pkts := make([]*rtp.Packet, len(frags))
	for i, f := range frags {
		pkts[i] = s.packet(f, i == len(frags)-1)
	}
	s.ts += 90000 / 15
	return pkts
}

// keyframesBuilt pushes pkts through a samplebuilder with the production
// bound and returns how many popped samples hold an IDR, and the packet count
// packetCounter reported for each.
func keyframesBuilt(pkts []*rtp.Packet) (int, []int) {
	builder := samplebuilder.New(samplebuilderMaxLate, &codecs.H264Packet{}, 90000)
	var counts packetCounter
	n := 0
	var sizes []int
	for _, p := range pkts {
		counts.add(p.Timestamp)
		builder.Push(p)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			packets := counts.take(sample.PacketTimestamp)
			for _, u := range splitAnnexB(sample.Data) {
				if nalType(u) == nalTypeIDR {
					n++
					sizes = append(sizes, packets)
					break
				}
			}
		}
	}
	return n, sizes
}

func TestLargeKeyframesComplete(t *testing.T) {
	for _, idrPackets := range []int{13, 117, 118, 400} {
		var s rtpStream
		var pkts []*rtp.Packet
		for gop := 0; gop < 3; gop++ {
			pkts = append(pkts, s.idr(idrPackets)...)
			for i := 0; i < 29; i++ {
				pkts = append(pkts, s.pFrame()...)
			}
		}
		// The last access unit is only popped once a later one starts.
		pkts = append(pkts, s.pFrame()...)
		n, sizes := keyframesBuilt(pkts)
		if n != 3 {
			t.Errorf("IDR of %d packets: %d of 3 keyframes completed", idrPackets, n)
			continue
		}
		for _, got := range sizes {
			if got != idrPackets {
				t.Errorf("IDR of %d packets: packet count reported %d", idrPackets, got)
			}
		}
	}
}

// One lost packet that NACK never repairs loses that keyframe only; the next
// one completes.
func TestKeyframeAfterAnUnrepairedLoss(t *testing.T) {
	var s rtpStream
	first := s.idr(117)
	var pkts []*rtp.Packet
	pkts = append(pkts, first[:60]...)
	pkts = append(pkts, first[61:]...)
	for i := 0; i < 29; i++ {
		pkts = append(pkts, s.pFrame()...)
	}
	pkts = append(pkts, s.idr(117)...)
	for i := 0; i < 200; i++ {
		pkts = append(pkts, s.pFrame()...)
	}
	if n, _ := keyframesBuilt(pkts); n != 1 {
		t.Fatalf("%d keyframes completed, want 1 (the one after the loss)", n)
	}
}
