package crealityws

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/coder/websocket"
)

// startFakeServer runs an in-process WebSocket server on loopback and calls
// handler once per accepted connection. It returns the host and port a
// Client can dial. The server and any connection it accepted are cleaned up
// automatically at test end.
func startFakeServer(t *testing.T, handler func(t *testing.T, conn *websocket.Conn)) (host string, port int) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{"wsslicer"},
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		handler(t, conn)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse fake server url %q: %v", srv.URL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse fake server port from %q: %v", srv.URL, err)
	}
	return u.Hostname(), p
}
