package crealityws

import (
	"strconv"
	"strings"
)

// Int is an optional int: a field that was absent from every push frame read
// (Present false) must not be confused with a field that was present and
// genuinely decoded to zero (Present true, Value 0). Several 9999 fields have
// a real, meaningful zero value (state 0 is idle, upgradeStatus 0 is "no
// upgrade", cfsConnect 0 is disconnected), so a consumer that reads Value
// without also checking Present would silently treat "we never heard from
// the printer about this" as that permissive zero (dev_docs/safety-
// architecture.md P1: fail closed on anything unknown).
type Int struct {
	Value   int
	Present bool
}

// Str is the string equivalent of Int; see its doc comment.
type Str struct {
	Value   string
	Present bool
}

// StatusErr mirrors the port 9999 "err" object: {"errcode": int, "key": int,
// "value": string}. Some firmware reports a bare int instead of an object
// (04-creality-ws-camera.md); ErrCode still carries the code in that case.
// Present is false when "err" was absent from every frame read or could not
// be decoded in either shape; a caller must not read ErrCode/Key/Value as
// "no error known" without checking it first (see Int's doc comment).
type StatusErr struct {
	ErrCode int
	Key     int
	Value   string
	Present bool
}

// Status is the subset of port 9999 telemetry the safety architecture uses
// (dev_docs/safety-architecture.md, section 3.1), decoded from whatever
// fields were present in the merged push frames.
//
// The fields below are grouped in two kinds:
//
//   - Safety-relevant fields (state, deviceState, feedState, upgradeStatus,
//     repoPlrStatus, powerLoss, materialStatus, cfsConnect, lightSw, err,
//     printId) use the optional Int/Str/StatusErr types above so presence is
//     part of the type: a caller cannot read Value without also seeing
//     Present, and cannot mistake "never reported" for a genuine zero.
//     MissingSafetyFields reports which of these were absent.
//   - The remaining fields are informational only (display and diagnostics,
//     never a gating decision) and keep plain types; a missing one decodes
//     to its zero value, matching the previous behaviour.
type Status struct {
	State          Int
	DeviceState    Int
	FeedState      Int
	UpgradeStatus  Int
	RepoPlrStatus  Int
	PowerLoss      Int
	MaterialStatus Int
	CfsConnect     Int
	LightSw        Int
	Err            StatusErr
	PrintID        Str

	// WithSelfTest and EnableSelfTest are the print-start self-test progress
	// (100 = finished or never started) and the printer-side self-test
	// setting (dev_docs/cfs-print-start.md sections 1.2 and 4.3). They are
	// optional Ints but deliberately not in MissingSafetyFields: a printer
	// that never reports them simply contributes no start-window signal, and
	// start_print then defaults the self-test flag to false (plan 8a.7).
	WithSelfTest   Int
	EnableSelfTest Int

	// Informational-only fields: see the Status doc comment.
	Model           string
	Hostname        string
	WebrtcSupport   int
	Video           int
	ModelFanPct     int
	CaseFanPct      int
	AuxiliaryFanPct int
	CurFeedratePct  int
	CurFlowratePct  int
	PrintProgress   int
	PrintFileName   string

	// Raw is the cumulative merge of every decoded push frame's fields, kept
	// for diagnostics beyond the typed fields above.
	Raw map[string]any
}

// MissingSafetyFields returns the raw JSON key of every safety-relevant field
// (see the Status doc comment) that was absent from every push frame read,
// in a fixed order. An empty result means the merged frames covered all of
// them at least once; it says nothing about the informational-only fields.
func (s Status) MissingSafetyFields() []string {
	var missing []string
	add := func(name string, present bool) {
		if !present {
			missing = append(missing, name)
		}
	}
	add("state", s.State.Present)
	add("deviceState", s.DeviceState.Present)
	add("feedState", s.FeedState.Present)
	add("upgradeStatus", s.UpgradeStatus.Present)
	add("repoPlrStatus", s.RepoPlrStatus.Present)
	add("powerLoss", s.PowerLoss.Present)
	add("materialStatus", s.MaterialStatus.Present)
	add("cfsConnect", s.CfsConnect.Present)
	add("lightSw", s.LightSw.Present)
	add("err", s.Err.Present)
	add("printId", s.PrintID.Present)
	return missing
}

// keyFields are present only in the printer's initial full-state push, never
// in a later delta (04-creality-ws-camera.md, section 1). Their presence in
// the merged map means we have seen that first push and can stop reading
// early instead of waiting out the full budget.
var keyFields = []string{"state", "deviceState", "model", "hostname"}

func hasKeyFields(raw map[string]any) bool {
	for _, k := range keyFields {
		if _, ok := raw[k]; !ok {
			return false
		}
	}
	return true
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

// buildStatus decodes the typed fields from a merged raw push map. Missing
// or unparsable fields are left at their zero value rather than erroring, so
// a partial capture still produces a usable Status.
func buildStatus(raw map[string]any) Status {
	s := Status{Raw: raw}
	s.State = asOptInt(raw["state"])
	s.DeviceState = asOptInt(raw["deviceState"])
	s.FeedState = asOptInt(raw["feedState"])
	s.UpgradeStatus = asOptInt(raw["upgradeStatus"])
	s.RepoPlrStatus = asOptInt(raw["repoPlrStatus"])
	s.PowerLoss = asOptInt(raw["powerLoss"])
	s.MaterialStatus = asOptInt(raw["materialStatus"])
	s.CfsConnect = asOptInt(raw["cfsConnect"])
	s.LightSw = asOptInt(raw["lightSw"])
	s.Err = asStatusErr(raw["err"])
	s.PrintID = asOptString(raw["printId"])
	s.WithSelfTest = asOptInt(raw["withSelfTest"])
	s.EnableSelfTest = asOptInt(raw["enableSelfTest"])
	s.Model, _ = asString(raw["model"])
	s.Hostname, _ = asString(raw["hostname"])
	s.WebrtcSupport, _ = asInt(raw["webrtcSupport"])
	s.Video, _ = asInt(raw["video"])
	s.ModelFanPct, _ = asInt(raw["modelFanPct"])
	s.CaseFanPct, _ = asInt(raw["caseFanPct"])
	s.AuxiliaryFanPct, _ = asInt(raw["auxiliaryFanPct"])
	s.CurFeedratePct, _ = asInt(raw["curFeedratePct"])
	s.CurFlowratePct, _ = asInt(raw["curFlowratePct"])
	s.PrintProgress, _ = asInt(raw["printProgress"])
	s.PrintFileName, _ = asString(raw["printFileName"])
	return s
}

// asInt coerces a decoded JSON value into an int. json.Unmarshal into
// map[string]any turns JSON numbers into float64, but this firmware streams
// several numeric fields as decimal strings (e.g. "nozzleTemp":
// "29.360000"); asInt accepts either shape, plus a bare bool as 0/1, so any
// field on this protocol can be read consistently regardless of which
// encoding a given firmware build used for it.
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, false
		}
		return int(f), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// asOptInt wraps asInt in the optional Int type: Present is true only when v
// was actually decodable, so a field that was absent from every frame (v is
// nil) or present but unparsable comes out the same way, not present, which
// is the fail-closed choice (P1) rather than guessing it forward as a genuine
// zero.
func asOptInt(v any) Int {
	val, ok := asInt(v)
	return Int{Value: val, Present: ok}
}

// asOptString is the Str equivalent of asOptInt.
func asOptString(v any) Str {
	val, ok := asString(v)
	return Str{Value: val, Present: ok}
}

// asStatusErr decodes the "err" field, which is normally an object but on
// some firmware is a bare int errcode (04-creality-ws-camera.md, section
// 1.7). Anything else (absent, garbage) yields the zero value with Present
// false, i.e. "no error known" is never inferred, only observed.
func asStatusErr(v any) StatusErr {
	switch t := v.(type) {
	case map[string]any:
		var e StatusErr
		e.ErrCode, _ = asInt(t["errcode"])
		e.Key, _ = asInt(t["key"])
		e.Value, _ = asString(t["value"])
		e.Present = true
		return e
	default:
		if code, ok := asInt(v); ok {
			return StatusErr{ErrCode: code, Present: true}
		}
		return StatusErr{}
	}
}
