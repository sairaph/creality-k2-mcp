package gcodeinfo

import (
	"archive/zip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Zip safety limits for a local 3MF archive (plan-v0.1.0.md T8: "open the
// zip safely"). These are deliberately generous for a legitimate slicer
// project (a handful of small XML/JSON metadata files plus a few PNG
// thumbnails and the mesh itself) while still refusing an archive shaped
// like a zip bomb: a huge declared uncompressed size, an implausible
// compression ratio, or simply too many entries to be a real 3MF.
const (
	max3MFEntries                     = 4096
	max3MFTotalUncompressed           = 512 << 20 // 512 MiB
	max3MFEntryUncompressed           = 128 << 20 // 128 MiB
	max3MFCompressionRatio            = 300       // uncompressed:compressed, per entry
	minRatioCheckBytes                = 4 << 10   // skip the ratio check for tiny entries
	DefaultMaxFullSettingsBytes int64 = 256 << 10
)

// ThreeMFOptions controls how much settings detail InspectLocal3MF returns.
type ThreeMFOptions struct {
	// FullSettings requests every key from Metadata/project_settings.config
	// (still capped at MaxSettingsBytes of serialised size), instead of the
	// filtered printer/filament/process allowlist InspectLocal3MF returns
	// by default.
	FullSettings bool
	// MaxSettingsBytes caps the full dump's approximate serialised size.
	// Zero uses DefaultMaxFullSettingsBytes. Ignored when FullSettings is
	// false: the filtered allowlist summary is always small enough that no
	// cap is needed.
	MaxSettingsBytes int64
}

func (o ThreeMFOptions) maxSettingsBytes() int64 {
	if o.MaxSettingsBytes > 0 {
		return o.MaxSettingsBytes
	}
	return DefaultMaxFullSettingsBytes
}

// settingsAllowlist is the curated set of printer/filament/process keys
// InspectLocal3MF surfaces by default. It intentionally mirrors the field
// names this package's gcode CONFIG_BLOCK parsing already knows (header.go):
// Creality Print / OrcaSlicer's project_settings.config uses the same key
// vocabulary as the gcode comment dump it is generated alongside.
var settingsAllowlist = []string{
	"printer_model",
	"printer_variant",
	"nozzle_diameter",
	"filament_type",
	"filament_colour",
	"default_filament_colour",
	"filament_density",
	"filament_diameter",
	"nozzle_temperature",
	"nozzle_temperature_initial_layer",
	"hot_plate_temp",
	"hot_plate_temp_initial_layer",
	"bed_temperature",
	"first_layer_bed_temperature",
	"layer_height",
	"initial_layer_height",
	"fill_density",
	"wall_loops",
	"top_shell_layers",
	"bottom_shell_layers",
	"supports_enable",
	"support_type",
	"travel_speed",
	"outer_wall_speed",
}

var (
	platePNGPattern = regexp.MustCompile(`(?i)^Metadata/plate_(\d+)\.png$`)
)

// InspectLocal3MF opens a local 3MF project archive, safely (bounded entry
// count, total uncompressed size, per-entry size, and rejecting path
// traversal names), and returns a yaml-frontmatter-safe summary plus any
// plate/legacy thumbnails as separate decoded images. A 3MF that is not
// from the Creality Print / OrcaSlicer family (no recognisable
// Metadata/project_settings.config) still returns whatever plates, objects
// and thumbnails could be found, with Dialect "unknown" and a warning,
// rather than failing outright (unlike the Python reference server's
// analyze_3mf, which raised on a missing project_settings.config).
func InspectLocal3MF(filePath string, opts ThreeMFOptions) (*ThreeMFInfo, []Thumbnail, error) {
	zr, size, err := openSafe3MF(filePath)
	if err != nil {
		return nil, nil, err
	}
	defer zr.Close()

	info := &ThreeMFInfo{Path: filePath, SizeBytes: size, Dialect: DialectUnknown}
	var thumbs []Thumbnail

	objects, objWarnings := readModelObjects(zr.File)
	info.Objects = objects
	info.Warnings = append(info.Warnings, objWarnings...)

	settings, settingsWarnings := readProjectSettings(zr.File)
	info.Warnings = append(info.Warnings, settingsWarnings...)
	if settings != nil {
		applySettings(info, settings, opts)
	} else {
		info.Warnings = append(info.Warnings, "no Metadata/project_settings.config found; settings and dialect could not be determined")
	}

	plates, plateWarnings := readPlates(zr.File)
	info.Warnings = append(info.Warnings, plateWarnings...)

	plateThumbs, thumbWarnings := readThumbnails(zr.File)
	thumbs = append(thumbs, plateThumbs...)
	info.Warnings = append(info.Warnings, thumbWarnings...)
	for _, t := range plateThumbs {
		info.Thumbnails = append(info.Thumbnails, ThumbnailMeta{
			Width: t.Width, Height: t.Height, Mime: t.Mime, Label: t.Label, Truncated: t.Truncated,
		})
	}

	info.Plates = plates
	return info, thumbs, nil
}

// openSafe3MF opens path as a zip archive and validates it against the zip
// safety limits before any entry is read. It returns the archive's own file
// size (the whole-archive size on disk, not any entry's declared size).
func openSafe3MF(filePath string) (*zip.ReadCloser, int64, error) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		return nil, 0, fmt.Errorf("gcodeinfo: open 3mf %s: %w", filePath, err)
	}

	if len(zr.File) > max3MFEntries {
		zr.Close()
		return nil, 0, fmt.Errorf("gcodeinfo: 3mf %s has %d entries, over the %d limit", filePath, len(zr.File), max3MFEntries)
	}

	var total uint64
	for _, f := range zr.File {
		if err := checkZipEntryName(f.Name); err != nil {
			zr.Close()
			return nil, 0, fmt.Errorf("gcodeinfo: 3mf %s: %w", filePath, err)
		}
		if f.UncompressedSize64 > uint64(max3MFEntryUncompressed) {
			zr.Close()
			return nil, 0, fmt.Errorf("gcodeinfo: 3mf %s: entry %q declares %d bytes uncompressed, over the %d byte limit", filePath, f.Name, f.UncompressedSize64, max3MFEntryUncompressed)
		}
		if f.UncompressedSize64 >= minRatioCheckBytes && f.CompressedSize64 > 0 {
			ratio := f.UncompressedSize64 / f.CompressedSize64
			if ratio > max3MFCompressionRatio {
				zr.Close()
				return nil, 0, fmt.Errorf("gcodeinfo: 3mf %s: entry %q has a %dx compression ratio, refusing as a likely zip bomb", filePath, f.Name, ratio)
			}
		}
		total += f.UncompressedSize64
		if total > uint64(max3MFTotalUncompressed) {
			zr.Close()
			return nil, 0, fmt.Errorf("gcodeinfo: 3mf %s: declared total uncompressed size exceeds the %d byte limit", filePath, max3MFTotalUncompressed)
		}
	}

	fi, statErr := os.Stat(filePath)
	if statErr != nil {
		zr.Close()
		return nil, 0, fmt.Errorf("gcodeinfo: stat 3mf %s: %w", filePath, statErr)
	}
	return zr, fi.Size(), nil
}

// checkZipEntryName rejects an absolute path, a backslash separator (a
// Windows-built zip storing traversal-capable names), or any ".." path
// segment, none of which are legitimate inside a well-formed 3MF.
func checkZipEntryName(name string) error {
	if name == "" {
		return fmt.Errorf("zip entry has an empty name")
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("zip entry %q uses a backslash path separator", name)
	}
	if strings.HasPrefix(name, "/") {
		return fmt.Errorf("zip entry %q is an absolute path", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return fmt.Errorf("zip entry %q contains a path traversal segment", name)
		}
	}
	if cleaned := path.Clean(name); strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("zip entry %q escapes the archive root", name)
	}
	return nil
}

// readEntryCapped reads a zip entry's content, never more than capBytes.
// Truncated is true when the entry's actual content was cut off at
// capBytes (which can happen even after openSafe3MF's declared-size checks,
// since those trust the zip's own header; this is the load-bearing limit).
func readEntryCapped(f *zip.File, capBytes int64) (data []byte, truncated bool, err error) {
	rc, err := f.Open()
	if err != nil {
		return nil, false, fmt.Errorf("open zip entry %q: %w", f.Name, err)
	}
	defer rc.Close()

	limited := io.LimitReader(rc, capBytes+1)
	data, err = io.ReadAll(limited)
	if err != nil {
		return nil, false, fmt.Errorf("read zip entry %q: %w", f.Name, err)
	}
	if int64(len(data)) > capBytes {
		return data[:capBytes], true, nil
	}
	return data, false, nil
}

func findZipEntry(files []*zip.File, name string) *zip.File {
	for _, f := range files {
		if f.Name == name {
			return f
		}
	}
	for _, f := range files {
		if strings.EqualFold(f.Name, name) {
			return f
		}
	}
	return nil
}

// --- 3D/3dmodel.model ---

type model3MFXML struct {
	Resources struct {
		Objects []struct {
			ID   string `xml:"id,attr"`
			Name string `xml:"name,attr"`
			Type string `xml:"type,attr"`
		} `xml:"object"`
	} `xml:"resources"`
}

func readModelObjects(files []*zip.File) ([]ModelObject, []string) {
	f := findZipEntry(files, "3D/3dmodel.model")
	if f == nil {
		return nil, []string{"no 3D/3dmodel.model found in the archive"}
	}
	data, truncated, err := readEntryCapped(f, max3MFEntryUncompressed)
	if err != nil {
		return nil, []string{"could not read 3D/3dmodel.model: " + err.Error()}
	}
	var parsed model3MFXML
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return nil, []string{"could not parse 3D/3dmodel.model as XML: " + err.Error()}
	}
	var warnings []string
	if truncated {
		warnings = append(warnings, "3D/3dmodel.model was truncated at the read cap; the object list may be incomplete")
	}
	objects := make([]ModelObject, 0, len(parsed.Resources.Objects))
	for _, o := range parsed.Resources.Objects {
		objects = append(objects, ModelObject{ID: o.ID, Name: o.Name, Type: o.Type})
	}
	return objects, warnings
}

// --- Metadata/project_settings.config ---

func readProjectSettings(files []*zip.File) (map[string]json.RawMessage, []string) {
	f := findZipEntry(files, "Metadata/project_settings.config")
	if f == nil {
		return nil, nil
	}
	data, truncated, err := readEntryCapped(f, max3MFEntryUncompressed)
	if err != nil {
		return nil, []string{"could not read Metadata/project_settings.config: " + err.Error()}
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, []string{"Metadata/project_settings.config is not a valid JSON object: " + err.Error()}
	}
	var warnings []string
	if truncated {
		warnings = append(warnings, "Metadata/project_settings.config was truncated at the read cap; some settings may be missing")
	}
	return settings, warnings
}

func applySettings(info *ThreeMFInfo, settings map[string]json.RawMessage, opts ThreeMFOptions) {
	info.SettingsCount = len(settings)

	info.PrinterModel = coerceString(settings["printer_model"])
	info.IsK2Project = strings.Contains(strings.ToUpper(info.PrinterModel), "K2")
	info.NozzleDiameter = coerceFloat(settings["nozzle_diameter"])
	info.FilamentType = coerceString(settings["filament_type"])
	info.FilamentColour = firstNonEmpty(coerceString(settings["filament_colour"]), coerceString(settings["default_filament_colour"]))
	info.LayerHeight = coerceFloat(settings["layer_height"])
	info.NozzleTemp = coerceFloat(settings["nozzle_temperature"])
	info.BedTemp = firstNonNilFloat(
		coerceFloat(settings["hot_plate_temp"]),
		coerceFloat(settings["bed_temperature"]),
		coerceFloat(settings["first_layer_bed_temperature"]),
	)

	// Dialect: project_settings.config is written by the whole Creality
	// Print / OrcaSlicer / BambuStudio family, which share this exact
	// key/value shape; printer_model is the only cheap signal this package
	// has to tell them apart.
	switch {
	case strings.Contains(strings.ToUpper(info.PrinterModel), "K2") || strings.Contains(strings.ToLower(info.PrinterModel), "creality"):
		info.Dialect = DialectCrealityPrint
	default:
		info.Dialect = DialectOrcaSlicer
	}

	if opts.FullSettings {
		info.SettingsFull = true
		info.Settings, info.SettingsTruncated = dumpSettings(settings, opts.maxSettingsBytes())
		return
	}

	info.Settings = make(map[string]string, len(settingsAllowlist))
	for _, key := range settingsAllowlist {
		raw, ok := settings[key]
		if !ok {
			continue
		}
		v := coerceString(raw)
		if v != "" {
			info.Settings[key] = v
		}
	}
}

// dumpSettings serialises every settings key as a string value, in sorted
// key order for determinism, stopping once the approximate serialised size
// would exceed maxBytes.
func dumpSettings(settings map[string]json.RawMessage, maxBytes int64) (map[string]string, bool) {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(keys))
	var used int64
	truncated := false
	for _, k := range keys {
		v := coerceString(settings[k])
		cost := int64(len(k) + len(v) + 4)
		if used+cost > maxBytes {
			truncated = true
			break
		}
		out[k] = v
		used += cost
	}
	return out, truncated
}

// coerceString reads a project_settings.config value as display text,
// regardless of whether the slicer wrote it as a JSON string, a per-
// extruder array of strings, a number, or a bool.
func coerceString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return strings.Join(arr, ",")
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b)
	}
	return strings.Trim(string(raw), `"`)
}

func coerceFloat(raw json.RawMessage) *float64 {
	s := coerceString(raw)
	if s == "" {
		return nil
	}
	// A per-extruder value can be a comma-joined list (see coerceString's
	// array case); the first entry is this project's primary extruder.
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil {
		return nil
	}
	return &v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonNilFloat(values ...*float64) *float64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// --- Metadata/slice_info.config (per-plate prediction/weight, XML) ---

type sliceInfoXML struct {
	Plates []struct {
		Metadata []struct {
			Key   string `xml:"key,attr"`
			Value string `xml:"value,attr"`
		} `xml:"metadata"`
		Objects []struct {
			IdentifyID string `xml:"identify_id,attr"`
		} `xml:"object"`
	} `xml:"plate"`
}

func readPlates(files []*zip.File) ([]Plate, []string) {
	byIndex := map[int]*Plate{}
	var warnings []string

	for _, f := range files {
		m := platePNGPattern.FindStringSubmatch(f.Name)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		p := byIndex[idx]
		if p == nil {
			p = &Plate{Index: idx}
			byIndex[idx] = p
		}
		p.ThumbnailLabel = fmt.Sprintf("plate_%d", idx)
	}

	if f := findZipEntry(files, "Metadata/slice_info.config"); f != nil {
		data, truncated, err := readEntryCapped(f, max3MFEntryUncompressed)
		if err != nil {
			warnings = append(warnings, "could not read Metadata/slice_info.config: "+err.Error())
		} else {
			if truncated {
				warnings = append(warnings, "Metadata/slice_info.config was truncated at the read cap")
			}
			var parsed sliceInfoXML
			if err := xml.Unmarshal(data, &parsed); err != nil {
				warnings = append(warnings, "Metadata/slice_info.config is not recognised XML: "+err.Error())
			} else {
				for _, plate := range parsed.Plates {
					idx := -1
					var prediction, weight *float64
					for _, md := range plate.Metadata {
						switch md.Key {
						case "index":
							if v, err := strconv.Atoi(md.Value); err == nil {
								idx = v
							}
						case "prediction":
							if v, err := strconv.ParseFloat(md.Value, 64); err == nil {
								prediction = &v
							}
						case "weight":
							if v, err := strconv.ParseFloat(md.Value, 64); err == nil {
								weight = &v
							}
						}
					}
					if idx < 0 {
						continue
					}
					p := byIndex[idx]
					if p == nil {
						p = &Plate{Index: idx}
						byIndex[idx] = p
					}
					p.PredictionSeconds = prediction
					p.WeightG = weight
					for _, o := range plate.Objects {
						if o.IdentifyID != "" {
							p.ObjectIDs = append(p.ObjectIDs, o.IdentifyID)
						}
					}
				}
			}
		}
	}

	if len(byIndex) == 0 {
		return nil, warnings
	}
	indexes := make([]int, 0, len(byIndex))
	for idx := range byIndex {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	plates := make([]Plate, 0, len(indexes))
	for _, idx := range indexes {
		plates = append(plates, *byIndex[idx])
	}
	return plates, warnings
}

// --- Thumbnails: Metadata/plate_*.png and the legacy Metadata/thumbnail.png ---

func readThumbnails(files []*zip.File) ([]Thumbnail, []string) {
	var thumbs []Thumbnail
	var warnings []string

	type candidate struct {
		name  string
		label string
	}
	var candidates []candidate
	for _, f := range files {
		if m := platePNGPattern.FindStringSubmatch(f.Name); m != nil {
			candidates = append(candidates, candidate{name: f.Name, label: fmt.Sprintf("plate_%s", m[1])})
		}
	}
	if f := findZipEntry(files, "Metadata/thumbnail.png"); f != nil {
		candidates = append(candidates, candidate{name: f.Name, label: "thumbnail"})
	}

	for _, c := range candidates {
		f := findZipEntry(files, c.name)
		if f == nil {
			continue
		}
		data, truncated, err := readEntryCapped(f, max3MFEntryUncompressed)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not read %s: %s", c.name, err.Error()))
			continue
		}

		var width, height int
		mime := "image/png"
		rejected := false
		if len(data) == 0 {
			truncated = true
		} else {
			actualWidth, actualHeight, actualMime, cfgErr := decodeThumbnailConfig(data)
			switch {
			case cfgErr != nil:
				warnings = append(warnings, fmt.Sprintf("thumbnail %q could not be decoded as a valid image and was rejected: %s", c.name, cfgErr))
				data = nil
				rejected = true
			case thumbnailSizeExceedsCap(actualWidth, actualHeight):
				warnings = append(warnings, fmt.Sprintf("thumbnail %q decodes to an oversized %dx%d image and was rejected", c.name, actualWidth, actualHeight))
				data = nil
				rejected = true
			default:
				width, height, mime = actualWidth, actualHeight, actualMime
			}
		}
		if rejected {
			truncated = true
		}

		thumbs = append(thumbs, Thumbnail{
			Width: width, Height: height, Mime: mime,
			Label: c.label, Data: data, Truncated: truncated,
		})
		if truncated && !rejected {
			warnings = append(warnings, fmt.Sprintf("thumbnail %q is truncated or not a complete PNG", c.name))
		}
	}
	return thumbs, warnings
}
