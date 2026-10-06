// Package doctorlist builds the list of doctor checks this server runs. The
// `doctor` command (main.go's runDoctor) and the app's Doctor screen both take
// it from here, so the screen can run the checks one by one and show each row
// as it finishes while the command's text output stays what mcp-wizard's
// doctor.Runner has always printed.
package doctorlist

import (
	"context"
	"fmt"
	"strings"

	"github.com/sairaph/mcp-wizard/doctor"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/update"

	"github.com/sairaph/creality-k2-mcp/internal/doctorchecks"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// Options is everything the list needs from the process that builds it.
type Options struct {
	// Version is this binary's version ("dev" for a development build, which
	// gets no Update check).
	Version string
	// Update carries the owner, repo and install directory (InstallDir) the
	// PATH and Update checks use.
	Update update.Options
	// Tools is the full registered tool set (mcpserver.ToolCatalog), passed in
	// so this package never depends on internal/mcpserver; it lets the
	// settings check flag a tools.overrides name that matches no real tool.
	Tools []domain.ToolInfo
	// NewDetector builds the AI client detector for a server name (main.go's
	// newDetector).
	NewDetector func(name string) (*harness.Detector, error)
}

// Checks builds the check list: Executable, PATH, Version, AI clients, Update
// (release builds only), then this server's own checks (registry, settings,
// every enabled printer's reachability and the background daemon). Building it
// reads the registry once to learn which printers are enabled; no check runs
// and nothing is written.
func Checks(opts Options) []doctor.Check {
	checks := []doctor.Check{
		doctorchecks.ExecutableCheck{},
		doctor.PathCheck{Dir: opts.Update.InstallDir},
		versionCheck{version: opts.Version},
		clientsCheck{newDetector: opts.NewDetector},
	}
	if opts.Version != "dev" {
		checks = append(checks, doctor.UpdateCheck{Opts: opts.Update})
	}
	return append(checks, doctorchecks.Checks(opts.Tools)...)
}

// versionCheck reports this binary's version and how to spot an AI client
// still running an older server process after an update (field feedback
// item 4): tool replies that carry the printer state, and list_printers, name
// the process that answered in their server_version and server_pid fields.
type versionCheck struct{ version string }

func (versionCheck) Name() string { return "Version" }

func (c versionCheck) Run(context.Context) doctor.Result {
	return doctor.Result{Name: "Version", Status: doctor.OK, Detail: fmt.Sprintf(
		"%s %s. An AI client keeps the server process it started: if server_version in a tool reply "+
			"differs from this, restart the AI client (or its MCP connection). Never kill the server process "+
			"itself: a client may not start it again.", domain.BinaryName, c.version)}
}

// clientsCheck lists the AI clients that have this server registered.
type clientsCheck struct {
	newDetector func(name string) (*harness.Detector, error)
}

func (clientsCheck) Name() string { return "AI clients" }

func (c clientsCheck) Run(ctx context.Context) doctor.Result {
	if c.newDetector == nil {
		return doctor.Result{Name: "AI clients", Status: doctor.Fail, Detail: "no client detector is available"}
	}
	detector, err := c.newDetector(domain.ServerName)
	if err != nil {
		return doctor.Result{Name: "AI clients", Status: doctor.Fail, Detail: err.Error()}
	}
	var configured []string
	for _, h := range detector.Detect(ctx) {
		if h.Configured {
			configured = append(configured, h.Name)
		}
	}
	if len(configured) == 0 {
		return doctor.Result{Name: "AI clients", Status: doctor.Warn, Detail: "no client is configured; run `creality-k2-mcp install`"}
	}
	return doctor.Result{Name: "AI clients", Status: doctor.OK, Detail: strings.Join(configured, ", ")}
}
