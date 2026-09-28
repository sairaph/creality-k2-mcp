package clicmd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/discovery"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
	"github.com/sairaph/creality_k2_mcp/internal/wizard"
)

const printersUsage = `usage: printers [scan | add <host> | enable <id> | disable <id> | control on|off <id>]
  printers                    list registry entries: enabled, control, reachability
  printers scan                scan the LAN; never changes the registry
  printers add <host>          probe one host and register it (enabled, control off)
  printers enable|disable <id> enable or disable a registered printer
  printers control on|off <id> allow or refuse control (only while enabled)
`

// controlWarning is printed whenever "printers control on" actually turns
// control on, so the person doing it sees what it grants before it takes
// effect (dev_docs/plan-v0.1.0.md decision 6: "read-only unless the
// printer's allow_control is true"; the "Control" tool group listed in the
// tool surface section).
const controlWarning = "Control lets the AI change this printer through the MCP tools: start, pause, resume " +
	"and cancel prints; upload and delete gcode files; set nozzle and bed temperature; fans, speed and flow " +
	"factors; the chamber light; and exclude objects. Only turn this on for a printer, and an AI client, you " +
	"trust with those actions."

// RunPrinters dispatches the "printers" one-shot command and its
// subcommands. It never touches an MCP tool or the TUI directly; every
// subcommand below calls the same internal/wizard and internal/discovery
// functions those surfaces call.
func RunPrinters(ctx context.Context, deps Deps, args []string) int {
	deps = deps.withDefaults()

	if len(args) == 0 {
		return runPrintersList(ctx, deps)
	}
	switch args[0] {
	case "scan":
		return runPrintersScan(ctx, deps, args[1:])
	case "add":
		return runPrintersAdd(ctx, deps, args[1:])
	case "enable":
		return runPrintersSetEnabled(deps, args[1:], true)
	case "disable":
		return runPrintersSetEnabled(deps, args[1:], false)
	case "control":
		return runPrintersControl(deps, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(deps.Stdout, printersUsage)
		return 0
	default:
		fmt.Fprintf(deps.Stderr, "printers: unknown subcommand %q\n%s", args[0], printersUsage)
		return 2
	}
}

// --- printers (list) ---

// printersListConcurrency and printersListTimeout bound the per-printer
// gather this runs across every registry entry, matching
// internal/mcpserver's list_printers tool (tools_registry.go) so a dead or
// slow printer never hangs the whole listing.
const (
	printersListConcurrency = 4
	printersListTimeout     = 5 * time.Second
)

type printerRowView struct {
	printer   domain.Printer
	reachable bool
	state     string
}

func runPrintersList(ctx context.Context, deps Deps) int {
	reg, _, err := loadFileRegistry(deps)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers:", err)
		return 1
	}
	if len(reg.Printers) == 0 {
		fmt.Fprintln(deps.Stdout, "No printers registered. Run `printers scan` to find some on the network, "+
			"or `printers add <host>` for one you already know.")
		return 0
	}

	views := gatherPrinterViews(ctx, deps, reg.Printers)

	tw := tabwriter.NewWriter(deps.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tHOST\tENABLED\tCONTROL\tREACHABLE\tSTATE")
	for _, v := range views {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			v.printer.ID, v.printer.Name, v.printer.Host,
			yesNo(v.printer.Enabled), yesNo(v.printer.AllowControl), yesNo(v.reachable), v.state)
	}
	tw.Flush()
	return 0
}

// gatherPrinterViews takes a printerstate.Snapshot of every printer in
// printers (bounded concurrency and per-printer timeout), the same
// printerstate.Take + DeriveActivityState pipeline every MCP status tool
// uses, so "printers" reports exactly the reachability and state those tools
// would see. Reachability is snap.ServerInfoErr == nil, matching
// list_printers (tools_registry.go).
func gatherPrinterViews(ctx context.Context, deps Deps, printers []domain.Printer) []printerRowView {
	views := make([]printerRowView, len(printers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, printersListConcurrency)
	for i, p := range printers {
		i, p := i, p
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, printersListTimeout)
			defer cancel()
			pdeps := deps.PrinterClients(p)
			snap := printerstate.Take(pctx, pdeps, p)
			derived := printerstate.DeriveActivityState(snap, nil)
			views[i] = printerRowView{printer: p, reachable: snap.ServerInfoErr == nil, state: derived.State}
		}()
	}
	wg.Wait()
	return views
}

// --- printers scan ---

func runPrintersScan(ctx context.Context, deps Deps, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(deps.Stderr, "usage: printers scan")
		return 2
	}
	reg, _, err := loadFileRegistry(deps)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers scan:", err)
		return 1
	}

	opts := discovery.Options{
		ScanOptions: discovery.ScanOptions{
			Progress: func(p discovery.Progress) {
				fmt.Fprintf(deps.Stderr, "\rscanning: %d/%d hosts, %d found", p.Scanned, p.Total, p.Found)
			},
		},
	}
	report, merged, err := wizard.ScanAndMerge(ctx, deps.Discover, opts, reg, true)
	fmt.Fprintln(deps.Stderr)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers scan:", err)
		return 1
	}

	if len(merged) == 0 {
		fmt.Fprintln(deps.Stdout, "No Creality K2 printers were found on the network.")
	} else {
		tw := tabwriter.NewWriter(deps.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "HOST\tHOSTNAME\tMODEL\tIDENTIFIED\tREGISTERED")
		for _, m := range merged {
			registered := "no"
			if m.Registered && m.Existing != nil {
				registered = fmt.Sprintf("yes (%s)", m.Existing.ID)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				m.Discovered.Host, m.Discovered.Hostname, m.Discovered.Model,
				yesNo(m.Discovered.IdentifiedK2), registered)
		}
		tw.Flush()
	}
	if report.Partial {
		fmt.Fprintln(deps.Stdout, "\nThe scan did not finish within its time budget; run `printers scan` again to keep looking.")
	}
	fmt.Fprintln(deps.Stdout, "\nThis never changes the registry. Run `install` (or `add` in a project) to "+
		"register every printer found here, `printers add <host>` for one specific host, or use the TUI.")
	return 0
}

// --- printers add ---

func runPrintersAdd(ctx context.Context, deps Deps, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(deps.Stderr, "usage: printers add <host>")
		return 2
	}
	host := args[0]

	reg, _, err := loadFileRegistry(deps)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers add:", err)
		return 1
	}

	_, merged, err := wizard.ProbeAndMerge(ctx, deps.Probe, host, 0, reg)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers add:", err)
		return 1
	}

	if merged.Registered {
		name := merged.Discovered.Host
		if merged.Existing != nil {
			name = fmt.Sprintf("%s (%s)", merged.Existing.ID, merged.Existing.Name)
		}
		fmt.Fprintf(deps.Stdout, "%s is already registered as %s.\n", host, name)
		return 0
	}
	if merged.Proposed == nil {
		fmt.Fprintf(deps.Stderr, "printers add: %s could not be added: %s\n", host, merged.ReasonNotProposed)
		return 1
	}

	rows := wizard.BuildRows(reg, nil, nil)
	rows = wizard.MergeRow(rows, merged)
	_, path, err := wizard.SaveSelection(deps.Dir, rows)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers add:", err)
		return 1
	}

	p := *merged.Proposed
	fmt.Fprintf(deps.Stdout, "Added %s (%s), enabled, control off.\n", p.ID, p.Host)
	fmt.Fprintf(deps.Stdout, "Run `printers control on %s` to allow the AI to control it.\n", p.ID)
	fmt.Fprintf(deps.Stdout, "Registry: %s\n", path)
	return 0
}

// --- printers enable / disable ---

func runPrintersSetEnabled(deps Deps, args []string, enable bool) int {
	verb := "disable"
	if enable {
		verb = "enable"
	}
	if len(args) != 1 {
		fmt.Fprintf(deps.Stderr, "usage: printers %s <id>\n", verb)
		return 2
	}
	query := args[0]

	reg, _, err := loadFileRegistry(deps)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "printers %s: %v\n", verb, err)
		return 1
	}
	rows := wizard.BuildRows(reg, nil, nil)
	idx := findRowIndex(rows, query)
	if idx < 0 {
		fmt.Fprintf(deps.Stderr, "printers %s: no registered printer matches %q. Run `printers` to list.\n", verb, query)
		return 1
	}
	if rows[idx].Printer.Enabled == enable {
		fmt.Fprintf(deps.Stdout, "%s is already %sd.\n", rows[idx].Printer.ID, verb)
		return 0
	}

	id := rows[idx].Printer.ID
	ps := &wizard.PrinterState{Rows: rows, Cursor: idx}
	wizard.ToggleEnabled(ps)
	_, path, err := wizard.SaveSelection(deps.Dir, ps.Rows)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "printers %s: %v\n", verb, err)
		return 1
	}

	fmt.Fprintf(deps.Stdout, "%sd %s.\n", verb, id)
	if !enable {
		fmt.Fprintln(deps.Stdout, "Control was also turned off: a disabled printer can never keep control.")
	}
	fmt.Fprintf(deps.Stdout, "Registry: %s\n", path)
	return 0
}

// --- printers control on|off ---

func runPrintersControl(deps Deps, args []string) int {
	if len(args) != 2 || (args[0] != "on" && args[0] != "off") {
		fmt.Fprintln(deps.Stderr, "usage: printers control on|off <id>")
		return 2
	}
	desired := args[0] == "on"
	query := args[1]

	reg, _, err := loadFileRegistry(deps)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers control:", err)
		return 1
	}
	rows := wizard.BuildRows(reg, nil, nil)
	idx := findRowIndex(rows, query)
	if idx < 0 {
		fmt.Fprintf(deps.Stderr, "printers control: no registered printer matches %q. Run `printers` to list.\n", query)
		return 1
	}
	row := rows[idx]
	if row.Printer.AllowControl == desired {
		fmt.Fprintf(deps.Stdout, "%s already has control %s.\n", row.Printer.ID, args[0])
		return 0
	}
	// Checked here, ahead of ToggleControl, only so the exit code (1: a
	// runtime condition, not a usage mistake) and the message are under this
	// command's control; ToggleControl (wizard/printers_step.go) refuses the
	// same combination internally and would otherwise leave rows unsaved
	// with no explanation of why nothing happened.
	if desired && !row.Printer.Enabled {
		fmt.Fprintf(deps.Stderr, "printers control: %s is disabled; run `printers enable %s` first.\n",
			row.Printer.ID, row.Printer.ID)
		return 1
	}

	ps := &wizard.PrinterState{Rows: rows, Cursor: idx}
	wizard.ToggleControl(ps)
	if ps.Message != "" {
		fmt.Fprintln(deps.Stderr, "printers control:", ps.Message)
		return 1
	}
	_, path, err := wizard.SaveSelection(deps.Dir, ps.Rows)
	if err != nil {
		fmt.Fprintln(deps.Stderr, "printers control:", err)
		return 1
	}

	if desired {
		fmt.Fprintln(deps.Stdout, controlWarning)
	}
	fmt.Fprintf(deps.Stdout, "%s: control %s.\n", row.Printer.ID, args[0])
	fmt.Fprintf(deps.Stdout, "Registry: %s\n", path)
	return 0
}

// findRowIndex returns the index of the Addable row in rows whose printer id
// or name matches query case insensitively, or -1.
func findRowIndex(rows []wizard.PrinterRow, query string) int {
	q := strings.TrimSpace(query)
	for i, r := range rows {
		if !r.Addable {
			continue
		}
		if strings.EqualFold(r.Printer.ID, q) || strings.EqualFold(r.Printer.Name, q) {
			return i
		}
	}
	return -1
}
