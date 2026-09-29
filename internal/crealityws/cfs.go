package crealityws

// CFS reads and writes on port 9999 (dev_docs/plan-v0.2.0.md section 1 and
// section 8a.7; dev_docs/cfs-protocol.md; dev_docs/cfs-print-start.md).
//
// Names (brand, material, colour of a slot, the filament catalog) come only
// from the printer (plan decision V1): nothing of Creality's catalog is
// embedded in this repository.
//
// Every write here is a fixed-shape struct with fixed JSON tags, never a map,
// so the wire bytes are deterministic and a test can pin them by parsed
// comparison. Like SetLight, none of them has an acknowledgement frame: a
// write is "sent" when the frame reached the socket and "confirmed" only when
// a later push proves the effect.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	// BoxsInfoTimeout, MaterialsTimeout and GcodeFilesTimeout bound how long
	// the typed reads wait for their reply frame (plan 1.1). The catalog reply
	// is by far the largest (about 173 KB, protocol section 3).
	BoxsInfoTimeout   = 6 * time.Second
	MaterialsTimeout  = 10 * time.Second
	GcodeFilesTimeout = 8 * time.Second

	// ModifyConfirmTimeout bounds how long ModifyMaterial waits for the
	// confirming boxsInfo push (plan 1.3; the live write test saw all three
	// reply frames within 6 s, protocol section 8).
	ModifyConfirmTimeout = 6 * time.Second
)

// startLingerDuration is how long a start write keeps its socket open and
// reading after the start frame was written (plan 8a.7, review-2 MF7): the
// frame must not be lost to an immediate close, and Creality's own client
// keeps its socket open. A variable so tests do not each wait a real second.
var startLingerDuration = time.Second

// modifyConfirmTimeout is ModifyConfirmTimeout as a variable for the same
// reason: the unconfirmed-outcome test must not wait six real seconds.
var modifyConfirmTimeout = ModifyConfirmTimeout

// heartbeatModeCode is the ModeCode of the frame the printer sends
// periodically and expects the literal text "ok" back for.
const heartbeatModeCode = "heart_beat"

// --- frame reading ---

// readObject reads one message and returns its top-level JSON object with
// every value left raw. ok is false (nil error) for a frame to skip: the
// literal "ok", non-JSON, or JSON that is not an object.
func readObject(ctx context.Context, conn *websocket.Conn) (map[string]json.RawMessage, bool, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, false, err
	}
	if string(data) == heartbeatText {
		return nil, false, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, false, nil
	}
	return obj, true, nil
}

// isHeartbeat reports whether obj is the printer's heartbeat frame.
func isHeartbeat(obj map[string]json.RawMessage) bool {
	raw, ok := obj["ModeCode"]
	if !ok {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == heartbeatModeCode
}

// answerHeartbeat writes the literal "ok" reply, bounded so a wedged socket
// cannot hang the caller.
func answerHeartbeat(ctx context.Context, conn *websocket.Conn) error {
	wctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, []byte(heartbeatText))
}

// writeJSON marshals v and sends it as one text frame.
func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, body)
}

// get is the request helper every typed read uses (plan 1.1): connect, send
// {"method":"get","params":{"<param>":1}}, read frames until an object has
// replyKey (answering heartbeats with "ok" on the way), return that key's raw
// value and close. Replies are not correlated to requests on this protocol,
// so an unrelated push frame is simply skipped.
func (c *Client) get(ctx context.Context, param, replyKey string, timeout time.Duration) (json.RawMessage, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.CloseNow()

	req := struct {
		Method string         `json:"method"`
		Params map[string]int `json:"params"`
	}{Method: "get", Params: map[string]int{param: 1}}
	if err := writeJSON(ctx, conn, req); err != nil {
		return nil, fmt.Errorf("crealityws: send get %s: %w", param, err)
	}

	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		obj, ok, err := readObject(readCtx, conn)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("crealityws: get %s: %w", param, ctx.Err())
			}
			return nil, fmt.Errorf("crealityws: get %s: no %q reply within %s: %w", param, replyKey, timeout, err)
		}
		if !ok {
			continue
		}
		if isHeartbeat(obj) {
			if err := answerHeartbeat(ctx, conn); err != nil {
				return nil, fmt.Errorf("crealityws: get %s: answer heartbeat: %w", param, err)
			}
			continue
		}
		if raw, found := obj[replyKey]; found {
			return raw, nil
		}
	}
}

// --- tolerant number and string coercion ---
//
// Numbers on this protocol may be JSON numbers or numeric strings
// (status.go asInt makes the same point for the telemetry frames).

func rawFloat(raw json.RawMessage) (float64, bool) {
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func rawInt(raw json.RawMessage) (int, bool) {
	f, ok := rawFloat(raw)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func objInt(m map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	return rawInt(raw)
}

func objString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := rawString(raw)
	return s
}

func objIntPtr(m map[string]json.RawMessage, key string) *int {
	v, ok := objInt(m, key)
	if !ok {
		return nil
	}
	return &v
}

func objFloatPtr(m map[string]json.RawMessage, key string) *float64 {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	f, ok := rawFloat(raw)
	if !ok {
		return nil
	}
	return &f
}

// --- boxsInfo ---

// BoxsInfo is the printer's own view of every filament unit
// (dev_docs/cfs-protocol.md section 3): the CFS units, the side spool holder
// and the printer's regrouping of interchangeable slots.
type BoxsInfo struct {
	MaterialBoxs []MaterialBox
	// SameMaterial is the printer's same_material regrouping, decoded from
	// [[code6, color7, [{"boxId":n,"materialId":m},...], name], ...].
	// SameMaterialOK is false when the key was absent or any group failed to
	// decode, in which case SameMaterial is nil: a caller must treat "unknown"
	// as unknown, never as "no groups".
	SameMaterial   []SameGroup
	SameMaterialOK bool
}

// SameGroup is one same_material entry: slots the printer treats as
// interchangeable for auto-refill. Code is the 6-char material code ("0" +
// the 5-char catalog id) and Color the 7-char colour without a leading "#".
type SameGroup struct {
	Code, Color, Name string
	Slots             []SlotRef
}

// SlotRef names one slot by unit id and slot index (A=0..D=3).
type SlotRef struct{ BoxID, MaterialID int }

// MaterialBox is one entry of materialBoxs: type 0 is a CFS unit (id 1..4),
// type 1 the side spool holder (id 0).
type MaterialBox struct {
	ID, Type, State int
	Name            string // materialBoxName
	Temp, Humidity  *float64
	Materials       []SlotMaterial
}

// SlotMaterial is one slot. RFID is the 5-char catalog id (not a tag UID);
// Color is "#" plus 7 hex characters. State 0 is undefined, 1 a manual
// definition, 2 an RFID-recognised spool; nil pointers mean the printer did
// not report the field.
type SlotMaterial struct {
	ID                                   int
	Vendor, Type, Name, RFID, Color      string
	MinTemp, MaxTemp, Pressure           *float64
	Percent, State, Selected, EditStatus *int
}

// UnmarshalJSON decodes tolerantly (numbers as numbers or numeric strings,
// unknown fields ignored). A box without a decodable id or type cannot be
// addressed and fails the decode, which fails the read closed.
func (b *MaterialBox) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*b = MaterialBox{}
	var ok bool
	if b.ID, ok = objInt(m, "id"); !ok {
		return errors.New("materialBoxs entry has no decodable id")
	}
	if b.Type, ok = objInt(m, "type"); !ok {
		return errors.New("materialBoxs entry has no decodable type")
	}
	b.State, _ = objInt(m, "state")
	b.Name = objString(m, "materialBoxName")
	b.Temp = objFloatPtr(m, "temp")
	b.Humidity = objFloatPtr(m, "humidity")
	if raw, has := m["materials"]; has {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("materials: %w", err)
		}
		for _, it := range items {
			var s SlotMaterial
			if err := json.Unmarshal(it, &s); err != nil {
				return err
			}
			b.Materials = append(b.Materials, s)
		}
	}
	return nil
}

// UnmarshalJSON: see MaterialBox.UnmarshalJSON. A slot needs a decodable id.
func (s *SlotMaterial) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*s = SlotMaterial{}
	var ok bool
	if s.ID, ok = objInt(m, "id"); !ok {
		return errors.New("materials entry has no decodable id")
	}
	s.Vendor = objString(m, "vendor")
	s.Type = objString(m, "type")
	s.Name = objString(m, "name")
	s.RFID = objString(m, "rfid")
	s.Color = objString(m, "color")
	s.MinTemp = objFloatPtr(m, "minTemp")
	s.MaxTemp = objFloatPtr(m, "maxTemp")
	s.Pressure = objFloatPtr(m, "pressure")
	s.Percent = objIntPtr(m, "percent")
	s.State = objIntPtr(m, "state")
	s.Selected = objIntPtr(m, "selected")
	s.EditStatus = objIntPtr(m, "editStatus")
	return nil
}

// UnmarshalJSON decodes a boxsInfo object. materialBoxs must be present (a
// frame without it is not a usable read); a same_material that fails to
// decode only clears SameMaterialOK.
func (b *BoxsInfo) UnmarshalJSON(data []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*b = BoxsInfo{}
	raw, ok := m["materialBoxs"]
	if !ok {
		return errors.New("boxsInfo has no materialBoxs")
	}
	if err := json.Unmarshal(raw, &b.MaterialBoxs); err != nil {
		return fmt.Errorf("materialBoxs: %w", err)
	}
	if sm, has := m["same_material"]; has {
		b.SameMaterial, b.SameMaterialOK = decodeSameMaterial(sm)
	}
	return nil
}

func decodeSameMaterial(raw json.RawMessage) ([]SameGroup, bool) {
	var groups []json.RawMessage
	if json.Unmarshal(raw, &groups) != nil {
		return nil, false
	}
	out := make([]SameGroup, 0, len(groups))
	for _, g := range groups {
		var parts []json.RawMessage
		if json.Unmarshal(g, &parts) != nil || len(parts) != 4 {
			return nil, false
		}
		code, ok1 := rawString(parts[0])
		color, ok2 := rawString(parts[1])
		name, ok3 := rawString(parts[3])
		var refs []map[string]json.RawMessage
		if !ok1 || !ok2 || !ok3 || json.Unmarshal(parts[2], &refs) != nil {
			return nil, false
		}
		grp := SameGroup{Code: code, Color: color, Name: name}
		for _, r := range refs {
			box, okB := objInt(r, "boxId")
			mat, okM := objInt(r, "materialId")
			if !okB || !okM {
				return nil, false
			}
			grp.Slots = append(grp.Slots, SlotRef{BoxID: box, MaterialID: mat})
		}
		out = append(out, grp)
	}
	return out, true
}

// BoxsInfo reads the printer's filament units (get boxsInfo).
func (c *Client) BoxsInfo(ctx context.Context) (BoxsInfo, error) {
	raw, err := c.get(ctx, "boxsInfo", "boxsInfo", BoxsInfoTimeout)
	if err != nil {
		return BoxsInfo{}, err
	}
	var out BoxsInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		return BoxsInfo{}, fmt.Errorf("crealityws: decode boxsInfo: %w", err)
	}
	return out, nil
}

// --- catalog ---

// CatalogEntry is one entry of the printer's own filament catalog
// (reqMaterials -> retMaterials[]): base.id, base.brand, base.name,
// base.meterialType [sic], base.minTemp, base.maxTemp and
// kvParam.pressure_advance (a string on the wire). PressureAdvance is nil
// when absent or unparsable; ModifyMaterial refuses such an entry.
type CatalogEntry struct {
	ID, Brand, Name, Type string
	MinTemp, MaxTemp      float64
	PressureAdvance       *float64
}

// Materials reads the printer's filament catalog (get reqMaterials). Entries
// without a base.id cannot be written back and are dropped.
func (c *Client) Materials(ctx context.Context) ([]CatalogEntry, error) {
	raw, err := c.get(ctx, "reqMaterials", "retMaterials", MaterialsTimeout)
	if err != nil {
		return nil, err
	}
	var items []struct {
		Base   map[string]json.RawMessage `json:"base"`
		KVParm map[string]json.RawMessage `json:"kvParam"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("crealityws: decode retMaterials: %w", err)
	}
	out := make([]CatalogEntry, 0, len(items))
	for _, it := range items {
		id := objString(it.Base, "id")
		if id == "" {
			continue
		}
		e := CatalogEntry{
			ID:    id,
			Brand: objString(it.Base, "brand"),
			Name:  objString(it.Base, "name"),
			Type:  objString(it.Base, "meterialType"),
		}
		if v := objFloatPtr(it.Base, "minTemp"); v != nil {
			e.MinTemp = *v
		}
		if v := objFloatPtr(it.Base, "maxTemp"); v != nil {
			e.MaxTemp = *v
		}
		e.PressureAdvance = objFloatPtr(it.KVParm, "pressure_advance")
		out = append(out, e)
	}
	return out, nil
}

// --- files ---

// GcodeFileInfo is the printer's own record of a stored file
// (retGcodeFileInfo2[]): the source Creality's clients use for a file's
// filament list (dev_docs/cfs-print-start.md section 2). Material and
// MaterialColors are ';' separated in tool order; Match is the last recorded
// colorMatch ("T1A=T1B T1B=T1C "). Size and CreateTime identify the file
// version for token binding (plan 8a.3).
type GcodeFileInfo struct {
	Name, Path, Material, MaterialColors, Match string
	MaterialIDs                                 string
	Size, CreateTime                            int64
}

// GcodeFiles reads the printer's stored-file records (get reqGcodeFile).
func (c *Client) GcodeFiles(ctx context.Context) ([]GcodeFileInfo, error) {
	raw, err := c.get(ctx, "reqGcodeFile", "retGcodeFileInfo2", GcodeFilesTimeout)
	if err != nil {
		return nil, err
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("crealityws: decode retGcodeFileInfo2: %w", err)
	}
	out := make([]GcodeFileInfo, 0, len(items))
	for _, it := range items {
		f := GcodeFileInfo{
			Name:           objString(it, "name"),
			Path:           objString(it, "path"),
			Material:       objString(it, "material"),
			MaterialColors: objString(it, "materialColors"),
			MaterialIDs:    objString(it, "materialIds"),
			Match:          objString(it, "match"),
		}
		if v, ok := objInt64(it, "file_size"); ok {
			f.Size = v
		}
		if v, ok := objInt64(it, "create_time"); ok {
			f.CreateTime = v
		}
		out = append(out, f)
	}
	return out, nil
}

func objInt64(m map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	f, ok := rawFloat(raw)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// --- writes ---

// setFrame is the envelope every write uses. P is one of the fixed-shape
// param structs below, never a map.
type setFrame[P any] struct {
	Method string `json:"method"`
	Params P      `json:"params"`
}

func newSetFrame[P any](p P) setFrame[P] { return setFrame[P]{Method: "set", Params: p} }

type modifyMaterialParams struct {
	ModifyMaterial modifyMaterialBody `json:"modifyMaterial"`
}

type modifyMaterialBody struct {
	BoxID    int     `json:"boxId"`
	ID       int     `json:"id"`
	BoxType  int     `json:"boxType"`
	RFID     string  `json:"rfid"`
	Vendor   string  `json:"vendor"`
	Type     string  `json:"type"`
	Name     string  `json:"name"`
	Color    string  `json:"color"`
	MinTemp  float64 `json:"minTemp"`
	MaxTemp  float64 `json:"maxTemp"`
	Pressure float64 `json:"pressure"`
}

// MaterialEdit is one slot edit: every field comes from ONE catalog entry
// (plan V2); Color is "#rrggbb".
type MaterialEdit struct {
	BoxID, SlotID, BoxType int
	Entry                  CatalogEntry
	Color                  string
}

// normColor reduces a colour to lower-case "rrggbb": the "#rrggbb" form the
// callers use and the "#0rrggbb" form the printer streams. Anything else
// yields "" so a garbage colour never compares equal to anything.
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
	return s
}

func (e MaterialEdit) validate() error {
	if e.BoxID < 0 || e.BoxID > 4 || e.SlotID < 0 || e.SlotID > 3 {
		return fmt.Errorf("crealityws: modifyMaterial: box %d slot %d out of range", e.BoxID, e.SlotID)
	}
	if e.BoxType != 0 && e.BoxType != 1 {
		return fmt.Errorf("crealityws: modifyMaterial: boxType %d must be 0 (CFS) or 1 (side spool)", e.BoxType)
	}
	if e.BoxType == 0 && e.BoxID < 1 {
		return fmt.Errorf("crealityws: modifyMaterial: a CFS slot needs a unit id 1 to 4, got %d", e.BoxID)
	}
	if e.BoxType == 1 && (e.BoxID != 0 || e.SlotID != 0) {
		return fmt.Errorf("crealityws: modifyMaterial: the side spool is unit 0 slot 0, got unit %d slot %d", e.BoxID, e.SlotID)
	}
	if e.Entry.ID == "" {
		return errors.New("crealityws: modifyMaterial: catalog entry has no id")
	}
	if strings.TrimSpace(e.Entry.Type) == "" || strings.TrimSpace(e.Entry.Name) == "" {
		return fmt.Errorf("crealityws: modifyMaterial: catalog entry %s has no material type or name", e.Entry.ID)
	}
	if e.Entry.PressureAdvance == nil {
		return fmt.Errorf("crealityws: modifyMaterial: catalog entry %s has no pressure_advance", e.Entry.ID)
	}
	if !strings.HasPrefix(e.Color, "#") || len(e.Color) != 7 || normColor(e.Color) == "" {
		return fmt.Errorf("crealityws: modifyMaterial: colour %q is not #rrggbb", e.Color)
	}
	return nil
}

// confirms reports whether b proves e took effect (plan V2): the slot shows
// the new rfid and colour AND editStatus 1 AND the printer's same_material
// has regrouped the slot into a group with code "0"+id and colour "0"+rrggbb.
// The first boxsInfo pushed after a write is mid-write (old grouping,
// editStatus 0 for the edited slot: protocol section 8, review M4), which is
// exactly what the editStatus and regrouping conditions reject.
// Confirms is the exported form of the V2 confirmation test, for a caller that
// has to judge a boxsInfo read taken after ModifyMaterial timed out.
func (e MaterialEdit) Confirms(b BoxsInfo) bool { return e.confirms(b) }

func (e MaterialEdit) confirms(b BoxsInfo) bool {
	want := normColor(e.Color)
	var slot *SlotMaterial
	for i := range b.MaterialBoxs {
		box := &b.MaterialBoxs[i]
		if box.ID != e.BoxID || box.Type != e.BoxType {
			continue
		}
		for j := range box.Materials {
			if box.Materials[j].ID == e.SlotID {
				slot = &box.Materials[j]
			}
		}
	}
	if slot == nil || slot.RFID != e.Entry.ID || normColor(slot.Color) != want {
		return false
	}
	if slot.EditStatus == nil || *slot.EditStatus != 1 {
		return false
	}
	// The side spool is never in a same_material group (supervised session
	// 2026-09-29: rfid, colour and editStatus 1 confirm it; the first push has
	// editStatus 0 as for a CFS slot), so only CFS slots need the regrouping.
	if e.BoxType == 1 {
		return true
	}
	if !b.SameMaterialOK {
		return false
	}
	for _, g := range b.SameMaterial {
		if g.Code != "0"+e.Entry.ID || !strings.EqualFold(g.Color, "0"+want) {
			continue
		}
		for _, r := range g.Slots {
			if r.BoxID == e.BoxID && r.MaterialID == e.SlotID {
				return true
			}
		}
	}
	return false
}

// ModifyMaterial rewrites one slot's filament definition with Creality's own
// modifyMaterial message (plan 1.3, dev_docs/cfs-protocol.md section 4 and
// 8), then reads frames for up to ModifyConfirmTimeout and returns on the
// FIRST boxsInfo frame that confirms the edit (MaterialEdit.confirms).
//
// sent reports whether the frame was written. A timeout without a confirming
// frame is (nil, true, nil): "sent, not confirmed", SetLight's pattern.
// err is non-nil for an invalid edit (nothing sent), a connect or send
// failure, or the caller's context ending; a socket that dies while waiting
// returns sent true with the error.
func (c *Client) ModifyMaterial(ctx context.Context, e MaterialEdit) (confirmedFrame *BoxsInfo, sent bool, err error) {
	if err := e.validate(); err != nil {
		return nil, false, err
	}
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, false, err
	}
	defer conn.CloseNow()

	frame := newSetFrame(modifyMaterialParams{ModifyMaterial: modifyMaterialBody{
		BoxID:    e.BoxID,
		ID:       e.SlotID,
		BoxType:  e.BoxType,
		RFID:     e.Entry.ID,
		Vendor:   e.Entry.Brand,
		Type:     e.Entry.Type,
		Name:     e.Entry.Name,
		Color:    "#0" + normColor(e.Color),
		MinTemp:  e.Entry.MinTemp + 1e-8, // +1e-8 forces JSON floats, as Creality's UI does
		MaxTemp:  e.Entry.MaxTemp + 1e-8,
		Pressure: *e.Entry.PressureAdvance,
	}})
	if err := writeJSON(ctx, conn, frame); err != nil {
		return nil, false, fmt.Errorf("crealityws: send modifyMaterial: %w", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, modifyConfirmTimeout)
	defer cancel()
	for {
		obj, ok, err := readObject(readCtx, conn)
		if err != nil {
			if ctx.Err() != nil {
				return nil, true, fmt.Errorf("crealityws: confirm modifyMaterial: %w", ctx.Err())
			}
			if readCtx.Err() != nil {
				return nil, true, nil // sent, not confirmed
			}
			return nil, true, fmt.Errorf("crealityws: confirm modifyMaterial: %w", err)
		}
		if !ok {
			continue
		}
		if isHeartbeat(obj) {
			if err := answerHeartbeat(ctx, conn); err != nil {
				return nil, true, fmt.Errorf("crealityws: answer heartbeat: %w", err)
			}
			continue
		}
		raw, has := obj["boxsInfo"]
		if !has {
			continue
		}
		var b BoxsInfo
		if json.Unmarshal(raw, &b) != nil {
			continue
		}
		if e.confirms(b) {
			return &b, true, nil
		}
	}
}

// ColorMatchItem is one entry of a colorMatch list
// (dev_docs/cfs-print-start.md section 1.2): ID is the SLICER TOOL
// ("T1A" = the file's filament 0), BoxID and MaterialID the physical slot it
// is mapped to, Type that slot's material type and Color its "#rrggbb".
type ColorMatchItem struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Color      string `json:"color"`
	BoxID      int    `json:"boxId"`
	MaterialID int    `json:"materialId"`
}

type colorMatchParams struct {
	ColorMatch colorMatchBody `json:"colorMatch"`
}

type colorMatchBody struct {
	Path string           `json:"path"`
	List []ColorMatchItem `json:"list"`
}

type multiColorPrintParams struct {
	MultiColorPrint multiColorPrintBody `json:"multiColorPrint"`
}

type multiColorPrintBody struct {
	Gcode          string `json:"gcode"`
	EnableSelfTest int    `json:"enableSelfTest"`
}

type opGcodeFileParams struct {
	OpGcodeFile    string `json:"opGcodeFile"`
	EnableSelfTest int    `json:"enableSelfTest"`
}

func selfTestInt(on bool) int {
	if on {
		return 1
	}
	return 0
}

// pump is the reader goroutine every start write runs for the whole call
// (plan 8a.7, review-2 MF7): it keeps answering the printer's heartbeats with
// "ok" so the socket is not dropped between the frames, including while the
// caller's verifyMap callback runs. Frames other than heartbeats are
// discarded: nothing here waits for a reply, because none of the start
// messages has one.
type pump struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func startPump(ctx context.Context, conn *websocket.Conn) *pump {
	rctx, cancel := context.WithCancel(ctx)
	p := &pump{conn: conn, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			obj, ok, err := readObject(rctx, conn)
			if err != nil {
				return
			}
			if ok && isHeartbeat(obj) {
				if answerHeartbeat(rctx, conn) != nil {
					return
				}
			}
		}
	}()
	return p
}

// closed reports whether the socket died (the reader exited).
func (p *pump) closed() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// linger keeps the socket open and reading for d after a start frame, or
// until the socket dies or ctx ends.
func (p *pump) linger(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-p.done:
	case <-ctx.Done():
	}
}

// stop ends the reader and waits for it so no goroutine outlives the call.
func (p *pump) stop() {
	p.once.Do(func() {
		p.cancel()
		p.conn.CloseNow()
		<-p.done
	})
}

// StartFrameError is returned (with startSent false) when writing the START
// frame itself failed: a timeout, or a socket the printer closed right after the
// bytes were flushed, can surface as a write error even though the frame was
// delivered, so a caller must treat the outcome as unconfirmed and never as
// "not sent" (safety review m1). Every failure BEFORE the start frame (connect,
// the colorMatch write, verifyMap, a socket already closed) is a definite
// not-sent and is returned as a plain error.
type StartFrameError struct{ Err error }

func (e *StartFrameError) Error() string {
	return "crealityws: the start frame may have been delivered: " + e.Err.Error()
}

func (e *StartFrameError) Unwrap() error { return e.Err }

// StartCFSPrint reproduces Creality's Device Manager "file already on the
// printer" path (plan 1.3 and V3; dev_docs/cfs-print-start.md sections 1.1 and
// 1.2) on ONE connection: send colorMatch, call verifyMap (the caller polls
// Moonraker box.map, plan 3.4), and only if that succeeds send multiColorPrint.
// If verifyMap returns an error the connection is closed WITHOUT sending the
// start frame and the result is (false, err); the printer may keep the map
// colorMatch wrote until the next start (print-start open question 6).
//
// path is the file's retGcodeFileInfo2[].path verbatim (K2:
// /mnt/UDISK/printer_data/gcodes/<name>). A reader goroutine answers
// heartbeats for the whole call, including while verifyMap runs; after the
// start frame the socket stays open and reading for one second so the frame
// is not lost to an immediate close. There is no acknowledgement for the
// start frame, so startSent means "written", never "the print began".
func (c *Client) StartCFSPrint(ctx context.Context, path string, items []ColorMatchItem, selfTest bool, verifyMap func(ctx context.Context) error) (startSent bool, err error) {
	if path == "" {
		return false, errors.New("crealityws: start CFS print: empty path")
	}
	if len(items) == 0 {
		return false, errors.New("crealityws: start CFS print: empty colorMatch list")
	}
	if verifyMap == nil {
		return false, errors.New("crealityws: start CFS print: no verifyMap callback; refusing to start without a map check")
	}
	conn, err := c.connect(ctx)
	if err != nil {
		return false, err
	}
	p := startPump(ctx, conn)
	defer p.stop()

	cm := newSetFrame(colorMatchParams{ColorMatch: colorMatchBody{Path: path, List: items}})
	if err := writeJSON(ctx, conn, cm); err != nil {
		return false, fmt.Errorf("crealityws: send colorMatch: %w", err)
	}
	if err := verifyMap(ctx); err != nil {
		return false, err
	}
	if p.closed() {
		return false, errors.New("crealityws: the printer closed the connection before the start frame; the colorMatch map may have been written")
	}
	start := newSetFrame(multiColorPrintParams{MultiColorPrint: multiColorPrintBody{Gcode: path, EnableSelfTest: selfTestInt(selfTest)}})
	if err := writeJSON(ctx, conn, start); err != nil {
		return false, &StartFrameError{Err: fmt.Errorf("send multiColorPrint: %w", err)}
	}
	p.linger(ctx, startLingerDuration)
	return true, nil
}

// StartSpoolPrint sends Creality's single-material start,
// {"method":"set","params":{"opGcodeFile":"printprt:<path>","enableSelfTest":0|1}},
// alone (print-start section 1.4: the safest reproduction for a side-spool
// start), then lingers like StartCFSPrint. Refused by policy in any release
// unless the spool start was exercised in the supervised session (plan V3).
func (c *Client) StartSpoolPrint(ctx context.Context, path string, selfTest bool) (sent bool, err error) {
	if path == "" {
		return false, errors.New("crealityws: start spool print: empty path")
	}
	conn, err := c.connect(ctx)
	if err != nil {
		return false, err
	}
	p := startPump(ctx, conn)
	defer p.stop()

	frame := newSetFrame(opGcodeFileParams{OpGcodeFile: "printprt:" + path, EnableSelfTest: selfTestInt(selfTest)})
	if err := writeJSON(ctx, conn, frame); err != nil {
		return false, &StartFrameError{Err: fmt.Errorf("send opGcodeFile: %w", err)}
	}
	p.linger(ctx, startLingerDuration)
	return true, nil
}

type stopParams struct {
	Stop int `json:"stop"`
}

// Stop sends Creality's own "Stop" message,
// {"method":"set","params":{"stop":1}} (dev_docs/cfs-print-start.md section
// 5), then lingers like the start writes. It exists for exactly one caller
// path: cancelling during the start window, when print_stats has no job yet
// and a Moonraker cancel is not known to stop the self-test (plan 8a.1). The
// policy layer keeps it behind stopDuringStartVerified until the supervised
// session has verified it. There is no acknowledgement frame.
func (c *Client) Stop(ctx context.Context) (sent bool, err error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return false, err
	}
	p := startPump(ctx, conn)
	defer p.stop()

	if err := writeJSON(ctx, conn, newSetFrame(stopParams{Stop: 1})); err != nil {
		return false, fmt.Errorf("crealityws: send stop: %w", err)
	}
	p.linger(ctx, startLingerDuration)
	return true, nil
}
