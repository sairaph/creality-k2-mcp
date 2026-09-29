package policy

import (
	"fmt"
	"strings"

	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// cfsRule is an action's rule while a CFS is connected (plan-v0.2.0.md
// section 3.1). It replaces the blanket BlockedByCFS of v0.1.0 (D5), which
// blocked everything except stopping because CFS state was unvalidated. The
// CFS flags on printerstate.Derived (Known, Quiescent, Error) now let each
// action say exactly what it needs. The rule is applied only when
// derived.CFSConnected; the bucket gate always runs first.
type cfsRule int

const (
	// cfsNone: the bucket gate alone decides (pause_print, cancel_print,
	// set_light, set_bed_temperature, upload_gcode_file, delete_gcode_file).
	// Stopping must never be blocked by a CFS signal, and upload, delete and
	// bed temperature have no CFS interaction (V9).
	cfsNone cfsRule = iota
	// cfsKnownNoError: the CFS must be positively read and error free
	// (exclude_object, set_speed_factor).
	cfsKnownNoError
	// cfsFan: set_fan_speed. Bucket I needs a quiescent CFS; bucket P needs
	// Known and no error.
	cfsFan
	// cfsNozzle: set_nozzle_temperature. Bucket I needs a quiescent CFS;
	// during a print it is refused (V5): the CFS changes the nozzle
	// temperature itself during filament changes and this server cannot
	// detect a tool change.
	cfsNozzle
	// cfsRefuse: set_flow_factor is refused whenever a CFS is connected (V5).
	cfsRefuse
	// cfsStart: start_print needs a quiescent CFS, plus the mapping proposal
	// of section 3.4.
	cfsStart
	// cfsResume: resume_print needs Known and no error here; the remaining
	// conditions need the snapshot and the pause record (section 3.5,
	// checkResumeCFS).
	cfsResume
	// cfsQuiescent: set_filament_definition needs a quiescent CFS.
	cfsQuiescent
)

func (r cfsRule) String() string {
	switch r {
	case cfsNone:
		return "cfsNone"
	case cfsKnownNoError:
		return "cfsKnownNoError"
	case cfsFan:
		return "cfsFan"
	case cfsNozzle:
		return "cfsNozzle"
	case cfsRefuse:
		return "cfsRefuse"
	case cfsStart:
		return "cfsStart"
	case cfsResume:
		return "cfsResume"
	case cfsQuiescent:
		return "cfsQuiescent"
	}
	return "cfsUnknown"
}

// checkCFS applies spec.CFS to derived. It returns nil when the rule allows
// the action (or no CFS is connected), otherwise a CodeUnavailable error
// that names the rule and the next step. params is accepted so a rule can
// depend on it later; none of the current rules does.
func checkCFS(spec actionSpec, derived printerstate.Derived, params Params) *Error {
	_ = params
	if !derived.CFSConnected {
		return nil
	}
	refuse := func(format string, args ...any) *Error {
		return &Error{Action: spec.Name, Code: CodeUnavailable, Message: fmt.Sprintf(format, args...)}
	}
	why := cfsWhy(derived)
	switch spec.CFS {
	case cfsNone:
		return nil
	case cfsKnownNoError:
		if !derived.CFSKnown || derived.CFSError {
			return refuse("%s needs a CFS that is positively read and reports no error (rule %s): %s. Check the CFS on the printer, or call get_printer_status to see its state", spec.Name, spec.CFS, why)
		}
	case cfsFan:
		switch derived.Bucket {
		case printerstate.BucketI:
			if !derived.CFSQuiescent {
				return refuse("%s needs a quiescent CFS while idle (rule %s): %s. Wait for the CFS to finish, or check it on the printer", spec.Name, spec.CFS, why)
			}
		default:
			if !derived.CFSKnown || derived.CFSError {
				return refuse("%s during a print needs a CFS that is positively read and reports no error (rule %s): %s", spec.Name, spec.CFS, why)
			}
		}
	case cfsNozzle:
		if derived.Bucket == printerstate.BucketI {
			if !derived.CFSQuiescent {
				return refuse("%s needs a quiescent CFS while idle (rule %s): %s. Wait for the CFS to finish, or check it on the printer", spec.Name, spec.CFS, why)
			}
			return nil
		}
		return refuse("a connected CFS changes the nozzle temperature itself during filament changes, which this server cannot detect, so it will not change it during a print (rule %s). Change it on the printer if you must", spec.CFS)
	case cfsRefuse:
		return refuse("a connected CFS makes the flow factor unsafe to change: flow scales its purge volumes, so this server will not change it while a CFS is connected (rule %s). Change it on the printer if you must", spec.CFS)
	case cfsStart, cfsQuiescent:
		if !derived.CFSQuiescent {
			return refuse("%s needs a quiescent CFS (rule %s): %s. Wait for the CFS to finish or clear its error on the printer, then try again", spec.Name, spec.CFS, why)
		}
	case cfsResume:
		if !derived.CFSKnown || derived.CFSError {
			return refuse("%s needs a CFS that is positively read and reports no error (rule %s): %s. This pause was not necessarily clean: resume on the printer screen or in Creality Print", spec.Name, spec.CFS, why)
		}
		if !derived.PauseRecorded {
			return refuse("resume_print with a CFS connected is refused (rule %s): %s. %s", spec.CFS, noPauseRecordReason, resumeRefusal)
		}
	}
	return nil
}

// cfsWhy renders the CFS reasons for a refusal message.
func cfsWhy(derived printerstate.Derived) string {
	if r := nonEmpty(derived.CFSReasons); len(r) > 0 {
		return strings.Join(r, "; ")
	}
	return "no CFS reason recorded"
}

// cfsEffectNote is the CFS-dependent effect note added to a Result only when
// a CFS is connected, never to the static spec (plan 3.1).
func cfsEffectNote(name ActionName, derived printerstate.Derived) string {
	if !derived.CFSConnected {
		return ""
	}
	switch name {
	case ActionSetFanSpeed, ActionSetSpeedFactor:
		return "a CFS is connected: on a CFS print the next filament change can reset this"
	}
	return ""
}

// withCFSNote returns effects plus the CFS note for name, when one applies,
// without touching the static spec's slice.
func withCFSNote(effects []string, name ActionName, derived printerstate.Derived) []string {
	note := cfsEffectNote(name, derived)
	if note == "" {
		return effects
	}
	out := make([]string, 0, len(effects)+1)
	out = append(out, effects...)
	return append(out, note)
}

// pauseRecordNote tells the caller, after a CFS pause, whether a resume from
// this server is possible: it is only when a record was kept (the CFS was Known
// and error free when the pause settled), and then only while the CFS stays
// clean and nothing is changed at the printer.
func pauseRecordNote(recorded bool) string {
	if recorded {
		return "a CFS is connected: a resume record was kept, so resume from this server is possible only while the CFS stays clean and nothing is changed at the printer"
	}
	return "a CFS is connected: no resume record was kept (the CFS was not read clean when the pause settled, or the pause was not confirmed), so resume this print on the printer screen or in Creality Print"
}
