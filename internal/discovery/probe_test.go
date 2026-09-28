package discovery

import (
	"context"
	"testing"
	"time"
)

func TestProbeHostRejectsInvalidHost(t *testing.T) {
	_, err := ProbeHost(context.Background(), "not a valid host!", 7125)
	if err == nil {
		t.Fatal("expected an error for an invalid host")
	}
}

func TestProbeHostAtIdentifiesRealListener(t *testing.T) {
	moonrakerHost, moonrakerPort := startFakeMoonraker(t, "K2-5885", "1.5.0")
	frame := pushFrame(t, idleK2PushFields(t), nil)
	_, wsPort := startFakeWSServer(t, [][]byte{frame})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := ProbeHostAt(ctx, moonrakerHost, moonrakerPort, wsPort)
	if err != nil {
		t.Fatalf("ProbeHostAt: %v", err)
	}
	if !res.IdentifiedK2 {
		t.Fatalf("IdentifiedK2 = false, want true; res = %+v", res)
	}
	if res.Model != "F021" {
		t.Fatalf("Model = %q, want F021", res.Model)
	}
}

func TestProbeHostAtDefaultsPort(t *testing.T) {
	moonrakerHost, moonrakerPort := startFakeMoonraker(t, "K2-5885", "1.5.0")
	frame := pushFrame(t, idleK2PushFields(t), nil)
	_, wsPort := startFakeWSServer(t, [][]byte{frame})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := ProbeHostAt(ctx, moonrakerHost, moonrakerPort, wsPort)
	if err != nil {
		t.Fatalf("ProbeHostAt: %v", err)
	}
	if res.Port != moonrakerPort {
		t.Fatalf("Port = %d, want %d", res.Port, moonrakerPort)
	}
}

func TestProbeHostAtValidatesBeforeDialing(t *testing.T) {
	_, err := ProbeHostAt(context.Background(), "", 7125, 9999)
	if err == nil {
		t.Fatal("expected an error for an empty host")
	}
}
