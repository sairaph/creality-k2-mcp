package policy

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Filament-to-slot matching (dev_docs/plan-v0.2.0.md section 3.4;
// dev_docs/cfs-print-start.md section 3.1). This is a port of Creality's own
// Device Manager algorithm for "print a file that is already on the printer"
// (matchModelColor / getMatchColor / calculateColorDistance), so a start from
// this server maps filaments to slots exactly the way Creality's UI would:
//
//   - exact, case-sensitive material TYPE equality (not a family match),
//   - CIEDE2000 colour distance with weights kL 0.8, kC 0.6, kH 0.8, from an
//     sRGB (D65) to XYZ to Lab conversion,
//   - a greedy loop that consumes a slot per filament, taking the globally
//     closest pair first among filaments whose matched slot type agrees with
//     the first candidate of the round, and the lowest pool index on a tie.
//
// The loop is ported literally, quirks included (the "same slot type as the
// current best" condition), because a "cleaner" loop picks different pairs
// (review-2, minor findings). The one deliberate deviation from Creality's
// pool is that this server only offers slots with a definition (state 1 or 2)
// and a colour, where Creality's pool also includes state 0 with a colour.

const (
	ciedeKL = 0.8
	ciedeKC = 0.6
	ciedeKH = 0.8
)

// poolSlot is one candidate physical slot for a filament: a CFS slot with a
// definition and a colour.
type poolSlot struct {
	BoxID, MaterialID int
	Label             string // "T1B": unit and slot letter
	Type              string // the slot's material type, e.g. "PETG"
	Color             string // "#rrggbb"
	Vendor, Name      string
	RFID              string
	State             int
}

// fileFilament is one filament of the file, in tool order (index n = tool n).
type fileFilament struct {
	Index int
	Type  string
	Color string // "#rrggbb" as recorded by the slicer (may be empty)
}

// assignment is one matched pair.
type assignment struct {
	Filament fileFilament
	Slot     poolSlot
	Distance float64
}

// toolID names the slicer tool of filament n: "T" + unit (n/4+1) + letter
// (A + n%4), the key of the printer's tnn_map and the colorMatch id.
func toolID(n int) string {
	return fmt.Sprintf("T%d%c", n/4+1, 'A'+rune(n%4))
}

// slotLabel names a physical slot: unit id and slot index (A=0..D=3).
func slotLabel(boxID, materialID int) string {
	return fmt.Sprintf("T%d%c", boxID, 'A'+rune(materialID))
}

// parseToolID is the inverse of toolID for T1A..T4D.
func parseToolID(s string) (int, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 3 || s[0] != 'T' || s[1] < '1' || s[1] > '4' || s[2] < 'A' || s[2] > 'D' {
		return 0, false
	}
	return int(s[1]-'1')*4 + int(s[2]-'A'), true
}

// parseSlotLabel reads a slot name T1A..T4D into unit id and slot index.
func parseSlotLabel(s string) (boxID, materialID int, ok bool) {
	n, ok := parseToolID(s)
	if !ok {
		return 0, 0, false
	}
	return n/4 + 1, n % 4, true
}

// --- colour distance ---

func srgbLinear(v float64) float64 {
	if v > 0.04045 {
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return v / 12.92
}

func labF(t float64) float64 {
	if t > 0.008856 {
		return math.Cbrt(t)
	}
	return 7.787*t + 16.0/116.0
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return 10 + int(c-'a')
	case c >= 'A' && c <= 'F':
		return 10 + int(c-'A')
	}
	return 0
}

// parseHex reads "#RRGGBB" (the '#' optional) like Creality's parser: a
// string shorter than six digits reads as black.
func parseHex(s string) (r, g, b int) {
	s = strings.TrimPrefix(s, "#")
	if len(s) < 6 {
		return 0, 0, 0
	}
	return hexNibble(s[0])<<4 | hexNibble(s[1]), hexNibble(s[2])<<4 | hexNibble(s[3]), hexNibble(s[4])<<4 | hexNibble(s[5])
}

// hexToLab converts an sRGB colour to CIE Lab with the D65 white point
// Creality's code uses (0.95047, 1.0, 1.08883).
func hexToLab(hex string) (L, a, b float64) {
	ri, gi, bi := parseHex(hex)
	r, g, bl := srgbLinear(float64(ri)/255), srgbLinear(float64(gi)/255), srgbLinear(float64(bi)/255)
	x := (r*0.412453 + g*0.35758 + bl*0.180423) / 0.95047
	y := (r*0.212671 + g*0.71516 + bl*0.072169) / 1.0
	z := (r*0.019334 + g*0.119193 + bl*0.950227) / 1.08883
	fx, fy, fz := labF(x), labF(y), labF(z)
	return 116*fy - 16, 500 * (fx - fy), 200 * (fy - fz)
}

const degToRad = math.Pi / 180

// deltaE2000 is CIEDE2000 with parametric weights, line for line the same as
// Creality's match_color.cpp / the Device Manager JS.
func deltaE2000(L1, a1, b1, L2, a2, b2, KL, KC, KH float64) float64 {
	lMean := (L1 + L2) * 0.5
	lMean50 := math.Pow(lMean-50, 2)
	sL := 1 + (0.015*lMean50)/math.Sqrt(20+lMean50)
	dL := (L2 - L1) / (sL * KL)

	c1 := math.Sqrt(a1*a1 + b1*b1)
	c2 := math.Sqrt(a2*a2 + b2*b2)
	cMean := 0.5 * (c1 + c2)
	cMean7 := math.Pow(cMean, 7)
	pow25_7 := math.Pow(25, 7)
	g := 0.5 * (1 - math.Sqrt(cMean7/(cMean7+pow25_7)))

	a1x := a1 * (1 + g)
	a2x := a2 * (1 + g)
	c1x := math.Sqrt(a1x*a1x + b1*b1)
	c2x := math.Sqrt(a2x*a2x + b2*b2)
	cxMean := 0.5 * (c1x + c2x)
	sC := 1 + 0.045*cxMean
	dC := (c2x - c1x) / (sC * KC)

	const eps = 1e-4
	h1 := 270.0
	if c1x > eps {
		h1 = math.Atan2(b1, a1x) / degToRad
	}
	h2 := 270.0
	if c2x > eps {
		h2 = math.Atan2(b2, a2x) / degToRad
	}
	if h1 < 0 {
		h1 += 360
	}
	if h2 < 0 {
		h2 += 360
	}
	hMean := 0.5 * (h1 + h2)
	dh := h2 - h1
	if math.Abs(dh) > 180 {
		if hMean < 180 {
			hMean += 180
		} else {
			hMean -= 180
		}
		if h1 >= h2 {
			dh += 360
		} else {
			dh -= 360
		}
	}
	dHx := 2 * math.Sqrt(c1x*c2x) * math.Sin(0.5*dh*degToRad)
	t := 1 -
		0.17*math.Cos((hMean-30)*degToRad) +
		0.24*math.Cos(2*hMean*degToRad) +
		0.32*math.Cos((3*hMean+6)*degToRad) -
		0.20*math.Cos((4*hMean-63)*degToRad)
	sH := 1 + 0.015*cxMean*t
	dH := dHx / (sH * KH)

	dTheta := 30 * math.Exp(-math.Pow((hMean-275)/25, 2))
	cx7 := math.Pow(cxMean, 7)
	rC := 2 * math.Sqrt(cx7/(cx7+pow25_7))
	rT := -rC * math.Sin(2*dTheta*degToRad)

	return math.Sqrt(dL*dL + dC*dC + dH*dH + rT*dC*dH)
}

// colorDistance is Creality's calculateColorDistance: CIEDE2000 with
// kL 0.8, kC 0.6, kH 0.8 between two "#rrggbb" colours.
func colorDistance(hex1, hex2 string) float64 {
	L1, a1, b1 := hexToLab(hex1)
	L2, a2, b2 := hexToLab(hex2)
	return deltaE2000(L1, a1, b1, L2, a2, b2, ciedeKL, ciedeKC, ciedeKH)
}

// --- assignment ---

// poolMatch is getMatchColor's result: the pool index of the closest slot of
// the requested type, or idx -1 when no slot has that type.
type poolMatch struct {
	idx  int
	dist float64
}

// getMatchColor scans the pool in order, considers only slots whose type
// equals typ exactly, and keeps the smallest distance (strictly less, so the
// first slot in pool order wins a tie).
func getMatchColor(color, typ string, pool []poolSlot) poolMatch {
	best := poolMatch{idx: -1}
	for i, s := range pool {
		if s.Type != typ {
			continue
		}
		d := colorDistance(color, s.Color)
		if best.idx < 0 || d < best.dist {
			best = poolMatch{idx: i, dist: d}
		}
	}
	return best
}

// matchFilaments assigns a slot to each filament. Overrides (filament index
// to slot label) are applied first; each must name a pool slot of exactly the
// filament's type, and consumes it. The remaining filaments go through the
// greedy loop. The returned assignments are ordered by filament index;
// unmatched lists the indexes of filaments that got no slot.
func matchFilaments(filaments []fileFilament, pool []poolSlot, overrides map[int]string) (assigned []assignment, unmatched []int, err error) {
	pool = append([]poolSlot(nil), pool...)
	used := map[int]bool{}
	result := map[int]assignment{}

	for idx, label := range overrides {
		if idx < 0 || idx >= len(filaments) {
			return nil, nil, fmt.Errorf("slot_map names %s, but the file has only %d filament(s)", toolID(idx), len(filaments))
		}
		f := filaments[idx]
		at := -1
		for i, s := range pool {
			if strings.EqualFold(s.Label, label) {
				at = i
			}
		}
		if at < 0 {
			return nil, nil, fmt.Errorf("slot_map maps %s to %s, which is not a defined, unused CFS slot", toolID(idx), label)
		}
		if pool[at].Type != f.Type {
			return nil, nil, fmt.Errorf("slot_map maps %s (%s) to %s, which holds %s: the material types must be equal", toolID(idx), f.Type, label, pool[at].Type)
		}
		result[idx] = assignment{Filament: f, Slot: pool[at], Distance: colorDistance(f.Color, pool[at].Color)}
		used[idx] = true
		pool = append(pool[:at], pool[at+1:]...)
	}

	for len(pool) > 0 {
		var best *assignment
		bestIdx, bestPool := -1, -1
		for idx, f := range filaments {
			if f.Color == "" || used[idx] {
				continue
			}
			m := getMatchColor(f.Color, f.Type, pool)
			if best == nil && m.idx >= 0 {
				best = &assignment{Filament: f, Slot: pool[m.idx], Distance: m.dist}
				bestIdx, bestPool = idx, m.idx
			} else if best != nil && m.idx >= 0 && m.dist < best.Distance && pool[m.idx].Type == best.Slot.Type {
				best = &assignment{Filament: f, Slot: pool[m.idx], Distance: m.dist}
				bestIdx, bestPool = idx, m.idx
			}
		}
		if best == nil {
			break
		}
		used[bestIdx] = true
		result[bestIdx] = *best
		pool = append(pool[:bestPool], pool[bestPool+1:]...)
	}

	for idx := range filaments {
		if a, ok := result[idx]; ok {
			assigned = append(assigned, a)
		} else {
			unmatched = append(unmatched, idx)
		}
	}
	return assigned, unmatched, nil
}

// parseSlotMap reads the canonical override form "T1A=T1C,T1B=T1D": tool id
// (the file's filament, T1A = filament 0) = physical slot. Both sides are
// validated.
func parseSlotMap(s string) (map[int]string, error) {
	out := map[int]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("slot_map entry %q is not tool=slot (for example T1A=T1C)", part)
		}
		idx, ok := parseToolID(kv[0])
		if !ok {
			return nil, fmt.Errorf("slot_map tool %q is not T1A..T4D", kv[0])
		}
		if _, _, ok := parseSlotLabel(kv[1]); !ok {
			return nil, fmt.Errorf("slot_map slot %q is not T1A..T4D", kv[1])
		}
		if _, dup := out[idx]; dup {
			return nil, fmt.Errorf("slot_map maps %s twice", strings.ToUpper(strings.TrimSpace(kv[0])))
		}
		out[idx] = strings.ToUpper(strings.TrimSpace(kv[1]))
	}
	seen := map[string]bool{}
	for _, slot := range out {
		if seen[slot] {
			return nil, fmt.Errorf("slot_map uses slot %s for more than one filament", slot)
		}
		seen[slot] = true
	}
	return out, nil
}

// canonicalMapping renders assignments as "T1A=T1B,T1B=T1C" in filament
// order, the form bound into a proposal token.
func canonicalMapping(as []assignment) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, toolID(a.Filament.Index)+"="+a.Slot.Label)
	}
	return strings.Join(parts, ",")
}

func fmtDistance(d float64) string { return strconv.FormatFloat(d, 'f', 1, 64) }
