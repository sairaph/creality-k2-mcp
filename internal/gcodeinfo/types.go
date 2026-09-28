package gcodeinfo

// Dialect identifies which slicer's comment conventions a file's fields were
// read with. It is always set, never left empty: an unrecognised file
// reports DialectUnknown rather than silently omitting the field
// (references/analysis/01-python-server.md section 6, "Single-slicer-
// dialect G-code parsing").
type Dialect string

const (
	DialectCrealityPrint Dialect = "creality_print"
	DialectOrcaSlicer    Dialect = "orcaslicer"
	DialectBambuStudio   Dialect = "bambustudio"
	DialectPrusaSlicer   Dialect = "prusaslicer"
	DialectSuperSlicer   Dialect = "superslicer"
	DialectCura          Dialect = "cura"
	DialectUnknown       Dialect = "unknown"
)

// Bounds is a model's bounding box, from the gcode MINX..MAXZ comments or a
// 3MF's per-plate geometry. A pointer field on the containing struct
// (Bounds is always embedded as *Bounds) reports "not present in the file"
// as a whole rather than a fabricated all-zero box.
type Bounds struct {
	MinX *float64 `yaml:"min_x,omitempty"`
	MinY *float64 `yaml:"min_y,omitempty"`
	MinZ *float64 `yaml:"min_z,omitempty"`
	MaxX *float64 `yaml:"max_x,omitempty"`
	MaxY *float64 `yaml:"max_y,omitempty"`
	MaxZ *float64 `yaml:"max_z,omitempty"`
}

// Estimate is the settings/estimate comment block usually found at the tail
// of a Creality Print / OrcaSlicer style gcode file (filament cost, printer
// temperatures and colour, etc), or the equivalent Cura/PrusaSlicer fields
// where cheap to recognise. Every field is best-effort: a slicer only
// emits the comments it emits, and this struct never fabricates a value for
// one that is missing.
type Estimate struct {
	EstimatedTimeText    string   `yaml:"estimated_time_text,omitempty"`
	EstimatedTimeSeconds *float64 `yaml:"estimated_time_seconds,omitempty"`
	FilamentUsedMM       *float64 `yaml:"filament_used_mm,omitempty"`
	FilamentUsedG        *float64 `yaml:"filament_used_g,omitempty"`
	FilamentCost         *float64 `yaml:"filament_cost,omitempty"`
	LayerHeightMM        *float64 `yaml:"layer_height_mm,omitempty"`
	NozzleTemperatureC   *float64 `yaml:"nozzle_temperature_c,omitempty"`
	BedTemperatureC      *float64 `yaml:"bed_temperature_c,omitempty"`
	FilamentType         string   `yaml:"filament_type,omitempty"`
	FilamentColour       string   `yaml:"filament_colour,omitempty"`
	PrinterModel         string   `yaml:"printer_model,omitempty"`
}

// ThumbnailMeta is frontmatter-safe thumbnail metadata: dimensions and MIME
// type, but never the image bytes. See Thumbnail for the paired image data,
// which a caller keeps out of any YAML document and turns into MCP image
// content instead.
type ThumbnailMeta struct {
	Width     int    `yaml:"width"`
	Height    int    `yaml:"height"`
	Mime      string `yaml:"mime"`
	Label     string `yaml:"label,omitempty"`
	Truncated bool   `yaml:"truncated,omitempty"`
}

// Thumbnail is one decoded thumbnail image. It is always returned alongside
// a yaml-tagged result struct, never embedded inside one, so that emitting
// YAML frontmatter never has to skip or truncate raw image bytes.
// Truncated is true when the source was cut before this image's data
// completed (a bounded head read landing mid base64, or a bounded zip-entry
// read hitting its cap); Data then holds whatever prefix could still be
// decoded as a partial PNG stream, which may be empty.
type Thumbnail struct {
	Width     int
	Height    int
	Mime      string
	Label     string
	Data      []byte
	Truncated bool
}

// GCodeInfo is the yaml-frontmatter-safe result of parsing a gcode file's
// head and tail. Thumbnail image bytes are never part of this struct; see
// ParseHeader's second return value.
type GCodeInfo struct {
	Dialect          Dialect         `yaml:"dialect"`
	Generator        string          `yaml:"generator,omitempty"`
	GeneratorVersion string          `yaml:"generator_version,omitempty"`
	TotalLayers      *int            `yaml:"total_layers,omitempty"`
	FilamentDensity  *float64        `yaml:"filament_density,omitempty"`
	FilamentDiameter *float64        `yaml:"filament_diameter,omitempty"`
	MaxZHeight       *float64        `yaml:"max_z_height,omitempty"`
	CrealityUUID     string          `yaml:"creality_uuid,omitempty"`
	TaskID           string          `yaml:"task_id,omitempty"`
	Bounds           *Bounds         `yaml:"bounds,omitempty"`
	MulticolorMethod *int            `yaml:"multicolor_method,omitempty"`
	Estimate         *Estimate       `yaml:"estimate,omitempty"`
	Thumbnails       []ThumbnailMeta `yaml:"thumbnails,omitempty"`

	// HeadTruncated is true when the supplied head buffer looks like it was
	// cut off before the header content it started (an opened HEADER_BLOCK
	// or THUMBNAIL_BLOCK with no matching *_END marker). It is informational,
	// not an error: a caller that only read a bounded head should expect
	// this on a file whose header is unusually large.
	HeadTruncated bool `yaml:"head_truncated,omitempty"`

	// Warnings carries non-fatal parse notes (a block opened but never
	// closed, a value that did not parse as a number, and so on).
	Warnings []string `yaml:"warnings,omitempty"`
}

// LocalGCodeFile is InspectLocalGCode's result: file identity plus the same
// parsed fields ParseHeader produces.
type LocalGCodeFile struct {
	Path      string `yaml:"path"`
	SizeBytes int64  `yaml:"size_bytes"`
	GCodeInfo `yaml:",inline"`
}

// ModelObject is one object entry from a 3MF's 3D/3dmodel.model.
type ModelObject struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name,omitempty"`
	Type string `yaml:"type,omitempty"`
}

// Plate is one build plate inside a multi-plate 3MF (an OrcaSlicer /
// Creality Print / BambuStudio project). ThumbnailLabel, when set, matches
// a Thumbnail.Label returned alongside the ThreeMFInfo result.
type Plate struct {
	Index             int      `yaml:"index"`
	ThumbnailLabel    string   `yaml:"thumbnail_label,omitempty"`
	ObjectIDs         []string `yaml:"object_ids,omitempty"`
	PredictionSeconds *float64 `yaml:"prediction_seconds,omitempty"`
	WeightG           *float64 `yaml:"weight_g,omitempty"`
}

// ThreeMFInfo is the yaml-frontmatter-safe result of inspecting a local 3MF
// project archive. Thumbnail image bytes are never part of this struct; see
// InspectLocal3MF's second return value.
type ThreeMFInfo struct {
	Path      string  `yaml:"path"`
	SizeBytes int64   `yaml:"size_bytes"`
	Dialect   Dialect `yaml:"dialect"`

	Plates  []Plate       `yaml:"plates,omitempty"`
	Objects []ModelObject `yaml:"objects,omitempty"`

	PrinterModel   string   `yaml:"printer_model,omitempty"`
	IsK2Project    bool     `yaml:"is_k2_project,omitempty"`
	NozzleDiameter *float64 `yaml:"nozzle_diameter,omitempty"`
	FilamentType   string   `yaml:"filament_type,omitempty"`
	FilamentColour string   `yaml:"filament_colour,omitempty"`
	LayerHeight    *float64 `yaml:"layer_height,omitempty"`
	NozzleTemp     *float64 `yaml:"nozzle_temperature,omitempty"`
	BedTemp        *float64 `yaml:"bed_temperature,omitempty"`

	EstimatedTimeText    string   `yaml:"estimated_time_text,omitempty"`
	EstimatedTimeSeconds *float64 `yaml:"estimated_time_seconds,omitempty"`

	// Settings is the filtered, size-capped settings summary: a curated
	// allowlist of printer/filament/process keys (settingsAllowlist), or,
	// when Options.FullSettings was requested, every key from
	// project_settings.config up to Options.MaxSettingsBytes of serialised
	// size. SettingsCount is always the full key count regardless of how
	// many are actually included in Settings, so a caller can tell a
	// filtered view from a complete one.
	Settings          map[string]string `yaml:"settings,omitempty"`
	SettingsCount     int               `yaml:"settings_count,omitempty"`
	SettingsFull      bool              `yaml:"settings_full,omitempty"`
	SettingsTruncated bool              `yaml:"settings_truncated,omitempty"`

	Thumbnails []ThumbnailMeta `yaml:"thumbnails,omitempty"`
	Warnings   []string        `yaml:"warnings,omitempty"`
}
