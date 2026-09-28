package gcodeinfo

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func buildThumbnailBlock(t *testing.T, w, h int) (block string, png []byte) {
	t.Helper()
	png = makeTestPNG(t, w, h)
	b64 := base64.StdEncoding.EncodeToString(png)
	var b strings.Builder
	fmt.Fprintf(&b, "; thumbnail begin %dx%d %d\n", w, h, len(b64))
	b.WriteString(wrapAsThumbnailComment(b64, 78))
	b.WriteString("; thumbnail end\n")
	return b.String(), png
}

func TestExtractThumbnails_MultipleSizes(t *testing.T) {
	block1, png1 := buildThumbnailBlock(t, 16, 16)
	block2, png2 := buildThumbnailBlock(t, 96, 96)
	head := "; THUMBNAIL_BLOCK_START\n" + block1 + "; THUMBNAIL_BLOCK_END\n" +
		"; THUMBNAIL_BLOCK_START\n" + block2 + "; THUMBNAIL_BLOCK_END\n"

	thumbs, metas, warnings := extractThumbnails(head)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(thumbs) != 2 {
		t.Fatalf("len(thumbs) = %d, want 2", len(thumbs))
	}
	if thumbs[0].Width != 16 || thumbs[0].Height != 16 || string(thumbs[0].Data) != string(png1) {
		t.Errorf("first thumbnail mismatch: %+v", thumbs[0])
	}
	if thumbs[1].Width != 96 || thumbs[1].Height != 96 || string(thumbs[1].Data) != string(png2) {
		t.Errorf("second thumbnail mismatch: %+v", thumbs[1])
	}
	if len(metas) != 2 || metas[0].Truncated || metas[1].Truncated {
		t.Errorf("metas = %+v", metas)
	}
}

func TestExtractThumbnails_MissingEndMarker(t *testing.T) {
	block, _ := buildThumbnailBlock(t, 32, 32)
	// Drop the "thumbnail end" line entirely, simulating data cut off
	// before the terminator was written.
	cut := strings.Index(block, "; thumbnail end")
	head := "; THUMBNAIL_BLOCK_START\n" + block[:cut]

	thumbs, _, warnings := extractThumbnails(head)
	if len(thumbs) != 1 {
		t.Fatalf("len(thumbs) = %d, want 1", len(thumbs))
	}
	if !thumbs[0].Truncated {
		t.Error("thumbnail with no end marker should be Truncated")
	}
	if len(warnings) == 0 {
		t.Error("expected a warning about the truncated thumbnail")
	}
}

func TestExtractThumbnails_DeclaredSizeMismatch(t *testing.T) {
	// A thumbnail whose "thumbnail end" is present but whose collected data
	// is shorter than the size declared on the begin line (as would happen
	// if a byte range cut lines out of the middle) must still be flagged.
	head := "; thumbnail begin 32x32 999999\n; aGVsbG8=\n; thumbnail end\n"
	thumbs, _, _ := extractThumbnails(head)
	if len(thumbs) != 1 {
		t.Fatalf("len(thumbs) = %d, want 1", len(thumbs))
	}
	if !thumbs[0].Truncated {
		t.Error("a declared-size mismatch should be reported as Truncated")
	}
}

func TestExtractThumbnails_OversizedIHDRRejected(t *testing.T) {
	// A crafted thumbnail whose IHDR claims a huge canvas, even though the
	// file itself is only a couple dozen bytes, must be rejected rather
	// than passed on as if it were a legitimate preview image.
	fake := makeOversizedIHDRPNG(t, 50000, 50000)
	b64 := base64.StdEncoding.EncodeToString(fake)
	head := fmt.Sprintf("; thumbnail begin 32x32 %d\n%s; thumbnail end\n", len(b64), wrapAsThumbnailComment(b64, 78))

	thumbs, metas, warnings := extractThumbnails(head)
	if len(thumbs) != 1 {
		t.Fatalf("len(thumbs) = %d, want 1", len(thumbs))
	}
	if !thumbs[0].Truncated {
		t.Error("an oversized thumbnail should be reported as Truncated")
	}
	if thumbs[0].Data != nil {
		t.Errorf("an oversized thumbnail should carry no Data, got %d bytes", len(thumbs[0].Data))
	}
	if !metas[0].Truncated {
		t.Error("ThumbnailMeta for an oversized thumbnail should be Truncated")
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning about the oversized thumbnail")
	}
	if !strings.Contains(warnings[0], "oversized") && !strings.Contains(warnings[0], "rejected") {
		t.Errorf("warning should explain the oversize rejection, got %q", warnings[0])
	}
}

func TestExtractThumbnails_DeclaredActualSizeMismatchWarns(t *testing.T) {
	// The "thumbnail begin WxH size" comment line's declared dimensions
	// are only a hint; when the actual decoded PNG disagrees, the actual
	// size must win and a warning must explain the discrepancy.
	png := makeTestPNG(t, 16, 16)
	b64 := base64.StdEncoding.EncodeToString(png)
	head := fmt.Sprintf("; thumbnail begin 32x32 %d\n%s; thumbnail end\n", len(b64), wrapAsThumbnailComment(b64, 78))

	thumbs, metas, warnings := extractThumbnails(head)
	if len(thumbs) != 1 {
		t.Fatalf("len(thumbs) = %d, want 1", len(thumbs))
	}
	if thumbs[0].Width != 16 || thumbs[0].Height != 16 {
		t.Errorf("thumbnail dims = %dx%d, want the actual 16x16, not the declared 32x32", thumbs[0].Width, thumbs[0].Height)
	}
	if thumbs[0].Truncated {
		t.Error("a declared/actual size mismatch alone should not be reported as Truncated")
	}
	if string(thumbs[0].Data) != string(png) {
		t.Error("thumbnail data should still be kept when only the declared size disagrees")
	}
	if metas[0].Width != 16 || metas[0].Height != 16 {
		t.Errorf("meta dims = %dx%d, want 16x16", metas[0].Width, metas[0].Height)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning about the declared/actual size mismatch")
	}
}

func TestExtractThumbnails_NoThumbnails(t *testing.T) {
	thumbs, metas, warnings := extractThumbnails("G28\nG1 X1 Y1\n")
	if len(thumbs) != 0 || len(metas) != 0 || len(warnings) != 0 {
		t.Errorf("expected no thumbnails, got %+v %+v %+v", thumbs, metas, warnings)
	}
}

func TestDecodeBase64Lenient(t *testing.T) {
	full := base64.StdEncoding.EncodeToString([]byte("the quick brown fox"))
	if data, err := decodeBase64Lenient(full); err != nil || string(data) != "the quick brown fox" {
		t.Fatalf("full decode failed: %v %v", data, err)
	}
	// Cut mid-stream: not a multiple of 4, missing padding.
	cut := full[:len(full)-3]
	if _, err := decodeBase64Lenient(cut); err != nil {
		t.Errorf("lenient decode of a cut string should still recover a prefix, got error: %v", err)
	}
	if _, err := decodeBase64Lenient(""); err == nil {
		t.Error("decoding an empty string should error")
	}
	if _, err := decodeBase64Lenient("!!!!not base64!!!!"); err == nil {
		t.Error("decoding pure garbage should error")
	}
}
