package discovery

import (
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func TestMergeMatchesByHostname(t *testing.T) {
	reg := domain.Registry{Printers: []domain.Printer{
		{ID: "k2-5885", Name: "k2-5885", Host: "192.168.1.50", MoonrakerPort: 7125, Hostname: "K2-5885", Enabled: true},
	}}
	results := []Result{
		{Host: "192.168.1.99", Port: 7125, Hostname: "k2-5885", Model: "F021", IdentifiedK2: true},
	}

	out := Merge(results, reg, true)
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if !out[0].Registered {
		t.Fatalf("Registered = false, want true (case-insensitive hostname match); out = %+v", out[0])
	}
	if out[0].Proposed != nil {
		t.Fatalf("Proposed = %+v, want nil for a registered printer", out[0].Proposed)
	}
	if out[0].Existing == nil || out[0].Existing.ID != "k2-5885" {
		t.Fatalf("Existing = %+v, want the k2-5885 entry", out[0].Existing)
	}
}

func TestMergeMatchesByHostWhenHostnameUnknown(t *testing.T) {
	reg := domain.Registry{Printers: []domain.Printer{
		{ID: "env", Name: "env", Host: "192.168.1.50", MoonrakerPort: 7125, Enabled: true},
	}}
	results := []Result{
		{Host: "192.168.1.50", Port: 7125}, // not identified, no hostname
	}

	out := Merge(results, reg, true)
	if !out[0].Registered {
		t.Fatalf("Registered = false, want true (host match); out = %+v", out[0])
	}
	if out[0].Existing.ID != "env" {
		t.Fatalf("Existing.ID = %q, want env", out[0].Existing.ID)
	}
}

func TestMergeHostnameTakesPriorityOverHost(t *testing.T) {
	reg := domain.Registry{Printers: []domain.Printer{
		{ID: "stale-ip-entry", Name: "stale", Host: "192.168.1.99", MoonrakerPort: 7125, Enabled: true},
		{ID: "k2-5885", Name: "k2-5885", Host: "192.168.1.50", MoonrakerPort: 7125, Hostname: "K2-5885", Enabled: true},
	}}
	// This result's current IP matches the stale entry's recorded host, but
	// its Klipper hostname matches a different, more current entry. The
	// hostname match must win (safety-architecture.md 3.4: hostname is the
	// stable identity).
	results := []Result{
		{Host: "192.168.1.99", Port: 7125, Hostname: "k2-5885", Model: "F021", IdentifiedK2: true},
	}

	out := Merge(results, reg, true)
	if out[0].Existing.ID != "k2-5885" {
		t.Fatalf("Existing.ID = %q, want k2-5885 (hostname match should win over host match)", out[0].Existing.ID)
	}
}

func TestMergeProposesNewEntry(t *testing.T) {
	reg := domain.Registry{}
	results := []Result{
		{Host: "192.168.1.60", Port: 7125, Hostname: "K2-9001", Model: "F021", APIVersion: "1.5.0", IdentifiedK2: true},
	}

	out := Merge(results, reg, true)
	if out[0].Registered {
		t.Fatal("Registered = true, want false: empty registry")
	}
	p := out[0].Proposed
	if p == nil {
		t.Fatal("Proposed = nil, want a new entry")
	}
	if p.ID != domain.DeriveID("K2-9001") {
		t.Fatalf("ID = %q, want %q", p.ID, domain.DeriveID("K2-9001"))
	}
	if p.Name != "K2-9001" {
		t.Fatalf("Name = %q, want K2-9001 (the hostname)", p.Name)
	}
	if p.Hostname != "K2-9001" {
		t.Fatalf("Hostname = %q, want K2-9001", p.Hostname)
	}
	if p.Host != "192.168.1.60" {
		t.Fatalf("Host = %q, want 192.168.1.60", p.Host)
	}
	if p.MoonrakerPort != 7125 {
		t.Fatalf("MoonrakerPort = %d, want 7125", p.MoonrakerPort)
	}
	if p.Model != "F021" {
		t.Fatalf("Model = %q, want F021", p.Model)
	}
	if !p.Enabled {
		t.Fatal("Enabled = false, want true (caller chose true)")
	}
	if p.AllowControl {
		t.Fatal("AllowControl = true, want false always for a discovered printer")
	}
}

func TestMergeProposedEntryEnabledPerCaller(t *testing.T) {
	results := []Result{{Host: "192.168.1.60", Hostname: "K2-9001", IdentifiedK2: true}}

	out := Merge(results, domain.Registry{}, false)
	if out[0].Proposed.Enabled {
		t.Fatal("Enabled = true, want false when the caller chose false")
	}
}

func TestMergeNoProposalForUnidentifiedHost(t *testing.T) {
	results := []Result{
		{Host: "192.168.1.61", Port: 7125, Reason: "connection refused"}, // never identified as a K2
	}

	out := Merge(results, domain.Registry{}, true)
	if out[0].Registered {
		t.Fatal("Registered = true, want false: empty registry")
	}
	if out[0].Proposed != nil {
		t.Fatalf("Proposed = %+v, want nil for a host never identified as a K2", out[0].Proposed)
	}
	if out[0].ReasonNotProposed == "" {
		t.Fatal("ReasonNotProposed = \"\", want a reason")
	}
}

func TestMergeNoProposalForNonK2Host(t *testing.T) {
	// Identified a Klipper hostname, but IdentifiedK2 is false: some other
	// Klipper printer answered, not a Creality K2.
	results := []Result{
		{Host: "192.168.1.62", Port: 7125, Hostname: "some-other-printer", IdentifiedK2: false, Reason: "not a Creality K2"},
	}

	out := Merge(results, domain.Registry{}, true)
	if out[0].Proposed != nil {
		t.Fatalf("Proposed = %+v, want nil for a non-K2 host", out[0].Proposed)
	}
	if out[0].ReasonNotProposed != "not a Creality K2" {
		t.Fatalf("ReasonNotProposed = %q, want the discovery reason", out[0].ReasonNotProposed)
	}
}

func TestMergeNoProposalForIdentifiedK2WithoutHostname(t *testing.T) {
	// Confirmed a K2, but printer/info never gave us its Klipper hostname:
	// the registry requires one (safety-architecture.md 3.4), so nothing is
	// proposed, but the host is still reported with a reason.
	results := []Result{
		{Host: "192.168.1.63", Port: 7125, IdentifiedK2: true},
	}

	out := Merge(results, domain.Registry{}, true)
	if out[0].Registered {
		t.Fatal("Registered = true, want false: empty registry")
	}
	if out[0].Proposed != nil {
		t.Fatalf("Proposed = %+v, want nil: no Klipper hostname", out[0].Proposed)
	}
	if out[0].ReasonNotProposed == "" {
		t.Fatal("ReasonNotProposed = \"\", want a reason")
	}
}

func TestMergeProposesIdentifiedK2WithHostname(t *testing.T) {
	results := []Result{
		{Host: "192.168.1.64", Port: 7125, Hostname: "K2-4242", Model: "F021", IdentifiedK2: true},
	}

	out := Merge(results, domain.Registry{}, true)
	p := out[0].Proposed
	if p == nil {
		t.Fatal("Proposed = nil, want an entry for an identified K2 with a hostname")
	}
	if p.Hostname != "K2-4242" {
		t.Fatalf("Hostname = %q, want K2-4242", p.Hostname)
	}
	if p.ID != domain.DeriveID("K2-4242") {
		t.Fatalf("ID = %q, want derived from the hostname", p.ID)
	}
	if out[0].ReasonNotProposed != "" {
		t.Fatalf("ReasonNotProposed = %q, want empty when a proposal was made", out[0].ReasonNotProposed)
	}
}

func TestMergeDoesNotMutateRegistryOrResults(t *testing.T) {
	reg := domain.Registry{Printers: []domain.Printer{
		{ID: "k2-5885", Name: "k2-5885", Host: "192.168.1.50", MoonrakerPort: 7125, Hostname: "K2-5885", Enabled: true},
	}}
	results := []Result{
		{Host: "192.168.1.50", Port: 7125, Hostname: "K2-5885", Model: "F021", IdentifiedK2: true},
	}

	out := Merge(results, reg, true)

	// Mutate the returned copy; the input registry must be untouched.
	out[0].Existing.Name = "mutated"
	out[0].Existing.Enabled = false

	if reg.Printers[0].Name != "k2-5885" {
		t.Fatalf("input registry was mutated: Name = %q", reg.Printers[0].Name)
	}
	if !reg.Printers[0].Enabled {
		t.Fatal("input registry was mutated: Enabled = false")
	}
}

func TestMergeMultipleResultsIndependent(t *testing.T) {
	reg := domain.Registry{Printers: []domain.Printer{
		{ID: "k2-5885", Name: "k2-5885", Host: "192.168.1.50", MoonrakerPort: 7125, Hostname: "K2-5885", Enabled: true},
	}}
	results := []Result{
		{Host: "192.168.1.50", Port: 7125, Hostname: "K2-5885", Model: "F021", IdentifiedK2: true},
		{Host: "192.168.1.61", Port: 7125, Hostname: "K2-1111", Model: "F021", IdentifiedK2: true},
	}

	out := Merge(results, reg, true)
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if !out[0].Registered || out[1].Registered {
		t.Fatalf("expected [registered, new], got %+v", out)
	}
}
