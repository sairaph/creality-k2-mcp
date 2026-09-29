package moonraker

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// Presence rule (safety-architecture.md P1, fail closed; 11-state-model.md
// section 1.1): every decoded field the state engine or the action policy
// uses to decide whether a write is safe must let a caller tell "Klipper did
// not report this field" apart from "Klipper reported it as its zero value"
// (false, 0, ""). encoding/json already does this correctly for a pointer
// type (a missing or JSON-null key decodes to a nil pointer; a present key
// decodes to a pointer to the reported value, even if that value is the zero
// value) and for a slice type (a missing key decodes to a nil slice; a
// present-but-empty JSON array decodes to a non-nil, zero-length slice) - so
// this package uses `*T` for every scalar field a caller needs this
// distinction on, and leaves naturally-distinguishing slice fields as plain
// slices. A nil pointer or nil slice on one of these fields means the value
// is unknown, not that it is false/zero/empty; callers must treat unknown
// the same as failing closed (P1), never silently substitute a zero value.
// Fields that are read-only display detail with no bearing on any gating or
// policy decision keep plain (non-pointer) types.
//
// print_stats.state is a documented exception: it is already a string, and
// an empty string is already an invalid/unrecognised state under the
// activity-state precedence (11-state-model.md section 1.1, row 19,
// "unknown"), so no separate nil case is added for it; empty and "missing"
// collapse to the same fail-closed outcome by construction.

// ServerInfoResult is the "server/info" response: Klippy connectivity and
// this build's component list. Creality trims and extends the stock
// component set (02-moonraker-api.md section 0), so callers should treat
// Components as this printer's own source of truth for what it supports,
// never api_version.
type ServerInfoResult struct {
	KlippyConnected           bool     `json:"klippy_connected"`
	KlippyState               string   `json:"klippy_state"`
	Components                []string `json:"components"`
	FailedComponents          []string `json:"failed_components"`
	RegisteredDirectories     []string `json:"registered_directories"`
	Warnings                  []string `json:"warnings"`
	WebsocketCount            int      `json:"websocket_count"`
	MoonrakerVersion          string   `json:"moonraker_version"`
	MissingKlippyRequirements []string `json:"missing_klippy_requirements"`
	APIVersion                []int    `json:"api_version"`
	APIVersionString          string   `json:"api_version_string"`
}

// HasComponent reports whether name is in this response's component list.
func (s ServerInfoResult) HasComponent(name string) bool {
	for _, c := range s.Components {
		if c == name {
			return true
		}
	}
	return false
}

// ServerInfo fetches Klippy connectivity and the component list. Callers on
// a write path must call this fresh immediately before acting (P5 in
// safety-architecture.md); nothing in this package caches klippy_state
// across calls.
func (c *Client) ServerInfo(ctx context.Context) (ServerInfoResult, error) {
	var out ServerInfoResult
	err := c.get(ctx, "ServerInfo", "/server/info", nil, timeoutStatus, &out)
	return out, err
}

// PrinterInfoResult is the "printer/info" response: Klippy host identity.
type PrinterInfoResult struct {
	State           string `json:"state"`
	StateMessage    string `json:"state_message"`
	Hostname        string `json:"hostname"`
	KlipperPath     string `json:"klipper_path"`
	PythonPath      string `json:"python_path"`
	LogFile         string `json:"log_file"`
	ConfigFile      string `json:"config_file"`
	SoftwareVersion string `json:"software_version"`
	CPUInfo         string `json:"cpu_info"`
}

// PrinterInfo fetches the Klippy host's own identity and state.
func (c *Client) PrinterInfo(ctx context.Context) (PrinterInfoResult, error) {
	var out PrinterInfoResult
	err := c.get(ctx, "PrinterInfo", "/printer/info", nil, timeoutStatus, &out)
	return out, err
}

// ObjectsList returns every printer object Klipper currently has loaded
// (mostly gcode_macro entries on this printer, plus the real Klipper and
// Creality objects QueryObjects can fetch).
func (c *Client) ObjectsList(ctx context.Context) ([]string, error) {
	var out struct {
		Objects []string `json:"objects"`
	}
	err := c.get(ctx, "ObjectsList", "/printer/objects/list", nil, timeoutStatus, &out)
	return out.Objects, err
}

// QueryObjects fetches one or more printer objects in a single round trip.
// The map key is the object name exactly as Klipper reports it (including
// the space in names like "output_pin fan0" or "gcode_macro PRINTER_PARAM");
// the value is the list of fields to fetch, or nil/empty for every field on
// that object. The result is keyed the same way, holding each object's raw
// JSON status so callers can decode only what they need with the typed
// decoders below, or handle an object this package has no type for.
// An object the caller asked for that Klipper does not currently report is
// simply absent from the result map, not an error.
func (c *Client) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	query := url.Values{}
	for name, fields := range objects {
		// Moonraker parses this the same way Tornado parses any query
		// string: split on '&' and the first '=', then URL-decode each
		// side. An object name with no field filter is sent as a bare
		// "name=" (empty value), which Moonraker's own parser
		// (components/application.py's _object_parser: "if not val:
		// args[key] = None") treats as "every field"; a filtered object is
		// "name=field1,field2".
		value := ""
		if len(fields) > 0 {
			value = strings.Join(fields, ",")
		}
		query.Set(name, value)
	}

	var out struct {
		Status map[string]json.RawMessage `json:"status"`
	}
	err := c.get(ctx, "QueryObjects", "/printer/objects/query", query, timeoutStatus, &out)
	return out.Status, err
}

// --- Typed decoders for the objects this server cares about. ---
//
// Every struct here is tolerant of unknown fields (Go's encoding/json
// ignores them by default) and of the object being absent from a
// QueryObjects result (the corresponding Decode* function simply is not
// called; callers check the map for the key first).

// PrintStats is Klipper's print_stats, with the two Creality-only fields
// this fork adds (power_loss, z_pos; 02-moonraker-api.md section 2). State
// stays a plain string, not *string: an empty string is already an invalid,
// unrecognised state under the activity-state precedence
// (11-state-model.md section 1.1, row 19, "unknown"), so "missing" and
// "reported empty" already collapse to the same fail-closed outcome and a
// pointer would add no information a caller needs to act on.
type PrintStats struct {
	Filename      string         `json:"filename"`
	TotalDuration float64        `json:"total_duration"`
	PrintDuration float64        `json:"print_duration"`
	FilamentUsed  float64        `json:"filament_used"`
	State         string         `json:"state"`
	Message       string         `json:"message"`
	Info          PrintStatsInfo `json:"info"`
	PowerLoss     int            `json:"power_loss"`
	ZPos          float64        `json:"z_pos"`
}

type PrintStatsInfo struct {
	TotalLayer   *int `json:"total_layer"`
	CurrentLayer *int `json:"current_layer"`
}

func DecodePrintStats(raw json.RawMessage) (PrintStats, error) {
	var v PrintStats
	err := json.Unmarshal(raw, &v)
	return v, err
}

// PauseResume is Klipper's pause_resume, plus Creality's resume_err
// addition used by the RESUME_EXTERNAL macro chain. IsPaused is the primary
// gating input for resume_print (safety-architecture.md 4.2) and must
// distinguish "not reported" from "reported false"; a nil IsPaused must fail
// closed, never be treated as "not paused".
type PauseResume struct {
	IsPaused  *bool `json:"is_paused"`
	ResumeErr *bool `json:"resume_err"`
}

func DecodePauseResume(raw json.RawMessage) (PauseResume, error) {
	var v PauseResume
	err := json.Unmarshal(raw, &v)
	return v, err
}

// IdleTimeout is Klipper's idle_timeout. State feeds the busy_command
// gating row (11-state-model.md section 1.1 row 18) and must distinguish
// "not reported" from the empty string.
type IdleTimeout struct {
	State        *string `json:"state"`
	PrintingTime float64 `json:"printing_time"`
}

func DecodeIdleTimeout(raw json.RawMessage) (IdleTimeout, error) {
	var v IdleTimeout
	err := json.Unmarshal(raw, &v)
	return v, err
}

// Webhooks is Klipper's webhooks, the field every write must gate on. State
// must distinguish "not reported" from the empty string; a nil State must
// fail closed, never be treated as "ready".
type Webhooks struct {
	State        *string `json:"state"`
	StateMessage string  `json:"state_message"`
}

func DecodeWebhooks(raw json.RawMessage) (Webhooks, error) {
	var v Webhooks
	err := json.Unmarshal(raw, &v)
	return v, err
}

// FlushPara is a multicolor flush parameter block inside Creality's rich
// print metadata.
type FlushPara struct {
	FlushMultiplier    float64   `json:"flush_multiplier"`
	FlushVolumesMatrix []float64 `json:"flush_volumes_matrix"`
}

// ModelInfo is the plate bounding box and material block inside Creality's
// rich print metadata.
type ModelInfo struct {
	MinX             float64 `json:"MINX"`
	MinY             float64 `json:"MINY"`
	MinZ             float64 `json:"MINZ"`
	MaxX             float64 `json:"MAXX"`
	MaxY             float64 `json:"MAXY"`
	MaxZ             float64 `json:"MAXZ"`
	MulticolorMethod int     `json:"multicolor_method"`
	MaterialType     string  `json:"MaterialType"`
}

// CrealityPrintMetadata is the rich per-print metadata Creality's own
// slicer embeds, as reported at virtual_sdcard.cur_print_data.metadata.
// This is a different, richer shape than server/files/metadata, which
// reports "Unknown" for the same file on this fork (02-moonraker-api.md
// section 2 and 3): this is the metadata to prefer whenever it is present.
type CrealityPrintMetadata struct {
	Size                  int64      `json:"size"`
	Modified              float64    `json:"modified"`
	UUID                  string     `json:"uuid"`
	Slicer                string     `json:"slicer"`
	SlicerVersion         string     `json:"slicer_version"`
	GCodeStartByte        int64      `json:"gcode_start_byte"`
	GCodeEndByte          int64      `json:"gcode_end_byte"`
	LayerCount            int        `json:"layer_count"`
	ObjectHeight          float64    `json:"object_height"`
	EstimatedTime         float64    `json:"estimated_time"`
	LayerHeight           float64    `json:"layer_height"`
	FirstLayerHeight      float64    `json:"first_layer_height"`
	FilamentType          string     `json:"filament_type"`
	FlushPara             *FlushPara `json:"flush_para"`
	ModelInfo             *ModelInfo `json:"model_info"`
	DefaultFilamentColour []string   `json:"default_filament_colour"`
	FilamentUsedG         []string   `json:"filament_used_g"`
}

// CurPrintData is virtual_sdcard.cur_print_data: the last (or current) job,
// which stays populated after the job ends.
type CurPrintData struct {
	EndTime       float64                `json:"end_time"`
	FilamentUsed  float64                `json:"filament_used"`
	Filename      string                 `json:"filename"`
	Metadata      *CrealityPrintMetadata `json:"metadata"`
	PrintDuration float64                `json:"print_duration"`
	StartTime     float64                `json:"start_time"`
	Status        string                 `json:"status"`
	TotalDuration float64                `json:"total_duration"`
}

// VirtualSDCard is Creality's heavily customised virtual_sdcard object.
// Its field names do not match upstream Moonraker's documented shape at
// all (02-moonraker-api.md section 2); this models the shape actually
// observed on this fork. IsActive and BedMeshCalibrateState both feed
// activity-state gating (11-state-model.md section 1.1 rows 5, 6, 11, 12)
// and must distinguish "not reported" from "reported false"; nil must fail
// closed, never be treated as "not active"/"not calibrating".
type VirtualSDCard struct {
	FilePath              *string       `json:"file_path"`
	Progress              float64       `json:"progress"`
	IsActive              *bool         `json:"is_active"`
	FilePosition          float64       `json:"file_position"`
	FileSize              float64       `json:"file_size"`
	FirstLayerStop        bool          `json:"first_layer_stop"`
	Layer                 int           `json:"layer"`
	LayerCount            int           `json:"layer_count"`
	RunDis                float64       `json:"run_dis"`
	BedMeshCalibrateState *bool         `json:"bed_mesh_calibate_state"`
	KlipperCapture        bool          `json:"klipper_capture"`
	KlipperCaptureCount   int           `json:"klipper_capture_cnt"`
	CurPrintData          *CurPrintData `json:"cur_print_data"`
}

func DecodeVirtualSDCard(raw json.RawMessage) (VirtualSDCard, error) {
	var v VirtualSDCard
	err := json.Unmarshal(raw, &v)
	return v, err
}

// Extruder is Klipper's extruder heater and pressure-advance state.
// Temperature and Target are read by the temperature-setpoint policy
// (set_nozzle_temperature, D1's +-10 C band) and must distinguish "not
// reported" from a genuine 0; nil must fail closed (ErrLiveCapUnknown-style
// refusal), never be treated as "off"/"target 0".
type Extruder struct {
	Temperature                    *float64 `json:"temperature"`
	Target                         *float64 `json:"target"`
	Power                          float64  `json:"power"`
	CanExtrude                     bool     `json:"can_extrude"`
	ExtrudeBelowMinTempErrIsReport bool     `json:"extrude_below_min_temp_err_is_report"`
	NozzleDiameter                 float64  `json:"nozzle_diameter"`
	PressureAdvance                float64  `json:"pressure_advance"`
	SmoothTime                     float64  `json:"smooth_time"`
}

func DecodeExtruder(raw json.RawMessage) (Extruder, error) {
	var v Extruder
	err := json.Unmarshal(raw, &v)
	return v, err
}

// HeaterBed is Klipper's heater_bed state. Temperature and Target are read
// by the temperature-setpoint policy (set_bed_temperature, D1's +-5 C band)
// and must distinguish "not reported" from a genuine 0; nil must fail
// closed, never be treated as "off"/"target 0".
type HeaterBed struct {
	Temperature *float64 `json:"temperature"`
	Target      *float64 `json:"target"`
	Power       float64  `json:"power"`
}

func DecodeHeaterBed(raw json.RawMessage) (HeaterBed, error) {
	var v HeaterBed
	err := json.Unmarshal(raw, &v)
	return v, err
}

// GCodeMove is Klipper's gcode_move: the live speed and extrusion factors,
// and the current gcode-space position. SpeedFactor and ExtrudeFactor are
// read by set_speed_factor/set_flow_factor's band checks (D1) and must
// distinguish "not reported" from a genuine 0; nil must fail closed, never
// be treated as "0% speed/flow".
type GCodeMove struct {
	SpeedFactor         *float64  `json:"speed_factor"`
	Speed               float64   `json:"speed"`
	ExtrudeFactor       *float64  `json:"extrude_factor"`
	AbsoluteCoordinates bool      `json:"absolute_coordinates"`
	AbsoluteExtrude     bool      `json:"absolute_extrude"`
	HomingOrigin        []float64 `json:"homing_origin"`
	Position            []float64 `json:"position"`
	GCodePosition       []float64 `json:"gcode_position"`
}

func DecodeGCodeMove(raw json.RawMessage) (GCodeMove, error) {
	var v GCodeMove
	err := json.Unmarshal(raw, &v)
	return v, err
}

// Toolhead is Klipper's toolhead, plus Creality's G29_flag addition.
type Toolhead struct {
	HomedAxes            string    `json:"homed_axes"`
	AxisMinimum          []float64 `json:"axis_minimum"`
	AxisMaximum          []float64 `json:"axis_maximum"`
	PrintTime            float64   `json:"print_time"`
	Stalls               int       `json:"stalls"`
	EstimatedPrintTime   float64   `json:"estimated_print_time"`
	Extruder             string    `json:"extruder"`
	Position             []float64 `json:"position"`
	MaxVelocity          float64   `json:"max_velocity"`
	MaxAccel             float64   `json:"max_accel"`
	MaxAccelToDecel      float64   `json:"max_accel_to_decel"`
	SquareCornerVelocity float64   `json:"square_corner_velocity"`
	G29Flag              bool      `json:"G29_flag"`
}

func DecodeToolhead(raw json.RawMessage) (Toolhead, error) {
	var v Toolhead
	err := json.Unmarshal(raw, &v)
	return v, err
}

// ExcludeObject is Klipper's exclude_object. Objects and ExcludedObjects are
// plain slices, not pointer types: encoding/json already gives slices the
// presence distinction this package needs without a pointer indirection - a
// missing key decodes to a nil slice, while a present-but-empty JSON array
// ("[]") decodes to a non-nil, zero-length slice - so callers can tell "the
// printer did not report this list" (nil) from "the printer reported an
// empty list" (non-nil, len 0) directly from the slice's nil-ness.
type ExcludeObject struct {
	Objects         []map[string]any `json:"objects"`
	ExcludedObjects []string         `json:"excluded_objects"`
	CurrentObject   *string          `json:"current_object"`
}

func DecodeExcludeObject(raw json.RawMessage) (ExcludeObject, error) {
	var v ExcludeObject
	err := json.Unmarshal(raw, &v)
	return v, err
}

// CustomMacro is Creality's custom_macro object: the leveling-calibration
// busy flag and the default print temperatures its macros read
// (03-k2-cfs-objects.md section 4.3, custom_macro.py:33-40).
// LevelingCalibration feeds the calibrating gating row (11-state-model.md
// section 1.1 row 6) and must distinguish "not reported" from "reported 0
// (not calibrating)"; nil must fail closed, never be treated as "not
// calibrating".
type CustomMacro struct {
	LevelingCalibration *int    `json:"leveling_calibration"`
	DefaultExtruderTemp float64 `json:"default_extruder_temp"`
	DefaultBedTemp      float64 `json:"default_bed_temp"`
	G28ExtTemp          float64 `json:"g28_ext_temp"`
	QmodeFlag           float64 `json:"qmode_flag"`
}

func DecodeCustomMacro(raw json.RawMessage) (CustomMacro, error) {
	var v CustomMacro
	err := json.Unmarshal(raw, &v)
	return v, err
}

// DisplayStatus is Klipper's display_status.
type DisplayStatus struct {
	Progress float64 `json:"progress"`
	Message  *string `json:"message"`
}

func DecodeDisplayStatus(raw json.RawMessage) (DisplayStatus, error) {
	var v DisplayStatus
	err := json.Unmarshal(raw, &v)
	return v, err
}

// MotorControlCut is the cutter sub-state inside motor_control.
type MotorControlCut struct {
	State bool    `json:"state"`
	PosX  float64 `json:"pos_x"`
}

// MotorControl is Creality's motor_control object (RS-485 stepper driver
// bus health and the filament cutter). dev_uuid and other per-driver detail
// are intentionally not modelled: nothing in this project's tool surface
// reads them, and encoding/json leaves them as ignored unknown fields.
// IsHoming feeds the homing gating row (11-state-model.md section 1.1
// row 4) and must distinguish "not reported" from "reported false"; nil
// must fail closed, never be treated as "not homing".
type MotorControl struct {
	MotorReady bool            `json:"motor_ready"`
	IsHoming   *bool           `json:"is_homing"`
	Cut        MotorControlCut `json:"cut"`
}

func DecodeMotorControl(raw json.RawMessage) (MotorControl, error) {
	var v MotorControl
	err := json.Unmarshal(raw, &v)
	return v, err
}

// OutputPin is the status shape shared by every "output_pin <name>" object
// this project reads (fan0, fan1, fan2, LED): just the live PWM value.
// Value is read back after set_fan_speed/set_light writes and must
// distinguish "not reported" from a genuine 0; nil must fail closed, never
// be treated as "off".
type OutputPin struct {
	Value *float64 `json:"value"`
}

func DecodeOutputPin(raw json.RawMessage) (OutputPin, error) {
	var v OutputPin
	err := json.Unmarshal(raw, &v)
	return v, err
}

// FilamentRack is Creality's filament_rack object (the side spool holder,
// distinct from the CFS/box unit). Every field here is display-only detail
// shown to the caller, but a nil pointer still must distinguish "not
// reported" from a genuine zero value/empty string, so a status tool never
// shows a fabricated "no filament"/empty-color reading for a field the
// printer simply did not report.
type FilamentRack struct {
	Vender                 *string  `json:"vender"`
	ColorValue             *string  `json:"color_value"`
	MaterialType           *string  `json:"material_type"`
	RemainMaterialColor    *string  `json:"remain_material_color"`
	RemainMaterialType     *string  `json:"remain_material_type"`
	RemainMaterialVelocity *float64 `json:"remain_material_velocity"`
}

func DecodeFilamentRack(raw json.RawMessage) (FilamentRack, error) {
	var v FilamentRack
	err := json.Unmarshal(raw, &v)
	return v, err
}

// Box is Creality's CFS (Creality Filament System) controller object
// (dev_docs/plan-v0.2.0.md section 2.1; captured in
// dev_docs/captures/moonraker_box_filament_rack_20260929.json).
//
// Enable, FilamentUseup, AutoRefill and Filament are pointers under the
// presence rule at the top of this file: filament_useup 0 means "no runout"
// and enable 1 is the value at rest, so a field the printer did not report
// (or reported with an unexpected type) must never read as that zero. A wrong
// type on state, enable or filament_useup leaves the pointer nil (fail
// closed), it never decodes to zero, and no wrong-typed field fails the whole
// decode.
//
// The json tags exist only so a Box marshals in the same wire shape the
// custom UnmarshalJSON reads for the simple fields (tests build Moonraker
// fixtures by marshalling a Box); the composite decoded fields are not
// round-tripped.
type Box struct {
	Filament      *int    `json:"filament,omitempty"`
	State         *string `json:"state,omitempty"`
	AutoRefill    *int    `json:"auto_refill,omitempty"`
	Enable        *int    `json:"enable,omitempty"`
	FilamentUseup *int    `json:"filament_useup,omitempty"`
	// SameMaterial is the printer's regrouping of interchangeable slots in
	// Moonraker's form, [code6, color7, ["T1A",...], name]. SameMaterialOK
	// is false when the key was absent or any group failed to decode (then
	// SameMaterial is nil).
	SameMaterial   []BoxSameGroup    `json:"-"`
	SameMaterialOK bool              `json:"-"`
	Map            map[string]string `json:"map,omitempty"`
	// Units holds the per-unit display data for keys T1..T4 only.
	Units map[string]BoxUnit `json:"-"`
}

// BoxSameGroup is one same_material group: Code is the 6-char material code
// ("0" + the 5-char catalog id), Color the 7-char colour without "#", Slots
// the member slot names ("T1A"), Name the material type.
type BoxSameGroup struct {
	Code, Color string
	Slots       []string
	Name        string
}

// BoxUnit is the display data of one CFS unit (T1..T4): State is the unit's
// own state string ("connect", "None"...) and MaterialType/ColorValue are the
// per-slot 6-char material code and 7-char colour, in slot order A..D. The
// arrays may mix strings and numbers on the wire; every element is coerced to
// a string, and a malformed array decodes to nil rather than failing the box.
type BoxUnit struct {
	State                    string
	MaterialType, ColorValue []string
}

// boxUnitKeys are the unit keys this package decodes.
var boxUnitKeys = []string{"T1", "T2", "T3", "T4"}

// UnmarshalJSON decodes a box object field by field so that one wrongly
// typed field costs only that field (see the Box doc comment).
func (b *Box) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*b = Box{}
	b.Filament = boxInt(m["filament"])
	b.AutoRefill = boxInt(m["auto_refill"])
	b.Enable = boxInt(m["enable"])
	b.FilamentUseup = boxInt(m["filament_useup"])
	if raw, ok := m["state"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			b.State = &s
		}
	}
	if raw, ok := m["map"]; ok {
		var mp map[string]string
		if json.Unmarshal(raw, &mp) == nil {
			b.Map = mp
		}
	}
	if raw, ok := m["same_material"]; ok {
		b.SameMaterial, b.SameMaterialOK = decodeBoxSameMaterial(raw)
	}
	for _, key := range boxUnitKeys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var um map[string]json.RawMessage
		if json.Unmarshal(raw, &um) != nil || um == nil {
			continue
		}
		var u BoxUnit
		if sraw, ok := um["state"]; ok {
			_ = json.Unmarshal(sraw, &u.State) // a wrong type leaves ""
		}
		u.MaterialType = boxStrings(um["material_type"])
		u.ColorValue = boxStrings(um["color_value"])
		if b.Units == nil {
			b.Units = map[string]BoxUnit{}
		}
		b.Units[key] = u
	}
	return nil
}

// boxInt decodes a JSON number with an integral value; anything else
// (absent, null, string, fraction) is nil.
func boxInt(raw json.RawMessage) *int {
	if len(raw) == 0 {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || f != float64(int(f)) {
		return nil
	}
	v := int(f)
	return &v
}

// boxStrings decodes a display array whose elements may be strings or
// numbers into strings; a non-array is nil and a null or structured element
// becomes "".
func boxStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]string, len(items))
	for i, it := range items {
		var s string
		if json.Unmarshal(it, &s) == nil {
			out[i] = s
			continue
		}
		var n json.Number
		if json.Unmarshal(it, &n) == nil {
			out[i] = n.String()
		}
	}
	return out
}

func decodeBoxSameMaterial(raw json.RawMessage) ([]BoxSameGroup, bool) {
	var groups []json.RawMessage
	if json.Unmarshal(raw, &groups) != nil {
		return nil, false
	}
	out := make([]BoxSameGroup, 0, len(groups))
	for _, g := range groups {
		var parts []json.RawMessage
		if json.Unmarshal(g, &parts) != nil || len(parts) != 4 {
			return nil, false
		}
		var grp BoxSameGroup
		if json.Unmarshal(parts[0], &grp.Code) != nil || json.Unmarshal(parts[1], &grp.Color) != nil ||
			json.Unmarshal(parts[2], &grp.Slots) != nil || json.Unmarshal(parts[3], &grp.Name) != nil {
			return nil, false
		}
		out = append(out, grp)
	}
	return out, true
}

// Connected reports whether a CFS unit is on the bus. The only value ever
// observed for "not connected" is the string "disconnect"
// (03-k2-cfs-objects.md section 1.1); anything else, including State being
// nil (not reported at all), is treated as connected, fail-closed per this
// project's safety posture (an unrecognised or missing state must not be
// silently treated as safe to control around; safety-architecture.md P1,
// Q5).
func (b Box) Connected() bool {
	if b.State == nil {
		return true
	}
	return *b.State != "disconnect"
}

// AnyUnitConnected reports whether any per-unit state is "connect". A brief
// bus drop can flip box.state to "disconnect" while a unit still reports
// itself connected; the CFS rules must not lift on that (plan 2.2).
func (b Box) AnyUnitConnected() bool {
	for _, u := range b.Units {
		if u.State == "connect" {
			return true
		}
	}
	return false
}

func DecodeBox(raw json.RawMessage) (Box, error) {
	var v Box
	err := json.Unmarshal(raw, &v)
	return v, err
}

// PrinterParam is Creality's "gcode_macro PRINTER_PARAM" object: runtime
// machine parameters and the values PAUSE stores for RESUME to restore
// (HotendTemp, Fan0Speed, Fan2Speed, ZSafePause). Those four stored-target
// fields are what resume_print's proposal discloses to the caller
// (safety-architecture.md 4.2) and must distinguish "not reported" from a
// genuine 0; nil must fail closed (the resume proposal cannot claim a
// stored target it does not actually have).
type PrinterParam struct {
	ZSafePause    *float64  `json:"z_safe_pause"`
	ZSafeG28      float64   `json:"z_safe_g28"`
	MaxXPosition  float64   `json:"max_x_position"`
	MaxYPosition  float64   `json:"max_y_position"`
	MaxZPosition  float64   `json:"max_z_position"`
	Fans          int       `json:"fans"`
	AutoG29       int       `json:"auto_g29"`
	Fan0Min       float64   `json:"fan0_min"`
	Fan1Min       float64   `json:"fan1_min"`
	Fan2Min       float64   `json:"fan2_min"`
	Fan0Speed     *float64  `json:"fan0_speed"`
	Fan2Speed     *float64  `json:"fan2_speed"`
	HotendTemp    *float64  `json:"hotend_temp"`
	EMinCurrent   float64   `json:"e_min_current"`
	BedSteadyTemp []float64 `json:"bed_steady_temp"`
	BedSteadyTime []float64 `json:"bed_steady_time"`
}

func DecodePrinterParam(raw json.RawMessage) (PrinterParam, error) {
	var v PrinterParam
	err := json.Unmarshal(raw, &v)
	return v, err
}

// ProductParam is Creality's "gcode_macro product_param" object: the
// per-model hardware ceilings (plan-v0.1.0.md decision 6's live cap).
// NozzleTemp and BedTemp are the live temperature caps domain.LiveCap is
// built from; they must distinguish "not reported" from a genuine 0, since
// a caller must return domain.ErrLiveCapUnknown rather than treat a missing
// live cap as "cap is 0" or silently fall back to the code ceiling alone.
type ProductParam struct {
	BedSizeX    float64  `json:"bed_size_x"`
	BedSizeY    float64  `json:"bed_size_y"`
	BedSizeZ    float64  `json:"bed_size_z"`
	NozzleTemp  *float64 `json:"nozzle_temp"`
	BedTemp     *float64 `json:"bed_temp"`
	ChamberTemp float64  `json:"chamber_temp"`
}

func DecodeProductParam(raw json.RawMessage) (ProductParam, error) {
	var v ProductParam
	err := json.Unmarshal(raw, &v)
	return v, err
}
