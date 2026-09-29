package policy

import (
	"strings"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// ActionName names one of the writes this package knows how to
// perform. There is no way to Execute an action outside this fixed set.
type ActionName string

const (
	ActionStartPrint            ActionName = "start_print"
	ActionPausePrint            ActionName = "pause_print"
	ActionResumePrint           ActionName = "resume_print"
	ActionCancelPrint           ActionName = "cancel_print"
	ActionSetNozzleTemperature  ActionName = "set_nozzle_temperature"
	ActionSetBedTemperature     ActionName = "set_bed_temperature"
	ActionSetFanSpeed           ActionName = "set_fan_speed"
	ActionSetSpeedFactor        ActionName = "set_speed_factor"
	ActionSetSpeedPreset        ActionName = "set_speed_preset"
	ActionSetFlowFactor         ActionName = "set_flow_factor"
	ActionSetLight              ActionName = "set_light"
	ActionExcludeObject         ActionName = "exclude_object"
	ActionUploadGCodeFile       ActionName = "upload_gcode_file"
	ActionDeleteGCodeFile       ActionName = "delete_gcode_file"
	ActionSetFilamentDefinition ActionName = "set_filament_definition"
)

// Actions lists every action name in a stable order, for iteration
// (Available, regression tests).
var Actions = []ActionName{
	ActionStartPrint,
	ActionPausePrint,
	ActionResumePrint,
	ActionCancelPrint,
	ActionSetNozzleTemperature,
	ActionSetBedTemperature,
	ActionSetFanSpeed,
	ActionSetSpeedFactor,
	ActionSetSpeedPreset,
	ActionSetFlowFactor,
	ActionSetLight,
	ActionExcludeObject,
	ActionUploadGCodeFile,
	ActionDeleteGCodeFile,
	ActionSetFilamentDefinition,
}

// Confirmation is the confirmation kind an action needs, per D3
// (dev_docs/safety-architecture.md section 10): the proposal_token is only a
// TOCTOU state-binding guard, never a stand-in for a human's own
// confirmation, which the harness handles outside this package entirely.
type Confirmation string

const (
	// ConfirmationNone: Execute performs the write in one call. Mid-print
	// setpoint actions still gate on D1's bands; they simply do not need a
	// second, token-confirmed call to do it.
	ConfirmationNone Confirmation = "none"
	// ConfirmationProposalToken: a first call with no token returns a
	// proposal and a token bound to the exact situation; a second call with
	// that token re-verifies the binding against a fresh snapshot and, if
	// nothing bound has changed, performs the write.
	ConfirmationProposalToken Confirmation = "proposal_token"
	// ConfirmationConditional is upload_gcode_file's own case: a
	// proposal_token is required only when the fresh snapshot shows the
	// target filename already exists (an overwrite); a genuinely new
	// filename needs no token at all.
	ConfirmationConditional Confirmation = "conditional"
)

// Params carries every parameter any action might need. Only the fields a
// given action documents are read; the rest are ignored. Every field is a
// plain comparable type on purpose: a proposal token binds the exact Params
// value it was issued for via ==, so Params must never gain a slice, map or
// pointer field.
type Params struct {
	// Filename names a gcodes-root file: the file to start (start_print),
	// the remote name to upload or overwrite (upload_gcode_file), or the
	// file to remove (delete_gcode_file).
	Filename string
	// LocalPath is the local source file for upload_gcode_file.
	LocalPath string
	// TargetC is the requested target temperature in Celsius
	// (set_nozzle_temperature, set_bed_temperature).
	TargetC float64
	// Fan names which of the three channels set_fan_speed addresses.
	Fan domain.FanChannel
	// FanPercent is the requested fan speed, 0 to 100 (set_fan_speed).
	FanPercent float64
	// Percent is the requested speed or flow factor, e.g. 100 for
	// unchanged (set_speed_factor, set_flow_factor).
	Percent float64
	// Preset is the requested speed preset (set_speed_preset): silent, stable,
	// standard or ultrafast.
	Preset string
	// On is the requested light state (set_light).
	On bool
	// ObjectName is the object to exclude (exclude_object), matched
	// case-insensitively per dev_docs/safety-architecture.md 4.2.
	ObjectName string

	// Slot names the slot set_filament_definition edits (T1A..T4D or
	// side_spool, case-insensitive). Material is a 5-character catalog id or an
	// exact catalog name, and Color is #rrggbb or rrggbb (plan 3.3).
	Slot     string
	Material string
	Color    string

	// Source is start_print's filament source with a CFS connected: "cfs"
	// (default when empty) or "spool" (plan 3.4). SlotMap is the canonical
	// override form "T1A=T1C,T1B=T1D": slicer tool = physical slot.
	// SelfTest requests the printer's pre-print self-test; it only counts when
	// SelfTestExplicit is true, otherwise the printer's own enableSelfTest
	// value is the default, else false (plan 8a.7). SelfTestExplicit exists
	// because a plain bool cannot tell "not asked" from "asked for false".
	Source           string
	SlotMap          string
	SelfTest         bool
	SelfTestExplicit bool
}

// IdleHeatArmRequest is what Execute sent Deps.Watchdog.Arm for a
// successful idle-heat write (D2): this package arms the watchdog itself,
// synchronously, before the heater command is ever sent (Deps.Watchdog);
// this struct is kept on Result afterward for display only, not as a
// contract for the caller to arm anything itself. A nil field on Result
// means no idle-heat arm was needed for that call (mid-print, or an
// idle-bucket target of 0).
type IdleHeatArmRequest struct {
	Identity   string // printer identity: the verified hostname resolveExecuteIdentity resolved (review backlog item 31)
	Heater     string // "extruder" or "heater_bed"
	TargetC    float64
	ArmMinutes int // settings.IdleHeatMinutes at the time of the call

	// Host, MoonrakerPort and APIKey are this printer's live connection
	// details, carried through so the daemon's watchdog can reach the
	// printer directly at expiry with no registry lookup by identity
	// (dev_docs/safety-architecture.md section 10 D2, review backlog item
	// 36): an env-override printer (K2_MCP_HOST) is never saved to the
	// registry file, so a registry-backed lookup could never find it. APIKey
	// is kept in the daemon's process memory only for as long as the heater
	// stays armed and is never logged.
	Host          string
	MoonrakerPort int
	APIKey        string
}

// Result is what Execute returns for every outcome that is not a typed
// Error: either a no-side-effect proposal, or the outcome of a write that
// was actually sent.
type Result struct {
	Action ActionName

	// Proposed is true for a proposal_token/conditional call made without a
	// token: no command was sent, Token is the caller's next argument, and
	// Before is the only state block populated.
	Proposed  bool
	Token     string
	ExpiresAt time.Time

	// Accepted is true when the write's HTTP/WS call itself returned no
	// transport or protocol error. It is independent of Effect: a write can
	// be Accepted and still Effect "unconfirmed" if the settle poll timed
	// out, and (rarely) a write whose response was lost to the network can
	// still settle and read back Effect "confirmed" (dev_docs 10-hazard-analysis.md
	// section 3.6: never assume failure from a lost response).
	Accepted bool
	// Effect is "confirmed" once the settle condition was observed, or
	// "unconfirmed" if the settle timeout elapsed first. It is empty for a
	// Proposed result (nothing was sent yet).
	Effect string

	// Disclosed effects and the exact commands sent, for the proposal phase
	// and for the final result alike.
	Effects  []string
	Commands []string

	// Before and After are the state blocks from the fresh snapshot taken
	// immediately before sending, and from the last poll (or the same
	// snapshot, if the action needed no settle poll). After is empty for a
	// Proposed result.
	Before printerstate.StateBlock
	After  printerstate.StateBlock

	// Printer and Job identify what this call actually acted on
	// (dev_docs/safety-architecture.md section 5, hazard 10-hazard-analysis.md
	// 3.8/3.9).
	Printer domain.Printer
	Job     *printerstate.JobIdentity

	// IdleHeatArm is set only for a successful set_nozzle_temperature or
	// set_bed_temperature write made while idle with a nonzero target (D2):
	// it reports what Execute already armed on Deps.Watchdog before sending,
	// for display only.
	IdleHeatArm *IdleHeatArmRequest

	// StartPrintFlowRestored is set only for start_print: it reports the
	// flow factor percent this call restored because the print did not
	// reach printing (D7). A nil value means either the print reached
	// printing normally, or the flow factor was already 100 and needed no
	// restore.
	StartPrintFlowRestored *float64

	// Changed lists which bound fields a rejected proposal_token call found
	// different from the ones it was issued for (conflict only).
	Changed []string

	// Filament is set by set_filament_definition: the slot before and after and
	// the printer's same_material regrouping (plan 3.2).
	Filament *FilamentChange

	// SpeedPreset is set by set_speed_preset: the target and what each channel
	// reported after the write (plan-v0.3.0.md 2a.5).
	SpeedPreset *SpeedPresetReport

	// Mapping is set by a CFS start_print proposal and result: one entry per
	// file filament with the slot it maps to (plan 3.2, 3.4).
	Mapping []MappedFilament
}

// SpeedPresetReport is a set_speed_preset outcome's two channel readings:
// Moonraker's (Silent flag and speed factor, authoritative) and port 9999's
// (speedMode and curFeedratePct, corroborating; nil when it did not report
// them). Notes explain a disagreement, an absent 9999 reading or a partial
// application.
type SpeedPresetReport struct {
	Preset             string
	MoonrakerSilent    string // on, off or unknown
	MoonrakerFactorPct *float64
	SpeedMode9999      *int
	CurFeedratePct9999 *int
	Notes              []string
}

// SlotDefinition is one slot's filament definition as the printer reports it
// (9999 boxsInfo).
type SlotDefinition struct {
	RFID   string // 5-character catalog id
	Vendor string
	Type   string
	Name   string
	Color  string // #rrggbb
	State  int
}

// FilamentChange reports a set_filament_definition edit (plan 3.2, 3.3).
// MoonrakerMaterialType and MoonrakerColor are the read-back values from the
// Moonraker box (or filament_rack) object, empty when they could not be read.
type FilamentChange struct {
	Slot                  string
	Before, After         SlotDefinition
	SameMaterialBefore    []string
	SameMaterialAfter     []string
	MoonrakerMaterialType string
	MoonrakerColor        string
}

// MappedFilament is one row of a CFS start mapping (plan 3.2, 3.4, 8a.4).
// SameGroup lists every member of the mapped slot's same_material group and
// LikelyRunAs the slot the printer is expected to actually run (the group's
// first member); Warnings carries the canonicalisation and colour-distance
// disclosures.
type MappedFilament struct {
	Index       int
	ToolID      string
	FileType    string
	FileColor   string
	Slot        string
	SlotVendor  string
	SlotName    string
	SlotType    string
	SlotColor   string
	Distance    float64
	SameGroup   []string
	LikelyRunAs string
	Warnings    []string
}

// DisplayName joins a brand and a name for display: just the name when it
// already starts with the brand (case-insensitive), so "Generic" + "Generic PETG"
// reads "Generic PETG", otherwise "<brand> <name>".
func DisplayName(brand, name string) string {
	brand, name = strings.TrimSpace(brand), strings.TrimSpace(name)
	if brand == "" || strings.HasPrefix(strings.ToLower(name), strings.ToLower(brand)) {
		return name
	}
	return brand + " " + name
}
