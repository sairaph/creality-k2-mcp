package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sairaph/creality-k2-mcp/internal/camera"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
	"github.com/sairaph/mcp-wizard/render"
)

// maxMessageBytes bounds an error message: the message says what failed,
// any explanatory dump belongs in the body instead.
const maxMessageBytes = 2 << 10

// replyMargin is room kept in a reply for its YAML frontmatter and hint
// text, so an image or a large body still fits under render.MaxBytes.
const replyMargin = 32 << 10

// maxImageBytes is the largest image a reply without other large content can
// carry: images travel base64-encoded, 4 bytes for every 3
// (freecad-mcp/internal/mcpserver/reply.go's own maxImageBytes, same
// reasoning).
const maxImageBytes = (render.MaxBytes - replyMargin) / 4 * 3

// Hints for the error classes failure reports.
const (
	unavailableHint = "Make sure the printer is powered on and reachable on the LAN. " +
		"Call list_printers to see reachability for every registered printer, or run `" +
		domain.BinaryName + " doctor`."
	authHint = "Moonraker rejected the request with an authentication error. " +
		"Check the printer's api_key in the registry (list_printers shows which printers have one configured) " +
		"and update it if it has changed on the printer."
	cancelledHint      = "The call was cancelled before the printer answered; send it again to retry."
	invalidPrinterHint = "Call list_printers to see the printers this server knows about."
)

// noVideoMessage and noVideoHint are used whenever a camera call (snapshot,
// recording, or the viewer stream) times out waiting for the printer's
// camera to produce a complete keyframe with no other explanation (review
// backlog item 47). The field cause found in v0.3.1 (a keyframe larger than
// the receive window, with the chamber light on) is fixed in internal/camera;
// any remaining cause is not known, so the text claims none
// (dev_docs/field-camera-transport-analysis.md).
const noVideoHint = "Call get_printer_status to confirm the printer is reachable, then try again."

// noVideoMessage states that no complete keyframe arrived, within wait when it
// is known.
func noVideoMessage(wait time.Duration) string {
	return (&camera.NoKeyframeError{Wait: wait}).Error() + "; " + camera.NoKeyframeCause
}

// isNoVideoTimeout reports whether err is (or wraps) a context deadline
// exceeded, the shape internal/camera.Snapshot's waitForKeyframe/
// waitForNextAccessUnit return when the camera's own budget elapses with no
// keyframe ever arriving (review backlog item 47): the common, expected
// case of "the printer's camera simply is not producing video right now",
// as opposed to a connection failure, a decode error, or something else
// unexpected worth its own distinct message.
func isNoVideoTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// toolError is an error that already carries its final render.Error shape.
// A lower layer this package will grow (a future internal/policy adapter, a
// registry-loading helper) can return one to bypass failure()'s generic
// classification, the same pattern freecad-mcp uses
// (internal/mcpserver/reply.go's toolError).
type toolError struct{ e render.Error }

func (t *toolError) Error() string { return t.e.Message }

// policyError is satisfied by an error internal/policy returns that already
// carries its final render.Error shape (its code, message and hint),
// mirroring toolError. internal/policy is being built separately and
// concurrently with this task; failure() unwraps either the same way, so
// wiring the real package in later needs no change here.
type policyError interface {
	error
	RenderError() render.Error
}

// failure builds an error result for err, which occurred while doing what.
// hint, when not empty, replaces the hint of err's class. It classifies:
//
//   - toolError / policyError: already-final render.Error, returned as is.
//   - *domain.ResolveError: not_found or ambiguous, per its own Code.
//   - *moonraker.Error: Code already matches a render code string exactly
//     (internal/moonraker/errors.go's Code constants share their spelling
//     with render.Code* by design), so it is used directly; a 401
//     (CodeAuthentication) gets authHint unless the caller supplied one.
//   - context.Canceled / a timeout (context.DeadlineExceeded or a net.Error
//     reporting Timeout()): unavailable, since the printer simply did not
//     answer in time.
//   - a plain net.Error (connection refused/reset, never a timeout: that
//     already matched above): unavailable, the printer stopped answering.
//   - anything else: internal_error.
func failure(what string, err error, hint string) *mcp.CallToolResult {
	var te *toolError
	if errors.As(err, &te) {
		return render.ErrorResult(te.e)
	}
	var pe policyError
	if errors.As(err, &pe) {
		return render.ErrorResult(pe.RenderError())
	}

	e := render.Error{
		Code:    render.CodeInternal,
		Message: shortMessage(fmt.Sprintf("Failed to %s: %v", what, err)),
	}

	var (
		resolveErr *domain.ResolveError
		moonErr    *moonraker.Error
		netErr     net.Error
	)
	switch {
	case errors.As(err, &resolveErr):
		e.Code = string(resolveErr.Code)
		e.Hint = invalidPrinterHint
	case errors.As(err, &moonErr):
		e.Code = string(moonErr.Code)
		if moonErr.Code == moonraker.CodeAuthentication {
			e.Hint = authHint
		} else if moonErr.Code == moonraker.CodeUnavailable {
			e.Hint = unavailableHint
		}
	case errors.Is(err, context.Canceled):
		e.Code = render.CodeUnavailable
		e.Message = shortMessage(fmt.Sprintf("Failed to %s: the request was cancelled", what))
		e.Hint = cancelledHint
	case isTimeout(err):
		e.Code = render.CodeUnavailable
		e.Hint = unavailableHint
	case errors.As(err, &netErr):
		e.Code = render.CodeUnavailable
		e.Hint = unavailableHint
	}

	if hint != "" {
		e.Hint = hint
	}
	return render.ErrorResult(e)
}

// isTimeout reports whether err is a reply that did not arrive in time.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout()
}

// shortMessage keeps the start of an error message, which says what failed,
// within maxMessageBytes.
func shortMessage(s string) string {
	if len(s) <= maxMessageBytes {
		return s
	}
	cut := maxMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " ... (message truncated)"
}

// jsonBlock renders v as indented JSON in a fence.
func jsonBlock(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		data = []byte(fmt.Sprint(v))
	}
	return render.Fence(string(data), "json")
}

// textBlock renders free text in a fence.
func textBlock(s string) string {
	return render.Fence(s, "text")
}

// invalidArguments turns the error the SDK returns for arguments that fail a
// tool's input schema, or cannot be decoded into its input, into the
// invalid_input error every hand-written validation error in this project
// also uses. The tools themselves never set such an error on a result, so
// one that carries it came from the SDK (freecad-mcp/internal/mcpserver/reply.go's
// invalidArguments, copied in spirit).
func invalidArguments(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return result, err
		}
		res, ok := result.(*mcp.CallToolResult)
		if !ok || res == nil || !res.IsError || res.GetError() == nil {
			return result, err
		}
		tool := "the tool"
		if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil && call.Params.Name != "" {
			tool = call.Params.Name
		}
		return render.ErrorResult(render.Error{
			Code:    render.CodeInvalidInput,
			Message: shortMessage("Invalid arguments: " + argumentProblem(res.GetError())),
			Hint: fmt.Sprintf("Call %s again with arguments that match its input schema: every required "+
				"argument, each of the listed type, and only listed values and argument names.", tool),
		}), nil
	}
}

// argumentProblem states an SDK argument error without the SDK's framing.
func argumentProblem(err error) string {
	msg := err.Error()
	for _, prefix := range []string{`validating "arguments": `, "validating root: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	msg = strings.TrimPrefix(msg, "json: ")
	return msg
}
