// Package wizard implements the "Printers" and "Settings" install-wizard
// steps for creality-k2-mcp (dev_docs/plan-v0.1.0.md decision 4), built on
// the mcp-wizard flow.Step abstraction the same way installer.HarnessStep
// and installer.ApplyStep are built.
//
// The business logic (scanning, merging discovery results with the
// registry, building the rows the TUI shows, saving the selection) is kept
// in plain functions in this file so the interactive step, a later
// non-install TUI screen, and the unattended install path
// (dev_docs/plan-v0.1.0.md decision 4, "Unattended install (--yes) calls
// the same discovery and merge functions directly") all share one
// implementation instead of three.
package wizard

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// DiscoverFunc matches discovery.Discover's signature. Steps and the
// unattended path take one as a dependency, defaulting to discovery.Discover,
// so tests can inject a fake that never opens a socket (AGENTS.md hard
// testing rule).
type DiscoverFunc func(ctx context.Context, opts discovery.Options) (discovery.Report, error)

// ProbeFunc matches discovery.ProbeHost's signature, injected for the same
// reason as DiscoverFunc.
type ProbeFunc func(ctx context.Context, host string, port int) (discovery.Result, error)

// PrinterRow is one row the Printers step shows: either an already
// registered printer, a newly discovered candidate proposed by
// discovery.Merge, or a host that answered but is not addable (not a
// confirmed K2, or a K2 whose Klipper hostname could not be read).
type PrinterRow struct {
	// Printer is the entry as it will be saved if this row is Addable.
	// Its Enabled and AllowControl fields are what the user is choosing in
	// the step; every other field is identity/display data from the
	// registry or from discovery.
	Printer domain.Printer
	// Existing is true when this row already has a registry entry (found
	// again by a scan, found by host, or simply not re-discovered this
	// scan).
	Existing bool
	// Addable is false for a row that cannot be toggled on: an
	// unidentified host, or a confirmed K2 without a Klipper hostname.
	// Reason explains why.
	Addable bool
	Reason  string
}

// key identifies a row across scans: the registry id when there is one
// (every Addable row has one, since a Proposed entry is only ever built
// from a real Klipper hostname), otherwise the lowercased host, so a
// not-addable row found again on rescan is recognised as the same row.
func (r PrinterRow) key() string {
	if r.Printer.ID != "" {
		return r.Printer.ID
	}
	return "host:" + strings.ToLower(strings.TrimSpace(r.Printer.Host))
}

// displayName is what the row shows in the list: the printer's name when it
// has one, its host otherwise.
func displayName(r PrinterRow) string {
	if r.Printer.Name != "" {
		return r.Printer.Name
	}
	return r.Printer.Host
}

// ScanAndMerge runs discover (discovery.Discover when discover is nil) and
// merges the report against reg, proposing new entries enabledByDefault for
// confirmed K2s that are not already registered
// (dev_docs/safety-architecture.md 3.4, discovery.Merge). It never mutates
// reg.
func ScanAndMerge(ctx context.Context, discover DiscoverFunc, opts discovery.Options, reg domain.Registry, enabledByDefault bool) (discovery.Report, []discovery.MergeResult, error) {
	if discover == nil {
		discover = discovery.Discover
	}
	report, err := discover(ctx, opts)
	if err != nil {
		return discovery.Report{}, nil, err
	}
	return report, discovery.Merge(report.Results, reg, enabledByDefault), nil
}

// ProbeAndMerge identifies one manually entered host (probe defaults to
// discovery.ProbeHost) and merges the single result against reg, matching
// ScanAndMerge's shape for the "add a host manually" action.
func ProbeAndMerge(ctx context.Context, probe ProbeFunc, host string, port int, reg domain.Registry) (discovery.Result, discovery.MergeResult, error) {
	if probe == nil {
		probe = discovery.ProbeHost
	}
	res, err := probe(ctx, host, port)
	if err != nil {
		return discovery.Result{}, discovery.MergeResult{}, err
	}
	merged := discovery.Merge([]discovery.Result{res}, reg, true)
	return res, merged[0], nil
}

// freshRowFromMerge builds the row for one discovery.MergeResult, ignoring
// any earlier session state (BuildRows and MergeRow layer that back in from
// a previous row with the same key).
func freshRowFromMerge(m discovery.MergeResult) PrinterRow {
	switch {
	case m.Registered && m.Existing != nil:
		return PrinterRow{Printer: *m.Existing, Existing: true, Addable: true}
	case m.Proposed != nil:
		return PrinterRow{Printer: *m.Proposed, Existing: false, Addable: true}
	default:
		name := m.Discovered.Host
		if m.Discovered.Model != "" {
			name = fmt.Sprintf("%s (%s)", m.Discovered.Host, m.Discovered.Model)
		}
		return PrinterRow{
			Printer: domain.Printer{Name: name, Host: m.Discovered.Host},
			Reason:  m.ReasonNotProposed,
		}
	}
}

// applyPrevious carries the user's in-session Enabled/AllowControl choice
// from prev onto row, so a rescan or a manual probe never silently discards
// a toggle the user already made this session.
func applyPrevious(row, prev PrinterRow) PrinterRow {
	row.Printer.Enabled = prev.Printer.Enabled
	row.Printer.AllowControl = prev.Printer.AllowControl
	return row
}

func indexRows(rows []PrinterRow) map[string]PrinterRow {
	idx := make(map[string]PrinterRow, len(rows))
	for _, r := range rows {
		idx[r.key()] = r
	}
	return idx
}

// findRow returns a pointer to the row in rows with the given key, or nil.
func findRow(rows []PrinterRow, key string) *PrinterRow {
	for i := range rows {
		if rows[i].key() == key {
			return &rows[i]
		}
	}
	return nil
}

// upsertRow replaces the row in rows matching row's key, or appends it.
func upsertRow(rows []PrinterRow, row PrinterRow) []PrinterRow {
	for i := range rows {
		if rows[i].key() == row.key() {
			rows[i] = row
			return rows
		}
	}
	return append(rows, row)
}

func sortRows(rows []PrinterRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(displayName(rows[i])) < strings.ToLower(displayName(rows[j]))
	})
}

// BuildRows turns a scan's merged results, plus every registry entry the
// scan did not re-discover (so an offline printer is never dropped from the
// list), into the rows the Printers step shows. previous carries the
// Enabled/AllowControl the user already chose this session (nil on the
// first build) forward onto any row with a matching key, including a
// not-addable row found again on rescan.
func BuildRows(reg domain.Registry, merged []discovery.MergeResult, previous []PrinterRow) []PrinterRow {
	prevByKey := indexRows(previous)
	seen := make(map[string]bool, len(merged)+len(reg.Printers))
	rows := make([]PrinterRow, 0, len(merged)+len(reg.Printers))

	for _, m := range merged {
		row := freshRowFromMerge(m)
		if prev, ok := prevByKey[row.key()]; ok {
			row = applyPrevious(row, prev)
		}
		rows = append(rows, row)
		if row.Addable {
			seen[row.key()] = true
		}
	}

	for _, p := range reg.Printers {
		row := PrinterRow{Printer: p, Existing: true, Addable: true}
		if seen[row.key()] {
			continue
		}
		if prev, ok := prevByKey[row.key()]; ok {
			row = applyPrevious(row, prev)
		}
		rows = append(rows, row)
		seen[row.key()] = true
	}

	sortRows(rows)
	return rows
}

// MergeRow folds one discovery.MergeResult (from ProbeAndMerge, the "add a
// host manually" action) into rows, replacing a row with the same key or
// appending a new one, carrying forward that row's Enabled/AllowControl if
// it already existed this session.
func MergeRow(rows []PrinterRow, m discovery.MergeResult) []PrinterRow {
	row := freshRowFromMerge(m)
	if prev := findRow(rows, row.key()); prev != nil {
		row = applyPrevious(row, *prev)
	}
	rows = upsertRow(rows, row)
	sortRows(rows)
	return rows
}

// registryFromRows builds the registry SaveSelection would persist (and what
// a dry run previews) from the Addable rows: a not-addable row has no valid
// hostname and is never saved. AllowControl is forced off whenever Enabled is
// false, so a disabled printer can never carry control forward from a stale
// in-session toggle or a hand-edited registry file (safety-architecture 3.4:
// a printer only allows control while it is enabled).
func registryFromRows(rows []PrinterRow) domain.Registry {
	var reg domain.Registry
	for _, r := range rows {
		if !r.Addable {
			continue
		}
		p := r.Printer
		if !p.Enabled {
			p.AllowControl = false
		}
		reg.Printers = append(reg.Printers, p)
	}
	return reg
}

// SaveSelection builds a registry from the Addable rows (see
// registryFromRows) and atomically writes it to the registry file resolved
// for dir: the project file when dir names a directory that already has one,
// the global file otherwise (domain.RegistryPath), matching the resolution
// domain.LoadRegistry uses to load it. dir is "" for the global install
// wizard, the project directory for the "add" wizard.
func SaveSelection(dir string, rows []PrinterRow) (domain.Registry, string, error) {
	reg := registryFromRows(rows)
	path, _, err := domain.RegistryPath(dir)
	if err != nil {
		return domain.Registry{}, "", err
	}
	if err := domain.SaveRegistry(path, reg); err != nil {
		return domain.Registry{}, path, err
	}
	return reg, path, nil
}

// UnattendedResult is what UnattendedInstall did, for PrintUnattendedResult.
type UnattendedResult struct {
	RegistryPath string
	// Identified is how many confirmed K2s this scan found, registered or
	// not, enabled or not: the count that matters for "nothing was found".
	Identified int
	// Added holds newly registered printers (enabled, control off).
	Added []domain.Printer
	// Enabled holds already-registered printers this run turned on (and, if
	// it was on, turned control off for).
	Enabled []domain.Printer
	// Partial is true when the scan did not finish within its time budget.
	Partial bool
}

// UnattendedInstall runs discover (discovery.Discover when nil), merges the
// report against the registry resolved for dir the same way SaveSelection
// resolves it, registers every confirmed K2 enabled with control off
// (dev_docs/plan-v0.1.0.md decision 4: "every discovered printer enabled,
// control off"), and makes sure a settings file exists (writing the
// defaults if one does not; an existing settings file is left untouched).
// It never disables or removes a printer nor changes an already-enabled
// one's control setting to on.
func UnattendedInstall(ctx context.Context, discover DiscoverFunc, dir string) (UnattendedResult, error) {
	reg, path, _, err := domain.LoadRegistry(dir)
	if err != nil {
		return UnattendedResult{}, err
	}

	report, merged, err := ScanAndMerge(ctx, discover, discovery.Options{}, reg, true)
	if err != nil {
		return UnattendedResult{}, err
	}

	res := UnattendedResult{RegistryPath: path, Partial: report.Partial}
	changed := false
	for _, m := range merged {
		if !m.Discovered.IdentifiedK2 {
			continue
		}
		res.Identified++
		switch {
		case m.Registered && m.Existing != nil:
			if m.Existing.Enabled && !m.Existing.AllowControl {
				continue
			}
			for i := range reg.Printers {
				if reg.Printers[i].ID != m.Existing.ID {
					continue
				}
				reg.Printers[i].Enabled = true
				reg.Printers[i].AllowControl = false
				res.Enabled = append(res.Enabled, reg.Printers[i])
				changed = true
			}
		case m.Proposed != nil:
			p := *m.Proposed
			p.Enabled = true
			p.AllowControl = false
			reg, err = domain.AddPrinter(reg, p)
			if err != nil {
				return UnattendedResult{}, err
			}
			res.Added = append(res.Added, p)
			changed = true
		}
	}

	if changed {
		if err := domain.SaveRegistry(path, reg); err != nil {
			return UnattendedResult{}, err
		}
	}

	settingsPath, err := domain.SettingsPath()
	if err != nil {
		return UnattendedResult{}, err
	}
	if _, err := domain.LoadSettings(settingsPath); err != nil {
		return UnattendedResult{}, err
	}

	return res, nil
}

// PrintUnattendedResult reports what UnattendedInstall did. When nothing was
// identified it tells the user how to use K2_MCP_HOST instead
// (dev_docs/plan-v0.1.0.md decision 4).
func PrintUnattendedResult(w io.Writer, res UnattendedResult) {
	if res.Identified == 0 {
		fmt.Fprintln(w, "  No Creality K2 printers were found on the network.")
		fmt.Fprintf(w, "  If the printer is not reachable by a network scan, set %s (and optionally %s, %s, %s) so the server talks to it directly, without a registry entry.\n",
			domain.EnvHost, domain.EnvPort, domain.EnvAPIKey, domain.EnvAllowControl)
		return
	}
	fmt.Fprintf(w, "  Found %d Creality K2 printer(s).\n", res.Identified)
	for _, p := range res.Added {
		fmt.Fprintf(w, "  added    %-22s %s  (control: off)\n", p.Name, p.Host)
	}
	for _, p := range res.Enabled {
		fmt.Fprintf(w, "  enabled  %-22s %s  (control: off)\n", p.Name, p.Host)
	}
	if len(res.Added) == 0 && len(res.Enabled) == 0 {
		fmt.Fprintln(w, "  Every discovered printer was already registered, enabled, with control off.")
	}
	fmt.Fprintf(w, "  Registry: %s\n", res.RegistryPath)
	if res.Partial {
		fmt.Fprintln(w, "  The scan did not finish within its time budget; some printers on the network may be missing. Re-run to try again.")
	}
}
