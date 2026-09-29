package policy

import (
	"math"
	"strings"
	"testing"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.5f, want %.5f (+-%g)", name, got, want, tol)
	}
}

// The first three pairs of Sharma, Wu and Dalal's published CIEDE2000 test
// data (kL = kC = kH = 1) pin the deltaE2000 formula itself.
func TestDeltaE2000_PublishedPairs(t *testing.T) {
	near(t, "pair 1", deltaE2000(50, 2.6772, -79.7751, 50, 0, -82.7485, 1, 1, 1), 2.0425, 1e-4)
	near(t, "pair 2", deltaE2000(50, 3.1571, -77.2803, 50, 0, -82.7485, 1, 1, 1), 2.8615, 1e-4)
	near(t, "pair 3", deltaE2000(50, 2.8361, -74.0200, 50, 0, -82.7485, 1, 1, 1), 3.4412, 1e-4)
}

// Hand-computed with Creality's weights (kL 0.8): black to white is a pure
// lightness difference of 100 at a mean lightness of 50, where S_L is 1, so
// the distance is 100 / (1 * 0.8) = 125. Identical colours are 0, and the
// distance is symmetric.
func TestColorDistance_HandComputed(t *testing.T) {
	near(t, "black vs white", colorDistance("#000000", "#ffffff"), 125, 0.01)
	near(t, "identical", colorDistance("#f4e076", "#F4E076"), 0, 1e-9)
	near(t, "symmetric", colorDistance("#123456", "#abcdef")-colorDistance("#abcdef", "#123456"), 0, 1e-9)
	// A shorter-than-six-digit string reads as black, like Creality's parser.
	near(t, "short is black", colorDistance("#abc", "#000000"), 0, 1e-9)
}

func TestToolIDAndSlotLabel(t *testing.T) {
	for n, want := range map[int]string{0: "T1A", 1: "T1B", 3: "T1D", 4: "T2A", 15: "T4D"} {
		if got := toolID(n); got != want {
			t.Errorf("toolID(%d) = %s, want %s", n, got, want)
		}
		if back, ok := parseToolID(want); !ok || back != n {
			t.Errorf("parseToolID(%s) = %d,%v", want, back, ok)
		}
	}
	if got := slotLabel(1, 2); got != "T1C" {
		t.Errorf("slotLabel(1,2) = %s", got)
	}
	if box, mat, ok := parseSlotLabel("t2d"); !ok || box != 2 || mat != 3 {
		t.Errorf("parseSlotLabel(t2d) = %d,%d,%v", box, mat, ok)
	}
	for _, bad := range []string{"", "T5A", "T1E", "T0A", "TA1", "T1"} {
		if _, ok := parseToolID(bad); ok {
			t.Errorf("parseToolID(%q) accepted", bad)
		}
	}
}

func pool(slots ...poolSlot) []poolSlot { return slots }

func slot(label, typ, color string) poolSlot {
	box, mat, _ := parseSlotLabel(label)
	return poolSlot{BoxID: box, MaterialID: mat, Label: label, Type: typ, Color: color, State: 1}
}

func TestGetMatchColor_ExactTypeAndFirstWinsTie(t *testing.T) {
	p := pool(slot("T1A", "PLA", "#000000"), slot("T1B", "PETG", "#000000"), slot("T1C", "PETG", "#000000"))
	m := getMatchColor("#000000", "PETG", p)
	if m.idx != 1 {
		t.Fatalf("idx = %d, want 1 (the first of two identical PETG slots, PLA never considered)", m.idx)
	}
	if m := getMatchColor("#000000", "petg", p); m.idx != -1 {
		t.Fatalf("type comparison must be case-sensitive, got idx %d", m.idx)
	}
	if m := getMatchColor("#000000", "PETG-CF", p); m.idx != -1 {
		t.Fatalf("PETG-CF must not match a PETG slot (no family matching), got idx %d", m.idx)
	}
}

func byFilament(as []assignment) map[int]string {
	out := map[int]string{}
	for _, a := range as {
		out[a.Filament.Index] = a.Slot.Label
	}
	return out
}

// Two black PETG slots: the first filament takes the lowest index (T1A),
// consumption gives the second filament the other one.
func TestMatchFilaments_ConsumedSlotsAndLowestIndexTie(t *testing.T) {
	p := pool(slot("T1A", "PETG", "#000000"), slot("T1B", "PETG", "#000000"))
	fs := []fileFilament{{0, "PETG", "#000000"}, {1, "PETG", "#000000"}}
	as, un, err := matchFilaments(fs, p, nil)
	if err != nil || len(un) != 0 {
		t.Fatalf("err=%v unmatched=%v", err, un)
	}
	if got := byFilament(as); got[0] != "T1A" || got[1] != "T1B" {
		t.Fatalf("mapping = %v, want 0->T1A 1->T1B", got)
	}
	// Three filaments, two slots: the third is unmatched (a slot is never used twice).
	fs = append(fs, fileFilament{2, "PETG", "#000000"})
	as, un, err = matchFilaments(fs, p, nil)
	if err != nil || len(as) != 2 || len(un) != 1 || un[0] != 2 {
		t.Fatalf("as=%d unmatched=%v err=%v, want 2 assigned and filament 2 unmatched", len(as), un, err)
	}
}

// The greedy loop takes the globally closest pair first among filaments
// whose slot type agrees with the current best: filament 1 (exact black) is
// paired with the black slot before filament 0 (dark grey) can claim it.
func TestMatchFilaments_ClosestPairFirst(t *testing.T) {
	p := pool(slot("T1A", "PETG", "#000000"), slot("T1B", "PETG", "#ffffff"))
	fs := []fileFilament{{0, "PETG", "#303030"}, {1, "PETG", "#000000"}}
	as, _, err := matchFilaments(fs, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := byFilament(as); got[1] != "T1A" || got[0] != "T1B" {
		t.Fatalf("mapping = %v, want 1->T1A (closest pair first) and 0->T1B", got)
	}
}

// Two types interleaved: each type is matched only against its own slots.
func TestMatchFilaments_InterleavedTypes(t *testing.T) {
	p := pool(slot("T1A", "PLA", "#ff0000"), slot("T1B", "PETG", "#0000ff"), slot("T1C", "PLA", "#00ff00"), slot("T1D", "PETG", "#ffffff"))
	fs := []fileFilament{{0, "PETG", "#ffffff"}, {1, "PLA", "#00ff00"}, {2, "PETG", "#0000ff"}, {3, "PLA", "#ff0000"}}
	as, un, err := matchFilaments(fs, p, nil)
	if err != nil || len(un) != 0 {
		t.Fatalf("err=%v unmatched=%v", err, un)
	}
	got := byFilament(as)
	if got[0] != "T1D" || got[1] != "T1C" || got[2] != "T1B" || got[3] != "T1A" {
		t.Fatalf("mapping = %v", got)
	}
	if s := canonicalMapping(as); s != "T1A=T1D,T1B=T1C,T1C=T1B,T1D=T1A" {
		t.Fatalf("canonical = %q", s)
	}
}

func TestMatchFilaments_UnmatchedType(t *testing.T) {
	p := pool(slot("T1A", "PLA", "#ff0000"))
	fs := []fileFilament{{0, "ABS", "#ff0000"}, {1, "PLA", "#ff0000"}}
	as, un, err := matchFilaments(fs, p, nil)
	if err != nil || len(as) != 1 || len(un) != 1 || un[0] != 0 {
		t.Fatalf("as=%d unmatched=%v err=%v, want filament 0 (ABS) unmatched", len(as), un, err)
	}
}

func TestMatchFilaments_Overrides(t *testing.T) {
	p := pool(slot("T1A", "PETG", "#000000"), slot("T1B", "PETG", "#000000"), slot("T1C", "PLA", "#ff0000"))
	fs := []fileFilament{{0, "PETG", "#000000"}, {1, "PETG", "#000000"}}

	as, _, err := matchFilaments(fs, p, map[int]string{0: "T1B"})
	if err != nil {
		t.Fatal(err)
	}
	if got := byFilament(as); got[0] != "T1B" || got[1] != "T1A" {
		t.Fatalf("mapping = %v, want the override 0->T1B and the rest matched 1->T1A", got)
	}

	for name, ov := range map[string]map[int]string{
		"type mismatch":      {0: "T1C"},
		"undefined slot":     {0: "T2A"},
		"filament beyond":    {5: "T1A"},
		"slot already taken": {0: "T1A", 1: "T1A"},
	} {
		if _, _, err := matchFilaments(fs, p, ov); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseSlotMap(t *testing.T) {
	got, err := parseSlotMap("t1a=T1C, T1B=t1d")
	if err != nil || got[0] != "T1C" || got[1] != "T1D" || len(got) != 2 {
		t.Fatalf("got %v err %v", got, err)
	}
	if m, err := parseSlotMap(""); err != nil || len(m) != 0 {
		t.Fatalf("empty: %v %v", m, err)
	}
	for _, bad := range []string{"T1A", "T1A=", "X=T1A", "T1A=Z9", "T1A=T1B,T1A=T1C", "T1A=T1C,T1B=T1C"} {
		if _, err := parseSlotMap(bad); err == nil || !strings.Contains(err.Error(), "slot_map") {
			t.Errorf("parseSlotMap(%q) = %v, want a slot_map error", bad, err)
		}
	}
}
