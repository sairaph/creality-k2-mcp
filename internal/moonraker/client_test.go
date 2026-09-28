package moonraker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewIgnoresAmbientProxy(t *testing.T) {
	c := New("http://192.168.1.102:7125", "")
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client transport is %T, want *http.Transport", c.http.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("client transport must not consult ambient proxy settings; Proxy must be nil")
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New("http://192.168.1.102:7125/", "key")
	if c.baseURL != "http://192.168.1.102:7125" {
		t.Fatalf("baseURL = %q, want no trailing slash", c.baseURL)
	}
}

func TestAPIKeySentOnlyWhenConfigured(t *testing.T) {
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Api-Key")
		w.Write([]byte(`{"result": {"klippy_connected": true, "klippy_state": "ready", "components": []}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	if _, err := c.ServerInfo(context.Background()); err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if gotHeader != "" {
		t.Fatalf("X-Api-Key header = %q, want empty when no key configured", gotHeader)
	}

	c = New(ts.URL, "secret-key")
	if _, err := c.ServerInfo(context.Background()); err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if gotHeader != "secret-key" {
		t.Fatalf("X-Api-Key header = %q, want %q", gotHeader, "secret-key")
	}
}

func TestConnectionRefusedIsUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := ts.URL
	ts.Close() // nothing is listening on addr any more

	c := New(addr, "")
	_, err := c.ServerInfo(context.Background())
	if err == nil {
		t.Fatal("expected an error against a closed connection")
	}
	merr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if merr.Code != CodeUnavailable {
		t.Fatalf("Code = %q, want %q", merr.Code, CodeUnavailable)
	}
	if merr.Status != 0 {
		t.Fatalf("Status = %d, want 0 for a transport-level failure", merr.Status)
	}
}

func TestContextTimeoutIsUnavailable(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() {
		close(block)
		ts.Close()
	}()

	c := New(ts.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.ServerInfo(ctx)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	merr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if merr.Code != CodeUnavailable {
		t.Fatalf("Code = %q, want %q", merr.Code, CodeUnavailable)
	}
}

func TestRouteMemoSkipsRepeatedNetworkCallsForUnregisteredRoute(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(bareTornado404))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	_, err1 := c.Metadata(context.Background(), "a.gcode")
	_, err2 := c.Metadata(context.Background(), "b.gcode")

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server received %d requests, want 1 (second call should be memoized)", got)
	}
	for i, err := range []error{err1, err2} {
		merr, ok := err.(*Error)
		if !ok {
			t.Fatalf("call %d: error type = %T, want *Error", i, err)
		}
		if merr.Code != CodeUnavailable {
			t.Fatalf("call %d: Code = %q, want %q", i, merr.Code, CodeUnavailable)
		}
	}
}

func TestRouteMemoDoesNotCacheStructuredNotFound(t *testing.T) {
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": {"code": 404, "message": "No file found"}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	c.Metadata(context.Background(), "a.gcode")
	c.Metadata(context.Background(), "b.gcode")

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server received %d requests, want 2 (structured not_found must not be memoized)", got)
	}
}
