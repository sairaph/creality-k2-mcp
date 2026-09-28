package camera

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// signalingPath and defaultCameraPort match the printer's WebRTC signaling
// endpoint (references/analysis/04-creality-ws-camera.md section 3):
// http://<host>:8000/call/webrtc_local.
const (
	signalingPath     = "/call/webrtc_local"
	defaultCameraPort = "8000"
)

// normalizeHostPort returns host as a "host:port" pair, appending
// defaultCameraPort if host did not already specify one. host may be a bare
// hostname or IP (the common case for a real printer, which always serves
// the camera on port 8000), or a "host:port" pair (used by tests pointing
// at a fake signaling server on an arbitrary local port).
func normalizeHostPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, defaultCameraPort)
}

// signalingURL builds the printer's signaling URL from host.
func signalingURL(host string) string {
	return "http://" + normalizeHostPort(host) + signalingPath
}

// sigMessage is the base64-JSON wrapping the printer's signaling endpoint
// uses for both the offer body and, sometimes, the answer body (section 3:
// "WebRTC Direct" mode).
type sigMessage struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// postOffer sends offerSDP to the printer's signaling endpoint at url and
// returns the plain SDP text of its answer, whichever of the two encodings
// the printer replied with (see parseAnswer). ctx bounds the whole HTTP
// round trip.
func postOffer(ctx context.Context, url, offerSDP string) (string, error) {
	payload, err := json.Marshal(sigMessage{Type: "offer", SDP: offerSDP})
	if err != nil {
		return "", fmt.Errorf("camera: marshal offer: %w", err)
	}
	body := base64.StdEncoding.EncodeToString(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		return "", fmt.Errorf("camera: build signaling request: %w", err)
	}
	req.Header.Set("Content-Type", "plain/text")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("camera: signaling request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("camera: read signaling response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("camera: signaling endpoint %s returned status %d: %s", url, resp.StatusCode, string(respBody))
	}

	return parseAnswer(respBody)
}

// parseAnswer accepts either a raw SDP answer (starting with "v=0") or the
// same base64-JSON wrapping used for the offer, per
// references/analysis/04-creality-ws-camera.md section 3.
func parseAnswer(body []byte) (string, error) {
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "v=0") {
		// SDP requires every line, including the last, to end with CRLF
		// (RFC 4566); TrimSpace above may have removed that. Without it,
		// pion's SDP parser reports "failed to unmarshal SDP: EOF" instead
		// of treating end-of-input as an implicit line terminator.
		if !strings.HasSuffix(text, "\r\n") {
			text += "\r\n"
		}
		return text, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", fmt.Errorf("camera: answer is neither a raw SDP nor valid base64: %w", err)
	}

	var msg sigMessage
	if err := json.Unmarshal(decoded, &msg); err != nil {
		return "", fmt.Errorf("camera: decode base64-json answer: %w", err)
	}
	if msg.SDP == "" {
		return "", fmt.Errorf("camera: base64-json answer has no sdp field")
	}
	return msg.SDP, nil
}

// Deliberately no SDP-based profile-level-id extraction here: the K2's SDP
// answer advertises profile-level-id=42e01f (Baseline) while the stream's
// own SPS has been observed to actually be Main profile (4d001f). CodecInfo
// always derives ProfileLevelID from the SPS itself (see nal.go's
// parseSPS), never from SDP.
