package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/filaments"
	"github.com/sairaph/creality-k2-mcp/internal/policy"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
)

// This file implements the filament tool group (dev_docs/plan-v0.2.0.md
// section 4): get_filaments and list_filament_catalog (monitor) and
// set_filament_definition (control). The reads share internal/filaments with
// the "filaments" CLI command; the write goes through policy.Execute like
// every other control tool. Names come only from the printer (decision V1).

func registerFilamentTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "get_filaments", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "get_filaments",
		Description: "Shows the filament in the CFS (Creality Filament System) and the side spool holder: per unit its " +
			"model, temperature and humidity, and per slot (T1A to T4D, side_spool) the stored definition: status " +
			"(defined = a definition is stored and it is not RFID, rfid = an RFID-tagged spool, undefined = no definition, unknown), brand, name, " +
			"material, colour, the nozzle temperature range, whether the slot is selected at the filament hub, and " +
			"whether set_filament_definition can edit it right now (editable; why_not appears only for a slot-level reason: " +
			"an RFID spool, an undefined slot, a slot being written, one selected at the hub. A printer-wide reason, " +
			"a printing printer, a busy CFS, a print start in flight or control off, makes every slot editable false and is " +
			"stated once in edit_blocked, not repeated per slot). It also shows " +
			"the printer's own refill_groups (slots it treats as interchangeable for auto-refill), auto_refill, and " +
			"the CFS state (idle, busy, in_print, error or unknown) in the cfs block of the frontmatter. The status " +
			"is the printer's stored definition, not a sensor: whether filament is physically loaded in a slot " +
			"cannot be detected, so ask the user which spool is in which slot before relying on it. Names come " +
			"only from the printer over port 9999; when that is unreachable only Moonraker's material codes and " +
			"colours are shown and the reply says so. Related: list_filament_catalog for the ids " +
			"set_filament_definition accepts, start_print for the mapping proposal that uses these slots.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getFilamentsHandler(s))

	registerTool(s, domain.ToolInfo{Name: "list_filament_catalog", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "list_filament_catalog",
		Description: "Lists the printer's own filament catalog (the definitions a slot can be set to): id (the 5 " +
			"character catalog id), brand, name, material, nozzle temperature range, and whether the entry can be " +
			"written to a slot right now (writable, with why_not when not: an entry with no temperature range, one above the " +
			"printer's nozzle limit, or a printer state that does not allow an edit). Without brand and material filters " +
			"it returns the whole catalog (dozens of entries), so filter when you can. " +
			"Optional brand and material filters match exactly, case-insensitively. Nothing of Creality's catalog is " +
			"built into this server: the list is read from the printer over port 9999, so it needs the printer " +
			"reachable. Related: set_filament_definition takes an id or an exact name from this list, get_filaments " +
			"shows what each slot currently holds.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listFilamentCatalogHandler(s))

	registerTool(s, domain.ToolInfo{Name: "set_filament_definition", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "set_filament_definition",
		Description: "Rewrites one slot's filament definition on the printer: the material, brand and name of one " +
			"catalog entry (its id or exact name, see list_filament_catalog), the nozzle temperature range and " +
			"pressure advance of that entry, and the colour (#rrggbb). It only relabels the slot and never moves " +
			"filament. Before calling it, ask the user which spool is physically in the slot and confirm the new " +
			"definition matches it: a wrong definition makes the printer use wrong temperatures and can change which " +
			"slots auto-refill swaps between (the reply shows the printer's refill groups before and after). It is " +
			"persistent until changed again. Only available while the printer is idle and the CFS is quiescent; a " +
			"slot with an RFID-tagged spool, an undefined or not-ready slot, a slot being written, or the slot " +
			"currently selected at the filament hub is refused with the reason (get_filaments shows editable and " +
			"why_not per slot). The side spool can be edited too (verified on hardware). Sends " +
			"immediately with no confirm_token and reads the result back from both the printer and Moonraker: effect " +
			"confirmed means both agree, unconfirmed shows what each channel reports, no_change means the slot " +
			"already held that definition. Related: get_filaments, list_filament_catalog.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true},
	}, setFilamentDefinitionHandler(s))
}

// --- get_filaments ---

type getFilamentsInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
}

type getFilamentsFront struct {
	printerstate.StateBlock `yaml:",inline"`
	filaments.View          `yaml:",inline"`
}

func (f *getFilamentsFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func getFilamentsHandler(s *Server) func(context.Context, *mcp.CallToolRequest, getFilamentsInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in getFilamentsInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		view := filaments.Read(ctx, deps.WS9999, snap, derived.CFSConnected, filaments.EditGate(printer, derived, s.deps.Settings))
		front := &getFilamentsFront{StateBlock: block, View: view}
		body := filaments.Text(view)
		if view.NamesAvailable && view.EditBlocked == "" {
			body += "\nTo change a slot, ask the user what spool is really in it, pick an entry with list_filament_catalog, " +
				"then call set_filament_definition. To print with these slots, call start_print with a file that is on the printer."
		}
		return successResult(front, nil, strings.TrimRight(body, "\n")), nil, nil
	}
}

// --- list_filament_catalog ---

type listFilamentCatalogInput struct {
	Printer  *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Brand    *string `json:"brand,omitempty" jsonschema:"only entries of this brand (exact, case insensitive)"`
	Material *string `json:"material,omitempty" jsonschema:"only entries of this material type, for example PLA (exact, case insensitive)"`
}

type listFilamentCatalogFront struct {
	printerstate.StateBlock `yaml:",inline"`
	Count                   int                      `yaml:"count"`
	Entries                 []filaments.CatalogEntry `yaml:"entries"`
}

func (f *listFilamentCatalogFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func listFilamentCatalogHandler(s *Server) func(context.Context, *mcp.CallToolRequest, listFilamentCatalogInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in listFilamentCatalogInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := s.derive(snap)
		block := printerstate.BuildStateBlock(snap, derived, nil)

		brand, material := "", ""
		if in.Brand != nil {
			brand = *in.Brand
		}
		if in.Material != nil {
			material = *in.Material
		}
		var nozzleCap *float64
		if snap.ProductParam != nil {
			nozzleCap = snap.ProductParam.NozzleTemp
		}
		entries, err := filaments.Catalog(ctx, deps.WS9999, brand, material, filaments.EditGate(printer, derived, s.deps.Settings), nozzleCap)
		if err != nil {
			return render.ErrorResult(render.Error{
				Code:    render.CodeUnavailable,
				Message: "the printer's filament catalog could not be read over port 9999: " + shortMessage(err.Error()),
				Hint:    "Check that the printer is reachable (get_printer_status shows ws9999_reachable), then call again. This server has no built-in catalog: names only ever come from the printer.",
			}), nil, nil
		}
		front := &listFilamentCatalogFront{StateBlock: block, Count: len(entries), Entries: entries}
		body := fmt.Sprintf("%d catalog entr", len(entries))
		if len(entries) == 1 {
			body += "y"
		} else {
			body += "ies"
		}
		body += " from the printer. Pass an id or an exact name as material to set_filament_definition; entries that are " +
			"not writable say why."
		return successResult(front, nil, body), nil, nil
	}
}

// --- set_filament_definition ---

type setFilamentDefinitionInput struct {
	Printer  *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
	Slot     string  `json:"slot" jsonschema:"the slot to edit: T1A to T4D (unit 1 to 4, slot A to D) or side_spool (case insensitive)"`
	Material string  `json:"material" jsonschema:"the catalog entry: its 5 character id or its exact name, as listed by list_filament_catalog"`
	Color    string  `json:"color" jsonschema:"the slot colour as #rrggbb or rrggbb"`
}

func setFilamentDefinitionHandler(s *Server) func(context.Context, *mcp.CallToolRequest, setFilamentDefinitionInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in setFilamentDefinitionInput) (*mcp.CallToolResult, any, error) {
		printer, errRes := s.resolvePrinter(printerQuery(in.Printer))
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := s.executeControl(ctx, printer, policy.ActionSetFilamentDefinition,
			policy.Params{Slot: in.Slot, Material: in.Material, Color: in.Color}, "")
		if err != nil {
			return controlFailure(err), nil, nil
		}
		front := controlFrontFrom(res)
		return successResult(front, nil, controlBody(res)), nil, nil
	}
}
