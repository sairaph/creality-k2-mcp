// Package mcpserver provides the MCP server setup and tool registration.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/policy"
	"github.com/sairaph/creality-k2-mcp/internal/printerclient"
)

// Server wraps the MCP server and the dependencies its tool handlers need.
type Server struct {
	mcpServer *mcp.Server
	config    Config
	deps      Deps

	// allToolInfos accumulates every tool registerTool was ever asked to
	// register, whether or not the current preset/overrides enabled it (see
	// registry.go). newServer uses this once, after every register*Tools()
	// call has run, to warn about config.toml override names that match no
	// registered tool (review backlog item 7).
	allToolInfos []domain.ToolInfo
}

// New creates a new MCP server with the given config and dependencies, and
// registers its tools (only those Deps.Settings.EnabledTools enables). It
// panics if deps.LoadRegistry is nil: every tool needs it to resolve a
// printer.
func New(config Config, deps Deps) *Server {
	return newServer(config, deps)
}

// newServer is New's implementation, taking optional extra registration
// funcs run after every real tool group. Tests use this (never New
// directly) to register the helper-exercising sample tool defined in
// server_test.go, so it never ships in a production build.
func newServer(config Config, deps Deps, extra ...func(*Server)) *Server {
	if deps.LoadRegistry == nil {
		panic("mcpserver.New: deps.LoadRegistry must not be nil")
	}
	if deps.PrinterClients == nil {
		deps.PrinterClients = printerclient.Default
	}
	if deps.PolicyEngine == nil {
		deps.PolicyEngine = policy.New()
	}
	if deps.Policy == nil {
		deps.Policy = policyAvailableAdapter{settings: deps.Settings}
	}

	srv := &Server{
		config: config,
		deps:   deps,
		mcpServer: mcp.NewServer(
			&mcp.Implementation{
				Name:    "creality-k2-mcp",
				Version: config.Version,
			},
			&mcp.ServerOptions{
				Capabilities: &mcp.ServerCapabilities{},
				Instructions: "Creality K2 3D printer control through the Model Context Protocol. " +
					"Call list_printers first to see which printers are registered and enabled. " +
					"Every tool result carries the printer's current state in its frontmatter, " +
					"including an actions list showing which writes are currently available.",
			},
		),
	}

	// Tool groups are registered here, one register*Tools() call per group
	// (see internal/mcpserver/tools_*.go), each calling registerTool
	// (registry.go) per tool so Deps.Settings decides what actually gets
	// exposed.
	registerRegistryTools(srv)
	registerStatusTools(srv)
	registerCameraTools(srv)
	registerViewerTools(srv)
	registerControlTools(srv)
	registerFilamentTools(srv)
	registerFilesTools(srv)
	registerRecordingTools(srv)

	for _, register := range extra {
		register(srv)
	}

	warnUnknownToolOverrides(srv.deps.Settings, srv.allToolInfos)

	srv.mcpServer.AddReceivingMiddleware(invalidArguments)

	return srv
}

// warnUnknownToolOverrides prints a stderr warning naming every
// tools.overrides key in config.toml that matches no registered tool
// (review backlog item 7). It never fails startup and never changes what
// gets registered: an unknown override is simply a name EnabledTools always
// ignores (registry.go), so this only helps the user notice a typo. The
// comparison set is every tool registerTool was ever asked to register
// (allToolInfos), not just the ones the current preset enabled, so an
// override that names a real tool disabled by the preset is never
// misreported as unknown.
func warnUnknownToolOverrides(settings domain.Settings, tools []domain.ToolInfo) {
	unknown := settings.UnknownToolOverrides(tools)
	if len(unknown) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "creality-k2-mcp: warning: config.toml tools.overrides names unknown tool(s): %s\n", strings.Join(unknown, ", "))
}

// MCPServer exposes the underlying server, for tests.
func (s *Server) MCPServer() *mcp.Server { return s.mcpServer }

// ToolCatalog returns every tool this server can register, the same
// domain.ToolInfo values registerTool records regardless of the current tool
// preset or per-tool overrides (review backlog item 28: a single source of
// truth for the full tool set, instead of a separately maintained list that
// can drift from actual registration). It builds a throwaway Server purely
// to run every register*Tools() call and collect the ToolInfo each one
// records: deps.LoadRegistry is only ever invoked at tool-call time, never
// during registration, so the stub below is never called and no printer,
// registry file, camera or daemon connection is ever made. main.go's
// newDoctor passes this into doctorchecks.SettingsCheck.KnownTools so an
// unrecognised tools.overrides name in config.toml is flagged.
func ToolCatalog() []domain.ToolInfo {
	srv := newServer(Config{}, Deps{
		LoadRegistry: func() (domain.Registry, error) {
			return domain.Registry{}, fmt.Errorf("printerclient catalog: LoadRegistry must not be called during registration")
		},
	})
	return srv.allToolInfos
}

// Run starts the server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	switch s.config.Transport {
	case "http":
		return s.runHTTP(ctx)
	default:
		return s.runStdio(ctx)
	}
}

func (s *Server) runStdio(ctx context.Context) error {
	return s.mcpServer.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) runHTTP(ctx context.Context) error {
	addr := s.config.HTTPAddr
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			return s.mcpServer
		},
		&mcp.StreamableHTTPOptions{},
	)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "error shutting down HTTP server: %v\n", err)
		}
	}()

	fmt.Fprintf(os.Stderr, "creality-k2-mcp listening on %s (Streamable HTTP)\n", addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve http: %w", err)
	}
	return nil
}
