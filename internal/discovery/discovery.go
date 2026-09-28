// Package discovery finds Creality K2 printers on the LAN and identifies
// them, and is the one place the wizard step, the unattended install path,
// the TUI, "printers scan" and the discover_printers tool share this logic
// (dev_docs/plan-v0.1.0.md decision 3; dev_docs/safety-architecture.md).
//
// Three things are kept independently testable:
//
//   - Subnets/TargetHosts: which host addresses to try, derived from this
//     machine's network interfaces (InterfaceSource, injectable).
//   - Scan: trying those hosts for an open Moonraker port under a
//     concurrency and time budget, then identifying whichever ones answer
//     (Dialer, injectable).
//   - identify (used by both Scan and ProbeHost): the actual K2 confirmation
//     sequence against one host, through internal/moonraker and
//     internal/crealityws.
//
// Nothing in this package scans or identifies without being asked to; it
// never mutates a domain.Registry (Merge only reports and proposes).
package discovery

// Result is what discovery learned about one host, whether found by a scan
// or by ProbeHost. Port is the Moonraker port that was tried. Hostname,
// Model and APIVersion are empty when they could not be read. Reason
// explains why IdentifiedK2 is false; Errors keeps a short message per
// failed step (never the full text of a hostile or malformed response).
type Result struct {
	Host         string   `json:"host"`
	Port         int      `json:"port"`
	Hostname     string   `json:"hostname,omitempty"`
	Model        string   `json:"model,omitempty"`
	APIVersion   string   `json:"api_version,omitempty"`
	IdentifiedK2 bool     `json:"identified_k2"`
	Reason       string   `json:"reason,omitempty"`
	Errors       []string `json:"errors,omitempty"`
}
