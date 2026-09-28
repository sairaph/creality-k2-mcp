package camera

import (
	"bytes"
	"testing"
)

func TestSplitAnnexBAndNalType(t *testing.T) {
	// 4-byte start codes, three tiny NAL units: SPS-ish(7), PPS-ish(8), IDR-ish(5).
	data := []byte{0, 0, 0, 1, 0x67, 0xaa, 0, 0, 0, 1, 0x68, 0xbb, 0, 0, 0, 1, 0x65, 0xcc}
	units := splitAnnexB(data)
	if len(units) != 3 {
		t.Fatalf("splitAnnexB: got %d units, want 3", len(units))
	}
	wantTypes := []int{nalTypeSPS, nalTypePPS, nalTypeIDR}
	for i, u := range units {
		if got := nalType(u); got != wantTypes[i] {
			t.Errorf("unit %d: nalType = %d, want %d", i, got, wantTypes[i])
		}
	}
}

func TestSplitAnnexBThreeByteStartCode(t *testing.T) {
	data := []byte{0, 0, 1, 0x67, 0xaa, 0, 0, 1, 0x65, 0xbb}
	units := splitAnnexB(data)
	if len(units) != 2 {
		t.Fatalf("splitAnnexB: got %d units, want 2", len(units))
	}
	if nalType(units[0]) != nalTypeSPS {
		t.Errorf("unit 0: got type %d, want SPS", nalType(units[0]))
	}
	if nalType(units[1]) != nalTypeIDR {
		t.Errorf("unit 1: got type %d, want IDR", nalType(units[1]))
	}
}

func TestSplitAnnexBNoStartCode(t *testing.T) {
	if units := splitAnnexB([]byte{1, 2, 3, 4}); units != nil {
		t.Errorf("splitAnnexB with no start code: got %v, want nil", units)
	}
}

func TestDeEmulate(t *testing.T) {
	in := []byte{0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x02, 0x00, 0x00, 0x01}
	want := []byte{0x00, 0x00, 0x01, 0x00, 0x00, 0x02, 0x00, 0x00, 0x01}
	got := deEmulate(in)
	if !bytes.Equal(got, want) {
		t.Errorf("deEmulate: got %x, want %x", got, want)
	}
}

func TestBitReaderUEAndSE(t *testing.T) {
	// Exp-Golomb stream for ue(v) values 0,1,2,3,4 packed as:
	// 0 -> "1"
	// 1 -> "010"
	// 2 -> "011"
	// 3 -> "00100"
	// 4 -> "00101"
	// bits: 1 010 011 00100 00101 -> pad to bytes
	bitsStr := "1" + "010" + "011" + "00100" + "00101"
	data := bitsToBytes(bitsStr)
	r := newBitReader(data)
	want := []uint32{0, 1, 2, 3, 4}
	for i, w := range want {
		if got := r.ue(); got != w {
			t.Fatalf("ue() call %d: got %d, want %d", i, got, w)
		}
	}
	if r.error() != nil {
		t.Fatalf("unexpected error: %v", r.error())
	}
}

func TestBitReaderExhausted(t *testing.T) {
	r := newBitReader([]byte{0x00})
	r.ue() // consumes all 8 zero bits looking for a terminating 1 bit
	if r.error() == nil {
		t.Fatal("bitReader.ue() past end of data: want error, got nil")
	}
}

// bitsToBytes packs a string of '0'/'1' characters into bytes, most
// significant bit first, padding the final byte with zero bits.
func bitsToBytes(bits string) []byte {
	out := make([]byte, (len(bits)+7)/8)
	for i, c := range bits {
		if c == '1' {
			out[i/8] |= 1 << uint(7-i%8)
		}
	}
	return out
}

// TestParseSPSDimensionsFromSampleClip parses the real SPS from
// decode/testdata/camera_sample_clip.h264 (read here, not copied) and
// checks it against the known dimensions of that clip (confirmed via
// ffprobe, see references/analysis/04-creality-ws-camera.md section 3 and
// internal/camera/decode's own tests): 1280x720.
func TestParseSPSDimensionsFromSampleClip(t *testing.T) {
	gops := loadTestGOPs(t)

	var sps []byte
	for _, gop := range gops {
		for _, n := range gop {
			if n.spsOrPPS && int(n.data[0]&0x1f) == nalTypeSPS {
				sps = n.data
				break
			}
		}
		if sps != nil {
			break
		}
	}
	if sps == nil {
		t.Fatal("sample clip has no SPS NAL unit")
	}

	// parseSPS expects the NAL unit with its Annex-B start code, which
	// loadTestGOPs strips (h264reader.NAL.Data has none); prepend one.
	withStartCode := append([]byte{0, 0, 0, 1}, sps...)

	info, err := parseSPS(withStartCode)
	if err != nil {
		t.Fatalf("parseSPS: %v", err)
	}
	if info.Width != 1280 || info.Height != 720 {
		t.Fatalf("parseSPS: got %dx%d, want 1280x720", info.Width, info.Height)
	}
	// The real stream's SPS is Main profile (4d001f) even though the K2's
	// SDP answer advertises profile-level-id=42e01f (Baseline); this
	// package must always report what the SPS itself says, never the SDP.
	if info.ProfileLevelID != "4d001f" {
		t.Fatalf("parseSPS: got ProfileLevelID %q, want %q", info.ProfileLevelID, "4d001f")
	}
}

func TestParseSPSRejectsNonSPS(t *testing.T) {
	notSPS := []byte{0, 0, 0, 1, 0x65, 0x00, 0x00, 0x00} // NAL type 5 (IDR), not 7 (SPS)
	if _, err := parseSPS(notSPS); err == nil {
		t.Fatal("parseSPS on a non-SPS NAL unit: want error, got nil")
	}
}

func TestParseSPSTruncated(t *testing.T) {
	truncated := []byte{0, 0, 0, 1, 0x67}
	if _, err := parseSPS(truncated); err == nil {
		t.Fatal("parseSPS on truncated input: want error, got nil")
	}
}
