package daemon

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// This file exercises the T11b hardening review findings (dev_docs/plan-v0.1.0.md
// T11b review, "T11b hardening" row of dev_docs/review-backlog.md): the
// security response headers, the Host-header DNS rebinding guard, the HTTP
// server's ReadHeaderTimeout, and the bounded number of concurrent /stream
// subscriptions. Every server here is httptest.NewServer (127.0.0.1 only),
// per AGENTS.md's hard testing rule; see viewer_http_test.go for the shared
// fixtures (fakeViewerHub, loadViewerFixture, getStreamResponse, ...) this
// file reuses.

// newTestViewerServerV is newTestViewerServer plus the *viewerServer itself,
// for tests that need to reach past the HTTP surface (here: the stream slot
// counters).
func newTestViewerServerV(t *testing.T, hub viewerHub, printers PrinterLister) (srv *httptest.Server, token string, v *viewerServer) {
	t.Helper()
	v = newViewerServer(hub, printers, nil)
	token = "test-token-0123456789abcdef"
	mux := http.NewServeMux()
	v.registerRoutes(mux, token)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, token, v
}

// --- security headers (finding 1) ---

// Every response - including a 403 for a missing token - carries the
// hardening headers, since setNoCache (which sets them) runs before the
// token check.
func TestViewerRoutes_SetSecurityHeaders(t *testing.T) {
	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET / (no token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET / (no token): status = %d, want 403", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp)

	resp2, err := http.Get(srv.URL + "/?token=" + token)
	if err != nil {
		t.Fatalf("GET / with a valid token: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET / with a valid token: status = %d, want 200", resp2.StatusCode)
	}
	assertSecurityHeaders(t, resp2)
}

func assertSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want %q", got, "no-referrer")
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want %q", got, "DENY")
	}
	if cc := resp.Header.Get("Cache-Control"); cc == "" {
		t.Errorf("Cache-Control is empty, want the existing no-store headers to still be set")
	}
}

// --- Host header / DNS rebinding guard (finding 2) ---

// A request whose Host header does not name 127.0.0.1 or localhost on the
// actual bound port is rejected with 403 before the token is even checked
// (a valid token is supplied in every case below, so a pass here would only
// be possible if the Host check were missing or broken).
func TestViewerRoutes_RejectsUnexpectedHost(t *testing.T) {
	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address %q: %v", srv.Listener.Addr().String(), err)
	}

	badHosts := []string{
		"evil.example.com:" + port, // DNS rebinding: right IP, attacker's hostname
		"127.0.0.1:19999",          // right host, wrong (not the bound) port
		"127.0.0.1",                // no port at all
		"[::1]:" + port,            // not 127.0.0.1 or localhost
	}
	for _, host := range badHosts {
		resp := getWithHost(t, srv.URL+"/?token="+token, host)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET / with Host %q: status = %d, want 403", host, resp.StatusCode)
		}
	}

	goodHosts := []string{"127.0.0.1:" + port, "localhost:" + port}
	for _, host := range goodHosts {
		resp := getWithHost(t, srv.URL+"/?token="+token, host)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET / with Host %q: status = %d, want 200", host, resp.StatusCode)
		}
	}
}

func getWithHost(t *testing.T, url, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request for %s: %v", url, err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s with Host %q: %v", url, host, err)
	}
	return resp
}

// --- ReadHeaderTimeout, no WriteTimeout (finding 3) ---

// ensureStarted's *http.Server carries the fixed ReadHeaderTimeout and
// leaves WriteTimeout at its zero value (unbounded), so a long-lived
// /stream response is never cut off on a wall-clock budget.
func TestEnsureStarted_ReadHeaderTimeoutSetNoWriteTimeout(t *testing.T) {
	v := newViewerServer(NewHub(&fakeOpener{}), fakePrinterLister{printers: testViewerPrinters()}, nil)
	if _, _, err := v.ensureStarted(); err != nil {
		t.Fatalf("ensureStarted: %v", err)
	}
	t.Cleanup(v.stop)

	if v.srv.ReadHeaderTimeout != viewerReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %s, want %s", v.srv.ReadHeaderTimeout, viewerReadHeaderTimeout)
	}
	if v.srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want 0 (unbounded, streams end on disconnect)", v.srv.WriteTimeout)
	}
}

// --- concurrent /stream subscription limit (finding 4) ---

// acquireStreamSlot/releaseStreamSlot enforce both the per-printer and the
// total bound, and a release always frees exactly the slot it held: pure
// bookkeeping, exercised directly rather than through real HTTP connections
// so the per-printer and total-limit edges are deterministic.
func TestStreamSlots_PerPrinterAndTotalLimits(t *testing.T) {
	v := newViewerServer(nil, nil, nil)

	// Fill printer "a" to its per-printer cap.
	for i := 0; i < maxStreamsPerPrinter; i++ {
		if !v.acquireStreamSlot("a") {
			t.Fatalf("acquireStreamSlot(a) #%d: want true (under the per-printer cap)", i)
		}
	}
	if v.acquireStreamSlot("a") {
		t.Fatalf("acquireStreamSlot(a) at the per-printer cap: want false")
	}

	// A different printer is unaffected by "a" being full.
	if !v.acquireStreamSlot("b") {
		t.Fatalf("acquireStreamSlot(b): want true, a different printer's cap must not block this one")
	}
	v.releaseStreamSlot("b")

	// Releasing one of "a"'s slots frees exactly one.
	v.releaseStreamSlot("a")
	if !v.acquireStreamSlot("a") {
		t.Fatalf("acquireStreamSlot(a) after a release: want true")
	}

	// Drain "a" back to empty and fill the total budget across many
	// printers, respecting each one's own per-printer cap.
	for i := 0; i < maxStreamsPerPrinter; i++ {
		v.releaseStreamSlot("a")
	}
	acquired := 0
	for p := 0; acquired < maxStreamsTotal; p++ {
		printer := fmt.Sprintf("printer-%d", p)
		for i := 0; i < maxStreamsPerPrinter && acquired < maxStreamsTotal; i++ {
			if !v.acquireStreamSlot(printer) {
				t.Fatalf("acquireStreamSlot(%s) #%d: want true, total acquired so far = %d", printer, i, acquired)
			}
			acquired++
		}
	}
	if acquired != maxStreamsTotal {
		t.Fatalf("acquired = %d, want %d", acquired, maxStreamsTotal)
	}
	// A fresh printer, nowhere near its own per-printer cap, is still
	// rejected once the total budget is spent.
	if v.acquireStreamSlot("printer-fresh") {
		t.Fatalf("acquireStreamSlot(printer-fresh) at the total cap: want false")
	}
}

// A real /stream request finds the per-printer cap already spent (simulated
// directly through acquireStreamSlot, the same bookkeeping a genuinely open
// stream would hold - see TestStreamSlots_PerPrinterAndTotalLimits for the
// bookkeeping itself) and gets 429; releasing one slot lets a real request
// through.
//
// This does not hold the "already open" slots via real concurrent /stream
// connections: fakeViewerHub.feed (unlike the real Hub) broadcasts every
// fed access unit to every current subscriber of a host, so feeding a fresh
// keyframe to a newly-subscribed Nth connection would also redeliver it to
// connections 1..N-1 with a duplicate PTS, which is not a scenario this
// test is about and would make the fixture setup fight the fmp4 writer's
// own strictly-increasing PTS requirement instead of exercising the limit.
func TestHandleStream_OverPerPrinterLimitReturns429(t *testing.T) {
	fixture := loadViewerFixture(t)

	hub := newFakeViewerHub()
	srv, token, v := newTestViewerServerV(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	for i := 0; i < maxStreamsPerPrinter; i++ {
		if !v.acquireStreamSlot("k2") {
			t.Fatalf("acquireStreamSlot(k2) #%d: want true", i)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < maxStreamsPerPrinter; i++ {
			v.releaseStreamSlot("k2")
		}
	})

	resp, err := http.Get(srv.URL + "/stream/k2.mp4?token=" + token)
	if err != nil {
		t.Fatalf("GET /stream/k2.mp4 over the per-printer limit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 with %d streams already open for this printer", resp.StatusCode, maxStreamsPerPrinter)
	}
	assertSecurityHeaders(t, resp)

	// Releasing one slot (as a real stream's handleStream would on
	// disconnect, via its deferred releaseStreamSlot) lets a fresh request
	// all the way through to a real streamed response.
	v.releaseStreamSlot("k2")

	resp2, cancel2 := getStreamResponse(t, srv, hub, "192.0.2.10", "/stream/k2.mp4?token="+token, fixture.aus[0])
	defer cancel2()
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("stream after a slot freed up: status = %d, want 200", resp2.StatusCode)
	}
}
