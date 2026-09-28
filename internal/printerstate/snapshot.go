package printerstate

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// ws9999Budget bounds the port-9999 read Snapshot performs alongside the rest
// of the gather (dev_docs/safety-architecture.md section 3.1: "a short 9999
// read (first push, 3 s budget)"). This is independent of crealityws.Client's
// own internal StatusReadTimeout so a caller's context deadline is always
// respected even if that constant ever changes.
const ws9999Budget = 3 * time.Second

// historyHeadLimit bounds the page of server/history/list Snapshot reads to
// find the most recent job's job_id (references/analysis/11-state-model.md
// section 5.2's history_job_id cross-check). internal/moonraker's HistoryList
// exposes only limit/start, not Moonraker's order parameter, so Snapshot
// fetches a small page and picks the entry with the greatest start_time
// itself (see historyHead) rather than trusting an unspecified server-side
// default order. This is a deliberate choice to avoid adding an order
// parameter to internal/moonraker for a single, corroboration-only field
// (job identity's primary source is always the live Moonraker status
// objects, never history).
const historyHeadLimit = 5

// gcodeStoreTailCount bounds the console tail Snapshot reads for the
// informational "recent activity" summary (safety-architecture.md 3.1: never
// a gate in v0.1.0).
const gcodeStoreTailCount = 20

// MoonrakerClient is the subset of *moonraker.Client Snapshot needs, kept
// small so tests can supply a fake instead of a real HTTP server
// (safety-architecture.md 3.1).
type MoonrakerClient interface {
	ServerInfo(ctx context.Context) (moonraker.ServerInfoResult, error)
	// PrinterInfo fetches the live Klipper hostname (review backlog item 24,
	// dev_docs/safety-architecture.md section 3.4). Take reads it in the
	// same pass as every other source so DeriveActivityState can verify a
	// registry-backed printer's persisted hostname is still the one
	// actually answering at this address before treating anything else in
	// the snapshot as trustworthy.
	PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error)
	QueryObjects(ctx context.Context, objects map[string][]string) (map[string]json.RawMessage, error)
	HistoryList(ctx context.Context, limit, start int) (moonraker.HistoryList, error)
	GCodeStore(ctx context.Context, count int) ([]moonraker.GCodeStoreEntry, error)
}

// WS9999Client is the subset of *crealityws.Client Snapshot needs.
type WS9999Client interface {
	ReadStatus(ctx context.Context) (crealityws.Status, error)
}

// Deps bundles the two printer-facing clients Snapshot needs.
type Deps struct {
	Moonraker MoonrakerClient
	WS9999    WS9999Client
}

// queryObjectNames is the exact object set dev_docs/safety-architecture.md
// section 3.1 and references/analysis/11-state-model.md section 2.1 require
// for activity-state derivation, plus the CFS, stored-pause-target and
// live-temperature-cap objects the rest of this project's action policy
// needs from the same round trip.
var queryObjectNames = []string{
	"webhooks",
	"print_stats",
	"pause_resume",
	"idle_timeout",
	"virtual_sdcard",
	"motor_control",
	"toolhead",
	"exclude_object",
	"display_status",
	"custom_macro",
	"gcode_move",
	"extruder",
	"heater_bed",
	"output_pin fan0",
	"output_pin fan1",
	"output_pin fan2",
	"output_pin LED",
	"filament_rack",
	"box",
	"gcode_macro PRINTER_PARAM",
	"gcode_macro product_param",
}

// Snapshot is one consistent, timestamped read of everything
// DeriveActivityState and the state-block frontmatter need. Every source's
// own failure is recorded here (Err + Duration fields) rather than aborting
// the whole gather: a totally unreachable printer still yields a usable
// Snapshot whose derived state is "offline" (11-state-model.md section 1.1
// row 1), per P1 ("reads are always allowed") and P6 (always tell the caller
// something useful). A decoded object field is a pointer that is nil
// whenever the object was absent from the query result or failed to decode;
// callers must treat nil as unknown, never as that type's zero value (see
// internal/moonraker's own presence-rule doc comment).
type Snapshot struct {
	Printer domain.Printer
	Taken   time.Time

	ServerInfo         moonraker.ServerInfoResult
	ServerInfoErr      error
	ServerInfoDuration time.Duration

	// PrinterInfo, PrinterInfoErr: the live printer/info read (review
	// backlog item 24). checkIdentity (state.go) is the only reader that
	// treats PrinterInfoErr as meaningful; every other check in this
	// package ignores it, the same way the other sources' own Err fields
	// are scoped to what actually depends on them.
	PrinterInfo         moonraker.PrinterInfoResult
	PrinterInfoErr      error
	PrinterInfoDuration time.Duration

	ObjectsErr      error
	ObjectsDuration time.Duration
	// DecodeErrs holds one entry per queried object name whose raw JSON was
	// present but failed to decode into this package's typed shape. An
	// object simply absent from the query result (Klipper does not currently
	// report it) is not an error and has no entry here; its typed field is
	// just left nil, the same fail-closed outcome as a decode failure.
	DecodeErrs map[string]error

	Webhooks      *moonraker.Webhooks
	PrintStats    *moonraker.PrintStats
	PauseResume   *moonraker.PauseResume
	IdleTimeout   *moonraker.IdleTimeout
	VirtualSDCard *moonraker.VirtualSDCard
	MotorControl  *moonraker.MotorControl
	Toolhead      *moonraker.Toolhead
	ExcludeObject *moonraker.ExcludeObject
	DisplayStatus *moonraker.DisplayStatus
	CustomMacro   *moonraker.CustomMacro
	GCodeMove     *moonraker.GCodeMove
	Extruder      *moonraker.Extruder
	HeaterBed     *moonraker.HeaterBed
	Fan0          *moonraker.OutputPin
	Fan1          *moonraker.OutputPin
	Fan2          *moonraker.OutputPin
	LED           *moonraker.OutputPin
	FilamentRack  *moonraker.FilamentRack
	Box           *moonraker.Box
	PrinterParam  *moonraker.PrinterParam
	ProductParam  *moonraker.ProductParam

	History         moonraker.HistoryList
	HistoryErr      error
	HistoryDuration time.Duration

	GCodeStoreTail     []moonraker.GCodeStoreEntry
	GCodeStoreErr      error
	GCodeStoreDuration time.Duration

	WS9999          crealityws.Status
	WS9999Err       error
	WS9999Reachable bool
	WS9999Duration  time.Duration
}

// Take gathers a Snapshot for printer in one pass: server/info, printer/info
// (review backlog item 24), one Moonraker objects query, a budgeted 9999
// read, a history head and a gcode_store tail all run concurrently on
// independent goroutines that each write only to their own, disjoint
// Snapshot fields; the caller only reads the result after every goroutine
// has returned (sync.WaitGroup), so this is race-free without a mutex. Take
// never itself returns an error: a fully unreachable printer is a valid,
// meaningful Snapshot (DeriveActivityState reads it as "offline"), not a
// failure of this function.
func Take(ctx context.Context, deps Deps, printer domain.Printer) Snapshot {
	snap := Snapshot{
		Printer: printer,
		Taken:   time.Now().UTC(),
	}

	var wg sync.WaitGroup
	wg.Add(6)

	go func() {
		defer wg.Done()
		start := time.Now()
		info, err := deps.Moonraker.ServerInfo(ctx)
		snap.ServerInfoDuration = time.Since(start)
		snap.ServerInfo = info
		snap.ServerInfoErr = err
	}()

	go func() {
		defer wg.Done()
		start := time.Now()
		info, err := deps.Moonraker.PrinterInfo(ctx)
		snap.PrinterInfoDuration = time.Since(start)
		snap.PrinterInfo = info
		snap.PrinterInfoErr = err
	}()

	go func() {
		defer wg.Done()
		start := time.Now()
		objects := make(map[string][]string, len(queryObjectNames))
		for _, name := range queryObjectNames {
			objects[name] = nil
		}
		raw, err := deps.Moonraker.QueryObjects(ctx, objects)
		snap.ObjectsDuration = time.Since(start)
		snap.ObjectsErr = err
		snap.decodeObjects(raw)
	}()

	go func() {
		defer wg.Done()
		start := time.Now()
		wsCtx, cancel := context.WithTimeout(ctx, ws9999Budget)
		defer cancel()
		status, err := deps.WS9999.ReadStatus(wsCtx)
		snap.WS9999Duration = time.Since(start)
		snap.WS9999 = status
		snap.WS9999Err = err
		snap.WS9999Reachable = err == nil
	}()

	go func() {
		defer wg.Done()
		start := time.Now()
		list, err := deps.Moonraker.HistoryList(ctx, historyHeadLimit, 0)
		snap.HistoryDuration = time.Since(start)
		snap.History = list
		snap.HistoryErr = err
	}()

	go func() {
		defer wg.Done()
		start := time.Now()
		entries, err := deps.Moonraker.GCodeStore(ctx, gcodeStoreTailCount)
		snap.GCodeStoreDuration = time.Since(start)
		snap.GCodeStoreTail = entries
		snap.GCodeStoreErr = err
	}()

	wg.Wait()
	return snap
}

// decodeObjects decodes every object this package reads from a QueryObjects
// result. An object absent from raw (or raw itself nil, e.g. the whole query
// failed) leaves the corresponding field nil: every DeriveActivityState check
// treats a nil pointer as "unknown", per P1, never as that field's zero
// value.
func (s *Snapshot) decodeObjects(raw map[string]json.RawMessage) {
	s.DecodeErrs = map[string]error{}
	decode := func(name string, into func(json.RawMessage) error) {
		r, ok := raw[name]
		if !ok {
			return
		}
		if err := into(r); err != nil {
			s.DecodeErrs[name] = err
		}
	}

	decode("webhooks", func(r json.RawMessage) error {
		v, err := moonraker.DecodeWebhooks(r)
		if err == nil {
			s.Webhooks = &v
		}
		return err
	})
	decode("print_stats", func(r json.RawMessage) error {
		v, err := moonraker.DecodePrintStats(r)
		if err == nil {
			s.PrintStats = &v
		}
		return err
	})
	decode("pause_resume", func(r json.RawMessage) error {
		v, err := moonraker.DecodePauseResume(r)
		if err == nil {
			s.PauseResume = &v
		}
		return err
	})
	decode("idle_timeout", func(r json.RawMessage) error {
		v, err := moonraker.DecodeIdleTimeout(r)
		if err == nil {
			s.IdleTimeout = &v
		}
		return err
	})
	decode("virtual_sdcard", func(r json.RawMessage) error {
		v, err := moonraker.DecodeVirtualSDCard(r)
		if err == nil {
			s.VirtualSDCard = &v
		}
		return err
	})
	decode("motor_control", func(r json.RawMessage) error {
		v, err := moonraker.DecodeMotorControl(r)
		if err == nil {
			s.MotorControl = &v
		}
		return err
	})
	decode("toolhead", func(r json.RawMessage) error {
		v, err := moonraker.DecodeToolhead(r)
		if err == nil {
			s.Toolhead = &v
		}
		return err
	})
	decode("exclude_object", func(r json.RawMessage) error {
		v, err := moonraker.DecodeExcludeObject(r)
		if err == nil {
			s.ExcludeObject = &v
		}
		return err
	})
	decode("display_status", func(r json.RawMessage) error {
		v, err := moonraker.DecodeDisplayStatus(r)
		if err == nil {
			s.DisplayStatus = &v
		}
		return err
	})
	decode("custom_macro", func(r json.RawMessage) error {
		v, err := moonraker.DecodeCustomMacro(r)
		if err == nil {
			s.CustomMacro = &v
		}
		return err
	})
	decode("gcode_move", func(r json.RawMessage) error {
		v, err := moonraker.DecodeGCodeMove(r)
		if err == nil {
			s.GCodeMove = &v
		}
		return err
	})
	decode("extruder", func(r json.RawMessage) error {
		v, err := moonraker.DecodeExtruder(r)
		if err == nil {
			s.Extruder = &v
		}
		return err
	})
	decode("heater_bed", func(r json.RawMessage) error {
		v, err := moonraker.DecodeHeaterBed(r)
		if err == nil {
			s.HeaterBed = &v
		}
		return err
	})
	decode("output_pin fan0", func(r json.RawMessage) error {
		v, err := moonraker.DecodeOutputPin(r)
		if err == nil {
			s.Fan0 = &v
		}
		return err
	})
	decode("output_pin fan1", func(r json.RawMessage) error {
		v, err := moonraker.DecodeOutputPin(r)
		if err == nil {
			s.Fan1 = &v
		}
		return err
	})
	decode("output_pin fan2", func(r json.RawMessage) error {
		v, err := moonraker.DecodeOutputPin(r)
		if err == nil {
			s.Fan2 = &v
		}
		return err
	})
	decode("output_pin LED", func(r json.RawMessage) error {
		v, err := moonraker.DecodeOutputPin(r)
		if err == nil {
			s.LED = &v
		}
		return err
	})
	decode("filament_rack", func(r json.RawMessage) error {
		v, err := moonraker.DecodeFilamentRack(r)
		if err == nil {
			s.FilamentRack = &v
		}
		return err
	})
	decode("box", func(r json.RawMessage) error {
		v, err := moonraker.DecodeBox(r)
		if err == nil {
			s.Box = &v
		}
		return err
	})
	decode("gcode_macro PRINTER_PARAM", func(r json.RawMessage) error {
		v, err := moonraker.DecodePrinterParam(r)
		if err == nil {
			s.PrinterParam = &v
		}
		return err
	})
	decode("gcode_macro product_param", func(r json.RawMessage) error {
		v, err := moonraker.DecodeProductParam(r)
		if err == nil {
			s.ProductParam = &v
		}
		return err
	})
}

// historyHead picks the most recently started job out of a history page. See
// historyHeadLimit's doc comment for why this is done locally instead of
// relying on Moonraker's own ordering. ok is false when list has no jobs.
func historyHead(list moonraker.HistoryList) (job moonraker.HistoryJob, ok bool) {
	for _, j := range list.Jobs {
		if !ok || j.StartTime > job.StartTime {
			job = j
			ok = true
		}
	}
	return job, ok
}
