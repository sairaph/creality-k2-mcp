package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
)

// fakeCFS is the CFS half of fakePrinter (dev_docs/plan-v0.2.0.md section
// 3.2): settable 9999 CFS signal fields, fake boxsInfo, catalog and file
// records, and the write methods (ModifyMaterial, StartCFSPrint,
// StartSpoolPrint, Stop) that mutate them the way the printer does: a slot
// edit regroups same_material and updates Moonraker's box arrays, a colorMatch
// writes box.map canonicalised to the group head, a start frame begins the
// self-test with print_stats still standby. All invented ids and names (V1).
type fakeCFS struct {
	// 9999 signals. A field named in omit is reported as absent.
	deviceState, feedState, materialStatus, errcode, errkey int
	repoPlrStatus, upgradeStatus                            int
	withSelfTest, enableSelfTest                            int
	state                                                   int // 9999 state: 0 idle, 1 printing or self-test, 7 stopping, 8 resuming, 4 aborted
	omit                                                    map[string]bool
	ws9999Unreachable                                       bool

	// Moonraker box / pause_resume.
	omitBox    bool
	useup      int
	resumeErr  bool
	enable     int
	boxMap     map[string]string
	moonLag    bool // slot edits do not reach Moonraker's box arrays
	frozenUnit map[string]any
	unitStates map[string]string

	// 9999 reads and their failures.
	boxs         crealityws.BoxsInfo
	catalog      []crealityws.CatalogEntry
	fileInfos    []crealityws.GcodeFileInfo
	boxsInfoErr  error
	catalogErr   error
	fileInfoErr  error
	boxsInfoHook func(call int) // called at the start of every BoxsInfo call

	// Write behaviour and records.
	modifyNoFrame   bool // ModifyMaterial times out: (nil, true, nil)
	modifyIgnored   bool // the frame is sent but the printer does not apply it
	modifyNotSent   bool
	modifyCalls     int
	lastEdit        crealityws.MaterialEdit
	colorMatchCalls int
	lastItems       []crealityws.ColorMatchItem
	lastStartPath   string
	lastSelfTest    bool
	startFrames     int
	spoolStarts     int
	stopCalls       int
	forcedMapping   map[string]string // overrides the map the printer writes for a colorMatch
	startNotSent    bool
	startAmbiguous  bool  // the start frame write errors after verifyMap passed
	boxsInfoHang    bool  // BoxsInfo blocks until its context ends
	stopNoState     bool  // a stop frame does not show state 7 or 4
	stateSeq        []int // ReadStatus reports these states one per call before the plain state
	boxsInfoCalls   int
	wsEvents        []string
}

func newFakeCFS() *fakeCFS {
	c := &fakeCFS{
		feedState: 0, withSelfTest: 100, enable: 1, omit: map[string]bool{},
		unitStates: map[string]string{},
	}
	slot := func(id int, vendor, typ, name, rfid, color string) crealityws.SlotMaterial {
		one, zero, hundred := 1, 0, 100
		mn, mx, pr := 190.0, 240.0, 0.04
		return crealityws.SlotMaterial{ID: id, Vendor: vendor, Type: typ, Name: name, RFID: rfid, Color: color,
			MinTemp: &mn, MaxTemp: &mx, Pressure: &pr, Percent: &hundred, State: &one, Selected: &zero, EditStatus: &one}
	}
	c.boxs = crealityws.BoxsInfo{MaterialBoxs: []crealityws.MaterialBox{
		{ID: 0, Type: 1, State: 0, Materials: []crealityws.SlotMaterial{slot(0, "Acme", "PLA", "Acme Side PLA", "99003", "#0ffffff")}},
		{ID: 1, Type: 0, State: 1, Name: "TESTBOX", Materials: []crealityws.SlotMaterial{
			slot(0, "Acme", "PLA", "Acme Test PLA", "99001", "#0ff0000"),
			slot(1, "Acme", "PETG", "Acme Test PETG", "99002", "#0000000"),
			slot(2, "Acme", "PETG", "Acme Test PETG", "99002", "#0000000"),
			slot(3, "Acme", "PETG", "Acme Test PETG", "99002", "#0ffffff"),
		}},
	}}
	c.regroup()
	pa := func(v float64) *float64 { return &v }
	c.catalog = []crealityws.CatalogEntry{
		{ID: "99001", Brand: "Acme", Name: "Acme Test PLA", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: pa(0.04)},
		{ID: "99002", Brand: "Acme", Name: "Acme Test PETG", Type: "PETG", MinTemp: 220, MaxTemp: 270, PressureAdvance: pa(0.04)},
		{ID: "99003", Brand: "Acme", Name: "Acme Side PLA", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: pa(0.04)},
		{ID: "99004", Brand: "Acme", Name: "Acme Test ABS", Type: "ABS", MinTemp: 240, MaxTemp: 280, PressureAdvance: pa(0.05)},
		{ID: "99005", Brand: "Acme", Name: "Acme Zero", Type: "PLA", MinTemp: 0, MaxTemp: 0, PressureAdvance: pa(0.04)},
		{ID: "99006", Brand: "Acme", Name: "Acme NoPA", Type: "PLA", MinTemp: 190, MaxTemp: 240},
		{ID: "99007", Brand: "Acme", Name: "Acme Hot", Type: "PC", MinTemp: 250, MaxTemp: 350, PressureAdvance: pa(0.05)},
		{ID: "99008", Brand: "Acme", Name: "Acme Dup", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: pa(0.04)},
		{ID: "99009", Brand: "Other", Name: "Acme Dup", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: pa(0.04)},
		{ID: "99010", Brand: "Acme", Name: "Acme Wild PA", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: pa(2.5)},
	}
	c.resetMap()
	return c
}

// regroup recomputes same_material the way the printer does: CFS slots with
// the same material code and colour share a group, ordered by first appearance.
func (c *fakeCFS) regroup() {
	type key struct{ code, color string }
	var order []key
	groups := map[key]*crealityws.SameGroup{}
	for _, box := range c.boxs.MaterialBoxs {
		if box.Type != 0 {
			continue
		}
		for _, m := range box.Materials {
			if m.State == nil || *m.State == 0 {
				continue
			}
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
	c.boxs.SameMaterial = nil
	for _, k := range order {
		c.boxs.SameMaterial = append(c.boxs.SameMaterial, *groups[k])
	}
	c.boxs.SameMaterialOK = true
}

func (c *fakeCFS) resetMap() {
	c.boxMap = map[string]string{}
	for u := 1; u <= 4; u++ {
		for l := 'A'; l <= 'D'; l++ {
			k := fmt.Sprintf("T%d%c", u, l)
			c.boxMap[k] = k
		}
	}
}

func (c *fakeCFS) slotAt(box, id int) *crealityws.SlotMaterial {
	for i := range c.boxs.MaterialBoxs {
		if c.boxs.MaterialBoxs[i].ID == box {
			for j := range c.boxs.MaterialBoxs[i].Materials {
				if c.boxs.MaterialBoxs[i].Materials[j].ID == id {
					return &c.boxs.MaterialBoxs[i].Materials[j]
				}
			}
		}
	}
	return nil
}

// moonUnit renders Moonraker's box.T1 display object from the fake slots.
func (c *fakeCFS) moonUnit(box int) map[string]any {
	if c.frozenUnit != nil && box == 1 {
		return c.frozenUnit
	}
	state := "connect"
	if s, ok := c.unitStates[fmt.Sprintf("T%d", box)]; ok {
		state = s
	}
	types := make([]any, 4)
	colors := make([]any, 4)
	for i := 0; i < 4; i++ {
		types[i], colors[i] = "-1", "-1"
		if m := c.slotAt(box, i); m != nil {
			types[i] = "0" + m.RFID
			colors[i] = strings.TrimPrefix(m.Color, "#")
		}
	}
	return map[string]any{"state": state, "material_type": types, "color_value": colors}
}

// moonBox renders the Moonraker box object.
func (c *fakeCFS) moonBox(boxState string) map[string]any {
	out := map[string]any{"state": boxState, "enable": c.enable, "filament_useup": c.useup, "auto_refill": 1, "map": c.boxMap}
	if boxState == "connect" {
		out["T1"] = c.moonUnit(1)
	}
	return out
}

// applyColorMatch is what the printer does with a colorMatch frame: it writes
// box.map, canonicalising each requested slot to the first member of its
// same_material group (cfs-print-start.md 4.1). forcedMapping overrides it.
func (c *fakeCFS) applyColorMatch(items []crealityws.ColorMatchItem) {
	for _, it := range items {
		want := slotLabel(it.BoxID, it.MaterialID)
		for _, g := range c.boxs.SameMaterial {
			for _, s := range g.Slots {
				if s.BoxID == it.BoxID && s.MaterialID == it.MaterialID {
					want = slotLabel(g.Slots[0].BoxID, g.Slots[0].MaterialID)
				}
			}
		}
		c.boxMap[it.ID] = want
	}
	for k, v := range c.forcedMapping {
		c.boxMap[k] = v
	}
}

// --- fakePrinter setters ---

func (f *fakePrinter) cfs9999(fn func(c *fakeCFS)) { f.withLock(func() { fn(f.cfs) }) }

func (f *fakePrinter) setFeedState(v int)   { f.cfs9999(func(c *fakeCFS) { c.feedState = v }) }
func (f *fakePrinter) setDeviceState(v int) { f.cfs9999(func(c *fakeCFS) { c.deviceState = v }) }
func (f *fakePrinter) setCFSErr(code int)   { f.cfs9999(func(c *fakeCFS) { c.errcode = code }) }
func (f *fakePrinter) setWS9999Unreachable(u bool) {
	f.cfs9999(func(c *fakeCFS) { c.ws9999Unreachable = u })
}
func (f *fakePrinter) setOmitBox(o bool) { f.cfs9999(func(c *fakeCFS) { c.omitBox = o }) }
func (f *fakePrinter) omit9999(names ...string) {
	f.cfs9999(func(c *fakeCFS) {
		for _, n := range names {
			c.omit[n] = true
		}
	})
}
func (f *fakePrinter) setSelfTest(progress int) {
	f.cfs9999(func(c *fakeCFS) { c.withSelfTest = progress })
}

// addCFSFile registers a gcodes file with the printer's own record of its
// filaments (types ';' separated, colours ';' separated).
func (f *fakePrinter) addCFSFile(name, material, colors string) {
	f.withLock(func() {
		f.files[name] = true
		f.cfs.fileInfos = append(f.cfs.fileInfos, crealityws.GcodeFileInfo{
			Name: name, Path: "/mnt/UDISK/printer_data/gcodes/" + name, Material: material, MaterialColors: colors,
			Size: 1000, CreateTime: 1790000000,
		})
	})
}

func (f *fakePrinter) wsEventList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cfs.wsEvents...)
}

// --- policy.WS9999Client CFS methods ---

var errFakeUnreachable = errors.New("fake: port 9999 unreachable")

func (f *fakePrinter) BoxsInfo(ctx context.Context) (crealityws.BoxsInfo, error) {
	f.mu.Lock()
	f.cfs.boxsInfoCalls++
	call := f.cfs.boxsInfoCalls
	hook := f.cfs.boxsInfoHook
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	f.mu.Lock()
	hang := f.cfs.boxsInfoHang
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return crealityws.BoxsInfo{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cfs.ws9999Unreachable {
		return crealityws.BoxsInfo{}, errFakeUnreachable
	}
	if f.cfs.boxsInfoErr != nil {
		return crealityws.BoxsInfo{}, f.cfs.boxsInfoErr
	}
	return cloneBoxs(f.cfs.boxs), nil
}

func cloneBoxs(b crealityws.BoxsInfo) crealityws.BoxsInfo {
	out := crealityws.BoxsInfo{SameMaterialOK: b.SameMaterialOK}
	for _, g := range b.SameMaterial {
		g.Slots = append([]crealityws.SlotRef(nil), g.Slots...)
		out.SameMaterial = append(out.SameMaterial, g)
	}
	cp := func(p *int) *int {
		if p == nil {
			return nil
		}
		v := *p
		return &v
	}
	for _, box := range b.MaterialBoxs {
		mats := make([]crealityws.SlotMaterial, len(box.Materials))
		for i, m := range box.Materials {
			m.State, m.Selected, m.EditStatus, m.Percent = cp(m.State), cp(m.Selected), cp(m.EditStatus), cp(m.Percent)
			mats[i] = m
		}
		box.Materials = mats
		out.MaterialBoxs = append(out.MaterialBoxs, box)
	}
	return out
}

func (f *fakePrinter) Materials(ctx context.Context) ([]crealityws.CatalogEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cfs.ws9999Unreachable {
		return nil, errFakeUnreachable
	}
	if f.cfs.catalogErr != nil {
		return nil, f.cfs.catalogErr
	}
	return append([]crealityws.CatalogEntry(nil), f.cfs.catalog...), nil
}

func (f *fakePrinter) GcodeFiles(ctx context.Context) ([]crealityws.GcodeFileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cfs.ws9999Unreachable {
		return nil, errFakeUnreachable
	}
	if f.cfs.fileInfoErr != nil {
		return nil, f.cfs.fileInfoErr
	}
	return append([]crealityws.GcodeFileInfo(nil), f.cfs.fileInfos...), nil
}

func (f *fakePrinter) ModifyMaterial(ctx context.Context, e crealityws.MaterialEdit) (*crealityws.BoxsInfo, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cfs
	c.modifyCalls++
	c.lastEdit = e
	c.wsEvents = append(c.wsEvents, "modifyMaterial")
	if c.modifyNotSent || c.ws9999Unreachable {
		return nil, false, errFakeUnreachable
	}
	m := c.slotAt(e.BoxID, e.SlotID)
	if m == nil {
		return nil, true, nil
	}
	if c.modifyIgnored {
		return nil, true, nil
	}
	mn, mx, pr := e.Entry.MinTemp, e.Entry.MaxTemp, *e.Entry.PressureAdvance
	m.RFID, m.Vendor, m.Type, m.Name = e.Entry.ID, e.Entry.Brand, e.Entry.Type, e.Entry.Name
	m.Color = "#0" + strings.ToLower(strings.TrimPrefix(e.Color, "#"))
	m.MinTemp, m.MaxTemp, m.Pressure = &mn, &mx, &pr
	c.regroup()
	if c.modifyNoFrame {
		return nil, true, nil
	}
	b := cloneBoxs(c.boxs)
	return &b, true, nil
}

func (f *fakePrinter) StartCFSPrint(ctx context.Context, path string, items []crealityws.ColorMatchItem, selfTest bool, verifyMap func(ctx context.Context) error) (bool, error) {
	f.mu.Lock()
	c := f.cfs
	c.colorMatchCalls++
	c.lastItems = append([]crealityws.ColorMatchItem(nil), items...)
	c.lastStartPath, c.lastSelfTest = path, selfTest
	c.wsEvents = append(c.wsEvents, "colorMatch")
	if c.ws9999Unreachable {
		f.mu.Unlock()
		return false, errFakeUnreachable
	}
	c.applyColorMatch(items)
	f.mu.Unlock()

	if err := verifyMap(ctx); err != nil {
		f.mu.Lock()
		c.wsEvents = append(c.wsEvents, "verify_failed")
		f.mu.Unlock()
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.startNotSent {
		return false, errFakeUnreachable
	}
	if c.startAmbiguous {
		c.startFrames++ // the printer may well have received it
		c.wsEvents = append(c.wsEvents, "multiColorPrint_write_error")
		return false, &crealityws.StartFrameError{Err: errFakeUnreachable}
	}
	c.startFrames++
	c.wsEvents = append(c.wsEvents, "multiColorPrint")
	c.withSelfTest = 40 // the self-test begins; print_stats stays standby
	c.state = 1
	return true, nil
}

func (f *fakePrinter) StartSpoolPrint(ctx context.Context, path string, selfTest bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cfs
	c.spoolStarts++
	c.lastStartPath, c.lastSelfTest = path, selfTest
	c.wsEvents = append(c.wsEvents, "opGcodeFile")
	c.enable = 0
	c.withSelfTest = 40
	return true, nil
}

func (f *fakePrinter) Stop(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cfs
	c.stopCalls++
	c.wsEvents = append(c.wsEvents, "stop")
	c.withSelfTest = 100
	if !c.stopNoState {
		c.state = 7
	}
	c.resetMap()
	return true, nil
}

// setMoonLag freezes Moonraker's view of unit T1 so a later slot edit reaches
// port 9999 but never Moonraker (the "unconfirmed" read-back case).
func (f *fakePrinter) setMoonLag(lag bool) {
	f.cfs9999(func(c *fakeCFS) {
		c.moonLag = lag
		if lag {
			c.frozenUnit = nil
			c.frozenUnit = c.moonUnit(1)
		} else {
			c.frozenUnit = nil
		}
	})
}

// setPreparing puts the fake in START_PRINT's prepare phase (printing with
// print_duration 0).
func (f *fakePrinter) setPreparing(filename string) {
	f.setPrinting(filename)
	f.withLock(func() { f.printDuration = 0 })
}
