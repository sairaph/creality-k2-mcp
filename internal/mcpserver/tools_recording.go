package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
)

// This file implements the recording half of the "Camera" tool group
// (dev_docs/plan-v0.1.0.md's tool surface, decision 11, T11c):
// start_recording, stop_recording, list_recordings, delete_recording. Every
// call goes through Deps.CameraRecorder (internal/daemon/client.Client in
// production), never the background daemon's socket directly, matching
// open_camera_view's own CameraViewer seam (tools_viewer.go). Like that
// tool, recordings are read-only on the printer itself
// (dev_docs/safety-architecture.md 4.2's "camera, recording" row: allowed
// in every state, no confirmation, no printer-state gating) so these tools
// build their own frontmatter directly with render.SuccessResult rather
// than embedding printerstate.StateBlock through successResult.

func registerRecordingTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "start_recording", Category: domain.ToolCategoryCamera}, &mcp.Tool{
		Name: "start_recording",
		Description: "Starts recording this server's own live view of a printer's onboard chamber camera, via the " +
			"background daemon that also serves open_camera_view (one camera connection per printer, shared by " +
			"every viewer and recording at once, so starting a recording adds no extra load beyond the first). " +
			"mode: \"video\" (the default) writes a local fragmented MP4 file, written in fragments and fsynced as " +
			"it goes, so it stays playable up to the last flushed fragment even if this server's background " +
			"process is killed outright. mode: \"timelapse\" writes one JPEG still per printer layer change " +
			"(polling the printer's current layer every 2 seconds; falling back to one still every 30 seconds if " +
			"the current layer cannot be determined) to its own directory, full resolution, JPEG quality 85. until " +
			"controls when the recording stops on its own: for video, \"stopped\" (the default) only ever stops " +
			"when stop_recording is called, the configured max duration is reached, or free disk space runs low; " +
			"for timelapse, \"print_end\" is the default (also stops automatically once the current job finishes: " +
			"completed, cancelled, errored, or the printer goes idle after having been seen printing). " +
			"max_duration_minutes bounds the recording regardless of until (default 12 hours for video, 48 hours " +
			"for timelapse). Only one recording (video or timelapse) may be active per printer at a time; call " +
			"stop_recording first to replace one. Recording is read-only on the printer and works in every " +
			"printer state. Related: list_recordings to see what is active or already recorded, stop_recording to " +
			"end this one, open_camera_view for a live view without recording anything.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false},
	}, startRecordingHandler(s))

	registerTool(s, domain.ToolInfo{Name: "stop_recording", Category: domain.ToolCategoryCamera}, &mcp.Tool{
		Name: "stop_recording",
		Description: "Stops an active recording started by start_recording, flushing and closing its file " +
			"cleanly, and returns its final metadata (duration, parts, total size, and why it stopped). Call " +
			"list_recordings first if the recording's id is not already known. Stopping an id that is not " +
			"currently active returns a not_found error rather than silently succeeding.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false},
	}, stopRecordingHandler(s))

	registerTool(s, domain.ToolInfo{Name: "list_recordings", Category: domain.ToolCategoryCamera}, &mcp.Tool{
		Name: "list_recordings",
		Description: "Lists every recording this server knows about, active and completed, newest first: each " +
			"one's id, printer, mode, whether it is still active, how it stopped (once it has), duration, total " +
			"size, and its part count (video) or frame count (timelapse), plus the recordings directory's total " +
			"disk usage in the frontmatter. Pass printer to filter to one printer's recordings. Related: " +
			"start_recording to begin one, stop_recording to end an active one, delete_recording to remove a " +
			"completed one's files.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listRecordingsHandler(s))

	registerTool(s, domain.ToolInfo{Name: "delete_recording", Category: domain.ToolCategoryCamera}, &mcp.Tool{
		Name: "delete_recording",
		Description: "Deletes a completed recording's files (every part or timelapse frame plus its metadata) " +
			"from disk. Deleting a currently active recording is refused outright; call stop_recording first. " +
			"Every delete requires the " +
			"two-step proposal flow: call once with no confirm_token to see what will happen, then again with the " +
			"same id plus that confirm_token to actually remove it. This cannot be undone. Related: " +
			"list_recordings to confirm the id first.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, deleteRecordingHandler(s))
}

// recordingRecorderUnavailableHint mirrors cameraViewerUnavailableHint
// (tools_viewer.go) for the recording seam.
const recordingRecorderUnavailableHint = "This server's background daemon could not be started or reached; " +
	"make sure this process can launch its own executable, then try again."

func recorderUnavailable() *mcp.CallToolResult {
	return render.ErrorResult(render.Error{
		Code:    render.CodeUnavailable,
		Message: "The recorder is not available: this server's background daemon client is not wired up.",
		Hint: "This usually means the server's own executable path could not be resolved at startup; " +
			"restarting the server should fix it.",
	})
}

// recordingFailure classifies an error internal/daemon/client.Client's
// recording methods return. The daemon/socket RPC layer only ever carries
// an error's message text across the wire (no structured code), so
// classification here is necessarily by message substring, matching every
// distinct error internal/daemon.Recorder returns.
func recordingFailure(what string, err error) *mcp.CallToolResult {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already active for printer"):
		return render.ErrorResult(render.Error{
			Code:    render.CodeConflict,
			Message: shortMessage(fmt.Sprintf("Failed to %s: %v", what, err)),
			Hint:    "Call stop_recording with that recording's id first, or list_recordings to find it.",
		})
	case strings.Contains(msg, "safety threshold"):
		return render.ErrorResult(render.Error{
			Code:    render.CodeConflict,
			Message: shortMessage(fmt.Sprintf("Failed to %s: %v", what, err)),
			Hint:    "Free up disk space on the recordings volume and try again.",
		})
	case strings.Contains(msg, "is active; call recording.stop first"):
		return render.ErrorResult(render.Error{
			Code:    render.CodeConflict,
			Message: shortMessage(fmt.Sprintf("Failed to %s: %v", what, err)),
			Hint:    "Call stop_recording with this id first, then delete_recording again.",
		})
	case strings.Contains(msg, "no active recording with id"), strings.Contains(msg, "not found"):
		return render.ErrorResult(render.Error{
			Code:    render.CodeNotFound,
			Message: shortMessage(fmt.Sprintf("Failed to %s: %v", what, err)),
			Hint:    "Call list_recordings to see valid ids.",
		})
	case strings.Contains(msg, "no camera keyframe"):
		// The daemon's own recorder.go waits recorderKeyframeWait for a
		// keyframe before failing this way (review backlog item 47). That is
		// not a connection problem, so this gets the same message as
		// get_camera_snapshot's equivalent timeout rather than a generic
		// "unreachable printer" hint.
		return render.ErrorResult(render.Error{
			Code:    render.CodeUnavailable,
			Message: shortMessage(fmt.Sprintf("Failed to %s: %s; try again.", what, noVideoMessage(daemon.RecorderKeyframeWait))),
			Hint:    noVideoHint,
		})
	case strings.Contains(msg, "not yet available"):
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: shortMessage(fmt.Sprintf("Failed to %s: %v", what, err)),
		})
	default:
		return failure(what, err, "")
	}
}

// --- start_recording ---

type startRecordingInput struct {
	printerInput
	Mode               *string `json:"mode,omitempty" jsonschema:"video (default) or timelapse (one JPEG still per layer change)"`
	Until              *string `json:"until,omitempty" jsonschema:"stopped or print_end; defaults to stopped for video, print_end for timelapse"`
	MaxDurationMinutes *int    `json:"max_duration_minutes,omitempty" jsonschema:"maximum recording length in minutes; defaults to 720 (12 hours) for video, 2880 (48 hours) for timelapse"`
}

type startRecordingFront struct {
	ID                 string `yaml:"id"`
	PrinterID          string `yaml:"printer_id"`
	PrinterName        string `yaml:"printer_name"`
	Mode               string `yaml:"mode"`
	Until              string `yaml:"until"`
	MaxDurationMinutes int    `yaml:"max_duration_minutes"`
	StartedAt          string `yaml:"started_at"`
	Active             bool   `yaml:"active"`
}

func startRecordingHandler(s *Server) func(context.Context, *mcp.CallToolRequest, startRecordingInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in startRecordingInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}

		mode := string(daemon.RecordModeVideo)
		if in.Mode != nil && strings.TrimSpace(*in.Mode) != "" {
			mode = strings.ToLower(strings.TrimSpace(*in.Mode))
		}
		if mode != string(daemon.RecordModeVideo) && mode != string(daemon.RecordModeTimelapse) {
			return render.ErrorResult(render.Error{
				Code:    render.CodeInvalidInput,
				Message: fmt.Sprintf("unknown mode %q", mode),
				Hint:    "Use \"video\" or \"timelapse\".",
			}), nil, nil
		}

		// until defaults to stopped for video (T11c, unchanged) and to
		// print_end for timelapse (T11d, decision 11): a timelapse left
		// running with no explicit until is far more likely to be meant to
		// cover "the rest of this print" than "until I remember to stop it".
		until := ""
		if in.Until != nil && strings.TrimSpace(*in.Until) != "" {
			until = strings.ToLower(strings.TrimSpace(*in.Until))
			if until != string(daemon.RecordUntilStopped) && until != string(daemon.RecordUntilPrintEnd) {
				return render.ErrorResult(render.Error{
					Code:    render.CodeInvalidInput,
					Message: fmt.Sprintf("unknown until %q", until),
					Hint:    "Use \"stopped\" or \"print_end\".",
				}), nil, nil
			}
		} else if mode == string(daemon.RecordModeTimelapse) {
			until = string(daemon.RecordUntilPrintEnd)
		} else {
			until = string(daemon.RecordUntilStopped)
		}

		maxMinutes := 0
		if in.MaxDurationMinutes != nil {
			if *in.MaxDurationMinutes <= 0 {
				return render.ErrorResult(render.Error{
					Code:    render.CodeInvalidInput,
					Message: "max_duration_minutes must be positive",
				}), nil, nil
			}
			maxMinutes = *in.MaxDurationMinutes
		}

		if s.deps.CameraRecorder == nil {
			return recorderUnavailable(), nil, nil
		}

		// The recording is attributed to, and (for until: print_end) polled
		// against, a verified live printer identity, never the raw
		// configured host (review backlog item 31): printerstate.Identity
		// returns the Klipper hostname a fresh printer/info read actually
		// confirmed, or printerstate.UnverifiedIdentity when that read
		// failed or returned no hostname, in which case starting a
		// recording is refused outright rather than keying it to an
		// unconfirmed printer.
		clientDeps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, clientDeps, printer)
		identity := printerstate.Identity(snap)
		if identity == printerstate.UnverifiedIdentity {
			return render.ErrorResult(render.Error{
				Code: render.CodeUnavailable,
				Message: fmt.Sprintf(
					"%s's identity could not be verified (its printer/info read failed or returned no hostname).",
					printer.Name),
				Hint: "A recording is keyed to a verified printer identity so it is reliably attributed to the " +
					"right printer. Make sure the printer is powered on and reachable, then try again.",
			}), nil, nil
		}

		params := daemon.RecordingStartParams{
			PrinterID:          printer.ID,
			Host:               printer.Host,
			Identity:           identity,
			Mode:               mode,
			Until:              until,
			MaxDurationSeconds: maxMinutes * 60,
		}
		info, err := s.deps.CameraRecorder.StartRecording(ctx, params)
		if err != nil {
			return recordingFailure("start the recording", err), nil, nil
		}

		front := &startRecordingFront{
			ID:                 info.ID,
			PrinterID:          info.PrinterID,
			PrinterName:        printer.Name,
			Mode:               string(info.Mode),
			Until:              string(info.Until),
			MaxDurationMinutes: int(info.MaxDuration.Minutes()),
			StartedAt:          info.StartedAt.UTC().Format(time.RFC3339),
			Active:             info.Active,
		}
		body := fmt.Sprintf("Recording %s started for %s (until: %s, max duration %d minutes). "+
			"Call stop_recording with id: %q to end it, or list_recordings to check on it.",
			info.ID, printer.Name, until, front.MaxDurationMinutes, info.ID)
		if until == string(daemon.RecordUntilPrintEnd) {
			body += " It will stop automatically once the current job finishes."
		}
		return render.SuccessResult(front, body), nil, nil
	}
}

// --- stop_recording ---

type stopRecordingInput struct {
	ID string `json:"id" jsonschema:"the recording id, from start_recording or list_recordings"`
}

type stopRecordingFront struct {
	ID              string  `yaml:"id"`
	PrinterID       string  `yaml:"printer_id"`
	Active          bool    `yaml:"active"`
	StopReason      string  `yaml:"stop_reason,omitempty"`
	DurationSeconds float64 `yaml:"duration_seconds"`
	Bytes           int64   `yaml:"bytes"`
	Parts           int     `yaml:"parts"`
}

func stopRecordingHandler(s *Server) func(context.Context, *mcp.CallToolRequest, stopRecordingInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in stopRecordingInput) (*mcp.CallToolResult, any, error) {
		id := strings.TrimSpace(in.ID)
		if id == "" {
			return render.ErrorResult(render.Error{Code: render.CodeInvalidInput, Message: "id must not be empty"}), nil, nil
		}
		if s.deps.CameraRecorder == nil {
			return recorderUnavailable(), nil, nil
		}

		info, err := s.deps.CameraRecorder.StopRecording(ctx, id)
		if err != nil {
			return recordingFailure("stop the recording", err), nil, nil
		}

		front := &stopRecordingFront{
			ID:              info.ID,
			PrinterID:       info.PrinterID,
			Active:          info.Active,
			StopReason:      info.StopReason,
			DurationSeconds: info.DurationSeconds,
			Bytes:           info.Bytes,
			Parts:           len(info.Parts),
		}
		body := fmt.Sprintf("Stopped recording %s (%s). Duration %.0fs, %d bytes across %d part(s).",
			info.ID, info.StopReason, info.DurationSeconds, info.Bytes, len(info.Parts))
		return render.SuccessResult(front, body), nil, nil
	}
}

// --- list_recordings ---

type listRecordingsInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; omit to list every printer's recordings"`
	Page    *int    `json:"page,omitempty" jsonschema:"1-indexed page number; defaults to 1"`
}

type recordingRow struct {
	ID              string
	PrinterID       string
	Mode            string
	Until           string
	Active          bool
	StopReason      string
	StartedAt       time.Time
	DurationSeconds float64
	Bytes           int64
	Parts           int
	Frames          int
}

func renderRecordingRows(rows []recordingRow) (string, error) {
	if len(rows) == 0 {
		return "(no recordings)\n", nil
	}
	var b strings.Builder
	b.WriteString("| id | printer | mode | active | started_at | duration_s | bytes | parts | frames | stop_reason |\n" +
		"|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		active := ""
		if r.Active {
			active = "yes"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %.0f | %d | %d | %d | %s |\n",
			escapeTableCell(r.ID), escapeTableCell(r.PrinterID), r.Mode, active,
			r.StartedAt.UTC().Format(time.RFC3339), r.DurationSeconds, r.Bytes, r.Parts, r.Frames,
			escapeTableCell(r.StopReason))
	}
	return b.String(), nil
}

type listRecordingsFront struct {
	render.PageMeta `yaml:",inline"`
	DiskUsageBytes  int64 `yaml:"disk_usage_bytes"`
	ActiveCount     int   `yaml:"active_count"`
}

func listRecordingsHandler(s *Server) func(context.Context, *mcp.CallToolRequest, listRecordingsInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in listRecordingsInput) (*mcp.CallToolResult, any, error) {
		if s.deps.CameraRecorder == nil {
			return recorderUnavailable(), nil, nil
		}

		var filterPrinterID string
		if in.Printer != nil && strings.TrimSpace(*in.Printer) != "" {
			p, errRes := s.resolvePrinter(*in.Printer)
			if errRes != nil {
				return errRes, nil, nil
			}
			filterPrinterID = p.ID
		}

		res, err := s.deps.CameraRecorder.ListRecordings(ctx)
		if err != nil {
			return recordingFailure("list recordings", err), nil, nil
		}

		rows := make([]recordingRow, 0, len(res.Recordings))
		activeCount := 0
		for _, r := range res.Recordings {
			if filterPrinterID != "" && r.PrinterID != filterPrinterID {
				continue
			}
			if r.Active {
				activeCount++
			}
			rows = append(rows, recordingRow{
				ID:              r.ID,
				PrinterID:       r.PrinterID,
				Mode:            string(r.Mode),
				Until:           string(r.Until),
				Active:          r.Active,
				StopReason:      r.StopReason,
				StartedAt:       r.StartedAt,
				DurationSeconds: r.DurationSeconds,
				Bytes:           r.Bytes,
				Parts:           len(r.Parts),
				Frames:          len(r.Frames),
			})
		}

		page := 1
		if in.Page != nil {
			page = *in.Page
		}
		window, meta, nextHint, err := paginatePage(rows, page, renderRecordingRows)
		if err != nil {
			return failure("paginate recordings", err, ""), nil, nil
		}
		tableRows, err := renderRecordingRows(window)
		if err != nil {
			return failure("render recordings", err, ""), nil, nil
		}

		front := &listRecordingsFront{
			PageMeta:       meta,
			DiskUsageBytes: res.DiskUsageBytes,
			ActiveCount:    activeCount,
		}
		body := fmt.Sprintf("%d recording(s), %d active, %d bytes on disk in the recordings directory.\n\n%s\n%s",
			meta.Total, activeCount, res.DiskUsageBytes, tableRows, nextHint)
		return render.SuccessResult(front, body), nil, nil
	}
}

// --- delete_recording ---

// deleteRecordingTokenTTL matches internal/policy's own proposal_token TTL
// (120s, dev_docs/safety-architecture.md section 3.3), kept independent of
// that package's tokenStore (delete_recording is not a printer-state-bound
// action, D3's proposal_token list does not include it; plan-v0.1.0.md
// decision 6 still calls for the same two-step proposal/confirm flow as
// delete_gcode_file, so this is a small, self-contained equivalent).
const deleteRecordingTokenTTL = 120 * time.Second

type deleteRecordingProposal struct {
	id       string
	issuedAt time.Time
}

type deleteRecordingTokenStore struct {
	mu     sync.Mutex
	tokens map[string]deleteRecordingProposal
}

// deleteRecordingTokens is process-wide (not per-Server) deliberately: it
// only ever binds an opaque random token to the recording id it was issued
// for, so sharing it across every mcpserver.Server instance in this process
// is safe and needs no extra plumbing.
var deleteRecordingTokens = &deleteRecordingTokenStore{tokens: map[string]deleteRecordingProposal{}}

func (s *deleteRecordingTokenStore) issue(id string, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, p := range s.tokens {
		if now.Sub(p.issuedAt) > deleteRecordingTokenTTL {
			delete(s.tokens, tok)
		}
	}
	tok := randomDeleteRecordingToken()
	s.tokens[tok] = deleteRecordingProposal{id: id, issuedAt: now}
	return tok
}

// consume reports whether token is a currently valid, unused proposal for
// id, removing it unconditionally the moment it is found (single use).
func (s *deleteRecordingTokenStore) consume(token, id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.tokens[token]
	if !ok {
		return false
	}
	delete(s.tokens, token)
	if now.Sub(p.issuedAt) > deleteRecordingTokenTTL {
		return false
	}
	return p.id == id
}

func randomDeleteRecordingToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic("mcpserver: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

type deleteRecordingInput struct {
	ID           string  `json:"id" jsonschema:"the recording id, from list_recordings"`
	ConfirmToken *string `json:"confirm_token,omitempty" jsonschema:"the token from a prior call to this same tool, required to confirm the delete; omit for the first call"`
}

type deleteRecordingFront struct {
	ID           string `yaml:"id"`
	Proposed     bool   `yaml:"proposed,omitempty"`
	Deleted      bool   `yaml:"deleted,omitempty"`
	ConfirmToken string `yaml:"confirm_token,omitempty"`
}

func deleteRecordingHandler(s *Server) func(context.Context, *mcp.CallToolRequest, deleteRecordingInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in deleteRecordingInput) (*mcp.CallToolResult, any, error) {
		id := strings.TrimSpace(in.ID)
		if id == "" {
			return render.ErrorResult(render.Error{Code: render.CodeInvalidInput, Message: "id must not be empty"}), nil, nil
		}
		if s.deps.CameraRecorder == nil {
			return recorderUnavailable(), nil, nil
		}

		token := tokenArg(in.ConfirmToken)
		now := time.Now()
		if token == "" {
			tok := deleteRecordingTokens.issue(id, now)
			front := &deleteRecordingFront{ID: id, Proposed: true, ConfirmToken: tok}
			body := fmt.Sprintf("This will permanently delete every file for recording %s. This cannot be undone. "+
				"Call delete_recording again with id: %q and confirm_token: %q (valid for %s) to actually delete it.",
				id, id, tok, deleteRecordingTokenTTL)
			return render.SuccessResult(front, body), nil, nil
		}

		if !deleteRecordingTokens.consume(token, id, now) {
			return render.ErrorResult(render.Error{
				Code:    render.CodeNotFound,
				Message: "no such proposal (expired, already used, the id changed, or the server restarted)",
				Hint:    "Call delete_recording again with no confirm_token to request a fresh proposal.",
			}), nil, nil
		}

		if err := s.deps.CameraRecorder.DeleteRecording(ctx, id); err != nil {
			return recordingFailure("delete the recording", err), nil, nil
		}

		front := &deleteRecordingFront{ID: id, Deleted: true}
		body := fmt.Sprintf("Deleted recording %s. This cannot be undone.", id)
		return render.SuccessResult(front, body), nil, nil
	}
}
