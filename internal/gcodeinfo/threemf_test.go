package gcodeinfo

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testModelXML = `<?xml version="1.0" encoding="UTF-8"?>
<model unit="millimeter" xmlns="http://schemas.microsoft.com/3dmanufacturing/core/2015/02">
  <resources>
    <object id="1" type="model" name="Cube"><mesh/></object>
    <object id="2" type="model" name="Cylinder"><mesh/></object>
  </resources>
  <build>
    <item objectid="1" transform="1 0 0 0 1 0 0 0 1 0 0 0"/>
    <item objectid="2" transform="1 0 0 0 1 0 0 0 1 0 0 0"/>
  </build>
</model>`

const testSliceInfoXML = `<?xml version="1.0" encoding="UTF-8"?>
<config>
  <plate>
    <metadata key="index" value="1"/>
    <metadata key="prediction" value="1234"/>
    <metadata key="weight" value="12.3"/>
    <object identify_id="1"/>
  </plate>
</config>`

func testProjectSettingsJSON() string {
	return `{
		"printer_model": "Creality K2 Plus",
		"nozzle_diameter": ["0.4"],
		"filament_type": ["PLA"],
		"filament_colour": ["#00FF00"],
		"layer_height": "0.2",
		"nozzle_temperature": ["220"],
		"hot_plate_temp": ["55"],
		"some_internal_key_not_on_the_allowlist": "should not appear in the filtered summary"
	}`
}

// buildTestZip writes entries (path -> content) into a new zip file under
// t.TempDir() using the standard Deflate-compressing Create, and returns
// its path.
func buildTestZip(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.3mf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return p
}

func fullTestEntries(t *testing.T) map[string][]byte {
	return map[string][]byte{
		"3D/3dmodel.model":                 []byte(testModelXML),
		"Metadata/project_settings.config": []byte(testProjectSettingsJSON()),
		"Metadata/slice_info.config":       []byte(testSliceInfoXML),
		"Metadata/plate_1.png":             makeTestPNG(t, 64, 64),
		"Metadata/thumbnail.png":           makeTestPNG(t, 128, 128),
	}
}

func TestInspectLocal3MF_Basic(t *testing.T) {
	p := buildTestZip(t, fullTestEntries(t))

	info, thumbs, err := InspectLocal3MF(p, ThreeMFOptions{})
	if err != nil {
		t.Fatalf("InspectLocal3MF: %v", err)
	}

	if info.Dialect != DialectCrealityPrint {
		t.Errorf("Dialect = %q, want %q", info.Dialect, DialectCrealityPrint)
	}
	if !info.IsK2Project {
		t.Error("IsK2Project should be true for printer_model containing K2")
	}
	if info.PrinterModel != "Creality K2 Plus" {
		t.Errorf("PrinterModel = %q", info.PrinterModel)
	}
	if info.NozzleDiameter == nil || *info.NozzleDiameter != 0.4 {
		t.Errorf("NozzleDiameter = %v, want 0.4", info.NozzleDiameter)
	}
	if info.FilamentType != "PLA" {
		t.Errorf("FilamentType = %q", info.FilamentType)
	}
	if info.FilamentColour != "#00FF00" {
		t.Errorf("FilamentColour = %q", info.FilamentColour)
	}
	if info.LayerHeight == nil || *info.LayerHeight != 0.2 {
		t.Errorf("LayerHeight = %v", info.LayerHeight)
	}
	if info.NozzleTemp == nil || *info.NozzleTemp != 220 {
		t.Errorf("NozzleTemp = %v", info.NozzleTemp)
	}
	if info.BedTemp == nil || *info.BedTemp != 55 {
		t.Errorf("BedTemp = %v", info.BedTemp)
	}

	if len(info.Objects) != 2 {
		t.Fatalf("len(Objects) = %d, want 2", len(info.Objects))
	}
	if info.Objects[0].Name != "Cube" || info.Objects[1].Name != "Cylinder" {
		t.Errorf("Objects = %+v", info.Objects)
	}

	if len(info.Plates) != 1 {
		t.Fatalf("len(Plates) = %d, want 1", len(info.Plates))
	}
	plate := info.Plates[0]
	if plate.Index != 1 || plate.ThumbnailLabel != "plate_1" {
		t.Errorf("Plate = %+v", plate)
	}
	if plate.PredictionSeconds == nil || *plate.PredictionSeconds != 1234 {
		t.Errorf("PredictionSeconds = %v", plate.PredictionSeconds)
	}
	if plate.WeightG == nil || *plate.WeightG != 12.3 {
		t.Errorf("WeightG = %v", plate.WeightG)
	}
	if len(plate.ObjectIDs) != 1 || plate.ObjectIDs[0] != "1" {
		t.Errorf("ObjectIDs = %v", plate.ObjectIDs)
	}

	if info.SettingsCount != 8 {
		t.Errorf("SettingsCount = %d, want 8", info.SettingsCount)
	}
	if info.SettingsFull {
		t.Error("SettingsFull should be false by default")
	}
	if _, present := info.Settings["some_internal_key_not_on_the_allowlist"]; present {
		t.Error("filtered settings summary should not include a non-allowlisted key")
	}
	if info.Settings["printer_model"] != "Creality K2 Plus" {
		t.Errorf("Settings[printer_model] = %q", info.Settings["printer_model"])
	}

	if len(thumbs) != 2 {
		t.Fatalf("len(thumbs) = %d, want 2", len(thumbs))
	}
	byLabel := map[string]Thumbnail{}
	for _, th := range thumbs {
		byLabel[th.Label] = th
	}
	if pt, ok := byLabel["plate_1"]; !ok || pt.Width != 64 || pt.Height != 64 || pt.Truncated {
		t.Errorf("plate_1 thumbnail = %+v, ok=%v", pt, ok)
	}
	if tn, ok := byLabel["thumbnail"]; !ok || tn.Width != 128 || tn.Height != 128 || tn.Truncated {
		t.Errorf("thumbnail = %+v, ok=%v", tn, ok)
	}
	if len(info.Thumbnails) != 2 {
		t.Errorf("ThreeMFInfo.Thumbnails = %+v", info.Thumbnails)
	}
}

func TestInspectLocal3MF_FullSettings(t *testing.T) {
	p := buildTestZip(t, fullTestEntries(t))

	info, _, err := InspectLocal3MF(p, ThreeMFOptions{FullSettings: true})
	if err != nil {
		t.Fatalf("InspectLocal3MF: %v", err)
	}
	if !info.SettingsFull {
		t.Error("SettingsFull should be true when requested")
	}
	if info.SettingsCount != 8 {
		t.Errorf("SettingsCount = %d, want 8", info.SettingsCount)
	}
	if _, present := info.Settings["some_internal_key_not_on_the_allowlist"]; !present {
		t.Error("full dump should include every key, including ones outside the allowlist")
	}
}

func TestInspectLocal3MF_FullSettingsCap(t *testing.T) {
	entries := fullTestEntries(t)
	// Replace project_settings.config with one that has many keys, so the
	// full dump's byte cap actually has to kick in.
	settingsBuf := strings.Builder{}
	settingsBuf.WriteString("{")
	for i := 0; i < 200; i++ {
		if i > 0 {
			settingsBuf.WriteString(",")
		}
		fmt.Fprintf(&settingsBuf, `"key_%03d": "some moderately long value number %03d"`, i, i)
	}
	settingsBuf.WriteString("}")
	entries["Metadata/project_settings.config"] = []byte(settingsBuf.String())

	p := buildTestZip(t, entries)
	info, _, err := InspectLocal3MF(p, ThreeMFOptions{FullSettings: true, MaxSettingsBytes: 500})
	if err != nil {
		t.Fatalf("InspectLocal3MF: %v", err)
	}
	if info.SettingsCount != 200 {
		t.Errorf("SettingsCount = %d, want 200", info.SettingsCount)
	}
	if !info.SettingsTruncated {
		t.Error("SettingsTruncated should be true once the byte cap is hit")
	}
	if len(info.Settings) >= 200 {
		t.Errorf("expected the capped dump to include fewer than all 200 keys, got %d", len(info.Settings))
	}
}

func TestInspectLocal3MF_MissingSettings(t *testing.T) {
	p := buildTestZip(t, map[string][]byte{
		"3D/3dmodel.model": []byte(testModelXML),
	})

	info, thumbs, err := InspectLocal3MF(p, ThreeMFOptions{})
	if err != nil {
		t.Fatalf("InspectLocal3MF should not fail on a 3MF with no project_settings.config: %v", err)
	}
	if info.Dialect != DialectUnknown {
		t.Errorf("Dialect = %q, want %q", info.Dialect, DialectUnknown)
	}
	if len(info.Objects) != 2 {
		t.Errorf("Objects should still be read: %+v", info.Objects)
	}
	if len(info.Plates) != 0 {
		t.Errorf("Plates = %+v, want none", info.Plates)
	}
	if len(thumbs) != 0 {
		t.Errorf("thumbs = %+v, want none", thumbs)
	}
	foundWarning := false
	for _, w := range info.Warnings {
		if strings.Contains(w, "project_settings.config") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected a warning about the missing project_settings.config, got %v", info.Warnings)
	}
}

func TestInspectLocal3MF_PathTraversal(t *testing.T) {
	cases := []string{"../evil.txt", "/etc/passwd", "a/../../b.txt"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "evil.3mf")
			f, err := os.Create(p)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			zw := zip.NewWriter(f)
			w, err := zw.Create(name)
			if err != nil {
				// archive/zip itself may refuse some malformed names when
				// writing; either refusal (at write time or at read time)
				// satisfies this test's intent, but only a write-time
				// refusal short-circuits the read assertion below.
				zw.Close()
				f.Close()
				return
			}
			_, _ = w.Write([]byte("x"))
			if err := zw.Close(); err != nil {
				t.Fatalf("zip close: %v", err)
			}
			f.Close()

			_, _, err = InspectLocal3MF(p, ThreeMFOptions{})
			if err == nil {
				t.Fatalf("expected InspectLocal3MF to reject a path-traversal entry name %q", name)
			}
		})
	}
}

func TestInspectLocal3MF_BackslashTraversal(t *testing.T) {
	// archive/zip's Writer.Create stores the name verbatim (it does not
	// interpret backslashes as separators), so this reaches
	// checkZipEntryName's backslash check directly.
	p := filepath.Join(t.TempDir(), "evil.3mf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(`..\evil.txt`)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	_, _ = w.Write([]byte("x"))
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	f.Close()

	_, _, err = InspectLocal3MF(p, ThreeMFOptions{})
	if err == nil {
		t.Fatal("expected InspectLocal3MF to reject a backslash path in a zip entry name")
	}
}

func TestInspectLocal3MF_TooManyEntries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "many.3mf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	for i := 0; i < max3MFEntries+1; i++ {
		if _, err := zw.Create(fmt.Sprintf("file_%d.txt", i)); err != nil {
			t.Fatalf("zip create entry %d: %v", i, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	f.Close()

	_, _, err = InspectLocal3MF(p, ThreeMFOptions{})
	if err == nil {
		t.Fatal("expected InspectLocal3MF to reject an archive with too many entries")
	}
}

// writeRawZipEntry writes an entry with a header-declared size that does not
// have to match the actual bytes written, using archive/zip's raw writer.
// This is how the zip-bomb-style tests below fake an enormous declared
// uncompressed size, or an implausible compression ratio, without actually
// writing gigabytes of data: openSafe3MF's checks only ever look at the zip
// header's own declared size fields before any entry is decompressed, which
// is exactly the field a real zip bomb's header lies about.
func writeRawZipEntry(t *testing.T, zw *zip.Writer, name string, method uint16, uncompressedSize, compressedSize uint64, raw []byte) {
	t.Helper()
	fh := &zip.FileHeader{
		Name:               name,
		Method:             method,
		UncompressedSize64: uncompressedSize,
		CompressedSize64:   compressedSize,
	}
	w, err := zw.CreateRaw(fh)
	if err != nil {
		t.Fatalf("CreateRaw %q: %v", name, err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatalf("write raw entry %q: %v", name, err)
	}
}

func TestInspectLocal3MF_EntryTooLarge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge-entry.3mf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	raw := []byte("0123456789")
	// Declare CompressedSize64 equal to UncompressedSize64 (ratio 1) so this
	// test isolates the per-entry size cap from the ratio check.
	size := uint64(max3MFEntryUncompressed) + 1
	writeRawZipEntry(t, zw, "Metadata/huge.bin", zip.Store, size, size, raw)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	f.Close()

	_, _, err = InspectLocal3MF(p, ThreeMFOptions{})
	if err == nil {
		t.Fatal("expected InspectLocal3MF to reject an entry declaring an oversized uncompressed size")
	}
}

func TestInspectLocal3MF_ZipBombRatio(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bomb.3mf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	raw := []byte("0123456789")
	// Declare sizes whose ratio is far past max3MFCompressionRatio, well
	// above minRatioCheckBytes so the check is not skipped as "too small to
	// judge", but still safely under max3MFEntryUncompressed/
	// max3MFTotalUncompressed so this test isolates the ratio check
	// specifically from the two absolute-size checks.
	const declaredUncompressed = 100 * minRatioCheckBytes
	const declaredCompressed = 100
	writeRawZipEntry(t, zw, "Metadata/bomb.bin", zip.Deflate, declaredUncompressed, declaredCompressed, raw)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	f.Close()

	_, _, err = InspectLocal3MF(p, ThreeMFOptions{})
	if err == nil {
		t.Fatal("expected InspectLocal3MF to reject an entry with an implausible compression ratio")
	}
	if !strings.Contains(err.Error(), "ratio") && !strings.Contains(err.Error(), "bomb") {
		t.Errorf("error should mention the ratio/bomb rejection reason, got: %v", err)
	}
}

func TestInspectLocal3MF_TotalUncompressedTooLarge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big-total.3mf")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	raw := []byte("0123456789")
	// Five entries, each declared just under the per-entry cap with a 1:1
	// compression ratio (so neither the per-entry-size nor the ratio check
	// fires), summing to comfortably over the total-uncompressed cap:
	// isolates the total-size check specifically.
	perEntry := uint64(max3MFEntryUncompressed) - 1
	for i := 0; i < 5; i++ {
		writeRawZipEntry(t, zw, fmt.Sprintf("Metadata/part_%d.bin", i), zip.Store, perEntry, perEntry, raw)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	f.Close()

	_, _, err = InspectLocal3MF(p, ThreeMFOptions{})
	if err == nil {
		t.Fatal("expected InspectLocal3MF to reject an archive whose declared total uncompressed size is too large")
	}
}

func TestInspectLocal3MF_OversizedThumbnailRejected(t *testing.T) {
	// A crafted Metadata/thumbnail.png whose IHDR claims a huge canvas,
	// even though the entry itself is tiny, must be rejected rather than
	// passed on as if it were a legitimate preview image.
	entries := map[string][]byte{
		"3D/3dmodel.model":       []byte(testModelXML),
		"Metadata/thumbnail.png": makeOversizedIHDRPNG(t, 50000, 50000),
	}
	p := buildTestZip(t, entries)

	info, thumbs, err := InspectLocal3MF(p, ThreeMFOptions{})
	if err != nil {
		t.Fatalf("InspectLocal3MF: %v", err)
	}
	if len(thumbs) != 1 {
		t.Fatalf("len(thumbs) = %d, want 1", len(thumbs))
	}
	if !thumbs[0].Truncated {
		t.Error("an oversized thumbnail should be reported as Truncated")
	}
	if thumbs[0].Data != nil {
		t.Errorf("an oversized thumbnail should carry no Data, got %d bytes", len(thumbs[0].Data))
	}
	foundWarning := false
	for _, w := range info.Warnings {
		if strings.Contains(w, "oversized") || strings.Contains(w, "rejected") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected a warning about the oversized thumbnail, got %v", info.Warnings)
	}
}

func TestInspectLocal3MF_NotAZip(t *testing.T) {
	p := writeTempFile(t, []byte("this is not a zip file"))
	_, _, err := InspectLocal3MF(p, ThreeMFOptions{})
	if err == nil {
		t.Fatal("expected an error for a file that is not a zip archive")
	}
}
