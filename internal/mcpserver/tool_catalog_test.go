package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// TestToolCatalogMatchesRegisteredTools proves ToolCatalog (server.go,
// review backlog item 28) is the same single source of truth
// registerTool uses at registration: every tool registered under the
// control preset (the union of every category: monitor, camera and
// control, domain.Settings.presetIncludes) must appear in ToolCatalog, and
// every entry in ToolCatalog must actually get registered under that
// preset, so the two can never drift.
//
// This builds the server directly with newServer (no extra registration
// funcs), deliberately not testSession, since testSession also registers
// this package's own test-only sample tools (registerSampleTools), which
// ToolCatalog never sees and must not be expected to list.
func TestToolCatalogMatchesRegisteredTools(t *testing.T) {
	catalog := ToolCatalog()
	if len(catalog) == 0 {
		t.Fatal("ToolCatalog returned no tools")
	}
	inCatalog := make(map[string]bool, len(catalog))
	for _, info := range catalog {
		if info.Name == "" {
			t.Fatal("ToolCatalog contains a tool with an empty name")
		}
		if inCatalog[info.Name] {
			t.Fatalf("ToolCatalog lists %q more than once", info.Name)
		}
		inCatalog[info.Name] = true
	}

	deps := Deps{
		Settings: settingsWithPreset(domain.PresetControl),
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{}, nil
		},
	}
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

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("ListTools returned no tools under the control preset")
	}

	registered := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		registered[tool.Name] = true
		if !inCatalog[tool.Name] {
			t.Errorf("tool %q is registered under the control preset but missing from ToolCatalog", tool.Name)
		}
	}
	for name := range inCatalog {
		if !registered[name] {
			t.Errorf("tool %q is in ToolCatalog but was not registered under the control preset", name)
		}
	}
}
