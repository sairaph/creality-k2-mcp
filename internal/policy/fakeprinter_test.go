package policy

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

// fakePrinter is a test-only double built from the real macro call graph in
// references/printer-snapshot/config/gcode_macro.cfg, not from a
// guard-aware reimplementation (dev_docs/safety-architecture.md section 6):
// every lifecycle write is accepted regardless of current state, and each
// one applies the real side effects the macro chain applies, including the
// ones that are hazardous when called from the wrong state (RESUME homing
// and purging before RESUME_BASE's own pause check; PAUSE heating to 140 C
// even when nothing is printing). It never opens a socket (AGENTS.md hard
// testing rule): every method here is a plain Go function over in-memory
// state, guarded by mu.
type fakePrinter struct {
	mu sync.Mutex

	klippyConnected bool
	klippyState     string

	printState    string // standby, printing, paused, cancelled, complete, error
	isPaused      bool
	sdActive      bool
	idleState     string // "Ready" or "Printing"
	homedAxes     string // "", "xy", "xyz"
	printDuration float64

	nozzleTemp, nozzleTarget   float64
	bedTemp, bedTarget         float64
	speedFactor, extrudeFactor float64

	fan0, fan1, fan2, led float64

	objects  []map[string]any
	excluded []string

	boxState   string
	cfsConnect int

	// cfs is the CFS half of the fake (fakecfs_test.go): 9999 CFS signals,
	// boxsInfo, catalog, file records and the writes that mutate them.
	cfs *fakeCFS

	storedHotendTemp       float64
	storedFan0, storedFan2 float64

	nozzleCap, bedCap float64

	filename string
	jobUUID  string
	jobStart float64

	files map[string]bool

	eepromCleared bool
	events        []string

	// startPrintFails simulates a start that is accepted but never reaches
	// printing (D7's flow-restore test needs this).
	startPrintFails bool

	// omitProductParam simulates product_param never being reported at all
	// (10-hazard-analysis.md section 6 item 5 / 5.2's "live cap unknown"
	// case), distinct from a reported cap of 0.
	omitProductParam bool

	// mute simulates a lifecycle write that Moonraker accepted (counted,
	// "ok") but whose effect never actually lands - the same "unknown
	// outcome" class as a lost response (10-hazard-analysis.md 3.2, 3.6) -
	// so a test can force Execute's settle poll to time out deterministically
	// without waiting out a real multi-second timeout.
	mute bool

	// failTemplate makes RunTemplate fail (the M221 reset failure case).
	failTemplate bool
	// resumeMacro makes PrintResume behave like the K2: 9999 state 8 for the
	// duration and the HTTP answer only after the macro (until resumeRelease closes).
	resumeMacro bool
	// resumeNoState keeps 9999 silent during the macro (a dropped frame or reconnect).
	resumeNoState bool
	// resumeErrAfter makes PrintResume fail with resumeErr only after this delay.
	resumeErrAfter time.Duration
	// pauseMacro makes PrintPause behave like the K2: 9999 state 5 while parking and
	// wiping, the HTTP answer (and print_stats paused) only after pauseRelease closes.
	pauseMacro    bool
	pauseRelease  chan struct{}
	resumeRelease chan struct{}
	resumeErr     error
	pauseErr      error
	// templateCalls counts every RunTemplate call (M221, heaters, fans...).
	templateCalls int

	// Silent-mode (Creality Qmode) simulation for set_speed_preset: silent is the
	// flag both Moonraker sources report, savedFactor the factor Qmode captured on
	// entry and restores on exit (gcode_macro.cfg:140-229). qmodeOmit drops both
	// sources, qmodeMismatch makes the macro copy disagree. speedFrames records
	// every speedMode frame in order; failSpeedMode and failM220 make that write
	// fail; speedMute accepts M220 without effect; blockTemplate holds any setpoint template in
	// flight until its context is cancelled. ws9999SpeedMode and
	// ws9999FeedratePct override what 9999 reports (nil = consistent with the
	// state), ws9999OmitSpeed drops both.
	// homing and leveling simulate the self-test's own motion (motor_control.is_homing,
	// custom_macro.leveling_calibration).
	// timeoutApplied makes every setpoint template take effect and then return a
	// deadline error (Moonraker queued it behind a macro and it ran, but the answer
	// never came); timeoutLost returns the deadline error without ever applying it.
	timeoutApplied, timeoutLost bool
	// timeoutLostAfter makes every template after that many calls time out unapplied;
	// refusedTemplate fails with a dial error (nothing sent, Status 0); http500Template
	// answers with a definite HTTP 500; omitSavedFactor hides Qmode's saved factor.
	timeoutLostAfter                 int
	refusedTemplate, http500Template bool
	omitSavedFactor                  bool
	homing                           bool
	leveling                         int

	silent, qmodeOmit, qmodeMismatch bool
	savedFactor                      float64
	speedFrames                      []bool
	failSpeedMode, failM220          bool
	failM220Times                    int
	speedMute, blockTemplate         bool
	blockLight, endPrintAfterSpeed   bool
	// blockSpeedMode holds a speedMode frame in flight (never delivered) until its
	// context is cancelled; pauseAfterSpeed pauses the job right after Silent is
	// entered; uploadRelease, when set, holds an Upload until it is closed.
	blockSpeedMode, pauseAfterSpeed bool
	// speedModeHeld and lightHeld count calls currently held by blockSpeedMode and
	// blockLight, so a test waits for the event instead of sleeping; readDelay
	// delays every port-9999 read (context-aware).
	speedModeHeld, lightHeld int
	readDelay                time.Duration
	uploadRelease            chan struct{}
	m220OverwriteAfter       time.Duration
	m220OverwriteTo          float64
	ws9999SpeedMode          *int
	ws9999FeedratePct        *int
	ws9999OmitSpeed          bool
	m220Calls                int

	printStartCalls  int
	printPauseCalls  int
	printResumeCalls int
	printCancelCalls int

	// watchdog is this fake's policy.Watchdog, nil by default (production's
	// "daemon not wired up" case). A test that exercises D2's idle-heat arm
	// sets it with setWatchdog.
	watchdog Watchdog

	// printerInfoHostname and printerInfoErr control PrinterInfo's response,
	// used by the env-override identity resolution tests (execute_test.go):
	// resolveExecuteIdentity calls PrinterInfo when the printer it was given
	// carries no persisted Hostname.
	printerInfoHostname string
	printerInfoErr      error
}

// newFakePrinter returns a fake at a cold, idle, unhomed baseline.
func newFakePrinter() *fakePrinter {
	return &fakePrinter{
		klippyConnected: true,
		klippyState:     "ready",
		printState:      "standby",
		idleState:       "Ready",
		speedFactor:     1.0,
		extrudeFactor:   1.0,
		boxState:        "disconnect",
		nozzleCap:       300,
		bedCap:          100,
		files:           map[string]bool{},
		cfs:             newFakeCFS(),
	}
}

// --- test setup helpers ---

func (f *fakePrinter) withLock(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakePrinter) setHomed(homed bool) {
	f.withLock(func() {
		if homed {
			f.homedAxes = "xyz"
		} else {
			f.homedAxes = ""
		}
	})
}

func (f *fakePrinter) setHot(target float64) {
	f.withLock(func() { f.nozzleTemp = target; f.nozzleTarget = target })
}

func (f *fakePrinter) setPrinting(filename string) {
	f.withLock(func() {
		f.filename = filename
		f.jobUUID = "fake-uuid-" + filename
		f.jobStart = 1000
		f.printState = "printing"
		f.isPaused = false
		f.sdActive = true
		f.printDuration = 5
		f.idleState = "Ready"
	})
}

func (f *fakePrinter) setPaused() {
	f.withLock(func() {
		f.printState = "paused"
		f.isPaused = true
		f.sdActive = true
	})
}

func (f *fakePrinter) setCancelled() {
	f.withLock(func() {
		f.printState = "cancelled"
		f.isPaused = false
		f.sdActive = false
		f.idleState = "Ready"
	})
}

func (f *fakePrinter) setSpeedFactor(pct float64) { f.withLock(func() { f.speedFactor = pct / 100 }) }
func (f *fakePrinter) setExtrudeFactor(pct float64) {
	f.withLock(func() { f.extrudeFactor = pct / 100 })
}
func (f *fakePrinter) setPartFan(pct float64) {
	f.withLock(func() { f.fan0 = fractionFor(domain.FanPart, pct) })
}
func (f *fakePrinter) setCFSConnected(connected bool) {
	f.withLock(func() {
		if connected {
			f.boxState = "connect"
			f.cfsConnect = 1
		} else {
			f.boxState = "disconnect"
			f.cfsConnect = 0
		}
	})
}
func (f *fakePrinter) addObject(name string) {
	f.withLock(func() {
		f.objects = append(f.objects, map[string]any{"name": strings.ToUpper(name)})
	})
}
func (f *fakePrinter) setKlippyUnreachable() {
	f.withLock(func() { f.klippyConnected = false; f.klippyState = "disconnected" })
}
func (f *fakePrinter) addFile(name string) { f.withLock(func() { f.files[name] = true }) }
func (f *fakePrinter) setStartPrintFails(fails bool) {
	f.withLock(func() { f.startPrintFails = fails })
}
func (f *fakePrinter) setOmitProductParam(omit bool) {
	f.withLock(func() { f.omitProductParam = omit })
}
func (f *fakePrinter) setMute(mute bool) {
	f.withLock(func() { f.mute = mute })
}
func (f *fakePrinter) setWatchdog(w Watchdog) {
	f.withLock(func() { f.watchdog = w })
}
func (f *fakePrinter) setPrinterInfo(hostname string, err error) {
	f.withLock(func() { f.printerInfoHostname = hostname; f.printerInfoErr = err })
}

// recordEvent appends e to the fake's own event log under its lock, so a
// test-only collaborator (fakeWatchdog) sharing this log with fakePrinter
// can prove ordering against the fake's own lifecycle/command events (e.g.
// "the watchdog was armed before the heater command was sent").
func (f *fakePrinter) recordEvent(e string) {
	f.withLock(func() { f.events = append(f.events, e) })
}

func (f *fakePrinter) eventsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakePrinter) hasEvent(want string) bool {
	for _, e := range f.eventsSnapshot() {
		if e == want {
			return true
		}
	}
	return false
}

func (f *fakePrinter) counts() (start, pause, resume, cancel int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.printStartCalls, f.printPauseCalls, f.printResumeCalls, f.printCancelCalls
}

// fractionFor computes the output_pin fraction the real M106 macro's floor
// scaling would report for a requested percent on channel, using exactly
// domain's own verified formulas (fans.go) so the fake's read-back and
// policy's own settle check always agree without re-deriving the macro's
// jinja arithmetic here.
func fractionFor(channel domain.FanChannel, percent float64) float64 {
	if percent <= 0 {
		return 0
	}
	spec, ok := domain.FanSpecFor(channel)
	if !ok {
		return 0
	}
	min := float64(spec.MinValue)
	return (min + percent/100*(255-min)) / 255
}

// --- printerstate.MoonrakerClient / policy.MoonrakerClient ---

func (f *fakePrinter) ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return moonraker.ServerInfoResult{
		KlippyConnected: f.klippyConnected,
		KlippyState:     f.klippyState,
		Components:      []string{"webhooks", "print_stats", "pause_resume"},
	}, nil
}

func (f *fakePrinter) PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.printerInfoErr != nil {
		return moonraker.PrinterInfoResult{}, f.printerInfoErr
	}
	return moonraker.PrinterInfoResult{State: "ready", Hostname: f.printerInfoHostname}, nil
}

func (f *fakePrinter) rawObjects() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]any{
		"webhooks": map[string]any{"state": "ready"},
		"print_stats": map[string]any{
			"filename": f.filename, "state": f.printState,
			"print_duration": f.printDuration, "total_duration": f.printDuration,
		},
		"pause_resume": f.pauseResumeRaw(),
		"idle_timeout": map[string]any{"state": f.idleState},
		"virtual_sdcard": map[string]any{
			"is_active": f.sdActive,
			"cur_print_data": map[string]any{
				"filename": f.filename, "start_time": f.jobStart,
				"metadata": map[string]any{"uuid": f.jobUUID},
			},
		},
		"motor_control":   map[string]any{"is_homing": f.homing},
		"toolhead":        map[string]any{"homed_axes": f.homedAxes},
		"exclude_object":  map[string]any{"objects": f.objects, "excluded_objects": f.excluded},
		"display_status":  map[string]any{},
		"custom_macro":    f.customMacroRaw(),
		"gcode_move":      map[string]any{"speed_factor": f.speedFactor, "extrude_factor": f.extrudeFactor},
		"extruder":        map[string]any{"temperature": f.nozzleTemp, "target": f.nozzleTarget, "can_extrude": f.nozzleTemp >= 170},
		"heater_bed":      map[string]any{"temperature": f.bedTemp, "target": f.bedTarget},
		"output_pin fan0": map[string]any{"value": f.fan0},
		"output_pin fan1": map[string]any{"value": f.fan1},
		"output_pin fan2": map[string]any{"value": f.fan2},
		"output_pin LED":  map[string]any{"value": f.led},
		"filament_rack":   f.rackRaw(),
		"box":             f.cfs.moonBox(f.boxState),
		"gcode_macro PRINTER_PARAM": map[string]any{
			"hotend_temp": f.storedHotendTemp, "fan0_speed": f.storedFan0, "fan2_speed": f.storedFan2,
			"fan0_min": 25, "fan1_min": 50, "fan2_min": 100,
		},
		"gcode_macro product_param": map[string]any{"nozzle_temp": f.nozzleCap, "bed_temp": f.bedCap},
		"gcode_macro Qmode":         f.qmodeMacroRaw(),
	}
	if f.omitProductParam {
		delete(out, "gcode_macro product_param")
	}
	if f.cfs.omitBox {
		delete(out, "box")
	}
	return out
}

func (f *fakePrinter) QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error) {
	all := f.rawObjects()
	out := map[string]json.RawMessage{}
	for name := range objects {
		v, ok := all[name]
		if !ok {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}

func (f *fakePrinter) HistoryList(ctx context.Context, limit, start int) (moonraker.HistoryList, error) {
	return moonraker.HistoryList{}, nil
}

func (f *fakePrinter) GCodeStore(ctx context.Context, count int) ([]moonraker.GCodeStoreEntry, error) {
	return nil, nil
}

// PrintStart implements START_PRINT (gcode_macro.cfg:463-512), simplified to
// resolve instantaneously: homes if unhomed, heats, and (unless
// startPrintFails simulates a stalled start) reaches printing directly.
func (f *fakePrinter) PrintStart(ctx context.Context, filename string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.printStartCalls++
	if f.mute {
		return nil
	}
	if f.startPrintFails {
		f.events = append(f.events, "start_stalled")
		return nil
	}
	if !strings.Contains(f.homedAxes, "xyz") {
		f.homedAxes = "xyz"
		f.events = append(f.events, "homed")
	}
	f.nozzleTemp, f.nozzleTarget = 220, 220
	f.bedTemp, f.bedTarget = 60, 60
	f.filename = filename
	f.jobUUID = "fake-uuid-" + filename
	f.jobStart = 1000
	f.printState = "printing"
	f.isPaused = false
	f.sdActive = true
	f.printDuration = 5
	f.speedFactor = 1.0
	f.events = append(f.events, "start_reached_printing")
	return nil
}

// PrintPause implements PAUSE (gcode_macro.cfg:700-762): it checks only
// is_paused, exactly like the real macro, never print_stats.state - calling
// it while idle still heats to 140 C and parks if homed
// (10-hazard-analysis.md 2.4).
func (f *fakePrinter) PrintPause(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.printPauseCalls++
	if f.mute {
		return nil
	}
	if f.pauseMacro {
		f.cfs.state = 5
		rel := f.pauseRelease
		f.mu.Unlock()
		<-rel
		f.mu.Lock()
		f.isPaused = true
		f.printState = "paused"
		f.storedHotendTemp = f.nozzleTarget
		return nil
	}
	if f.isPaused {
		return nil // pause_resume.py: "Print already paused", idempotent
	}
	f.isPaused = true
	f.printState = "paused"
	f.sdActive = true
	f.storedHotendTemp = f.nozzleTarget
	f.nozzleTarget = 140
	f.events = append(f.events, "heat_140")
	if strings.Contains(f.homedAxes, "xyz") {
		f.events = append(f.events, "park")
	}
	f.storedFan0, f.storedFan2 = f.fan0, f.fan2
	f.fan0, f.fan2 = 0, 0
	return f.pauseErr // set by a test to model Moonraker answering after the macro
}

// PrintResume implements RESUME (gcode_macro.cfg:844-916):
// RESUME_EXTERNAL_PROCESS (home if unhomed, purge 82 mm if hot) runs
// unconditionally, before RESUME_BASE's own is_paused check
// (10-hazard-analysis.md 2.4, confirmed live catastrophic-class hazard).
func (f *fakePrinter) PrintResume(ctx context.Context) error {
	f.mu.Lock()
	if f.resumeErr != nil {
		f.printResumeCalls++
		err, after := f.resumeErr, f.resumeErrAfter
		f.mu.Unlock()
		time.Sleep(after)
		return err
	}
	if f.resumeMacro {
		f.printResumeCalls++
		if !f.resumeNoState {
			f.cfs.state = 8
		}
		rel := f.resumeRelease
		f.mu.Unlock()
		<-rel // the HTTP answer only comes after the macro
		f.mu.Lock()
		f.cfs.state = 1
		f.isPaused = false
		f.printState = "printing"
		f.mu.Unlock()
		return nil
	}
	defer f.mu.Unlock()
	f.printResumeCalls++
	if f.mute {
		return nil
	}

	if !strings.Contains(f.homedAxes, "xyz") {
		f.homedAxes = "xyz"
		f.events = append(f.events, "homed")
	}
	if f.nozzleTemp >= 170 {
		f.events = append(f.events, "purge:82.0")
	} else {
		f.events = append(f.events, "purge_failed_cold")
	}

	if !f.isPaused {
		f.events = append(f.events, "resume_aborted_not_paused")
		return nil // Moonraker itself still answers ok (control_test_20260928.md)
	}

	f.isPaused = false
	f.printState = "printing"
	if f.storedHotendTemp > 0 {
		f.nozzleTarget = f.storedHotendTemp
		f.storedHotendTemp = 0
	}
	f.fan0, f.fan2 = f.storedFan0, f.storedFan2
	if f.printDuration == 0 {
		f.printDuration = 5
	}
	f.events = append(f.events, "resumed")
	return nil
}

// PrintCancel implements CANCEL_PRINT (gcode_macro.cfg:918-927): END_PRINT
// then CLEAR_EEPROM_INFO, a genuine physical EEPROM write
// (08-klipper-persistence-guards.md 1.5).
func (f *fakePrinter) PrintCancel(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.printCancelCalls++
	if f.mute {
		return nil
	}
	f.silent = false // END_PRINT calls Qmode_exit
	f.nozzleTarget = 0
	f.bedTarget = 0
	f.fan0, f.fan1, f.fan2 = 0, 0, 0
	f.speedFactor = 1.0
	f.excluded = nil
	f.printState = "cancelled"
	f.isPaused = false
	f.sdActive = false
	f.idleState = "Ready"
	f.eepromCleared = true
	f.events = append(f.events, "eeprom_cleared")
	return nil
}

func (f *fakePrinter) RunTemplate(ctx context.Context, t moonraker.Template, args map[string]string) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.templateCalls++
	if f.blockTemplate {
		// A setpoint write held in flight until its context is cancelled
		// (pre-emption tests).
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return ctx.Err()
	}
	if f.failTemplate {
		return errFakeUnreachable
	}
	// The production shape of a response timeout: *moonraker.Error with Status 0.
	timeoutErr := &moonraker.Error{Op: "RunTemplate", Code: moonraker.CodeUnavailable, Body: "Post \"http://printer/printer/gcode/script\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)"}
	if f.refusedTemplate {
		return &moonraker.Error{Op: "RunTemplate", Code: moonraker.CodeUnavailable, Body: "dial tcp 127.0.0.1:7125: connect: connection refused"}
	}
	if f.http500Template {
		return &moonraker.Error{Op: "RunTemplate", Status: 500, Code: moonraker.CodeInternal, Body: "Internal Server Error"}
	}
	if f.timeoutLost || (f.timeoutLostAfter > 0 && f.templateCalls > f.timeoutLostAfter) {
		return timeoutErr
	}
	if f.timeoutApplied {
		defer func() { err = timeoutErr }()
	}
	switch t {
	case moonraker.TemplateSetHeaterTemperature:
		target, _ := strconv.ParseFloat(args["target"], 64)
		if args["heater"] == "extruder" {
			f.nozzleTarget = target
		} else {
			f.bedTarget = target
		}
		// Recorded so a test can assert the idle-heat watchdog was armed
		// before this command was ever sent (D2).
		f.events = append(f.events, "heater_set:"+args["heater"]+":"+args["target"])
	case moonraker.TemplateM106:
		s, _ := strconv.Atoi(args["speed"])
		var channel domain.FanChannel
		switch args["fan"] {
		case "0":
			channel = domain.FanPart
		case "1":
			channel = domain.FanCase
		case "2":
			channel = domain.FanAuxiliary
		}
		if f.silent && float64(s)*100/255 > 50 {
			// While Silent is on M106 caps every fan at half its range (gcode_macro.cfg).
			s = int(50 * 255 / 100)
		}
		pct := float64(s) * 100 / 255
		value := fractionFor(channel, pct)
		switch args["fan"] {
		case "0":
			f.fan0 = value
		case "1":
			f.fan1 = value
		case "2":
			f.fan2 = value
		}
	case moonraker.TemplateM220:
		f.m220Calls++
		if f.failM220 || f.failM220Times > 0 {
			if f.failM220Times > 0 {
				f.failM220Times--
			}
			// A definite HTTP error answer from Moonraker (nothing was queued).
			return &moonraker.Error{Op: "RunTemplate", Status: 500, Body: "internal error"}
		}
		if f.speedMute {
			return nil
		}
		p, _ := strconv.Atoi(args["percent"])
		f.speedFactor = float64(p) / 100
		f.events = append(f.events, "m220:"+args["percent"])
		if f.m220OverwriteAfter > 0 {
			// The factor lands and is then overwritten (Qmode_exit's own M220).
			after, to := f.m220OverwriteAfter, f.m220OverwriteTo
			go func() {
				time.Sleep(after)
				f.withLock(func() { f.speedFactor = to })
			}()
		}
	case moonraker.TemplateM221:
		p, _ := strconv.Atoi(args["percent"])
		f.extrudeFactor = float64(p) / 100
	case moonraker.TemplateExcludeObject:
		name := strings.ToUpper(args["name"])
		found := false
		for _, e := range f.excluded {
			if e == name {
				found = true
			}
		}
		if !found {
			f.excluded = append(f.excluded, name)
		}
	}
	return nil
}

func (f *fakePrinter) Upload(ctx context.Context, localPath, remoteName string) (moonraker.UploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rel := f.uploadRelease; rel != nil {
		f.mu.Unlock()
		select {
		case <-rel:
		case <-ctx.Done():
		}
		f.mu.Lock()
	}
	f.files[remoteName] = true
	var out moonraker.UploadResult
	out.Item.Path = remoteName
	return out, nil
}

func (f *fakePrinter) Delete(ctx context.Context, filename string) (moonraker.DeleteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.files[filename] {
		return moonraker.DeleteResult{}, &moonraker.Error{Op: "Delete", Status: 400, Code: moonraker.CodeNotFound, Body: "Invalid file path: gcodes/" + filename}
	}
	delete(f.files, filename)
	var out moonraker.DeleteResult
	out.Item.Path = filename
	return out, nil
}

func (f *fakePrinter) List(ctx context.Context) ([]moonraker.GCodeFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]moonraker.GCodeFile, 0, len(f.files))
	for name := range f.files {
		out = append(out, moonraker.GCodeFile{Path: name})
	}
	return out, nil
}

func (f *fakePrinter) Metadata(ctx context.Context, filename string) (moonraker.FileMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.files[filename] {
		return moonraker.FileMetadata{}, &moonraker.Error{Op: "Metadata", Status: 404, Code: moonraker.CodeNotFound, Body: "not found"}
	}
	return moonraker.FileMetadata{Filename: filename}, nil
}

// --- policy.WS9999Client ---

func (f *fakePrinter) ReadStatus(ctx context.Context) (crealityws.Status, error) {
	f.mu.Lock()
	delay := f.readDelay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return crealityws.Status{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cfs
	if c.ws9999Unreachable {
		return crealityws.Status{}, errFakeUnreachable
	}
	p := func(name string, v int) crealityws.Int {
		if c.omit[name] {
			return crealityws.Int{}
		}
		return crealityws.Int{Value: v, Present: true}
	}
	stateNow := c.state
	if len(c.stateSeq) > 0 {
		stateNow, c.stateSeq = c.stateSeq[0], c.stateSeq[1:]
	}
	st := crealityws.Status{
		State:          p("state", stateNow),
		DeviceState:    p("deviceState", c.deviceState),
		FeedState:      p("feedState", c.feedState),
		UpgradeStatus:  p("upgradeStatus", c.upgradeStatus),
		RepoPlrStatus:  p("repoPlrStatus", c.repoPlrStatus),
		MaterialStatus: p("materialStatus", c.materialStatus),
		CfsConnect:     p("cfsConnect", f.cfsConnect),
		LightSw:        p("lightSw", int(f.led)),
		WithSelfTest:   p("withSelfTest", c.withSelfTest),
		EnableSelfTest: p("enableSelfTest", c.enableSelfTest),
	}
	if !f.ws9999OmitSpeed {
		mode := 0
		if f.silent {
			mode = 1
		}
		if f.ws9999SpeedMode != nil {
			mode = *f.ws9999SpeedMode
		}
		feed := int(f.speedFactor*100 + 0.5)
		if f.ws9999FeedratePct != nil {
			feed = *f.ws9999FeedratePct
		}
		st.SpeedMode = crealityws.Int{Value: mode, Present: true}
		st.CurFeedrate = crealityws.Int{Value: feed, Present: true}
	}
	if !c.omit["err"] {
		st.Err = crealityws.StatusErr{ErrCode: c.errcode, Key: c.errkey, Present: true}
	}
	return st, nil
}

func (f *fakePrinter) SetLight(ctx context.Context, on bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blockLight {
		f.lightHeld++
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return false, ctx.Err()
	}
	if on {
		f.led = 1
	} else {
		f.led = 0
	}
	return true, nil
}

// deps returns a policy.Deps backed by this single fake for both halves
// (Moonraker and WS9999), matching how a single physical printer is dialed
// twice in production (internal/moonraker, internal/crealityws).
func (f *fakePrinter) deps() Deps {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Deps{Moonraker: f, WS9999: f, Watchdog: f.watchdog}
}

// pauseResumeRaw renders pause_resume including Creality's resume_err.
func (f *fakePrinter) pauseResumeRaw() map[string]any {
	out := map[string]any{"is_paused": f.isPaused}
	if !f.cfs.omit["resume_err"] {
		out["resume_err"] = f.cfs.resumeErr
	}
	return out
}

// rackRaw renders the side spool holder (filament_rack) from the fake slot.
func (f *fakePrinter) rackRaw() map[string]any {
	if m := f.cfs.slotAt(0, 0); m != nil {
		return map[string]any{"material_type": "0" + m.RFID, "color_value": strings.TrimPrefix(m.Color, "#")}
	}
	return map[string]any{}
}

// --- Silent mode (Creality Qmode), plan-v0.3.0.md ---

func (f *fakePrinter) flagValue() float64 {
	if f.silent {
		return 1
	}
	return 0
}

// customMacroRaw is custom_macro: qmode_flag unless qmodeOmit.
func (f *fakePrinter) customMacroRaw() map[string]any {
	out := map[string]any{"leveling_calibration": f.leveling}
	if !f.qmodeOmit {
		out["qmode_flag"] = f.flagValue()
	}
	return out
}

// qmodeMacroRaw is gcode_macro Qmode: flag unless qmodeOmit; qmodeMismatch
// makes it disagree with custom_macro (an aborted macro).
func (f *fakePrinter) qmodeMacroRaw() map[string]any {
	if f.qmodeOmit {
		return map[string]any{}
	}
	v := f.flagValue()
	if f.qmodeMismatch {
		v = 1 - v
	}
	out := map[string]any{"flag": v}
	if f.silent && !f.omitSavedFactor {
		out["speed_factor"] = f.savedFactor // what Qmode_exit will restore
	}
	return out
}

func (f *fakePrinter) setSilent(on bool) {
	f.withLock(func() {
		f.silent = on
		if on {
			f.savedFactor = f.speedFactor
			f.speedFactor = 0.5
		}
	})
}

func (f *fakePrinter) speedFramesSnapshot() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.speedFrames...)
}

// SetSpeedMode implements the 9999 speedMode frame as the firmware does
// (protocol doc 2.3): speedMode:1 runs Qmode and speedMode:0 runs Qmode_exit,
// both only while printing or paused; Qmode captures the factor and sets 50%,
// Qmode_exit restores the captured factor.
func (f *fakePrinter) SetSpeedMode(ctx context.Context, on bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSpeedMode {
		return false, errFakeUnreachable
	}
	if f.blockSpeedMode {
		f.speedModeHeld++
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return false, ctx.Err()
	}
	f.speedFrames = append(f.speedFrames, on)
	f.events = append(f.events, "speedMode:"+strconv.FormatBool(on))
	inPrint := f.printState == "printing" || f.printState == "paused"
	switch {
	case on && inPrint && !f.silent:
		f.silent = true
		f.savedFactor = f.speedFactor
		f.speedFactor = 0.5
		if f.pauseAfterSpeed {
			f.printState, f.isPaused = "paused", true
		}
		if f.endPrintAfterSpeed {
			// The print ends right after Silent is entered: the flag stays on.
			f.printState, f.sdActive = "complete", false
		}
	case !on && inPrint && f.silent:
		f.silent = false
		f.speedFactor = f.savedFactor
	}
	return true, nil
}
