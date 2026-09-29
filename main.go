package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sairaph/mcp-wizard/cli"
	"github.com/sairaph/mcp-wizard/command"
	"github.com/sairaph/mcp-wizard/doctor"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/harness"
	"github.com/sairaph/mcp-wizard/installer"
	"github.com/sairaph/mcp-wizard/tui"
	"github.com/sairaph/mcp-wizard/update"

	"github.com/sairaph/creality-k2-mcp/internal/clicmd"
	"github.com/sairaph/creality-k2-mcp/internal/daemon"
	daemonclient "github.com/sairaph/creality-k2-mcp/internal/daemon/client"
	"github.com/sairaph/creality-k2-mcp/internal/doctorchecks"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/mcpserver"
	internaltui "github.com/sairaph/creality-k2-mcp/internal/tui"
	"github.com/sairaph/creality-k2-mcp/internal/wizard"
)

// version is set by goreleaser via -ldflags "-X main.version=...".
var version = "dev"

var oneShotCommands = command.New()

func init() {
	// One-shot CLI commands share business logic with the MCP tools and the
	// TUI (dev_docs/plan-v0.1.0.md's "TUI and CLI" section, T13a):
	// internal/clicmd calls the same internal/wizard, internal/discovery,
	// internal/printerstate and internal/camera functions the MCP tools do,
	// through a fresh clicmd.Deps built for this process.
	oneShotCommands.Register(command.Handler{
		Name:        "printers",
		Description: "List, scan for, add, enable/disable and control registered printers",
		Run: func(ctx context.Context, args []string) int {
			return clicmd.RunPrinters(ctx, clicmd.NewDefaultDeps(), args)
		},
	})
	oneShotCommands.Register(command.Handler{
		Name:        "status",
		Description: "Show one printer's derived state, temperatures, job and CFS flag",
		Run: func(ctx context.Context, args []string) int {
			return clicmd.RunStatus(ctx, clicmd.NewDefaultDeps(), args)
		},
	})
	oneShotCommands.Register(command.Handler{
		Name:        "filaments",
		Description: "Show the CFS and side spool filament slots (read-only)",
		Run: func(ctx context.Context, args []string) int {
			return clicmd.RunFilaments(ctx, clicmd.NewDefaultDeps(), args)
		},
	})
	oneShotCommands.Register(command.Handler{
		Name:        "snapshot",
		Description: "Save one still frame from a printer's onboard camera",
		Run: func(ctx context.Context, args []string) int {
			return clicmd.RunSnapshot(ctx, clicmd.NewDefaultDeps(), args)
		},
	})
}

var usageSpecs = []cli.Spec{
	{Name: "mcp", Description: "Run the MCP server (default when not in a terminal)"},
	{Name: "install", Description: "Install and configure AI client integration"},
	{Name: "uninstall", Description: "Remove AI client integration"},
	{Name: "add", Description: "Register the server in this project's AI client configs"},
	{Name: "doctor", Description: "Diagnose the installation"},
	{Name: "update", Description: "Update to the latest release"},
	{Name: "version", Description: "Print the version"},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bare invocation in a terminal opens the app; everywhere else (an AI
	// client spawning us, a pipe) it is the MCP server, which cli.Parse
	// reports as "mcp".
	if opts := updateOptions(); opts.InstallDir != "" {
		update.RemoveStaleBinaries(opts.InstallDir, opts.BinaryName)
	}
	if len(os.Args) == 1 && tui.IsInteractive() {
		os.Exit(runApp(ctx))
	}

	cmd, err := cli.Parse(os.Args[1:])
	if err != nil {
		if errors.Is(err, cli.ErrUsage) {
			printUsage(os.Stdout)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}

	switch cmd.Name {
	case "mcp":
		os.Exit(runMCPServer(ctx, cmd))
	case "install", "configure":
		if cmd.Scope == string(harness.ScopeProject) {
			os.Exit(runAdd(ctx, cmd))
		}
		os.Exit(runInstall(ctx, cmd))
	case "uninstall":
		os.Exit(runUninstall(ctx, cmd))
	case "add":
		os.Exit(runAdd(ctx, cmd))
	case "doctor":
		os.Exit(runDoctor(ctx))
	case "update":
		os.Exit(runUpdate(ctx, cmd))
	case "camera":
		// Hidden: not in usageSpecs or oneShotCommands, so it never appears
		// in --help. `camera serve` runs the background camera/idle-heat
		// daemon (dev_docs/plan-v0.1.0.md T11a); it is what
		// internal/daemon/client's autostart execs, never something a user
		// types by hand. Other camera subcommands (open, record, stop) land
		// in later tasks (T11b/T11c/T13a).
		os.Exit(runCameraCommand(ctx, cmd))
	case "help":
		printUsage(os.Stdout)
	case "version":
		fmt.Println(version)
	default:
		if ok, code := oneShotCommands.Dispatch(ctx, cmd.Name, cmd.Args); ok {
			os.Exit(code)
		}
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd.Name)
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w *os.File) {
	cli.Usage(w, usageSpecs)
	oneShotCommands.PrintUsage(w)
}

// --- Shared helpers ---

// AppState is shared across install wizard steps.
type AppState struct {
	flow.BaseState
	Printers wizard.PrinterState
	Settings wizard.SettingsState
	Harness  installer.HarnessState
	Results  installer.ResultsState
}

func printerState(s *AppState) *wizard.PrinterState    { return &s.Printers }
func settingsState(s *AppState) *wizard.SettingsState  { return &s.Settings }
func harnessState(s *AppState) *installer.HarnessState { return &s.Harness }
func resultsState(s *AppState) *installer.ResultsState { return &s.Results }

// registryDir resolves a harness.Scope to the directory wizard.PrintersStep,
// wizard.SaveSelection and wizard.UnattendedInstall use to pick the project
// registry over the global one (domain.RegistryPath): "" for global scope,
// the project directory for project scope.
func registryDir(scope harness.Scope) string {
	if scope.IsProject() {
		return scope.Dir
	}
	return ""
}

func serverName(cmd cli.Command) string {
	if cmd.ServerName != "" {
		return cmd.ServerName
	}
	return domain.ServerName
}

func newDetector(name string) (*harness.Detector, error) {
	exe, err := harness.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	return harness.New(harness.ServerSpec{
		Name:    name,
		Command: exe,
		Args:    []string{"mcp"},
		Env:     domain.DefaultEnv(),
	})
}

// selectIDs picks the harnesses to act on. --clients wins, then --all, and by
// default every selectable client that is not configured yet. The returned
// reason explains an empty selection.
func selectIDs(harnesses []harness.Harness, cmd cli.Command) (ids []harness.ID, reason string) {
	if len(cmd.Clients) > 0 {
		var unknown []string
		for _, want := range cmd.Clients {
			found := false
			for _, h := range harnesses {
				if strings.EqualFold(string(h.ID), want) && h.Selectable() {
					ids = append(ids, h.ID)
					found = true
				}
			}
			if !found {
				unknown = append(unknown, want)
			}
		}
		if len(unknown) > 0 {
			return nil, "no detected client matches --clients " + strings.Join(unknown, ",")
		}
		return ids, ""
	}
	// Default: installed clients that are not configured yet. --all: every
	// installed or configured client (in project scope that means every
	// client found on this machine gets a project entry).
	candidates := 0
	for _, h := range harnesses {
		if !h.Selectable() || !h.Relevant() {
			continue
		}
		candidates++
		if cmd.All || !h.Configured {
			ids = append(ids, h.ID)
		}
	}
	if len(ids) == 0 && candidates > 0 {
		return nil, "every detected client is already configured (use --all to re-register)"
	}
	return ids, ""
}

func exitCodeFor(results []harness.Result) int {
	for _, r := range results {
		if r.State == harness.ApplyFailed {
			return 1
		}
	}
	return 0
}

func byID(harnesses []harness.Harness) map[harness.ID]harness.Harness {
	m := make(map[harness.ID]harness.Harness, len(harnesses))
	for _, h := range harnesses {
		m[h.ID] = h
	}
	return m
}

// warnUnusedCredentials tells the user that --email/--token, which cli.Parse
// still accepts for install/add/uninstall, do nothing for this server: it has
// no login concept.
func warnUnusedCredentials(cmd cli.Command) {
	if len(cmd.Credentials) > 0 {
		fmt.Println("  --email and --token are not used by this server; ignoring them.")
	}
}

// --- Install / add ---

func runInstall(ctx context.Context, cmd cli.Command) int {
	detector, err := newDetector(serverName(cmd))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if tui.IsInteractive() && !cmd.Yes {
		return runWizard(ctx, detector, harness.Scope{}, cmd, "creality-k2-mcp setup")
	}
	return runUnattended(ctx, detector, harness.Scope{}, cmd, harness.Present)
}

// projectDir resolves --dir (default: the working directory) to an absolute path.
func projectDir(cmd cli.Command) (string, error) {
	dir := cmd.Dir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	return filepath.Abs(dir)
}

func runAdd(ctx context.Context, cmd cli.Command) int {
	dir, err := projectDir(cmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	detector, err := newDetector(serverName(cmd))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	scope := harness.ProjectScopeDir(dir)

	if tui.IsInteractive() && !cmd.Yes {
		return runWizard(ctx, detector, scope, cmd, "creality-k2-mcp project setup")
	}
	return runUnattended(ctx, detector, scope, cmd, harness.Present)
}

// runWizard drives the interactive install: pick clients, register.
func runWizard(ctx context.Context, detector *harness.Detector, scope harness.Scope, cmd cli.Command, title string) int {
	state := &AppState{}
	steps := []flow.Step[AppState]{
		wizard.PrintersStep(ctx, printerState, wizard.PrintersStepOptions{Dir: registryDir(scope), DryRun: cmd.DryRun}),
		wizard.SettingsStep(settingsState, wizard.SettingsStepOptions{DryRun: cmd.DryRun}),
		wizard.HarnessStep(installer.HarnessStep(ctx, detector, harnessState, installer.HarnessStepOptions{AllDetected: true, Scope: scope}), harnessState),
		installer.ApplyStep(ctx, detector, harnessState, resultsState, installer.ApplyStepOptions{Scope: scope, DryRun: cmd.DryRun}),
	}
	f := flow.New(steps, state)
	code := tui.Run(ctx, f, tui.Options{Title: title})
	if state.Failure != nil {
		fmt.Fprintln(os.Stderr, state.Failure)
	}
	return code
}

func runUnattended(ctx context.Context, detector *harness.Detector, scope harness.Scope, cmd cli.Command, desired harness.DesiredState) int {
	warnUnusedCredentials(cmd)
	enabling := desired == harness.Present

	// Unattended install/add: discover printers and merge them with the
	// registry before touching AI client configs (plan-v0.1.0.md decision
	// 4). --dry-run never writes the registry either, matching --dry-run's
	// meaning for the harness registration below.
	if enabling && !cmd.DryRun {
		res, err := wizard.UnattendedInstall(ctx, nil, registryDir(scope))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		wizard.PrintUnattendedResult(os.Stdout, res)
		fmt.Println()
	}

	harnesses := detector.DetectIn(ctx, scope)

	var ids []harness.ID
	if enabling {
		var reason string
		ids, reason = selectIDs(harnesses, cmd)
		if reason != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", reason)
			if len(cmd.Clients) > 0 {
				return 2
			}
			return 0
		}
	} else {
		for _, h := range harnesses {
			if h.Configured {
				ids = append(ids, h.ID)
			}
		}
	}
	if len(ids) == 0 {
		if enabling {
			installer.PrintNoClients(os.Stdout, domain.BinaryName, false)
		} else {
			fmt.Println("  No clients are configured.")
		}
		return 0
	}
	if cmd.DryRun {
		return printPlan(ctx, detector, scope, ids, desired)
	}

	policy := harness.ConflictReplace
	if !enabling {
		policy = harness.ConflictError
	}
	results := detector.ApplyIn(ctx, scope, ids, desired, policy)
	installer.PrintResultsWithScope(os.Stdout, results, scope, enabling, false)
	installer.PrintReloadHints(os.Stdout, results, byID(harnesses))

	return exitCodeFor(results)
}

func printPlan(ctx context.Context, detector *harness.Detector, scope harness.Scope, ids []harness.ID, desired harness.DesiredState) int {
	policy := harness.ConflictReplace
	if desired == harness.Absent {
		policy = harness.ConflictError
	}
	changes, err := detector.PlanResultsIn(ctx, scope, ids, desired, policy)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	installer.PrintChanges(os.Stdout, changes, scope)
	return 0
}

// --- Uninstall ---

func runUninstall(ctx context.Context, cmd cli.Command) int {
	detector, err := newDetector(serverName(cmd))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	scope := harness.Scope{}
	if cmd.Scope == string(harness.ScopeProject) {
		dir, err := projectDir(cmd)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		scope = harness.ProjectScopeDir(dir)
	}
	return runUnattended(ctx, detector, scope, cmd, harness.Absent)
}

// --- Doctor ---

func updateOptions() update.Options {
	opts := update.Options{
		Owner:          domain.Owner,
		Repo:           domain.Repo,
		CurrentVersion: version,
		AssetName:      domain.AssetName,
		BinaryName:     domain.BinaryName,
	}
	if exe, err := harness.ResolveExecutable(); err == nil {
		opts.InstallDir = filepath.Dir(exe)
		opts.BinaryName = filepath.Base(exe)
	}
	return opts
}

func newDoctor(ctx context.Context) *doctor.Runner {
	opts := updateOptions()
	r := doctor.New(
		doctor.ExecutableCheck{},
		doctor.PathCheck{Dir: opts.InstallDir},
		clientsCheck{},
	)
	if version != "dev" {
		r.Add(doctor.UpdateCheck{Opts: opts})
	}
	// This server's own checks (dev_docs/plan-v0.1.0.md T14): registry,
	// settings, every enabled printer's reachability, and the background
	// camera/idle-heat daemon. internal/doctorchecks reads the registry and
	// settings itself; nothing here writes to either. mcpserver.ToolCatalog
	// (review backlog item 28) supplies the full registered tool set so
	// SettingsCheck can flag a tools.overrides name that matches no real
	// tool, without doctorchecks itself importing mcpserver.
	r.Add(doctorchecks.Checks(mcpserver.ToolCatalog())...)
	return r
}

func runDoctor(ctx context.Context) int {
	return newDoctor(ctx).Run(ctx, os.Stdout)
}

// clientsCheck lists the AI clients that have this server registered.
type clientsCheck struct{}

func (clientsCheck) Name() string { return "AI clients" }

func (clientsCheck) Run(ctx context.Context) doctor.Result {
	detector, err := newDetector(domain.ServerName)
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

// --- Update ---

func runUpdate(ctx context.Context, cmd cli.Command) int {
	opts := updateOptions()

	// `update --from <file>` is used by the install script, which has already
	// downloaded and verified the new binary.
	if len(cmd.Args) >= 2 && cmd.Args[0] == "--from" {
		if err := update.SwapFrom(ctx, cmd.Args[1], opts); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("  Updated.")
		return 0
	}

	if version == "dev" {
		fmt.Println("  This is a development build; build from source to update.")
		return 0
	}
	latest, available, err := update.Check(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Update check failed: %v\n", err)
		return 1
	}
	if !available {
		fmt.Printf("  creality-k2-mcp %s is up to date.\n", version)
		return 0
	}
	fmt.Printf("  Updating creality-k2-mcp %s -> %s\n", version, latest)
	if err := update.SelfUpdate(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "  Update failed: %v\n", err)
		return 1
	}
	fmt.Printf("  Updated to %s.\n", latest)
	return 0
}

// --- App ---

// The interactive app opens when the binary is run bare in a terminal
// (dev_docs/plan-v0.1.0.md T13b): Printers, Status, Camera, Recordings,
// Settings, Run doctor, Quit. internal/tui owns every screen; this just
// wires up its dependencies the same way runMCPServer wires up
// mcpserver.Deps - the registry directory (matching loadMCPRegistry's own
// cwd-based resolution), the doctor report (the same doctor.Runner the
// `doctor` command uses) and, when available, the background camera/
// idle-heat daemon client (matching newWatchdogClient's own "nil means not
// wired up" contract: an interface field is only ever assigned a non-nil
// *daemonclient.Client, never a nil one wrapped in a non-nil interface).
func runApp(ctx context.Context) int {
	dir, _ := os.Getwd()
	deps := internaltui.Deps{
		Dir: dir,
		RunDoctor: func(ctx context.Context) string {
			var buf bytes.Buffer
			newDoctor(ctx).Run(ctx, &buf)
			return buf.String()
		},
	}
	if wd := newWatchdogClient(); wd != nil {
		deps.Daemon = wd
	}
	return internaltui.Run(ctx, version, deps)
}

// --- MCP Server ---

func runMCPServer(ctx context.Context, cmd cli.Command) int {
	// This server only ever serves its own MCP tools over the configured
	// transport; it never bridges to another MCP endpoint.
	if cmd.Remote != "" {
		fmt.Fprintf(os.Stderr, "  %s does not bridge to other MCP servers, so `mcp --remote %s` is refused.\n"+
			"  Run `%s mcp` without --remote.\n", domain.BinaryName, cmd.Remote, domain.BinaryName)
		return 2
	}

	transport := os.Getenv("TRANSPORT")
	if transport == "" {
		transport = "stdio"
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	settingsPath, err := domain.SettingsPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	settings, err := domain.LoadSettings(settingsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	deps := mcpserver.Deps{
		Settings:     settings,
		LoadRegistry: loadMCPRegistry,
	}
	// One daemon client instance covers every seam that legitimately
	// autostarts the background daemon: idle-heat watchdog (D2), the T11b
	// camera viewer and T11c recording all talk to the same background
	// daemon and all need it started on first use. Only assign
	// Deps.Watchdog/Deps.CameraViewer/Deps.CameraRecorder when
	// newWatchdogClient actually returns a client: assigning a nil
	// *daemonclient.Client here would still leave each interface non-nil (a
	// nil pointer with a type is not a nil interface), breaking
	// internal/policy's "nil means the daemon is not wired up" contract and
	// open_camera_view/the recording tools' own nil checks.
	if wd := newWatchdogClient(); wd != nil {
		deps.Watchdog = wd
		deps.CameraViewer = wd
		deps.CameraRecorder = wd
		// Snapshots are captured through the same background daemon
		// instance (review backlog item 51: the daemon hub's rolling GOP
		// buffer, not a direct, independent WebRTC session per call), so
		// they share this one client the same way CameraViewer/
		// CameraRecorder already do.
		deps.CameraSnapshot = wd
	}
	// get_printer_status's watchdog status path must never start the daemon
	// as a side effect of a read-only status call (dev_docs/review-backlog.md
	// item 29): it gets its own client built with client.WithoutAutostart,
	// separate from wd above, so this stays true even if Status ever grew an
	// autostart fallback of its own.
	if wdStatus := newWatchdogStatusClient(); wdStatus != nil {
		deps.WatchdogStatus = wdStatus
		// The same non-autostarting client also answers camera.status
		// (review backlog item 47): CameraStatus never autostarts the
		// daemon either, so reusing this instance keeps that guarantee by
		// construction rather than needing a third client.
		deps.CameraStatus = wdStatus
	}

	srv := mcpserver.New(mcpserver.Config{
		Version:   version,
		Transport: transport,
		HTTPAddr:  addr,
	}, deps)
	runErr := srv.Run(ctx)
	if runErr == nil || errors.Is(runErr, context.Canceled) || isClientHangup(runErr) {
		return 0
	}
	fmt.Fprintln(os.Stderr, runErr)
	return 1
}

// newWatchdogClient builds the policy.Watchdog the MCP server uses for D2's
// idle-heat feature: internal/daemon/client.Client, which autostarts the
// background daemon (`camera serve`) on first need. A nil return (only when
// this process's paths cannot be resolved, e.g. no home directory) leaves
// mcpserver.Deps.Watchdog nil, which internal/policy already treats exactly
// like "the daemon is not alive" (idle heating refused with a clear hint) -
// never a reason to fail the whole server.
func newWatchdogClient() *daemonclient.Client {
	c, err := daemonclient.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: idle-heat watchdog unavailable: %v\n", err)
		return nil
	}
	return c
}

// newWatchdogStatusClient builds the internal/daemon/client.Client the MCP
// server uses only for get_printer_status's watchdog status path
// (Deps.WatchdogStatus): the same production client type as
// newWatchdogClient, but built with client.WithoutAutostart so this
// read-only status probe can never start the background daemon
// (dev_docs/review-backlog.md item 29). A nil return (only when this
// process's paths cannot be resolved) leaves Deps.WatchdogStatus nil, which
// get_printer_status already reports as "unknown".
func newWatchdogStatusClient() *daemonclient.Client {
	c, err := daemonclient.New(daemonclient.WithoutAutostart())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: watchdog status unavailable: %v\n", err)
		return nil
	}
	return c
}

// --- Camera daemon (hidden `camera serve`) ---

// runCameraCommand dispatches the hidden `camera` command's subcommands:
// `serve` runs the background daemon in the foreground (autostart execs
// exactly this, never anything below); every other subcommand (open, record,
// stop, recordings, delete, T13a) is a user-facing one-shot command and is
// handed to internal/clicmd, which shares its daemon-client seams with the
// MCP tools (tools_viewer.go, tools_recording.go).
func runCameraCommand(ctx context.Context, cmd cli.Command) int {
	if len(cmd.Args) > 0 && cmd.Args[0] == "serve" {
		if len(cmd.Args) != 1 {
			fmt.Fprintln(os.Stderr, "Usage: creality-k2-mcp camera serve")
			return 2
		}
		return runCameraServe(ctx)
	}
	return clicmd.RunCamera(ctx, clicmd.NewDefaultDeps(), cmd.Args)
}

// runCameraServe runs the background camera/idle-heat daemon in the
// foreground until ctx is cancelled (Ctrl+C, SIGTERM, or the daemon's own
// idle self-exit), which is exactly how internal/daemon/lock.EnsureRunning
// (via internal/daemon/client's autostart) launches it: detached, with
// stdout/stderr redirected to its own log file.
func runCameraServe(ctx context.Context) int {
	paths, err := daemon.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	srv, err := daemon.Open(daemon.NewProductionOptions(paths))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer srv.Close()

	if err := srv.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// loadMCPRegistry is mcpserver.RegistryLoader for a real run: the
// K2_MCP_HOST environment override (domain.EnvOverride), when set, bypasses
// the registry file entirely and yields its single ad hoc printer; otherwise
// this loads the project registry (current working directory) or, absent
// one, the global registry (domain.LoadRegistry). Warnings about dropped or
// deduplicated entries are logged to stderr rather than failing the server,
// matching LoadRegistryFile's own "never lock the user out" behaviour.
func loadMCPRegistry() (domain.Registry, error) {
	if p, ok, err := domain.EnvOverride(); err != nil {
		return domain.Registry{}, err
	} else if ok {
		return domain.Registry{Version: 1, Printers: []domain.Printer{p}}, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return domain.Registry{}, err
	}
	reg, _, warnings, err := domain.LoadRegistry(cwd)
	if err != nil {
		return domain.Registry{}, err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	return reg, nil
}

// isClientHangup reports whether the server stopped because the client closed
// the transport (stdin EOF for stdio), which is how MCP sessions normally end.
// The SDK formats this as "server is closing: EOF" without wrapping io.EOF, so
// the check is textual.
func isClientHangup(err error) bool {
	return strings.HasSuffix(err.Error(), io.EOF.Error())
}
