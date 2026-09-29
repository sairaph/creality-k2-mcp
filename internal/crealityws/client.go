// Package crealityws is a short-lived client for the Creality K2 firmware's
// port 9999 WebSocket (dev_docs/plan-v0.1.0.md, decision 8;
// dev_docs/safety-architecture.md, section 3.5). The protocol has no
// request/reply correlation: every push is a partial or full state dump, and
// a "set" command has no acknowledgement frame distinguishable from ordinary
// telemetry (references/analysis/04-creality-ws-camera.md, section 1;
// references/analysis/09-creality-state-machine.md, section 4, point 6).
// Every operation therefore connects, does its one job, and closes rather
// than keeping a long-lived connection open.
//
// Moonraker is authoritative for gating decisions; this package only
// corroborates (safety-architecture.md, P3), except for the CFS signals
// (printerstate package doc). It is the sole write path for the chamber
// light, for CFS filament definitions, CFS starts and the start-window stop,
// none of which has a Moonraker-side command of its own. The writes are exactly these, all fixed
// shape and all without an acknowledgement frame (cfs.go):
//
//   - SetLight: {"lightSw":0|1}
//   - ModifyMaterial: {"modifyMaterial":{...}} (rewrite one slot definition)
//   - StartCFSPrint: {"colorMatch":{...}} then {"multiColorPrint":{...}}
//   - StartSpoolPrint: {"opGcodeFile":"printprt:<path>","enableSelfTest":n}
//   - Stop: {"stop":1}, used only to cancel during the print-start window
//   - SetSpeedMode: {"speedMode":1} (Silent) or {"speedMode":0}, used only by
//     set_speed_preset (the speed factor itself goes through Moonraker)
//
// The typed reads are BoxsInfo, Materials and GcodeFiles (plan-v0.2.0.md
// section 1).
package crealityws

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

// DefaultPort is the port the K2 firmware's WebSocket listens on.
const DefaultPort = 9999

const (
	// ConnectTimeout bounds the initial TCP + WebSocket handshake.
	ConnectTimeout = 5 * time.Second
	// StatusReadTimeout bounds how long ReadStatus waits for the key
	// identifying fields to appear across merged push frames.
	StatusReadTimeout = 3 * time.Second
	// LightConfirmTimeout bounds how long SetLight waits for a push that
	// confirms lightSw reached the requested value.
	LightConfirmTimeout = 5 * time.Second
	// maxReadLimit bounds how large a single frame this client will accept
	// from the printer (github.com/coder/websocket defaults to 32 KiB, which
	// the full-state push already gets close to; 1 MiB is generous headroom
	// for that push to grow while still bounding memory against a
	// misbehaving or compromised printer, since this package trusts nothing
	// below Moonraker, per P1/P3).
	maxReadLimit = 1 << 20 // 1 MiB
)

// heartbeatText is the literal (non-JSON) text frame used as a heartbeat
// acknowledgement in both directions on this protocol
// (04-creality-ws-camera.md, section 1).
const heartbeatText = "ok"

// Client talks to one printer's port 9999. It holds no connection between
// calls; each exported method opens its own short-lived WebSocket.
type Client struct {
	host string
	port int
}

// New returns a Client for host. Port defaults to DefaultPort when 0.
func New(host string, port int) *Client {
	if port == 0 {
		port = DefaultPort
	}
	return &Client{host: host, port: port}
}

func (c *Client) url() string {
	return fmt.Sprintf("ws://%s:%d", c.host, c.port)
}

// connect dials the printer's port 9999 WebSocket, bounded by ConnectTimeout
// regardless of the caller's own deadline (a hung TCP handshake must not
// block longer than that).
func (c *Client) connect(ctx context.Context) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, c.url(), &websocket.DialOptions{
		Subprotocols: []string{"wsslicer"},
	})
	if err != nil {
		return nil, fmt.Errorf("crealityws: connect to %s: %w", c.url(), err)
	}
	// The full-state push is the largest frame this protocol sends and is
	// already tens of KB; 1 MiB bounds memory against a misbehaving or
	// compromised printer (P1/P3: nothing below Moonraker is trusted) while
	// leaving plenty of room for that push to grow.
	conn.SetReadLimit(maxReadLimit)
	return conn, nil
}

// ReadStatus connects, reads and merges push frames until the fields that
// identify a printer (state, deviceState, model, hostname - present only in
// the firmware's initial full-state push, never in a later delta) are all
// seen or StatusReadTimeout elapses, then closes the connection.
//
// A frame that is not valid JSON, the literal "ok" heartbeat text, or a
// non-object JSON value is skipped rather than treated as an error - this
// protocol interleaves telemetry with no framing guarantee beyond "one JSON
// object or the literal ok per text frame". If the read budget elapses
// without ever seeing a usable frame, ReadStatus returns an error; if it
// elapses after seeing at least one usable frame, it returns whatever was
// merged so far rather than erroring, since a partial status is still
// useful and the caller can inspect Raw to see what is missing.
func (c *Client) ReadStatus(ctx context.Context) (Status, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return Status{}, err
	}
	defer conn.CloseNow()

	readCtx, cancel := context.WithTimeout(ctx, StatusReadTimeout)
	defer cancel()

	raw := map[string]any{}
	for {
		frame, ok, err := readFrame(readCtx, conn)
		if err != nil {
			if ctx.Err() != nil {
				return Status{}, fmt.Errorf("crealityws: read status: %w", ctx.Err())
			}
			if len(raw) == 0 {
				return Status{}, fmt.Errorf("crealityws: read status: %w", err)
			}
			break
		}
		if ok {
			mergeInto(raw, frame)
			if hasKeyFields(raw) {
				break
			}
		}
	}

	return buildStatus(raw), nil
}

// SetLight is the chamber-light write: connect, send
// {"method":"set","params":{"lightSw":0|1}}, then read pushes until lightSw
// equals the requested value or LightConfirmTimeout elapses, then close.
//
// The protocol has no acknowledgement for a set command
// (04-creality-ws-camera.md, section 1; 09-creality-state-machine.md,
// section 4 point 6), so a timeout is a legitimate, expected outcome, not a
// failure: SetLight returns (false, nil) in that case, meaning "sent, not
// confirmed" - never assume the write took effect. err is non-nil only for
// an actual failure to connect, send or read (network error, or the
// caller's own context being cancelled), not for an unconfirmed write.
func (c *Client) SetLight(ctx context.Context, on bool) (confirmed bool, err error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()

	want := 0
	if on {
		want = 1
	}

	body, err := json.Marshal(map[string]any{
		"method": "set",
		"params": map[string]any{"lightSw": want},
	})
	if err != nil {
		return false, fmt.Errorf("crealityws: encode set lightSw: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, body); err != nil {
		return false, fmt.Errorf("crealityws: send set lightSw: %w", err)
	}

	readCtx, cancel2 := context.WithTimeout(ctx, LightConfirmTimeout)
	defer cancel2()

	for {
		frame, ok, err := readFrame(readCtx, conn)
		if err != nil {
			if ctx.Err() != nil {
				return false, fmt.Errorf("crealityws: confirm set lightSw: %w", ctx.Err())
			}
			// LightConfirmTimeout elapsed with no confirming push: a
			// legitimate unconfirmed outcome, not an error.
			return false, nil
		}
		if !ok {
			continue
		}
		if v, present := frame["lightSw"]; present {
			if got, ok := asInt(v); ok && got == want {
				return true, nil
			}
		}
	}
}

// readFrame reads one WebSocket message and, if it decodes to a JSON
// object, returns it. It returns ok=false (with a nil error) for a frame
// that should be skipped: the literal "ok" heartbeat, a binary or text frame
// that is not valid JSON, or JSON that does not decode to an object (e.g. a
// bare number or array). Binary frames are treated the same as text, since
// this firmware's own reference client UTF-8-decodes binary frames before
// the same JSON parse (04-creality-ws-camera.md, section 1).
func readFrame(ctx context.Context, conn *websocket.Conn) (map[string]any, bool, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, false, err
	}
	if string(data) == heartbeatText {
		return nil, false, nil
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		return nil, false, nil
	}
	return frame, true, nil
}
