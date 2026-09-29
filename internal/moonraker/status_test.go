package moonraker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// fixtureServer serves body verbatim for every request, for tests that only
// care about response decoding.
func fixtureServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestServerInfoDecodesRealCapture(t *testing.T) {
	ts := fixtureServer(t, readFixture(t, "server_info.json"))
	c := New(ts.URL, "")
	info, err := c.ServerInfo(context.Background())
	if err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if !info.KlippyConnected {
		t.Fatal("KlippyConnected = false, want true")
	}
	if info.KlippyState != "ready" {
		t.Fatalf("KlippyState = %q, want %q", info.KlippyState, "ready")
	}
	if !info.HasComponent("history") {
		t.Fatal("HasComponent(history) = false, want true")
	}
	if info.HasComponent("update_manager") {
		t.Fatal("HasComponent(update_manager) = true, want false (trimmed on this fork)")
	}
	if info.MoonrakerVersion != "?" {
		t.Fatalf("MoonrakerVersion = %q, want %q (Creality's build does not set it)", info.MoonrakerVersion, "?")
	}
}

func TestPrinterInfoDecodesRealCapture(t *testing.T) {
	ts := fixtureServer(t, readFixture(t, "printer_info.json"))
	c := New(ts.URL, "")
	info, err := c.PrinterInfo(context.Background())
	if err != nil {
		t.Fatalf("PrinterInfo: %v", err)
	}
	if info.Hostname != "K2-5885" {
		t.Fatalf("Hostname = %q, want %q", info.Hostname, "K2-5885")
	}
	if info.State != "ready" {
		t.Fatalf("State = %q, want %q", info.State, "ready")
	}
}

func TestObjectsListDecodesRealCapture(t *testing.T) {
	ts := fixtureServer(t, readFixture(t, "printer_objects_list.json"))
	c := New(ts.URL, "")
	objects, err := c.ObjectsList(context.Background())
	if err != nil {
		t.Fatalf("ObjectsList: %v", err)
	}
	if len(objects) < 100 {
		t.Fatalf("ObjectsList returned %d objects, want 100+", len(objects))
	}
	found := false
	for _, name := range objects {
		if name == "box" {
			found = true
		}
	}
	if !found {
		t.Fatal(`ObjectsList missing "box"`)
	}
}

func TestQueryObjectsBuildsBareAndFilteredQueryKeys(t *testing.T) {
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"result": {"status": {}}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	_, err := c.QueryObjects(context.Background(), map[string][]string{
		"heaters":  nil,
		"extruder": {"target", "temperature"},
	})
	if err != nil {
		t.Fatalf("QueryObjects: %v", err)
	}
	// url.Values.Encode() sorts keys, so this is deterministic. Moonraker's
	// own query parser (components/application.py _object_parser) splits
	// each key=value pair and comma-splits the value, so the object name
	// must be the query key and the field list must be the value, not
	// smashed together into one key.
	want := "extruder=target%2Ctemperature&heaters="
	if gotQuery != want {
		t.Fatalf("query = %q, want %q", gotQuery, want)
	}
}

func TestQueryObjectsReturnsRawPerObjectFromFixture(t *testing.T) {
	ts := fixtureServer(t, readFixture(t, "objects_query_full.json"))
	c := New(ts.URL, "")
	status, err := c.QueryObjects(context.Background(), map[string][]string{"print_stats": nil})
	if err != nil {
		t.Fatalf("QueryObjects: %v", err)
	}
	if _, ok := status["print_stats"]; !ok {
		t.Fatal(`status missing "print_stats"`)
	}
	if _, ok := status["this_object_does_not_exist"]; ok {
		t.Fatal("status should not contain an object that was never in the fixture")
	}
}

// loadFullStatus decodes the shared objects_query_full.json fixture into a
// raw status map, the way QueryObjects would hand it to a caller.
func loadFullStatus(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Result struct {
			Status map[string]json.RawMessage `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(readFixture(t, "objects_query_full.json"), &envelope); err != nil {
		t.Fatalf("decode fixture envelope: %v", err)
	}
	return envelope.Result.Status
}

func requireKey(t *testing.T, status map[string]json.RawMessage, key string) json.RawMessage {
	t.Helper()
	raw, ok := status[key]
	if !ok {
		t.Fatalf("fixture missing object %q", key)
	}
	return raw
}

func TestDecodePrintStatsFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodePrintStats(requireKey(t, status, "print_stats"))
	if err != nil {
		t.Fatalf("DecodePrintStats: %v", err)
	}
	if v.State != "standby" {
		t.Fatalf("State = %q, want %q", v.State, "standby")
	}
	if v.ZPos == 0 {
		t.Fatal("ZPos should be the Creality-only extra field, non-zero in the fixture")
	}
}

func TestDecodePauseResumeFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodePauseResume(requireKey(t, status, "pause_resume"))
	if err != nil {
		t.Fatalf("DecodePauseResume: %v", err)
	}
	if v.IsPaused == nil {
		t.Fatal("IsPaused is nil, want a pointer to false")
	}
	if *v.IsPaused {
		t.Fatal("IsPaused = true, want false")
	}
	if v.ResumeErr == nil || *v.ResumeErr != false {
		t.Fatal("ResumeErr should decode to a pointer to false")
	}
}

func TestDecodeIdleTimeoutFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeIdleTimeout(requireKey(t, status, "idle_timeout"))
	if err != nil {
		t.Fatalf("DecodeIdleTimeout: %v", err)
	}
	if v.State == nil || *v.State != "Ready" {
		t.Fatalf("State = %v, want pointer to %q", v.State, "Ready")
	}
}

func TestDecodeWebhooksFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeWebhooks(requireKey(t, status, "webhooks"))
	if err != nil {
		t.Fatalf("DecodeWebhooks: %v", err)
	}
	if v.State == nil || *v.State != "ready" {
		t.Fatalf("State = %v, want pointer to %q", v.State, "ready")
	}
}

func TestDecodeVirtualSDCardFromRealCaptureIncludesCrealityMetadata(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeVirtualSDCard(requireKey(t, status, "virtual_sdcard"))
	if err != nil {
		t.Fatalf("DecodeVirtualSDCard: %v", err)
	}
	if v.CurPrintData == nil {
		t.Fatal("CurPrintData is nil")
	}
	if v.CurPrintData.Metadata == nil {
		t.Fatal("CurPrintData.Metadata is nil")
	}
	md := v.CurPrintData.Metadata
	if md.Slicer != "Creality" {
		t.Fatalf("Slicer = %q, want %q", md.Slicer, "Creality")
	}
	if md.LayerCount != 149 {
		t.Fatalf("LayerCount = %d, want 149", md.LayerCount)
	}
	if md.ModelInfo == nil || md.ModelInfo.MaterialType != "PLA" {
		t.Fatal("ModelInfo.MaterialType should be PLA")
	}
	if len(md.FilamentUsedG) != 1 || md.FilamentUsedG[0] != "10.42" {
		t.Fatalf("FilamentUsedG = %v, want [\"10.42\"]", md.FilamentUsedG)
	}
}

func TestDecodeExtruderFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeExtruder(requireKey(t, status, "extruder"))
	if err != nil {
		t.Fatalf("DecodeExtruder: %v", err)
	}
	if v.NozzleDiameter != 0.4 {
		t.Fatalf("NozzleDiameter = %v, want 0.4", v.NozzleDiameter)
	}
}

func TestDecodeHeaterBedFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeHeaterBed(requireKey(t, status, "heater_bed"))
	if err != nil {
		t.Fatalf("DecodeHeaterBed: %v", err)
	}
	if v.Temperature == nil || *v.Temperature <= 0 {
		t.Fatal("Temperature should be a pointer to a positive value in the fixture")
	}
}

func TestDecodeGCodeMoveFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeGCodeMove(requireKey(t, status, "gcode_move"))
	if err != nil {
		t.Fatalf("DecodeGCodeMove: %v", err)
	}
	if v.SpeedFactor == nil || *v.SpeedFactor != 1.0 {
		t.Fatalf("SpeedFactor = %v, want pointer to 1.0", v.SpeedFactor)
	}
}

func TestDecodeToolheadFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeToolhead(requireKey(t, status, "toolhead"))
	if err != nil {
		t.Fatalf("DecodeToolhead: %v", err)
	}
	if v.Extruder != "extruder" {
		t.Fatalf("Extruder = %q, want %q", v.Extruder, "extruder")
	}
	if len(v.Position) != 4 {
		t.Fatalf("Position has %d elements, want 4", len(v.Position))
	}
}

func TestDecodeExcludeObjectFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeExcludeObject(requireKey(t, status, "exclude_object"))
	if err != nil {
		t.Fatalf("DecodeExcludeObject: %v", err)
	}
	if v.CurrentObject != nil {
		t.Fatal("CurrentObject should be nil in the fixture")
	}
}

func TestDecodeDisplayStatusFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeDisplayStatus(requireKey(t, status, "display_status"))
	if err != nil {
		t.Fatalf("DecodeDisplayStatus: %v", err)
	}
	if v.Message != nil {
		t.Fatal("Message should be nil in the fixture")
	}
}

func TestDecodeMotorControlFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeMotorControl(requireKey(t, status, "motor_control"))
	if err != nil {
		t.Fatalf("DecodeMotorControl: %v", err)
	}
	if !v.MotorReady {
		t.Fatal("MotorReady = false, want true")
	}
	if !v.Cut.State {
		t.Fatal("Cut.State = false, want true")
	}
}

func TestDecodeOutputPinFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	for _, name := range []string{"output_pin fan0", "output_pin fan1", "output_pin fan2", "output_pin LED"} {
		if _, err := DecodeOutputPin(requireKey(t, status, name)); err != nil {
			t.Fatalf("DecodeOutputPin(%s): %v", name, err)
		}
	}
}

func TestDecodeFilamentRackFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeFilamentRack(requireKey(t, status, "filament_rack"))
	if err != nil {
		t.Fatalf("DecodeFilamentRack: %v", err)
	}
	if v.RemainMaterialVelocity == nil || *v.RemainMaterialVelocity != 575.0 {
		t.Fatalf("RemainMaterialVelocity = %v, want pointer to 575.0", v.RemainMaterialVelocity)
	}
}

func TestDecodeBoxFromRealCaptureAndConnectedHelper(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeBox(requireKey(t, status, "box"))
	if err != nil {
		t.Fatalf("DecodeBox: %v", err)
	}
	if v.State == nil || *v.State != "disconnect" {
		t.Fatalf("State = %v, want pointer to %q", v.State, "disconnect")
	}
	if v.Connected() {
		t.Fatal("Connected() = true for state \"disconnect\", want false")
	}
	other := "anything_else"
	v.State = &other
	if !v.Connected() {
		t.Fatal("Connected() = false for a non-disconnect state, want true (fail closed)")
	}
	v.State = nil
	if !v.Connected() {
		t.Fatal("Connected() = false for a nil (not reported) state, want true (fail closed)")
	}
}

func TestDecodePrinterParamFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodePrinterParam(requireKey(t, status, "gcode_macro PRINTER_PARAM"))
	if err != nil {
		t.Fatalf("DecodePrinterParam: %v", err)
	}
	if v.Fan0Min != 25 || v.Fan1Min != 50 || v.Fan2Min != 100 {
		t.Fatalf("fan mins = %v/%v/%v, want 25/50/100", v.Fan0Min, v.Fan1Min, v.Fan2Min)
	}
	if v.HotendTemp == nil || *v.HotendTemp != 0 {
		t.Fatalf("HotendTemp = %v, want pointer to 0 (no pause stored in this capture)", v.HotendTemp)
	}
}

func TestDecodeProductParamFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeProductParam(requireKey(t, status, "gcode_macro product_param"))
	if err != nil {
		t.Fatalf("DecodeProductParam: %v", err)
	}
	if v.NozzleTemp == nil || *v.NozzleTemp != 300 {
		t.Fatalf("NozzleTemp = %v, want pointer to 300", v.NozzleTemp)
	}
	if v.BedTemp == nil || *v.BedTemp != 100 {
		t.Fatalf("BedTemp = %v, want pointer to 100", v.BedTemp)
	}
}

func TestTypedDecodersToleratesUnknownFields(t *testing.T) {
	raw := json.RawMessage(`{"temperature": 1.0, "target": 2.0, "power": 0.0, "an_unexpected_new_field": "should be ignored"}`)
	v, err := DecodeHeaterBed(raw)
	if err != nil {
		t.Fatalf("DecodeHeaterBed with unknown field: %v", err)
	}
	if v.Temperature == nil || *v.Temperature != 1.0 {
		t.Fatalf("Temperature = %v, want pointer to 1.0", v.Temperature)
	}
}

// --- Presence audit: every field the state engine or the action policy uses
// to gate a write must decode to nil, distinct from a reported zero value,
// when its key is absent (status.go's presence rule, top of file). ---

func TestDecodeCustomMacroFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeCustomMacro(requireKey(t, status, "custom_macro"))
	if err != nil {
		t.Fatalf("DecodeCustomMacro: %v", err)
	}
	if v.LevelingCalibration == nil || *v.LevelingCalibration != 0 {
		t.Fatalf("LevelingCalibration = %v, want pointer to 0", v.LevelingCalibration)
	}
	if v.DefaultBedTemp != 50.0 {
		t.Fatalf("DefaultBedTemp = %v, want 50.0", v.DefaultBedTemp)
	}
}

func TestDecodeVirtualSDCardActivityFlagsFromRealCapture(t *testing.T) {
	status := loadFullStatus(t)
	v, err := DecodeVirtualSDCard(requireKey(t, status, "virtual_sdcard"))
	if err != nil {
		t.Fatalf("DecodeVirtualSDCard: %v", err)
	}
	if v.IsActive == nil || *v.IsActive {
		t.Fatalf("IsActive = %v, want pointer to false", v.IsActive)
	}
	if v.BedMeshCalibrateState == nil || *v.BedMeshCalibrateState {
		t.Fatalf("BedMeshCalibrateState = %v, want pointer to false", v.BedMeshCalibrateState)
	}
}

func TestDecodePauseResumeMissingIsPausedIsNil(t *testing.T) {
	v, err := DecodePauseResume(json.RawMessage(`{"resume_err": false}`))
	if err != nil {
		t.Fatalf("DecodePauseResume: %v", err)
	}
	if v.IsPaused != nil {
		t.Fatalf("IsPaused = %v, want nil when the key is absent", v.IsPaused)
	}
}

func TestDecodeVirtualSDCardMissingActivityFlagsAreNil(t *testing.T) {
	v, err := DecodeVirtualSDCard(json.RawMessage(`{"progress": 0.0}`))
	if err != nil {
		t.Fatalf("DecodeVirtualSDCard: %v", err)
	}
	if v.IsActive != nil {
		t.Fatalf("IsActive = %v, want nil when the key is absent", v.IsActive)
	}
	if v.BedMeshCalibrateState != nil {
		t.Fatalf("BedMeshCalibrateState = %v, want nil when the key is absent", v.BedMeshCalibrateState)
	}
}

func TestDecodeMotorControlMissingIsHomingIsNil(t *testing.T) {
	v, err := DecodeMotorControl(json.RawMessage(`{"motor_ready": true}`))
	if err != nil {
		t.Fatalf("DecodeMotorControl: %v", err)
	}
	if v.IsHoming != nil {
		t.Fatalf("IsHoming = %v, want nil when the key is absent", v.IsHoming)
	}
}

func TestDecodeCustomMacroMissingLevelingCalibrationIsNil(t *testing.T) {
	v, err := DecodeCustomMacro(json.RawMessage(`{"default_bed_temp": 50.0}`))
	if err != nil {
		t.Fatalf("DecodeCustomMacro: %v", err)
	}
	if v.LevelingCalibration != nil {
		t.Fatalf("LevelingCalibration = %v, want nil when the key is absent", v.LevelingCalibration)
	}
}

func TestDecodeIdleTimeoutMissingStateIsNil(t *testing.T) {
	v, err := DecodeIdleTimeout(json.RawMessage(`{"printing_time": 0.0}`))
	if err != nil {
		t.Fatalf("DecodeIdleTimeout: %v", err)
	}
	if v.State != nil {
		t.Fatalf("State = %v, want nil when the key is absent", v.State)
	}
}

func TestDecodeWebhooksMissingStateIsNil(t *testing.T) {
	v, err := DecodeWebhooks(json.RawMessage(`{"state_message": "hello"}`))
	if err != nil {
		t.Fatalf("DecodeWebhooks: %v", err)
	}
	if v.State != nil {
		t.Fatalf("State = %v, want nil when the key is absent", v.State)
	}
}

func TestDecodeExtruderMissingTemperatureAndTargetAreNil(t *testing.T) {
	v, err := DecodeExtruder(json.RawMessage(`{"power": 0.0}`))
	if err != nil {
		t.Fatalf("DecodeExtruder: %v", err)
	}
	if v.Temperature != nil {
		t.Fatalf("Temperature = %v, want nil when the key is absent", v.Temperature)
	}
	if v.Target != nil {
		t.Fatalf("Target = %v, want nil when the key is absent", v.Target)
	}
}

func TestDecodeHeaterBedMissingTemperatureAndTargetAreNil(t *testing.T) {
	v, err := DecodeHeaterBed(json.RawMessage(`{"power": 0.0}`))
	if err != nil {
		t.Fatalf("DecodeHeaterBed: %v", err)
	}
	if v.Temperature != nil {
		t.Fatalf("Temperature = %v, want nil when the key is absent", v.Temperature)
	}
	if v.Target != nil {
		t.Fatalf("Target = %v, want nil when the key is absent", v.Target)
	}
}

func TestDecodeGCodeMoveMissingFactorsAreNil(t *testing.T) {
	v, err := DecodeGCodeMove(json.RawMessage(`{"speed": 100.0}`))
	if err != nil {
		t.Fatalf("DecodeGCodeMove: %v", err)
	}
	if v.SpeedFactor != nil {
		t.Fatalf("SpeedFactor = %v, want nil when the key is absent", v.SpeedFactor)
	}
	if v.ExtrudeFactor != nil {
		t.Fatalf("ExtrudeFactor = %v, want nil when the key is absent", v.ExtrudeFactor)
	}
}

func TestDecodeOutputPinMissingValueIsNil(t *testing.T) {
	v, err := DecodeOutputPin(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("DecodeOutputPin: %v", err)
	}
	if v.Value != nil {
		t.Fatalf("Value = %v, want nil when the key is absent", v.Value)
	}
}

func TestDecodeExcludeObjectDistinguishesMissingFromEmptyLists(t *testing.T) {
	missing, err := DecodeExcludeObject(json.RawMessage(`{"current_object": null}`))
	if err != nil {
		t.Fatalf("DecodeExcludeObject (missing): %v", err)
	}
	if missing.Objects != nil {
		t.Fatalf("Objects = %v, want nil when the key is absent", missing.Objects)
	}
	if missing.ExcludedObjects != nil {
		t.Fatalf("ExcludedObjects = %v, want nil when the key is absent", missing.ExcludedObjects)
	}

	empty, err := DecodeExcludeObject(json.RawMessage(`{"objects": [], "excluded_objects": [], "current_object": null}`))
	if err != nil {
		t.Fatalf("DecodeExcludeObject (empty): %v", err)
	}
	if empty.Objects == nil {
		t.Fatal("Objects is nil, want a non-nil empty slice when the key is present as []")
	}
	if len(empty.Objects) != 0 {
		t.Fatalf("Objects = %v, want empty", empty.Objects)
	}
	if empty.ExcludedObjects == nil {
		t.Fatal("ExcludedObjects is nil, want a non-nil empty slice when the key is present as []")
	}
	if len(empty.ExcludedObjects) != 0 {
		t.Fatalf("ExcludedObjects = %v, want empty", empty.ExcludedObjects)
	}
}

func TestDecodeProductParamMissingLiveCapFieldsAreNil(t *testing.T) {
	v, err := DecodeProductParam(json.RawMessage(`{"bed_size_x": 260}`))
	if err != nil {
		t.Fatalf("DecodeProductParam: %v", err)
	}
	if v.NozzleTemp != nil {
		t.Fatalf("NozzleTemp = %v, want nil when the key is absent", v.NozzleTemp)
	}
	if v.BedTemp != nil {
		t.Fatalf("BedTemp = %v, want nil when the key is absent", v.BedTemp)
	}
}

func TestDecodePrinterParamMissingStoredPauseTargetsAreNil(t *testing.T) {
	v, err := DecodePrinterParam(json.RawMessage(`{"fans": 3}`))
	if err != nil {
		t.Fatalf("DecodePrinterParam: %v", err)
	}
	if v.ZSafePause != nil {
		t.Fatalf("ZSafePause = %v, want nil when the key is absent", v.ZSafePause)
	}
	if v.Fan0Speed != nil {
		t.Fatalf("Fan0Speed = %v, want nil when the key is absent", v.Fan0Speed)
	}
	if v.Fan2Speed != nil {
		t.Fatalf("Fan2Speed = %v, want nil when the key is absent", v.Fan2Speed)
	}
	if v.HotendTemp != nil {
		t.Fatalf("HotendTemp = %v, want nil when the key is absent", v.HotendTemp)
	}
}

func TestDecodeBoxMissingStateIsNilAndConnectedFailsClosed(t *testing.T) {
	v, err := DecodeBox(json.RawMessage(`{"filament": 0}`))
	if err != nil {
		t.Fatalf("DecodeBox: %v", err)
	}
	if v.State != nil {
		t.Fatalf("State = %v, want nil when the key is absent", v.State)
	}
	if !v.Connected() {
		t.Fatal("Connected() = false for a missing state, want true (fail closed)")
	}
}

func TestDecodeFilamentRackMissingFieldsAreNil(t *testing.T) {
	v, err := DecodeFilamentRack(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("DecodeFilamentRack: %v", err)
	}
	if v.Vender != nil || v.ColorValue != nil || v.MaterialType != nil ||
		v.RemainMaterialColor != nil || v.RemainMaterialType != nil || v.RemainMaterialVelocity != nil {
		t.Fatalf("FilamentRack fields = %+v, want all nil when every key is absent", v)
	}
}

// TestDecodeBox_ConnectedCfsCapture decodes the real Moonraker box object
// captured with a CFS connected (2026-09-29): pointer fields carry the
// reported values, same_material decodes from Moonraker's own form and the
// T1 unit exposes its per-slot material codes and colours.
func TestDecodeBox_ConnectedCfsCapture(t *testing.T) {
	var envelope struct {
		Result struct {
			Status map[string]json.RawMessage `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(readFixture(t, "moonraker_box_filament_rack_20260929.json"), &envelope); err != nil {
		t.Fatalf("decode capture: %v", err)
	}
	v, err := DecodeBox(requireKey(t, envelope.Result.Status, "box"))
	if err != nil {
		t.Fatalf("DecodeBox: %v", err)
	}
	if v.State == nil || *v.State != "connect" {
		t.Fatalf("State = %v, want connect", v.State)
	}
	if v.Enable == nil || *v.Enable != 1 || v.FilamentUseup == nil || *v.FilamentUseup != 0 ||
		v.AutoRefill == nil || *v.AutoRefill != 1 || v.Filament == nil || *v.Filament != 1 {
		t.Fatalf("pointer fields = enable %v useup %v auto_refill %v filament %v", v.Enable, v.FilamentUseup, v.AutoRefill, v.Filament)
	}
	if !v.SameMaterialOK || len(v.SameMaterial) != 3 {
		t.Fatalf("same_material ok=%v groups=%d, want ok with 3 groups", v.SameMaterialOK, len(v.SameMaterial))
	}
	if g := v.SameMaterial[0]; g.Code != "000003" || g.Color != "0000000" || g.Name != "PETG" ||
		len(g.Slots) != 2 || g.Slots[0] != "T1A" || g.Slots[1] != "T1B" {
		t.Fatalf("group 0 = %+v", g)
	}
	if v.Map["T1B"] != "T1B" || len(v.Map) != 16 {
		t.Fatalf("map = %v, want the 16-entry identity map", v.Map)
	}
	t1, ok := v.Units["T1"]
	if !ok || t1.State != "connect" {
		t.Fatalf("T1 = %+v ok=%v", t1, ok)
	}
	if len(t1.MaterialType) != 4 || t1.MaterialType[0] != "000003" || t1.ColorValue[2] != "0f4e076" {
		t.Fatalf("T1 arrays = %v / %v", t1.MaterialType, t1.ColorValue)
	}
	if u := v.Units["T2"]; u.State != "None" {
		t.Fatalf("T2 state = %q, want None", u.State)
	}
	if !v.AnyUnitConnected() {
		t.Fatal("AnyUnitConnected = false with T1 connected")
	}
}

// A wrong type on state, enable or filament_useup leaves that pointer nil,
// never zero, and never fails the whole decode (plan 2.1); a malformed
// display array does not fail the decode either.
func TestDecodeBox_WrongTypesFailClosedWithoutFailingTheDecode(t *testing.T) {
	v, err := DecodeBox(json.RawMessage(`{
		"state": 5, "enable": "1", "filament_useup": "0", "auto_refill": 1.5, "filament": true,
		"map": ["not","a","map"], "same_material": [["000003","0000000",["T1A"]]],
		"T1": {"state": "connect", "material_type": "oops", "color_value": ["0000000", 12, null, {"x":1}]},
		"T2": "not an object",
		"T9": {"state": "connect"}}`))
	if err != nil {
		t.Fatalf("DecodeBox failed on wrong types: %v", err)
	}
	if v.State != nil || v.Enable != nil || v.FilamentUseup != nil || v.AutoRefill != nil || v.Filament != nil {
		t.Fatalf("wrong-typed fields must be nil, got state %v enable %v useup %v auto %v filament %v",
			v.State, v.Enable, v.FilamentUseup, v.AutoRefill, v.Filament)
	}
	if v.Map != nil || v.SameMaterial != nil || v.SameMaterialOK {
		t.Fatalf("map/same_material = %v/%v/%v, want nil/nil/false", v.Map, v.SameMaterial, v.SameMaterialOK)
	}
	u, ok := v.Units["T1"]
	if !ok || u.MaterialType != nil {
		t.Fatalf("T1 = %+v ok=%v, want the unit with a nil MaterialType", u, ok)
	}
	if len(u.ColorValue) != 4 || u.ColorValue[0] != "0000000" || u.ColorValue[1] != "12" || u.ColorValue[2] != "" || u.ColorValue[3] != "" {
		t.Fatalf("mixed colour array = %q, want numbers coerced and junk empty", u.ColorValue)
	}
	if _, ok := v.Units["T2"]; ok {
		t.Fatal("a non-object unit must be skipped")
	}
	if _, ok := v.Units["T9"]; ok {
		t.Fatal("only T1..T4 are decoded")
	}
	if !v.Connected() {
		t.Fatal("nil state must still fail closed to connected")
	}
}

func TestBoxAnyUnitConnected(t *testing.T) {
	if (Box{}).AnyUnitConnected() {
		t.Fatal("no units must not count as connected")
	}
	b := Box{Units: map[string]BoxUnit{"T1": {State: "None"}, "T2": {State: "connect"}}}
	if !b.AnyUnitConnected() {
		t.Fatal("T2 connect must count")
	}
}

// R3: the Qmode macro's fields decode independently. flag is strict; an unreadable
// speed_factor never drops it.
func TestDecodeQmodeMacro_FieldsAreIndependent(t *testing.T) {
	v, err := DecodeQmodeMacro(json.RawMessage(`{"flag":1,"speed_factor":"oops"}`))
	if err != nil || v.Flag == nil || *v.Flag != 1 || v.SpeedFactor != nil {
		t.Fatalf("bad speed_factor: %+v %v", v, err)
	}
	v, err = DecodeQmodeMacro(json.RawMessage(`{"flag":0,"speed_factor":1.25}`))
	if err != nil || v.Flag == nil || *v.Flag != 0 || v.SpeedFactor == nil || *v.SpeedFactor != 1.25 {
		t.Fatalf("good values: %+v %v", v, err)
	}
	v, err = DecodeQmodeMacro(json.RawMessage(`{"flag":0}`))
	if err != nil || v.SpeedFactor != nil {
		t.Fatalf("missing speed_factor: %+v %v", v, err)
	}
	if _, err := DecodeQmodeMacro(json.RawMessage(`{"flag":"x"}`)); err == nil {
		t.Fatal("a non-numeric flag must stay a decode error (Silent reads unknown)")
	}
	if _, err := DecodeQmodeMacro(json.RawMessage(`[1]`)); err == nil {
		t.Fatal("a non-object must be an error")
	}
}
