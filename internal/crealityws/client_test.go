package crealityws

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSetLight_Confirmed(t *testing.T) {
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Errorf("fake server read set command: %v", err)
			return
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				LightSw int `json:"lightSw"`
			} `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Errorf("fake server decode set command: %v", err)
			return
		}
		if msg.Method != "set" {
			t.Errorf("method = %q, want set", msg.Method)
		}
		if msg.Params.LightSw != 1 {
			t.Errorf("params.lightSw = %d, want 1", msg.Params.LightSw)
		}

		// Confirming push, as observed live in light_test_20260928.md: the
		// field flips within about a second of the set command.
		confirm := withOverrides(t, idleBaseFields(t), map[string]any{"lightSw": 1})
		if err := conn.Write(ctx, websocket.MessageText, confirm); err != nil {
			t.Errorf("fake server write confirmation: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	confirmed, err := client.SetLight(ctx, true)
	if err != nil {
		t.Fatalf("SetLight: %v", err)
	}
	if !confirmed {
		t.Error("SetLight: confirmed = false, want true")
	}
}

func TestSetLight_ConfirmedOff(t *testing.T) {
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		if _, _, err := conn.Read(ctx); err != nil {
			t.Errorf("fake server read set command: %v", err)
			return
		}
		confirm := withOverrides(t, idleBaseFields(t), map[string]any{"lightSw": 0})
		if err := conn.Write(ctx, websocket.MessageText, confirm); err != nil {
			t.Errorf("fake server write confirmation: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	confirmed, err := client.SetLight(ctx, false)
	if err != nil {
		t.Fatalf("SetLight: %v", err)
	}
	if !confirmed {
		t.Error("SetLight: confirmed = false, want true")
	}
}

func TestSetLight_IgnoresUnrelatedAndWrongValuePushes(t *testing.T) {
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		if _, _, err := conn.Read(ctx); err != nil {
			t.Errorf("fake server read set command: %v", err)
			return
		}
		// Unrelated telemetry, then a push still showing the old value,
		// then the real confirmation. SetLight must not stop early on
		// either of the first two.
		if err := conn.Write(ctx, websocket.MessageText, tempDeltaJSON); err != nil {
			t.Errorf("fake server write temp delta: %v", err)
			return
		}
		stale := withOverrides(t, idleBaseFields(t), map[string]any{"lightSw": 0})
		if err := conn.Write(ctx, websocket.MessageText, stale); err != nil {
			t.Errorf("fake server write stale lightSw: %v", err)
			return
		}
		confirm := withOverrides(t, idleBaseFields(t), map[string]any{"lightSw": 1})
		if err := conn.Write(ctx, websocket.MessageText, confirm); err != nil {
			t.Errorf("fake server write confirmation: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	confirmed, err := client.SetLight(ctx, true)
	if err != nil {
		t.Fatalf("SetLight: %v", err)
	}
	if !confirmed {
		t.Error("SetLight: confirmed = false, want true")
	}
}

func TestSetLight_TimeoutIsUnconfirmedNotError(t *testing.T) {
	done := make(chan struct{})
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		if _, _, err := conn.Read(ctx); err != nil {
			t.Errorf("fake server read set command: %v", err)
			return
		}
		// Never confirms: the printer received the command but the
		// protocol gives no acknowledgement, so the client must time out.
		<-done
	})
	defer close(done)

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), LightConfirmTimeout+2*time.Second)
	defer cancel()

	start := time.Now()
	confirmed, err := client.SetLight(ctx, true)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("SetLight: unexpected error on timeout: %v", err)
	}
	if confirmed {
		t.Error("SetLight: confirmed = true, want false (never assume success without a confirming push)")
	}
	if elapsed < LightConfirmTimeout {
		t.Errorf("SetLight returned after %v, want at least the %v confirm budget", elapsed, LightConfirmTimeout)
	}
}

func TestSetLight_ConnectFailureIsError(t *testing.T) {
	// Nothing is listening on this port.
	client := New("127.0.0.1", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	confirmed, err := client.SetLight(ctx, true)
	if err == nil {
		t.Fatal("SetLight: want error when the printer is unreachable, got nil")
	}
	if confirmed {
		t.Error("SetLight: confirmed = true on a connect failure")
	}
}

// TestExportedAPI pins the package's exported surface to exactly ReadStatus
// and SetLight (decision 8 / safety-architecture.md 3.5: crealityws exposes
// no write method other than the chamber light). Adding any other Set*
// method to Client must be a deliberate, reviewed change to this test, not
// an accident.
func TestExportedAPI(t *testing.T) {
	typ := reflect.TypeOf(&Client{})

	want := map[string]bool{
		"ReadStatus": true,
		"SetLight":   true,
	}

	got := make(map[string]bool, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		got[typ.Method(i).Name] = true
	}

	for name := range want {
		if !got[name] {
			t.Errorf("Client is missing expected exported method %s", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("Client has unexpected exported method %s (crealityws must expose no write method besides SetLight)", name)
		}
	}
}
