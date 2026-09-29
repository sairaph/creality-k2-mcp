package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/mcp-wizard/render"
)

// This file implements open_camera_view (dev_docs/plan-v0.1.0.md's tool
// surface, decision 11, T11b): a read-only tool that hands back the local
// browser URL for the camera viewer page internal/daemon's background
// daemon serves (see internal/daemon/viewer*.go), ensuring that daemon is
// running by autostarting it through Deps.CameraViewer (the production
// internal/daemon/client.Client). Like list_printers/discover_printers
// (tools_registry.go), this does not always resolve to a single printer the
// way most tools do (resolvePrinter/StateFront): with no `printer` argument
// the result legitimately covers every enabled printer at once, so it
// builds its own frontmatter and calls render.SuccessResult directly.

func registerViewerTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "open_camera_view", Category: domain.ToolCategoryCamera}, &mcp.Tool{
		Name: "open_camera_view",
		Description: "Returns a local browser URL for a continuously updating live view of a printer's onboard " +
			"chamber camera, served by this server's own background daemon over Media Source Extensions (MSE): " +
			"open the URL in any browser on this computer and it keeps updating in real time until the page or " +
			"the daemon closes. The URL is local to this computer only (127.0.0.1, never reachable from another " +
			"device or over the network) and carries a random access token generated for this daemon session, so " +
			"it should not be shared and stops working once the daemon exits. Pass `printer` to preselect one " +
			"printer's stream; omit it to open a page listing every enabled printer, each with its own video. " +
			"Opening this view never opens a second camera connection: this server shares one WebRTC connection " +
			"to a printer's camera across every browser viewer, recording and snapshot in use at the same time, " +
			"so opening the page adds no extra load on the printer beyond the first viewer. This tool never " +
			"captures or returns image data itself; call get_camera_snapshot for a single still frame instead. " +
			"Camera use is read-only and works in every printer state except offline.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, openCameraViewHandler(s))
}

type openCameraViewInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; omit to show every enabled printer instead of one"`
}

type openCameraViewPrinter struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

type openCameraViewFront struct {
	URL      string                  `yaml:"url"`
	Printers []openCameraViewPrinter `yaml:"printers"`
}

// cameraViewerUnavailableHint is used when Deps.CameraViewer.ViewerURL
// itself fails (the daemon could not be started or reached), as distinct
// from Deps.CameraViewer being nil entirely (a separate, more specific
// message below).
const cameraViewerUnavailableHint = "This server's background daemon could not be started or reached; " +
	"make sure this process can launch its own executable, then try again."

func openCameraViewHandler(s *Server) func(context.Context, *mcp.CallToolRequest, openCameraViewInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in openCameraViewInput) (*mcp.CallToolResult, any, error) {
		reg, err := s.deps.LoadRegistry()
		if err != nil {
			return failure("load the printer registry", err, ""), nil, nil
		}

		var (
			printerID string
			shown     []domain.Printer
		)
		if in.Printer != nil && strings.TrimSpace(*in.Printer) != "" {
			p, err := domain.ResolvePrinter(reg.Printers, *in.Printer)
			if err != nil {
				return failure("resolve the printer", err, printerListHint(reg)), nil, nil
			}
			printerID = p.ID
			shown = []domain.Printer{*p}
		} else {
			shown = reg.Enabled()
		}

		if len(shown) == 0 {
			return render.ErrorResult(render.Error{
				Code:    render.CodeNotFound,
				Message: "No printer is enabled.",
				Hint:    "Call list_printers to see the registry, or register one first.",
			}), nil, nil
		}

		if s.deps.CameraViewer == nil {
			return render.ErrorResult(render.Error{
				Code: render.CodeUnavailable,
				Message: "The camera viewer is not available: this server's background daemon client is not " +
					"wired up.",
				Hint: "This usually means the server's own executable path could not be resolved at startup; " +
					"restarting the server should fix it.",
			}), nil, nil
		}

		url, err := s.deps.CameraViewer.ViewerURL(ctx, printerID)
		if err != nil {
			// Always unavailable, never internal_error: a ViewerURL failure
			// only ever means the background daemon could not be reached or
			// started (autostart already retried once, client.go's own
			// ViewerURL doc comment), not a bug in this call. failure()'s
			// generic classification would otherwise fall through to
			// internal_error for a plain error, so this is mapped directly
			// rather than through failure().
			return render.ErrorResult(render.Error{
				Code:    render.CodeUnavailable,
				Message: shortMessage(fmt.Sprintf("Failed to start the camera viewer: %v", err)),
				Hint:    cameraViewerUnavailableHint,
			}), nil, nil
		}

		printers := make([]openCameraViewPrinter, len(shown))
		for i, p := range shown {
			printers[i] = openCameraViewPrinter{ID: p.ID, Name: p.Name}
		}

		front := &openCameraViewFront{URL: url, Printers: printers}
		return render.SuccessResult(front, openCameraViewBody(url, shown)), nil, nil
	}
}

// openCameraViewBody tells the AI what to do with the URL: open it in a
// browser, that it is local to this computer only, and that one camera
// connection is shared by every viewer.
func openCameraViewBody(url string, printers []domain.Printer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Open %s in a browser on this computer for a continuously updating live view", url)
	if len(printers) == 1 {
		fmt.Fprintf(&b, " of %s's onboard chamber camera.", printers[0].Name)
	} else {
		names := make([]string, len(printers))
		for i, p := range printers {
			names[i] = p.Name
		}
		fmt.Fprintf(&b, " of %d printers: %s.", len(printers), strings.Join(names, ", "))
	}
	b.WriteString(" This URL is local to this computer only: it will not work from another device or over the " +
		"network, and it stops working once this server's background daemon exits. Opening it does not open a " +
		"second camera connection; one camera connection per printer is shared by every browser viewer, " +
		"recording and snapshot in use at the same time.")
	b.WriteString(" If the page does not show video right away, this is usually not a connection problem: " +
		noVideoMessage + "; the page keeps waiting and starts showing video on its own once a complete " +
		"keyframe arrives, with no need to reopen it.")
	return b.String()
}
