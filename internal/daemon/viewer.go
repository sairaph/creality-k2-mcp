// This file implements T11b's browser live view: a lazily started,
// 127.0.0.1-only HTTP server owned by the daemon, serving a self-contained
// MSE viewer page, a per-printer fragmented MP4 stream and a JSON printer
// list, all gated by a random per-daemon access token. See viewer_stream.go
// for the fMP4 muxing handler and viewer_page.go for the page and printer
// list handlers.
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// viewerTokenBytes is how many random bytes the per-daemon access token is
// generated from (hex encoded, so the token itself is twice this many
// characters): 128 bits of entropy, far more than needed to keep another
// local process on 127.0.0.1 from guessing it. The viewer HTTP server binds
// loopback only, but any other process on this machine can still reach a
// loopback port, so every route requires this token as a query parameter
// (dev_docs/plan-v0.1.0.md T11b).
const viewerTokenBytes = 16

// viewerShutdownTimeout bounds how long stop() waits for the HTTP server to
// finish in-flight requests (in practice, open streaming responses) before
// giving up, so daemon shutdown cannot hang forever on a browser that is
// still connected.
const viewerShutdownTimeout = 2 * time.Second

// viewerReadHeaderTimeout bounds how long the HTTP server waits to read a
// request's headers. There is deliberately no WriteTimeout: /stream
// responses are long-lived MSE streams that end only when the client
// disconnects or the camera stops, not on a fixed wall-clock budget.
const viewerReadHeaderTimeout = 5 * time.Second

// maxStreamsPerPrinter and maxStreamsTotal bound how many browser tabs may
// have a /stream/<id>.mp4 response open at once: each subscription holds an
// fmp4 writer and a Hub subscription for as long as the connection lasts, so
// an unbounded number of them (many tabs, or a client that never closes its
// connection) could exhaust the daemon's resources. A caller over the limit
// gets a clear 429 rather than an unbounded queue or a silently dropped
// stream.
const (
	maxStreamsPerPrinter = 4
	maxStreamsTotal      = 16
)

// viewerHub is the subset of *Hub's API the viewer HTTP server depends on.
// *Hub satisfies this directly; tests substitute a fake hub that produces
// access units from a fixture clip instead of a real camera.Session/WebRTC
// connection (AGENTS.md hard testing rule).
type viewerHub interface {
	Subscribe(host string) (id int, ch <-chan camera.AccessUnit)
	Unsubscribe(host string, id int)
	RequestKeyframe(host string) error
}

var _ viewerHub = (*Hub)(nil)

// ViewerPrinter is one enabled printer the camera viewer may show: id and
// display name for the page/JSON list, host for resolving its camera stream
// internally (never sent to the client).
type ViewerPrinter struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host,omitempty"`
}

// PrinterLister lists the enabled printers the camera viewer may show.
// registryAccess (registry_access.go) is the production implementation, via
// the global printer registry; tests substitute a fixed fake list, never the
// real registry file.
type PrinterLister interface {
	ListEnabled() ([]ViewerPrinter, error)
}

// ListEnabled implements PrinterLister for registryAccess: every enabled
// printer in the global registry, id/name/host only (T11b never needs the
// rest of domain.Printer over this seam).
func (registryAccess) ListEnabled() ([]ViewerPrinter, error) {
	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		return nil, err
	}
	enabled := reg.Enabled()
	out := make([]ViewerPrinter, 0, len(enabled))
	for _, p := range enabled {
		out = append(out, ViewerPrinter{ID: p.ID, Name: p.Name, Host: p.Host})
	}
	return out, nil
}

var _ PrinterLister = registryAccess{}

// viewerServer is the lazily started camera viewer HTTP server. Building one
// (newViewerServer) does not open any listener; ensureStarted does that on
// first need, from either the viewer.url RPC method (daemon.go) or a test.
type viewerServer struct {
	hub      viewerHub
	printers PrinterLister
	log      *log.Logger

	mu      sync.Mutex
	started bool
	ln      net.Listener
	srv     *http.Server
	baseURL string
	token   string

	// streamMu guards streamsByPrinter/streamsTotal, the concurrent
	// /stream subscription bookkeeping (see acquireStreamSlot).
	streamMu         sync.Mutex
	streamsByPrinter map[string]int
	streamsTotal     int
}

func newViewerServer(hub viewerHub, printers PrinterLister, logger *log.Logger) *viewerServer {
	return &viewerServer{hub: hub, printers: printers, log: logger, streamsByPrinter: map[string]int{}}
}

// acquireStreamSlot reserves one of maxStreamsPerPrinter/maxStreamsTotal
// concurrent /stream subscriptions for printerID, reporting false (no slot
// reserved) when either bound is already at capacity. Every successful
// acquire must be matched by exactly one releaseStreamSlot, normally via
// defer in the caller (viewer_stream.go's handleStream).
func (v *viewerServer) acquireStreamSlot(printerID string) bool {
	v.streamMu.Lock()
	defer v.streamMu.Unlock()
	if v.streamsTotal >= maxStreamsTotal {
		return false
	}
	if v.streamsByPrinter[printerID] >= maxStreamsPerPrinter {
		return false
	}
	v.streamsByPrinter[printerID]++
	v.streamsTotal++
	return true
}

// releaseStreamSlot releases a slot reserved by a prior successful
// acquireStreamSlot(printerID).
func (v *viewerServer) releaseStreamSlot(printerID string) {
	v.streamMu.Lock()
	defer v.streamMu.Unlock()
	if v.streamsByPrinter[printerID] > 0 {
		v.streamsByPrinter[printerID]--
	}
	if v.streamsByPrinter[printerID] == 0 {
		delete(v.streamsByPrinter, printerID)
	}
	if v.streamsTotal > 0 {
		v.streamsTotal--
	}
}

// ensureStarted starts the HTTP server on 127.0.0.1 with a random free port
// on first call, generating a fresh random access token, and returns its
// base URL ("http://127.0.0.1:PORT") and token. Later calls are a no-op and
// return the same values: one viewer HTTP server per daemon process.
func (v *viewerServer) ensureStarted() (baseURL, token string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.started {
		return v.baseURL, v.token, nil
	}

	// 127.0.0.1 only, never 0.0.0.0 or an empty host (which net.Listen would
	// bind to every interface): dev_docs/plan-v0.1.0.md T11b and AGENTS.md's
	// hard testing rule both require loopback only.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("daemon: viewer: listen on 127.0.0.1: %w", err)
	}

	tok, err := generateViewerToken()
	if err != nil {
		ln.Close()
		return "", "", fmt.Errorf("daemon: viewer: generate access token: %w", err)
	}

	mux := http.NewServeMux()
	v.registerRoutes(mux, tok)
	// ReadHeaderTimeout bounds a slow or stalled client from tying up a
	// connection before it has even sent a full request. There is no
	// WriteTimeout: /stream responses are long-lived and end only when the
	// client disconnects or the camera stops (see viewerReadHeaderTimeout).
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: viewerReadHeaderTimeout}

	v.ln = ln
	v.srv = srv
	v.baseURL = fmt.Sprintf("http://%s", ln.Addr().String())
	v.token = tok
	v.started = true

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			if v.log != nil {
				v.log.Printf("viewer: http server stopped: %v", serveErr)
			}
		}
	}()

	if v.log != nil {
		v.log.Printf("viewer: http server listening on %s", v.baseURL)
	}
	return v.baseURL, tok, nil
}

func generateViewerToken() (string, error) {
	b := make([]byte, viewerTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// URL builds the full viewer page URL: the base URL plus the access token,
// and, if printerID is non-empty, a printer query parameter preselecting
// that printer's stream. It starts the HTTP server lazily on first call.
func (v *viewerServer) URL(printerID string) (string, error) {
	base, token, err := v.ensureStarted()
	if err != nil {
		return "", err
	}
	u := fmt.Sprintf("%s/?token=%s", base, url.QueryEscape(token))
	if printerID != "" {
		u += "&printer=" + url.QueryEscape(printerID)
	}
	return u, nil
}

// stop shuts down the HTTP server if it was ever started, waiting up to
// viewerShutdownTimeout for in-flight requests (open browser streams) to
// finish. Safe to call more than once or on a server that was never started.
func (v *viewerServer) stop() {
	v.mu.Lock()
	srv := v.srv
	v.started = false
	v.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), viewerShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// registerRoutes wires every viewer route onto mux, each gated by
// checkToken(token).
func (v *viewerServer) registerRoutes(mux *http.ServeMux, token string) {
	mux.HandleFunc("GET /", v.checkToken(token, v.handleIndex))
	mux.HandleFunc("GET /api/printers", v.checkToken(token, v.handlePrinters))
	mux.HandleFunc("GET /stream/{file}", v.checkToken(token, v.handleStream))
}

// checkToken wraps next so it only runs when the request's Host header names
// this server itself (checkHost, rejecting DNS rebinding before the token is
// even looked at) and its "token" query parameter matches token
// (constant-time compare). It sets the security headers on every response
// regardless of outcome, per T11b's access-control requirement: the server
// binds 127.0.0.1 only, but any other local process can still reach a
// loopback port.
func (v *viewerServer) checkToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setNoCache(w)
		if !checkHost(r) {
			http.Error(w, "forbidden: unexpected Host header", http.StatusForbidden)
			return
		}
		got := r.URL.Query().Get("token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "forbidden: missing or invalid token", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// checkHost reports whether r's Host header names exactly "127.0.0.1:<port>"
// or "localhost:<port>" for the port this connection was actually accepted
// on (read from the request's context, not stored ahead of time, so this
// works whether the server was started via ensureStarted or, in tests,
// wired directly onto an httptest.Server listening on its own port).
//
// This guards against DNS rebinding: an attacker page on a public origin
// whose DNS record is rebound to 127.0.0.1 can cause the victim's browser to
// connect here, but the Host header the browser sends is still the
// attacker's original hostname, not 127.0.0.1 or localhost, so the request
// is rejected before the token (which that page could not know anyway) is
// even checked.
func checkHost(r *http.Request) bool {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return false
	}
	if host != "127.0.0.1" && host != "localhost" {
		return false
	}
	localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	_, wantPort, err := net.SplitHostPort(localAddr.String())
	if err != nil {
		return false
	}
	return port == wantPort
}

// setNoCache sets headers that stop a browser (or an intermediate proxy)
// from caching a viewer response (the page, the printer list and the video
// stream are all live, never something a client should reuse from cache),
// plus a small set of hardening headers applied to every response
// regardless of outcome: no referrer ever leaves this page (there is
// nowhere for it to go that isn't a local disclosure), no browser MIME
// sniffing away from the declared Content-Type, and no framing by another
// page (this server has nothing worth clickjacking, but there is no reason
// to allow it either).
func setNoCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}
