package clicmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

const statusUsage = "usage: status [printer] [--json]\n"

// RunStatus is the "status" one-shot command: it resolves a printer exactly
// as the get_printer_status MCP tool does (resolvePrinter, backed by
// domain.ResolvePrinter) and then calls the same
// printerstate.Take/DeriveActivityState/BuildStateBlock pipeline
// (internal/mcpserver/tools_status.go's getPrinterStatusHandler), so the two
// surfaces can never disagree about a printer's derived state.
func RunStatus(ctx context.Context, deps Deps, args []string) int {
	deps = deps.withDefaults()

	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	asJSON := fs.Bool("json", false, "print the StateBlock as JSON instead of text")
	if err := fs.Parse(reorderArgsFlagsFirst(args, map[string]bool{"json": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(deps.Stdout, statusUsage)
			return 0
		}
		fmt.Fprint(deps.Stderr, statusUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprint(deps.Stderr, statusUsage)
		return 2
	}
	query := ""
	if fs.NArg() == 1 {
		query = fs.Arg(0)
	}

	printer, err := resolvePrinter(deps, query)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "status:", err)
		return 1
	}

	pdeps := deps.PrinterClients(printer)
	snap := printerstate.Take(ctx, pdeps, printer)
	derived := printerstate.DeriveActivityState(snap, nil)
	block := printerstate.BuildStateBlock(snap, derived, nil)

	if *asJSON {
		data, err := json.MarshalIndent(block, "", "  ")
		if err != nil {
			fmt.Fprintln(deps.Stderr, "status:", err)
			return 1
		}
		fmt.Fprintln(deps.Stdout, string(data))
		return 0
	}

	fmt.Fprint(deps.Stdout, formatStatusText(block, snap))
	return 0
}

// formatStatusText renders block (and snap, for the two job-progress fields
// StateBlock does not carry: layer and percent complete) as human-readable
// text: derived state, bucket, reasons, temperatures, job progress and the
// CFS flag, per dev_docs/plan-v0.1.0.md's "TUI and CLI" section.
func formatStatusText(block printerstate.StateBlock, snap printerstate.Snapshot) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s (%s)\n", block.PrinterName, block.PrinterHost)
	fmt.Fprintf(&b, "State: %s   bucket: %s   class: %s\n", block.ActivityState, block.Bucket, block.GatingClass)
	if len(block.Reasons) > 0 {
		b.WriteString("Reasons:\n")
		for _, r := range block.Reasons {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}

	if block.NozzleTemperatureC != nil {
		fmt.Fprintf(&b, "Nozzle: %.1f", *block.NozzleTemperatureC)
		if block.NozzleTargetC != nil {
			fmt.Fprintf(&b, "/%.1f", *block.NozzleTargetC)
		}
		b.WriteString(" C\n")
	}
	if block.BedTemperatureC != nil {
		fmt.Fprintf(&b, "Bed: %.1f", *block.BedTemperatureC)
		if block.BedTargetC != nil {
			fmt.Fprintf(&b, "/%.1f", *block.BedTargetC)
		}
		b.WriteString(" C\n")
	}

	if block.Job != nil {
		fmt.Fprintf(&b, "Job: %s\n", block.Job.Filename)
		if snap.VirtualSDCard != nil {
			if snap.VirtualSDCard.Progress > 0 {
				fmt.Fprintf(&b, "  progress: %.1f%%\n", snap.VirtualSDCard.Progress*100)
			}
			if snap.VirtualSDCard.LayerCount > 0 {
				fmt.Fprintf(&b, "  layer: %d/%d\n", snap.VirtualSDCard.Layer, snap.VirtualSDCard.LayerCount)
			}
		}
	}

	fmt.Fprintf(&b, "CFS connected: %s\n", yesNo(block.CFSConnected))
	fmt.Fprintf(&b, "9999 reachable: %s\n", yesNo(block.Ws9999Reachable))

	return b.String()
}
