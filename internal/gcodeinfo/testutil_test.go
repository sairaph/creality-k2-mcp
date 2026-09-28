package gcodeinfo

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// makeTestPNG renders a tiny solid-colour PNG for use as a synthetic
// thumbnail, matching what a real slicer would embed (base64 PNG bytes),
// without depending on any binary fixture file.
func makeTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 40, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

// makeOversizedIHDRPNG builds the smallest possible byte stream that
// image.DecodeConfig will recognise as a PNG - the 8-byte signature plus a
// complete IHDR chunk header and body, no IDAT and no trailing CRC - but
// whose IHDR claims an implausibly large width and height. This is what a
// crafted or corrupted thumbnail looks like: a tiny file that nonetheless
// decodes (successfully, per the PNG spec, since DecodeConfig never reads
// past IHDR) to a canvas far past what any real slicer thumbnail would use.
func makeOversizedIHDRPNG(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	var lengthAndType [8]byte
	lengthAndType[0], lengthAndType[1], lengthAndType[2], lengthAndType[3] = 0, 0, 0, 13
	copy(lengthAndType[4:8], "IHDR")
	buf.Write(lengthAndType[:])
	var ihdr [13]byte
	ihdr[0] = byte(width >> 24)
	ihdr[1] = byte(width >> 16)
	ihdr[2] = byte(width >> 8)
	ihdr[3] = byte(width)
	ihdr[4] = byte(height >> 24)
	ihdr[5] = byte(height >> 16)
	ihdr[6] = byte(height >> 8)
	ihdr[7] = byte(height)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 6  // colour type: truecolor with alpha
	ihdr[10] = 0 // compression method
	ihdr[11] = 0 // filter method
	ihdr[12] = 0 // interlace method
	buf.Write(ihdr[:])
	return buf.Bytes()
}

// wrapAsThumbnailComment renders base64-encoded data as a THUMBNAIL_BLOCK
// body: one gcode comment line per commentWidth base64 characters, matching
// the real Creality Print / OrcaSlicer layout observed in
// references/K2_Series_Klipper/gcodes/F021/3DBench_PLA_14m.gcode.
func wrapAsThumbnailComment(b64 string, commentWidth int) string {
	var b strings.Builder
	for len(b64) > 0 {
		n := commentWidth
		if n > len(b64) {
			n = len(b64)
		}
		b.WriteString("; ")
		b.WriteString(b64[:n])
		b.WriteString("\n")
		b64 = b64[n:]
	}
	return b.String()
}
