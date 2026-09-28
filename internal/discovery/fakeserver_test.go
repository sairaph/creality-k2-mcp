package discovery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/coder/websocket"
)

// startFakeWSServer runs an in-process port 9999-shaped WebSocket server on
// loopback, sending frames once per accepted connection, and returns the
// host and port a crealityws.Client can dial. Modelled on
// internal/crealityws's own startFakeServer test helper.
func startFakeWSServer(t *testing.T, frames [][]byte) (host string, port int) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"wsslicer"},
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, frame := range frames {
			if err := conn.Write(r.Context(), websocket.MessageText, frame); err != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake ws server url %q: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse fake ws server port from %q: %v", srv.URL, err)
	}
	return u.Hostname(), p
}

// startFakeMoonraker runs an in-process HTTP server answering /printer/info
// and /server/info the way this project's Moonraker fork does, returning
// the host and port a moonraker.Client can dial.
func startFakeMoonraker(t *testing.T, hostname, apiVersionString string) (host string, port int) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/printer/info":
			w.Write([]byte(`{"result": {"state": "ready", "hostname": "` + hostname + `"}}`))
		case "/server/info":
			w.Write([]byte(`{"result": {"klippy_connected": true, "klippy_state": "ready", "components": [], "api_version_string": "` + apiVersionString + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake moonraker url %q: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse fake moonraker port from %q: %v", srv.URL, err)
	}
	return u.Hostname(), p
}

// idleK2PushFields loads the real, unaltered first full-state push captured
// from a K2 (internal/crealityws/testdata/idle_full_push.json), so
// discovery's tests exercise the same fixture crealityws's own tests do
// rather than a hand-built stand-in.
func idleK2PushFields(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../crealityws/testdata/idle_full_push.json")
	if err != nil {
		t.Fatalf("read crealityws idle fixture: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("decode crealityws idle fixture: %v", err)
	}
	return fields
}

// pushFrame clones base, applies overrides, and encodes the result as one
// JSON push frame.
func pushFrame(t *testing.T, base map[string]any, overrides map[string]any) []byte {
	t.Helper()
	clone := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		clone[k] = v
	}
	for k, v := range overrides {
		clone[k] = v
	}
	data, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("marshal push frame: %v", err)
	}
	return data
}
