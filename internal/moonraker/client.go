// Package moonraker is a typed HTTP client for the Moonraker API on a
// Creality K2, trimmed to exactly the endpoints creality-k2-mcp needs.
//
// Two things make this printer's Moonraker unlike a stock install, and both
// shape this package (see references/analysis/02-moonraker-api.md):
//
//   - Creality's fork trims and extends the component set (no thumbnails,
//     no machine/peripherals, extra fields on several objects such as
//     print_stats and virtual_sdcard). A bare Tornado 404 means "this route
//     does not exist on this build", not "this resource is missing" - the
//     two must not be confused, so classify() tells them apart by the
//     traceback shape rather than by status code alone.
//   - Several write endpoints return ok regardless of printer state
//     (control_test_20260928.md); this package sends exactly the requests
//     it is asked to send and reports what Moonraker said. Refusing
//     state-inconsistent writes is the caller's job (internal/policy in a
//     later task), not this package's.
//
// This package only ever emits the fixed, closed set of gcode templates in
// template.go and the named write endpoints in write.go. There is no
// generic "send a gcode script" or "make an arbitrary request" method:
// TestClientExportedWriteMethodSet pins the exact exported method set so a
// new write method cannot be added silently.
package moonraker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Per-call timeouts. These are deliberately not a single client-wide
// timeout: status and object queries answer in well under a second on this
// printer, file listing/metadata can be slower on a first pass over a large
// file, and upload must scale to the file's size (see uploadTimeout).
const (
	timeoutStatus = 10 * time.Second
	timeoutFiles  = 30 * time.Second

	// maxResponseBytes bounds how much of any single response this client
	// will read into memory. Status, history and console responses are all
	// small JSON documents; this is only a backstop against a misbehaving
	// or malicious server, not a limit anything real is expected to hit.
	maxResponseBytes = 16 << 20 // 16 MiB
)

// uploadTimeout scales the upload timeout to the file size, per
// 02-moonraker-api.md section 11 ("File upload: scale the timeout to file
// size"). 30s base plus 1s per MiB, capped at 10 minutes; the largest file
// seen on a real K2 gcodes folder was ~34 MB (server_files_list_root_gcodes
// fixture), which lands well under the cap.
func uploadTimeout(size int64) time.Duration {
	const base = 30 * time.Second
	const perMiB = time.Second
	const maxTimeout = 10 * time.Minute
	d := base + time.Duration(size/(1<<20))*perMiB
	if d > maxTimeout {
		return maxTimeout
	}
	return d
}

// Client talks to one printer's Moonraker instance over plain HTTP.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client

	routes routeMemo
}

// routeMemo remembers which request paths this build of Moonraker has
// confirmed do not exist at all (a bare Tornado 404, per classify()), so a
// repeated call to a route this printer simply does not have (e.g. a
// trimmed-out endpoint) does not have to hit the network again to find that
// out a second time. It never memoizes a structured "resource not found"
// (a real file, job or object that happens to be missing right now), only
// "this Moonraker build has no such route", which cannot change without a
// firmware/Moonraker update.
type routeMemo struct {
	mu   sync.Mutex
	seen map[string]*Error
}

func (r *routeMemo) lookup(path string) *Error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[path]
}

func (r *routeMemo) remember(path string, err error) {
	merr, ok := err.(*Error)
	if !ok || merr.Status != http.StatusNotFound || merr.Code != CodeUnavailable {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]*Error)
	}
	r.seen[path] = merr
}

// New builds a client for one printer. apiKey may be empty: this printer's
// trusted-LAN posture means most calls need no credential at all
// (02-moonraker-api.md section 9), and X-Api-Key is only sent when apiKey
// is non-empty.
//
// The client ignores ambient proxy environment variables (HTTP_PROXY,
// HTTPS_PROXY, NO_PROXY): a Moonraker instance is always a device on the
// local network, and routing that traffic through a configured proxy would
// be wrong at best and a way to leak LAN traffic at worst.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{
			Transport: &http.Transport{
				Proxy: nil,
			},
		},
	}
}

// resultEnvelope is the "{"result": ...}" shape almost every Moonraker
// response uses. Upload is the one documented exception (write.go).
type resultEnvelope struct {
	Result json.RawMessage `json:"result"`
}

// do performs one HTTP request and returns the raw response body and
// status. It never returns a non-nil error for a non-2xx HTTP response;
// only for a failure to get a response at all (refused connection, DNS
// failure, timeout, TLS error, or a body that could not be read), which is
// always classified unavailable.
func (c *Client) do(ctx context.Context, op, method, path string, query url.Values, body io.Reader, contentType string, timeout time.Duration) (status int, respBody []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := c.baseURL + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return 0, nil, &Error{Op: op, Code: CodeInternal, Body: "build request: " + err.Error()}
	}
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, &Error{Op: op, Code: CodeUnavailable, Body: err.Error()}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, &Error{Op: op, Status: resp.StatusCode, Code: CodeInternal, Body: "read response: " + err.Error()}
	}
	return resp.StatusCode, data, nil
}

// decodeEnvelope decodes a "{"result": ...}" response, classifying a
// non-2xx status via classify() and a malformed body as internal_error.
// out may be nil when the caller only cares that the call succeeded.
func decodeEnvelope(op string, status int, data []byte, out any) error {
	if status < 200 || status >= 300 {
		return &Error{Op: op, Status: status, Code: classify(status, data), Body: errorMessage(data)}
	}
	if out == nil {
		return nil
	}
	var env resultEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return &Error{Op: op, Status: status, Code: CodeInternal, Body: "decode response: " + err.Error()}
	}
	if len(env.Result) == 0 {
		return &Error{Op: op, Status: status, Code: CodeInternal, Body: "response carried no result field"}
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return &Error{Op: op, Status: status, Code: CodeInternal, Body: "decode result: " + err.Error()}
	}
	return nil
}

// get performs a GET against path and decodes its "result" envelope into
// out, consulting and updating the route memo along the way.
func (c *Client) get(ctx context.Context, op, path string, query url.Values, timeout time.Duration, out any) error {
	if cached := c.routes.lookup(path); cached != nil {
		return cached
	}
	status, data, err := c.do(ctx, op, http.MethodGet, path, query, nil, "", timeout)
	if err != nil {
		return err
	}
	derr := decodeEnvelope(op, status, data, out)
	c.routes.remember(path, derr)
	return derr
}

// postForm performs a POST with parameters in the query string (Moonraker's
// convention for its print control endpoints) and no request body.
func (c *Client) postForm(ctx context.Context, op, path string, query url.Values, timeout time.Duration, out any) error {
	status, data, err := c.do(ctx, op, http.MethodPost, path, query, nil, "", timeout)
	if err != nil {
		return err
	}
	return decodeEnvelope(op, status, data, out)
}

// postJSON performs a POST with a JSON request body.
func (c *Client) postJSON(ctx context.Context, op, path string, payload any, timeout time.Duration, out any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return &Error{Op: op, Code: CodeInternal, Body: "encode request: " + err.Error()}
	}
	status, data, err := c.do(ctx, op, http.MethodPost, path, nil, bytes.NewReader(buf), "application/json", timeout)
	if err != nil {
		return err
	}
	return decodeEnvelope(op, status, data, out)
}

// deleteEnvelope performs a DELETE and decodes its "result" envelope.
func (c *Client) deleteEnvelope(ctx context.Context, op, path string, timeout time.Duration, out any) error {
	status, data, err := c.do(ctx, op, http.MethodDelete, path, nil, nil, "", timeout)
	if err != nil {
		return err
	}
	return decodeEnvelope(op, status, data, out)
}
