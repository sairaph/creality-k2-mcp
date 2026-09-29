package crealityws

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Test fixtures use INVENTED catalog ids, brands and names (plan decision
// V1: nothing of Creality's catalog is embedded), except the real 3-frame
// write-reply capture replayed by the ModifyMaterial confirmation tests,
// which is unaltered printer output.

func ptrF(f float64) *float64 { return &f }

// readClientJSON reads one text frame from the client and decodes it into a
// generic value for parsed comparison (never byte comparison: plan 1.3).
func readClientJSON(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Errorf("fake server read: %v", err)
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Errorf("fake server decode %q: %v", data, err)
		return nil
	}
	return v
}

// --- get helper and typed reads ---

func TestGet_AnswersHeartbeatSkipsNoiseAndReturnsReplyValue(t *testing.T) {
	gotOK := make(chan string, 1)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		req := readClientJSON(t, ctx, conn)
		want := map[string]any{"method": "get", "params": map[string]any{"boxsInfo": float64(1)}}
		if !reflect.DeepEqual(req, want) {
			t.Errorf("request = %v, want %v", req, want)
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"state":0,"deviceState":0}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"ModeCode":"heart_beat"}`))
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Errorf("read heartbeat answer: %v", err)
			return
		}
		gotOK <- string(data)
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"boxsInfo":{"materialBoxs":[]}}`))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := New(host, port).get(ctx, "boxsInfo", "boxsInfo", time.Second)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(raw) != `{"materialBoxs":[]}` {
		t.Errorf("raw = %s", raw)
	}
	select {
	case s := <-gotOK:
		if s != "ok" {
			t.Errorf("heartbeat answer = %q, want the literal ok", s)
		}
	default:
		t.Error("server never saw the heartbeat answer")
	}
}

func TestGet_TimesOutWithoutReply(t *testing.T) {
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		_, _, _ = conn.Read(ctx)
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"state":0}`))
		_, _, _ = conn.Read(ctx) // hold the connection until the client closes
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := New(host, port).get(ctx, "boxsInfo", "boxsInfo", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no \"boxsInfo\" reply") {
		t.Fatalf("err = %v, want a no-reply error", err)
	}
}

const boxsInfoFixture = `{"boxsInfo":{
 "same_material":[["099001","0aabbcc",[{"boxId":1,"materialId":0},{"boxId":"1","materialId":"1"}],"TESTPLA"],
                  ["099002","0112233",[{"boxId":1,"materialId":2}],"TESTPETG"]],
 "materialBoxs":[
  {"id":0,"state":0,"type":1,"materials":[{"id":0,"vendor":"Acme","type":"PLA","color":"#0ffffff","name":"Acme Side","minTemp":190,"maxTemp":240,"selected":0,"pressure":0.04,"percent":100,"editStatus":1,"rfid":"99101","state":1}]},
  {"id":"1","materialBoxName":"BOX-A","state":1,"type":0,"temp":"29.5","humidity":35,"sn":"x","materials":[
    {"id":0,"vendor":"Acme","type":"PLA","name":"Acme Test PLA","rfid":"99001","color":"#0aabbcc","minTemp":"190","maxTemp":240.5,"pressure":0.04,"percent":100,"state":1,"selected":0,"editStatus":1},
    {"id":1,"vendor":"Acme","type":"PLA","name":"Acme Test PLA","rfid":"99001","color":"#0aabbcc","state":2},
    {"id":2,"type":"PETG","rfid":"99002","color":"#0112233","state":"bad"}]}],
 "colorMatch":[],"enable":1}}`

func TestBoxsInfo_DecodesTolerantly(t *testing.T) {
	var frame map[string]json.RawMessage
	if err := json.Unmarshal([]byte(boxsInfoFixture), &frame); err != nil {
		t.Fatal(err)
	}
	var b BoxsInfo
	if err := json.Unmarshal(frame["boxsInfo"], &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(b.MaterialBoxs) != 2 {
		t.Fatalf("boxes = %d, want 2", len(b.MaterialBoxs))
	}
	side, cfs := b.MaterialBoxs[0], b.MaterialBoxs[1]
	if side.ID != 0 || side.Type != 1 || len(side.Materials) != 1 || side.Materials[0].RFID != "99101" {
		t.Errorf("side spool = %+v", side)
	}
	if cfs.ID != 1 || cfs.Type != 0 || cfs.State != 1 || cfs.Name != "BOX-A" {
		t.Errorf("cfs box = %+v", cfs)
	}
	if cfs.Temp == nil || *cfs.Temp != 29.5 || cfs.Humidity == nil || *cfs.Humidity != 35 {
		t.Errorf("temp/humidity = %v/%v", cfs.Temp, cfs.Humidity)
	}
	s0 := cfs.Materials[0]
	if s0.MinTemp == nil || *s0.MinTemp != 190 || s0.MaxTemp == nil || *s0.MaxTemp != 240.5 {
		t.Errorf("temps = %v/%v (numeric string and number must both decode)", s0.MinTemp, s0.MaxTemp)
	}
	if s1 := cfs.Materials[1]; s1.MinTemp != nil || s1.EditStatus != nil || s1.State == nil || *s1.State != 2 {
		t.Errorf("slot 1 absent fields must stay nil: %+v", s1)
	}
	if s2 := cfs.Materials[2]; s2.State != nil {
		t.Errorf("a wrongly typed state must decode to nil, not 0: %+v", s2.State)
	}
	if !b.SameMaterialOK || len(b.SameMaterial) != 2 {
		t.Fatalf("same_material ok=%v groups=%d", b.SameMaterialOK, len(b.SameMaterial))
	}
	g := b.SameMaterial[0]
	if g.Code != "099001" || g.Color != "0aabbcc" || g.Name != "TESTPLA" ||
		!reflect.DeepEqual(g.Slots, []SlotRef{{1, 0}, {1, 1}}) {
		t.Errorf("group 0 = %+v", g)
	}
}

func TestBoxsInfo_SameMaterialFailureAndMissingBoxes(t *testing.T) {
	cases := map[string]string{
		"group with three parts":   `{"materialBoxs":[],"same_material":[["099001","0aabbcc",[]]]}`,
		"slot ref without boxId":   `{"materialBoxs":[],"same_material":[["099001","0aabbcc",[{"materialId":0}],"X"]]}`,
		"same_material not a list": `{"materialBoxs":[],"same_material":"nope"}`,
		"same_material absent":     `{"materialBoxs":[]}`,
	}
	for name, body := range cases {
		var b BoxsInfo
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Errorf("%s: decode error %v (a bad same_material must only clear SameMaterialOK)", name, err)
			continue
		}
		if b.SameMaterialOK || b.SameMaterial != nil {
			t.Errorf("%s: SameMaterialOK=%v groups=%v, want false/nil", name, b.SameMaterialOK, b.SameMaterial)
		}
	}
	var b BoxsInfo
	if err := json.Unmarshal([]byte(`{"same_material":[]}`), &b); err == nil {
		t.Error("boxsInfo without materialBoxs must fail the decode")
	}
	if err := json.Unmarshal([]byte(`{"materialBoxs":[{"state":1,"type":0}]}`), &b); err == nil {
		t.Error("a box without an id must fail the decode")
	}
	var ok BoxsInfo
	if err := json.Unmarshal([]byte(`{"materialBoxs":[],"same_material":[]}`), &ok); err != nil || !ok.SameMaterialOK {
		t.Errorf("an empty same_material is known-empty: ok=%v err=%v", ok.SameMaterialOK, err)
	}
}

func TestClient_TypedReads(t *testing.T) {
	materials := `{"retMaterials":[
	 {"base":{"id":"99001","brand":"Acme","name":"Acme Test PLA","meterialType":"PLA","minTemp":190,"maxTemp":"240"},"kvParam":{"pressure_advance":"0.04"}},
	 {"base":{"id":"99002","brand":"Acme","name":"Acme Zero","meterialType":"PETG","minTemp":0,"maxTemp":0},"kvParam":{}},
	 {"base":{"brand":"NoId"},"kvParam":{}}]}`
	files := `{"retGcodeFileInfo2":[{"name":"a.gcode","path":"/root/gcodes/a.gcode","file_size":1000,"create_time":"1790000000","material":"PLA;PETG","materialColors":"#000000;#FFFFFF","materialIds":"99001;99002","match":"T1A=T1B "}]}`
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		req := readClientJSON(t, ctx, conn)
		params, _ := req["params"].(map[string]any)
		switch {
		case params["boxsInfo"] != nil:
			_ = conn.Write(ctx, websocket.MessageText, []byte(boxsInfoFixture))
		case params["reqMaterials"] != nil:
			_ = conn.Write(ctx, websocket.MessageText, []byte(materials))
		case params["reqGcodeFile"] != nil:
			_ = conn.Write(ctx, websocket.MessageText, []byte(files))
		default:
			t.Errorf("unexpected request %v", req)
		}
	})
	c := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b, err := c.BoxsInfo(ctx)
	if err != nil || len(b.MaterialBoxs) != 2 {
		t.Fatalf("BoxsInfo: %v %+v", err, b)
	}
	cat, err := c.Materials(ctx)
	if err != nil {
		t.Fatalf("Materials: %v", err)
	}
	if len(cat) != 2 {
		t.Fatalf("catalog = %d entries, want 2 (the entry without an id is dropped)", len(cat))
	}
	if cat[0].ID != "99001" || cat[0].Brand != "Acme" || cat[0].Type != "PLA" || cat[0].MinTemp != 190 || cat[0].MaxTemp != 240 ||
		cat[0].PressureAdvance == nil || *cat[0].PressureAdvance != 0.04 {
		t.Errorf("entry 0 = %+v", cat[0])
	}
	if cat[1].PressureAdvance != nil {
		t.Errorf("an absent pressure_advance must stay nil, got %v", *cat[1].PressureAdvance)
	}
	fs, err := c.GcodeFiles(ctx)
	if err != nil || len(fs) != 1 {
		t.Fatalf("GcodeFiles: %v %+v", err, fs)
	}
	want := GcodeFileInfo{Name: "a.gcode", Path: "/root/gcodes/a.gcode", Material: "PLA;PETG", MaterialColors: "#000000;#FFFFFF",
		MaterialIDs: "99001;99002", Match: "T1A=T1B ", Size: 1000, CreateTime: 1790000000}
	if fs[0] != want {
		t.Errorf("file = %+v, want %+v", fs[0], want)
	}
}

// --- ModifyMaterial ---

// replayFrames reads the real 3-frame write-reply capture (unaltered, BOM
// stripped) as raw text frames.
func replayFrames(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "t1a_write_reply_20260929.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			frames = append(frames, append([]byte(nil), line...))
		}
	}
	if len(frames) != 3 {
		t.Fatalf("capture has %d frames, want 3", len(frames))
	}
	return frames
}

// captureEdit is the write the capture recorded: T1A rewritten to the
// catalog entry 00001 with colour #ff0000 (protocol section 8).
func captureEdit() MaterialEdit {
	return MaterialEdit{
		BoxID: 1, SlotID: 0, BoxType: 0,
		Entry: CatalogEntry{ID: "00001", Brand: "Generic", Name: "Generic PLA", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: ptrF(0.04)},
		Color: "#ff0000",
	}
}

func decodeCaptureBoxs(t *testing.T, frame []byte) BoxsInfo {
	t.Helper()
	var f struct {
		BoxsInfo *BoxsInfo `json:"boxsInfo"`
	}
	if err := json.Unmarshal(frame, &f); err != nil || f.BoxsInfo == nil {
		t.Fatalf("decode boxsInfo frame: %v", err)
	}
	return *f.BoxsInfo
}

// The first pushed frame after the write is mid-write (review M4): the slot
// already shows the new rfid and colour but editStatus is 0 and same_material
// still has the old grouping. It must NOT confirm; the third frame must.
func TestMaterialEditConfirms_ReplaysRealCapture(t *testing.T) {
	frames := replayFrames(t)
	e := captureEdit()
	if e.confirms(decodeCaptureBoxs(t, frames[0])) {
		t.Fatal("frame 1 (mid-write: editStatus 0, old same_material) confirmed the edit")
	}
	if strings.Contains(string(frames[1]), "boxsInfo") {
		t.Fatal("frame 2 should be the materialState frame")
	}
	if !e.confirms(decodeCaptureBoxs(t, frames[2])) {
		t.Fatal("frame 3 (editStatus 1, regrouped same_material) did not confirm the edit")
	}
}

func TestMaterialEditConfirms_EachConditionIsRequired(t *testing.T) {
	frames := replayFrames(t)
	e := captureEdit()
	good := func() BoxsInfo { return decodeCaptureBoxs(t, frames[2]) }
	slot := func(b *BoxsInfo) *SlotMaterial { return &b.MaterialBoxs[1].Materials[0] }
	cases := map[string]func(b *BoxsInfo){
		"wrong rfid":           func(b *BoxsInfo) { slot(b).RFID = "00003" },
		"wrong colour":         func(b *BoxsInfo) { slot(b).Color = "#0000000" },
		"editStatus 0":         func(b *BoxsInfo) { z := 0; slot(b).EditStatus = &z },
		"editStatus absent":    func(b *BoxsInfo) { slot(b).EditStatus = nil },
		"same_material bad":    func(b *BoxsInfo) { b.SameMaterialOK = false },
		"slot not in a group":  func(b *BoxsInfo) { b.SameMaterial = b.SameMaterial[1:] },
		"group code differs":   func(b *BoxsInfo) { b.SameMaterial[0].Code = "000003" },
		"group colour differs": func(b *BoxsInfo) { b.SameMaterial[0].Color = "0000000" },
		"slot absent":          func(b *BoxsInfo) { b.MaterialBoxs[1].Materials = b.MaterialBoxs[1].Materials[1:] },
	}
	for name, mutate := range cases {
		b := good()
		mutate(&b)
		if e.confirms(b) {
			t.Errorf("%s: still confirmed", name)
		}
	}
	// Colour comparison is case-insensitive.
	b := good()
	slot(&b).Color = "#0FF0000"
	b.SameMaterial[0].Color = "0FF0000"
	if !e.confirms(b) {
		t.Error("an upper-case colour must still confirm")
	}
}

func TestModifyMaterial_SendsFixedShapeAndReturnsOnConfirmingFrame(t *testing.T) {
	frames := replayFrames(t)
	sentFrame := make(chan map[string]any, 1)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		sentFrame <- readClientJSON(t, ctx, conn)
		for i, f := range frames {
			_ = conn.Write(ctx, websocket.MessageText, f)
			if i == 0 {
				// A heartbeat between frames is answered, not fatal.
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"ModeCode":"heart_beat"}`))
			}
		}
		_, _, _ = conn.Read(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, sent, err := New(host, port).ModifyMaterial(ctx, captureEdit())
	if err != nil || !sent {
		t.Fatalf("ModifyMaterial: sent=%v err=%v", sent, err)
	}
	if got == nil {
		t.Fatal("no confirming frame returned")
	}
	if !got.SameMaterialOK || len(got.SameMaterial) != 4 {
		t.Fatalf("returned frame is not frame 3 (regrouped, 4 groups): %+v", got.SameMaterial)
	}
	want := map[string]any{"method": "set", "params": map[string]any{"modifyMaterial": map[string]any{
		"boxId": float64(1), "id": float64(0), "boxType": float64(0),
		"rfid": "00001", "vendor": "Generic", "type": "PLA", "name": "Generic PLA",
		"color": "#0ff0000", "minTemp": 190.00000001, "maxTemp": 240.00000001, "pressure": 0.04,
	}}}
	if diff := <-sentFrame; !reflect.DeepEqual(diff, want) {
		t.Errorf("wire frame = %v\nwant %v", diff, want)
	}
}

func TestModifyMaterial_UnconfirmedIsSentNotError(t *testing.T) {
	frames := replayFrames(t)
	old := modifyConfirmTimeout
	modifyConfirmTimeout = 300 * time.Millisecond
	t.Cleanup(func() { modifyConfirmTimeout = old })

	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		_, _, _ = conn.Read(ctx)
		_ = conn.Write(ctx, websocket.MessageText, frames[0]) // only the mid-write frame
		_, _, _ = conn.Read(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, sent, err := New(host, port).ModifyMaterial(ctx, captureEdit())
	if err != nil || !sent || got != nil {
		t.Fatalf("got=%v sent=%v err=%v, want (nil, true, nil)", got, sent, err)
	}
}

func TestModifyMaterial_InvalidEditNeverConnects(t *testing.T) {
	var conns atomic.Int32
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) { conns.Add(1) })
	base := captureEdit()
	noPA := base
	noPA.Entry.PressureAdvance = nil
	badColor := base
	badColor.Color = "ff0000"
	badBox := base
	badBox.BoxID = 9
	badType := base
	badType.BoxType = 2
	noID := base
	noID.Entry.ID = ""
	for name, e := range map[string]MaterialEdit{"nil pressure advance": noPA, "colour without #": badColor, "box out of range": badBox, "bad boxType": badType, "no id": noID} {
		_, sent, err := New(host, port).ModifyMaterial(context.Background(), e)
		if err == nil || sent {
			t.Errorf("%s: sent=%v err=%v, want an error and nothing sent", name, sent, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := conns.Load(); n != 0 {
		t.Errorf("%d connections opened for invalid edits, want 0", n)
	}
}

// --- starts ---

func testItems() []ColorMatchItem {
	return []ColorMatchItem{
		{ID: "T1A", Type: "PLA", Color: "#aabbcc", BoxID: 1, MaterialID: 2},
		{ID: "T1B", Type: "PETG", Color: "#112233", BoxID: 1, MaterialID: 0},
	}
}

const testPath = "/root/gcodes/Part.gcode"

func shrinkLinger(t *testing.T, d time.Duration) {
	t.Helper()
	old := startLingerDuration
	startLingerDuration = d
	t.Cleanup(func() { startLingerDuration = old })
}

func TestStartCFSPrint_FrameOrderAndShape(t *testing.T) {
	shrinkLinger(t, 10*time.Millisecond)
	frames := make(chan map[string]any, 3)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		for range 2 {
			frames <- readClientJSON(t, ctx, conn)
		}
		_, _, _ = conn.Read(ctx)
	})
	verified := false
	sent, err := New(host, port).StartCFSPrint(context.Background(), testPath, testItems(), true, func(ctx context.Context) error {
		verified = true
		return nil
	})
	if err != nil || !sent || !verified {
		t.Fatalf("sent=%v err=%v verified=%v", sent, err, verified)
	}
	wantMatch := map[string]any{"method": "set", "params": map[string]any{"colorMatch": map[string]any{
		"path": testPath,
		"list": []any{
			map[string]any{"id": "T1A", "type": "PLA", "color": "#aabbcc", "boxId": float64(1), "materialId": float64(2)},
			map[string]any{"id": "T1B", "type": "PETG", "color": "#112233", "boxId": float64(1), "materialId": float64(0)},
		},
	}}}
	wantStart := map[string]any{"method": "set", "params": map[string]any{"multiColorPrint": map[string]any{
		"gcode": testPath, "enableSelfTest": float64(1),
	}}}
	if got := <-frames; !reflect.DeepEqual(got, wantMatch) {
		t.Errorf("frame 1 = %v\nwant %v", got, wantMatch)
	}
	if got := <-frames; !reflect.DeepEqual(got, wantStart) {
		t.Errorf("frame 2 = %v\nwant %v", got, wantStart)
	}
}

func TestStartCFSPrint_VerifyFailureSendsNoStartFrame(t *testing.T) {
	shrinkLinger(t, 10*time.Millisecond)
	var multi atomic.Int32
	var total atomic.Int32
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			total.Add(1)
			if strings.Contains(string(data), "multiColorPrint") {
				multi.Add(1)
			}
		}
	})
	wantErr := "map mismatch"
	sent, err := New(host, port).StartCFSPrint(context.Background(), testPath, testItems(), false, func(ctx context.Context) error {
		return errString(wantErr)
	})
	if sent || err == nil || err.Error() != wantErr {
		t.Fatalf("sent=%v err=%v, want (false, %q)", sent, err, wantErr)
	}
	waitUntil(t, func() bool { return total.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if multi.Load() != 0 || total.Load() != 1 {
		t.Fatalf("server saw %d frames, %d multiColorPrint; want only the colorMatch", total.Load(), multi.Load())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

// The reader goroutine must answer heartbeats while verifyMap is running
// (review-2 MF7): the callback blocks until the server has seen "ok".
func TestStartCFSPrint_AnswersHeartbeatWhileVerifyMapRuns(t *testing.T) {
	shrinkLinger(t, 10*time.Millisecond)
	sawOK := make(chan struct{})
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		_, _, _ = conn.Read(ctx) // colorMatch
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"ModeCode":"heart_beat"}`))
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if string(data) == "ok" {
				select {
				case <-sawOK:
				default:
					close(sawOK)
				}
			}
		}
	})
	sent, err := New(host, port).StartCFSPrint(context.Background(), testPath, testItems(), false, func(ctx context.Context) error {
		select {
		case <-sawOK:
			return nil
		case <-time.After(2 * time.Second):
			return errString("heartbeat was not answered while verifyMap ran")
		}
	})
	if err != nil || !sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
}

// After the start frame the socket must stay open for the linger interval
// (review-2 MF7), measured on the server side at the real 1 s default.
func TestStartCFSPrint_KeepsSocketOpenForOneSecondAfterStartFrame(t *testing.T) {
	if startLingerDuration != time.Second {
		t.Fatalf("default linger = %s, want 1s (plan 8a.7)", startLingerDuration)
	}
	type times struct{ start, closed time.Time }
	res := make(chan times, 1)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		_, _, _ = conn.Read(ctx) // colorMatch
		_, _, _ = conn.Read(ctx) // start
		tm := times{start: time.Now()}
		_, _, _ = conn.Read(ctx) // returns when the client closes
		tm.closed = time.Now()
		res <- tm
	})
	sent, err := New(host, port).StartCFSPrint(context.Background(), testPath, testItems(), false, func(ctx context.Context) error { return nil })
	if err != nil || !sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	tm := <-res
	if d := tm.closed.Sub(tm.start); d < 900*time.Millisecond {
		t.Errorf("socket closed %s after the start frame, want at least about 1s", d)
	}
}

func TestStartCFSPrint_SocketClosedBeforeStartFrameIsNotSent(t *testing.T) {
	shrinkLinger(t, 10*time.Millisecond)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background()) // colorMatch
		conn.Close(websocket.StatusNormalClosure, "bye")
	})
	sent, err := New(host, port).StartCFSPrint(context.Background(), testPath, testItems(), false, func(ctx context.Context) error {
		// Give the reader time to notice the close.
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	if sent || err == nil {
		t.Fatalf("sent=%v err=%v, want (false, error)", sent, err)
	}
}

func TestStartCFSPrint_RefusesBadInputBeforeConnecting(t *testing.T) {
	var conns atomic.Int32
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) { conns.Add(1) })
	c := New(host, port)
	ok := func(context.Context) error { return nil }
	if sent, err := c.StartCFSPrint(context.Background(), "", testItems(), false, ok); sent || err == nil {
		t.Error("empty path accepted")
	}
	if sent, err := c.StartCFSPrint(context.Background(), testPath, nil, false, ok); sent || err == nil {
		t.Error("empty list accepted")
	}
	if sent, err := c.StartCFSPrint(context.Background(), testPath, testItems(), false, nil); sent || err == nil {
		t.Error("nil verifyMap accepted")
	}
	if sent, err := c.StartSpoolPrint(context.Background(), "", false); sent || err == nil {
		t.Error("empty spool path accepted")
	}
	time.Sleep(50 * time.Millisecond)
	if conns.Load() != 0 {
		t.Errorf("%d connections opened, want 0", conns.Load())
	}
}

func TestStartSpoolPrint_Frame(t *testing.T) {
	shrinkLinger(t, 10*time.Millisecond)
	got := make(chan map[string]any, 1)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		got <- readClientJSON(t, ctx, conn)
		_, _, _ = conn.Read(ctx)
	})
	sent, err := New(host, port).StartSpoolPrint(context.Background(), testPath, false)
	if err != nil || !sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	want := map[string]any{"method": "set", "params": map[string]any{
		"opGcodeFile": "printprt:" + testPath, "enableSelfTest": float64(0),
	}}
	if g := <-got; !reflect.DeepEqual(g, want) {
		t.Errorf("frame = %v\nwant %v", g, want)
	}
}

func TestStop_Frame(t *testing.T) {
	shrinkLinger(t, 10*time.Millisecond)
	got := make(chan map[string]any, 1)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		got <- readClientJSON(t, ctx, conn)
		_, _, _ = conn.Read(ctx)
	})
	sent, err := New(host, port).Stop(context.Background())
	if err != nil || !sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	want := map[string]any{"method": "set", "params": map[string]any{"stop": float64(1)}}
	if g := <-got; !reflect.DeepEqual(g, want) {
		t.Errorf("frame = %v\nwant %v", g, want)
	}
}

func TestStartFrameErrorIsDistinguishableAndUnwraps(t *testing.T) {
	inner := context.DeadlineExceeded
	var err error = &StartFrameError{Err: inner}
	var sfe *StartFrameError
	if !errors.As(err, &sfe) || !errors.Is(err, inner) || !strings.Contains(err.Error(), "may have been delivered") {
		t.Fatalf("err = %v", err)
	}
}

func TestMaterialEdit_ValidateBoxAndCatalogFields(t *testing.T) {
	base := captureEdit()
	cases := map[string]func(e *MaterialEdit){
		"CFS slot in unit 0":         func(e *MaterialEdit) { e.BoxID = 0 },
		"side spool in unit 1":       func(e *MaterialEdit) { e.BoxType, e.BoxID = 1, 1 },
		"side spool slot 2":          func(e *MaterialEdit) { e.BoxType, e.BoxID, e.SlotID = 1, 0, 2 },
		"catalog entry with no type": func(e *MaterialEdit) { e.Entry.Type = "" },
		"catalog entry with no name": func(e *MaterialEdit) { e.Entry.Name = " " },
	}
	for name, mutate := range cases {
		e := base
		mutate(&e)
		if err := e.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	side := base
	side.BoxType, side.BoxID, side.SlotID = 1, 0, 0
	if err := side.validate(); err != nil {
		t.Errorf("valid side spool edit: %v", err)
	}
}

// Side spool edits confirm without same_material (supervised session
// 2026-09-29): rfid, colour and editStatus 1 are enough, and the first push
// (editStatus 0) still does not confirm.
func TestMaterialEditConfirms_SideSpoolNeedsNoSameMaterial(t *testing.T) {
	e := MaterialEdit{BoxID: 0, SlotID: 0, BoxType: 1,
		Entry: CatalogEntry{ID: "99003", Brand: "Acme", Name: "Acme Side PLA", Type: "PLA", MinTemp: 190, MaxTemp: 240, PressureAdvance: ptrF(0.04)}, Color: "#112233"}
	one, zero := 1, 0
	frame := func(edit *int, rfid string, groups bool) BoxsInfo {
		b := BoxsInfo{MaterialBoxs: []MaterialBox{{ID: 0, Type: 1, Materials: []SlotMaterial{{ID: 0, RFID: rfid, Color: "#0112233", EditStatus: edit, State: &one}}}}}
		if groups {
			b.SameMaterialOK = true
		}
		return b
	}
	if !e.Confirms(frame(&one, "99003", false)) {
		t.Fatal("rfid, colour and editStatus 1 must confirm a side spool edit without same_material")
	}
	if e.Confirms(frame(&zero, "99003", false)) {
		t.Fatal("the first push (editStatus 0) must not confirm")
	}
	if e.Confirms(frame(&one, "99001", false)) {
		t.Fatal("a different rfid must not confirm")
	}
	// A CFS slot still needs the regrouping.
	cfs := captureEdit()
	b := decodeCaptureBoxs(t, replayFrames(t)[2])
	b.SameMaterialOK = false
	if cfs.Confirms(b) {
		t.Fatal("a CFS slot must still require same_material")
	}
}

// SetSpeedMode sends exactly one fixed-shape frame per call and never a
// setFeedratePct (plan-v0.3.0.md 2a.2).
func TestSetSpeedMode_Frames(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		mode float64
	}{{true, 1}, {false, 0}} {
		shrinkLinger(t, 10*time.Millisecond)
		got := make(chan map[string]any, 4)
		host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
			ctx := context.Background()
			got <- readClientJSON(t, ctx, conn)
			_, _, _ = conn.Read(ctx)
		})
		sent, err := New(host, port).SetSpeedMode(context.Background(), tc.on)
		if err != nil || !sent {
			t.Fatalf("on=%v: sent=%v err=%v", tc.on, sent, err)
		}
		want := map[string]any{"method": "set", "params": map[string]any{"speedMode": tc.mode}}
		if g := <-got; !reflect.DeepEqual(g, want) {
			t.Errorf("on=%v: frame = %v\nwant %v", tc.on, g, want)
		}
		select {
		case extra := <-got:
			t.Errorf("on=%v: unexpected second frame %v", tc.on, extra)
		default:
		}
	}
}

func TestSetSpeedMode_ConnectFailureIsNotSent(t *testing.T) {
	sent, err := New("127.0.0.1", 1).SetSpeedMode(context.Background(), true)
	if err == nil || sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
}

func TestBuildStatus_SpeedModeAndFeedrateArePresenceAware(t *testing.T) {
	s := buildStatus(map[string]any{"speedMode": float64(1), "curFeedratePct": float64(50)})
	if !s.SpeedMode.Present || s.SpeedMode.Value != 1 || !s.CurFeedrate.Present || s.CurFeedrate.Value != 50 {
		t.Fatalf("status = %+v", s)
	}
	if e := buildStatus(map[string]any{}); e.SpeedMode.Present || e.CurFeedrate.Present {
		t.Fatalf("absent fields reported present: %+v", e)
	}
}
