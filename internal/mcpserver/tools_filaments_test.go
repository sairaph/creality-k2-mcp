package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/policy"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// End-to-end tests of the filament tool group through the real MCP protocol
// against in-memory fakes (no sockets, AGENTS.md hard testing rule). Fixtures
// use invented catalog ids and names (plan decision V1).

type cfsWorld struct {
	mu        sync.Mutex
	connected bool // CFS connected (box.state connect and 9999 cfsConnect 1)
	wsDown    bool
	boxs      crealityws.BoxsInfo
	catalog   []crealityws.CatalogEntry
	files     []crealityws.GcodeFileInfo
	boxMap    map[string]string
	frames    []string
	state     int // 9999 state: 1 after a start frame, 7 after a stop frame

	// knobs for the failure outcomes
	forceMap  map[string]string // the printer writes this map instead of the requested one
	notSent   bool              // the start frame definitely cannot be sent
	ambiguous bool              // writing the start frame errors (it may have been delivered)
	frozen    map[string]any    // Moonraker's stale view of unit T1 (slot edits do not reach it)
}

func ip(v int) *int { return &v }

func newCFSWorld() *cfsWorld {
	slot := func(id int, typ, name, rfid, color string) crealityws.SlotMaterial {
		mn, mx, pr := 190.0, 240.0, 0.04
		return crealityws.SlotMaterial{ID: id, Vendor: "Acme", Type: typ, Name: name, RFID: rfid, Color: color, MinTemp: &mn, MaxTemp: &mx,
			Pressure: &pr, State: ip(1), Selected: ip(0), EditStatus: ip(1)}
	}
	w := &cfsWorld{connected: true, boxMap: map[string]string{}}
	for u := 1; u <= 4; u++ {
		for l := 'A'; l <= 'D'; l++ {
			k := fmt.Sprintf("T%d%c", u, l)
			w.boxMap[k] = k
		}
	}
	w.boxs = crealityws.BoxsInfo{MaterialBoxs: []crealityws.MaterialBox{
		{ID: 0, Type: 1, Materials: []crealityws.SlotMaterial{slot(0, "PLA", "Acme Side PLA", "99003", "#0ffffff")}},
		{ID: 1, Type: 0, State: 1, Name: "TESTBOX", Temp: fp(29), Humidity: fp(35), Materials: []crealityws.SlotMaterial{
			slot(0, "PLA", "Acme Test PLA", "99001", "#0ff0000"),
			slot(1, "PETG", "Acme Test PETG", "99002", "#0000000"),
			slot(2, "PETG", "Acme Test PETG", "99002", "#0000000"),
			slot(3, "PETG", "Acme Test PETG", "99002", "#0ffffff"),
		}},
	}}
	w.regroup()
	pa := fp(0.04)
	w.catalog = []crealityws.CatalogEntry{
		{ID: "99001", Brand: "Acme", Name: "Acme Test PLA", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: pa},
		{ID: "99002", Brand: "Acme", Name: "Acme Test PETG", Type: "PETG", MinTemp: 220, MaxTemp: 270, PressureAdvance: pa},
		{ID: "99005", Brand: "Other", Name: "Other Zero", Type: "PLA", MinTemp: 0, MaxTemp: 0, PressureAdvance: pa},
	}
	w.files = []crealityws.GcodeFileInfo{{Name: "three.gcode", Path: "/mnt/UDISK/printer_data/gcodes/three.gcode",
		Material: "PLA;PETG;PETG", MaterialColors: "#FF0000;#000000;#FFFFFF", Size: 10, CreateTime: 20}}
	return w
}

func fp(v float64) *float64 { return &v }

func (w *cfsWorld) regroup() {
	type key struct{ code, color string }
	var order []key
	groups := map[key]*crealityws.SameGroup{}
	for _, box := range w.boxs.MaterialBoxs {
		if box.Type != 0 {
			continue
		}
		for _, m := range box.Materials {
			k := key{"0" + m.RFID, strings.TrimPrefix(m.Color, "#")}
			g, ok := groups[k]
			if !ok {
				g = &crealityws.SameGroup{Code: k.code, Color: k.color, Name: m.Type}
				groups[k] = g
				order = append(order, k)
			}
			g.Slots = append(g.Slots, crealityws.SlotRef{BoxID: box.ID, MaterialID: m.ID})
		}
	}
	w.boxs.SameMaterial = nil
	for _, k := range order {
		w.boxs.SameMaterial = append(w.boxs.SameMaterial, *groups[k])
	}
	w.boxs.SameMaterialOK = true
}

func (w *cfsWorld) unit() map[string]any {
	if w.frozen != nil {
		return w.frozen
	}
	types, colors := make([]any, 4), make([]any, 4)
	for _, b := range w.boxs.MaterialBoxs {
		if b.ID == 1 {
			for i, m := range b.Materials {
				types[i], colors[i] = "0"+m.RFID, strings.TrimPrefix(m.Color, "#")
			}
		}
	}
	return map[string]any{"state": "connect", "material_type": types, "color_value": colors}
}

// cfsMoon wraps the control fake's Moonraker client, replacing the box and
// pause_resume objects with a fully reported CFS.
type cfsMoon struct {
	*fakeMoonrakerControl
	w *cfsWorld
}

func (m *cfsMoon) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	out, err := m.fakeMoonrakerControl.QueryObjects(ctx, objects)
	if err != nil {
		return nil, err
	}
	m.w.mu.Lock()
	defer m.w.mu.Unlock()
	state := "disconnect"
	if m.w.connected {
		state = "connect"
	}
	box := map[string]any{"state": state, "enable": 1, "filament_useup": 0, "auto_refill": 1, "map": m.w.boxMap, "T1": m.w.unit()}
	if !m.w.connected {
		delete(box, "T1")
	}
	b, _ := json.Marshal(box)
	out["box"] = b
	for _, bx := range m.w.boxs.MaterialBoxs {
		if bx.Type == 1 && len(bx.Materials) > 0 {
			rack, _ := json.Marshal(map[string]any{"material_type": "0" + bx.Materials[0].RFID, "color_value": strings.TrimPrefix(bx.Materials[0].Color, "#")})
			out["filament_rack"] = rack
		}
	}
	var prm map[string]any
	_ = json.Unmarshal(out["pause_resume"], &prm)
	if prm == nil {
		prm = map[string]any{}
	}
	prm["resume_err"] = false
	pr, _ := json.Marshal(prm)
	out["pause_resume"] = pr
	return out, nil
}

type cfsWS struct {
	*fakeWS9999Control
	w *cfsWorld
}

func (c *cfsWS) ReadStatus(ctx context.Context) (crealityws.Status, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if c.w.wsDown {
		return crealityws.Status{}, fmt.Errorf("port 9999 unreachable")
	}
	p := func(v int) crealityws.Int { return crealityws.Int{Value: v, Present: true} }
	state := c.w.state
	cfs := 0
	if c.w.connected {
		cfs = 1
	}
	return crealityws.Status{State: p(state), DeviceState: p(0), FeedState: p(0), UpgradeStatus: p(0), RepoPlrStatus: p(0),
		MaterialStatus: p(0), CfsConnect: p(cfs), Err: crealityws.StatusErr{Present: true}, WithSelfTest: p(100)}, nil
}

func (c *cfsWS) BoxsInfo(ctx context.Context) (crealityws.BoxsInfo, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if c.w.wsDown {
		return crealityws.BoxsInfo{}, fmt.Errorf("port 9999 unreachable")
	}
	out := crealityws.BoxsInfo{SameMaterialOK: c.w.boxs.SameMaterialOK, SameMaterial: append([]crealityws.SameGroup(nil), c.w.boxs.SameMaterial...)}
	for _, bx := range c.w.boxs.MaterialBoxs {
		bx.Materials = append([]crealityws.SlotMaterial(nil), bx.Materials...)
		out.MaterialBoxs = append(out.MaterialBoxs, bx)
	}
	return out, nil
}

func (c *cfsWS) Materials(ctx context.Context) ([]crealityws.CatalogEntry, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if c.w.wsDown {
		return nil, fmt.Errorf("port 9999 unreachable")
	}
	return append([]crealityws.CatalogEntry(nil), c.w.catalog...), nil
}

func (c *cfsWS) GcodeFiles(ctx context.Context) ([]crealityws.GcodeFileInfo, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	return append([]crealityws.GcodeFileInfo(nil), c.w.files...), nil
}

func (c *cfsWS) ModifyMaterial(ctx context.Context, e crealityws.MaterialEdit) (*crealityws.BoxsInfo, bool, error) {
	c.w.mu.Lock()
	c.w.frames = append(c.w.frames, "modifyMaterial")
	for bi := range c.w.boxs.MaterialBoxs {
		bx := &c.w.boxs.MaterialBoxs[bi]
		if bx.ID != e.BoxID || bx.Type != e.BoxType {
			continue
		}
		for mi := range bx.Materials {
			m := &bx.Materials[mi]
			if m.ID == e.SlotID {
				m.RFID, m.Vendor, m.Type, m.Name = e.Entry.ID, e.Entry.Brand, e.Entry.Type, e.Entry.Name
				m.Color = "#0" + strings.TrimPrefix(e.Color, "#")
			}
		}
	}
	c.w.regroup()
	c.w.mu.Unlock()
	b, err := c.BoxsInfo(ctx)
	return &b, err == nil, err
}

func (c *cfsWS) StartCFSPrint(ctx context.Context, path string, items []crealityws.ColorMatchItem, selfTest bool, verify func(ctx context.Context) error) (bool, error) {
	c.w.mu.Lock()
	c.w.frames = append(c.w.frames, "colorMatch")
	for _, it := range items {
		c.w.boxMap[it.ID] = fmt.Sprintf("T%d%c", it.BoxID, 'A'+it.MaterialID)
	}
	for k, v := range c.w.forceMap {
		c.w.boxMap[k] = v
	}
	c.w.mu.Unlock()
	if err := verify(ctx); err != nil {
		return false, err
	}
	c.w.mu.Lock()
	if c.w.notSent {
		c.w.mu.Unlock()
		return false, fmt.Errorf("port 9999 closed")
	}
	if c.w.ambiguous {
		c.w.mu.Unlock()
		return false, &crealityws.StartFrameError{Err: fmt.Errorf("write timed out")}
	}
	c.w.frames = append(c.w.frames, "multiColorPrint")
	c.w.state = 1
	c.w.mu.Unlock()
	return true, nil
}

func cfsSetup(t *testing.T, connected bool) (*mcp.ClientSession, *cfsWorld, *fakeControlState) {
	t.Helper()
	w := newCFSWorld()
	w.connected = connected
	deps, st := controlDeps(t, true, nil)
	moon := &cfsMoon{fakeMoonrakerControl: &fakeMoonrakerControl{st: st}, w: w}
	ws := &cfsWS{fakeWS9999Control: &fakeWS9999Control{st: st}, w: w}
	deps.PrinterClients = func(p domain.Printer) printerstate.Deps { return printerstate.Deps{Moonraker: moon, WS9999: ws} }
	st.files = []string{"three.gcode"}
	return controlSession(t, deps), w, st
}

func replyText(res *mcp.CallToolResult) string { return strings.Join(texts(res), "\n") }

// --- get_filaments ---

func TestGetFilaments_ShowsSlotsEditabilityAndRefillGroups(t *testing.T) {
	cs, _, _ := cfsSetup(t, true)
	res := call(t, cs, "get_filaments", nil)
	text := replyText(res)
	if res.IsError {
		t.Fatalf("get_filaments failed: %s", text)
	}
	for _, want := range []string{
		"cfs_connected: true", "auto_refill: \"on\"", "names_available: true", "units:", "unit: T1", "model: TESTBOX",
		"slot: T1A", "status: defined", "brand: Acme", "name: Acme Test PLA", "material: PLA", "color: '#ff0000'", "catalog_id: \"99001\"",
		"nozzle_min_c: 190", "selected_on_hub: false", "editable: true", "side_spool:", "slot: side_spool",
		"refill_groups:", "T1B+T1C (PETG #000000)", "cfs:", "state: idle",
		"cannot be detected", "ask the user which spool",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("get_filaments reply missing %q:\n%s", want, text)
		}
	}
	// The side spool edit was verified on hardware (2026-09-29): editable like a slot.
	if strings.Contains(text, "not yet verified on this firmware") {
		t.Errorf("the side spool is still reported as unverified:\n%s", text)
	}
}

func TestGetFilaments_WhyNotComesFromThePoliciesSlotChecks(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	w.mu.Lock()
	w.boxs.MaterialBoxs[1].Materials[0].State = ip(2)
	w.boxs.MaterialBoxs[1].Materials[1].Selected = ip(1)
	w.boxs.MaterialBoxs[1].Materials[2].EditStatus = ip(0)
	w.boxs.MaterialBoxs[1].Materials[3].State = ip(0)
	w.mu.Unlock()
	text := replyText(call(t, cs, "get_filaments", nil))
	for _, want := range []string{"status: rfid", "RFID-managed", "selected at the filament hub", "being written", "status: undefined", "no definition or is not ready"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
}

func TestGetFilaments_NinetyNineNinetyNineDownFallsBackToMoonrakerCodes(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	w.mu.Lock()
	w.wsDown = true
	w.mu.Unlock()
	res := call(t, cs, "get_filaments", nil)
	text := replyText(res)
	if res.IsError {
		t.Fatalf("get_filaments must not fail when 9999 is down: %s", text)
	}
	for _, want := range []string{"names_available: false", "problem:", "port 9999 could not be read", "brand and material names are missing",
		"moonraker_material_type: \"099001\"", "status: unknown", "editable: false", "cfs:", "state: unknown"} {
		if !strings.Contains(text, want) {
			t.Errorf("fallback reply missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Acme") {
		t.Errorf("names must not appear without 9999:\n%s", text)
	}
}

func TestGetFilaments_NoCFS(t *testing.T) {
	cs, _, _ := cfsSetup(t, false)
	text := replyText(call(t, cs, "get_filaments", nil))
	if !strings.Contains(text, "cfs_connected: false") || strings.Contains(text, "cfs:\n") {
		t.Errorf("no-CFS reply:\n%s", text)
	}
}

// --- list_filament_catalog ---

func TestListFilamentCatalog_FiltersAndWritable(t *testing.T) {
	cs, _, _ := cfsSetup(t, true)
	text := replyText(call(t, cs, "list_filament_catalog", nil))
	for _, want := range []string{"count: 3", "id: \"99001\"", "brand: Acme", "material: PETG", "writable: true", "writable: false", "no nozzle temperature range"} {
		if !strings.Contains(text, want) {
			t.Errorf("catalog reply missing %q:\n%s", want, text)
		}
	}
	text = replyText(call(t, cs, "list_filament_catalog", map[string]any{"brand": "ACME", "material": "petg"}))
	if !strings.Contains(text, "count: 1") || strings.Contains(text, "99001") || !strings.Contains(text, "99002") {
		t.Errorf("filtered catalog:\n%s", text)
	}
}

func TestListFilamentCatalog_UnreachableIsAnError(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	w.mu.Lock()
	w.wsDown = true
	w.mu.Unlock()
	res := call(t, cs, "list_filament_catalog", nil)
	text := replyText(res)
	if !res.IsError || !strings.Contains(text, "code: unavailable") || !strings.Contains(text, "no built-in catalog") {
		t.Fatalf("reply = %s", text)
	}
}

// --- set_filament_definition ---

func TestSetFilamentDefinition_ThroughTheTool(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	res := call(t, cs, "set_filament_definition", map[string]any{"slot": "t1a", "material": "Acme Test PETG", "color": "000000"})
	text := replyText(res)
	if res.IsError {
		t.Fatalf("set_filament_definition failed: %s", text)
	}
	for _, want := range []string{"action: set_filament_definition", "effect: confirmed", "filament:", "slot: T1A", "before:", "after:", "rfid: \"99001\"", "rfid: \"99002\"",
		"refill_groups_before:", "refill_groups_after:", "T1A+T1B+T1C", "Refill groups before:", "moonraker_material_type: \"099002\""} {
		if !strings.Contains(text, want) {
			t.Errorf("reply missing %q:\n%s", want, text)
		}
	}
	w.mu.Lock()
	got := w.boxs.MaterialBoxs[1].Materials[0].RFID
	w.mu.Unlock()
	if got != "99002" {
		t.Fatalf("slot rfid = %s", got)
	}

	// The same edit again is a no_change with nothing sent.
	w.mu.Lock()
	w.frames = nil
	w.mu.Unlock()
	res = call(t, cs, "set_filament_definition", map[string]any{"slot": "T1A", "material": "99002", "color": "#000000"})
	text = replyText(res)
	if res.IsError || !strings.Contains(text, "effect: no_change") || !strings.Contains(text, "nothing was sent") {
		t.Fatalf("no_change reply: %s", text)
	}
	if len(w.frames) != 0 {
		t.Fatalf("frames sent for a no_change: %v", w.frames)
	}
}

func TestSetFilamentDefinition_RefusalsCarryAHint(t *testing.T) {
	cs, _, _ := cfsSetup(t, true)
	res := call(t, cs, "set_filament_definition", map[string]any{"slot": "T9Z", "material": "99001", "color": "#ffffff"})
	text := replyText(res)
	if !res.IsError || !strings.Contains(text, "code: invalid_input") || !strings.Contains(text, "T1A..T4D or side_spool") {
		t.Fatalf("reply = %s", text)
	}
	res = call(t, cs, "set_filament_definition", map[string]any{"slot": "side_spool", "material": "99001", "color": "#ffffff"})
	if text = replyText(res); res.IsError || !strings.Contains(text, "effect: confirmed") {
		t.Fatalf("side spool edit reply = %s", text)
	}
}

// --- start_print with a CFS ---

func TestStartPrintCFS_ProposalShowsTheMappingThenSends(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	prop := call(t, cs, "start_print", map[string]any{"filename": "three.gcode"})
	text := replyText(prop)
	if prop.IsError {
		t.Fatalf("proposal failed: %s", text)
	}
	for _, want := range []string{"proposed: true", "mapping:", "tool: T1A", "slot: T1A", "file_type: PETG", "color_distance:", "refill_group:",
		"| filament | tool |", "| 1 | T1B |", "show the user this mapping including every warning", "SAME filename, source, slot_map and self_test",
		"slots cannot be checked for physical filament", "the bed is clear"} {
		if !strings.Contains(text, want) {
			t.Errorf("proposal missing %q:\n%s", want, text)
		}
	}
	if len(w.frames) != 0 {
		t.Fatalf("a proposal sent frames: %v", w.frames)
	}
	token := extractYAMLValue(t, text, "confirm_token")

	res := call(t, cs, "start_print", map[string]any{"filename": "three.gcode", "confirm_token": token})
	text = replyText(res)
	if res.IsError {
		t.Fatalf("start failed: %s", text)
	}
	for _, want := range []string{"effect: sent", "not printing yet", "self-test for several minutes", "Filament to slot mapping used"} {
		if !strings.Contains(text, want) {
			t.Errorf("start reply missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "effect: started") || strings.Contains(text, "effect: confirmed") {
		t.Errorf("a CFS start must never claim started or confirmed:\n%s", text)
	}
	if strings.Join(w.frames, ",") != "colorMatch,multiColorPrint" {
		t.Fatalf("frames = %v", w.frames)
	}
}

func TestStartPrintCFS_SlotMapAndSelfTestAreBoundByTheToken(t *testing.T) {
	cs, _, _ := cfsSetup(t, true)
	args := map[string]any{"filename": "three.gcode", "slot_map": []any{map[string]any{"filament": 1, "slot": "t1c"}}, "self_test": true}
	prop := call(t, cs, "start_print", args)
	text := replyText(prop)
	if prop.IsError || !strings.Contains(text, "slot: T1C") || !strings.Contains(text, "the printer may use T1B instead of T1C: both must hold this spool") ||
		!strings.Contains(text, "self-test before printing: true") {
		t.Fatalf("proposal:\n%s", text)
	}
	token := extractYAMLValue(t, text, "confirm_token")

	// A different self_test invalidates the token.
	bad := call(t, cs, "start_print", map[string]any{"filename": "three.gcode", "slot_map": args["slot_map"], "self_test": false, "confirm_token": token})
	if !bad.IsError || !strings.Contains(replyText(bad), "code: conflict") {
		t.Fatalf("changed self_test reply = %s", replyText(bad))
	}
	// The token was consumed by the failed attempt: a fresh proposal is needed.
	prop = call(t, cs, "start_print", args)
	token = extractYAMLValue(t, replyText(prop), "confirm_token")
	args["confirm_token"] = token
	ok := call(t, cs, "start_print", args)
	if ok.IsError || !strings.Contains(replyText(ok), "effect: sent") {
		t.Fatalf("confirmed start: %s", replyText(ok))
	}
}

func TestStartPrintCFS_InvalidSlotMapAndSource(t *testing.T) {
	cs, _, _ := cfsSetup(t, true)
	for name, tc := range map[string]struct {
		args map[string]any
		code string
		msg  string
	}{
		"bad slot":   {map[string]any{"filename": "three.gcode", "slot_map": []any{map[string]any{"filament": 0, "slot": "T9A"}}}, "invalid_input", "is not T1A to T4D"},
		"negative":   {map[string]any{"filename": "three.gcode", "slot_map": []any{map[string]any{"filament": -1, "slot": "T1A"}}}, "invalid_input", "out of range"},
		"duplicate":  {map[string]any{"filename": "three.gcode", "slot_map": []any{map[string]any{"filament": 0, "slot": "T1A"}, map[string]any{"filament": 0, "slot": "T1B"}}}, "invalid_input", "names filament 0 twice"},
		"wrong type": {map[string]any{"filename": "three.gcode", "slot_map": []any{map[string]any{"filament": 0, "slot": "T1B"}}}, "invalid_input", "material types must be equal"},
		"spool":      {map[string]any{"filename": "three.gcode", "source": "spool"}, "unavailable", "not yet verified"},
	} {
		res := call(t, cs, "start_print", tc.args)
		text := replyText(res)
		if !res.IsError || !strings.Contains(text, "code: "+tc.code) || !strings.Contains(text, tc.msg) {
			t.Errorf("%s: reply = %s, want code %s and %q", name, text, tc.code, tc.msg)
		}
	}
}

func TestCanonicalSlotMap(t *testing.T) {
	got, err := canonicalSlotMap([]slotMapEntry{{Filament: 5, Slot: "t2d"}, {Filament: 0, Slot: "T1C"}})
	if err != nil || got != "T1A=T1C,T2B=T2D" {
		t.Fatalf("got %q %v", got, err)
	}
	if got, _ := canonicalSlotMap(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

// --- registration ---

func TestFilamentToolsRegistrationAndAnnotations(t *testing.T) {
	for _, tc := range []struct {
		preset  domain.ToolPreset
		present []string
		absent  []string
	}{
		{domain.PresetMonitor, []string{"get_filaments", "list_filament_catalog"}, []string{"set_filament_definition"}},
		{domain.PresetCamera, []string{"get_filaments", "list_filament_catalog"}, []string{"set_filament_definition"}},
		{domain.PresetControl, []string{"get_filaments", "list_filament_catalog", "set_filament_definition"}, nil},
	} {
		deps, _ := singlePrinterDeps(t, tc.preset, nil)
		cs := testSession(t, deps)
		list, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		by := map[string]*mcp.Tool{}
		for _, tool := range list.Tools {
			by[tool.Name] = tool
		}
		for _, n := range tc.present {
			if by[n] == nil {
				t.Errorf("%s missing under %s", n, tc.preset)
			}
		}
		for _, n := range tc.absent {
			if by[n] != nil {
				t.Errorf("%s registered under %s", n, tc.preset)
			}
		}
		if tc.preset == domain.PresetControl {
			a := by["set_filament_definition"].Annotations
			if a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint || !a.IdempotentHint {
				t.Errorf("set_filament_definition annotations = %+v, want destructive and idempotent", a)
			}
			for _, n := range []string{"get_filaments", "list_filament_catalog"} {
				if !by[n].Annotations.ReadOnlyHint {
					t.Errorf("%s must be read-only", n)
				}
			}
			schema, _ := json.Marshal(by["start_print"].InputSchema)
			for _, want := range []string{"source", "slot_map", "self_test", "confirm_token", "\"cfs\"", "\"spool\"", "filament", "slot"} {
				if !strings.Contains(string(schema), want) {
					t.Errorf("start_print schema missing %q: %s", want, schema)
				}
			}
			desc := by["start_print"].Description
			for _, want := range []string{"two-step flow", "bed is clear", "slot_map", "may use T1B instead of T1C", "physical filament", "never reports started"} {
				if !strings.Contains(desc, want) {
					t.Errorf("start_print description missing %q", want)
				}
			}
		}
	}
	names := map[string]bool{}
	for _, info := range ToolCatalog() {
		names[info.Name] = true
	}
	for _, n := range []string{"get_filaments", "list_filament_catalog", "set_filament_definition"} {
		if !names[n] {
			t.Errorf("ToolCatalog is missing %s", n)
		}
	}
}

// Every tool that shows state uses the policy engine's derivation, so the
// start-window record is reflected right after a CFS start even though the
// fake printer shows no signal yet (self-test 100, identity-looking map).
func TestStartWindowIsReflectedByEveryStateTool(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	prop := call(t, cs, "start_print", map[string]any{"filename": "three.gcode"})
	token := extractYAMLValue(t, replyText(prop), "confirm_token")
	if res := call(t, cs, "start_print", map[string]any{"filename": "three.gcode", "confirm_token": token}); res.IsError {
		t.Fatalf("start: %s", replyText(res))
	}
	w.mu.Lock()
	for k := range w.boxMap {
		w.boxMap[k] = k
	}
	w.mu.Unlock()
	// Every reader in the package derives through Server.derive (the files and
	// camera tools cannot run against this fake, but share the same helper).
	for _, tool := range []string{"get_printer_status", "get_filaments", "list_filament_catalog", "get_current_job", "list_job_history", "list_console_messages", "list_printers"} {
		text := replyText(call(t, cs, tool, nil))
		if !strings.Contains(text, "activity_state: preparing") || !strings.Contains(text, "bucket: PP") {
			t.Errorf("%s does not reflect the start window:\n%s", tool, text[:min(len(text), 700)])
		}
	}
	// The window blocks a second start and every setpoint, and says why.
	res := call(t, cs, "set_bed_temperature", map[string]any{"target_c": 60})
	if !res.IsError || !strings.Contains(replyText(res), "preparing") {
		t.Fatalf("set_bed_temperature in the window: %s", replyText(res))
	}
	// Cancel is offered in the window (the 9999 stop is verified): a proposal, then
	// the stop frame, never Moonraker's cancel.
	res = call(t, cs, "cancel_print", nil)
	tok := extractYAMLValue(t, replyText(res), "confirm_token")
	res = call(t, cs, "cancel_print", map[string]any{"confirm_token": tok})
	text := replyText(res)
	if res.IsError || !strings.Contains(text, "effect: stopping") || !strings.Contains(text, "returns to idle within about a minute") {
		t.Fatalf("cancel_print in the window: %s", text)
	}
	if w.frames[len(w.frames)-1] != "stop" {
		t.Fatalf("frames = %v, want the 9999 stop last", w.frames)
	}
}

// --- review fixes (surface review) ---

func startArgs() map[string]any { return map[string]any{"filename": "three.gcode"} }

func propose1(t *testing.T, cs *mcp.ClientSession, args map[string]any) string {
	t.Helper()
	res := call(t, cs, "start_print", args)
	if res.IsError {
		t.Fatalf("proposal failed: %s", replyText(res))
	}
	return extractYAMLValue(t, replyText(res), "confirm_token")
}

func confirmed(args map[string]any, token string) map[string]any {
	out := map[string]any{"confirm_token": token}
	for k, v := range args {
		out[k] = v
	}
	return out
}

// The rendered bodies of the CFS start outcomes and the flow disclosure.
func TestStartPrintCFS_OutcomeBodies(t *testing.T) {
	t.Run("refused_map_mismatch", func(t *testing.T) {
		cs, w, st := cfsSetup(t, true)
		st.mu.Lock()
		st.extrudeFactor = 0.85
		st.mu.Unlock()
		w.forceMap = map[string]string{"T1A": "T1D"}
		token := propose1(t, cs, startArgs())
		res := call(t, cs, "start_print", confirmed(startArgs(), token))
		text := replyText(res)
		for _, want := range []string{"effect: refused_map_mismatch", "was NOT started", "Do not simply retry", "may keep it until the next start",
			"preparing with no print running", "the flow factor was restored to 85%", "flow_factor_restored_percent: 85"} {
			if !strings.Contains(text, want) {
				t.Errorf("mismatch reply missing %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "then call start_print again for a fresh proposal") {
			t.Errorf("the guidance still says to simply retry:\n%s", text)
		}
		if strings.Join(w.frames, ",") != "colorMatch" {
			t.Errorf("frames = %v, want only colorMatch", w.frames)
		}
	})
	t.Run("not_sent", func(t *testing.T) {
		cs, w, _ := cfsSetup(t, true)
		w.notSent = true
		token := propose1(t, cs, startArgs())
		text := replyText(call(t, cs, "start_print", confirmed(startArgs(), token)))
		for _, want := range []string{"effect: not_sent", "was NOT started", "the start frame was not sent", "may keep it until the next start"} {
			if !strings.Contains(text, want) {
				t.Errorf("not_sent reply missing %q:\n%s", want, text)
			}
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		cs, w, st := cfsSetup(t, true)
		st.mu.Lock()
		st.extrudeFactor = 0.9
		st.mu.Unlock()
		w.ambiguous = true
		token := propose1(t, cs, startArgs())
		res := call(t, cs, "start_print", confirmed(startArgs(), token))
		text := replyText(res)
		for _, want := range []string{"effect: unconfirmed", "the print may be starting", "Do not start it again", "stays at 100%"} {
			if !strings.Contains(text, want) {
				t.Errorf("unconfirmed reply missing %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "flow_factor_restored_percent") {
			t.Errorf("the flow must not be restored under a start that may be running:\n%s", text)
		}
		// The record is kept: the start window shows in the state right away.
		if st := replyText(call(t, cs, "get_printer_status", nil)); !strings.Contains(st, "activity_state: preparing") {
			t.Errorf("state after an unconfirmed start:\n%s", st[:min(len(st), 600)])
		}
	})
	t.Run("sent discloses the flow reset", func(t *testing.T) {
		cs, _, st := cfsSetup(t, true)
		st.mu.Lock()
		st.extrudeFactor = 0.85
		st.mu.Unlock()
		prop := replyText(call(t, cs, "start_print", startArgs()))
		for _, want := range []string{"flow factor is 85% and will be reset to 100% first"} {
			if !strings.Contains(prop, want) {
				t.Errorf("proposal missing %q:\n%s", want, prop)
			}
		}
		token := extractYAMLValue(t, prop, "confirm_token")
		text := replyText(call(t, cs, "start_print", confirmed(startArgs(), token)))
		if !strings.Contains(text, "reset from 85% to 100% for this start and stays at 100%") || !strings.Contains(text, "the map was verified") {
			t.Errorf("sent reply:\n%s", text)
		}
	})
}

// Each canonicalisation warning appears once in a proposal, and a large colour
// distance is rendered through the tool.
func TestStartPrintCFS_WarningsAreNotDuplicatedAndDistanceIsShown(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	w.files = append(w.files, crealityws.GcodeFileInfo{Name: "magenta.gcode", Path: "/mnt/UDISK/printer_data/gcodes/magenta.gcode", Material: "PETG", MaterialColors: "#FF00FF", Size: 1, CreateTime: 2})
	text := replyText(call(t, cs, "start_print", map[string]any{"filename": "three.gcode", "slot_map": []any{map[string]any{"filament": 1, "slot": "T1C"}}}))
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		t.Fatalf("no frontmatter:\n%s", text)
	}
	if n := strings.Count(parts[2], "the printer may use T1B instead of T1C: both must hold this spool"); n != 1 {
		t.Errorf("the canonicalisation warning appears %d times in the proposal body, want once:\n%s", n, parts[2])
	}
	if strings.Contains(text, "WARNING ") {
		t.Errorf("the body table still repeats warnings:\n%s", text)
	}
}

func TestGetFilaments_EditableFollowsThePrinterState(t *testing.T) {
	cs, _, st := cfsSetup(t, true)
	setPrinting(st)
	text := replyText(call(t, cs, "get_filaments", nil))
	for _, want := range []string{"edit_blocked:", "editable: false", "the printer does not allow an edit right now"} {
		if !strings.Contains(text, want) {
			t.Errorf("printing: missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "editable: true") || strings.Contains(text, "call set_filament_definition") {
		t.Errorf("printing: still promises an edit:\n%s", text)
	}
	cat := replyText(call(t, cs, "list_filament_catalog", nil))
	if strings.Contains(cat, "writable: true") || !strings.Contains(cat, "the printer does not allow an edit right now") {
		t.Errorf("catalog while printing:\n%s", cat)
	}
}

func TestGetFilaments_BusyCFSBlocksEditsAndSaysWhich(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	w.mu.Lock()
	w.boxMap["T1A"] = "T1B" // a start in flight: non-identity map, the start window
	w.mu.Unlock()
	text := replyText(call(t, cs, "get_filaments", nil))
	if !strings.Contains(text, "editable: false") || !strings.Contains(text, "not available in state") {
		t.Errorf("start window:\n%s", text)
	}
}

func TestStartPrint_CFSOnlyArgumentsWithoutACFSAreRefused(t *testing.T) {
	cs, _, st := cfsSetup(t, false)
	for name, args := range map[string]map[string]any{
		"self_test": {"filename": "three.gcode", "self_test": true},
		"slot_map":  {"filename": "three.gcode", "slot_map": []any{map[string]any{"filament": 0, "slot": "T1A"}}},
		"source":    {"filename": "three.gcode", "source": "cfs"},
	} {
		res := call(t, cs, "start_print", args)
		text := replyText(res)
		if !res.IsError || !strings.Contains(text, "code: invalid_input") || !strings.Contains(text, "only apply with a CFS connected") {
			t.Errorf("%s: reply = %s", name, text)
		}
	}
	st.mu.Lock()
	state := st.printStatsState
	st.mu.Unlock()
	if state != "standby" {
		t.Fatalf("a plain print was started (%s)", state)
	}
	if res := call(t, cs, "start_print", startArgs()); res.IsError {
		t.Fatalf("plain start refused: %s", replyText(res))
	}
}

func TestSetFilamentDefinition_UnconfirmedBodyPointsAtGetFilaments(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	w.mu.Lock()
	w.frozen = w.unit() // Moonraker never reflects the edit
	w.mu.Unlock()
	res := call(t, cs, "set_filament_definition", map[string]any{"slot": "T1A", "material": "99002", "color": "#00ff00"})
	text := replyText(res)
	for _, want := range []string{"effect: unconfirmed", "Call get_filaments", "not confirmed on both channels", "Moonraker confirmed=false"} {
		if !strings.Contains(text, want) {
			t.Errorf("unconfirmed edit missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "settle timeout") {
		t.Errorf("a slot edit has no settle timeout:\n%s", text)
	}
}

func TestSetFilamentDefinition_ErrorHintsFitTheTool(t *testing.T) {
	cs, _, _ := cfsSetup(t, true)
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"not found":     {map[string]any{"slot": "T1A", "material": "nope", "color": "#ffffff"}, "list_filament_catalog"},
		"invalid input": {map[string]any{"slot": "T9Z", "material": "99001", "color": "#ffffff"}, "colour (#rrggbb)"},
		"unavailable":   {map[string]any{"slot": "T2A", "material": "99001", "color": "#ffffff"}, "get_filaments"},
	} {
		text := replyText(call(t, cs, "set_filament_definition", tc.args))
		if !strings.Contains(text, tc.want) {
			t.Errorf("%s: missing %q:\n%s", name, tc.want, text)
		}
		for _, bad := range []string{"confirm_token", "widen the band", "fresh proposal"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: hint mentions %q, which this tool does not have:\n%s", name, bad, text)
			}
		}
	}
}

func TestPausePrint_CFSResultSaysWhetherAResumeRecordWasKept(t *testing.T) {
	cs, _, st := cfsSetup(t, true)
	setPrinting(st)
	text := replyText(call(t, cs, "pause_print", nil))
	if !strings.Contains(text, "a resume record was kept") {
		t.Errorf("pause result:\n%s", text)
	}
}

// The description pins for the two tools whose wording matters.
func TestToolDescriptionsCarryTheRequiredGuidance(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetControl, nil)
	cs := testSession(t, deps)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	desc := map[string]string{}
	for _, tool := range list.Tools {
		desc[tool.Name] = tool.Description
	}
	for _, want := range []string{"ask the user which spool is physically in the slot", "confirm the new definition matches it", "persistent until changed again", "never moves filament", "wrong definition"} {
		if !strings.Contains(desc["set_filament_definition"], want) {
			t.Errorf("set_filament_definition description missing %q", want)
		}
	}
	for tool, want := range map[string]string{
		"set_nozzle_temperature": "refused during a print",
		"set_flow_factor":        "Refused whenever a CFS is connected",
		"set_fan_speed":          "a filament change can reset the value",
		"set_speed_factor":       "a filament change can reset it",
		"resume_print":           "only for a clean pause this server issued",
		"pause_print":            "whether a resume record was kept",
		"cancel_print":           "Creality's own 9999 stop",
		"exclude_object":         "read clean with no error",
		"start_print":            "stays at 100% once the start frame was sent",
	} {
		if !strings.Contains(desc[tool], want) {
			t.Errorf("%s description missing %q", tool, want)
		}
	}
	if strings.Contains(desc["start_print"], "this is not enforced by a token") {
		t.Error("start_print description still says the bed check is not enforced by a token next to the CFS token")
	}
	if !strings.Contains(desc["list_filament_catalog"], "filter when you can") {
		t.Error("list_filament_catalog description does not advise filtering")
	}
}

// The actions list in the frontmatter tells the truth in the start window.
func TestStartWindowActionsListBlocksCancelWithTheRealReason(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	token := propose1(t, cs, startArgs())
	if res := call(t, cs, "start_print", confirmed(startArgs(), token)); res.IsError {
		t.Fatal(replyText(res))
	}
	w.mu.Lock()
	for k := range w.boxMap {
		w.boxMap[k] = k
	}
	w.mu.Unlock()
	text := replyText(call(t, cs, "get_printer_status", nil))
	for _, want := range []string{"start_window: true", "Creality's own stop", "The printer is in the self-test of a print start"} {
		if !strings.Contains(text, want) {
			t.Errorf("status in the window missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "name: cancel_print\n      status: needs_confirmation") {
		t.Errorf("cancel_print is listed as needs_confirmation in the window:\n%s", text)
	}
}

// Brand and name are joined without repeating the brand ("Generic Generic PETG").
func TestBrandIsNotRepeatedWhenTheNameStartsWithIt(t *testing.T) {
	cs, w, _ := cfsSetup(t, true)
	w.mu.Lock()
	for i := range w.boxs.MaterialBoxs[1].Materials {
		w.boxs.MaterialBoxs[1].Materials[i].Vendor = "Generic"
		w.boxs.MaterialBoxs[1].Materials[i].Name = "Generic PETG"
	}
	w.mu.Unlock()
	body := replyText(call(t, cs, "get_filaments", nil))
	if strings.Contains(body, "Generic Generic") || !strings.Contains(body, "Generic PETG") {
		t.Errorf("get_filaments:\n%s", body)
	}
	prop := replyText(call(t, cs, "start_print", startArgs()))
	if strings.Contains(prop, "Generic Generic") {
		t.Errorf("proposal table:\n%s", prop)
	}
	edit := replyText(call(t, cs, "set_filament_definition", map[string]any{"slot": "T1A", "material": "99002", "color": "#123456"}))
	if strings.Contains(edit, "Generic Generic") {
		t.Errorf("edit result:\n%s", edit)
	}
}

func (c *cfsWS) Stop(ctx context.Context) (bool, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.w.frames = append(c.w.frames, "stop")
	c.w.state = 7
	return true, nil
}

// The stopping, resuming and cancel not_sent outcomes render their own bodies.
func TestControlBodyForStoppingAndResuming(t *testing.T) {
	printer := domain.Printer{Name: "K2"}
	stop := controlBody(policy.Result{Action: policy.ActionCancelPrint, Printer: printer, Accepted: true, Effect: "stopping", Effects: []string{"wind down"}})
	if !strings.Contains(stop, "the printer is stopping") || !strings.Contains(stop, "winds down over about a minute") || !strings.Contains(stop, "wind down") {
		t.Errorf("stopping body: %s", stop)
	}
	sent := controlBody(policy.Result{Action: policy.ActionCancelPrint, Printer: printer, Accepted: true, Effect: "sent"})
	if !strings.Contains(sent, "stop frame was sent") || strings.Contains(sent, "map was verified") {
		t.Errorf("cancel sent body: %s", sent)
	}
	notSent := controlBody(policy.Result{Action: policy.ActionCancelPrint, Printer: printer, Effect: "not_sent"})
	if !strings.Contains(notSent, "nothing was stopped") || strings.Contains(notSent, "NOT started") {
		t.Errorf("cancel not_sent body: %s", notSent)
	}
	res := controlBody(policy.Result{Action: policy.ActionResumePrint, Printer: printer, Accepted: true, Effect: "resuming", Effects: []string{"resuming: the printer reheats to 250 C"}})
	if !strings.Contains(res, "is resuming") || !strings.Contains(res, "not printing yet") || !strings.Contains(res, "cancel_print is still available") || !strings.Contains(res, "250 C") {
		t.Errorf("resuming body: %s", res)
	}
}
