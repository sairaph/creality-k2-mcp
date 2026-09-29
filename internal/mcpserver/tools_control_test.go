package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/policy"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// This file exercises tools_control.go end to end through the real MCP wire
// protocol against an in-memory printer: a pure-Go fake implementing
// policy.MoonrakerClient/policy.WS9999Client directly (no HTTP, no sockets
// at all, loopback or otherwise, satisfying AGENTS.md's hard testing rule),
// so internal/policy.Execute runs for real against controllable state. The
// fake models exactly the fields DeriveActivityState and the settle checks
// read (dev_docs/safety-architecture.md section 4.2, 11-state-model.md
// section 1.1); the deeper macro-fidelity fake (RESUME's real purge/home
// side effects) already lives in internal/policy's own test suite, which
// this task must not duplicate or alter.

// --- Fake printer state ---

type fakeControlState struct {
	mu sync.Mutex

	printStatsState  string
	filename         string
	printDuration    float64
	isPaused         bool
	vsdActive        bool
	idleTimeoutState string
	webhooksState    string

	extruderTemp, extruderTarget float64
	bedTemp, bedTarget           float64
	fan0, fan1, fan2, led        float64 // output_pin fraction, 0-1
	speedFactor, extrudeFactor   float64

	objects         []map[string]any
	excludedObjects []string

	boxState string
	files    []string

	serverInfoErr error

	// hostname is what PrinterInfo answers with (review backlog item 24):
	// controlDeps sets it to the same value as the printer's own persisted
	// Hostname (controlTestPrinter), so every control test's identity
	// verifies as a match by default unless a test overrides it.
	hostname string
}

func newFakeControlState() *fakeControlState {
	return &fakeControlState{
		printStatsState:  "standby",
		idleTimeoutState: "Ready",
		webhooksState:    "ready",
		extruderTemp:     25,
		bedTemp:          22,
		speedFactor:      1,
		extrudeFactor:    1,
		boxState:         "disconnect",
	}
}

// fanPinValue mirrors domain.ReportedFanPercent's own inverse: the firmware
// macro scales a nonzero M106 S (1-255) onto [min, 255], so this reconstructs
// the output_pin fraction a real printer would report back for a given
// requested percent, letting settleFuncFor's read-back check
// (internal/policy/actions.go) actually settle in this fake.
func fanPinValue(percent float64, min int) float64 {
	s := domain.PercentToS(percent)
	if s <= 0 {
		return 0
	}
	mapped := float64(min) + float64(s-1)*float64(255-min)/254
	return mapped / 255
}

func (st *fakeControlState) queryObjects() map[string]json.RawMessage {
	st.mu.Lock()
	defer st.mu.Unlock()

	out := map[string]json.RawMessage{}
	set := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		out[name] = b
	}

	webhooksState := st.webhooksState
	set("webhooks", moonraker.Webhooks{State: &webhooksState})

	set("print_stats", moonraker.PrintStats{Filename: st.filename, PrintDuration: st.printDuration, State: st.printStatsState})

	isPaused := st.isPaused
	set("pause_resume", moonraker.PauseResume{IsPaused: &isPaused})

	idleState := st.idleTimeoutState
	set("idle_timeout", moonraker.IdleTimeout{State: &idleState})

	vsdActive := st.vsdActive
	set("virtual_sdcard", moonraker.VirtualSDCard{IsActive: &vsdActive})

	notHoming := false
	set("motor_control", moonraker.MotorControl{IsHoming: &notHoming})

	set("toolhead", moonraker.Toolhead{})

	objs := st.objects
	if objs == nil {
		objs = []map[string]any{}
	}
	set("exclude_object", moonraker.ExcludeObject{Objects: objs, ExcludedObjects: append([]string{}, st.excludedObjects...)})

	set("display_status", moonraker.DisplayStatus{})

	notCalibrating := 0
	set("custom_macro", moonraker.CustomMacro{LevelingCalibration: &notCalibrating})

	speedFactor, extrudeFactor := st.speedFactor, st.extrudeFactor
	set("gcode_move", moonraker.GCodeMove{SpeedFactor: &speedFactor, ExtrudeFactor: &extrudeFactor})

	extruderTemp, extruderTarget := st.extruderTemp, st.extruderTarget
	set("extruder", moonraker.Extruder{Temperature: &extruderTemp, Target: &extruderTarget})

	bedTemp, bedTarget := st.bedTemp, st.bedTarget
	set("heater_bed", moonraker.HeaterBed{Temperature: &bedTemp, Target: &bedTarget})

	fan0, fan1, fan2, led := st.fan0, st.fan1, st.fan2, st.led
	set("output_pin fan0", moonraker.OutputPin{Value: &fan0})
	set("output_pin fan1", moonraker.OutputPin{Value: &fan1})
	set("output_pin fan2", moonraker.OutputPin{Value: &fan2})
	set("output_pin LED", moonraker.OutputPin{Value: &led})

	set("filament_rack", moonraker.FilamentRack{})

	boxState := st.boxState
	set("box", moonraker.Box{State: &boxState})

	hotendTemp, fan0Speed, fan2Speed := st.extruderTarget, st.fan0, st.fan2
	set("gcode_macro PRINTER_PARAM", moonraker.PrinterParam{HotendTemp: &hotendTemp, Fan0Speed: &fan0Speed, Fan2Speed: &fan2Speed})

	nozzleCap, bedCap := 300.0, 100.0
	set("gcode_macro product_param", moonraker.ProductParam{NozzleTemp: &nozzleCap, BedTemp: &bedCap})

	return out
}

// --- Fake clients ---

type fakeMoonrakerControl struct{ st *fakeControlState }

func (f *fakeMoonrakerControl) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	f.st.mu.Lock()
	err := f.st.serverInfoErr
	f.st.mu.Unlock()
	if err != nil {
		return moonraker.ServerInfoResult{}, err
	}
	return moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"}, nil
}

// PrinterInfo satisfies policy.MoonrakerClient (resolveExecuteIdentity,
// internal/policy/execute.go) and printerstate.MoonrakerClient
// (DeriveActivityState's own checkIdentity, review backlog item 24).
// controlDeps sets f.st.hostname to the same value as the printer's own
// persisted Hostname (controlTestPrinter), so every control test's identity
// verifies as a match unless a test explicitly overrides f.st.hostname.
func (f *fakeMoonrakerControl) PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	return moonraker.PrinterInfoResult{State: "ready", Hostname: f.st.hostname}, nil
}

func (f *fakeMoonrakerControl) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	return f.st.queryObjects(), nil
}

func (f *fakeMoonrakerControl) HistoryList(ctx context.Context, limit, start int) (moonraker.HistoryList, error) {
	return moonraker.HistoryList{}, nil
}

func (f *fakeMoonrakerControl) GCodeStore(ctx context.Context, count int) ([]moonraker.GCodeStoreEntry, error) {
	return nil, nil
}

func (f *fakeMoonrakerControl) PrintStart(ctx context.Context, filename string) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.printStatsState = "printing"
	f.st.filename = filename
	f.st.printDuration = 1
	f.st.isPaused = false
	f.st.vsdActive = true
	return nil
}

func (f *fakeMoonrakerControl) PrintPause(ctx context.Context) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.printStatsState = "paused"
	f.st.isPaused = true
	return nil
}

func (f *fakeMoonrakerControl) PrintResume(ctx context.Context) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.printStatsState = "printing"
	f.st.isPaused = false
	f.st.vsdActive = true
	if f.st.printDuration == 0 {
		f.st.printDuration = 1
	}
	return nil
}

func (f *fakeMoonrakerControl) PrintCancel(ctx context.Context) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.printStatsState = "cancelled"
	f.st.vsdActive = false
	f.st.isPaused = false
	return nil
}

func (f *fakeMoonrakerControl) RunTemplate(ctx context.Context, t moonraker.Template, args map[string]string) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	switch t {
	case moonraker.TemplateSetHeaterTemperature:
		target, _ := strconv.ParseFloat(args["target"], 64)
		if args["heater"] == "extruder" {
			f.st.extruderTarget = target
		} else {
			f.st.bedTarget = target
		}
	case moonraker.TemplateM106:
		speed, _ := strconv.Atoi(args["speed"])
		var min int
		var dst *float64
		switch args["fan"] {
		case "0":
			min, dst = domain.Fans[domain.FanPart].MinValue, &f.st.fan0
		case "1":
			min, dst = domain.Fans[domain.FanCase].MinValue, &f.st.fan1
		case "2":
			min, dst = domain.Fans[domain.FanAuxiliary].MinValue, &f.st.fan2
		}
		if dst != nil {
			if speed <= 0 {
				*dst = 0
			} else {
				*dst = (float64(min) + float64(speed-1)*float64(255-min)/254) / 255
			}
		}
	case moonraker.TemplateM220:
		p, _ := strconv.Atoi(args["percent"])
		f.st.speedFactor = float64(p) / 100
	case moonraker.TemplateM221:
		p, _ := strconv.Atoi(args["percent"])
		f.st.extrudeFactor = float64(p) / 100
	case moonraker.TemplateExcludeObject:
		name := args["name"]
		for _, e := range f.st.excludedObjects {
			if strings.EqualFold(e, name) {
				return nil
			}
		}
		f.st.excludedObjects = append(f.st.excludedObjects, name)
	}
	return nil
}

func (f *fakeMoonrakerControl) Upload(ctx context.Context, localPath, remoteName string) (moonraker.UploadResult, error) {
	return moonraker.UploadResult{}, fmt.Errorf("upload not supported by this fake")
}

func (f *fakeMoonrakerControl) Delete(ctx context.Context, filename string) (moonraker.DeleteResult, error) {
	return moonraker.DeleteResult{}, fmt.Errorf("delete not supported by this fake")
}

func (f *fakeMoonrakerControl) List(ctx context.Context) ([]moonraker.GCodeFile, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	out := make([]moonraker.GCodeFile, len(f.st.files))
	for i, name := range f.st.files {
		out[i] = moonraker.GCodeFile{Path: name}
	}
	return out, nil
}

func (f *fakeMoonrakerControl) Metadata(ctx context.Context, filename string) (moonraker.FileMetadata, error) {
	return moonraker.FileMetadata{}, fmt.Errorf("metadata not supported by this fake")
}

type fakeWS9999Control struct{ st *fakeControlState }

func (f *fakeWS9999Control) ReadStatus(ctx context.Context) (crealityws.Status, error) {
	return crealityws.Status{}, nil
}

func (f *fakeWS9999Control) SetLight(ctx context.Context, on bool) (bool, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if on {
		f.st.led = 1
	} else {
		f.st.led = 0
	}
	return true, nil
}

// The CFS reads and writes policy.WS9999Client gained in v0.2.0 (plan 3.2). The
// control-tool tests never reach them (no CFS is connected in this fake), so
// each reports the port as unreachable.
var errFakeNoCFS = fmt.Errorf("the control-test fake has no CFS")

func (f *fakeWS9999Control) BoxsInfo(ctx context.Context) (crealityws.BoxsInfo, error) {
	return crealityws.BoxsInfo{}, errFakeNoCFS
}

func (f *fakeWS9999Control) Materials(ctx context.Context) ([]crealityws.CatalogEntry, error) {
	return nil, errFakeNoCFS
}

func (f *fakeWS9999Control) GcodeFiles(ctx context.Context) ([]crealityws.GcodeFileInfo, error) {
	return nil, errFakeNoCFS
}

func (f *fakeWS9999Control) ModifyMaterial(ctx context.Context, e crealityws.MaterialEdit) (*crealityws.BoxsInfo, bool, error) {
	return nil, false, errFakeNoCFS
}

func (f *fakeWS9999Control) StartCFSPrint(ctx context.Context, path string, items []crealityws.ColorMatchItem, selfTest bool, verifyMap func(ctx context.Context) error) (bool, error) {
	return false, errFakeNoCFS
}

func (f *fakeWS9999Control) StartSpoolPrint(ctx context.Context, path string, selfTest bool) (bool, error) {
	return false, errFakeNoCFS
}

func (f *fakeWS9999Control) Stop(ctx context.Context) (bool, error) { return false, errFakeNoCFS }

// --- Test harness ---

// controlTestPrinter is domain.PrinterState.ID's own dedup key (Hostname);
// each test gets a unique one so a lock file left behind by one test's
// Execute call (released on every return, but still exercised on real disk
// under the user's config dir, matching how internal/policy's own tests
// already operate) can never collide with another test running in the same
// package binary.
func controlTestPrinter(t *testing.T, allowControl bool) domain.Printer {
	t.Helper()
	id := "ctl-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	return domain.Printer{
		ID: id, Name: id, Host: "127.0.0.1", MoonrakerPort: 7125,
		Hostname: id + ".local", Enabled: true, AllowControl: allowControl,
	}
}

func controlDeps(t *testing.T, allowControl bool, watchdog policy.Watchdog) (Deps, *fakeControlState) {
	t.Helper()
	st := newFakeControlState()
	moon := &fakeMoonrakerControl{st: st}
	ws := &fakeWS9999Control{st: st}
	printer := controlTestPrinter(t, allowControl)
	st.hostname = printer.Hostname
	deps := Deps{
		Settings: settingsWithPreset(domain.PresetControl),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: func(p domain.Printer) printerstate.Deps {
			return printerstate.Deps{Moonraker: moon, WS9999: ws}
		},
		Watchdog: watchdog,
	}
	return deps, st
}

func controlSession(t *testing.T, deps Deps) *mcp.ClientSession {
	t.Helper()
	srv := newServer(Config{Version: "test"}, deps)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.MCPServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func setPrinting(st *fakeControlState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.printStatsState = "printing"
	st.printDuration = 10
	st.vsdActive = true
	st.isPaused = false
	st.extruderTarget = 200
	st.bedTarget = 60
}

func setPaused(st *fakeControlState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.printStatsState = "paused"
	st.printDuration = 10
	st.vsdActive = true
	st.isPaused = true
	st.extruderTarget = 200
}

// --- start_print ---

func TestStartPrintHappyPath(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	st.files = []string{"model.gcode"}
	cs := controlSession(t, deps)

	res := call(t, cs, "start_print", map[string]any{"filename": "model.gcode"})
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("start_print failed: %s", text)
	}
	if !strings.Contains(text, "accepted: true") || !strings.Contains(text, "effect: confirmed") {
		t.Fatalf("start_print reply = %s", text)
	}
	st.mu.Lock()
	got := st.printStatsState
	st.mu.Unlock()
	if got != "printing" {
		t.Fatalf("printStatsState = %q, want printing", got)
	}
}

func TestStartPrintRefusalFileNotFound(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	res := call(t, cs, "start_print", map[string]any{"filename": "missing.gcode"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("start_print reply = %s", text)
	}
}

// --- pause_print ---

func TestPausePrintHappyPath(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "pause_print", nil)
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("pause_print failed: %s", text)
	}
	if !strings.Contains(text, "effect: confirmed") {
		t.Fatalf("pause_print reply = %s", text)
	}
	st.mu.Lock()
	got := st.printStatsState
	st.mu.Unlock()
	if got != "paused" {
		t.Fatalf("printStatsState = %q, want paused", got)
	}
}

func TestPausePrintRefusalWhileIdle(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	res := call(t, cs, "pause_print", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("pause_print reply = %s", text)
	}
}

// --- resume_print ---

func TestResumePrintTokenFlow(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPaused(st)
	cs := controlSession(t, deps)

	proposal := call(t, cs, "resume_print", nil)
	proposalText := strings.Join(texts(proposal), "\n")
	if proposal.IsError {
		t.Fatalf("resume_print proposal failed: %s", proposalText)
	}
	if !strings.Contains(proposalText, "proposed: true") || !strings.Contains(proposalText, "confirm_token:") {
		t.Fatalf("resume_print proposal reply = %s", proposalText)
	}
	token := extractYAMLValue(t, proposalText, "confirm_token")

	confirmed := call(t, cs, "resume_print", map[string]any{"confirm_token": token})
	confirmedText := strings.Join(texts(confirmed), "\n")
	if confirmed.IsError {
		t.Fatalf("resume_print confirm failed: %s", confirmedText)
	}
	if !strings.Contains(confirmedText, "effect: confirmed") {
		t.Fatalf("resume_print confirm reply = %s", confirmedText)
	}
	st.mu.Lock()
	got := st.printStatsState
	st.mu.Unlock()
	if got != "printing" {
		t.Fatalf("printStatsState = %q, want printing", got)
	}
}

func TestResumePrintStaleTokenConflict(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPaused(st)
	cs := controlSession(t, deps)

	proposal := call(t, cs, "resume_print", nil)
	token := extractYAMLValue(t, strings.Join(texts(proposal), "\n"), "confirm_token")

	// The situation changes before the token is used: cancel out from under
	// the outstanding proposal (bucket flips from Z to I once settled).
	st.mu.Lock()
	st.printStatsState = "cancelled"
	st.vsdActive = false
	st.isPaused = false
	st.mu.Unlock()

	res := call(t, cs, "resume_print", map[string]any{"confirm_token": token})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: conflict") {
		t.Fatalf("resume_print stale-token reply = %s", text)
	}
}

// --- cancel_print ---

func TestCancelPrintTokenFlow(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	proposal := call(t, cs, "cancel_print", nil)
	token := extractYAMLValue(t, strings.Join(texts(proposal), "\n"), "confirm_token")

	confirmed := call(t, cs, "cancel_print", map[string]any{"confirm_token": token})
	confirmedText := strings.Join(texts(confirmed), "\n")
	if confirmed.IsError {
		t.Fatalf("cancel_print confirm failed: %s", confirmedText)
	}
	if !strings.Contains(confirmedText, "effect: confirmed") {
		t.Fatalf("cancel_print confirm reply = %s", confirmedText)
	}
	st.mu.Lock()
	got := st.printStatsState
	st.mu.Unlock()
	if got != "cancelled" {
		t.Fatalf("printStatsState = %q, want cancelled", got)
	}
}

func TestCancelPrintRefusalWhileIdle(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	res := call(t, cs, "cancel_print", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("cancel_print reply = %s", text)
	}
}

// --- set_nozzle_temperature ---

func TestSetNozzleTemperatureHappyPath(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st) // target 200, band default +-10
	cs := controlSession(t, deps)

	res := call(t, cs, "set_nozzle_temperature", map[string]any{"target_c": 205})
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("set_nozzle_temperature failed: %s", text)
	}
	if !strings.Contains(text, "effect: confirmed") {
		t.Fatalf("set_nozzle_temperature reply = %s", text)
	}
	st.mu.Lock()
	got := st.extruderTarget
	st.mu.Unlock()
	if got != 205 {
		t.Fatalf("extruderTarget = %v, want 205", got)
	}
}

// TestSetNozzleTemperatureRefusalIdleNoWatchdog exercises the wiring the
// task calls out explicitly: until T11a lands the daemon, Deps.Watchdog is
// nil and idle heating must be refused with a clear hint.
func TestSetNozzleTemperatureRefusalIdleNoWatchdog(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_nozzle_temperature", map[string]any{"target_c": 210})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") || !strings.Contains(text, "idle-heat watchdog") {
		t.Fatalf("set_nozzle_temperature idle reply = %s", text)
	}
}

// --- set_bed_temperature ---

func TestSetBedTemperatureHappyPath(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st) // target 60, band default +-5
	cs := controlSession(t, deps)

	res := call(t, cs, "set_bed_temperature", map[string]any{"target_c": 63})
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("set_bed_temperature failed: %s", text)
	}
	st.mu.Lock()
	got := st.bedTarget
	st.mu.Unlock()
	if got != 63 {
		t.Fatalf("bedTarget = %v, want 63", got)
	}
}

func TestSetBedTemperatureRefusalOutsideBand(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st) // target 60, band default +-5
	cs := controlSession(t, deps)

	res := call(t, cs, "set_bed_temperature", map[string]any{"target_c": 80})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") || !strings.Contains(text, "band") {
		t.Fatalf("set_bed_temperature band reply = %s", text)
	}
}

// --- set_fan_speed ---

func TestSetFanSpeedHappyPath(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_fan_speed", map[string]any{"fan": "part", "percent": 80})
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("set_fan_speed failed: %s", text)
	}
	if !strings.Contains(text, "effect: confirmed") {
		t.Fatalf("set_fan_speed reply = %s", text)
	}
}

func TestSetFanSpeedRefusalOutOfRange(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_fan_speed", map[string]any{"fan": "part", "percent": 150})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("set_fan_speed reply = %s", text)
	}
}

// TestSetFanSpeedSchemaExposesFanEnum checks that set_fan_speed's InputSchema
// (tools_control.go's registration, built via schema.go's inputSchema/
// withEnum) actually reaches tools/list with fan restricted to its three
// real channel values, not left as a free-form string only described in
// prose.
func TestSetFanSpeedSchemaExposesFanEnum(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "set_fan_speed" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatal("set_fan_speed not registered")
	}
	b, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal set_fan_speed input schema: %v", err)
	}
	schema := string(b)
	for _, want := range []string{`"part"`, `"case"`, `"auxiliary"`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("set_fan_speed input schema missing enum value %s: %s", want, schema)
		}
	}
}

// TestSetFanSpeedInvalidChannelRejectedAsInvalidInput checks that an
// unlisted fan value is still refused with the project's own
// code: invalid_input shape (the MCP schema layer's own validation error is
// classified into that shape, reply.go's failure path), and never reaches
// internal/policy's runtime domain.FanSpecFor check at all - the schema enum
// added by this task catches it first.
func TestSetFanSpeedInvalidChannelRejectedAsInvalidInput(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_fan_speed", map[string]any{"fan": "bogus", "percent": 50})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("set_fan_speed invalid channel reply = %s", text)
	}
}

// --- set_speed_factor ---

func TestSetSpeedFactorHappyPath(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_speed_factor", map[string]any{"percent": 120})
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("set_speed_factor failed: %s", text)
	}
	st.mu.Lock()
	got := st.speedFactor
	st.mu.Unlock()
	if got != 1.2 {
		t.Fatalf("speedFactor = %v, want 1.2", got)
	}
}

func TestSetSpeedFactorRefusalWhileIdle(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_speed_factor", map[string]any{"percent": 120})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("set_speed_factor reply = %s", text)
	}
}

// --- set_flow_factor ---

func TestSetFlowFactorHappyPath(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_flow_factor", map[string]any{"percent": 105})
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("set_flow_factor failed: %s", text)
	}
	st.mu.Lock()
	got := st.extrudeFactor
	st.mu.Unlock()
	if got != 1.05 {
		t.Fatalf("extrudeFactor = %v, want 1.05", got)
	}
}

func TestSetFlowFactorRefusalOutsideBand(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_flow_factor", map[string]any{"percent": 200})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("set_flow_factor reply = %s", text)
	}
}

// --- set_light ---

func TestSetLightHappyPath(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_light", map[string]any{"on": true})
	text := strings.Join(texts(res), "\n")
	if res.IsError {
		t.Fatalf("set_light failed: %s", text)
	}
	if !strings.Contains(text, "effect: confirmed") || !strings.Contains(text, "light_on: true") {
		t.Fatalf("set_light reply = %s", text)
	}
}

func TestSetLightRefusalOffline(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	st.mu.Lock()
	st.serverInfoErr = fmt.Errorf("connection refused")
	st.mu.Unlock()
	cs := controlSession(t, deps)

	res := call(t, cs, "set_light", map[string]any{"on": true})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("set_light offline reply = %s", text)
	}
}

// --- exclude_object ---

func TestExcludeObjectTokenFlow(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	st.mu.Lock()
	st.objects = []map[string]any{{"name": "OBJ1"}, {"name": "OBJ2"}}
	st.mu.Unlock()
	cs := controlSession(t, deps)

	proposal := call(t, cs, "exclude_object", map[string]any{"object_name": "obj1"})
	proposalText := strings.Join(texts(proposal), "\n")
	if proposal.IsError {
		t.Fatalf("exclude_object proposal failed: %s", proposalText)
	}
	token := extractYAMLValue(t, proposalText, "confirm_token")

	confirmed := call(t, cs, "exclude_object", map[string]any{"object_name": "obj1", "confirm_token": token})
	confirmedText := strings.Join(texts(confirmed), "\n")
	if confirmed.IsError {
		t.Fatalf("exclude_object confirm failed: %s", confirmedText)
	}
	if !strings.Contains(confirmedText, "effect: confirmed") {
		t.Fatalf("exclude_object confirm reply = %s", confirmedText)
	}
	st.mu.Lock()
	excluded := append([]string{}, st.excludedObjects...)
	st.mu.Unlock()
	if len(excluded) != 1 || !strings.EqualFold(excluded[0], "OBJ1") {
		t.Fatalf("excludedObjects = %v, want [OBJ1]", excluded)
	}
}

func TestExcludeObjectRefusalNotFound(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	st.mu.Lock()
	st.objects = []map[string]any{{"name": "OBJ1"}, {"name": "OBJ2"}}
	st.mu.Unlock()
	cs := controlSession(t, deps)

	res := call(t, cs, "exclude_object", map[string]any{"object_name": "does-not-exist"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("exclude_object reply = %s", text)
	}
}

// --- policyDeps interface satisfaction ---

// minimalMoonrakerClient implements only printerstate.MoonrakerClient, the
// smaller read-only interface get_printer_status and the other Status tools
// need, deliberately leaving out PrintStart and the rest of
// policy.MoonrakerClient's richer superset (deps.go's policyDeps type
// assertion). It stands in for a PrinterClients implementation that hands
// back a client too thin for control, so policyDeps has something real to
// fail its type assertion against; calls is a call counter proving a
// refused control tool never reaches the printer.
type minimalMoonrakerClient struct{ calls int }

func (m *minimalMoonrakerClient) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	m.calls++
	return moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"}, nil
}

// PrinterInfo only exists to satisfy printerstate.MoonrakerClient (review
// backlog item 24 widened that interface); this test is about the
// policy.MoonrakerClient type assertion failing on the missing write
// methods, not about identity verification, so the response's exact
// hostname does not matter here.
func (m *minimalMoonrakerClient) PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error) {
	m.calls++
	return moonraker.PrinterInfoResult{State: "ready"}, nil
}

func (m *minimalMoonrakerClient) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	m.calls++
	return map[string]json.RawMessage{}, nil
}

func (m *minimalMoonrakerClient) HistoryList(ctx context.Context, limit, start int) (moonraker.HistoryList, error) {
	m.calls++
	return moonraker.HistoryList{}, nil
}

func (m *minimalMoonrakerClient) GCodeStore(ctx context.Context, count int) ([]moonraker.GCodeStoreEntry, error) {
	m.calls++
	return nil, nil
}

// minimalWS9999Client implements only printerstate.WS9999Client, leaving out
// SetLight, so it does not satisfy policy.WS9999Client either.
type minimalWS9999Client struct{ calls int }

func (w *minimalWS9999Client) ReadStatus(ctx context.Context) (crealityws.Status, error) {
	w.calls++
	return crealityws.Status{}, nil
}

// TestControlToolRefusesWhenClientsDoNotSatisfyPolicyInterfaces exercises
// policyDeps' own type assertions (deps.go): a PrinterClients
// implementation whose clients satisfy only the smaller
// printerstate.MoonrakerClient/WS9999Client interfaces (enough for a
// read-only Status tool) but not policy.MoonrakerClient/WS9999Client's
// richer superset must make every control tool refuse cleanly with an
// error - no panic, and nothing sent to (or even read from) the printer -
// rather than a failed type assertion crashing the handler.
func TestControlToolRefusesWhenClientsDoNotSatisfyPolicyInterfaces(t *testing.T) {
	moon := &minimalMoonrakerClient{}
	ws := &minimalWS9999Client{}
	printer := controlTestPrinter(t, true)
	deps := Deps{
		Settings: settingsWithPreset(domain.PresetControl),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: func(p domain.Printer) printerstate.Deps {
			return printerstate.Deps{Moonraker: moon, WS9999: ws}
		},
	}
	cs := controlSession(t, deps)

	res := call(t, cs, "set_light", map[string]any{"on": true})
	text := strings.Join(texts(res), "\n")
	if !res.IsError {
		t.Fatalf("set_light with non-policy clients reply = %s, want an error", text)
	}
	if !strings.Contains(text, "code: internal_error") {
		t.Fatalf("set_light with non-policy clients reply = %s, want code: internal_error", text)
	}
	if moon.calls != 0 || ws.calls != 0 {
		t.Fatalf("set_light with non-policy clients touched the printer client (moon=%d ws=%d), want zero: "+
			"policyDeps must refuse before any read or write", moon.calls, ws.calls)
	}
}

// --- allow_control forbidden ---

func TestControlToolForbiddenWhenAllowControlFalse(t *testing.T) {
	deps, _ := controlDeps(t, false, nil)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_light", map[string]any{"on": true})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: forbidden") || !strings.Contains(text, "allow_control") {
		t.Fatalf("set_light forbidden reply = %s", text)
	}
	if !strings.Contains(text, "printers control on") {
		t.Errorf("forbidden reply should name the command the user runs to allow control: %s", text)
	}

	// The status actions list must agree with the refusal instead of
	// listing set_light as available on an idle printer.
	status := strings.Join(texts(call(t, cs, "get_printer_status", nil)), "\n")
	i := strings.Index(status, "name: set_light")
	if i < 0 {
		t.Fatalf("get_printer_status lists no set_light action:\n%s", status)
	}
	entry := status[i:]
	if next := strings.Index(entry[1:], "- name:"); next >= 0 {
		entry = entry[:next+1]
	}
	if !strings.Contains(entry, "status: blocked") || !strings.Contains(entry, "allow_control is false") {
		t.Errorf("set_light action with control off = %q, want blocked because allow_control is false", entry)
	}
}

// --- preset filtering ---

func TestControlToolsAbsentUnderMonitorAndCameraPresets(t *testing.T) {
	controlToolNames := []string{
		"start_print", "pause_print", "resume_print", "cancel_print",
		"set_nozzle_temperature", "set_bed_temperature", "set_fan_speed",
		"set_speed_factor", "set_flow_factor", "set_light", "exclude_object",
	}
	for _, preset := range []domain.ToolPreset{domain.PresetMonitor, domain.PresetCamera} {
		deps, _ := controlDeps(t, true, nil)
		deps.Settings = settingsWithPreset(preset)
		cs := controlSession(t, deps)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		present := map[string]bool{}
		for _, tool := range res.Tools {
			present[tool.Name] = true
		}
		for _, name := range controlToolNames {
			if present[name] {
				t.Fatalf("control tool %s registered under preset %s", name, preset)
			}
		}
	}
}

// --- annotations ---

func TestControlToolsAnnotations(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	cs := controlSession(t, deps)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}

	wantDestructive := map[string]bool{
		"start_print": true, "pause_print": true, "resume_print": true, "cancel_print": true,
		"set_nozzle_temperature": false, "set_bed_temperature": false, "set_fan_speed": false,
		"set_speed_factor": false, "set_flow_factor": false, "set_light": false,
		"exclude_object": true,
	}
	wantIdempotent := map[string]bool{
		"start_print": false, "pause_print": false, "resume_print": false, "cancel_print": false,
		"set_nozzle_temperature": true, "set_bed_temperature": true, "set_fan_speed": true,
		"set_speed_factor": true, "set_flow_factor": true, "set_light": true,
		"exclude_object": false,
	}
	for name, wantDestr := range wantDestructive {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("%s not registered under preset control", name)
		}
		if tool.Annotations == nil {
			t.Fatalf("%s: no annotations", name)
		}
		if tool.Annotations.ReadOnlyHint {
			t.Fatalf("%s: readOnlyHint = true, want false (it is a write tool)", name)
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != wantDestr {
			t.Fatalf("%s: destructiveHint = %v, want %v", name, tool.Annotations.DestructiveHint, wantDestr)
		}
		if tool.Annotations.IdempotentHint != wantIdempotent[name] {
			t.Fatalf("%s: idempotentHint = %v, want %v", name, tool.Annotations.IdempotentHint, wantIdempotent[name])
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("%s: openWorldHint = %v, want false", name, tool.Annotations.OpenWorldHint)
		}
	}
}

// --- helpers ---

// extractYAMLValue pulls a scalar YAML value out of a rendered frontmatter
// block by key, e.g. `confirm_token: abcd1234` -> "abcd1234". It is
// deliberately simple (no YAML parsing dependency): every value this test
// file reads this way is a single-line scalar with no embedded colon.
func extractYAMLValue(t *testing.T, text, key string) string {
	t.Helper()
	prefix := key + ": "
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			v := strings.TrimPrefix(line, prefix)
			v = strings.Trim(v, `"`)
			return v
		}
	}
	t.Fatalf("key %q not found in:\n%s", key, text)
	return ""
}
