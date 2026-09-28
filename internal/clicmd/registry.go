package clicmd

import (
	"fmt"
	"strings"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// loadFileRegistry loads the file-based registry only, ignoring the
// K2_MCP_HOST environment override: every "printers" subcommand reads and
// writes the registry file itself, and an ad hoc override is never saved to
// a file (domain.EnvOverride's own doc comment), so there is nothing here
// for it to act on. Warnings about dropped or deduplicated entries are
// printed to deps.Stderr rather than failing the command, matching
// loadMCPRegistry's own "never lock the user out" behavior in main.go.
func loadFileRegistry(deps Deps) (domain.Registry, string, error) {
	reg, path, warnings, err := domain.LoadRegistry(deps.Dir)
	if err != nil {
		return domain.Registry{}, "", err
	}
	for _, w := range warnings {
		fmt.Fprintf(deps.Stderr, "warning: %s\n", w)
	}
	return reg, path, nil
}

// loadRegistry resolves the registry a printer-reading command (status,
// snapshot) should use: the K2_MCP_HOST environment override when set
// (bypassing the registry file entirely, one ad hoc printer), otherwise the
// file-based registry. This matches main.go's loadMCPRegistry exactly, so
// these commands never disagree with the MCP server about which printer an
// empty or ambiguous query resolves to.
func loadRegistry(deps Deps) (domain.Registry, error) {
	if p, ok, err := domain.EnvOverride(); err != nil {
		return domain.Registry{}, err
	} else if ok {
		return domain.Registry{Version: 1, Printers: []domain.Printer{p}}, nil
	}
	reg, _, err := loadFileRegistry(deps)
	return reg, err
}

// resolvePrinter loads the registry (environment-override aware) and
// resolves query against its enabled printers, mirroring
// internal/mcpserver.(*Server).resolvePrinter (domain.ResolvePrinter is the
// shared function both call) so a CLI status/snapshot call and the
// equivalent MCP tool call always agree on which printer an empty or
// ambiguous query resolves to.
func resolvePrinter(deps Deps, query string) (domain.Printer, error) {
	reg, err := loadRegistry(deps)
	if err != nil {
		return domain.Printer{}, fmt.Errorf("load the printer registry: %w", err)
	}
	p, err := domain.ResolvePrinter(reg.Printers, query)
	if err != nil {
		return domain.Printer{}, fmt.Errorf("%w\n%s", err, printerListHint(reg))
	}
	return *p, nil
}

// printerListHint lists the enabled printers of reg for a resolution
// failure's error output, mirroring internal/mcpserver's own
// printerListHint so the guidance reads the same from either surface.
func printerListHint(reg domain.Registry) string {
	enabled := reg.Enabled()
	if len(enabled) == 0 {
		return "No printer is enabled. Run `printers` to see the registry, or `printers add <host>` to register one."
	}
	names := make([]string, 0, len(enabled))
	for _, p := range enabled {
		names = append(names, fmt.Sprintf("%s (%s)", p.ID, p.Name))
	}
	return "Pass a printer id or name, one of: " + strings.Join(names, ", ") + "."
}

// yesNo renders b for a table cell.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
