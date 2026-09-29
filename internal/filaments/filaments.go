// Package filaments builds the read-only filament views shared by the
// get_filaments and list_filament_catalog MCP tools and the "filaments" CLI
// command (dev_docs/plan-v0.2.0.md sections 4 and 5), so the three surfaces
// can never disagree about what a slot holds or whether it can be edited.
//
// Names (brand, material name, catalog) come only from the printer's own port
// 9999 reads (plan decision V1). When 9999 cannot be read, the view falls back
// to Moonraker's material codes and colours and says so plainly.
package filaments

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/policy"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// PresenceNote is shown with every slot listing: the status is the printer's
// stored definition, not a sensor reading.
const PresenceNote = "A slot's status describes the definition stored on the printer (defined = a definition is stored and it is not " +
	"RFID, rfid = an RFID-tagged spool, undefined = no definition). Whether filament is physically loaded in a slot cannot be detected, so " +
	"ask the user which spool is in which slot."

// Slot status names.
const (
	StatusDefined   = "defined"
	StatusRFID      = "rfid"
	StatusUndefined = "undefined"
	StatusUnknown   = "unknown"
)

// Slot is one filament slot (a CFS slot or the side spool).
type Slot struct {
	Slot          string   `yaml:"slot" json:"slot"`
	Status        string   `yaml:"status" json:"status"`
	Brand         string   `yaml:"brand,omitempty" json:"brand,omitempty"`
	Name          string   `yaml:"name,omitempty" json:"name,omitempty"`
	Material      string   `yaml:"material,omitempty" json:"material,omitempty"`
	Color         string   `yaml:"color,omitempty" json:"color,omitempty"`
	CatalogID     string   `yaml:"catalog_id,omitempty" json:"catalog_id,omitempty"`
	NozzleMinC    *float64 `yaml:"nozzle_min_c,omitempty" json:"nozzle_min_c,omitempty"`
	NozzleMaxC    *float64 `yaml:"nozzle_max_c,omitempty" json:"nozzle_max_c,omitempty"`
	SelectedOnHub *bool    `yaml:"selected_on_hub,omitempty" json:"selected_on_hub,omitempty"`
	Editable      bool     `yaml:"editable" json:"editable"`
	WhyNot        string   `yaml:"why_not,omitempty" json:"why_not,omitempty"`

	// MoonrakerMaterialType and MoonrakerColor are Moonraker's own codes for
	// the slot ("0" plus the catalog id, and "0" plus rrggbb); they are the
	// only data available when port 9999 is unreachable.
	MoonrakerMaterialType string `yaml:"moonraker_material_type,omitempty" json:"moonraker_material_type,omitempty"`
	MoonrakerColor        string `yaml:"moonraker_color,omitempty" json:"moonraker_color,omitempty"`
}

// Unit is one CFS unit.
type Unit struct {
	Unit            string   `yaml:"unit" json:"unit"`
	Model           string   `yaml:"model,omitempty" json:"model,omitempty"`
	TemperatureC    *float64 `yaml:"temperature_c,omitempty" json:"temperature_c,omitempty"`
	HumidityPercent *float64 `yaml:"humidity_percent,omitempty" json:"humidity_percent,omitempty"`
	Slots           []Slot   `yaml:"slots" json:"slots"`
}

// View is get_filaments's frontmatter (the cfs state block and cfs_connected
// come from the shared state block the caller embeds).
type View struct {
	CFSConnected bool `yaml:"-" json:"cfs_connected"`
	// AutoRefill is Moonraker's box.auto_refill: on, off or unknown.
	AutoRefill string `yaml:"auto_refill" json:"auto_refill"`
	// NamesAvailable is false when port 9999 could not be read: only
	// Moonraker's codes are shown and names are missing.
	NamesAvailable bool   `yaml:"names_available" json:"names_available"`
	Units          []Unit `yaml:"units,omitempty" json:"units,omitempty"`
	SideSpool      *Slot  `yaml:"side_spool,omitempty" json:"side_spool,omitempty"`
	// RefillGroups is the printer's own same_material regrouping (slots it
	// treats as interchangeable for auto-refill), or the single entry
	// "unknown" when it could not be decoded.
	RefillGroups []string `yaml:"refill_groups,omitempty" json:"refill_groups,omitempty"`
	Note         string   `yaml:"note" json:"note"`
	// Problem says plainly what could not be read, empty when everything was.
	Problem string `yaml:"problem,omitempty" json:"problem,omitempty"`
	// EditBlocked is why set_filament_definition cannot be used right now for
	// ANY slot (the printer is not idle, the CFS is not at rest, a start is in
	// flight, control is off), empty when the printer allows an edit. Every slot
	// then shows editable false with this reason; per-slot reasons are separate.
	EditBlocked string `yaml:"edit_blocked,omitempty" json:"edit_blocked,omitempty"`
}

// EditGate returns why set_filament_definition is not available right now for
// this printer and state, or "" when it is: the action's own gate (bucket, CFS
// rule, allow_control), taken from the policy so get_filaments, the CLI and the
// actions list cannot disagree with Execute. derived should come from
// Policy.Derive where a policy engine exists.
func EditGate(printer domain.Printer, derived printerstate.Derived, settings domain.Settings) string {
	for _, g := range policy.GatesFor(printer, derived, settings) {
		if g.Name == string(policy.ActionSetFilamentDefinition) && g.Status == "blocked" {
			return g.Reason
		}
	}
	return ""
}

// BoxsReader is the 9999 read Read needs; *crealityws.Client satisfies it and
// a printerstate.WS9999Client that does not is treated as "9999 unreachable".
type BoxsReader interface {
	BoxsInfo(ctx context.Context) (crealityws.BoxsInfo, error)
}

// CatalogReader is the 9999 read Catalog needs.
type CatalogReader interface {
	Materials(ctx context.Context) ([]crealityws.CatalogEntry, error)
}

// normColor turns the printer's "#0rrggbb" (or "#rrggbb") into "#rrggbb", or
// "" when it is not a colour.
func normColor(s string) string {
	s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if len(s) == 7 && s[0] == '0' {
		s = s[1:]
	}
	if len(s) != 6 {
		return ""
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return "#" + s
}

func statusOf(state *int) string {
	if state == nil {
		return StatusUnknown
	}
	switch *state {
	case 0:
		return StatusUndefined
	case 1:
		return StatusDefined
	case 2:
		return StatusRFID
	}
	return StatusUnknown
}

func autoRefill(box *moonraker.Box) string {
	if box == nil || box.AutoRefill == nil {
		return "unknown"
	}
	switch *box.AutoRefill {
	case 0:
		return "off"
	case 1:
		return "on"
	}
	return strconv.Itoa(*box.AutoRefill)
}

func slotName(box, slot int) string { return fmt.Sprintf("T%d%c", box, 'A'+rune(slot)) }

// Read builds the view from a snapshot (Moonraker's box and filament_rack,
// port 9999 reachability) and a fresh boxsInfo read through ws when it can
// provide one.
func Read(ctx context.Context, ws any, snap printerstate.Snapshot, cfsConnected bool, editBlocked string) View {
	v := View{CFSConnected: cfsConnected, AutoRefill: autoRefill(snap.Box), Note: PresenceNote, EditBlocked: editBlocked}

	var boxs crealityws.BoxsInfo
	var readErr error
	if r, ok := ws.(BoxsReader); ok {
		boxs, readErr = r.BoxsInfo(ctx)
	} else {
		readErr = fmt.Errorf("this printer client cannot read port 9999 filament data")
	}
	if readErr != nil {
		return fallback(v, snap, readErr)
	}

	v.NamesAvailable = true
	for _, box := range boxs.MaterialBoxs {
		switch box.Type {
		case 0:
			if box.State != 1 {
				continue
			}
			u := Unit{Unit: fmt.Sprintf("T%d", box.ID), Model: box.Name, TemperatureC: box.Temp, HumidityPercent: box.Humidity}
			for _, m := range box.Materials {
				u.Slots = append(u.Slots, slotFrom(boxs, slotName(box.ID, m.ID), m, snap, editBlocked))
			}
			v.Units = append(v.Units, u)
		case 1:
			if len(box.Materials) > 0 {
				s := slotFrom(boxs, "side_spool", box.Materials[0], snap, editBlocked)
				v.SideSpool = &s
			}
		}
	}
	sort.Slice(v.Units, func(i, j int) bool { return v.Units[i].Unit < v.Units[j].Unit })
	if !boxs.SameMaterialOK {
		v.RefillGroups = []string{"unknown"}
	} else {
		for _, g := range boxs.SameMaterial {
			names := make([]string, len(g.Slots))
			for i, s := range g.Slots {
				names[i] = slotName(s.BoxID, s.MaterialID)
			}
			v.RefillGroups = append(v.RefillGroups, fmt.Sprintf("%s (%s %s)", strings.Join(names, "+"), g.Name, normColor(g.Color)))
		}
	}
	return v
}

func slotFrom(boxs crealityws.BoxsInfo, label string, m crealityws.SlotMaterial, snap printerstate.Snapshot, editBlocked string) Slot {
	s := Slot{
		Slot: label, Status: statusOf(m.State), Brand: m.Vendor, Name: m.Name, Material: m.Type,
		Color: normColor(m.Color), CatalogID: m.RFID, NozzleMinC: m.MinTemp, NozzleMaxC: m.MaxTemp,
	}
	if m.Selected != nil {
		sel := *m.Selected == 1
		s.SelectedOnHub = &sel
	}
	s.Editable, s.WhyNot = policy.SlotEditability(boxs, label)
	if editBlocked != "" {
		// The printer's current state forbids any edit; say which, and keep the
		// slot's own reason too when it has one.
		why := "the printer does not allow an edit right now: " + editBlocked
		if s.WhyNot != "" {
			why += "; also this slot: " + s.WhyNot
		}
		s.Editable, s.WhyNot = false, why
	}
	s.MoonrakerMaterialType, s.MoonrakerColor = moonrakerCodes(snap, label)
	return s
}

// moonrakerCodes reads Moonraker's material_type and color_value for a slot
// name (T1A..) or side_spool.
func moonrakerCodes(snap printerstate.Snapshot, label string) (string, string) {
	if label == "side_spool" {
		r := snap.FilamentRack
		if r == nil {
			return "", ""
		}
		mt, cv := "", ""
		if r.MaterialType != nil {
			mt = *r.MaterialType
		}
		if r.ColorValue != nil {
			cv = *r.ColorValue
		}
		return mt, cv
	}
	if snap.Box == nil || len(label) != 3 {
		return "", ""
	}
	u, ok := snap.Box.Units[label[:2]]
	i := int(label[2] - 'A')
	if !ok || i < 0 || i >= len(u.MaterialType) || i >= len(u.ColorValue) {
		return "", ""
	}
	return u.MaterialType[i], u.ColorValue[i]
}

// fallback builds the view from Moonraker alone when port 9999 cannot be
// read, saying so plainly: material codes and colours only, no names, and
// nothing is reported as editable.
func fallback(v View, snap printerstate.Snapshot, err error) View {
	v.NamesAvailable = false
	v.Problem = "port 9999 could not be read (" + err.Error() + "): only Moonraker's material codes and colours are shown, " +
		"brand and material names are missing, and no slot can be reported as editable"
	why := "port 9999 is unreachable, so this slot's definition and editability are unknown"
	if snap.Box != nil {
		keys := make([]string, 0, len(snap.Box.Units))
		for k := range snap.Box.Units {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			u := snap.Box.Units[k]
			if u.State != "connect" {
				continue
			}
			unit := Unit{Unit: k}
			for i := 0; i < len(u.MaterialType) && i < len(u.ColorValue) && i < 4; i++ {
				unit.Slots = append(unit.Slots, Slot{
					Slot: fmt.Sprintf("%s%c", k, 'A'+rune(i)), Status: StatusUnknown, Color: normColor(u.ColorValue[i]),
					MoonrakerMaterialType: u.MaterialType[i], MoonrakerColor: u.ColorValue[i], WhyNot: why,
				})
			}
			v.Units = append(v.Units, unit)
		}
		if snap.Box.SameMaterialOK {
			for _, g := range snap.Box.SameMaterial {
				v.RefillGroups = append(v.RefillGroups, fmt.Sprintf("%s (%s %s)", strings.Join(g.Slots, "+"), g.Name, normColor(g.Color)))
			}
		} else {
			v.RefillGroups = []string{"unknown"}
		}
	} else {
		v.RefillGroups = []string{"unknown"}
	}
	if r := snap.FilamentRack; r != nil && (r.MaterialType != nil || r.ColorValue != nil) {
		mt, cv := moonrakerCodes(snap, "side_spool")
		v.SideSpool = &Slot{Slot: "side_spool", Status: StatusUnknown, Color: normColor(cv), MoonrakerMaterialType: mt, MoonrakerColor: cv, WhyNot: why}
	}
	return v
}

// Text renders the view as plain text, for the CLI and for tool bodies.
func Text(v View) string {
	var b strings.Builder
	if !v.CFSConnected {
		b.WriteString("No CFS is connected.\n")
	}
	fmt.Fprintf(&b, "Auto-refill: %s\n", v.AutoRefill)
	if v.Problem != "" {
		b.WriteString("Problem: " + v.Problem + "\n")
	}
	if v.EditBlocked != "" {
		b.WriteString("Editing a slot is not possible right now: " + v.EditBlocked + "\n")
	}
	writeSlot := func(s Slot) {
		desc := s.Status
		if s.Name != "" {
			desc += ", " + policy.DisplayName(s.Brand, s.Name)
		}
		if s.Material != "" {
			desc += ", " + s.Material
		}
		if s.Color != "" {
			desc += ", " + s.Color
		}
		if s.MoonrakerMaterialType != "" && s.Name == "" {
			desc += ", moonraker code " + s.MoonrakerMaterialType
		}
		if s.NozzleMinC != nil && s.NozzleMaxC != nil {
			desc += fmt.Sprintf(", nozzle %g-%g C", *s.NozzleMinC, *s.NozzleMaxC)
		}
		if s.SelectedOnHub != nil && *s.SelectedOnHub {
			desc += ", selected at the hub"
		}
		edit := "editable"
		if !s.Editable {
			edit = "not editable: " + s.WhyNot
		}
		fmt.Fprintf(&b, "  %-11s %s (%s)\n", s.Slot, desc, edit)
	}
	for _, u := range v.Units {
		head := "Unit " + u.Unit
		if u.Model != "" {
			head += " (" + u.Model + ")"
		}
		if u.TemperatureC != nil {
			head += fmt.Sprintf(", %.0f C", *u.TemperatureC)
		}
		if u.HumidityPercent != nil {
			head += fmt.Sprintf(", %.0f%% humidity", *u.HumidityPercent)
		}
		b.WriteString(head + "\n")
		for _, s := range u.Slots {
			writeSlot(s)
		}
	}
	if v.SideSpool != nil {
		b.WriteString("Side spool\n")
		writeSlot(*v.SideSpool)
	}
	if len(v.RefillGroups) > 0 {
		b.WriteString("Refill groups (slots the printer treats as interchangeable): " + strings.Join(v.RefillGroups, "; ") + "\n")
	}
	b.WriteString(v.Note + "\n")
	return b.String()
}

// CatalogEntry is one entry of the printer's own filament catalog.
type CatalogEntry struct {
	ID         string  `yaml:"id" json:"id"`
	Brand      string  `yaml:"brand" json:"brand"`
	Name       string  `yaml:"name" json:"name"`
	Material   string  `yaml:"material" json:"material"`
	NozzleMinC float64 `yaml:"nozzle_min_c" json:"nozzle_min_c"`
	NozzleMaxC float64 `yaml:"nozzle_max_c" json:"nozzle_max_c"`
	Writable   bool    `yaml:"writable" json:"writable"`
	WhyNot     string  `yaml:"why_not,omitempty" json:"why_not,omitempty"`
}

// ifWhy renders an additional reason after the state reason.
func ifWhy(why string) string {
	if why == "" {
		return ""
	}
	return "; also this entry: " + why
}

// Catalog reads the printer's catalog and filters it by brand and material
// (case-insensitive exact match; an empty filter matches everything). writable
// takes the printer's current state (editBlocked, from EditGate) and the live
// nozzle limit (nozzleCapC, nil when unknown) into account, so it does not
// promise an edit Execute would refuse for a reason it can already see.
func Catalog(ctx context.Context, ws any, brand, material, editBlocked string, nozzleCapC *float64) ([]CatalogEntry, error) {
	r, ok := ws.(CatalogReader)
	if !ok {
		return nil, fmt.Errorf("this printer client cannot read the port 9999 filament catalog")
	}
	items, err := r.Materials(ctx)
	if err != nil {
		return nil, err
	}
	brand, material = strings.TrimSpace(brand), strings.TrimSpace(material)
	out := make([]CatalogEntry, 0, len(items))
	for _, e := range items {
		if brand != "" && !strings.EqualFold(e.Brand, brand) {
			continue
		}
		if material != "" && !strings.EqualFold(e.Type, material) {
			continue
		}
		w, why := policy.CatalogWritable(e)
		if w && nozzleCapC != nil && e.MaxTemp > *nozzleCapC {
			w, why = false, fmt.Sprintf("its maximum of %g C is above this printer's nozzle limit of %g C", e.MaxTemp, *nozzleCapC)
		}
		if editBlocked != "" {
			why = "the printer does not allow an edit right now: " + editBlocked + ifWhy(why)
			w = false
		}
		out = append(out, CatalogEntry{ID: e.ID, Brand: e.Brand, Name: e.Name, Material: e.Type,
			NozzleMinC: e.MinTemp, NozzleMaxC: e.MaxTemp, Writable: w, WhyNot: why})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Brand != out[j].Brand {
			return out[i].Brand < out[j].Brand
		}
		if out[i].Material != out[j].Material {
			return out[i].Material < out[j].Material
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
