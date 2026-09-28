package moonraker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Code is a render error code, matching the strings mcp-wizard's render
// package uses (render.CodeNotFound and friends), without importing that
// package here: this package has no MCP or rendering dependency, only an
// HTTP client and typed errors that a caller in another package can
// classify however it likes.
type Code string

// Codes this package's classifier ever returns. See
// references/analysis/02-moonraker-api.md section 11 for the mapping this
// implements, and control_test_20260928.md for the two live quirks
// (upload's missing result wrapper, delete's 400 on a missing file).
const (
	CodeNotFound       Code = "not_found"
	CodeInvalidInput   Code = "invalid_input"
	CodeAuthentication Code = "authentication"
	CodeConflict       Code = "conflict"
	CodeUnavailable    Code = "unavailable"
	CodeInternal       Code = "internal_error"
)

// Error is what every Client method returns on failure. Status is 0 for a
// transport-level failure (no HTTP response at all - refused connection,
// timeout, DNS failure); otherwise it is the HTTP status Moonraker sent.
// Body carries Moonraker's error message (or the raw body when it could not
// be parsed as one) so a caller can show the person something useful.
type Error struct {
	Op     string
	Status int
	Body   string
	Code   Code
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("moonraker: %s: %s", e.Op, e.Body)
	}
	return fmt.Sprintf("moonraker: %s: http %d: %s", e.Op, e.Status, e.Body)
}

// moonrakerErrorBody is the shape of Moonraker's JSON error envelope, both
// for a real handler's structured error and for the bare Tornado traceback
// a request to an unregistered route produces (see 02-moonraker-api.md
// section 0 and 11; both were captured byte-identical in
// references/printer-snapshot/extra/_compare_bogus_endpoint.json and
// server_files_thumbnails_base_obj.json).
type moonrakerErrorBody struct {
	Error struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		Traceback string `json:"traceback"`
	} `json:"error"`
}

// isBareTornadoTraceback reports whether a 404's traceback is the generic
// "this route does not exist" shape Tornado raises from app.py's request
// preparation, as opposed to a specific handler's own "resource not found"
// error. Only the former means the Moonraker build does not support this
// endpoint at all.
func isBareTornadoTraceback(traceback string) bool {
	return strings.Contains(traceback, "app.py") &&
		strings.Contains(traceback, "in prepare") &&
		strings.Contains(traceback, "HTTPError(404)")
}

// classify maps an HTTP status and response body to a render code, per
// 02-moonraker-api.md section 11 and the live quirks in
// control_test_20260928.md. It never inspects the request path: the delete
// "Invalid file path" quirk is identified from the message alone, since
// that is the only place it is safe to special-case (a generic 400 is
// invalid_input everywhere else, including on files this same body shape
// could describe for a different endpoint).
func classify(status int, body []byte) Code {
	var parsed moonrakerErrorBody
	decodeErr := json.Unmarshal(body, &parsed)

	switch status {
	case http.StatusNotFound:
		if decodeErr != nil {
			return CodeInternal
		}
		if isBareTornadoTraceback(parsed.Error.Traceback) {
			return CodeUnavailable
		}
		return CodeNotFound
	case http.StatusBadRequest:
		if decodeErr != nil {
			return CodeInternal
		}
		if strings.Contains(parsed.Error.Message, "Invalid file path") {
			return CodeNotFound
		}
		return CodeInvalidInput
	case http.StatusUnauthorized:
		return CodeAuthentication
	case http.StatusForbidden:
		// Per this project's write policy (safety-architecture.md 4.2,
		// delete_gcode_file): Moonraker returns 403 when a file is in use
		// (e.g. it is the current print), which is a state conflict, not a
		// permissions problem, so it is reported as conflict rather than
		// the generic "forbidden" code.
		return CodeConflict
	default:
		if status >= 500 {
			return CodeInternal
		}
		// Any other unexpected status (should not happen against a real
		// Moonraker build) is treated as internal_error: it is not one of
		// the classified cases and points at a bug or an unknown server
		// behaviour, not a normal operating condition.
		return CodeInternal
	}
}

// errorMessage extracts Moonraker's own error message from a response body,
// falling back to the raw body (trimmed and bounded) when it does not parse
// as the expected envelope.
func errorMessage(body []byte) string {
	var parsed moonrakerErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	text := strings.TrimSpace(string(body))
	const maxLen = 500
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}
	if text == "" {
		text = "(empty response body)"
	}
	return text
}
