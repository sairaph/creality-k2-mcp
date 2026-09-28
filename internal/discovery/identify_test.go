package discovery

import (
	"context"
	"net"
	"testing"
	"time"
)

// closedPort returns a loopback port that was briefly listened on and then
// closed, so a connection attempt to it reliably fails fast (connection
// refused) without depending on any real external host.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for a throwaway port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func TestIdentifySuccess(t *testing.T) {
	moonrakerHost, moonrakerPort := startFakeMoonraker(t, "K2-5885", "1.5.0")
	frame := pushFrame(t, idleK2PushFields(t), nil)
	wsHost, wsPort := startFakeWSServer(t, [][]byte{frame})
	_ = wsHost

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := identify(ctx, moonrakerHost, moonrakerPort, wsPort)

	if !res.IdentifiedK2 {
		t.Fatalf("IdentifiedK2 = false, want true; res = %+v", res)
	}
	if res.Model != "F021" {
		t.Fatalf("Model = %q, want F021", res.Model)
	}
	if res.Hostname != "K2-5885" {
		t.Fatalf("Hostname = %q, want K2-5885 (from printer/info)", res.Hostname)
	}
	if res.APIVersion != "1.5.0" {
		t.Fatalf("APIVersion = %q, want 1.5.0", res.APIVersion)
	}
	if res.Reason != "" {
		t.Fatalf("Reason = %q, want empty on success", res.Reason)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("Errors = %v, want none on success", res.Errors)
	}
	if res.Port != moonrakerPort {
		t.Fatalf("Port = %d, want %d", res.Port, moonrakerPort)
	}
}

func TestIdentifyFallsBackToWSHostnameWhenMoonrakerUnreachable(t *testing.T) {
	// No fake Moonraker at all: printer/info and server/info both fail, but
	// the port 9999 push still identifies the printer and supplies the
	// hostname.
	frame := pushFrame(t, idleK2PushFields(t), nil)
	wsHost, wsPort := startFakeWSServer(t, [][]byte{frame})

	unreachablePort := closedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := identify(ctx, wsHost, unreachablePort, wsPort)

	if !res.IdentifiedK2 {
		t.Fatalf("IdentifiedK2 = false, want true; res = %+v", res)
	}
	if res.Model != "F021" {
		t.Fatalf("Model = %q, want F021", res.Model)
	}
	if res.Hostname != "K2-5885" {
		t.Fatalf("Hostname = %q, want K2-5885 (from port 9999)", res.Hostname)
	}
	if res.APIVersion != "" {
		t.Fatalf("APIVersion = %q, want empty (moonraker unreachable)", res.APIVersion)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("Errors = %v, want 2 entries (printer/info, server/info)", res.Errors)
	}
}

func TestIdentifyNoModelIsNotIdentified(t *testing.T) {
	moonrakerHost, moonrakerPort := startFakeMoonraker(t, "K2-5885", "1.5.0")
	frame := pushFrame(t, idleK2PushFields(t), map[string]any{"model": ""})
	_, wsPort := startFakeWSServer(t, [][]byte{frame})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := identify(ctx, moonrakerHost, moonrakerPort, wsPort)

	if res.IdentifiedK2 {
		t.Fatalf("IdentifiedK2 = true, want false when model is empty; res = %+v", res)
	}
	if res.Reason == "" {
		t.Fatal("Reason is empty, want an explanation")
	}
	if res.Model != "" {
		t.Fatalf("Model = %q, want empty", res.Model)
	}
}

func TestIdentifyWSUnreachable(t *testing.T) {
	moonrakerHost, moonrakerPort := startFakeMoonraker(t, "K2-5885", "1.5.0")
	wsPort := closedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := identify(ctx, moonrakerHost, moonrakerPort, wsPort)

	if res.IdentifiedK2 {
		t.Fatalf("IdentifiedK2 = true, want false; res = %+v", res)
	}
	if res.Reason == "" {
		t.Fatal("Reason is empty, want an explanation of the port 9999 failure")
	}
	if res.Hostname != "K2-5885" {
		t.Fatalf("Hostname = %q, want K2-5885 (from printer/info, still readable)", res.Hostname)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("Errors = %v, want 1 entry (port 9999)", res.Errors)
	}
}

func TestIdentifyErrorsAreShort(t *testing.T) {
	unreachableMoonraker := closedPort(t)
	unreachableWS := closedPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := identify(ctx, "127.0.0.1", unreachableMoonraker, unreachableWS)

	if res.IdentifiedK2 {
		t.Fatal("IdentifiedK2 = true, want false when nothing answers")
	}
	if len(res.Errors) != 3 {
		t.Fatalf("Errors = %v, want 3 entries (printer/info, server/info, port 9999)", res.Errors)
	}
	for _, e := range res.Errors {
		if len(e) > maxErrLen+50 {
			t.Fatalf("error %q is too long (%d bytes)", e, len(e))
		}
	}
}
