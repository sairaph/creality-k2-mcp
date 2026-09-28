package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// TestAllTemplatesAreKnownToTheMoonrakerPackage implements
// dev_docs/safety-architecture.md section 3.2's regression check 1: every
// template the action table can render is a member of moonraker's closed
// Template enum (no way to reference a template outside it, since Template
// is a package-private-constructed int type), and every member of that
// enum is used by at least one action (no orphan templates the table never
// reaches).
func TestAllTemplatesAreKnownToTheMoonrakerPackage(t *testing.T) {
	known := map[moonraker.Template]bool{
		moonraker.TemplateSetHeaterTemperature: true,
		moonraker.TemplateM106:                 true,
		moonraker.TemplateM220:                 true,
		moonraker.TemplateM221:                 true,
		moonraker.TemplateExcludeObject:        true,
	}
	used := map[moonraker.Template]bool{}
	for _, spec := range specs {
		for _, tmpl := range spec.Templates {
			if !known[tmpl] {
				t.Errorf("action %s uses template %v, which is not in the moonraker.Template allowlist", spec.Name, tmpl)
			}
			used[tmpl] = true
		}
	}
	for tmpl := range known {
		if !used[tmpl] {
			t.Errorf("moonraker.Template %v is never used by any action in the policy table (orphan template)", tmpl)
		}
	}
}

// captureGCodeServer is a loopback-only HTTP test server (AGENTS.md hard
// testing rule) that answers every Moonraker call with ok and records the
// last gcode script sent to /printer/gcode/script, so the test below can
// inspect the literal string internal/moonraker's own render() function
// (unexported, only reachable through RunTemplate) actually produces.
func captureGCodeServer(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var last string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/printer/gcode/script" {
			var body struct {
				Script string `json:"script"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			last = body.Script
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result": "ok"}`))
	}))
	t.Cleanup(ts.Close)
	return ts, func() string { return last }
}

// TestNoTemplateRendersAForbiddenCommand implements
// dev_docs/safety-architecture.md section 3.2's regression check 2: send
// every template this package's action table can render through the real
// internal/moonraker.Client (against a loopback-only capture server, never
// the real printer) and assert the literal rendered command's mnemonic
// never appears in testdata/forbidden.txt.
func TestNoTemplateRendersAForbiddenCommand(t *testing.T) {
	forbidden := loadForbidden(t)
	ts, last := captureGCodeServer(t)
	client := moonraker.New(ts.URL, "")

	sampleArgs := map[moonraker.Template]map[string]string{
		moonraker.TemplateSetHeaterTemperature: {"heater": "extruder", "target": "200"},
		moonraker.TemplateM106:                 {"fan": "0", "speed": "128"},
		moonraker.TemplateM220:                 {"percent": "100"},
		moonraker.TemplateM221:                 {"percent": "100"},
		moonraker.TemplateExcludeObject:        {"name": "PART_A"},
	}

	// Every template used anywhere in the action table, exercised with a
	// valid sample argument set.
	seen := map[moonraker.Template]bool{}
	for _, spec := range specs {
		for _, tmpl := range spec.Templates {
			if seen[tmpl] {
				continue
			}
			seen[tmpl] = true
			args, ok := sampleArgs[tmpl]
			if !ok {
				t.Fatalf("no sample args registered for template %v used by action %s", tmpl, spec.Name)
			}
			if err := client.RunTemplate(context.Background(), tmpl, args); err != nil {
				t.Fatalf("RunTemplate(%v): %v", tmpl, err)
			}
			cmd := last()
			if cmd == "" {
				t.Fatalf("RunTemplate(%v) sent no script", tmpl)
			}
			mnemonic := strings.Fields(cmd)[0]
			if forbidden.matches(mnemonic) {
				t.Errorf("action %s's template %v rendered a forbidden command: %q", spec.Name, tmpl, cmd)
			}
		}
	}

	// Every action's documented Commands strings (endpoint names and the
	// human-readable command text in the table) must not name a forbidden
	// command as their first word either.
	for _, spec := range specs {
		for _, c := range spec.Commands {
			word := strings.Fields(c)[0]
			if forbidden.matches(word) {
				t.Errorf("action %s documents a forbidden command in Commands: %q", spec.Name, c)
			}
		}
	}
}
