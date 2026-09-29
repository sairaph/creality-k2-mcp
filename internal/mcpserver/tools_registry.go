package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
)

// This file implements the "Registry and discovery" tool group
// (dev_docs/plan-v0.1.0.md's tool surface, T7): list_printers and
// discover_printers. Neither operates on a single resolved printer the way
// every other tool does, so neither uses resolvePrinter or the StateFront
// (single-printer StateBlock) convention result.go's successResult expects;
// both call render.SuccessResult directly with their own typed frontmatter.

func registerRegistryTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "list_printers", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "list_printers",
		Description: "Lists every enabled printer in the registry with its id, name, host, model, whether " +
			"control writes are allowed for it (allow_control), whether it answered Moonraker just now " +
			"(reachability), and a one-line summary of its current state. Use this first to see which " +
			"printers this server knows about and which id or name to pass as the printer argument to every " +
			"other tool; every printer-specific tool defaults to the single enabled printer when only one is " +
			"enabled, and otherwise requires printer to disambiguate. An unreachable printer is still listed, " +
			"never treated as an error: check its reachability field before assuming a fault, and the state " +
			"summary will read offline or klippy_not_ready. This tool never adds, removes or enables a " +
			"printer itself; use the install wizard or the TUI to register a new printer, rename one, or " +
			"change which ones are enabled or allowed to be controlled, since this server has no tool that " +
			"can do that for you. Related: call discover_printers to find printers on the LAN that are not " +
			"registered yet, and get_printer_status for one printer's full detail.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listPrintersHandler(s))

	registerTool(s, domain.ToolInfo{Name: "discover_printers", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "discover_printers",
		Description: "Scans the local network for Creality K2 printers and reports what it found: host, " +
			"Moonraker port, Klipper hostname and model when identified, whether each one is already " +
			"registered, and, for one that is not registered, why it was or was not proposed as a new entry. " +
			"The scan is time and concurrency bounded; if it does not finish trying every candidate host in " +
			"time the result says so (a partial scan) and the tool can simply be called again. This tool only " +
			"reads the network: it never adds, removes or changes a registry entry, even for a host it " +
			"confirms as a K2. To register a printer this tool found, or to enable it or turn on control for " +
			"it, use the install wizard or the TUI; this server has no tool that can do that for you. Related: " +
			"list_printers shows what is already registered and enabled.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, discoverPrintersHandler(s))
}

// --- list_printers ---

// listPrintersConcurrency and listPrintersTimeout bound the per-printer
// gather list_printers runs across every enabled printer: at most this many
// printers are probed at once, and each one gets at most this long before it
// is reported unreachable rather than making the whole tool call hang on one
// dead printer.
const (
	listPrintersConcurrency = 4
	listPrintersTimeout     = 5 * time.Second
)

type listPrintersInput struct{}

type printerSummary struct {
	ID            string `yaml:"id"`
	Name          string `yaml:"name"`
	Host          string `yaml:"host"`
	Model         string `yaml:"model,omitempty"`
	AllowControl  bool   `yaml:"allow_control"`
	Reachable     bool   `yaml:"reachable"`
	ActivityState string `yaml:"activity_state"`
	Bucket        string `yaml:"bucket"`
	Summary       string `yaml:"summary"`
}

type listPrintersFront struct {
	Printers []printerSummary `yaml:"printers"`
}

func listPrintersHandler(s *Server) func(context.Context, *mcp.CallToolRequest, listPrintersInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in listPrintersInput) (*mcp.CallToolResult, any, error) {
		reg, err := s.deps.LoadRegistry()
		if err != nil {
			return failure("load the printer registry", err, ""), nil, nil
		}
		enabled := reg.Enabled()
		summaries := make([]printerSummary, len(enabled))

		var wg sync.WaitGroup
		sem := make(chan struct{}, listPrintersConcurrency)
		for i, p := range enabled {
			i, p := i, p
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				pctx, cancel := context.WithTimeout(ctx, listPrintersTimeout)
				defer cancel()
				deps := s.deps.PrinterClients(p)
				snap := printerstate.Take(pctx, deps, p)
				derived := s.derive(snap)
				block := printerstate.BuildStateBlock(snap, derived, nil)
				summaries[i] = printerSummary{
					ID:            p.ID,
					Name:          p.Name,
					Host:          p.Host,
					Model:         p.Model,
					AllowControl:  p.AllowControl,
					Reachable:     snap.ServerInfoErr == nil,
					ActivityState: block.ActivityState,
					Bucket:        block.Bucket,
					Summary:       summarizeState(block),
				}
			}()
		}
		wg.Wait()

		front := &listPrintersFront{Printers: summaries}
		return render.SuccessResult(front, listPrintersBody(summaries)), nil, nil
	}
}

func listPrintersBody(printers []printerSummary) string {
	var b strings.Builder
	if len(printers) == 0 {
		b.WriteString("No printer is enabled. Add or enable a printer in the install wizard or the TUI; " +
			"this server has no tool that can do that for you.\n\n")
	} else {
		b.WriteString("Pass `printer` (its id or name, case insensitive) to any other tool to target one of " +
			"these; when only one printer is enabled it is used by default.\n\n")
	}
	b.WriteString("To add a new printer, enable a disabled one, or turn on control for one, use the install " +
		"wizard or the TUI: this server has no tool that can change the registry itself.")
	return b.String()
}

// --- discover_printers ---

// discoverPrinters runs one discovery pass. Production leaves it as
// defaultDiscoverPrinters, a zero-Options call to discovery.Discover (this
// machine's real LAN interfaces and a real dialer). Tests override this var
// to drive discovery's own Scan/Discover machinery through fake, loopback-
// only interfaces, dialers and fake Moonraker/9999 servers instead, so
// discover_printers is exercised through discovery's real, bounded,
// partial-reporting scan without ever opening a non-loopback socket
// (AGENTS.md hard testing rule).
var discoverPrinters = defaultDiscoverPrinters

func defaultDiscoverPrinters(ctx context.Context) (discovery.Report, error) {
	return discovery.Discover(ctx, discovery.Options{})
}

type discoverPrintersInput struct{}

type discoveredPrinter struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port,omitempty"`
	Hostname     string `yaml:"hostname,omitempty"`
	Model        string `yaml:"model,omitempty"`
	IdentifiedK2 bool   `yaml:"identified_k2"`
	Registered   bool   `yaml:"registered"`
	RegisteredAs string `yaml:"registered_as,omitempty"`
	Reason       string `yaml:"reason,omitempty"`
}

type discoverPrintersFront struct {
	Partial bool                `yaml:"partial"`
	Scanned int                 `yaml:"scanned"`
	Total   int                 `yaml:"total"`
	Found   []discoveredPrinter `yaml:"found"`
}

func discoverPrintersHandler(s *Server) func(context.Context, *mcp.CallToolRequest, discoverPrintersInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in discoverPrintersInput) (*mcp.CallToolResult, any, error) {
		reg, err := s.deps.LoadRegistry()
		if err != nil {
			return failure("load the printer registry", err, ""), nil, nil
		}
		report, err := discoverPrinters(ctx)
		if err != nil {
			return failure("scan the network for printers", err, ""), nil, nil
		}

		merged := discovery.Merge(report.Results, reg, false)
		found := make([]discoveredPrinter, 0, len(merged))
		for _, m := range merged {
			d := discoveredPrinter{
				Host:         m.Discovered.Host,
				Port:         m.Discovered.Port,
				Hostname:     m.Discovered.Hostname,
				Model:        m.Discovered.Model,
				IdentifiedK2: m.Discovered.IdentifiedK2,
				Registered:   m.Registered,
			}
			if m.Existing != nil {
				d.RegisteredAs = fmt.Sprintf("%s (%s)", m.Existing.ID, m.Existing.Name)
			}
			if !m.Registered {
				d.Reason = m.ReasonNotProposed
			}
			found = append(found, d)
		}

		front := &discoverPrintersFront{Partial: report.Partial, Scanned: report.Scanned, Total: report.Total, Found: found}
		return render.SuccessResult(front, discoverPrintersBody(report, found)), nil, nil
	}
}

func discoverPrintersBody(report discovery.Report, found []discoveredPrinter) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Scanned %d of %d host(s)", report.Scanned, report.Total)
	if report.Partial {
		b.WriteString(" (the scan did not finish within its time budget; call discover_printers again to " +
			"continue looking).\n\n")
	} else {
		b.WriteString(" (the scan completed).\n\n")
	}
	if len(found) == 0 {
		b.WriteString("No Creality K2 printers were found on the LAN. ")
	} else {
		fmt.Fprintf(&b, "Found %d host(s) that answered the Moonraker port. ", len(found))
	}
	b.WriteString("discover_printers never changes the registry: to add a new printer, enable one, or turn on " +
		"control for one, use the install wizard or the TUI. This server has no tool that can register a " +
		"printer for you.")
	return b.String()
}
