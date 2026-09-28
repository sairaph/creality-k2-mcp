package fmp4

import (
	"fmt"

	h264c "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// CodecString returns the MSE/RFC 6381 codec string for an H.264 stream
// described by sps, a raw SPS NAL unit: it includes its own one-byte NAL
// header (0x67) but no Annex-B start code, the same form NewWriter expects.
// For a Baseline-compatible stream with profile-level-id 42e01f (this
// project's camera, see dev_docs/plan-v0.1.0.md decision 11), the result is
// "avc1.42e01f".
//
// The three hex-encoded bytes are profile_idc, the constraint-flags/
// reserved byte, and level_idc, read straight out of the SPS, i.e. exactly
// the same bytes RTP's profile-level-id and the AVCDecoderConfigurationRecord
// NewWriter embeds in the init segment's avcC box carry. A browser's
// SourceBuffer needs this string (as part of a full MIME type, e.g.
// `video/mp4; codecs="avc1.42e01f"`) to know it can decode the fragments a
// Writer produces before any of them arrive.
func CodecString(sps []byte) (string, error) {
	var parsed h264c.SPS
	if err := parsed.Unmarshal(sps); err != nil {
		return "", fmt.Errorf("fmp4: parse SPS for codec string: %w", err)
	}

	var constraints byte
	if parsed.ConstraintSet0Flag {
		constraints |= 1 << 7
	}
	if parsed.ConstraintSet1Flag {
		constraints |= 1 << 6
	}
	if parsed.ConstraintSet2Flag {
		constraints |= 1 << 5
	}
	if parsed.ConstraintSet3Flag {
		constraints |= 1 << 4
	}
	if parsed.ConstraintSet4Flag {
		constraints |= 1 << 3
	}
	if parsed.ConstraintSet5Flag {
		constraints |= 1 << 2
	}

	return fmt.Sprintf("avc1.%02x%02x%02x", parsed.ProfileIdc, constraints, parsed.LevelIdc), nil
}
