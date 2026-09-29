// Package printerclient builds the real, network-talking Moonraker and port
// 9999 clients for one domain.Printer, as printerstate.Deps. It exists
// because internal/mcpserver, internal/clicmd and internal/doctorchecks each
// need exactly this pair of clients for the same registry entry, and used to
// build it with three separately maintained copies (review backlog item 18:
// internal/mcpserver.DefaultPrinterClients and internal/clicmd's own copy in
// deps.go, kept in sync by hand and prone to drift; internal/doctorchecks
// already reused clicmd's copy). This package is the single implementation
// all three now call.
//
// It depends only on internal/domain, internal/moonraker, internal/crealityws
// and internal/printerstate, so every caller (including internal/policy's
// eventual production wiring) can import it without pulling in mcpserver or
// clicmd.
package printerclient

import (
	"fmt"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Default builds real Moonraker and port-9999 clients for p, talking to
// p.Host over the network. Never used by a test (AGENTS.md hard testing
// rule): every test supplies its own fakes bound to httptest and loopback
// WebSocket servers instead, so no test ever opens a non-loopback socket or
// contacts a real printer.
func Default(p domain.Printer) printerstate.Deps {
	return printerstate.Deps{
		Moonraker: moonraker.New(fmt.Sprintf("http://%s:%d", p.Host, p.MoonrakerPort), p.APIKey),
		WS9999:    crealityws.New(p.Host, 0),
	}
}
