package discovery

import (
	"strings"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// MergeResult pairs one discovered Result with what the registry already
// knows about it, so the wizard step, the TUI, the CLI and the
// discover_printers tool all share one answer to "already registered or
// new" instead of four copies of the matching logic.
type MergeResult struct {
	Discovered Result
	// Registered is true when Discovered matches an existing registry
	// entry, by Klipper hostname first, then by host.
	Registered bool
	// Existing is a copy of the matched entry when Registered is true, nil
	// otherwise. It is a copy: mutating it never affects the registry Merge
	// was given.
	Existing *domain.Printer
	// Proposed is a ready-to-add registry entry when Registered is false and
	// Discovered was confirmed as a K2 with a Klipper hostname (from
	// Moonraker printer/info), nil otherwise: id from domain.DeriveID, name
	// and hostname from the Klipper hostname discovery read, Enabled as the
	// caller chose, AllowControl always false (plan-v0.1.0.md decision 2: a
	// discovered printer only gets control permission when the user turns it
	// on). A registry entry keyed by anything other than a real Klipper
	// hostname cannot be locked or deduplicated reliably
	// (safety-architecture.md 3.4), so a host that was not identified as a K2
	// or whose hostname could not be read is never proposed; it is still
	// reported on the MergeResult with ReasonNotProposed explaining why.
	Proposed *domain.Printer
	// ReasonNotProposed explains why Proposed is nil when Registered is also
	// false. Empty whenever Registered is true or Proposed is set.
	ReasonNotProposed string
}

// Merge reports, for each discovered result, whether it is already in reg,
// and proposes a new entry for the ones that are not, confirmed K2 hosts with
// a known Klipper hostname. It never mutates reg or results: every Registry
// field it reads is copied before being handed back on a MergeResult.
func Merge(results []Result, reg domain.Registry, enabledByDefault bool) []MergeResult {
	out := make([]MergeResult, 0, len(results))
	for _, r := range results {
		m := MergeResult{Discovered: r}
		if existing := findRegistered(reg, r); existing != nil {
			cp := *existing
			m.Registered = true
			m.Existing = &cp
		} else if r.IdentifiedK2 && r.Hostname != "" {
			proposed := proposeEntry(r, enabledByDefault)
			m.Proposed = &proposed
		} else {
			m.ReasonNotProposed = reasonNotProposed(r)
		}
		out = append(out, m)
	}
	return out
}

// reasonNotProposed explains, for a host that did not match an existing
// registry entry, why Merge is not proposing one: either it was never
// confirmed as a K2, or it was confirmed but Moonraker's printer/info did not
// give us its Klipper hostname, which the registry requires for a stable
// identity (safety-architecture.md 3.4).
func reasonNotProposed(r Result) string {
	if !r.IdentifiedK2 {
		if r.Reason != "" {
			return r.Reason
		}
		return "not identified as a Creality K2"
	}
	return "identified as a K2 but its Klipper hostname could not be read from printer/info; a registry entry needs a hostname"
}

// findRegistered looks for an entry in reg matching r, by Klipper hostname
// first (case insensitive; that is the stable identity the safety
// architecture locks on, safety-architecture.md 3.4), then by host.
func findRegistered(reg domain.Registry, r Result) *domain.Printer {
	if r.Hostname != "" {
		for i := range reg.Printers {
			if reg.Printers[i].Hostname != "" && strings.EqualFold(reg.Printers[i].Hostname, r.Hostname) {
				return &reg.Printers[i]
			}
		}
	}
	if r.Host != "" {
		for i := range reg.Printers {
			if strings.EqualFold(reg.Printers[i].Host, r.Host) {
				return &reg.Printers[i]
			}
		}
	}
	return nil
}

// proposeEntry builds a new registry entry for a discovered printer that is
// not registered yet. The caller has already checked r.IdentifiedK2 and
// r.Hostname != "", so id and name are always derived from the real Klipper
// hostname, never from the host.
func proposeEntry(r Result, enabled bool) domain.Printer {
	p := domain.NewPrinter(r.Hostname, r.Host)
	if r.Port != 0 {
		p.MoonrakerPort = r.Port
	}
	p.Model = r.Model
	p.Enabled = enabled
	p.AllowControl = false
	return p
}
