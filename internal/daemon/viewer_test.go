package daemon

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sairaph/mcp-wizard/daemon/socket"
)

// This file exercises the viewer.url RPC method and viewerServer's own lazy
// start/token/route-registration behavior directly (never a real camera
// session or printer registry file). See viewer_http_test.go for the
// GET /, /api/printers and /stream/<id>.mp4 handlers, tested against a fake
// hub that plays back internal/camera/decode/testdata/camera_sample_clip.h264.

// fakePrinterLister is a PrinterLister that returns a fixed list or error,
// never touching the real printer registry file (AGENTS.md hard testing
// rule: no reliance on machine state).
type fakePrinterLister struct {
	printers []ViewerPrinter
	err      error
}

func (f fakePrinterLister) ListEnabled() ([]ViewerPrinter, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.printers, nil
}

// testViewerPrinters (a fixed {k2, K2 Printer, 192.0.2.10} list) is shared
// with viewer_http_test.go.

// The daemon's viewer.url RPC method starts the viewer HTTP server lazily,
// returns a URL carrying the access token (and, when asked, a printer query
// parameter), and returns the exact same base URL/token on a second call
// (one HTTP server per daemon process, not one per call).
func TestServer_IPC_ViewerURL(t *testing.T) {
	opts := testOptions(t)
	opts.Printers = fakePrinterLister{printers: testViewerPrinters()}
	s := openTestServer(t, opts)
	serveInBackground(t, s)

	conn, err := socket.Dial(opts.Paths.Socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	ctx := context.Background()

	var res1 ViewerURLResult
	if err := conn.Call(ctx, MethodViewerURL, ViewerURLParams{}, &res1); err != nil {
		t.Fatalf("viewer.url: %v", err)
	}
	if !strings.HasPrefix(res1.URL, "http://127.0.0.1:") {
		t.Fatalf("viewer.url = %q, want it to start with http://127.0.0.1:", res1.URL)
	}
	u1, err := url.Parse(res1.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", res1.URL, err)
	}
	token := u1.Query().Get("token")
	if token == "" {
		t.Fatalf("viewer.url = %q, want a non-empty token query parameter", res1.URL)
	}
	if u1.Query().Get("printer") != "" {
		t.Fatalf("viewer.url with no printer_id = %q, want no printer query parameter", res1.URL)
	}

	var res2 ViewerURLResult
	if err := conn.Call(ctx, MethodViewerURL, ViewerURLParams{PrinterID: "k2"}, &res2); err != nil {
		t.Fatalf("viewer.url with printer_id: %v", err)
	}
	u2, err := url.Parse(res2.URL)
	if err != nil {
		t.Fatalf("parse %q: %v", res2.URL, err)
	}
	if u2.Query().Get("printer") != "k2" {
		t.Fatalf("viewer.url with printer_id=k2 = %q, want printer=k2 in the URL", res2.URL)
	}
	if u1.Host != u2.Host || u1.Query().Get("token") != u2.Query().Get("token") {
		t.Fatalf("viewer.url started a second HTTP server or token: %q vs %q", res1.URL, res2.URL)
	}

	// The returned URL is actually reachable and answers with the token
	// accepted, confirming ensureStarted really did bind 127.0.0.1.
	resp, err := http.Get(res1.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", res1.URL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", res1.URL, resp.StatusCode)
	}
}

// ensureStarted is idempotent when called directly (not just through the
// RPC method), and stop() is safe to call on a server that was never
// started and more than once.
func TestViewerServer_EnsureStartedIdempotent(t *testing.T) {
	v := newViewerServer(NewHub(&fakeOpener{}), fakePrinterLister{printers: testViewerPrinters()}, log.New(io.Discard, "", 0))

	base1, token1, err := v.ensureStarted()
	if err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	base2, token2, err := v.ensureStarted()
	if err != nil {
		t.Fatalf("ensureStarted (2nd call): %v", err)
	}
	if base1 != base2 || token1 != token2 {
		t.Fatalf("ensureStarted is not idempotent: (%q,%q) vs (%q,%q)", base1, token1, base2, token2)
	}
	v.stop()
	v.stop() // must not panic

	never := newViewerServer(NewHub(&fakeOpener{}), nil, nil)
	never.stop() // must not panic on a server that was never started
}
