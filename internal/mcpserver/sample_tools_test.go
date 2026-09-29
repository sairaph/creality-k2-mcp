package mcpserver

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/render"
)

// This file defines a small set of test-only tools that exercise every
// shared helper in reply.go, result.go, registry.go and printer.go end to
// end through the real MCP wire protocol (mcp.NewInMemoryTransports in
// server_test.go), instead of unit-testing those helpers in isolation.
// registerSampleTools is only ever passed to newServer from a test; New
// (the production constructor) never calls it, so none of this ships.

// sampleFront is the frontmatter every sample tool returns: just the shared
// printerstate.StateBlock, embedded per StateFront's contract (result.go).
type sampleFront struct {
	printerstate.StateBlock `yaml:",inline"`
}

func (f *sampleFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

// sampleListFront additionally carries render.PageMeta, the canonical shape
// for a paginated tool's frontmatter (05-mcp-wizard-integration.md section 1).
type sampleListFront struct {
	printerstate.StateBlock `yaml:",inline"`
	render.PageMeta         `yaml:",inline"`
}

func (f *sampleListFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }

func idleFront(printerID string) printerstate.StateBlock {
	return printerstate.StateBlock{
		PrinterID:     printerID,
		PrinterName:   printerID,
		SnapshotTime:  time.Now().UTC().Format(time.RFC3339),
		ActivityState: printerstate.StateIdle,
		Bucket:        string(printerstate.BucketI),
		GatingClass:   string(printerstate.ClassSafeToAct),
	}
}

func registerSampleTools(s *Server) {
	registerTool(s, domain.ToolInfo{Name: "sample_state", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "sample_state",
		Description: "Test-only tool: resolves a printer, runs a real printerstate.Take against it, and returns " +
			"successResult with the derived StateBlock and a sample actions list, exercising the shared " +
			"success-result helper end to end. Never registered outside this package's own tests.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, sampleStateHandler(s))

	registerTool(s, domain.ToolInfo{Name: "sample_probe", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "sample_probe",
		Description: "Test-only tool: resolves a printer and calls Moonraker's server/info directly, exercising " +
			"resolvePrinter and failure()'s error classification (not_found, ambiguous, authentication, " +
			"unavailable) against a fake Moonraker. Never registered outside this package's own tests.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, sampleProbeHandler(s))

	registerTool(s, domain.ToolInfo{Name: "sample_image", Category: domain.ToolCategoryCamera}, &mcp.Tool{
		Name: "sample_image",
		Description: "Test-only tool exercising imageResult, including its size cap: too_big=true returns an " +
			"oversized image that must be refused with an internal_error rather than rendered. Never registered " +
			"outside this package's own tests.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, sampleImageHandler(s))

	registerTool(s, domain.ToolInfo{Name: "sample_list", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "sample_list",
		Description: "Test-only tool exercising paginatePage over 26 synthetic records, with render.PageMeta in " +
			"the frontmatter and render.NextPageHint in the body. Never registered outside this package's own tests.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, sampleListHandler(s))

	registerTool(s, domain.ToolInfo{Name: "sample_control", Category: domain.ToolCategoryControl}, &mcp.Tool{
		Name: "sample_control",
		Description: "Test-only, non-read-only tool that performs no write: it exists only to exercise preset " +
			"filtering for the control category and the destructiveHint annotation requirement for a " +
			"non-read-only tool. Never registered outside this package's own tests.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: false},
	}, sampleControlHandler(s))

	registerTool(s, domain.ToolInfo{Name: "sample_boom", Category: domain.ToolCategoryMonitor}, &mcp.Tool{
		Name: "sample_boom",
		Description: "Test-only tool that always fails with a plain Go error, exercising failure()'s generic " +
			"internal_error classification. Never registered outside this package's own tests.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, sampleBoomHandler(s))
}

type sampleStateInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
}

func sampleStateHandler(s *Server) func(context.Context, *mcp.CallToolRequest, sampleStateInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in sampleStateInput) (*mcp.CallToolResult, any, error) {
		query := ""
		if in.Printer != nil {
			query = *in.Printer
		}
		printer, errRes := s.resolvePrinter(query)
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		snap := printerstate.Take(ctx, deps, printer)
		derived := printerstate.DeriveActivityState(snap, nil)
		block := printerstate.BuildStateBlock(snap, derived, nil)
		front := &sampleFront{StateBlock: block}
		actions := []printerstate.ActionGate{{Name: "sample_action", Status: "available"}}
		return successResult(front, actions, "Sample state result exercising successResult with an actions list."), nil, nil
	}
}

type sampleProbeInput struct {
	Printer *string `json:"printer,omitempty" jsonschema:"printer id or name, case insensitive; defaults to the one enabled printer"`
}

func sampleProbeHandler(s *Server) func(context.Context, *mcp.CallToolRequest, sampleProbeInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in sampleProbeInput) (*mcp.CallToolResult, any, error) {
		query := ""
		if in.Printer != nil {
			query = *in.Printer
		}
		printer, errRes := s.resolvePrinter(query)
		if errRes != nil {
			return errRes, nil, nil
		}
		deps := s.deps.PrinterClients(printer)
		info, err := deps.Moonraker.ServerInfo(ctx)
		if err != nil {
			return failure("get server info", err, ""), nil, nil
		}
		front := &sampleFront{StateBlock: idleFront(printer.ID)}
		body := fmt.Sprintf("Probed %s: klippy_connected=%v.", printer.ID, info.KlippyConnected)
		return successResult(front, nil, body), nil, nil
	}
}

type sampleImageInput struct {
	TooBig bool `json:"too_big,omitempty" jsonschema:"if true, exercise the oversized-image error path instead of returning a real image"`
}

var samplePNGBytes = []byte("\x89PNG\r\n\x1a\nfake-sample-image-bytes")

func sampleImageHandler(s *Server) func(context.Context, *mcp.CallToolRequest, sampleImageInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in sampleImageInput) (*mcp.CallToolResult, any, error) {
		front := &sampleFront{StateBlock: idleFront("sample")}
		data := samplePNGBytes
		if in.TooBig {
			data = make([]byte, maxImageBytes+1)
		}
		return imageResult(front, nil, "Sample image result.", "image/png", data), nil, nil
	}
}

type sampleListInput struct {
	Page *int `json:"page,omitempty" jsonschema:"1-indexed page number; defaults to 1"`
}

func sampleListHandler(s *Server) func(context.Context, *mcp.CallToolRequest, sampleListInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in sampleListInput) (*mcp.CallToolResult, any, error) {
		page := 1
		if in.Page != nil {
			page = *in.Page
		}
		// Padded with high-entropy random content (not a repeated character,
		// which a BPE tokenizer compresses to very few tokens) so 60 records
		// span several pages at paginatePage's token budget, exercising real
		// pagination rather than always fitting on one page.
		seed := rand.New(rand.NewSource(42))
		records := make([]string, 60)
		for i := range records {
			buf := make([]byte, 300)
			seed.Read(buf)
			records[i] = fmt.Sprintf("record-%02d: %s", i+1, hex.EncodeToString(buf))
		}
		window, meta, nextHint, err := paginatePage(records, page, func(w []string) (string, error) {
			return strings.Join(w, "\n"), nil
		})
		if err != nil {
			return failure("paginate sample records", err, ""), nil, nil
		}
		front := &sampleListFront{StateBlock: idleFront("sample"), PageMeta: meta}
		body := strings.Join(window, "\n") + nextHint
		return successResult(front, nil, body), nil, nil
	}
}

type sampleControlInput struct {
	Confirm string `json:"confirm,omitempty" jsonschema:"unused; present only to exercise a non-read-only tool's schema"`
}

func sampleControlHandler(s *Server) func(context.Context, *mcp.CallToolRequest, sampleControlInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in sampleControlInput) (*mcp.CallToolResult, any, error) {
		front := &sampleFront{StateBlock: idleFront("sample")}
		return successResult(front, nil, "Sample control result (test-only; performs no write)."), nil, nil
	}
}

type sampleBoomInput struct{}

func sampleBoomHandler(s *Server) func(context.Context, *mcp.CallToolRequest, sampleBoomInput) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in sampleBoomInput) (*mcp.CallToolResult, any, error) {
		return failure("run sample_boom", errors.New("synthetic failure for testing"), ""), nil, nil
	}
}
