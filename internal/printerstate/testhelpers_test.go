package printerstate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// fixedNow is a stable reference time so every synthetic Snapshot in this
// package's tests is deterministic.
var fixedNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// readTestdata reads a file from this package's testdata directory. The real
// printer captures live there verbatim (unaltered copies of the live reads),
// so the tests run anywhere the repository is checked out.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}

// idleObjectsRaw decodes the real idle-baseline capture
// (testdata/printer_objects_query_all.json) into the same
// map[string]json.RawMessage shape QueryObjects returns, so it can be run
// through Snapshot.decodeObjects exactly as a live call would.
func idleObjectsRaw(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	data := readTestdata(t, "printer_objects_query_all.json")
	var envelope struct {
		Result struct {
			Status map[string]json.RawMessage `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode printer_objects_query_all.json: %v", err)
	}
	return envelope.Result.Status
}

// mergeRaw returns a shallow copy of base with override's top-level keys
// layered on top, mirroring how control_test_20260928.log's later polls only
// report the objects that actually changed since the previous one.
func mergeRaw(base map[string]json.RawMessage, overrideJSON string) map[string]json.RawMessage {
	var override map[string]json.RawMessage
	if err := json.Unmarshal([]byte(overrideJSON), &override); err != nil {
		panic("mergeRaw: bad override JSON: " + err.Error())
	}
	merged := make(map[string]json.RawMessage, len(base)+len(override))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range override {
		merged[k] = v
	}
	return merged
}

// idleServerInfo is the real idle server/info shape (klippy_connected true,
// klippy_state ready), matching every capture in this project
// (references/analysis/11-state-model.md section 1.1 row 2).
func idleServerInfo() moonraker.ServerInfoResult {
	return moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"}
}

// idleWS9999Status mirrors internal/crealityws/testdata/idle_full_push.json
// (the real idle first-state push): state 0, deviceState 0, cfsConnect 0,
// upgradeStatus 0, repoPlrStatus 0, powerLoss 1, hostname K2-5885, model
// F021, printId "6ab97686faee9c1a19d178e6".
func idleWS9999Status() crealityws.Status {
	return crealityws.Status{
		State:          crealityws.Int{Value: 0, Present: true},
		DeviceState:    crealityws.Int{Value: 0, Present: true},
		FeedState:      crealityws.Int{Value: 0, Present: true},
		UpgradeStatus:  crealityws.Int{Value: 0, Present: true},
		RepoPlrStatus:  crealityws.Int{Value: 0, Present: true},
		PowerLoss:      crealityws.Int{Value: 1, Present: true},
		MaterialStatus: crealityws.Int{Value: 0, Present: true},
		CfsConnect:     crealityws.Int{Value: 0, Present: true},
		LightSw:        crealityws.Int{Value: 0, Present: true},
		Err:            crealityws.StatusErr{Present: true},
		PrintID:        crealityws.Str{Value: "6ab97686faee9c1a19d178e6", Present: true},
		Model:          "F021",
		Hostname:       "K2-5885",
		Raw:            map[string]any{"bedTempAutoPid": float64(0), "nozzleTempAutoPid": float64(0)},
	}
}

// printingWS9999Status mirrors control_test_20260928.log line 33 (the push
// logged right after POST /printer/print/start): only state, deviceState and
// printProgress were recorded in that log line, so only those fields are
// marked Present; this project never captured whether cfsConnect/
// upgradeStatus/repoPlrStatus were re-sent unchanged in the same push.
func printingWS9999Status() crealityws.Status {
	return crealityws.Status{
		State:         crealityws.Int{Value: 1, Present: true},
		DeviceState:   crealityws.Int{Value: 1, Present: true},
		PrintProgress: 9,
	}
}

// pausedWS9999Status mirrors control_test_20260928.log line 39.
func pausedWS9999Status() crealityws.Status {
	return crealityws.Status{
		State:         crealityws.Int{Value: 5, Present: true},
		DeviceState:   crealityws.Int{Value: 1, Present: true},
		PrintProgress: 14,
	}
}

// cancelledWS9999Status mirrors control_test_20260928.log line 45.
func cancelledWS9999Status() crealityws.Status {
	return crealityws.Status{
		State:         crealityws.Int{Value: 4, Present: true},
		DeviceState:   crealityws.Int{Value: 0, Present: true},
		PrintProgress: 14,
	}
}

// printingOverrideJSON is control_test_20260928.log line 32's status payload
// verbatim (logged right after POST /printer/print/start).
const printingOverrideJSON = `{"print_stats": {"filename": "k2mcp_test.gcode", "total_duration": 4.20229762699455, "print_duration": 0.0, "filament_used": 0.0, "state": "printing", "message": "", "info": {"total_layer": null, "current_layer": null}, "power_loss": 0, "z_pos": 127.99749555555556}, "virtual_sdcard": {"file_path": "/mnt/UDISK/printer_data/gcodes/k2mcp_test.gcode", "progress": 0.09594822734946538, "is_active": true, "file_position": 682, "file_size": 7108, "first_layer_stop": false, "layer": 0, "layer_count": 0, "run_dis": -250.0, "bed_mesh_calibate_state": false, "klipper_capture": false, "klipper_capture_cnt": 0, "cur_print_data": {"end_time": 1790554656.3493848, "filament_used": 0.0, "filename": "k2mcp_test.gcode", "metadata": {"size": 7108, "modified": 1790554651.5936701, "uuid": "e091d41a-40fc-4ea7-86fc-93cdf76b9549", "slicer": "Unknown", "gcode_start_byte": 247, "gcode_end_byte": 7077, "model_info": {"multicolor_method": 0}}, "print_duration": 0.0, "start_time": 1790554656.2828565, "status": "in_progress", "total_duration": 0.0}}, "pause_resume": {"is_paused": false, "resume_err": false}, "idle_timeout": {"state": "Printing", "printing_time": 4.159019336046185}, "webhooks": {"state": "ready", "state_message": "Printer is ready"}, "exclude_object": {"objects": [{"name": "PART_A", "center": [100, 100], "polygon": [[90, 90], [110, 90], [110, 110], [90, 110]]}, {"name": "PART_B", "center": [160, 100], "polygon": [[150, 90], [170, 90], [170, 110], [150, 110]]}], "excluded_objects": [], "current_object": "PART_B"}, "display_status": {"progress": 0.09594822734946538, "message": "k2mcp test"}}`

// pausedOverrideJSON is control_test_20260928.log line 38's status payload
// verbatim (logged right after POST /printer/print/pause).
const pausedOverrideJSON = `{"print_stats": {"filename": "k2mcp_test.gcode", "total_duration": 16.48517171596177, "print_duration": 0.0, "filament_used": 0.0, "state": "paused", "message": "", "info": {"total_layer": null, "current_layer": null}, "power_loss": 0, "z_pos": 127.99749555555556}, "virtual_sdcard": {"file_path": "/mnt/UDISK/printer_data/gcodes/k2mcp_test.gcode", "progress": 0.15081598199212154, "is_active": false, "file_position": 1072, "file_size": 7108, "first_layer_stop": false, "layer": 0, "layer_count": 0, "run_dis": -250.0, "bed_mesh_calibate_state": false, "klipper_capture": false, "klipper_capture_cnt": 0, "cur_print_data": {"end_time": 1790554656.3493848, "filament_used": 0.0, "filename": "k2mcp_test.gcode", "metadata": {"size": 7108, "modified": 1790554651.5936701, "uuid": "e091d41a-40fc-4ea7-86fc-93cdf76b9549", "slicer": "Unknown", "gcode_start_byte": 247, "gcode_end_byte": 7077, "model_info": {"multicolor_method": 0}}, "print_duration": 0.0, "start_time": 1790554656.2828565, "status": "in_progress", "total_duration": 0.0}}, "pause_resume": {"is_paused": true, "resume_err": false}, "idle_timeout": {"state": "Ready", "printing_time": 0.0}, "webhooks": {"state": "ready", "state_message": "Printer is ready"}, "exclude_object": {"objects": [{"name": "PART_A", "center": [100, 100], "polygon": [[90, 90], [110, 90], [110, 110], [90, 110]]}, {"name": "PART_B", "center": [160, 100], "polygon": [[150, 90], [170, 90], [170, 110], [150, 110]]}], "excluded_objects": ["PART_B"], "current_object": "PART_A"}, "display_status": {"progress": 0.15081598199212154, "message": "k2mcp test"}, "extruder": {"temperature": 66.63, "target": 140.0, "power": 1.0, "can_extrude": false, "extrude_below_min_temp_err_is_report": false, "nozzle_diameter": 0.4, "pressure_advance": 0.04, "smooth_time": 0.04}, "heater_bed": {"temperature": 30.99, "target": 0.0, "power": 0.0}, "output_pin fan0": {"value": 0.0}, "output_pin fan2": {"value": 0.0}}`

// cancelledOverrideJSON is control_test_20260928.log line 44's status
// payload verbatim (logged right after POST /printer/print/cancel).
const cancelledOverrideJSON = `{"print_stats": {"filename": "k2mcp_test.gcode", "total_duration": 19.953510342980735, "print_duration": 0.0, "filament_used": 0.0, "state": "cancelled", "message": "", "info": {"total_layer": null, "current_layer": null}, "power_loss": 0, "z_pos": 127.99749555555556}, "virtual_sdcard": {"file_path": null, "progress": 0.0, "is_active": false, "file_position": 0.0, "file_size": 0.0, "first_layer_stop": false, "layer": 0, "layer_count": 0, "run_dis": -250.0, "bed_mesh_calibate_state": false, "klipper_capture": false, "klipper_capture_cnt": 0, "cur_print_data": {"end_time": 1790554676.3475306, "filament_used": 0.0, "filename": "k2mcp_test.gcode", "metadata": {"size": 7108, "modified": 1790554651.5936701, "uuid": "e091d41a-40fc-4ea7-86fc-93cdf76b9549", "slicer": "Unknown", "gcode_start_byte": 247, "gcode_end_byte": 7077, "model_info": {"multicolor_method": 0}}, "print_duration": 0.0, "start_time": 1790554656.2828565, "status": "cancelled", "total_duration": 0.0}}, "pause_resume": {"is_paused": false, "resume_err": false}, "idle_timeout": {"state": "Ready", "printing_time": 0.0}, "webhooks": {"state": "ready", "state_message": "Printer is ready"}, "exclude_object": {"objects": [], "excluded_objects": [], "current_object": null}, "display_status": {"progress": 0.0, "message": "k2mcp test"}, "extruder": {"temperature": 86.75, "target": 0.0, "power": 0.0, "can_extrude": false, "extrude_below_min_temp_err_is_report": false, "nozzle_diameter": 0.4, "pressure_advance": 0.04, "smooth_time": 0.04}, "heater_bed": {"temperature": 30.61, "target": 0.0, "power": 0.0}, "output_pin fan0": {"value": 0.5472510572856594}, "output_pin fan1": {"value": 0.0}, "output_pin fan2": {"value": 0.0}}`

// buildSnapshot decodes raw into a Snapshot the way Take does, with the given
// server/info and 9999 status attached and Taken fixed for determinism.
func buildSnapshot(t *testing.T, raw map[string]json.RawMessage, serverInfo moonraker.ServerInfoResult, ws crealityws.Status, ws9999Reachable bool) Snapshot {
	t.Helper()
	snap := Snapshot{
		Printer: domain.Printer{ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.102", Hostname: "K2-5885"},
		Taken:   fixedNow,
		// The registry Hostname above ("K2-5885") must be corroborated by a
		// live printer/info read (review backlog item 24) or every one of
		// these real-capture reconstructions would derive as
		// identity_unverified instead of the state each capture actually
		// pins; live and registry hostnames genuinely agree in every real
		// capture this project has, so this mirrors that.
		PrinterInfo:     moonraker.PrinterInfoResult{State: "ready", Hostname: "K2-5885"},
		ServerInfo:      serverInfo,
		WS9999:          ws,
		WS9999Reachable: ws9999Reachable,
	}
	snap.decodeObjects(raw)
	return snap
}

// idleSnapshot is the real, unaltered idle baseline
// (printer_objects_query_all.json + the idle 9999 push), decoded exactly as
// a live Take would decode it.
func idleSnapshot(t *testing.T) Snapshot {
	return buildSnapshot(t, idleObjectsRaw(t), idleServerInfo(), idleWS9999Status(), true)
}

// printingSnapshot reconstructs the live "printing" moment from
// control_test_20260928.log by layering that log's real status payload over
// the idle baseline for every object the log excerpt did not re-report
// (motor_control, custom_macro, toolhead, filament_rack, box, PRINTER_PARAM,
// product_param), matching the same reconstruction approach
// internal/crealityws/fixtures_test.go uses for its own push fixtures.
func printingSnapshot(t *testing.T) Snapshot {
	raw := mergeRaw(idleObjectsRaw(t), printingOverrideJSON)
	return buildSnapshot(t, raw, idleServerInfo(), printingWS9999Status(), true)
}

func pausedSnapshot(t *testing.T) Snapshot {
	raw := mergeRaw(idleObjectsRaw(t), pausedOverrideJSON)
	return buildSnapshot(t, raw, idleServerInfo(), pausedWS9999Status(), true)
}

func cancelledSnapshot(t *testing.T) Snapshot {
	raw := mergeRaw(idleObjectsRaw(t), cancelledOverrideJSON)
	return buildSnapshot(t, raw, idleServerInfo(), cancelledWS9999Status(), true)
}

// --- fakes for Take's dependencies ---

type fakeMoonrakerClient struct {
	serverInfo    moonraker.ServerInfoResult
	serverInfoErr error

	printerInfo    moonraker.PrinterInfoResult
	printerInfoErr error

	objects    map[string]json.RawMessage
	objectsErr error

	history    moonraker.HistoryList
	historyErr error

	gcodeStore    []moonraker.GCodeStoreEntry
	gcodeStoreErr error

	// delay, if set, is slept in each method before returning, to test that
	// Take runs its calls concurrently rather than sequentially.
	delay time.Duration
}

func (f *fakeMoonrakerClient) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.serverInfo, f.serverInfoErr
}

func (f *fakeMoonrakerClient) PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.printerInfo, f.printerInfoErr
}

func (f *fakeMoonrakerClient) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.objects, f.objectsErr
}

func (f *fakeMoonrakerClient) HistoryList(ctx context.Context, limit, start int) (moonraker.HistoryList, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.history, f.historyErr
}

func (f *fakeMoonrakerClient) GCodeStore(ctx context.Context, count int) ([]moonraker.GCodeStoreEntry, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.gcodeStore, f.gcodeStoreErr
}

type fakeWS9999Client struct {
	status  crealityws.Status
	err     error
	delay   time.Duration
	blocked bool // if true, ReadStatus blocks until ctx is done
}

func (f *fakeWS9999Client) ReadStatus(ctx context.Context) (crealityws.Status, error) {
	if f.blocked {
		<-ctx.Done()
		return crealityws.Status{}, ctx.Err()
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.status, f.err
}
