package mcpserver

import (
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// resolvePrinter loads the registry and resolves query (the tool's optional
// `printer` argument: an id or name, case insensitive, defaulting to the one
// enabled printer) against its enabled printers (domain.ResolvePrinter). On
// failure it returns a ready-to-return error result whose hint lists every
// enabled printer by id and name, so the model's next call can supply one
// without another round trip to list_printers.
func (s *Server) resolvePrinter(query string) (domain.Printer, *mcp.CallToolResult) {
	reg, err := s.deps.LoadRegistry()
	if err != nil {
		return domain.Printer{}, failure("load the printer registry", err, "")
	}
	p, err := domain.ResolvePrinter(reg.Printers, query)
	if err != nil {
		return domain.Printer{}, failure("resolve the printer", err, printerListHint(reg))
	}
	return *p, nil
}

// printerListHint renders the enabled printers of reg as a hint for a
// resolution failure, or points at list_printers when none are enabled.
func printerListHint(reg domain.Registry) string {
	enabled := reg.Enabled()
	if len(enabled) == 0 {
		return "No printer is enabled. Call list_printers to see the registry, or register one first."
	}
	names := make([]string, 0, len(enabled))
	for _, p := range enabled {
		names = append(names, fmt.Sprintf("%s (%s)", p.ID, p.Name))
	}
	return fmt.Sprintf("Pass `printer` as one of: %s.", strings.Join(names, ", "))
}
