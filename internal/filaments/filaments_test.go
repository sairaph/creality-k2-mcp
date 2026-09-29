package filaments

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

func TestNormColorAndStatus(t *testing.T) {
	for in, want := range map[string]string{"#0ff0000": "#ff0000", "#FF0000": "#ff0000", "0ABCDEF": "#abcdef", "": "", "#12": "", "#0gggggg": ""} {
		if got := normColor(in); got != want {
			t.Errorf("normColor(%q) = %q, want %q", in, got, want)
		}
	}
	zero, one, two, nine := 0, 1, 2, 9
	for state, want := range map[*int]string{nil: StatusUnknown, &zero: StatusUndefined, &one: StatusDefined, &two: StatusRFID, &nine: StatusUnknown} {
		if got := statusOf(state); got != want {
			t.Errorf("statusOf(%v) = %s, want %s", state, got, want)
		}
	}
}

type catReader struct {
	items []crealityws.CatalogEntry
	err   error
}

func (c catReader) Materials(ctx context.Context) ([]crealityws.CatalogEntry, error) {
	return c.items, c.err
}

func TestCatalogFiltersSortsAndFlagsWritable(t *testing.T) {
	pa := 0.04
	r := catReader{items: []crealityws.CatalogEntry{
		{ID: "2", Brand: "Zed", Name: "B", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: &pa},
		{ID: "1", Brand: "Acme", Name: "A", Type: "PETG", MinTemp: 0, MaxTemp: 0, PressureAdvance: &pa},
		{ID: "3", Brand: "Acme", Name: "C", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: &pa},
	}}
	all, err := Catalog(context.Background(), r, "", "", "", nil)
	if err != nil || len(all) != 3 || all[0].ID != "1" || all[1].ID != "3" || all[2].ID != "2" {
		t.Fatalf("all = %+v %v (sorted by brand, material, name)", all, err)
	}
	if all[0].Writable || all[0].WhyNot == "" || !all[1].Writable {
		t.Fatalf("writable flags: %+v", all)
	}
	some, _ := Catalog(context.Background(), r, "acme", "pla", "", nil)
	if len(some) != 1 || some[0].ID != "3" {
		t.Fatalf("filtered = %+v", some)
	}
	if _, err := Catalog(context.Background(), catReader{err: errors.New("down")}, "", "", "", nil); err == nil {
		t.Fatal("a read error must be returned")
	}
	if _, err := Catalog(context.Background(), struct{}{}, "", "", "", nil); err == nil {
		t.Fatal("a client without the catalog read must be an error")
	}
}

func TestReadFallsBackToMoonrakerCodes(t *testing.T) {
	one := 1
	snap := printerstate.Snapshot{Box: &moonraker.Box{AutoRefill: &one, Units: map[string]moonraker.BoxUnit{
		"T1": {State: "connect", MaterialType: []string{"099001", "-1"}, ColorValue: []string{"0ff0000", "-1"}},
		"T2": {State: "None"},
	}}}
	v := Read(context.Background(), struct{}{}, snap, true, "")
	if v.NamesAvailable || v.Problem == "" || v.AutoRefill != "on" {
		t.Fatalf("view = %+v", v)
	}
	if len(v.Units) != 1 || v.Units[0].Slots[0].Color != "#ff0000" || v.Units[0].Slots[0].Editable || v.Units[0].Slots[0].Name != "" {
		t.Fatalf("units = %+v", v.Units)
	}
	if len(v.RefillGroups) != 1 || v.RefillGroups[0] != "unknown" {
		t.Fatalf("refill groups = %v", v.RefillGroups)
	}
	if Read(context.Background(), struct{}{}, printerstate.Snapshot{}, false, "").AutoRefill != "unknown" {
		t.Fatal("auto_refill must be unknown when not reported")
	}
}

// The catalog's writable flag takes the printer state and the live nozzle limit
// into account and says which (surface review M3).
func TestCatalogWritableFollowsStateAndLiveCap(t *testing.T) {
	pa := 0.04
	r := catReader{items: []crealityws.CatalogEntry{
		{ID: "1", Brand: "A", Name: "Warm", Type: "PLA", MinTemp: 250, MaxTemp: 310, PressureAdvance: &pa},
		{ID: "2", Brand: "A", Name: "Cool", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: &pa},
	}}
	cap := 300.0
	got, _ := Catalog(context.Background(), r, "", "", "", &cap)
	if got[1].Writable || got[1].WhyNot == "" || !got[0].Writable {
		t.Fatalf("with a 300 C cap: %+v", got)
	}
	got, _ = Catalog(context.Background(), r, "", "", "the printer is printing", nil)
	for _, e := range got {
		if e.Writable || !strings.Contains(e.WhyNot, "does not allow an edit right now: the printer is printing") {
			t.Fatalf("blocked state: %+v", e)
		}
	}
}

func TestEditBlockedMakesEverySlotNotEditableAndSaysWhich(t *testing.T) {
	v := Read(context.Background(), boxsWS{}, printerstate.Snapshot{}, true, "the printer is printing")
	if v.EditBlocked == "" || len(v.Units) != 1 {
		t.Fatalf("view = %+v", v)
	}
	for _, s := range v.Units[0].Slots {
		if s.Editable || !strings.Contains(s.WhyNot, "the printer does not allow an edit right now: the printer is printing") {
			t.Fatalf("slot = %+v", s)
		}
	}
	if !strings.Contains(Text(v), "Editing a slot is not possible right now: the printer is printing") {
		t.Fatalf("text:\n%s", Text(v))
	}
	open := Read(context.Background(), boxsWS{}, printerstate.Snapshot{}, true, "")
	if !open.Units[0].Slots[0].Editable {
		t.Fatalf("slot = %+v", open.Units[0].Slots[0])
	}
}

type boxsWS struct{}

func (boxsWS) BoxsInfo(ctx context.Context) (crealityws.BoxsInfo, error) {
	one, zero := 1, 0
	return crealityws.BoxsInfo{MaterialBoxs: []crealityws.MaterialBox{{ID: 1, Type: 0, State: 1, Materials: []crealityws.SlotMaterial{
		{ID: 0, Type: "PLA", Name: "N", RFID: "1", Color: "#0ff0000", State: &one, Selected: &zero, EditStatus: &one}}}}, SameMaterialOK: true}, nil
}
