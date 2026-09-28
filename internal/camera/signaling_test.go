package camera

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestSignalingURL(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"192.168.1.102", "http://192.168.1.102:8000/call/webrtc_local"},
		{"k2-5885.local", "http://k2-5885.local:8000/call/webrtc_local"},
		{"127.0.0.1:54321", "http://127.0.0.1:54321/call/webrtc_local"},
	}
	for _, c := range cases {
		if got := signalingURL(c.host); got != c.want {
			t.Errorf("signalingURL(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestParseAnswerRawSDP(t *testing.T) {
	// parseAnswer trims surrounding whitespace but always leaves exactly
	// one trailing CRLF: SDP requires every line, including the last, to
	// end with CRLF (RFC 4566), and pion's parser fails with an EOF error
	// on an SDP whose final line is not newline-terminated.
	const body = "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-"
	const want = body + "\r\n"
	got, err := parseAnswer([]byte(body + "\r\n"))
	if err != nil {
		t.Fatalf("parseAnswer: %v", err)
	}
	if got != want {
		t.Errorf("parseAnswer raw sdp: got %q, want %q", got, want)
	}
}

func TestParseAnswerRawSDPWithWhitespace(t *testing.T) {
	const body = "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-"
	const want = body + "\r\n"
	got, err := parseAnswer([]byte("  " + body + "\n\n"))
	if err != nil {
		t.Fatalf("parseAnswer: %v", err)
	}
	if got != want {
		t.Errorf("parseAnswer raw sdp with whitespace: got %q, want %q", got, want)
	}
}

func TestParseAnswerBase64JSON(t *testing.T) {
	const sdp = "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\n"
	payload, err := json.Marshal(sigMessage{Type: "answer", SDP: sdp})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := base64.StdEncoding.EncodeToString(payload)

	got, err := parseAnswer([]byte(body))
	if err != nil {
		t.Fatalf("parseAnswer: %v", err)
	}
	if got != sdp {
		t.Errorf("parseAnswer base64-json: got %q, want %q", got, sdp)
	}
}

func TestParseAnswerGarbage(t *testing.T) {
	if _, err := parseAnswer([]byte("not sdp and not base64 either $$$")); err == nil {
		t.Fatal("parseAnswer on garbage: want error, got nil")
	}
}

func TestParseAnswerEmptyBody(t *testing.T) {
	// The printer replies with a literal "{}" for a bodyless or
	// non-Creality-format request (section 3), which is valid base64 for
	// an empty-ish payload but decodes to JSON with no sdp field.
	if _, err := parseAnswer([]byte("{}")); err == nil {
		t.Fatal("parseAnswer on {} (no sdp field): want error, got nil")
	}
}

func TestParseAnswerRejectsUnrelatedJSON(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte(`{"type":"answer"}`))
	if _, err := parseAnswer([]byte(body)); err == nil {
		t.Fatal("parseAnswer with sdp-less json: want error, got nil")
	} else if !strings.Contains(err.Error(), "sdp") {
		t.Errorf("error should mention the missing sdp field, got: %v", err)
	}
}
