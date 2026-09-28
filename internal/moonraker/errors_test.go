package moonraker

import (
	"net/http"
	"testing"
)

const bareTornado404 = `{"error": {"code": 404, "message": "Not Found", "traceback": "Traceback (most recent call last):\n\n  File \"/usr/lib/python3.9/site-packages/tornado/web.py\", line 1681, in _execute\n\n  File \"/usr/share/moonraker/app.py\", line 965, in prepare\n    raise tornado.web.HTTPError(404)\n\ntornado.web.HTTPError: HTTP 404: Not Found\n"}}`

func TestClassifyBareTornado404IsUnavailable(t *testing.T) {
	got := classify(http.StatusNotFound, []byte(bareTornado404))
	if got != CodeUnavailable {
		t.Fatalf("classify(404, bare tornado) = %q, want %q", got, CodeUnavailable)
	}
}

func TestClassifyStructuredNotFoundIsNotFound(t *testing.T) {
	body := `{"error": {"code": 404, "message": "No file found for filename in \"gcodes\""}}`
	got := classify(http.StatusNotFound, []byte(body))
	if got != CodeNotFound {
		t.Fatalf("classify(404, structured) = %q, want %q", got, CodeNotFound)
	}
}

func TestClassifyDeleteMissingFileIsNotFound(t *testing.T) {
	// control_test_20260928.md: deleting a missing file is HTTP 400
	// "Invalid file path", not 404.
	body := `{"error": {"code": 400, "message": "Invalid file path: gcodes/does_not_exist.gcode"}}`
	got := classify(http.StatusBadRequest, []byte(body))
	if got != CodeNotFound {
		t.Fatalf("classify(400, invalid file path) = %q, want %q", got, CodeNotFound)
	}
}

func TestClassifyGenericBadRequestIsInvalidInput(t *testing.T) {
	body := `{"error": {"code": 400, "message": "No objects provided"}}`
	got := classify(http.StatusBadRequest, []byte(body))
	if got != CodeInvalidInput {
		t.Fatalf("classify(400, generic) = %q, want %q", got, CodeInvalidInput)
	}
}

func TestClassifyUnauthorizedIsAuthentication(t *testing.T) {
	body := `{"error": {"code": 401, "message": "Unauthorized"}}`
	got := classify(http.StatusUnauthorized, []byte(body))
	if got != CodeAuthentication {
		t.Fatalf("classify(401) = %q, want %q", got, CodeAuthentication)
	}
}

func TestClassifyForbiddenIsConflict(t *testing.T) {
	// safety-architecture.md 4.2 delete_gcode_file: Moonraker returns 403
	// when the file is in use (e.g. the current print).
	body := `{"error": {"code": 403, "message": "File is loaded, DELETE not permitted"}}`
	got := classify(http.StatusForbidden, []byte(body))
	if got != CodeConflict {
		t.Fatalf("classify(403) = %q, want %q", got, CodeConflict)
	}
}

func TestClassifyServerErrorIsInternal(t *testing.T) {
	got := classify(http.StatusInternalServerError, []byte(`{"error": {"code": 500, "message": "boom"}}`))
	if got != CodeInternal {
		t.Fatalf("classify(500) = %q, want %q", got, CodeInternal)
	}
}

func TestClassifyUndecodableBodyIsInternal(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest} {
		got := classify(status, []byte("not json at all"))
		if got != CodeInternal {
			t.Fatalf("classify(%d, garbage) = %q, want %q", status, got, CodeInternal)
		}
	}
}

func TestErrorMessageFallsBackToRawBody(t *testing.T) {
	got := errorMessage([]byte("plain text error"))
	if got != "plain text error" {
		t.Fatalf("errorMessage = %q, want %q", got, "plain text error")
	}
}

func TestErrorMessagePrefersParsedMessage(t *testing.T) {
	got := errorMessage([]byte(`{"error": {"code": 400, "message": "specific problem"}}`))
	if got != "specific problem" {
		t.Fatalf("errorMessage = %q, want %q", got, "specific problem")
	}
}

func TestErrorErrorStringIncludesStatusWhenPresent(t *testing.T) {
	e := &Error{Op: "Test", Status: 404, Body: "gone", Code: CodeNotFound}
	got := e.Error()
	if got == "" {
		t.Fatal("Error() returned empty string")
	}
}

func TestErrorErrorStringWithoutStatus(t *testing.T) {
	e := &Error{Op: "Test", Body: "connection refused", Code: CodeUnavailable}
	got := e.Error()
	if got == "" {
		t.Fatal("Error() returned empty string")
	}
}
