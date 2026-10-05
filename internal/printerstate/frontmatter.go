package printerstate

import (
	"fmt"
	"strings"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

// ActionGate is one entry of the frontmatter's actions list
// (references/analysis/11-state-model.md section 6). internal/policy is the
// only package that ever populates StateBlock.Actions; this package leaves
// the field ready but empty (P2: policy.Available, not this package, decides
// per-action availability).
type ActionGate struct {
	Name   string `yaml:"name"`
	Status string `yaml:"status"` // available | blocked | needs_confirmation
	Reason string `yaml:"reason,omitempty"`
}

// PendingActionBlock is the rendered form of a PendingAction for the
// frontmatter (11-state-model.md section 6's PendingAction struct): a
// caller-facing snapshot with elapsed/timeout already resolved to
// milliseconds, rather than the raw time.Time/time.Duration pair
// DeriveActivityState consumes.
type PendingActionBlock struct {
	Kind      string `yaml:"kind"`
	IssuedAt  string `yaml:"issued_at"`
	ElapsedMs int64  `yaml:"elapsed_ms"`
	TimeoutMs int64  `yaml:"timeout_ms"`
}

// ArmedHeaterBlock is one heater the background daemon's idle-heat watchdog
// has armed (dev_docs/safety-architecture.md section 10 D2, review backlog
// item 20), rendered for WatchdogBlock. DeadlineAt is empty when the daemon
// did not report one.
type ArmedHeaterBlock struct {
	Heater     string  `yaml:"heater"`
	TargetC    float64 `yaml:"target_c"`
	DeadlineAt string  `yaml:"deadline_at,omitempty"`
}

// WatchdogBlock reports D2's idle-heat watchdog for one printer: whether the
// background daemon answered a bounded, non-autostarting status query, and
// every heater it currently has armed for this printer's identity (review
// backlog item 20). This package never imports internal/daemon (which
// itself imports this package), so the daemon's own HeaterStatus type is
// translated into this small, dependency-free shape by the caller
// (internal/mcpserver's get_printer_status handler) rather than reused
// directly.
//
// DaemonAlive is nil whenever the daemon could not be reached within the
// caller's bound: get_printer_status never autostarts the daemon just to
// check on it (dev_docs/safety-architecture.md section 10 D2), so an
// unreachable daemon is reported as "unknown", never asserted dead.
// ArmedHeaters is only ever populated when DaemonAlive is true.
//
// Only get_printer_status populates this (BuildStateBlock never touches it,
// the same pattern the policy-filled Actions field already uses); every
// other tool result leaves it nil.
type WatchdogBlock struct {
	DaemonAlive  *bool              `yaml:"daemon_alive,omitempty"`
	ArmedHeaters []ArmedHeaterBlock `yaml:"armed_heaters,omitempty"`
}

// CameraBlock reports the background daemon hub's live camera connection
// state for one printer (review backlog item 47, dev_docs/camera-keyframe-
// rca.md's item-47 investigation): whether the hub currently has an open
// upstream connection, and when it last actually received video (an access
// unit of any kind, not just a keyframe). This is visibility only, never a
// gate: get_camera_snapshot, open_camera_view, start_recording and the
// stream endpoint each still attempt their own connection and report their
// own timeout if the camera does not answer, regardless of what this block
// last observed.
//
// Like WatchdogBlock, this package never imports internal/daemon; only
// get_printer_status populates it (via Deps.CameraStatus, a bounded,
// non-autostarting query), leaving it nil for every other tool result and
// whenever the daemon could not be reached or nothing is wired up.
type CameraBlock struct {
	Connected         bool     `yaml:"connected"`
	LastMediaAt       string   `yaml:"last_media_at,omitempty"`
	SecondsSinceMedia *float64 `yaml:"seconds_since_media,omitempty"`
}

// CFS states reported in CFSBlock.State.
const (
	CFSStateUnknown = "unknown"
	CFSStateError   = "error"
	CFSStateBusy    = "busy"
	CFSStateIdle    = "idle"
	CFSStateInPrint = "in_print"
)

// CFSBlock is the frontmatter's cfs block, present only while a CFS is
// connected (plan-v0.2.0.md sections 2.5 and 8a.6): State is one of the
// CFSState constants and Reasons names every field behind a non-idle answer.
type CFSBlock struct {
	State   string   `yaml:"state" json:"state"`
	Reasons []string `yaml:"reasons,omitempty" json:"reasons,omitempty"`
}

// StateBlock is the typed YAML frontmatter every tool result embeds
// (dev_docs/safety-architecture.md section 5, references/analysis/11-state-model.md
// section 6). Field order here is the order it renders in, which is
// deliberately fixed (a Go struct, never a map, per 11-state-model.md
// section 6's own note on deterministic YAML key order). Optional fields
// carry omitempty; Actions is left for internal/policy to fill in.
type StateBlock struct {
	// Printer identity.
	PrinterID   string `yaml:"printer_id"`
	PrinterName string `yaml:"printer_name"`
	PrinterHost string `yaml:"printer_host"`
	Hostname    string `yaml:"hostname,omitempty"`
	// VerifiedHostname is the live Klipper hostname this snapshot's own
	// printer/info read returned (review backlog item 24), regardless of
	// whether it matched Hostname; empty whenever that read failed or came
	// back empty. Reported separately from Hostname (the registry's
	// persisted value) so a caller can see what actually answered even
	// while activity_state is identity_mismatch or identity_unverified.
	VerifiedHostname string `yaml:"verified_hostname,omitempty"`
	Model            string `yaml:"model,omitempty"`

	// ServerVersion and ServerPID identify the MCP server process that
	// answered (field feedback item 4): after an update an AI client can keep
	// talking to the old process until it is restarted, and comparing this
	// with `creality-k2-mcp version` shows it. Set by internal/mcpserver;
	// empty from the CLI and the TUI.
	ServerVersion string `yaml:"server_version,omitempty"`
	ServerPID     int    `yaml:"server_pid,omitempty"`

	// Snapshot time.
	SnapshotTime string `yaml:"snapshot_time"`

	// Primary derived state (P2).
	ActivityState string   `yaml:"activity_state"`
	Bucket        string   `yaml:"bucket"`
	GatingClass   string   `yaml:"gating_class"`
	Reasons       []string `yaml:"reasons,omitempty"`
	CFSConnected  bool     `yaml:"cfs_connected"`
	// CFS is set only while a CFS is connected (cfs_connected stays as is).
	CFS *CFSBlock `yaml:"cfs,omitempty"`
	// StartWindow is true while the printer is in the self-test of a print start
	// (state preparing, bucket PP, with print_stats not yet printing): cancel from
	// this server is refused in it, unlike the ordinary START_PRINT prepare phase.
	StartWindow bool `yaml:"start_window,omitempty"`

	// Job identity (nil-able as a group when no job is known).
	Job *JobIdentity `yaml:"job,omitempty"`

	// Temperatures and targets.
	NozzleTemperatureC *float64 `yaml:"nozzle_temperature_c,omitempty"`
	NozzleTargetC      *float64 `yaml:"nozzle_target_c,omitempty"`
	BedTemperatureC    *float64 `yaml:"bed_temperature_c,omitempty"`
	BedTargetC         *float64 `yaml:"bed_target_c,omitempty"`

	// Stored pause targets: only meaningful while paused
	// (gcode_macro PRINTER_PARAM, dev_docs/safety-architecture.md 4.2's
	// resume_print disclosure). Left nil whenever the printer is not paused,
	// even if PRINTER_PARAM happens to report stale nonzero values.
	StoredHotendTargetC *float64 `yaml:"stored_hotend_target_c,omitempty"`
	StoredFan0Percent   *float64 `yaml:"stored_fan0_percent,omitempty"`
	StoredFan2Percent   *float64 `yaml:"stored_fan2_percent,omitempty"`

	// Speed and flow factors.
	SpeedFactorPercent *float64 `yaml:"speed_factor_percent,omitempty"`
	FlowFactorPercent  *float64 `yaml:"flow_factor_percent,omitempty"`

	// SpeedPreset is silent, stable, standard, ultrafast, custom or unknown; it is
	// shown only in buckets P and Z (plan-v0.3.0.md 2a.10). unknown also covers a
	// Moonraker and port 9999 disagreement. SilentMode is the derived Silent flag
	// (on, off or unknown), and SpeedMode9999 and CurFeedratePct9999 are port
	// 9999's own readings when it reported them, so both channels are visible.
	SpeedPreset string `yaml:"speed_preset,omitempty"`
	SilentMode  string `yaml:"silent_mode,omitempty"`
	// SpeedPresetNote is set when Silent's limits are active but the speed factor is
	// not 50% (a CFS filament change runs M220 S100 while Silent stays on, or another
	// client changed it): speed_preset stays silent, and this says what differs.
	SpeedPresetNote    string `yaml:"speed_preset_note,omitempty"`
	SpeedMode9999      *int   `yaml:"speed_mode_9999,omitempty"`
	CurFeedratePct9999 *int   `yaml:"cur_feedrate_pct_9999,omitempty"`
	// ToolheadMaxVelocity is toolhead.max_velocity, information only: the firmware
	// does not clamp velocity in Silent (a file that sets VELOCITY overrides 150),
	// so no confirmation or gate ever reads it (plan-v0.3.0.md 2a.8).
	ToolheadMaxVelocity *float64 `yaml:"toolhead_max_velocity_mm_s,omitempty"`

	// Fans, reported as a percent via domain's fan helpers, and light.
	PartFanPercent      *float64 `yaml:"part_fan_percent,omitempty"`
	CaseFanPercent      *float64 `yaml:"case_fan_percent,omitempty"`
	AuxiliaryFanPercent *float64 `yaml:"auxiliary_fan_percent,omitempty"`
	LightOn             *bool    `yaml:"light_on,omitempty"`

	// Pending action, if any (section 3).
	Pending *PendingActionBlock `yaml:"pending,omitempty"`

	// 9999 reachability.
	Ws9999Reachable bool `yaml:"ws9999_reachable"`

	// CameraVideoFlag mirrors 9999's own "video" push field when present
	// (review backlog item 47, references/analysis/04-creality-ws-camera.md:
	// "video"/"video1" are documented as video/camera-enabled flags). Nil
	// when 9999 never reported it (never reachable, or this build's
	// firmware omits the field). Reported for visibility only, never as a
	// gate or a promise: dev_docs/camera-keyframe-rca.md's item-47
	// investigation found this field does not reliably predict whether the
	// camera is actually producing video right now (see that section for
	// what correlation, if any, was actually observed) - a caller must
	// still expect get_camera_snapshot/open_camera_view/start_recording/the
	// stream endpoint to time out on their own and must not skip trying
	// based on this value alone.
	CameraVideoFlag *int `yaml:"camera_video_flag,omitempty"`

	// Watchdog is D2's idle-heat watchdog liveness and armed heaters
	// (review backlog item 20). Left nil by BuildStateBlock; only
	// get_printer_status ever sets it.
	Watchdog *WatchdogBlock `yaml:"watchdog,omitempty"`

	// Camera is the daemon hub's live camera connection state (review
	// backlog item 47). Left nil by BuildStateBlock; only get_printer_status
	// ever sets it.
	Camera *CameraBlock `yaml:"camera,omitempty"`

	// Recent console activity: informational only, never a gate
	// (dev_docs/safety-architecture.md 3.1).
	RecentActivity []string `yaml:"recent_activity,omitempty"`

	// Actions is populated by internal/policy (policy.Available), never by
	// this package.
	Actions []ActionGate `yaml:"actions,omitempty"`
}

// BuildStateBlock assembles a StateBlock from a Snapshot and its already
// computed Derived state. pending is the same value the caller passed to
// DeriveActivityState, rendered here for display.
func BuildStateBlock(snap Snapshot, derived Derived, pending *PendingAction) StateBlock {
	block := StateBlock{
		PrinterID:        snap.Printer.ID,
		PrinterName:      snap.Printer.Name,
		PrinterHost:      snap.Printer.Host,
		Hostname:         snap.Printer.Hostname,
		Model:            snap.WS9999.Model,
		VerifiedHostname: verifiedHostname(snap),
		SnapshotTime:     snap.Taken.UTC().Format(rfc3339),
		ActivityState:    derived.State,
		Bucket:           string(derived.Bucket),
		GatingClass:      string(derived.Class),
		Reasons:          nonEmptyReasons(derived.Reasons),
		CFSConnected:     derived.CFSConnected,
		CFS:              buildCFSBlock(derived),
		StartWindow:      derived.StartWindow,
		Job:              JobIdentityFrom(snap),
		Ws9999Reachable:  snap.WS9999Reachable,
		RecentActivity:   recentActivity(snap),
	}

	if snap.WS9999.Raw != nil {
		if _, present := snap.WS9999.Raw["video"]; present {
			v := snap.WS9999.Video
			block.CameraVideoFlag = &v
		}
	}

	if snap.Extruder != nil {
		block.NozzleTemperatureC = snap.Extruder.Temperature
		block.NozzleTargetC = snap.Extruder.Target
	}
	if snap.HeaterBed != nil {
		block.BedTemperatureC = snap.HeaterBed.Temperature
		block.BedTargetC = snap.HeaterBed.Target
	}
	if snap.GCodeMove != nil {
		block.SpeedFactorPercent = percentFromFactor(snap.GCodeMove.SpeedFactor)
		block.FlowFactorPercent = percentFromFactor(snap.GCodeMove.ExtrudeFactor)
	}

	if preset := SpeedPresetOf(snap, derived); preset != "" {
		block.SpeedPreset = preset
		block.SilentMode = derived.Qmode.String()
		if preset == PresetSilent && snap.GCodeMove != nil && snap.GCodeMove.SpeedFactor != nil {
			if f := *snap.GCodeMove.SpeedFactor * 100; !presetNear(f, 50) {
				block.SpeedPresetNote = fmt.Sprintf("Silent's limits are active but the speed factor is %.0f%% (a CFS filament change or another client changed it)", f)
			}
		}
		if snap.Toolhead != nil && snap.Toolhead.MaxVelocity > 0 {
			v := snap.Toolhead.MaxVelocity
			block.ToolheadMaxVelocity = &v
		}
		if snap.WS9999Reachable {
			if snap.WS9999.SpeedMode.Present {
				v := snap.WS9999.SpeedMode.Value
				block.SpeedMode9999 = &v
			}
			if snap.WS9999.CurFeedrate.Present {
				v := snap.WS9999.CurFeedrate.Value
				block.CurFeedratePct9999 = &v
			}
		}
	}

	if derived.State == StatePaused && snap.PrinterParam != nil {
		block.StoredHotendTargetC = snap.PrinterParam.HotendTemp
		block.StoredFan0Percent = percentFromUnitFraction(snap.PrinterParam.Fan0Speed)
		block.StoredFan2Percent = percentFromUnitFraction(snap.PrinterParam.Fan2Speed)
	}

	block.PartFanPercent = fanPercent(snap.Fan0, domain.FanPart)
	block.CaseFanPercent = fanPercent(snap.Fan1, domain.FanCase)
	block.AuxiliaryFanPercent = fanPercent(snap.Fan2, domain.FanAuxiliary)

	if snap.LED != nil && snap.LED.Value != nil {
		on := *snap.LED.Value != 0
		block.LightOn = &on
	}

	block.Pending = buildPendingBlock(pending, snap.Taken)

	return block
}

// buildCFSBlock renders the cfs block, nil when no CFS is connected. The state is
// derived bucket-first (plan 8a.6): CFSQuiescent requires deviceState 0, which
// is presumably false during every print, so in a print bucket (P, PP, Z) the
// answer is in_print unless the CFS is not Known (unknown) or has an Error
// (error); tool changes and physical slot presence are undetectable. In bucket
// I it is unknown, error, busy (not quiescent) or idle. For every other
// bucket (U, E, B, T) the plan is silent: unknown and error still win, and
// otherwise the answer is busy, since such a printer is neither idle nor in a
// print and the fail-closed reading is "do not treat the CFS as at rest".
func buildCFSBlock(d Derived) *CFSBlock {
	if !d.CFSConnected {
		return nil
	}
	state := CFSStateBusy
	switch {
	case !d.CFSKnown:
		state = CFSStateUnknown
	case d.CFSError:
		state = CFSStateError
	case d.Bucket == BucketP || d.Bucket == BucketPP || d.Bucket == BucketZ:
		state = CFSStateInPrint
	case d.Bucket == BucketI && d.CFSQuiescent:
		state = CFSStateIdle
	}
	return &CFSBlock{State: state, Reasons: nonEmptyReasons(d.CFSReasons)}
}

// verifiedHostname reports the hostname snap's own printer/info read
// returned, or "" when that read failed or came back empty (review backlog
// item 24).
func verifiedHostname(snap Snapshot) string {
	if snap.PrinterInfoErr != nil {
		return ""
	}
	return strings.TrimSpace(snap.PrinterInfo.Hostname)
}

// UnverifiedIdentity is what Identity returns whenever a snapshot's own
// printer/info read could not verify a live Klipper hostname (review
// backlog item 31): a read-only caller shows this in place of a key rather
// than falling back to the printer's raw configured host, since the host is
// not verified to be answering as this printer right now.
const UnverifiedIdentity = "unverified"

// Identity returns the printer identity used for locks, watchdog keys,
// pending-action tracking and upload tracking, for a read-only caller that
// only has a snapshot to work from (policy.Available's pending-action
// lookup, get_printer_status's watchdog query, doctorchecks): the verified
// live Klipper hostname this snapshot's own printer/info read returned (the
// same value BuildStateBlock renders as verified_hostname), or
// UnverifiedIdentity when that read failed or came back empty. This
// replaces the three separate "hostname, else host" copies that used to
// live in internal/policy, internal/mcpserver and internal/doctorchecks
// (review backlog item 31): none of those keys may ever be the raw
// configured host, since the host is not verified to be answering as this
// printer right now.
//
// A write path does not call this: internal/policy's Execute resolves and
// verifies its own identity fresh, before ever taking a snapshot, via
// resolveExecuteIdentity, and threads that already-verified value through
// every send function instead, rather than trusting a snapshot that may be
// stale by the time the write actually happens. The two agree whenever the
// same printer answers both reads, which is the common case this function
// exists to keep consistent: a status query must report against the same
// watchdog key Execute just armed or disarmed.
func Identity(snap Snapshot) string {
	if v := verifiedHostname(snap); v != "" {
		return v
	}
	return UnverifiedIdentity
}

func nonEmptyReasons(reasons []string) []string {
	var out []string
	for _, r := range reasons {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// fanPercent converts a decoded output_pin value (0 to 1) to a user-facing
// percent via domain's verified M106 mapping (domain.ReportedFanPercent). A
// nil pin or a channel this package does not recognise yields nil, never a
// fabricated 0.
func fanPercent(pin *moonraker.OutputPin, channel domain.FanChannel) *float64 {
	if pin == nil || pin.Value == nil {
		return nil
	}
	spec, ok := domain.FanSpecFor(channel)
	if !ok {
		return nil
	}
	pct := domain.ReportedFanPercent(*pin.Value, spec.MinValue)
	return &pct
}

// percentFromFactor converts a Klipper factor (1.0 == 100%) to a percent.
func percentFromFactor(factor *float64) *float64 {
	if factor == nil {
		return nil
	}
	pct := *factor * 100
	return &pct
}

// percentFromUnitFraction converts a PRINTER_PARAM stored fan speed (0 to 1,
// the same unit fan0_speed/fan2_speed use) to a percent.
func percentFromUnitFraction(fraction *float64) *float64 {
	if fraction == nil {
		return nil
	}
	pct := *fraction * 100
	return &pct
}

// recentActivity summarises the gcode_store tail as plain message strings,
// informational only (dev_docs/safety-architecture.md 3.1: "recent console
// activity", never a gate). A failed or empty read yields nil, not an error:
// this field is a nicety, not a safety input.
func recentActivity(snap Snapshot) []string {
	if snap.GCodeStoreErr != nil || len(snap.GCodeStoreTail) == 0 {
		return nil
	}
	out := make([]string, 0, len(snap.GCodeStoreTail))
	for _, e := range CollapseTemperatureReports(snap.GCodeStoreTail, false) {
		out = append(out, e.Message)
	}
	return out
}

// buildPendingBlock renders a PendingAction for the frontmatter, resolving
// elapsed/timeout to milliseconds as of now. A nil or PendingNone action
// renders as nil (the "pending,omitempty" field is simply absent).
func buildPendingBlock(p *PendingAction, now time.Time) *PendingActionBlock {
	if p == nil || p.Kind == PendingNone {
		return nil
	}
	return &PendingActionBlock{
		Kind:      string(p.Kind),
		IssuedAt:  p.IssuedAt.Format(rfc3339),
		ElapsedMs: p.Elapsed(now).Milliseconds(),
		TimeoutMs: p.Timeout.Milliseconds(),
	}
}
