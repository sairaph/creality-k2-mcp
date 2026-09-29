package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/policy"
)

// set_speed_preset tool tests (plan-v0.3.0.md and 2a). They use the control
// fake (tools_control_test.go), whose 9999 side follows the firmware's Qmode.

func presetTool(t *testing.T, cs *mcp.ClientSession) *mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "set_speed_preset" {
			return tool
		}
	}
	t.Fatal("set_speed_preset is not registered")
	return nil
}

func TestSetSpeedPresetToolDescriptionDisclosesSilent(t *testing.T) {
	deps, _ := controlDeps(t, true, nil)
	tool := presetTool(t, controlSession(t, deps))
	lower := strings.ToLower(tool.Description)
	for _, want := range []string{
		"speed_mode.json", "power-loss-resume hint", "leaving silent does not clear", "(even of another print)",
		"velocity 150 mm/s", "acceleration to 2500", "pressure advance 0.05", "all three fans", "corner velocity",
		"stale after a cfs filament change", "only available while printing", "stuck_silent_possible", "set_speed_factor is refused",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("description lacks %q", want)
		}
	}
	if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint {
		t.Errorf("annotations = %+v, want not read-only, not destructive, idempotent", tool.Annotations)
	}
	schema, _ := json.Marshal(tool.InputSchema)
	for _, want := range []string{`"silent"`, `"stable"`, `"standard"`, `"ultrafast"`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("input schema lacks the preset enum value %s: %s", want, schema)
		}
	}
}

func TestSetSpeedPresetHappyPathAndBothChannels(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_speed_preset", map[string]any{"preset": "ultrafast"})
	text := replyText(res)
	if res.IsError {
		t.Fatalf("set_speed_preset failed: %s", text)
	}
	st.mu.Lock()
	got := st.speedFactor
	st.mu.Unlock()
	if got != 1.25 {
		t.Fatalf("speedFactor = %v, want 1.25", got)
	}
	for _, want := range []string{"effect: confirmed", "preset_result:", "moonraker_silent:", "speed_preset: ultrafast", "silent_mode:"} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
}

func TestSetSpeedPresetSilentAndLeaveIt(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)

	res := call(t, cs, "set_speed_preset", map[string]any{"preset": "silent"})
	text := replyText(res)
	if res.IsError || !strings.Contains(text, "effect: confirmed") || !strings.Contains(text, "speed_mode.json") {
		t.Fatalf("silent: %s", text)
	}
	st.mu.Lock()
	silent, frames := st.silent, append([]bool(nil), st.speedFrames...)
	st.mu.Unlock()
	if !silent || len(frames) != 1 || !frames[0] {
		t.Fatalf("silent=%v frames=%v", silent, frames)
	}

	// While Silent is on, set_speed_factor is refused with the S9 text.
	res = call(t, cs, "set_speed_factor", map[string]any{"percent": 100})
	text = replyText(res)
	if !res.IsError || !strings.Contains(text, "code: unavailable") || !strings.Contains(text, "use set_speed_preset") {
		t.Fatalf("set_speed_factor while Silent = %s", text)
	}

	res = call(t, cs, "set_speed_preset", map[string]any{"preset": "standard"})
	text = replyText(res)
	if res.IsError || !strings.Contains(text, "effect: confirmed") {
		t.Fatalf("leaving Silent: %s", text)
	}
	st.mu.Lock()
	silent, factor := st.silent, st.speedFactor
	st.mu.Unlock()
	if silent || factor != 1.0 {
		t.Fatalf("after leaving: silent=%v factor=%v", silent, factor)
	}
}

func TestSetSpeedPresetNoChangeUsesTheGenericOutcome(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	cs := controlSession(t, deps)
	res := call(t, cs, "set_speed_preset", map[string]any{"preset": "standard"})
	text := replyText(res)
	if res.IsError || !strings.Contains(text, "effect: no_change") || !strings.Contains(text, "nothing was sent") ||
		strings.Contains(text, "slot") {
		t.Fatalf("no_change reply = %s", text)
	}
	st.mu.Lock()
	n := len(st.speedFrames)
	st.mu.Unlock()
	if n != 0 {
		t.Fatalf("frames sent for a no_change: %d", n)
	}
}

func TestSetSpeedPresetRefusals(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	cs := controlSession(t, deps)
	// Idle.
	res := call(t, cs, "set_speed_preset", map[string]any{"preset": "standard"})
	if !res.IsError || !strings.Contains(replyText(res), "code: unavailable") {
		t.Fatalf("idle: %s", replyText(res))
	}
	// Paused.
	setPaused(st)
	res = call(t, cs, "set_speed_preset", map[string]any{"preset": "standard"})
	if !res.IsError || !strings.Contains(replyText(res), "code: unavailable") {
		t.Fatalf("paused: %s", replyText(res))
	}
	// Unknown Silent state while printing (both sources missing): fail closed.
	setPrinting(st)
	st.mu.Lock()
	st.qmodeOmit = true
	st.mu.Unlock()
	res = call(t, cs, "set_speed_preset", map[string]any{"preset": "standard"})
	if !res.IsError || !strings.Contains(replyText(res), "could not be read") {
		t.Fatalf("unknown Silent: %s", replyText(res))
	}
	res = call(t, cs, "set_speed_factor", map[string]any{"percent": 110})
	if !res.IsError || !strings.Contains(replyText(res), "could not be read") {
		t.Fatalf("set_speed_factor with an unknown Silent state: %s", replyText(res))
	}
	// An invalid preset never reaches the printer.
	res = call(t, cs, "set_speed_preset", map[string]any{"preset": "turbo"})
	if !res.IsError {
		t.Fatalf("invalid preset accepted: %s", replyText(res))
	}
	// Control off.
	deps2, _ := controlDeps(t, false, nil)
	res = call(t, controlSession(t, deps2), "set_speed_preset", map[string]any{"preset": "standard"})
	if !res.IsError || !strings.Contains(replyText(res), "code: forbidden") {
		t.Fatalf("control off: %s", replyText(res))
	}
}

// 2a.10 and 2a.9: speed_preset is shown while printing and paused and never
// while idle; an idle printer with Silent left on gets the stuck warning.
func TestStatusShowsSpeedPresetOnlyWhilePrintingAndWarnsAboutStuckSilent(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	cs := controlSession(t, deps)

	text := replyText(call(t, cs, "get_printer_status", nil))
	if strings.Contains(text, "speed_preset:") || strings.Contains(text, "Silent mode is still on") {
		t.Fatalf("idle status shows a preset or a warning:\n%s", text)
	}
	setPrinting(st)
	text = replyText(call(t, cs, "get_printer_status", nil))
	if !strings.Contains(text, "speed_preset: standard") || !strings.Contains(text, "set_speed_preset switches Creality's speed presets") {
		t.Fatalf("printing status:\n%s", text)
	}
	st.mu.Lock()
	st.silent, st.savedFactor, st.speedFactor = true, 1, 0.5
	st.mu.Unlock()
	text = replyText(call(t, cs, "get_printer_status", nil))
	if !strings.Contains(text, "speed_preset: silent") || !strings.Contains(text, "Silent mode is on: set_speed_factor is refused") {
		t.Fatalf("silent status:\n%s", text)
	}
	setPaused(st)
	text = replyText(call(t, cs, "get_printer_status", nil))
	if !strings.Contains(text, "speed_preset: silent") {
		t.Fatalf("paused status does not show the preset:\n%s", text)
	}
	// The print ends with Silent left on: idle plus the stuck warning, no preset shown.
	st.mu.Lock()
	st.printStatsState, st.vsdActive, st.isPaused = "complete", false, false
	st.mu.Unlock()
	text = replyText(call(t, cs, "get_printer_status", nil))
	if strings.Contains(text, "speed_preset:") {
		t.Fatalf("idle status shows a preset:\n%s", text)
	}
	for _, want := range []string{"Silent mode is still on while the printer is not printing", "firmware restart or a power cycle clears it", "cannot clear it"} {
		if !strings.Contains(text, want) {
			t.Errorf("stuck Silent guidance lacks %q:\n%s", want, text)
		}
	}
	// A warning, not a refusal: the actions list still offers start_print.
	if !strings.Contains(text, "start_print") {
		t.Errorf("actions list missing start_print:\n%s", text)
	}
}

// m-4: a confirmed effect with accepted false never reads as a rejection.
func TestControlBodyConfirmedWithoutAcceptedIsNotARejection(t *testing.T) {
	res := policy.Result{
		Action: policy.ActionSetSpeedPreset, Accepted: false, Effect: "confirmed",
		Effects: []string{"the M220 call reported an error but Moonraker reads the target speed factor"},
		Printer: domain.Printer{Name: "k2"},
	}
	body := controlBody(res)
	if strings.Contains(body, "rejected") || strings.Contains(body, "failed to accept") {
		t.Fatalf("body calls a confirmed effect a rejection: %s", body)
	}
	for _, want := range []string{"confirmed", "the M220 call reported an error but Moonraker reads the target speed factor"} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q: %s", want, body)
		}
	}
	// Unconfirmed and not accepted keeps the old wording for the other actions.
	bad := controlBody(policy.Result{Action: policy.ActionSetFanSpeed, Accepted: false, Effect: "unconfirmed", Printer: domain.Printer{Name: "k2"}})
	if !strings.Contains(bad, "rejected or failed to accept") {
		t.Errorf("unconfirmed rejection lost its wording: %s", bad)
	}
}

// The preempted, partial and stuck outcomes each have their own reply text.
func TestControlBodyPreemptedPartialAndStuck(t *testing.T) {
	printer := domain.Printer{Name: "k2"}
	for effect, want := range map[string]string{
		"preempted":             "interrupted",
		"partial":               "PARTLY applied",
		"stuck_silent_possible": "STUCK ON",
	} {
		body := controlBody(policy.Result{Action: policy.ActionSetSpeedPreset, Effect: effect, Printer: printer, Effects: []string{"note-" + effect}})
		if !strings.Contains(body, want) || !strings.Contains(body, "note-"+effect) {
			t.Errorf("%s body = %s", effect, body)
		}
	}
}

// The status guidance repeats it while printing.
func TestStatusSaysWhenSilentRunsAtAnotherFactor(t *testing.T) {
	deps, st := controlDeps(t, true, nil)
	setPrinting(st)
	st.mu.Lock()
	st.silent, st.savedFactor, st.speedFactor = true, 1, 1.0 // a swap ran M220 S100 under Silent
	st.mu.Unlock()
	text := replyText(call(t, controlSession(t, deps), "get_printer_status", nil))
	want := "Silent's limits are active but the speed factor is 100% (a CFS filament change or another client changed it)"
	if !strings.Contains(text, "speed_preset: silent") || strings.Count(text, want) < 2 {
		t.Fatalf("status lacks the note in the frontmatter and the guidance:\n%s", text)
	}
}
