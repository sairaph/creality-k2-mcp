package policy

import (
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// actionSpec is the static, declarative half of dev_docs/safety-architecture.md
// section 3.2's Action table: everything about an action that does not
// depend on the parameters or the printer's live state. The dynamic half
// (extra fresh preconditions, parameter rules, Send, Settle) lives in
// execute.go as one function per action, referenced by name from Execute's
// switch; keeping the two halves separate lets Available (display only) and
// the regression tests (allowlist_test.go, forbidden_test.go) work from this
// table alone, without touching the network-calling code at all.
type actionSpec struct {
	Name ActionName

	// AllowedBuckets is dev_docs/safety-architecture.md section 4.1's bucket
	// column: the only printerstate.Bucket values Execute will act in.
	AllowedBuckets map[printerstate.Bucket]bool

	// CFS is this action's rule while a CFS is connected (section 3.1 of
	// dev_docs/plan-v0.2.0.md; cfs.go). It replaces v0.1.0's blanket
	// BlockedByCFS: pause_print and cancel_print are cfsNone (stopping must
	// never be blocked by a CFS signal), every other action names what it
	// needs. Only consulted when the derived state says a CFS is connected.
	CFS cfsRule

	// Confirmation is the static default; only upload_gcode_file's spec
	// uses ConfirmationConditional, resolved per call by whether the fresh
	// snapshot shows the target file already exists.
	Confirmation Confirmation

	// Effects are the disclosed physical/persistent side effects, drawn
	// from references/printer-snapshot/config/gcode_macro.cfg's real macro
	// bodies (dev_docs/safety-architecture.md section 3.2).
	Effects []string

	// Commands lists, as human-readable strings, every closed command or
	// endpoint this action can send. Templates duplicates the subset that
	// are moonraker.Template values, for allowlist_test.go's membership
	// check; Commands also lists the non-template endpoint calls (e.g.
	// "PrintStart", "Upload") for documentation and Result.Commands.
	Commands  []string
	Templates []moonraker.Template

	// SettleTimeout bounds the poll loop (11-state-model.md section 3.3).
	SettleTimeout time.Duration
}

// pollInterval is the settle-poll interval every action uses
// (11-state-model.md section 3.2: "500ms-1s is reasonable").
const pollInterval = 500 * time.Millisecond

// bucketSet builds an AllowedBuckets map from a short list, so each spec
// below reads the same as safety-architecture.md section 4.1's table.
func bucketSet(buckets ...printerstate.Bucket) map[printerstate.Bucket]bool {
	m := make(map[printerstate.Bucket]bool, len(buckets))
	for _, b := range buckets {
		m[b] = true
	}
	return m
}

// allBucketsExcept builds an AllowedBuckets map of every bucket other than
// the ones listed, for the "all but U" / "all but offline" style rows.
func allBucketsExcept(excluded ...printerstate.Bucket) map[printerstate.Bucket]bool {
	all := []printerstate.Bucket{
		printerstate.BucketU, printerstate.BucketE, printerstate.BucketB,
		printerstate.BucketPP, printerstate.BucketP, printerstate.BucketT,
		printerstate.BucketZ, printerstate.BucketI,
	}
	ex := bucketSet(excluded...)
	m := make(map[printerstate.Bucket]bool, len(all))
	for _, b := range all {
		if !ex[b] {
			m[b] = true
		}
	}
	return m
}

// specs is the complete, fixed action table (dev_docs/safety-architecture.md
// section 3.2 and 4.2, as overridden by section 10's final decisions
// D1-D7). There is no way to Execute an action not listed here.
var specs = map[ActionName]actionSpec{
	ActionStartPrint: {
		Name:           ActionStartPrint,
		AllowedBuckets: bucketSet(printerstate.BucketI),
		CFS:            cfsStart,
		Confirmation:   ConfirmationNone,
		Effects: []string{
			"START_PRINT heats the bed and nozzle, homes, and cleans the nozzle before printing begins",
			"M221 S100 is sent first if the flow factor is not already 100 (10-hazard-analysis.md 2.4); D7: the previous flow factor is restored if the print does not reach printing",
		},
		Commands:      []string{"PrintStart", "M221"},
		Templates:     []moonraker.Template{moonraker.TemplateM221},
		SettleTimeout: 30 * time.Second,
	},
	ActionPausePrint: {
		Name:           ActionPausePrint,
		AllowedBuckets: bucketSet(printerstate.BucketP), // PP explicitly excluded: pause during START_PRINT is unverified
		CFS:            cfsNone,
		Confirmation:   ConfirmationNone,
		Effects: []string{
			"nozzle target drops to 140 C",
			"if homed: lifts, moves to the purge/clean position, wipes the nozzle, and parks",
			"part and auxiliary fans turn off",
		},
		Commands:      []string{"PrintPause"},
		SettleTimeout: 30 * time.Second,
	},
	ActionResumePrint: {
		Name:           ActionResumePrint,
		AllowedBuckets: bucketSet(printerstate.BucketZ),
		CFS:            cfsResume,
		Confirmation:   ConfirmationProposalToken,
		Effects: []string{
			"reheats the nozzle to the stored pre-pause target",
			"homes X/Y if unhomed, then purges 82 mm of filament and wipes the nozzle, before the firmware's own pause check ever runs (10-hazard-analysis.md 2.4, confirmed live)",
			"restores the stored part and auxiliary fan speeds",
		},
		Commands:      []string{"PrintResume"},
		SettleTimeout: 120 * time.Second,
	},
	ActionCancelPrint: {
		Name:           ActionCancelPrint,
		AllowedBuckets: bucketSet(printerstate.BucketP, printerstate.BucketPP, printerstate.BucketZ),
		CFS:            cfsNone,
		Confirmation:   ConfirmationProposalToken,
		Effects: []string{
			"END_PRINT runs: lifts, retracts if hot, turns off heaters and fans, parks",
			"the EEPROM power-loss-recovery slot is cleared (CLEAR_EEPROM_INFO), a genuine physical write (08-klipper-persistence-guards.md 1.5)",
		},
		Commands:      []string{"PrintCancel"},
		SettleTimeout: 60 * time.Second,
	},
	ActionSetNozzleTemperature: {
		Name:           ActionSetNozzleTemperature,
		AllowedBuckets: bucketSet(printerstate.BucketP, printerstate.BucketI),
		CFS:            cfsNozzle,
		Confirmation:   ConfirmationNone,
		Effects: []string{
			"changes the running print's nozzle target (while printing), or heats with no job queued and no idle heater shutoff on this printer, until the idle-heat watchdog turns it off (D2)",
		},
		Commands:      []string{"SET_HEATER_TEMPERATURE HEATER=extruder"},
		Templates:     []moonraker.Template{moonraker.TemplateSetHeaterTemperature},
		SettleTimeout: 10 * time.Second,
	},
	ActionSetBedTemperature: {
		Name:           ActionSetBedTemperature,
		AllowedBuckets: bucketSet(printerstate.BucketP, printerstate.BucketI),
		CFS:            cfsNone,
		Confirmation:   ConfirmationNone,
		Effects: []string{
			"changes the running print's bed target (while printing), or heats with no job queued and no idle heater shutoff on this printer, until the idle-heat watchdog turns it off (D2)",
		},
		Commands:      []string{"SET_HEATER_TEMPERATURE HEATER=heater_bed"},
		Templates:     []moonraker.Template{moonraker.TemplateSetHeaterTemperature},
		SettleTimeout: 10 * time.Second,
	},
	ActionSetFanSpeed: {
		Name:           ActionSetFanSpeed,
		AllowedBuckets: bucketSet(printerstate.BucketP, printerstate.BucketI),
		CFS:            cfsFan,
		Confirmation:   ConfirmationNone,
		Effects: []string{
			"changes the running print's fan speed (while printing)",
			"the case fan shares its pins with the chamber_fan thermostat and can be overridden later by it",
		},
		Commands:      []string{"M106"},
		Templates:     []moonraker.Template{moonraker.TemplateM106},
		SettleTimeout: 10 * time.Second,
	},
	ActionSetSpeedFactor: {
		Name:           ActionSetSpeedFactor,
		AllowedBuckets: bucketSet(printerstate.BucketP),
		CFS:            cfsKnownNoError,
		Confirmation:   ConfirmationNone,
		Effects:        []string{"runtime only, reset by START_PRINT/END_PRINT"},
		Commands:       []string{"M220"},
		Templates:      []moonraker.Template{moonraker.TemplateM220},
		SettleTimeout:  10 * time.Second,
	},
	ActionSetFlowFactor: {
		Name:           ActionSetFlowFactor,
		AllowedBuckets: bucketSet(printerstate.BucketP),
		CFS:            cfsRefuse,
		Confirmation:   ConfirmationNone,
		Effects:        []string{"runtime only, NOT reset by Creality's macros; start_print resets it defensively (10-hazard-analysis.md 2.4)"},
		Commands:       []string{"M221"},
		Templates:      []moonraker.Template{moonraker.TemplateM221},
		SettleTimeout:  10 * time.Second,
	},
	ActionSetLight: {
		Name:           ActionSetLight,
		AllowedBuckets: allBucketsExcept(), // gated on "not offline" directly, not by bucket (see checkSetLight)
		CFS:            cfsNone,
		Confirmation:   ConfirmationNone,
		Effects:        []string{"none: physically inert, drives the chamber light only"},
		Commands:       []string{"SetLight (port 9999)"},
		SettleTimeout:  5 * time.Second,
	},
	ActionExcludeObject: {
		Name:           ActionExcludeObject,
		AllowedBuckets: bucketSet(printerstate.BucketP, printerstate.BucketZ),
		CFS:            cfsKnownNoError,
		Confirmation:   ConfirmationProposalToken,
		Effects:        []string{"the object stops printing; irreversible for this job"},
		Commands:       []string{"EXCLUDE_OBJECT"},
		Templates:      []moonraker.Template{moonraker.TemplateExcludeObject},
		SettleTimeout:  10 * time.Second,
	},
	ActionUploadGCodeFile: {
		Name:           ActionUploadGCodeFile,
		AllowedBuckets: allBucketsExcept(printerstate.BucketU),
		CFS:            cfsNone,
		Confirmation:   ConfirmationConditional, // token only when overwriting an existing file
		Effects:        []string{"none on printer state; never starts a print"},
		Commands:       []string{"Upload"},
		SettleTimeout:  10 * time.Second,
	},
	ActionDeleteGCodeFile: {
		Name:           ActionDeleteGCodeFile,
		AllowedBuckets: allBucketsExcept(printerstate.BucketU),
		CFS:            cfsNone,
		Confirmation:   ConfirmationProposalToken,
		Effects:        []string{"the file is removed from the gcodes root"},
		Commands:       []string{"Delete"},
		SettleTimeout:  10 * time.Second,
	},
	ActionSetFilamentDefinition: {
		Name:           ActionSetFilamentDefinition,
		AllowedBuckets: bucketSet(printerstate.BucketI),
		CFS:            cfsQuiescent,
		Confirmation:   ConfirmationNone,
		Effects: []string{
			"rewrites one slot's filament definition stored on the printer (material, brand, colour, and the nozzle temperature range and pressure advance of that catalog entry); the printer recomputes which slots auto-refill treats as interchangeable; persistent until changed again; never moves filament",
		},
		Commands:      []string{"modifyMaterial (port 9999)"},
		SettleTimeout: 10 * time.Second,
	},
}
