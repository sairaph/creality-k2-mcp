package mcpserver

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
	"github.com/sairaph/mcp-wizard/budget"
	"github.com/sairaph/mcp-wizard/render"
)

// StateFront is implemented by every tool-specific frontmatter struct that
// embeds printerstate.StateBlock (dev_docs/safety-architecture.md section 5:
// "every tool result" carries it). A tool's front struct embeds the block
// with `yaml:",inline"` and adds this trivial method:
//
//	type myFront struct {
//	    printerstate.StateBlock `yaml:",inline"`
//	    SomeField string `yaml:"some_field,omitempty"`
//	}
//	func (f *myFront) StateBlockPtr() *printerstate.StateBlock { return &f.StateBlock }
//
// successResult uses the method to attach the actions list (when the caller
// has one) without every tool handler repeating that assignment.
type StateFront interface {
	StateBlockPtr() *printerstate.StateBlock
}

// successResult renders front (whose embedded printerstate.StateBlock is
// filled in by the caller already) plus body into a tool result. actions,
// when non-nil, is attached to the embedded StateBlock before rendering
// (internal/policy's output, once a tool group wires it in); a nil actions
// leaves whatever the caller already set on the block (normally empty).
func successResult(front StateFront, actions []printerstate.ActionGate, body string) *mcp.CallToolResult {
	if actions != nil {
		front.StateBlockPtr().Actions = actions
	}
	return render.SuccessResult(front, body)
}

// imageResult renders front plus body as the first content item, and data
// (already the raw image bytes, not base64) as an image content item after
// it, mirroring freecad-mcp's screenshot replies. It refuses to build a
// reply whose image would push the whole result over render.MaxBytes once
// base64-encoded (maxImageBytes, reply.go), returning an error result
// instead of a reply that render.Document.String would itself reject.
func imageResult(front StateFront, actions []printerstate.ActionGate, body, mimeType string, data []byte) *mcp.CallToolResult {
	if len(data) > maxImageBytes {
		return render.ErrorResult(render.Error{
			Code: render.CodeInternal,
			Message: fmt.Sprintf("the image is %d bytes, over the %d byte limit a single reply can carry",
				len(data), maxImageBytes),
			Hint: "This is an unexpectedly large image for this printer; report it as a bug.",
		})
	}
	if actions != nil {
		front.StateBlockPtr().Actions = actions
	}
	doc := render.Document{Front: front, Body: body}
	text, err := doc.String()
	if err != nil {
		return render.ErrorResult(render.Error{Code: render.CodeInternal, Message: "could not render result: " + err.Error()})
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
			&mcp.ImageContent{Data: data, MIMEType: mimeType},
		},
	}
}

// paginateBudget bounds the rendered token size of one page, the same
// pattern 05-mcp-wizard-integration.md section 1 shows for budget.Paginate:
// generous enough that a page holds many records, small enough that several
// pages plus their shared frontmatter stay well under render.MaxBytes.
const paginateBudget = 4000

// paginatePage runs budget.Paginate over records at page (1-indexed,
// matching render.PageMeta), rendering each candidate window with renderRows
// (the table/list body for just that window, no frontmatter). It returns the
// window for page, the PageMeta for the frontmatter, and the "Next: page=N."
// suffix (render.NextPageHint) to append to the body; page out of range
// yields a nil window and PageMeta describing the valid range, not an error.
func paginatePage[T any](records []T, page int, renderRows func([]T) (string, error)) (window []T, meta render.PageMeta, nextHint string, err error) {
	if page < 1 {
		page = 1
	}
	window, totalPages, err := budget.Paginate(records, page, paginateBudget, renderRows)
	if err != nil {
		return nil, render.PageMeta{}, "", err
	}
	meta = render.PageMeta{Page: page, Total: len(records), TotalPages: totalPages}
	return window, meta, render.NextPageHint(meta), nil
}
