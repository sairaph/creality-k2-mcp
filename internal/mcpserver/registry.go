package mcpserver

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// registerTool registers tool with handler h if settings.EnabledTools
// enables info.Name (preset plus per-tool overrides,
// dev_docs/safety-architecture.md D6), and enforces the MCP annotation
// contract every tool in this project must carry (D3):
//
//   - tool.Annotations must already be set by the caller (readOnlyHint and,
//     for a non-read-only tool, destructiveHint and idempotentHint) -
//     registerTool panics if it is missing, so a tool group cannot ship
//     without annotations by omission.
//   - openWorldHint is always forced to false here, regardless of what the
//     caller set: every tool this server exposes only ever talks to
//     printers already in the registry, never an open set of external
//     endpoints (D3).
//
// A disabled tool is simply never registered, so it is absent from
// tools/list, not merely rejected when called.
func registerTool[In, Out any](s *Server, info domain.ToolInfo, tool *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if tool.Annotations == nil {
		panic(fmt.Sprintf("registerTool: %s has no MCP annotations", info.Name))
	}
	if !tool.Annotations.ReadOnlyHint && tool.Annotations.DestructiveHint == nil {
		panic(fmt.Sprintf("registerTool: %s is not read-only but has no destructiveHint", info.Name))
	}
	closedWorld := false
	tool.Annotations.OpenWorldHint = &closedWorld

	if tool.Name == "" {
		tool.Name = info.Name
	}
	if tool.Name != info.Name {
		panic(fmt.Sprintf("registerTool: tool.Name %q does not match info.Name %q", tool.Name, info.Name))
	}

	// Recorded regardless of whether this tool ends up enabled, so
	// warnUnknownToolOverrides (server.go) can check every registered tool's
	// name against config.toml's overrides after every register*Tools() call
	// has run, not just the ones the current preset happens to enable.
	s.allToolInfos = append(s.allToolInfos, info)

	if !s.deps.Settings.EnabledTools([]domain.ToolInfo{info})[info.Name] {
		return
	}
	mcp.AddTool(s.mcpServer, tool, h)
}

// boolPtr is a small convenience for building *bool annotation fields
// (destructiveHint, openWorldHint) inline in a tool's registration call.
func boolPtr(b bool) *bool { return &b }
