package printerclient

import (
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// TestDefaultBuildsBothClients proves Default (review backlog item 18's
// single implementation, formerly duplicated in internal/mcpserver's and
// internal/clicmd's own deps.go) returns real, concrete Moonraker and port
// 9999 clients without contacting anything: both constructors only ever
// store their arguments (internal/moonraker.New, internal/crealityws.New),
// so this never opens a socket of any kind (AGENTS.md hard testing rule).
func TestDefaultBuildsBothClients(t *testing.T) {
	p := domain.Printer{
		ID:            "k2",
		Host:          "127.0.0.1",
		MoonrakerPort: 7125,
		APIKey:        "secret",
	}
	deps := Default(p)

	moon, ok := deps.Moonraker.(*moonraker.Client)
	if !ok || moon == nil {
		t.Fatalf("Default(p).Moonraker = %T, want a non-nil *moonraker.Client", deps.Moonraker)
	}
	ws, ok := deps.WS9999.(*crealityws.Client)
	if !ok || ws == nil {
		t.Fatalf("Default(p).WS9999 = %T, want a non-nil *crealityws.Client", deps.WS9999)
	}
}
