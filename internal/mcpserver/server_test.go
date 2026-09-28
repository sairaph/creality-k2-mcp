package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// --- Fixture loading: reuse the real captures internal/moonraker and
// internal/crealityws already carry, instead of hand-building new JSON, per
// T6c's "reusing testdata captures" requirement. go:embed cannot reach a
// sibling package's directory, so these are read at test time relative to
// this package's own directory (go test's working directory is always the
// package directory), never written to.

func fixture(t *testing.T, pkg, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", pkg, "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s/%s: %v", pkg, name, err)
	}
	return data
}

// --- Fake Moonraker: httptest on 127.0.0.1 only (AGENTS.md hard testing
// rule), serving the real fixtures for every endpoint printerstate.Take
// reads. overrides replaces or adds a handler for one path, for tests that
// need a specific failure (401, a closed server, and so on).

func fakeMoonraker(t *testing.T, overrides map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	serverInfo := fixture(t, "moonraker", "server_info.json")
	objectsQuery := fixture(t, "moonraker", "objects_query_full.json")
	historyList := fixture(t, "moonraker", "server_history_list_limit_10.json")
	gcodeStore := fixture(t, "moonraker", "server_gcode_store.json")

	handlers := map[string]http.HandlerFunc{
		"/server/info": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(serverInfo)
		},
		// Not the real printer_info.json fixture (whose hostname, "K2-5885",
		// would not match testPrinter's synthetic "<id>.local" convention
		// below): this echoes "k2.local" to match every single-printer
		// test's testPrinter("k2", ...) call, so the persisted-hostname
		// verification review backlog item 24 added sees a match instead of
		// failing every existing test closed with identity_unverified. A
		// test that needs a genuine mismatch, or an unreachable printer/info,
		// passes its own override.
		"/printer/info": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"result": {"state": "ready", "hostname": "k2.local"}}`))
		},
		"/printer/objects/query": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(objectsQuery)
		},
		"/server/history/list": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(historyList)
		},
		"/server/gcode_store": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write(gcodeStore)
		},
	}
	for path, h := range overrides {
		handlers[path] = h
	}

	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fake9999 runs a loopback WebSocket server that sends the real idle full
// push capture once, enough for crealityws.Client.ReadStatus to return
// (mirrors internal/crealityws/fakeserver_test.go's own startFakeServer).
func fake9999(t *testing.T) (host string, port int) {
	t.Helper()
	push := fixture(t, "crealityws", "idle_full_push.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"wsslicer"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.Write(context.Background(), websocket.MessageText, push)
		// Keep the connection open briefly so the client's read loop has
		// time to observe the push before the server tears it down.
		time.Sleep(50 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake 9999 url %q: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse fake 9999 port from %q: %v", srv.URL, err)
	}
	return u.Hostname(), p
}

// testPrinter builds a minimal, valid registry entry pointing at a fake
// Moonraker and fake 9999 server, both loopback only.
func testPrinter(id string, moon *httptest.Server, ws9999Host string, ws9999Port int) domain.Printer {
	u, _ := url.Parse(moon.URL)
	portStr := u.Port()
	port, _ := strconv.Atoi(portStr)
	return domain.Printer{
		ID:            id,
		Name:          id,
		Host:          u.Hostname(),
		MoonrakerPort: port,
		Hostname:      id + ".local",
		Enabled:       true,
		AllowControl:  true,
	}
}

// clientsFor returns a PrinterClients that ignores the printer's own
// host/port and always dials moon and (ws9999Host, ws9999Port): the fixed
// pair every test-scoped registry entry actually points at once
// domain.Printer.Host/MoonrakerPort have been rewritten to the fake
// server's real loopback address by testPrinter.
func clientsFor(moon *httptest.Server, ws9999Host string, ws9999Port int) PrinterClients {
	return func(p domain.Printer) printerstate.Deps {
		return printerstate.Deps{
			Moonraker: moonraker.New(moon.URL, p.APIKey),
			WS9999:    crealityws.New(ws9999Host, ws9999Port),
		}
	}
}

// testSession builds a Server registering only the sample tools (never a
// real tool group; production code never calls newServer with extras) and
// connects an in-memory MCP client/server pair to it, exactly as
// freecad-mcp/internal/mcpserver/server_test.go does with
// mcp.NewInMemoryTransports.
func testSession(t *testing.T, deps Deps) *mcp.ClientSession {
	t.Helper()
	srv := newServer(Config{Version: "test"}, deps, registerSampleTools)
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

func settingsWithPreset(preset domain.ToolPreset) domain.Settings {
	s := domain.DefaultSettings()
	s.Tools = domain.ToolSettings{Preset: preset}
	return s
}

func singlePrinterDeps(t *testing.T, preset domain.ToolPreset, moonOverrides map[string]http.HandlerFunc) (Deps, *httptest.Server) {
	t.Helper()
	moon := fakeMoonraker(t, moonOverrides)
	wsHost, wsPort := fake9999(t)
	printer := testPrinter("k2", moon, wsHost, wsPort)
	return Deps{
		Settings: settingsWithPreset(preset),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
	}, moon
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func texts(res *mcp.CallToolResult) []string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

func images(res *mcp.CallToolResult) int {
	n := 0
	for _, c := range res.Content {
		if _, ok := c.(*mcp.ImageContent); ok {
			n++
		}
	}
	return n
}

// --- Tests ---

func TestSampleToolsListedWithAnnotations(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetControl, nil)
	cs := testSession(t, deps)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	want := []string{"sample_state", "sample_probe", "sample_image", "sample_list", "sample_control", "sample_boom"}
	for _, name := range want {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("tool %s not registered under preset control; got %v", name, keysOf(byName))
		}
		if tool.Annotations == nil {
			t.Fatalf("%s: no annotations", name)
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Fatalf("%s: openWorldHint = %v, want false", name, tool.Annotations.OpenWorldHint)
		}
	}
	if ctl := byName["sample_control"]; ctl.Annotations.ReadOnlyHint || ctl.Annotations.DestructiveHint == nil || !*ctl.Annotations.DestructiveHint {
		t.Fatalf("sample_control annotations = %+v", ctl.Annotations)
	}
	if st := byName["sample_state"]; !st.Annotations.ReadOnlyHint {
		t.Fatalf("sample_state annotations = %+v, want readOnlyHint true", st.Annotations)
	}
}

func keysOf(m map[string]*mcp.Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestPresetFilteringHidesDisabledTools(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, tool := range res.Tools {
		present[tool.Name] = true
	}
	if !present["sample_state"] || !present["sample_probe"] || !present["sample_list"] || !present["sample_boom"] {
		t.Fatalf("monitor-category sample tools missing under preset monitor: %v", present)
	}
	if present["sample_image"] {
		t.Fatal("camera-category sample_image registered under preset monitor")
	}
	if present["sample_control"] {
		t.Fatal("control-category sample_control registered under preset monitor")
	}

	cameraDeps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	res2, err := testSession(t, cameraDeps).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	present2 := map[string]bool{}
	for _, tool := range res2.Tools {
		present2[tool.Name] = true
	}
	if !present2["sample_image"] {
		t.Fatal("camera-category sample_image missing under preset camera")
	}
	if present2["sample_control"] {
		t.Fatal("control-category sample_control registered under preset camera")
	}
}

func TestOverrideWinsOverPreset(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	deps.Settings.Tools.Overrides = map[string]bool{"sample_control": true, "sample_state": false}
	cs := testSession(t, deps)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, tool := range res.Tools {
		present[tool.Name] = true
	}
	if !present["sample_control"] {
		t.Fatal("override did not enable sample_control")
	}
	if present["sample_state"] {
		t.Fatal("override did not disable sample_state")
	}
}

func TestStateSuccessEmbedsStateBlockAndActions(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "sample_state", nil)
	if res.IsError {
		t.Fatalf("sample_state failed: %v", texts(res))
	}
	text := strings.Join(texts(res), "\n")
	for _, want := range []string{"printer_id: k2", "activity_state:", "bucket:", "gating_class:", "actions:", "name: sample_action", "status: available"} {
		if !strings.Contains(text, want) {
			t.Fatalf("sample_state reply missing %q:\n%s", want, text)
		}
	}
}

func TestImageResultAndSizeCap(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetCamera, nil)
	cs := testSession(t, deps)

	res := call(t, cs, "sample_image", map[string]any{"too_big": false})
	if res.IsError || images(res) != 1 {
		t.Fatalf("sample_image reply = %v (isError %v, images %d)", texts(res), res.IsError, images(res))
	}

	big := call(t, cs, "sample_image", map[string]any{"too_big": true})
	if !big.IsError || images(big) != 0 {
		t.Fatalf("oversized image accepted: isError=%v images=%d text=%v", big.IsError, images(big), texts(big))
	}
	if !strings.Contains(strings.Join(texts(big), ""), "code: internal_error") {
		t.Fatalf("oversized image error = %v", texts(big))
	}
}

func TestPaginationHelper(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)

	page1 := strings.Join(texts(call(t, cs, "sample_list", nil)), "\n")
	if !strings.Contains(page1, "page: 1") || !strings.Contains(page1, "total: 60") {
		t.Fatalf("sample_list page 1 = %s", page1)
	}
	if !strings.Contains(page1, "Next: page=2.") {
		t.Fatalf("sample_list page 1 missing next-page hint: %s", page1)
	}

	last := strings.Join(texts(call(t, cs, "sample_list", map[string]any{"page": 999})), "\n")
	if !strings.Contains(last, "total_pages:") {
		t.Fatalf("out-of-range page reply = %s", last)
	}
}

func TestErrorMappingNotFound(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "sample_probe", map[string]any{"printer": "does-not-exist"})
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: not_found") || !strings.Contains(text, "k2 (k2)") {
		t.Fatalf("not_found reply = %s", text)
	}
}

func TestErrorMappingAmbiguous(t *testing.T) {
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
	res := call(t, cs, "sample_probe", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: ambiguous") || !strings.Contains(text, "a (a)") || !strings.Contains(text, "b (b)") {
		t.Fatalf("ambiguous reply = %s", text)
	}
}

func TestErrorMappingAuthentication(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, map[string]http.HandlerFunc{
		"/server/info": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error": {"code": 401, "message": "Unauthorized"}}`))
		},
	})
	cs := testSession(t, deps)
	res := call(t, cs, "sample_probe", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: authentication") || !strings.Contains(text, "api_key") {
		t.Fatalf("authentication reply = %s", text)
	}
}

func TestErrorMappingUnavailable(t *testing.T) {
	moon := fakeMoonraker(t, nil)
	wsHost, wsPort := fake9999(t)
	printer := testPrinter("k2", moon, wsHost, wsPort)
	moon.Close() // nothing listens at moon.URL any more

	deps := Deps{
		Settings: settingsWithPreset(domain.PresetMonitor),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{Version: 1, Printers: []domain.Printer{printer}}, nil
		},
		PrinterClients: clientsFor(moon, wsHost, wsPort),
	}
	cs := testSession(t, deps)
	res := call(t, cs, "sample_probe", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: unavailable") || !strings.Contains(text, "doctor") {
		t.Fatalf("unavailable reply = %s", text)
	}
}

func TestErrorMappingInternal(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res := call(t, cs, "sample_boom", nil)
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: internal_error") || !strings.Contains(text, "synthetic failure") {
		t.Fatalf("internal_error reply = %s", text)
	}
}

func TestInvalidArgumentsMiddleware(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sample_list",
		Arguments: map[string]any{"page": "not-a-number"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("invalid arguments reply = %s", text)
	}
}

// TestInvalidArgumentsMiddleware_UnknownArgumentRejected confirms an
// argument that names no property of a tool's input schema is rejected at
// schema validation (the go-sdk's jsonschema.For infers additionalProperties:
// false for every struct-derived input schema, internal/mcpserver never sets
// InputSchema to anything looser) and normalised to invalid_input by
// invalidArguments, exactly like a wrong-typed known argument (review
// backlog item 8). list_printers takes no arguments at all
// (listPrintersInput is an empty struct), so any argument name here is
// necessarily unknown to it.
func TestInvalidArgumentsMiddleware_UnknownArgumentRejected(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_printers",
		Arguments: map[string]any{"not_a_real_argument": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("unknown-argument reply = %s, want an invalid_input error", text)
	}
}

// TestInvalidArgumentsMiddleware_UnknownArgumentRejectedAlongsideKnownOnes
// is the same check as
// TestInvalidArgumentsMiddleware_UnknownArgumentRejected but on a tool with
// real optional properties present in the call too, so a validator that
// short-circuits once every listed property matched would miss the extra
// one; get_printer_status only defines "printer", so "bogus" here is
// necessarily unknown.
func TestInvalidArgumentsMiddleware_UnknownArgumentRejectedAlongsideKnownOnes(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	cs := testSession(t, deps)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_printer_status",
		Arguments: map[string]any{"printer": "k2", "bogus": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(texts(res), "\n")
	if !res.IsError || !strings.Contains(text, "code: invalid_input") {
		t.Fatalf("unknown-argument reply = %s, want an invalid_input error", text)
	}
}

func TestResolvePrinterDirect(t *testing.T) {
	deps, _ := singlePrinterDeps(t, domain.PresetMonitor, nil)
	srv := newServer(Config{Version: "test"}, deps, registerSampleTools)
	p, errRes := srv.resolvePrinter("")
	if errRes != nil {
		t.Fatalf("resolvePrinter with one enabled printer failed: %v", texts(errRes))
	}
	if p.ID != "k2" {
		t.Fatalf("resolvePrinter picked %q", p.ID)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it, used to check the config.toml unknown-override
// warning (review backlog item 7) without touching the process's real
// stderr for the rest of the test binary.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}

// TestNewWarnsUnknownToolOverrides confirms New prints a stderr warning
// naming a config.toml tools.overrides key that matches no registered tool,
// without failing startup or otherwise changing what gets registered
// (review backlog item 7).
func TestNewWarnsUnknownToolOverrides(t *testing.T) {
	settings := domain.DefaultSettings()
	settings.Tools.Overrides = map[string]bool{
		"set_nozzle_temperature": true, // a real tool name: must not be warned about
		"set_nozle_temperature":  true, // typo: unknown
	}
	deps := Deps{
		Settings:     settings,
		LoadRegistry: func() (domain.Registry, error) { return domain.Registry{}, nil },
	}

	out := captureStderr(t, func() {
		New(Config{Version: "test"}, deps)
	})

	if !strings.Contains(out, "set_nozle_temperature") {
		t.Errorf("stderr = %q, want it to name the unknown override %q", out, "set_nozle_temperature")
	}
	if strings.Contains(out, "set_nozzle_temperature\n") || strings.Contains(out, "set_nozzle_temperature,") ||
		strings.Contains(out, "set_nozzle_temperature ") {
		t.Errorf("stderr = %q, must not warn about the real tool name set_nozzle_temperature", out)
	}
}

// TestNewNoWarningForKnownOverrides confirms New stays silent when every
// tools.overrides key matches a registered tool.
func TestNewNoWarningForKnownOverrides(t *testing.T) {
	settings := domain.DefaultSettings()
	settings.Tools.Overrides = map[string]bool{
		"set_nozzle_temperature": true,
		"list_printers":          false,
	}
	deps := Deps{
		Settings:     settings,
		LoadRegistry: func() (domain.Registry, error) { return domain.Registry{}, nil },
	}

	out := captureStderr(t, func() {
		New(Config{Version: "test"}, deps)
	})

	if strings.Contains(out, "tools.overrides") {
		t.Errorf("stderr = %q, want no unknown-override warning", out)
	}
}

func TestNewPanicsWithoutRegistryLoader(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New did not panic with a nil LoadRegistry")
		}
	}()
	New(Config{Version: "test"}, Deps{Settings: domain.DefaultSettings()})
}
