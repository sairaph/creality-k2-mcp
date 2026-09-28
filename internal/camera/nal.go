package camera

import "fmt"

// H.264 NAL unit types this package cares about.
const (
	nalTypeSPS = 7
	nalTypeIDR = 5
	nalTypePPS = 8
)

// splitAnnexB splits an Annex-B byte stream into individual NAL units, each
// returned including its start code (3 or 4 bytes). The samplebuilder
// output this package feeds in here always carries 4-byte start codes (see
// github.com/pion/rtp/codecs.H264Packet), but this also tolerates 3-byte
// start codes for robustness.
func splitAnnexB(data []byte) [][]byte {
	var starts []int
	for i := 0; i+2 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return nil
	}

	units := make([][]byte, 0, len(starts))
	for i, start := range starts {
		end := len(data)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		begin := start
		if begin > 0 && data[begin-1] == 0 {
			begin--
		}
		units = append(units, data[begin:end])
	}
	return units
}

// stripStartCode removes a NAL unit's leading Annex-B start code (3 or 4
// bytes), returning the NAL header byte followed by its RBSP payload.
func stripStartCode(unit []byte) []byte {
	if len(unit) >= 4 && unit[0] == 0 && unit[1] == 0 && unit[2] == 0 && unit[3] == 1 {
		return unit[4:]
	}
	if len(unit) >= 3 && unit[0] == 0 && unit[1] == 0 && unit[2] == 1 {
		return unit[3:]
	}
	return unit
}

// nalType returns the H.264 NAL unit type of unit, which must include its
// Annex-B start code. Returns -1 if unit is too short to contain a NAL
// header after its start code.
func nalType(unit []byte) int {
	payload := stripStartCode(unit)
	if len(payload) == 0 {
		return -1
	}
	return int(payload[0] & 0x1f)
}

// deEmulate removes H.264 emulation prevention bytes (the 0x03 in every
// 0x00 0x00 0x03 sequence) from an RBSP payload, as required before bit
// parsing it per the H.264 spec's emulation_prevention_three_byte.
func deEmulate(rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp))
	zeroRun := 0
	for _, b := range rbsp {
		if zeroRun >= 2 && b == 0x03 {
			zeroRun = 0
			continue
		}
		if b == 0x00 {
			zeroRun++
		} else {
			zeroRun = 0
		}
		out = append(out, b)
	}
	return out
}

// bitReader reads H.264 RBSP bits, including Exp-Golomb (ue/se) codes. A
// read past the end of the data sets err (checked via bitReader.error) and
// returns zero from then on, rather than panicking, since SPS parsing takes
// network-sourced bytes.
type bitReader struct {
	data []byte
	pos  int
	err  error
}

func newBitReader(data []byte) *bitReader {
	return &bitReader{data: data}
}

func (r *bitReader) error() error {
	return r.err
}

func (r *bitReader) bit() uint32 {
	if r.err != nil {
		return 0
	}
	byteIdx := r.pos / 8
	if byteIdx >= len(r.data) {
		r.err = fmt.Errorf("bit reader: exhausted after %d bits", r.pos)
		return 0
	}
	bitIdx := uint(7 - r.pos%8)
	b := (r.data[byteIdx] >> bitIdx) & 1
	r.pos++
	return uint32(b)
}

// u reads an n-bit unsigned integer, most significant bit first.
func (r *bitReader) u(n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v = (v << 1) | r.bit()
	}
	return v
}

// ue reads an Exp-Golomb coded unsigned integer (ue(v) in the H.264 spec).
func (r *bitReader) ue() uint32 {
	leadingZero := 0
	for r.bit() == 0 {
		if r.err != nil {
			return 0
		}
		leadingZero++
		if leadingZero > 32 {
			r.err = fmt.Errorf("bit reader: exp-golomb code too long")
			return 0
		}
	}
	if leadingZero == 0 {
		return 0
	}
	return (uint32(1) << uint(leadingZero)) - 1 + r.u(leadingZero)
}

// se reads an Exp-Golomb coded signed integer (se(v) in the H.264 spec).
func (r *bitReader) se() int32 {
	codeNum := r.ue()
	if codeNum%2 == 0 {
		return -int32(codeNum / 2)
	}
	return int32((codeNum + 1) / 2)
}

// skipScalingList consumes a scaling list of the given size (8 or 64
// entries) without keeping its values, per the H.264 spec's
// scaling_list() syntax. Only reached for high-profile SPS NAL units,
// which the K2's baseline-profile stream (42e01f) never sends, but is
// implemented for correctness against any future firmware that does.
func skipScalingList(r *bitReader, size int) {
	lastScale, nextScale := int32(8), int32(8)
	for i := 0; i < size && r.err == nil; i++ {
		if nextScale != 0 {
			deltaScale := r.se()
			nextScale = (lastScale + deltaScale + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
}

// highProfileIDCs lists the profile_idc values whose SPS carries the
// extended chroma/bit-depth/scaling-list fields (H.264 spec 7.3.2.1.1).
var highProfileIDCs = map[uint32]bool{
	100: true, 110: true, 122: true, 244: true, 44: true,
	83: true, 86: true, 118: true, 128: true, 138: true,
	139: true, 134: true, 135: true,
}

// spsInfo is what this package extracts from an H.264 SPS NAL unit.
type spsInfo struct {
	// ProfileLevelID is the hex encoding of the SPS's own profile_idc,
	// constraint flag byte and level_idc: the exact three bytes an SDP
	// fmtp's profile-level-id parameter encodes, read directly from the
	// bitstream instead of from SDP. The K2's SDP answer is not reliable
	// for this: it advertises profile-level-id=42e01f (Baseline) while the
	// stream's own SPS has been observed to actually be Main profile
	// (4d001f) in practice, so this package always derives it from the
	// SPS and never trusts or hardcodes a profile from SDP.
	ProfileLevelID string
	Width          int
	Height         int
}

// parseSPS parses an SPS NAL unit (including its Annex-B start code) and
// returns its profile-level-id and coded picture width and height in
// pixels, applying any frame cropping. It supports baseline/main/extended
// profiles fully and the high profiles' extra header fields well enough to
// skip past them.
func parseSPS(nal []byte) (spsInfo, error) {
	payload := stripStartCode(nal)
	if len(payload) < 4 {
		return spsInfo{}, fmt.Errorf("camera: sps nal unit too short")
	}
	if int(payload[0]&0x1f) != nalTypeSPS {
		return spsInfo{}, fmt.Errorf("camera: not an sps nal unit")
	}

	// profile-level-id is exactly these three raw bytes (RFC 6184 8.1),
	// read before any emulation-prevention removal: profile_idc and
	// level_idc are always outside the range that would ever need
	// escaping, so this is both simpler and matches how encoders and SDP
	// tooling define the value.
	profileLevelID := fmt.Sprintf("%02x%02x%02x", payload[1], payload[2], payload[3])

	rbsp := deEmulate(payload[1:])
	r := newBitReader(rbsp)

	profileIdc := r.u(8)
	r.u(8) // constraint_set flags + reserved bits
	r.u(8) // level_idc
	r.ue() // seq_parameter_set_id

	chromaFormatIdc := uint32(1)
	if highProfileIDCs[profileIdc] {
		chromaFormatIdc = r.ue()
		if chromaFormatIdc == 3 {
			r.u(1) // separate_colour_plane_flag
		}
		r.ue() // bit_depth_luma_minus8
		r.ue() // bit_depth_chroma_minus8
		r.u(1) // qpprime_y_zero_transform_bypass_flag
		if r.u(1) == 1 {
			count := 8
			if chromaFormatIdc == 3 {
				count = 12
			}
			for i := 0; i < count; i++ {
				if r.u(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}

	r.ue() // log2_max_frame_num_minus4
	picOrderCntType := r.ue()
	switch picOrderCntType {
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.u(1) // delta_pic_order_always_zero_flag
		r.se() // offset_for_non_ref_pic
		r.se() // offset_for_top_to_bottom_field
		numRefFrames := r.ue()
		for i := uint32(0); i < numRefFrames; i++ {
			r.se() // offset_for_ref_frame[i]
		}
	}

	r.ue() // max_num_ref_frames
	r.u(1) // gaps_in_frame_num_value_allowed_flag
	picWidthInMbsMinus1 := r.ue()
	picHeightInMapUnitsMinus1 := r.ue()
	frameMbsOnlyFlag := r.u(1)
	if frameMbsOnlyFlag == 0 {
		r.u(1) // mb_adaptive_frame_field_flag
	}
	r.u(1) // direct_8x8_inference_flag

	var cropLeft, cropRight, cropTop, cropBottom uint32
	if r.u(1) == 1 { // frame_cropping_flag
		cropLeft = r.ue()
		cropRight = r.ue()
		cropTop = r.ue()
		cropBottom = r.ue()
	}

	if r.error() != nil {
		return spsInfo{}, fmt.Errorf("camera: parse sps: %w", r.error())
	}

	subWidthC, subHeightC := uint32(2), uint32(2)
	if chromaFormatIdc == 0 || chromaFormatIdc == 3 {
		subWidthC, subHeightC = 1, 1
	}

	frameHeightInMbs := (2 - frameMbsOnlyFlag) * (picHeightInMapUnitsMinus1 + 1)

	w := (picWidthInMbsMinus1+1)*16 - subWidthC*(cropLeft+cropRight)
	h := frameHeightInMbs*16 - subHeightC*(2-frameMbsOnlyFlag)*(cropTop+cropBottom)

	if w == 0 || h == 0 {
		return spsInfo{}, fmt.Errorf("camera: sps produced non-positive dimensions %dx%d", w, h)
	}
	return spsInfo{ProfileLevelID: profileLevelID, Width: int(w), Height: int(h)}, nil
}
