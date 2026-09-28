package moonraker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPrintStartSendsFilenameQueryParam(t *testing.T) {
	var gotMethod, gotPath, gotFilename string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotFilename = r.URL.Query().Get("filename")
		w.Write([]byte(`{"result": "ok"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	if err := c.PrintStart(context.Background(), "job.gcode"); err != nil {
		t.Fatalf("PrintStart: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/printer/print/start" {
		t.Fatalf("path = %q, want %q", gotPath, "/printer/print/start")
	}
	if gotFilename != "job.gcode" {
		t.Fatalf("filename = %q, want %q", gotFilename, "job.gcode")
	}
}

func testPostOnlyEndpoint(t *testing.T, call func(*Client) error, wantPath string) {
	t.Helper()
	var gotMethod, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Write([]byte(`{"result": "ok"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	if err := call(c); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if gotPath != wantPath {
		t.Fatalf("path = %q, want %q", gotPath, wantPath)
	}
}

func TestPrintPausePostsToExpectedPath(t *testing.T) {
	testPostOnlyEndpoint(t, func(c *Client) error { return c.PrintPause(context.Background()) }, "/printer/print/pause")
}

func TestPrintResumePostsToExpectedPath(t *testing.T) {
	testPostOnlyEndpoint(t, func(c *Client) error { return c.PrintResume(context.Background()) }, "/printer/print/resume")
}

func TestPrintCancelPostsToExpectedPath(t *testing.T) {
	testPostOnlyEndpoint(t, func(c *Client) error { return c.PrintCancel(context.Background()) }, "/printer/print/cancel")
}

// TestPrintEndpointsAcceptOkRegardlessOfState documents the live quirk this
// package deliberately does not guard against (control_test_20260928.md):
// Moonraker answers {"result": "ok"} for pause-while-paused,
// cancel-while-idle and resume-while-idle alike. Refusing a
// state-inconsistent request is a later task's job (the policy layer), not
// this client's.
func TestPrintEndpointsAcceptOkRegardlessOfState(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result": "ok"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	if err := c.PrintResume(context.Background()); err != nil {
		t.Fatalf("PrintResume against an idle fake printer: %v", err)
	}
}
