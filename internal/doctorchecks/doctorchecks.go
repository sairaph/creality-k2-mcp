// Package doctorchecks implements creality-k2-mcp's own health checks
// (dev_docs/plan-v0.1.0.md, T14): the printer registry, settings, every
// enabled printer's reachability, and the background camera/idle-heat
// daemon. Each check satisfies github.com/sairaph/mcp-wizard/doctor.Check
// and reads only: nothing here ever writes to the registry, settings or any
// other file, and nothing here starts the daemon just to check it.
package doctorchecks

import (
	"os"
	"strings"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
)

// Checks builds every check this package provides, for main.go's newDoctor
// to add to its doctor.Runner alongside the scaffold's own checks
// (executable, PATH, AI clients, update). The registry is read once here so
// the per-printer and daemon checks both know the enabled printer set
// without re-reading the file; RegistryCheck (below) re-reads it fresh in
// its own Run so its report always reflects the file at the moment doctor
// actually runs, not the moment Checks was called.
//
// knownTools is every tool this build can register (mcpserver.ToolCatalog,
// review backlog item 28), passed in by main.go rather than imported here so
// this package never depends on internal/mcpserver; it is forwarded to
// SettingsCheck.KnownTools so an unrecognised tools.overrides name in
// config.toml is flagged instead of merely noted. An empty knownTools (a
// caller that has not wired ToolCatalog in, or a test) falls back to
// SettingsCheck's own "cannot check these" behaviour.
func Checks(knownTools []domain.ToolInfo) []doctor.Check {
	reg, _, _, _ := loadEffectiveRegistry()
	enabled := reg.Enabled()

	identities := make([]string, 0, len(enabled))
	for _, p := range enabled {
		identities = append(identities, identityForDoctor(p))
	}

	return []doctor.Check{
		RegistryCheck{},
		SettingsCheck{KnownTools: knownTools},
		PrintersCheck{Printers: enabled},
		DaemonCheck{Identities: identities},
	}
}

// loadEffectiveRegistry resolves and reads the registry exactly the way
// runMCPServer's loadMCPRegistry (main.go) does: the K2_MCP_HOST
// environment override wins when set, otherwise the project registry (this
// process's current working directory) or, absent one, the global registry.
// source describes which one was used, for RegistryCheck's report; warnings
// are the invalid-entry and hostname-dedup messages LoadRegistryFile
// produces, never a reason to fail the whole load. It never writes.
func loadEffectiveRegistry() (reg domain.Registry, source string, warnings []string, err error) {
	if p, ok, envErr := domain.EnvOverride(); envErr != nil {
		return domain.Registry{}, "", nil, envErr
	} else if ok {
		return domain.Registry{Version: 1, Printers: []domain.Printer{p}},
			domain.EnvHost + " environment variable", nil, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return domain.Registry{}, "", nil, err
	}
	path, isProject, err := domain.RegistryPath(cwd)
	if err != nil {
		return domain.Registry{}, "", nil, err
	}
	reg, warnings, err = domain.LoadRegistryFile(path)
	if err != nil {
		return domain.Registry{}, "", nil, err
	}
	scope := "global"
	if isProject {
		scope = "project"
	}
	return reg, path + " (" + scope + ")", warnings, nil
}

// identityForDoctor is doctorchecks' own best-effort identity for
// DaemonCheck's watchdog-armed-heater tally (review backlog item 31): the
// registry's persisted Klipper hostname, verified at discovery time (review
// backlog item 2's merge fix requires one before a printer is proposed), or
// printerstate.UnverifiedIdentity when none is on file. This is
// intentionally not printerstate.Identity, the shared helper every other
// caller uses (internal/policy, internal/mcpserver): Checks builds this list
// once, before any live printer read, so there is no snapshot to verify a
// hostname against here; it can only report the hostname registration
// already verified, never the raw configured host, which is never the
// identity anything gets armed, locked or disarmed under.
func identityForDoctor(p domain.Printer) string {
	if h := strings.TrimSpace(p.Hostname); h != "" {
		return h
	}
	return printerstate.UnverifiedIdentity
}
