package moonraker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

type deadlineRecorder struct{ remaining time.Duration }

func (d *deadlineRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	if dl, ok := r.Context().Deadline(); ok {
		d.remaining = time.Until(dl)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"result": "ok"}`)), Request: r}, nil
}

// Supervised session 2026-09-29: Moonraker answers pause, resume and cancel only
// after the whole macro has run (the K2 PAUSE took about 17 s and timed out at
// the 10 s status timeout although it paused). They get the lifecycle timeout;
// PrintStart keeps the short one.
func TestLifecycleWritesUseTheLongTimeout(t *testing.T) {
	for name, call := range map[string]func(*Client) error{
		"pause":  func(c *Client) error { return c.PrintPause(context.Background()) },
		"resume": func(c *Client) error { return c.PrintResume(context.Background()) },
		"cancel": func(c *Client) error { return c.PrintCancel(context.Background()) },
	} {
		rec := &deadlineRecorder{}
		c := New("http://127.0.0.1:1", "")
		c.http = &http.Client{Transport: rec}
		if err := call(c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rec.remaining < 60*time.Second || rec.remaining > timeoutLifecycle {
			t.Errorf("%s: deadline %s away, want about %s", name, rec.remaining, timeoutLifecycle)
		}
	}
	if timeoutLifecycle <= timeoutStatus {
		t.Fatal("the lifecycle timeout must exceed the status timeout")
	}
	rec := &deadlineRecorder{}
	c := New("http://127.0.0.1:1", "")
	c.http = &http.Client{Transport: rec}
	if err := c.PrintStart(context.Background(), "a.gcode"); err != nil || rec.remaining > timeoutStatus {
		t.Fatalf("PrintStart: %v, deadline %s", err, rec.remaining)
	}
}
