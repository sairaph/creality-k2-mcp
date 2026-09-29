package printerstate

import (
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

func f64(v float64) *float64 { return &v }

// snapWith builds a snapshot carrying just the fields the speed-preset
// derivation reads.
func snapWith(custom, macro *float64, factor *float64) Snapshot {
	s := Snapshot{
		CustomMacro: &moonraker.CustomMacro{QmodeFlag: custom},
		QmodeMacro:  &moonraker.QmodeMacro{Flag: macro},
	}
	if factor != nil {
		s.GCodeMove = &moonraker.GCodeMove{SpeedFactor: factor}
	}
	return s
}

// The Silent flag is a tri-state (plan-v0.3.0.md 2a.1): On or Off only when
// both sources are present, 0 or 1, and agree; everything else is unknown.
func TestQmodeTriState(t *testing.T) {
	for name, tc := range map[string]struct {
		snap Snapshot
		want QmodeState
	}{
		"both off":                 {snapWith(f64(0), f64(0), nil), QmodeOff},
		"both on":                  {snapWith(f64(1), f64(1), nil), QmodeOn},
		"disagree":                 {snapWith(f64(1), f64(0), nil), QmodeUnknown},
		"custom missing":           {snapWith(nil, f64(0), nil), QmodeUnknown},
		"macro missing":            {snapWith(f64(0), nil, nil), QmodeUnknown},
		"out of range":             {snapWith(f64(2), f64(2), nil), QmodeUnknown},
		"no custom_macro object":   {Snapshot{QmodeMacro: &moonraker.QmodeMacro{Flag: f64(0)}}, QmodeUnknown},
		"no Qmode macro object":    {Snapshot{CustomMacro: &moonraker.CustomMacro{QmodeFlag: f64(0)}}, QmodeUnknown},
		"nothing decoded at all":   {Snapshot{}, QmodeUnknown},
		"macro decode error state": {Snapshot{CustomMacro: &moonraker.CustomMacro{QmodeFlag: f64(0)}, DecodeErrs: map[string]error{"gcode_macro Qmode": errDecode}}, QmodeUnknown},
	} {
		if got := qmodeFor(tc.snap); got != tc.want {
			t.Errorf("%s: qmodeFor = %v, want %v", name, got, tc.want)
		}
	}
	if (Derived{}).Qmode != QmodeUnknown {
		t.Fatal("a zero Derived must read as unknown (fail closed)")
	}
}

var errDecode = &decodeErr{}

type decodeErr struct{}

func (*decodeErr) Error() string { return "decode error" }

func TestDeriveActivityStateSetsQmode(t *testing.T) {
	snap := Snapshot{ServerInfoErr: errDecode}
	if d := DeriveActivityState(snap, nil); d.Qmode != QmodeUnknown {
		t.Fatalf("offline snapshot: Qmode = %v", d.Qmode)
	}
}

func TestSpeedPresetOf(t *testing.T) {
	printing := Derived{Bucket: BucketP}
	on, off := printing, printing
	on.Qmode, off.Qmode = QmodeOn, QmodeOff
	unknown := printing

	for name, tc := range map[string]struct {
		snap Snapshot
		d    Derived
		want string
	}{
		"silent":                        {snapWith(f64(1), f64(1), f64(0.5)), on, PresetSilent},
		"stable is 50% with Silent off": {snapWith(f64(0), f64(0), f64(0.5)), off, PresetStable},
		"standard":                      {snapWith(f64(0), f64(0), f64(1.0)), off, PresetStandard},
		"ultrafast":                     {snapWith(f64(0), f64(0), f64(1.25)), off, PresetUltrafast},
		"custom":                        {snapWith(f64(0), f64(0), f64(0.8)), off, PresetCustom},
		"tolerance":                     {snapWith(f64(0), f64(0), f64(1.0049)), off, PresetStandard},
		"unknown qmode":                 {snapWith(nil, nil, f64(1.0)), unknown, PresetUnknownStr},
		"factor missing":                {snapWith(f64(0), f64(0), nil), off, PresetUnknownStr},
	} {
		if got := SpeedPresetOf(tc.snap, tc.d); got != tc.want {
			t.Errorf("%s: SpeedPresetOf = %q, want %q", name, got, tc.want)
		}
	}

	// Port 9999 disagreement fails closed to unknown.
	s := snapWith(f64(0), f64(0), f64(1.0))
	s.WS9999Reachable = true
	s.WS9999 = crealityws.Status{SpeedMode: crealityws.Int{Value: 1, Present: true}}
	if got := SpeedPresetOf(s, off); got != PresetUnknownStr {
		t.Errorf("speedMode disagreement: %q", got)
	}
	s.WS9999 = crealityws.Status{CurFeedrate: crealityws.Int{Value: 125, Present: true}}
	if got := SpeedPresetOf(s, off); got != PresetUnknownStr {
		t.Errorf("curFeedratePct disagreement: %q", got)
	}
	s.WS9999 = crealityws.Status{SpeedMode: crealityws.Int{Value: 0, Present: true}, CurFeedrate: crealityws.Int{Value: 100, Present: true}}
	if got := SpeedPresetOf(s, off); got != PresetStandard {
		t.Errorf("agreeing 9999: %q", got)
	}
	// An unreachable 9999 (stale Status) is not evidence.
	s.WS9999Reachable = false
	s.WS9999 = crealityws.Status{SpeedMode: crealityws.Int{Value: 1, Present: true}}
	if got := SpeedPresetOf(s, off); got != PresetStandard {
		t.Errorf("unreachable 9999 counted: %q", got)
	}
}

// 2a.10: shown only in buckets P and Z.
func TestSpeedPresetShownOnlyInPrintBuckets(t *testing.T) {
	snap := snapWith(f64(0), f64(0), f64(1.0))
	for bucket, want := range map[Bucket]bool{
		BucketP: true, BucketZ: true, BucketI: false, BucketPP: false, BucketT: false, BucketB: false, BucketU: false, BucketE: false,
	} {
		d := Derived{Bucket: bucket, Qmode: QmodeOff}
		got := SpeedPresetOf(snap, d) != ""
		if got != want {
			t.Errorf("bucket %s shown = %v, want %v", bucket, got, want)
		}
		block := BuildStateBlock(snap, d, nil)
		if (block.SpeedPreset != "") != want || (block.SilentMode != "") != want {
			t.Errorf("bucket %s block preset=%q silent=%q", bucket, block.SpeedPreset, block.SilentMode)
		}
	}
}

func TestStateBlockCarriesBothChannels(t *testing.T) {
	snap := snapWith(f64(1), f64(1), f64(0.5))
	snap.WS9999Reachable = true
	snap.WS9999 = crealityws.Status{SpeedMode: crealityws.Int{Value: 1, Present: true}, CurFeedrate: crealityws.Int{Value: 50, Present: true}}
	block := BuildStateBlock(snap, Derived{Bucket: BucketP, Qmode: QmodeOn}, nil)
	if block.SpeedPreset != PresetSilent || block.SilentMode != "on" || block.SpeedMode9999 == nil || *block.SpeedMode9999 != 1 ||
		block.CurFeedratePct9999 == nil || *block.CurFeedratePct9999 != 50 {
		t.Fatalf("block = %+v", block)
	}
	// Absent 9999 readings stay absent.
	snap.WS9999 = crealityws.Status{}
	block = BuildStateBlock(snap, Derived{Bucket: BucketP, Qmode: QmodeOn}, nil)
	if block.SpeedMode9999 != nil || block.CurFeedratePct9999 != nil {
		t.Fatalf("absent readings reported: %+v", block)
	}
}

func TestPresetFactorPercent(t *testing.T) {
	for name, want := range map[string]float64{"silent": 50, "stable": 50, "standard": 100, "ultrafast": 125} {
		if got, ok := PresetFactorPercent(name); !ok || got != want {
			t.Errorf("%s = %v %v", name, got, ok)
		}
	}
	if _, ok := PresetFactorPercent("custom"); ok {
		t.Error("custom is not a requestable preset")
	}
}

func TestStateBlockShowsToolheadVelocityAsInformationOnly(t *testing.T) {
	snap := snapWith(f64(1), f64(1), f64(0.5))
	snap.Toolhead = &moonraker.Toolhead{MaxVelocity: 150}
	block := BuildStateBlock(snap, Derived{Bucket: BucketP, Qmode: QmodeOn}, nil)
	if block.ToolheadMaxVelocity == nil || *block.ToolheadMaxVelocity != 150 {
		t.Fatalf("block = %+v", block)
	}
	// Not shown while idle, and never read by the preset derivation.
	idle := BuildStateBlock(snap, Derived{Bucket: BucketI, Qmode: QmodeOn}, nil)
	if idle.ToolheadMaxVelocity != nil {
		t.Fatalf("idle block shows the velocity: %+v", idle)
	}
	snap.Toolhead.MaxVelocity = 800 // a file that sets its own velocity overrides 150
	if got := SpeedPresetOf(snap, Derived{Bucket: BucketP, Qmode: QmodeOn}); got != PresetSilent {
		t.Fatalf("Silent must not depend on the velocity: %q", got)
	}
}
