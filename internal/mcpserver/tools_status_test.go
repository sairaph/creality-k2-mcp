package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/policy"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// rawQueryResp mirrors the top-level shape of a Moonraker
// /printer/objects/query response, letting tests mutate individual objects
// (print_stats.state, pause_resume.is_paused, ...) on top of the real
// objects_query_full.json fixture instead of hand-building a whole new
// capture for every activity state a test needs to exercise.
type rawQueryResp struct {
	Result struct {
		Eventtime float64                    `json:"eventtime"`
		Status    map[string]json.RawMessage `json:"status"`
	} `json:"result"`
}

// mutateObject decodes status[key] into a generic map, applies mutate, and
// re-encodes it back into status[key].
func mutateObject(t *testing.T, status map[string]json.RawMessage, key string, mutate func(map[string]any)) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(status[key], &m); err != nil {
		t.Fatalf("decode object %q for mutation: %v", key, err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode mutated object %q: %v", key, err)
	}
	status[key] = b
}

// objectsQueryOverride builds an HTTP handler serving the real
// objects_query_full.json fixture with the named objects mutated, for tests
// that need a specific activity state (printing, paused, ...) rather than
// the fixture's own idle snapshot.
func objectsQueryOverride(t *testing.T, mutations map[string]func(map[string]any)) http.HandlerFunc {
	t.Helper()
	base := fixture(t, "moonraker", "objects_query_full.json")
	var resp rawQueryResp
	if err := json.Unmarshal(base, &resp); err != nil {
		t.Fatalf("decode objects_query_full.json fixture: %v", err)
	}
	for key, fn := range mutations {
		mutateObject(t, resp.Result.Status, key, fn)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("re-encode mutated objects query fixture: %v", err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}
}

func printingOverrides() map[string]func(map[string]any) {
	return map[string]func(map[string]any){
		"print_stats": func(m map[string]any) {
			m["state"] = "printing"
			m["print_duration"] = 120.0
			m["filename"] = "FigureJoints5mmball.stl_PLA_41m51s.gcode"
		},
		"pause_resume": func(m map[string]any) { m["is_paused"] = false },
		"virtual_sdcard": func(m map[string]any) {
			m["is_active"] = true
			m["layer"] = 10.0
			m["layer_count"] = 149.0
			m["progress"] = 0.2
		},
	}
}

func pausedOverrides(hotendTarget float64) map[string]func(map[string]any) {
	return map[string]func(map[string]any){
		"print_stats": func(m map[string]any) {
			m["state"] = "paused"
			m["print_duration"] = 200.0
			m["filename"] = "FigureJoints5mmball.stl_PLA_41m51s.gcode"
		},
		"pause_resume":   func(m map[string]any) { m["is_paused"] = true },
		"virtual_sdcard": func(m map[string]any) { m["is_active"] = true },
		"gcode_macro PRINTER_PARAM": func(m map[string]any) {
			m["hotend_temp"] = hotendTarget
		},
	}
}

func errorStateOverrides() map[string]func(map[string]any) {
	return map[string]func(map[string]any){
		"print_stats": func(m map[string]any) { m["state"] = "error" },
	}
}

// --- statusGuidance confirm_token wording ---

// TestStatusGuidanceNamesConfirmTokenNotConfirmCancel guards against
// statusGuidance telling the AI cancel_print takes a confirm: "CANCEL"
// argument, a flow this server never implemented. cancel_print actually
// uses the same two-step confirm_token proposal flow as resume_print and
// exclude_object: call once with no confirm_token to receive a proposal,
// then call again with that proposal's confirm_token to actually cancel.
func TestStatusGuidanceNamesConfirmTokenNotConfirmCancel(t *testing.T) {
	for _, state := range []string{
		printerstate.StatePrinting, printerstate.StatePreparing, printerstate.StatePaused,
	} {
		block := printerstate.StateBlock{ActivityState: state}
		body := statusGuidance(block)
		if !strings.Contains(body, "confirm_token") {
			t.Fatalf("statusGuidance(%s) = %q, want it to mention confirm_token", state, body)
		}
		if strings.Contains(body, "CANCEL") {
			t.Fatalf("statusGuidance(%s) = %q, still mentions the old confirm: \"CANCEL\" flow", state, body)
		}
		if strings.Contains(body, "confirm:") {
			t.Fatalf("statusGuidance(%s) = %q, still mentions a confirm string parameter", state, body)
		}
	}
}

// TestStatusGuidancePausedSpellsOutResumeTwoStep guards against paused
// guidance naming resume_print's confirm_token flow only in passing: it must
// spell out resume_print's own two-step (call once with no confirm_token,
// then again with the returned confirm_token) exactly as explicitly as the
// cancel_print wording right next to it, not just point at cancel_print's
// steps and leave resume_print's implied (review backlog item 17).
func TestStatusGuidancePausedSpellsOutResumeTwoStep(t *testing.T) {
	block := printerstate.StateBlock{ActivityState: printerstate.StatePaused}
	body := statusGuidance(block)
	for _, want := range []string{
		"resume_print needs the two-step proposal flow",
		"call it once with no confirm_token to see what will happen, then again with the returned confirm_token to actually resume",
		"cancel_print uses the same two-step proposal flow",
		"call it once with no confirm_token to see what will happen, then again with the returned confirm_token to actually cancel",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("statusGuidance(paused) = %q, want it to contain %q", body, want)
		}
	}
}

// --- statusGuidance for PP, B, T, generic U, and the CFS-connected note
// (review backlog item 10) ---

// actionNamesMentioned returns which of internal/policy's action tool names
// appear anywhere in body, so a test can check statusGuidance's prose
// against internal/policy's own AllowedBuckets table (spec.go) instead of
// trusting the prose by itself.
func actionNamesMentioned(body string) []policy.ActionName {
	var found []policy.ActionName
	for _, name := range policy.Actions {
		if strings.Contains(body, string(name)) {
			found = append(found, name)
		}
	}
	return found
}

// assertGuidanceSuggestsOnlyLegalActions confirms every action name
// statusGuidance's body mentions is not "blocked" for derived, per
// policy.AvailableFor - the same pure gate get_printer_status's own actions
// list is built from (deps.go's policyAvailableAdapter) - so a guidance
// wording change can never drift into naming a call the bucket forbids.
func assertGuidanceSuggestsOnlyLegalActions(t *testing.T, body string, derived printerstate.Derived) {
	t.Helper()
	gates := policy.AvailableFor(derived, domain.DefaultSettings())
	blocked := make(map[string]string, len(gates))
	for _, g := range gates {
		if g.Status == "blocked" {
			blocked[g.Name] = g.Reason
		}
	}
	mentioned := actionNamesMentioned(body)
	if len(mentioned) == 0 {
		t.Errorf("statusGuidance for %s (bucket %s) mentions no action tool name at all; want it to name at least the legal one(s) or none by design - check the test's own expectation", derived.State, derived.Bucket)
	}
	for _, name := range mentioned {
		if reason, isBlocked := blocked[string(name)]; isBlocked {
			t.Errorf("statusGuidance for %s (bucket %s) mentions %q, but internal/policy blocks it: %s", derived.State, derived.Bucket, name, reason)
		}
	}
}

func TestStatusGuidancePreparingOnlySuggestsCancel(t *testing.T) {
	derived := printerstate.Derived{State: printerstate.StatePreparing, Bucket: printerstate.BucketPP, Class: printerstate.ClassBusy}
	block := printerstate.StateBlock{ActivityState: derived.State}
	body := statusGuidance(block)

	if !strings.Contains(body, "cancel_print") {
		t.Fatalf("statusGuidance(preparing) = %q, want it to mention cancel_print (allowed in bucket PP)", body)
	}
	for _, illegal := range []string{"pause_print", "resume_print", "set_nozzle_temperature", "set_bed_temperature"} {
		if strings.Contains(body, illegal) {
			t.Fatalf("statusGuidance(preparing) = %q, must not mention %q (not allowed in bucket PP)", body, illegal)
		}
	}
	assertGuidanceSuggestsOnlyLegalActions(t, body, derived)
}

func TestStatusGuidanceBusyStatesOnlyMentionSetLight(t *testing.T) {
	for _, state := range []string{printerstate.StateHoming, printerstate.StateCalibrating, printerstate.StateBusyCommand} {
		derived := printerstate.Derived{State: state, Bucket: printerstate.BucketB, Class: printerstate.ClassBusy}
		block := printerstate.StateBlock{ActivityState: state}
		body := statusGuidance(block)

		if !strings.Contains(body, "set_light") {
			t.Fatalf("statusGuidance(%s) = %q, want it to mention set_light (the one action allowed in bucket B)", state, body)
		}
		for _, illegal := range []string{"pause_print", "resume_print", "cancel_print", "start_print"} {
			if strings.Contains(body, illegal) {
				t.Fatalf("statusGuidance(%s) = %q, must not mention %q (blocked in bucket B)", state, body, illegal)
			}
		}
		assertGuidanceSuggestsOnlyLegalActions(t, body, derived)
	}
}

func TestStatusGuidanceTransitioningStatesSuggestNoWrite(t *testing.T) {
	for _, state := range []string{printerstate.StateCancelling, printerstate.StatePausing, printerstate.StateResuming} {
		derived := printerstate.Derived{State: state, Bucket: printerstate.BucketT, Class: printerstate.ClassTransitioning}
		block := printerstate.StateBlock{ActivityState: state}
		body := statusGuidance(block)

		if !strings.Contains(body, "not settled yet") && !strings.Contains(body, "get_printer_status again") {
			t.Fatalf("statusGuidance(%s) = %q, want it to say to poll get_printer_status rather than write", state, body)
		}
		for _, name := range policy.Actions {
			if name == policy.ActionSetLight {
				continue // set_light stays legal in every bucket but offline
			}
			if strings.Contains(body, string(name)) {
				t.Fatalf("statusGuidance(%s) = %q, must not mention %q (bucket T blocks every write except set_light)", state, body, name)
			}
		}
		// Bucket T's own guidance intentionally suggests no write at all
		// (poll and retry), not even set_light, so the generic
		// "mentions at least one legal action" check does not apply here;
		// only confirm nothing illegal is named, done above.
		gates := policy.AvailableFor(derived, domain.DefaultSettings())
		for _, g := range gates {
			if g.Status == "blocked" && strings.Contains(body, g.Name) {
				t.Fatalf("statusGuidance(%s) mentions blocked action %q: %s", state, g.Name, g.Reason)
			}
		}
	}
}

func TestStatusGuidanceKlippyNotReadySuggestsNoWrite(t *testing.T) {
	derived := printerstate.Derived{State: printerstate.StateKlippyNotReady, Bucket: printerstate.BucketU, Class: printerstate.ClassUnknownFailClosed}
	block := printerstate.StateBlock{ActivityState: derived.State}
	body := statusGuidance(block)

	if !strings.Contains(body, "Klipper is not ready") {
		t.Fatalf("statusGuidance(klippy_not_ready) = %q, want it to explain Klipper is not ready", body)
	}
	for _, name := range policy.Actions {
		if strings.Contains(body, string(name)) {
			t.Fatalf("statusGuidance(klippy_not_ready) = %q, must not mention %q (bucket U blocks every write except set_light, and this guidance suggests none)", body, name)
		}
	}
}

func TestStatusGuidanceGenericUnknownSuggestsNoWrite(t *testing.T) {
	// An activity state this switch's cases do not recognise at all (never
	// produced by DeriveActivityState itself, which always emits one of the
	// 19 named states) exercises statusGuidance's own default branch, the
	// same fail-closed "unknown" fallback bucket U ultimately maps every
	// unrecognised case to.
	derived := printerstate.Derived{State: "some_future_state_this_build_does_not_know", Bucket: printerstate.BucketU, Class: printerstate.ClassUnknownFailClosed}
	block := printerstate.StateBlock{ActivityState: derived.State}
	body := statusGuidance(block)

	if !strings.Contains(body, "treated as unknown") || !strings.Contains(body, "fail closed") {
		t.Fatalf("statusGuidance(unrecognised state) = %q, want the fail-closed unknown wording", body)
	}
	for _, name := range policy.Actions {
		if strings.Contains(body, string(name)) {
			t.Fatalf("statusGuidance(unrecognised state) = %q, must not mention %q (bucket U blocks every write except set_light, and this guidance suggests none)", body, name)
		}
	}
}

// TestStatusGuidanceIdentityStatesRefuseEveryAction pins review backlog
// item 24's guidance branch: identity_mismatch and identity_unverified both
// tell the AI to re-run discovery, and unlike every other bucket U state
// (which still allows set_light), no action tool name is suggested at all -
// policy.checkGate refuses set_light too here, since the printer's physical
// identity itself is uncertain.
func TestStatusGuidanceIdentityStatesRefuseEveryAction(t *testing.T) {
	for _, state := range []string{printerstate.StateIdentityMismatch, printerstate.StateIdentityUnverified} {
		block := printerstate.StateBlock{ActivityState: state}
		body := statusGuidance(block)

		if !strings.Contains(body, "identity") || !strings.Contains(body, "printers scan") {
			t.Fatalf("statusGuidance(%s) = %q, want it to mention identity verification and re-running discovery (printers scan)", state, body)
		}
		for _, name := range policy.Actions {
			if strings.Contains(body, string(name)) {
				t.Fatalf("statusGuidance(%s) = %q, must not mention %q (identity mismatch/unverified refuses every write, including set_light)", state, body, name)
			}
		}
	}
}

// TestStatusGuidanceCFSConnectedNote confirms the per-state CFS guidance
// (dev_docs/plan-v0.2.0.md 2.5, 3.1, 8a.6) says what works for each cfs state
// and activity state, and never claims a tool the policy's own gates refuse
// (surface review M1): paused, the start window and printing differ.
func TestStatusGuidanceCFSConnectedNote(t *testing.T) {
	cases := []struct {
		name     string
		activity string
		window   bool
		cfs      string
		want     []string
		never    []string
	}{
		{"idle", printerstate.StateIdle, false, printerstate.CFSStateIdle, []string{"two-step mapping proposal", "set_filament_definition", "flow factor is never changed"}, nil},
		{"printing", printerstate.StatePrinting, false, printerstate.CFSStateInPrint, []string{"set_fan_speed, set_speed_factor and exclude_object are available",
			"pause_print and cancel_print are never blocked by a CFS signal", "nozzle temperature and the flow factor cannot be changed", "clean pause this server issued"}, nil},
		{"paused", printerstate.StatePaused, false, printerstate.CFSStateInPrint, []string{"The print is paused", "cancel_print", "resume_print only for a clean pause this server issued",
			"Pause, fan and speed changes are not available while paused", "printer screen or in Creality Print"}, []string{"are never blocked", "set_speed_factor and exclude_object are available", "always work"}},
		{"start window", printerstate.StatePreparing, true, printerstate.CFSStateInPrint, []string{"start window", "set_light", "cancel_print from this server is refused", "stop it on the printer screen"},
			[]string{"are never blocked", "always work", "set_fan_speed"}},
		{"ordinary prepare", printerstate.StatePreparing, false, printerstate.CFSStateInPrint, []string{"cancel_print works", "every setpoint is refused"}, []string{"stop it on the printer screen"}},
		{"busy", printerstate.StateFilamentOperation, false, printerstate.CFSStateBusy, []string{"busy", "refused until it settles"}, nil},
		{"error", printerstate.StateIdle, false, printerstate.CFSStateError, []string{"reports an error", "printer screen or in Creality Print", "While a print is running, pause_print and cancel_print are not blocked by it"}, []string{"stay available", "always"}},
		{"unknown", printerstate.StateIdle, false, printerstate.CFSStateUnknown, []string{"could not be fully read", "fail closed"}, nil},
	}
	for _, tc := range cases {
		block := printerstate.StateBlock{ActivityState: tc.activity, CFSConnected: true, StartWindow: tc.window,
			CFS: &printerstate.CFSBlock{State: tc.cfs, Reasons: []string{"why-" + tc.cfs}}}
		body := statusGuidance(block)
		for _, want := range append(append([]string(nil), tc.want...), "get_filaments", "cfs state: "+tc.cfs, "Not detectable from here", "a tool change") {
			if !strings.Contains(body, want) {
				t.Errorf("%s: guidance = %q, missing %q", tc.name, body, want)
			}
		}
		for _, bad := range append(append([]string(nil), tc.never...), "later version", "additionally blocks") {
			if strings.Contains(body, bad) {
				t.Errorf("%s: guidance = %q, must not contain %q", tc.name, body, bad)
			}
		}
	}

	// The claims agree with the policy's own gates.
	gateStatuses := func(d printerstate.Derived) map[string]string {
		out := map[string]string{}
		for _, g := range policy.AvailableFor(d, domain.DefaultSettings()) {
			out[g.Name] = g.Status
		}
		return out
	}
	base := printerstate.Derived{CFSConnected: true, CFSKnown: true, CFSQuiescent: false}
	printing := base
	printing.State, printing.Bucket, printing.Class = printerstate.StatePrinting, printerstate.BucketP, printerstate.ClassBusy
	g := gateStatuses(printing)
	for _, n := range []policy.ActionName{policy.ActionSetFanSpeed, policy.ActionSetSpeedFactor, policy.ActionExcludeObject} {
		if g[string(n)] == "blocked" {
			t.Errorf("printing: %s blocked but the guidance says it is available", n)
		}
	}
	if g[string(policy.ActionPausePrint)] == "blocked" || g[string(policy.ActionCancelPrint)] == "blocked" {
		t.Errorf("printing: pause or cancel blocked")
	}
	paused := base
	paused.State, paused.Bucket, paused.Class = printerstate.StatePaused, printerstate.BucketZ, printerstate.ClassBusy
	g = gateStatuses(paused)
	for _, n := range []policy.ActionName{policy.ActionPausePrint, policy.ActionSetFanSpeed, policy.ActionSetSpeedFactor, policy.ActionSetNozzleTemperature, policy.ActionSetFlowFactor} {
		if g[string(n)] != "blocked" {
			t.Errorf("paused: %s = %q, the guidance says it is not available", n, g[string(n)])
		}
	}
	if g[string(policy.ActionCancelPrint)] != "needs_confirmation" || g[string(policy.ActionExcludeObject)] != "needs_confirmation" {
		t.Errorf("paused: cancel %q exclude %q", g[string(policy.ActionCancelPrint)], g[string(policy.ActionExcludeObject)])
	}
	if g[string(policy.ActionResumePrint)] != "blocked" {
		t.Errorf("paused without a pause record: resume = %q, want blocked", g[string(policy.ActionResumePrint)])
	}
	paused.PauseRecorded = true
	if got := gateStatuses(paused)[string(policy.ActionResumePrint)]; got != "needs_confirmation" {
		t.Errorf("paused with a pause record: resume = %q, want needs_confirmation", got)
	}
	window := base
	window.State, window.Bucket, window.Class, window.StartWindow = printerstate.StatePreparing, printerstate.BucketPP, printerstate.ClassBusy, true
	g = gateStatuses(window)
	if g[string(policy.ActionCancelPrint)] != "blocked" || g[string(policy.ActionSetNozzleTemperature)] != "blocked" {
		t.Errorf("start window: cancel %q nozzle %q, want both blocked", g[string(policy.ActionCancelPrint)], g[string(policy.ActionSetNozzleTemperature)])
	}
}

// An idle printer with the CFS connected is steered to start_print as the
// two-step mapping proposal only when the CFS is idle; a busy, error or unknown
// CFS says start_print is refused instead. The filament_operation state has its
// own wording instead of the v0.1.0 "cannot safely act" text.
func TestStatusGuidanceIdleWithCFSExplainsTheMappingProposal(t *testing.T) {
	for _, state := range []string{printerstate.StateIdle, printerstate.StateComplete, printerstate.StateCancelled} {
		body := statusGuidance(printerstate.StateBlock{ActivityState: state, CFSConnected: true,
			CFS: &printerstate.CFSBlock{State: printerstate.CFSStateIdle}})
		if strings.Contains(body, "safe to start") {
			t.Errorf("statusGuidance(%s, CFS connected) = %q, must not call the printer safe to start", state, body)
		}
		for _, want := range []string{string(policy.ActionStartPrint), "two-step mapping proposal"} {
			if !strings.Contains(body, want) {
				t.Errorf("statusGuidance(%s, CFS connected) = %q, missing %q", state, body, want)
			}
		}
		for _, cfs := range []string{printerstate.CFSStateBusy, printerstate.CFSStateError, printerstate.CFSStateUnknown} {
			body := statusGuidance(printerstate.StateBlock{ActivityState: state, CFSConnected: true, CFS: &printerstate.CFSBlock{State: cfs}})
			if !strings.Contains(body, "not ready for a start") || strings.Contains(body, "then start_print") {
				t.Errorf("statusGuidance(%s, cfs %s) = %q, want the not-ready wording without steering to start_print", state, cfs, body)
			}
		}
	}
	for _, state := range []string{printerstate.StateFilamentOperation, printerstate.StateCFSOperation} {
		body := statusGuidance(printerstate.StateBlock{ActivityState: state, CFSConnected: true,
			CFS: &printerstate.CFSBlock{State: printerstate.CFSStateBusy}})
		for _, want := range []string{"moving filament", "printer screen", "get_filaments"} {
			if !strings.Contains(body, want) {
				t.Errorf("statusGuidance(%s) = %q, missing %q", state, body, want)
			}
		}
		if strings.Contains(body, "cannot safely act around yet") {
			t.Errorf("statusGuidance(%s) kept the generic unknown-state text: %q", state, body)
		}
	}
	window := statusGuidance(printerstate.StateBlock{ActivityState: printerstate.StatePreparing, StartWindow: true})
	if !strings.Contains(window, "self-test") || !strings.Contains(window, "stop it on the printer screen") || !strings.Contains(window, "set_light") {
		t.Errorf("start window guidance = %q", window)
	}
	ordinary := statusGuidance(printerstate.StateBlock{ActivityState: printerstate.StatePreparing})
	if strings.Contains(ordinary, "stop it on the printer screen") || !strings.Contains(ordinary, "set_light and cancel_print") {
		t.Errorf("ordinary preparing guidance = %q", ordinary)
	}
}

// --- get_printer_status ---

func TestGetPrinterStatusIdleGuidance(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{"activity_state: idle", "bucket: I", "idle and safe to start", "start_print"} {
		if !strings.Contains(text, want) {
			t.Fatalf("get_printer_status idle reply missing %q:\n%s", want, text)
		}
	}
}

func TestGetPrinterStatusPrintingGuidance(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, map[string]http.HandlerFunc{
		"/printer/objects/query": objectsQueryOverride(t, printingOverrides()),
	})
	cs := testSession(t, deps)
	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{"activity_state: printing", "bucket: P", "pause_print", "cancel_print"} {
		if !strings.Contains(text, want) {
			t.Fatalf("get_printer_status printing reply missing %q:\n%s", want, text)
		}
	}
}

func TestGetPrinterStatusPausedGuidance(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, map[string]http.HandlerFunc{
		"/printer/objects/query": objectsQueryOverride(t, pausedOverrides(215)),
	})
	cs := testSession(t, deps)
	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{"activity_state: paused", "bucket: Z", "stored_hotend_target_c: 215", "reheat the nozzle to 215", "resume_print"} {
		if !strings.Contains(text, want) {
			t.Fatalf("get_printer_status paused reply missing %q:\n%s", want, text)
		}
	}
}

func TestGetPrinterStatusErrorGuidance(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, map[string]http.HandlerFunc{
		"/printer/objects/query": objectsQueryOverride(t, errorStateOverrides()),
	})
	cs := testSession(t, deps)
	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{"activity_state: error", "bucket: E", "clear the error"} {
		if !strings.Contains(text, want) {
			t.Fatalf("get_printer_status error reply missing %q:\n%s", want, text)
		}
	}
}

func TestGetPrinterStatusUnreachableIsOfflineNotError(t *testing.T) {
	moon := fakeMoonraker(t, nil)
	wsHost, wsPort := fake9999(t)
	printer := testPrinter("k2", moon, wsHost, wsPort)
	moon.Close()

	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
	}
	cs := testSession(t, deps)
	res := call(t, cs, "get_printer_status", nil)
	if res.IsError {
		t.Fatalf("get_printer_status reported an error for an unreachable printer: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "activity_state: offline") {
		t.Fatalf("get_printer_status reply missing activity_state: offline:\n%s", text)
	}
}

func TestGetPrinterStatusNotFoundPrinter(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "get_printer_status", map[string]any{"printer": "does-not-exist"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("get_printer_status not_found reply = %s", text)
	}
}

func TestGetPrinterStatusAmbiguousPrinter(t *testing.T) {
	moon := fakeMoonraker(t, nil)
	wsHost, wsPort := fake9999(t)
	a := testPrinter("a", moon, wsHost, wsPort)
	b := testPrinter("b", moon, wsHost, wsPort)
	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{a, b}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
	}
	cs := testSession(t, deps)
	res := call(t, cs, "get_printer_status", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: ambiguous") {
		t.Fatalf("get_printer_status ambiguous reply = %s", text)
	}
}

// --- get_current_job ---

func TestGetCurrentJobIdleShowsLastJobSummary(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "get_current_job", nil)
	if res.IsError {
		t.Fatalf("get_current_job failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{
		"has_job: false", "filename: FigureJoints5mmball.stl_PLA_41m51s.gcode",
		"slicer: Creality", "No job is currently running",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("get_current_job idle reply missing %q:\n%s", want, text)
		}
	}
}

func TestGetCurrentJobPrintingShowsProgressAndObjects(t *testing.T) {
	overrides := printingOverrides()
	overrides["exclude_object"] = func(m map[string]any) {
		m["objects"] = []any{map[string]any{"name": "obj1"}, map[string]any{"name": "obj2"}}
		m["excluded_objects"] = []any{"obj1"}
		m["current_object"] = "obj2"
	}
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, map[string]http.HandlerFunc{
		"/printer/objects/query": objectsQueryOverride(t, overrides),
	})
	cs := testSession(t, deps)
	res := call(t, cs, "get_current_job", nil)
	if res.IsError {
		t.Fatalf("get_current_job failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{
		"has_job: true", "layer: 10", "progress_percent: 20",
		"obj1", "obj2", "Excluded objects: obj1", `Currently printing object "obj2"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("get_current_job printing reply missing %q:\n%s", want, text)
		}
	}
}

// --- list_job_history ---

func TestListJobHistoryHappyPathWithTotals(t *testing.T) {
	// fakeMoonraker (server_test.go) does not serve /server/history/totals
	// by default, so this test adds it: server_history_totals.json is the
	// real captured fixture list_job_history's own extra HistoryTotals call
	// needs for its lifetime totals.
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, map[string]http.HandlerFunc{
		"/server/history/totals": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(fixture(t, "moonraker", "server_history_totals.json"))
		},
	})
	cs := testSession(t, deps)
	res := call(t, cs, "list_job_history", nil)
	if res.IsError {
		t.Fatalf("list_job_history failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{
		// server_history_list_limit_10.json's fixture carries 10 job records
		// (its own "count" field says 12, but that reflects a larger real
		// history than this fixture's captured page); server_history_totals.json
		// separately reports the lifetime total of 12 jobs.
		"page: 1", "total: 10", "total_jobs_lifetime: 12",
		"total_print_time_s:", "total_filament_used:", "job_id",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("list_job_history reply missing %q:\n%s", want, text)
		}
	}
}

func TestListJobHistoryOutOfRangePage(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "list_job_history", map[string]any{"page": 999})
	if res.IsError {
		t.Fatalf("list_job_history out-of-range page failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "total_pages:") {
		t.Fatalf("list_job_history out-of-range reply = %s", text)
	}
}

func TestListJobHistoryNotFoundPrinter(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "list_job_history", map[string]any{"printer": "does-not-exist"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("list_job_history not_found reply = %s", text)
	}
}

func TestListJobHistoryUnreachablePrinterErrors(t *testing.T) {
	moon := fakeMoonraker(t, nil)
	wsHost, wsPort := fake9999(t)
	printer := testPrinter("k2", moon, wsHost, wsPort)
	moon.Close()

	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
	}
	cs := testSession(t, deps)
	res := call(t, cs, "list_job_history", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") {
		t.Fatalf("list_job_history unreachable reply = %s", text)
	}
}

// --- list_console_messages ---

func TestListConsoleMessagesHappyPathNewestFirst(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "list_console_messages", map[string]any{"count": 20})
	if res.IsError {
		t.Fatalf("list_console_messages failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{
		"page: 1", "total: 20", "requested_count: 20", "fetched_count: 20",
		"end_temp!",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("list_console_messages reply missing %q:\n%s", want, text)
		}
	}
	// server_gcode_store.json's fixture is oldest-first, ending with
	// "// end_temp!"; list_console_messages must reverse it so that newest
	// message appears before the oldest one in the rendered body table.
	// The frontmatter's own recent_activity tail (printerstate.BuildStateBlock,
	// unrelated to this tool's pagination) also carries both strings in
	// fixture (oldest-first) order, so the comparison is scoped to the body
	// table alone rather than the whole rendered text.
	tableStart := strings.Index(text, "| time | type | message |")
	if tableStart == -1 {
		t.Fatalf("list_console_messages reply missing its message table:\n%s", text)
	}
	body := text[tableStart:]
	endIdx := strings.Index(body, "end_temp!")
	firstIdx := strings.Index(body, "cur_temp = 44.0")
	if endIdx == -1 || firstIdx == -1 || endIdx > firstIdx {
		t.Fatalf("list_console_messages did not reverse to newest-first:\n%s", text)
	}
}

func TestListConsoleMessagesCountBounded(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "list_console_messages", map[string]any{"count": 100000})
	if res.IsError {
		t.Fatalf("list_console_messages failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "requested_count: 1000") {
		t.Fatalf("list_console_messages did not bound an oversized count:\n%s", text)
	}
}

func TestListConsoleMessagesOutOfRangePage(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "list_console_messages", map[string]any{"page": 999})
	if res.IsError {
		t.Fatalf("list_console_messages out-of-range page failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	if !strings.Contains(text, "total_pages:") {
		t.Fatalf("list_console_messages out-of-range reply = %s", text)
	}
}

func TestListConsoleMessagesNotFoundPrinter(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "list_console_messages", map[string]any{"printer": "does-not-exist"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") {
		t.Fatalf("list_console_messages not_found reply = %s", text)
	}
}
