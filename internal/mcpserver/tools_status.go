package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
)

// watchdogStatusTimeout bounds get_printer_status's own query of D2's
// idle-heat watchdog (dev_docs/safety-architecture.md section 10 D2, review
// backlog item 20): long enough for a live daemon's local socket round trip,
// short enough that a dead or hung daemon never noticeably slows a status
// call. This is separate from, and much shorter than, ctx's own deadline.
const watchdogStatusTimeout = 2 * time.Second

// cameraStatusTimeout bounds get_printer_status's own query of the daemon
// hub's live camera connection state (review backlog item 47), the same
// pattern and bound as watchdogStatusTimeout.
const cameraStatusTimeout = 2 * time.Second

// This file implements the "Status" tool group (dev_docs/plan-v0.1.0.md's
// tool surface, T7): get_printer_status, get_current_job, list_job_history
// and list_console_messages. All four resolve a single printer
// (resolvePrinter) and embed printerstate.StateBlock in their frontmatter
// per dev_docs/safety-architecture.md section 5.

func registerStatusTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "get_printer_status", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "get_printer_status",
		Description: "Returns one printer's full current state: the derived activity state with its bucket, " +
			"gating class and the reasons behind it, nozzle and bed temperatures and targets, fan speeds as a " +
			"percent, speed and flow factors, the light, whether a CFS unit is connected, job identity, recent console activity, and, when a policy component is " +
			"wired in, which actions are currently available, blocked or need confirmation. The body explains " +
			"what the current state means in plain terms and gives the exact next call for it, for example " +
			"what a resume would restore while paused, or how to clear an error. Call this before attempting " +
			"any write and again afterward to confirm the outcome; an unreachable printer is reported as " +
			"offline here, not as an error. Related: get_current_job for job-specific progress and metadata, " +
			"list_printers to check reachability across every printer first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getPrinterStatusHandler(s))

	registerTool(s, domain.ToolInfo{Name: "get_current_job", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "get_current_job",
		Description: "Returns the file and rich Creality slicer metadata for the printer's current job (read " +
			"from virtual_sdcard.cur_print_data.metadata), its progress, current layer, elapsed time and " +
			"estimated time remaining, and the exclude_object list of printable objects with which ones are " +
			"already excluded. When no job is currently running, it says so plainly and instead shows a " +
			"summary of the last completed or cancelled job, since that data stays available on the printer " +
			"after the job ends. Use this after get_printer_status confirms a job is active, or on its own to " +
			"check what last happened on an idle printer. Related: list_job_history for older jobs, " +
			"get_printer_status for the printer's overall state.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getCurrentJobHandler(s))

	registerTool(s, domain.ToolInfo{Name: "list_job_history", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "list_job_history",
		Description: "Lists this printer's print job history, newest first and paginated, with lifetime " +
			"totals (job count, total print time, total filament used) carried in the frontmatter on every " +
			"page, not just the first. Each entry carries its job id, filename, outcome (for example completed " +
			"or cancelled), start time, durations and filament used. Call again with page set to the next page " +
			"when the body's hint says more are available; an empty history is reported plainly, not as an " +
			"error. Related: get_current_job for the job in progress, list_console_messages for console output " +
			"around a given time.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listJobHistoryHandler(s))

	registerTool(s, domain.ToolInfo{Name: "list_console_messages", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "list_console_messages",
		Description: "Lists this printer's cached console output (server/gcode_store), newest first and " +
			"paginated, with an optional count controlling how many recent lines are fetched from the printer " +
			"before pagination (bounded to a sane maximum so a huge request cannot stall the call). Console " +
			"text is informational only: it is never used to gate a write, so a stale or unexpected line here " +
			"never blocks or changes what another tool will do. Use this to see recent command responses, " +
			"temperature reports or macro output, for example while diagnosing an unexpected state. Related: " +
			"get_printer_status also carries a short tail of the most recent lines in its recent_activity " +
			"field for a quick check without a separate call.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listConsoleMessagesHandler(s))
}

// printerInput is the optional `printer` argument shared by every
// single-printer tool: id or name, case insensitive, defaulting to the one
// enabled printer (resolvePrinter, printer.go).
type printerInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
}

func printerQuery(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// historyTotalsClient is the subset of *moonraker.Client list_job_history
// needs beyond printerstate.MoonrakerClient (which stops at HistoryList).
// deps.Moonraker's static type is the smaller printerstate.MoonrakerClient
// interface, so this type assertion recovers the extra method; both
// production (DefaultPrinterClients) and every test (clientsFor) hand back a
// real *moonraker.Client, which always satisfies it.
type historyTotalsClient interface {
	HistoryTotals(ctx context.Context) (moonraker.HistoryTotals, error)
}

// summarizeState renders a one-line, human-readable summary of block for a
// listing (list_printers) or a status heading (get_printer_status):
// activity state, nozzle temperature if known, and the current job's
// filename if one is known.
func summarizeState(block printerstate.StateBlock) string {
	parts := []string{strings.ReplaceAll(block.ActivityState, "_", " ")}
	if block.NozzleTemperatureC != nil && block.NozzleTargetC != nil {
		parts = append(parts, fmt.Sprintf("nozzle %.0f/%.0fC", *block.NozzleTemperatureC, *block.NozzleTargetC))
	}
	if block.Job != nil && block.Job.Filename != "" {
		parts = append(parts, "job "+block.Job.Filename)
	}
	return strings.Join(parts, ", ")
}

// formatDuration renders a non-negative second count as a short "1h05m" or
// "5m" string for a status body; a negative input is clamped to zero rather
// than shown, since a negative ETA/elapsed is never meaningful to a caller.
func formatDuration(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds * float64(time.Second))
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// escapeTableCell keeps a display string from breaking a Markdown table row:
// a literal pipe would end the cell early, and an embedded newline would
// start a new (unheaded) row.
func escapeTableCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// --- get_printer_status ---

type statusFront struct {
	printerstate.StateBlock `yaml:",inline"`
}

func (f *statusFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func getPrinterStatusHandler(s *Server) func(context.Context, *mcp.CallToolRequest, printerInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in printerInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		// The policy engine's derivation adds the start-window record (plan 8a.1) so
		// the state shown here and the gates Execute enforces cannot disagree.
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)
		block.Watchdog = s.watchdogBlock(ctx, snap)
		block.Camera = s.cameraBlock(ctx, printer)

		var actions []printerstate.ActionGate
		if s.deps.Policy != nil {
			actions = s.deps.Policy.Actions(printer, derived, nil)
		}

		front := &statusFront{StateBlock: block}
		body := fmt.Sprintf("%s is %s.\n\n%s", block.PrinterName, summarizeState(block), statusGuidance(block))
		return successResult(front, actions, body), nil, nil
	}
}

// watchdogBlock queries D2's idle-heat watchdog for snap's printer's
// currently armed heaters (review backlog item 20), bounded by
// watchdogStatusTimeout and never autostarting the background daemon:
// Deps.WatchdogStatus.Status (internal/daemon/client.Client.Status in
// production, wired from main.go on a client built with
// client.WithoutAutostart, review backlog item 29) only ever dials the
// daemon's existing socket, so a daemon that is not already running is
// simply reported as "unknown" here rather than started just to answer a
// read. A nil Deps.WatchdogStatus (not wired up) reports the same "unknown"
// outcome.
//
// The identity queried is printerstate.Identity(snap): the verified live
// hostname this same snapshot's own printer/info read returned, exactly the
// identity internal/policy's Execute resolves (resolveExecuteIdentity) and
// arms/disarms the watchdog under (review backlog item 31) - never a
// fallback to the raw configured host, which was never the key anything was
// armed under. When snap could not verify an identity, there is nothing to
// query: this reports "unknown" rather than guessing at a key.
func (s *Server) watchdogBlock(ctx context.Context, snap printerstate.Snapshot) *printerstate.WatchdogBlock {
	if s.deps.WatchdogStatus == nil {
		return &printerstate.WatchdogBlock{}
	}
	identity := printerstate.Identity(snap)
	if identity == printerstate.UnverifiedIdentity {
		return &printerstate.WatchdogBlock{}
	}
	wctx, cancel := context.WithTimeout(ctx, watchdogStatusTimeout)
	defer cancel()
	heaters, ok := s.deps.WatchdogStatus.Status(wctx, identity)
	if !ok {
		return &printerstate.WatchdogBlock{}
	}
	alive := true
	block := &printerstate.WatchdogBlock{DaemonAlive: &alive}
	for _, h := range heaters {
		if !h.Armed {
			continue
		}
		var deadline string
		if !h.DeadlineAt.IsZero() {
			deadline = h.DeadlineAt.Format(time.RFC3339)
		}
		block.ArmedHeaters = append(block.ArmedHeaters, printerstate.ArmedHeaterBlock{
			Heater:     h.Heater,
			TargetC:    h.TargetC,
			DeadlineAt: deadline,
		})
	}
	return block
}

// cameraBlock queries the background daemon hub's live camera connection
// state for printer (review backlog item 47), bounded by cameraStatusTimeout
// and never autostarting the daemon: s.deps.CameraStatus (built on the same
// non-autostarting client instance as s.deps.WatchdogStatus) only ever dials
// the daemon's existing socket. A nil s.deps.CameraStatus, or a daemon that
// could not be reached, leaves this nil (the field is simply left out of
// the state block) rather than reporting a fabricated "not connected".
func (s *Server) cameraBlock(ctx context.Context, printer domain.Printer) *printerstate.CameraBlock {
	if s.deps.CameraStatus == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, cameraStatusTimeout)
	defer cancel()
	lastMediaAt, hasMedia, connected, ok := s.deps.CameraStatus.CameraStatus(cctx, printer.Host)
	if !ok {
		return nil
	}
	block := &printerstate.CameraBlock{Connected: connected}
	if hasMedia {
		block.LastMediaAt = lastMediaAt.Format(time.RFC3339)
		secs := time.Since(lastMediaAt).Seconds()
		block.SecondsSinceMedia = &secs
	}
	return block
}

// idleHeatersUnprotected reports whether block shows a heater with a
// nonzero target while the printer is idle (bucket I) and the watchdog
// daemon's liveness could not be confirmed (review backlog item 20): in
// that combination, D2's automatic turn-off is not known to be armed, so a
// heater left on idle stays on indefinitely unless a human or another tool
// call turns it off.
func idleHeatersUnprotected(block printerstate.StateBlock) bool {
	if block.Bucket != string(printerstate.BucketI) {
		return false
	}
	if block.Watchdog != nil && block.Watchdog.DaemonAlive != nil && *block.Watchdog.DaemonAlive {
		return false
	}
	nozzleOn := block.NozzleTargetC != nil && *block.NozzleTargetC > 0
	bedOn := block.BedTargetC != nil && *block.BedTargetC > 0
	return nozzleOn || bedOn
}

// statusGuidance is get_printer_status's state-specific Markdown guidance
// (dev_docs/safety-architecture.md section 5): what the current state means
// and the exact next call for it. It switches on the display state name,
// never the bucket alone, since only the display state distinguishes for
// example pausing from paused for wording purposes.
func statusGuidance(block printerstate.StateBlock) string {
	var body string
	switch block.ActivityState {
	case printerstate.StateIdle, printerstate.StateComplete, printerstate.StateCancelled:
		if block.CFSConnected && (block.CFS == nil || block.CFS.State != printerstate.CFSStateIdle) {
			body = "The printer is idle, but the CFS is not ready for a start (see below): start_print is refused until it " +
				"is. Call get_current_job for the last job's summary."
		} else if block.CFSConnected {
			body = "The printer is idle. Call list_gcode_files to pick a file, then start_print (if control tools are " +
				"enabled for this printer): with a CFS connected that is a two-step mapping proposal, see below. Call " +
				"get_current_job for the last job's summary."
		} else {
			body = "The printer is idle and safe to start a new job. Call list_gcode_files to pick a file, then " +
				"start_print (if control tools are enabled for this printer) to begin. Call get_current_job for " +
				"the last job's summary."
		}
	case printerstate.StatePrinting:
		body = "A job is printing. Call get_current_job for its progress, current layer and ETA. If control " +
			"tools are enabled, pause_print pauses it and cancel_print stops it (cancel_print needs the " +
			"two-step proposal flow: call it once with no confirm_token to see what will happen, then again " +
			"with the returned confirm_token to actually cancel)."
	case printerstate.StatePreparing:
		if block.StartWindow {
			body = "The printer is in the self-test of a print start (several minutes, with the job still reported as " +
				"standby, before it starts printing). Every write is refused during it except set_light and uploading or " +
				"deleting files other than the one being started; cancel_print from this server is refused too, because " +
				"Moonraker's cancel is not known to stop the self-test: stop it on the printer screen. Call " +
				"get_printer_status again shortly to follow it."
		} else {
			body = "The printer is running START_PRINT's own prepare/heat/home sequence for a job that is about " +
				"to begin printing. Writes are blocked except set_light and cancel_print (call cancel_print once with no " +
				"confirm_token to see what will happen, then again with the returned confirm_token to actually cancel) " +
				"until it settles into printing; call get_printer_status again shortly."
		}
	case printerstate.StatePaused:
		body = "The printer is paused."
		if block.StoredHotendTargetC != nil {
			body += fmt.Sprintf(" Resume will reheat the nozzle to %.0f C (stored at pause)", *block.StoredHotendTargetC)
		} else {
			body += " Resume will reheat the nozzle to its stored pause target"
		}
		body += ", home if needed and purge filament before continuing. If control tools are enabled, " +
			"resume_print needs the two-step proposal flow: call it once with no confirm_token to see what " +
			"will happen, then again with the returned confirm_token to actually resume. cancel_print uses " +
			"the same two-step proposal flow (call it once with no confirm_token to see what will happen, " +
			"then again with the returned confirm_token to actually cancel) to stop the job instead."
	case printerstate.StateError:
		body = "The printer reports a job-level error (print_stats.state is error). There is no error-dismiss " +
			"tool in this version: clear the error on the printer's own screen or in Creality Print, then " +
			"call get_printer_status again to confirm it cleared."
	case printerstate.StateOffline:
		body = "The printer did not answer Moonraker at all. Check that it is powered on and reachable on " +
			"the LAN, then call list_printers to confirm reachability, or run the doctor command."
	case printerstate.StateKlippyNotReady:
		body = "Moonraker answered but Klipper is not ready (starting up, shutting down, disconnected, or " +
			"reporting a state this server does not recognise). Wait a moment and call get_printer_status " +
			"again; if it persists, check the printer's own screen for a Klipper error."
	case printerstate.StateHoming, printerstate.StateCalibrating, printerstate.StateBusyCommand:
		body = "The printer is busy with a motion or calibration sequence started on the printer itself or by " +
			"another client. Writes are blocked except set_light until it settles; call get_printer_status " +
			"again shortly rather than retrying a write immediately."
	case printerstate.StateCancelling, printerstate.StatePausing, printerstate.StateResuming:
		body = "A pause, resume or cancel this server issued has not settled yet. Call get_printer_status " +
			"again in a few seconds to see the outcome before issuing another write."
	case printerstate.StateFilamentOperation, printerstate.StateCFSOperation:
		body = "The CFS or the extruder is moving filament (a load, unload or feed reported by the printer, or an operation this " +
			"server just started). Writes are blocked except set_light until it finishes; nothing can be done from here but " +
			"wait, so call get_printer_status again in a little while. Finish or stop the operation on the printer screen if it " +
			"does not end. get_filaments shows the CFS state and its reasons."
	case printerstate.StateUpgrading, printerstate.StateRecoveryPending:
		body = "The printer reports a state this server cannot safely act around yet. Monitor with " +
			"get_printer_status; writes are blocked until it moves to a recognised, settled state."
	case printerstate.StateIdentityMismatch, printerstate.StateIdentityUnverified:
		body = "This printer's identity could not be verified against its registry entry, so every write is " +
			"blocked with no exception (unlike other unknown states, the printer's physical identity itself " +
			"is uncertain here). Re-run discovery (printers scan, the install wizard, or the TUI) to confirm " +
			"the registry hostname still matches this printer, then call get_printer_status again."
	default:
		body = "This printer's state could not be positively confirmed from the data available, so it is " +
			"treated as unknown and writes are blocked (fail closed). Call get_printer_status again, and call " +
			"list_printers to check reachability."
	}
	if block.CFSConnected {
		body += cfsGuidance(block)
	}
	if idleHeatersUnprotected(block) {
		body += " The idle-heat watchdog daemon (dev_docs/safety-architecture.md section 10 D2) could not be " +
			"confirmed alive, so the heater target(s) shown above while the printer is idle are not currently " +
			"protected by its automatic turn-off: turn the heater(s) off, or start the background daemon (`" +
			domain.BinaryName + " camera serve`, or any call that opens the camera view or takes a snapshot, " +
			"which starts it automatically) so it can arm the watchdog. This call never starts the daemon just " +
			"to check it; a later version will add an explicit option to do so."
	}
	return body
}

// --- get_current_job ---

type jobMetadataFront struct {
	Slicer                string   `yaml:"slicer,omitempty"`
	SlicerVersion         string   `yaml:"slicer_version,omitempty"`
	LayerCount            int      `yaml:"layer_count,omitempty"`
	ObjectHeightMM        float64  `yaml:"object_height_mm,omitempty"`
	EstimatedTimeS        float64  `yaml:"estimated_time_s,omitempty"`
	LayerHeightMM         float64  `yaml:"layer_height_mm,omitempty"`
	FirstLayerHeightMM    float64  `yaml:"first_layer_height_mm,omitempty"`
	FilamentType          string   `yaml:"filament_type,omitempty"`
	FilamentUsedG         []string `yaml:"filament_used_g,omitempty"`
	DefaultFilamentColour []string `yaml:"default_filament_colour,omitempty"`
}

type jobFront struct {
	printerstate.StateBlock `yaml:",inline"`
	HasJob                  bool              `yaml:"has_job"`
	Filename                string            `yaml:"filename,omitempty"`
	UUID                    string            `yaml:"uuid,omitempty"`
	Status                  string            `yaml:"status,omitempty"`
	ProgressPercent         *float64          `yaml:"progress_percent,omitempty"`
	Layer                   int               `yaml:"layer,omitempty"`
	LayerCount              int               `yaml:"layer_count,omitempty"`
	ElapsedS                float64           `yaml:"elapsed_s,omitempty"`
	EtaS                    *float64          `yaml:"eta_s,omitempty"`
	FilamentUsed            float64           `yaml:"filament_used,omitempty"`
	Metadata                *jobMetadataFront `yaml:"metadata,omitempty"`
	Objects                 []string          `yaml:"objects,omitempty"`
	ExcludedObjects         []string          `yaml:"excluded_objects,omitempty"`
	CurrentObject           *string           `yaml:"current_object,omitempty"`
}

func (f *jobFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func getCurrentJobHandler(s *Server) func(context.Context, *mcp.CallToolRequest, printerInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in printerInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		front := &jobFront{StateBlock: block}
		front.HasJob = snap.VirtualSDCard != nil && snap.VirtualSDCard.IsActive != nil && *snap.VirtualSDCard.IsActive

		if snap.VirtualSDCard != nil {
			front.Layer = snap.VirtualSDCard.Layer
			front.LayerCount = snap.VirtualSDCard.LayerCount
			if snap.VirtualSDCard.FileSize > 0 || snap.VirtualSDCard.Progress > 0 {
				pct := snap.VirtualSDCard.Progress * 100
				front.ProgressPercent = &pct
			}
			if cur := snap.VirtualSDCard.CurPrintData; cur != nil {
				front.Filename = cur.Filename
				front.Status = cur.Status
				front.ElapsedS = cur.PrintDuration
				front.FilamentUsed = cur.FilamentUsed
				if cur.Metadata != nil {
					m := cur.Metadata
					front.UUID = m.UUID
					front.Metadata = &jobMetadataFront{
						Slicer:                m.Slicer,
						SlicerVersion:         m.SlicerVersion,
						LayerCount:            m.LayerCount,
						ObjectHeightMM:        m.ObjectHeight,
						EstimatedTimeS:        m.EstimatedTime,
						LayerHeightMM:         m.LayerHeight,
						FirstLayerHeightMM:    m.FirstLayerHeight,
						FilamentType:          m.FilamentType,
						FilamentUsedG:         m.FilamentUsedG,
						DefaultFilamentColour: m.DefaultFilamentColour,
					}
					if m.EstimatedTime > 0 {
						eta := m.EstimatedTime - cur.PrintDuration
						if eta < 0 {
							eta = 0
						}
						front.EtaS = &eta
					}
				}
			}
		}
		if snap.PrintStats != nil && front.Filename == "" {
			front.Filename = snap.PrintStats.Filename
		}
		if snap.ExcludeObject != nil {
			front.Objects = objectNames(snap.ExcludeObject.Objects)
			front.ExcludedObjects = snap.ExcludeObject.ExcludedObjects
			front.CurrentObject = snap.ExcludeObject.CurrentObject
		}

		return successResult(front, nil, jobBody(front)), nil, nil
	}
}

// objectNames extracts each object's "name" field from exclude_object's raw
// object list (Klipper's EXCLUDE_OBJECT_DEFINE shape); an object missing a
// usable name falls back to its raw representation rather than being
// dropped, so the count of names always matches the count of objects.
func objectNames(objs []map[string]any) []string {
	if objs == nil {
		return nil
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		if name, ok := o["name"].(string); ok && name != "" {
			out = append(out, name)
			continue
		}
		out = append(out, fmt.Sprint(o))
	}
	return out
}

func jobBody(front *jobFront) string {
	if !front.HasJob {
		if front.Filename == "" {
			return "No job is currently running, and no previous job is known for this printer."
		}
		status := front.Status
		if status == "" {
			status = "status unknown"
		}
		return fmt.Sprintf("No job is currently running. The last job was %q (%s). Call list_job_history for "+
			"older jobs.", front.Filename, status)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Printing %q: layer %d/%d", front.Filename, front.Layer, front.LayerCount)
	if front.ProgressPercent != nil {
		fmt.Fprintf(&b, ", %.0f%% complete", *front.ProgressPercent)
	}
	if front.EtaS != nil {
		fmt.Fprintf(&b, ", ETA %s", formatDuration(*front.EtaS))
	}
	b.WriteString(".")
	if len(front.ExcludedObjects) > 0 {
		fmt.Fprintf(&b, " Excluded objects: %s.", strings.Join(front.ExcludedObjects, ", "))
	}
	if front.CurrentObject != nil && *front.CurrentObject != "" {
		fmt.Fprintf(&b, " Currently printing object %q.", *front.CurrentObject)
	}
	return b.String()
}

// --- list_job_history ---

type jobHistoryInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Page    *int    `json:"page,omitempty" jsonschema:"1-indexed page number; defaults to 1"`
}

// jobHistoryFetchLimit bounds how many of the most recent job records
// list_job_history fetches from Moonraker before paginating them into pages;
// lifetime totals (from server/history/totals) are reported separately in
// the frontmatter regardless of this bound.
const jobHistoryFetchLimit = 200

type jobRecord struct {
	JobID          string  `yaml:"job_id"`
	Filename       string  `yaml:"filename"`
	Status         string  `yaml:"status"`
	StartTime      float64 `yaml:"start_time"`
	EndTime        float64 `yaml:"end_time,omitempty"`
	PrintDurationS float64 `yaml:"print_duration_s"`
	TotalDurationS float64 `yaml:"total_duration_s"`
	FilamentUsed   float64 `yaml:"filament_used"`
}

func jobRecordFrom(j moonraker.HistoryJob) jobRecord {
	return jobRecord{
		JobID:          j.JobID,
		Filename:       j.Filename,
		Status:         j.Status,
		StartTime:      j.StartTime,
		EndTime:        j.EndTime,
		PrintDurationS: j.PrintDuration,
		TotalDurationS: j.TotalDuration,
		FilamentUsed:   j.FilamentUsed,
	}
}

func renderJobRows(jobs []jobRecord) (string, error) {
	if len(jobs) == 0 {
		return "(no job history)\n", nil
	}
	var b strings.Builder
	b.WriteString("| job_id | filename | status | start_time | print_duration_s | filament_used |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, j := range jobs {
		fmt.Fprintf(&b, "| %s | %s | %s | %.0f | %.1f | %.1f |\n",
			escapeTableCell(j.JobID), escapeTableCell(j.Filename), escapeTableCell(j.Status),
			j.StartTime, j.PrintDurationS, j.FilamentUsed)
	}
	return b.String(), nil
}

type jobHistoryFront struct {
	printerstate.StateBlock `yaml:",inline"`
	render.PageMeta         `yaml:",inline"`
	TotalJobsLifetime       int     `yaml:"total_jobs_lifetime,omitempty"`
	TotalPrintTimeS         float64 `yaml:"total_print_time_s,omitempty"`
	TotalFilamentUsed       float64 `yaml:"total_filament_used,omitempty"`
}

func (f *jobHistoryFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func listJobHistoryHandler(s *Server) func(context.Context, *mcp.CallToolRequest, jobHistoryInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in jobHistoryInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		list, err := deps.Moonraker.HistoryList(ctx, jobHistoryFetchLimit, 0)
		if err != nil {
			return failure("list job history", err, ""), nil, nil
		}
		jobs := make([]jobRecord, len(list.Jobs))
		for i, j := range list.Jobs {
			jobs[i] = jobRecordFrom(j)
		}
		sort.Slice(jobs, func(i, k int) bool { return jobs[i].StartTime > jobs[k].StartTime })

		var totals moonraker.HistoryTotals
		if htc, ok := deps.Moonraker.(historyTotalsClient); ok {
			if t, err := htc.HistoryTotals(ctx); err == nil {
				totals = t
			}
		}

		page := 1
		if in.Page != nil {
			page = *in.Page
		}
		window, meta, nextHint, err := paginatePage(jobs, page, renderJobRows)
		if err != nil {
			return failure("paginate job history", err, ""), nil, nil
		}
		rows, err := renderJobRows(window)
		if err != nil {
			return failure("render job history", err, ""), nil, nil
		}

		front := &jobHistoryFront{
			StateBlock:        block,
			PageMeta:          meta,
			TotalJobsLifetime: totals.JobTotals.TotalJobs,
			TotalPrintTimeS:   totals.JobTotals.TotalPrintTime,
			TotalFilamentUsed: totals.JobTotals.TotalFilamentUsed,
		}
		body := rows + nextHint
		if len(jobs) == 0 {
			body = "No job history is recorded for this printer yet."
		}
		return successResult(front, nil, body), nil, nil
	}
}

// --- list_console_messages ---

// consoleMessagesMinCount, consoleMessagesMaxCount and
// consoleMessagesDefaultCount bound the count argument: at least one line,
// at most a sane maximum so a huge request cannot stall the call, defaulting
// to a generous-but-bounded tail when count is not given.
const (
	consoleMessagesMinCount     = 1
	consoleMessagesMaxCount     = 1000
	consoleMessagesDefaultCount = 100
)

type consoleInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Count   *int    `json:"count,omitempty" jsonschema:"how many recent console lines to fetch from the printer before pagination; bounded to at most 1000, defaults to 100"`
	Page    *int    `json:"page,omitempty" jsonschema:"1-indexed page number over the fetched lines, newest first; defaults to 1"`
}

func formatUnixTime(t float64) string {
	sec := int64(t)
	nsec := int64((t - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
}

func renderConsoleRows(entries []moonraker.GCodeStoreEntry) (string, error) {
	if len(entries) == 0 {
		return "(no console messages)\n", nil
	}
	var b strings.Builder
	b.WriteString("| time | type | message |\n|---|---|---|\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", formatUnixTime(e.Time), escapeTableCell(e.Type), escapeTableCell(e.Message))
	}
	return b.String(), nil
}

type consoleFront struct {
	printerstate.StateBlock `yaml:",inline"`
	render.PageMeta         `yaml:",inline"`
	RequestedCount          int `yaml:"requested_count"`
	FetchedCount            int `yaml:"fetched_count"`
}

func (f *consoleFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func listConsoleMessagesHandler(s *Server) func(context.Context, *mcp.CallToolRequest, consoleInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in consoleInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		count := consoleMessagesDefaultCount
		if in.Count != nil {
			count = *in.Count
		}
		if count < consoleMessagesMinCount {
			count = consoleMessagesMinCount
		}
		if count > consoleMessagesMaxCount {
			count = consoleMessagesMaxCount
		}

		entries, err := deps.Moonraker.GCodeStore(ctx, count)
		if err != nil {
			return failure("list console messages", err, ""), nil, nil
		}

		// GCodeStore returns entries oldest-first (newest last); reverse to
		// present them newest first, per this tool's contract.
		newest := make([]moonraker.GCodeStoreEntry, len(entries))
		for i, e := range entries {
			newest[len(entries)-1-i] = e
		}

		page := 1
		if in.Page != nil {
			page = *in.Page
		}
		window, meta, nextHint, err := paginatePage(newest, page, renderConsoleRows)
		if err != nil {
			return failure("paginate console messages", err, ""), nil, nil
		}
		rows, err := renderConsoleRows(window)
		if err != nil {
			return failure("render console messages", err, ""), nil, nil
		}

		front := &consoleFront{
			StateBlock:     block,
			PageMeta:       meta,
			RequestedCount: count,
			FetchedCount:   len(entries),
		}
		body := rows + nextHint
		if len(entries) == 0 {
			body = "No console messages are cached for this printer yet."
		}
		return successResult(front, nil, body), nil, nil
	}
}

// cfsGuidance is the per-state guidance for a printer with a CFS connected
// (dev_docs/plan-v0.2.0.md sections 2.5, 3.1 and 8a.6): what works from this
// server, what must be done at the printer, and which signals cannot be
// detected. The cfs state is derived bucket-first (a print is never called "busy"
// just because the idle quiescence test does not hold during printing), so the
// in_print wording depends on the activity state: printing, paused and the start
// window differ in what is available, and this never claims a tool works that the
// actions list refuses.
func cfsGuidance(block printerstate.StateBlock) string {
	state := "unknown"
	var reasons []string
	if block.CFS != nil {
		state, reasons = block.CFS.State, block.CFS.Reasons
	}
	var b strings.Builder
	b.WriteString(" A CFS is connected (cfs state: " + state + "). Call get_filaments to see its slots, their stored definitions and which are editable.")
	switch state {
	case printerstate.CFSStateIdle:
		b.WriteString(" The CFS is idle: start_print works as a two-step mapping proposal (the mapping is shown for the user to confirm), " +
			"set_filament_definition can relabel a slot, and the setpoint tools work as usual except that the flow factor is never changed while a CFS is connected.")
	case printerstate.CFSStateInPrint:
		b.WriteString(inPrintGuidance(block))
	case printerstate.CFSStateBusy:
		b.WriteString(" The CFS is busy (feeding, loading or otherwise not at rest), so CFS-dependent writes are refused until it settles: call get_printer_status again shortly." + reasonsText(reasons))
	case printerstate.CFSStateError:
		b.WriteString(" The CFS reports an error (a runout, a jam or an error code). Clear it on the printer screen or in Creality Print; this server has no tool for it. While a print is running, pause_print and cancel_print are not blocked by it." + reasonsText(reasons))
	default:
		b.WriteString(" The CFS could not be fully read (port 9999 unreachable or a field missing), so CFS-dependent writes are refused (fail closed) until it can be." + reasonsText(reasons))
	}
	b.WriteString(" Not detectable from here: a tool change, whether a slot physically holds filament, and a spool being pre-loaded or an RFID scan in progress; ask the user.")
	return b.String()
}

// inPrintGuidance is the CFS wording while the cfs state is in_print, which the
// frontmatter reports for printing, paused and preparing alike.
func inPrintGuidance(block printerstate.StateBlock) string {
	switch block.ActivityState {
	case printerstate.StatePaused:
		return " The print is paused. Available now: cancel_print, exclude_object (while the CFS reports no error) and resume_print only for a clean pause this server issued (the actions list says whether it is). " +
			"Pause, fan and speed changes are not available while paused, and the nozzle temperature and flow factor cannot be changed with a CFS connected. " +
			"If the pause was made at the printer screen, by another program, or after a runout or error, resume on the printer screen or in Creality Print."
	case printerstate.StatePreparing:
		if block.StartWindow {
			return " This is the start window: the CFS is part of the print start. Every write except set_light and uploads or deletes of other files is refused, " +
				"and cancel_print from this server is refused too (stop it on the printer screen)."
		}
		return " The print is preparing: cancel_print works and every setpoint is refused until it is printing."
	default:
		return " The CFS is in a print. While it reports no error, set_fan_speed, set_speed_factor and exclude_object are available (a filament change can reset a fan or speed value), " +
			"and pause_print and cancel_print are never blocked by a CFS signal. The nozzle temperature and the flow factor cannot be changed from here during a CFS print, " +
			"because the CFS changes them itself during filament changes and this server cannot see a tool change. After a pause, resume_print works only for a clean pause this server issued."
	}
}

// reasonsText renders the CFS reasons for guidance, or nothing when there are none.
func reasonsText(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return " Reasons: " + strings.Join(reasons, "; ") + "."
}
