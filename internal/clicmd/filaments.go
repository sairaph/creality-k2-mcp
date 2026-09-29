package clicmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/filaments"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

const filamentsUsage = "usage: filaments [printer] [--json]\n"

// RunFilaments is the "filaments" one-shot command (dev_docs/plan-v0.2.0.md
// section 5): a read-only listing of the CFS and side spool slots, built by
// the same internal/filaments code as the get_filaments MCP tool, so the two
// surfaces cannot disagree. It runs in its own process, so the state it shows
// is printerstate.DeriveActivityState's own (the signal-based start window
// still applies, the MCP server's in-process start record does not).
func RunFilaments(ctx context.Context, deps Deps, args []string) int {
	deps = deps.withDefaults()

	fs := flag.NewFlagSet("filaments", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	asJSON := fs.Bool("json", false, "print the filament view as JSON instead of text")
	if err := fs.Parse(reorderArgsFlagsFirst(args, map[string]bool{"json": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(deps.Stdout, filamentsUsage)
			return 0
		}
		fmt.Fprint(deps.Stderr, filamentsUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprint(deps.Stderr, filamentsUsage)
		return 2
	}
	query := ""
	if fs.NArg() == 1 {
		query = fs.Arg(0)
	}

	printer, err := resolvePrinter(deps, query)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "filaments:", err)
		return 1
	}

	pdeps := deps.PrinterClients(printer)
	snap := printerstate.Take(ctx, pdeps, printer)
	derived := printerstate.DeriveActivityState(snap, nil)
	view := filaments.Read(ctx, pdeps.WS9999, snap, derived.CFSConnected, filaments.EditGate(printer, derived, domain.DefaultSettings()))
	block := printerstate.BuildStateBlock(snap, derived, nil)

	if *asJSON {
		out := struct {
			ActivityState string                 `json:"activity_state"`
			CFS           *printerstate.CFSBlock `json:"cfs,omitempty"`
			filaments.View
		}{block.ActivityState, block.CFS, view}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			fmt.Fprintln(deps.Stderr, "filaments:", err)
			return 1
		}
		fmt.Fprintln(deps.Stdout, string(data))
		return 0
	}

	fmt.Fprintf(deps.Stdout, "%s (%s)\n", printer.Name, printer.Host)
	fmt.Fprintf(deps.Stdout, "State: %s\n", block.ActivityState)
	if block.CFS != nil {
		fmt.Fprintf(deps.Stdout, "CFS state: %s\n", block.CFS.State)
		for _, r := range block.CFS.Reasons {
			fmt.Fprintf(deps.Stdout, "  - %s\n", r)
		}
	}
	fmt.Fprint(deps.Stdout, filaments.Text(view))
	return 0
}
