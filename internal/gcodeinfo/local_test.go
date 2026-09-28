package gcodeinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempFile(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.gcode")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func TestInspectLocalGCode_SmallFile(t *testing.T) {
	png := makeTestPNG(t, 16, 16)
	content := buildCrealityHead(png) + buildCrealityTail()
	p := writeTempFile(t, []byte(content))

	result, thumbs, err := InspectLocalGCode(p, ReadLimits{})
	if err != nil {
		t.Fatalf("InspectLocalGCode: %v", err)
	}
	if result.Path != p {
		t.Errorf("Path = %q, want %q", result.Path, p)
	}
	if result.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d", result.SizeBytes, len(content))
	}
	if result.Dialect != DialectCrealityPrint {
		t.Errorf("Dialect = %q", result.Dialect)
	}
	if result.Estimate == nil || result.Estimate.PrinterModel != "K2 Plus" {
		t.Errorf("Estimate not read from a whole-file-in-head small file: %+v", result.Estimate)
	}
	if len(thumbs) != 1 {
		t.Fatalf("len(thumbs) = %d, want 1", len(thumbs))
	}
}

func TestInspectLocalGCode_BoundedReads(t *testing.T) {
	head := "; a harmless head with no distinguishing fields\n"
	filler := strings.Repeat("X", 3<<20) // 3 MiB, well past the default 1 MiB head/tail windows
	middle := "\n; total layer number: 999\n"
	tail := "; printer_model = K2 Plus\n"
	content := head + filler + middle + filler + tail
	p := writeTempFile(t, []byte(content))

	result, _, err := InspectLocalGCode(p, ReadLimits{})
	if err != nil {
		t.Fatalf("InspectLocalGCode: %v", err)
	}
	if result.TotalLayers != nil {
		t.Errorf("TotalLayers = %v, want nil: the middle-of-file marker must not be read by a bounded head/tail read", result.TotalLayers)
	}
	if result.Estimate == nil || result.Estimate.PrinterModel != "K2 Plus" {
		t.Errorf("the tail marker should still be read: Estimate = %+v", result.Estimate)
	}
}

func TestInspectLocalGCode_CustomLimits(t *testing.T) {
	headFiller := strings.Repeat("A", 500)
	headMarker := "; total layer number: 5\n"
	tailFiller := strings.Repeat("Y", 500)
	tailMarker := "; printer_model = K2 Plus\n"
	content := headFiller + headMarker + tailFiller + tailMarker
	p := writeTempFile(t, []byte(content))

	// A 10-byte head window ends well inside headFiller, before headMarker
	// even starts; a 200-byte tail window ends well inside tailFiller,
	// comfortably clear of headMarker on the other side, and still covers
	// tailMarker at the very end.
	result, _, err := InspectLocalGCode(p, ReadLimits{MaxHeadBytes: 10, MaxTailBytes: 200})
	if err != nil {
		t.Fatalf("InspectLocalGCode: %v", err)
	}
	if result.TotalLayers != nil {
		t.Errorf("TotalLayers = %v, want nil: a 10-byte head window should not reach headMarker", result.TotalLayers)
	}
	if result.Estimate == nil || result.Estimate.PrinterModel != "K2 Plus" {
		t.Errorf("the tail window should still reach tailMarker: Estimate = %+v", result.Estimate)
	}
}

func TestInspectLocalGCode_NotFound(t *testing.T) {
	_, _, err := InspectLocalGCode(filepath.Join(t.TempDir(), "does-not-exist.gcode"), ReadLimits{})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestInspectLocalGCode_Directory(t *testing.T) {
	_, _, err := InspectLocalGCode(t.TempDir(), ReadLimits{})
	if err == nil {
		t.Fatal("expected an error when given a directory")
	}
}
