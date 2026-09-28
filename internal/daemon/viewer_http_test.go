package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	h264c "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

	"github.com/sairaph/creality_k2_mcp/internal/camera"
	"github.com/sairaph/creality_k2_mcp/internal/camera/fmp4"
)

// This file exercises viewerServer's HTTP handlers (GET /, GET
// /api/printers, GET /stream/<printer-id>.mp4) against a fake hub that
// plays back internal/camera/decode/testdata/camera_sample_clip.h264, never
// a real camera.Session, WebRTC session or printer registry file (AGENTS.md
// hard testing rule). Every server here is httptest.NewServer, which binds
// 127.0.0.1 only.

// --- fake hub: produces access units purely from what the test feeds it ---

// fakeViewerHub is a viewerHub whose subscribers receive exactly what the
// test pushes via feed, in order, with no priming and no real upstream
// connection. subscribed reports every host Subscribe was called for, so a
// test can wait for the handler to actually subscribe before feeding it
// (feeding before that would just be dropped: unlike the real Hub, this
// fake keeps no "last keyframe").
type fakeViewerHub struct {
	mu         sync.Mutex
	subs       map[string]map[int]chan camera.AccessUnit
	nextID     int
	kfReqs     map[string]int
	subscribed chan string
}

func newFakeViewerHub() *fakeViewerHub {
	return &fakeViewerHub{
		subs:       map[string]map[int]chan camera.AccessUnit{},
		kfReqs:     map[string]int{},
		subscribed: make(chan string, 32),
	}
}

func (h *fakeViewerHub) Subscribe(host string) (int, <-chan camera.AccessUnit) {
	h.mu.Lock()
	id := h.nextID
	h.nextID++
	ch := make(chan camera.AccessUnit, 64)
	if h.subs[host] == nil {
		h.subs[host] = map[int]chan camera.AccessUnit{}
	}
	h.subs[host][id] = ch
	h.mu.Unlock()

	select {
	case h.subscribed <- host:
	default:
	}
	return id, ch
}

func (h *fakeViewerHub) Unsubscribe(host string, id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.subs[host]; ok {
		if ch, ok := m[id]; ok {
			close(ch)
			delete(m, id)
		}
	}
}

func (h *fakeViewerHub) RequestKeyframe(host string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.kfReqs[host]++
	return nil
}

func (h *fakeViewerHub) keyframeRequests(host string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.kfReqs[host]
}

// feed delivers au to every subscriber currently registered for host,
// blocking on each subscriber's own buffered channel (large enough,
// subscriberBufferSize-sized here too, that a test never has to drain
// concurrently just to keep feed from blocking).
func (h *fakeViewerHub) feed(host string, au camera.AccessUnit) {
	h.mu.Lock()
	var chans []chan camera.AccessUnit
	for _, ch := range h.subs[host] {
		chans = append(chans, ch)
	}
	h.mu.Unlock()
	for _, ch := range chans {
		ch <- au
	}
}

// waitForSubscribe blocks until Subscribe(host) has been observed, or fails
// the test after timeout.
func waitForSubscribe(t *testing.T, h *fakeViewerHub, host string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case got := <-h.subscribed:
			if got == host {
				return
			}
		case <-deadline:
			t.Fatalf("no Subscribe(%q) observed within %s", host, timeout)
		}
	}
}

var _ viewerHub = (*fakeViewerHub)(nil)

// --- fixture: the shared sample clip, grouped into access units ---

const viewerSampleClipPath = "../camera/decode/testdata/camera_sample_clip.h264"

type viewerClipFixture struct {
	sps, pps  []byte
	aus       []camera.AccessUnit // starts at the first keyframe, strictly increasing PTS
	keyframes int
}

// splitViewerAccessUnits groups annexB's NAL units (each including its own
// Annex-B start code) into access units. This mirrors
// internal/camera/fmp4/writer_test.go's splitAccessUnits over the same
// fixture (duplicated here rather than exported, since that package's own
// helper is unexported and this test needs no other part of it): a new
// access unit starts at every NAL unit following one that already carries a
// VCL NAL (type 1 or 5), since the clip carries at most one slice per
// picture.
func splitViewerAccessUnits(annexB []byte) [][]byte {
	var starts []int
	for i := 0; i+2 < len(annexB); i++ {
		if annexB[i] == 0 && annexB[i+1] == 0 && annexB[i+2] == 1 {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return nil
	}

	type nal struct{ begin, end int }
	nals := make([]nal, 0, len(starts))
	for i, s := range starts {
		end := len(annexB)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		begin := s
		if begin > 0 && annexB[begin-1] == 0 {
			begin--
		}
		nals = append(nals, nal{begin, end})
	}

	naluType := func(n nal) int {
		i := n.begin + 3
		if annexB[n.begin+2] == 0 {
			i = n.begin + 4
		}
		return int(annexB[i] & 0x1f)
	}
	isVCL := func(t int) bool { return t == 1 || t == 5 }

	var aus [][]byte
	var curStart int
	curHasVCL := false
	curOpen := false
	for _, n := range nals {
		if curHasVCL {
			aus = append(aus, annexB[curStart:n.begin])
			curOpen = false
			curHasVCL = false
		}
		if !curOpen {
			curStart = n.begin
			curOpen = true
		}
		if isVCL(naluType(n)) {
			curHasVCL = true
		}
	}
	if curOpen {
		aus = append(aus, annexB[curStart:])
	}
	return aus
}

func viewerIsKeyframe(t *testing.T, au []byte) bool {
	t.Helper()
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(au); err != nil {
		t.Fatalf("AnnexB.Unmarshal: %v", err)
	}
	return h264c.IsRandomAccess(nalus)
}

func viewerParameterSets(t *testing.T, au []byte) (sps, pps []byte) {
	t.Helper()
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(au); err != nil {
		t.Fatalf("AnnexB.Unmarshal: %v", err)
	}
	for _, n := range nalus {
		switch h264c.NALUType(n[0] & 0x1f) {
		case h264c.NALUTypeSPS:
			sps = n
		case h264c.NALUTypePPS:
			pps = n
		}
	}
	if sps == nil || pps == nil {
		t.Fatalf("access unit is missing SPS or PPS")
	}
	return sps, pps
}

// mutateViewerSPS returns a copy of au with its SPS NAL unit's last byte
// flipped, so fmp4.Writer sees it as different parameters (bytes.Equal
// fails) without needing the mutated bytes to still be syntactically valid
// SPS: mirrors internal/camera/fmp4/writer_test.go's mutateSPS.
func mutateViewerSPS(t *testing.T, au []byte) []byte {
	t.Helper()
	mutated := append([]byte(nil), au...)
	var nalus h264c.AnnexB
	if err := nalus.Unmarshal(mutated); err != nil {
		t.Fatalf("AnnexB.Unmarshal: %v", err)
	}
	for _, n := range nalus {
		if len(n) > 4 && h264c.NALUType(n[0]&0x1f) == h264c.NALUTypeSPS {
			n[len(n)-1] ^= 0xff
			return mutated
		}
	}
	t.Fatalf("mutateViewerSPS: access unit has no SPS NAL unit")
	return nil
}

// loadViewerFixture reads the shared sample clip and returns it as access
// units starting at the first keyframe (fmp4.Writer's own requirement),
// with synthesized strictly-increasing 90 kHz timestamps at a nominal 15
// fps.
func loadViewerFixture(t *testing.T) viewerClipFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(viewerSampleClipPath))
	if err != nil {
		t.Fatalf("read %s: %v", viewerSampleClipPath, err)
	}

	all := splitViewerAccessUnits(data)
	if len(all) < 2 {
		t.Fatalf("sample clip produced only %d access units", len(all))
	}

	first := -1
	for i, au := range all {
		if viewerIsKeyframe(t, au) {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("sample clip contains no keyframe access unit")
	}
	trimmed := all[first:]
	sps, pps := viewerParameterSets(t, trimmed[0])

	const ticksPerFrame = fmp4.TimeScale / 15
	aus := make([]camera.AccessUnit, len(trimmed))
	keyframes := 0
	for i, raw := range trimmed {
		kf := viewerIsKeyframe(t, raw)
		if kf {
			keyframes++
		}
		aus[i] = camera.AccessUnit{Data: raw, Keyframe: kf, PTS: uint64(i) * ticksPerFrame}
	}

	return viewerClipFixture{sps: sps, pps: pps, aus: aus, keyframes: keyframes}
}

// --- test server wiring ---

func testViewerPrinters() []ViewerPrinter {
	return []ViewerPrinter{{ID: "k2", Name: "K2 Printer", Host: "192.0.2.10"}}
}

// newTestViewerServer wires viewerServer's routes onto a fresh mux with a
// fixed token, served over httptest.NewServer (127.0.0.1 only), without
// going through ensureStarted (which would open a second, independent
// listener and generate its own random token neither test nor caller could
// predict).
func newTestViewerServer(t *testing.T, hub viewerHub, printers PrinterLister) (srv *httptest.Server, token string) {
	t.Helper()
	v := newViewerServer(hub, printers, nil)
	token = "test-token-0123456789abcdef"
	mux := http.NewServeMux()
	v.registerRoutes(mux, token)
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, token
}

// --- token enforcement ---

func TestViewerRoutes_RequireToken(t *testing.T) {
	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	for _, path := range []string{"/", "/api/printers", "/stream/k2.mp4"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s (no token): %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s with no token: status = %d, want 403", path, resp.StatusCode)
		}

		resp2, err := http.Get(srv.URL + path + "?token=wrong")
		if err != nil {
			t.Fatalf("GET %s (wrong token): %v", path, err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s with wrong token: status = %d, want 403", path, resp2.StatusCode)
		}
	}

	// A valid token reaches the handler: GET / succeeds, and an unknown
	// printer id on the stream route is rejected as not_found rather than
	// forbidden, without ever needing to feed the hub an access unit.
	resp, err := http.Get(srv.URL + "/?token=" + token)
	if err != nil {
		t.Fatalf("GET / with a valid token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / with a valid token: status = %d, want 200", resp.StatusCode)
	}

	resp3, err := http.Get(srv.URL + "/stream/does-not-exist.mp4?token=" + token)
	if err != nil {
		t.Fatalf("GET /stream/does-not-exist.mp4: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /stream/does-not-exist.mp4 with a valid token: status = %d, want 404", resp3.StatusCode)
	}
}

// --- GET /api/printers ---

func TestHandlePrinters_JSONList(t *testing.T) {
	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	resp, err := http.Get(srv.URL + "/api/printers?token=" + token)
	if err != nil {
		t.Fatalf("GET /api/printers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var got []printerJSON
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID != "k2" || got[0].Name != "K2 Printer" {
		t.Fatalf("got %+v, want one printer {k2, K2 Printer}", got)
	}
}

// --- GET / (the page) ---

func TestHandleIndex_PageContainsStreamURL(t *testing.T) {
	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	resp, err := http.Get(srv.URL + "/?token=" + token)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(body)

	wantURL := fmt.Sprintf("/stream/k2.mp4?token=%s", token)
	if !strings.Contains(text, wantURL) {
		t.Fatalf("page does not contain the stream URL %q:\n%s", wantURL, text)
	}
	if !strings.Contains(text, "K2 Printer") {
		t.Fatalf("page does not contain the printer's display name:\n%s", text)
	}
	if strings.Contains(text, "cdn.") || strings.Contains(text, "<link") {
		t.Fatalf("page appears to reference an external resource, want fully self-contained:\n%s", text)
	}
}

// GET /?printer=<id> filters the page to just that printer.
func TestHandleIndex_FiltersByPrinterQueryParam(t *testing.T) {
	hub := newFakeViewerHub()
	printers := fakePrinterLister{printers: []ViewerPrinter{
		{ID: "a", Name: "Printer A", Host: "192.0.2.1"},
		{ID: "b", Name: "Printer B", Host: "192.0.2.2"},
	}}
	srv, token := newTestViewerServer(t, hub, printers)

	resp, err := http.Get(srv.URL + "/?token=" + token + "&printer=b")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "/stream/b.mp4?token="+token) {
		t.Fatalf("page filtered to printer b does not contain its stream URL:\n%s", text)
	}
	if strings.Contains(text, "/stream/a.mp4") {
		t.Fatalf("page filtered to printer b still contains printer a's stream URL:\n%s", text)
	}
}

// --- GET /stream/<printer-id>.mp4 ---

// getStreamResponse starts a GET against path in a goroutine (since the
// handler blocks writing headers until a keyframe is fed) and returns once
// headers are received, having fed feedFirst on hub beforehand once the
// handler has subscribed.
func getStreamResponse(t *testing.T, srv *httptest.Server, hub *fakeViewerHub, host, path string, feedFirst camera.AccessUnit) (*http.Response, context.CancelFunc) {
	t.Helper()
	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		cancel()
		t.Fatalf("build request: %v", err)
	}

	type result struct {
		resp *http.Response
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		resultCh <- result{resp, err}
	}()

	waitForSubscribe(t, hub, host, 3*time.Second)
	hub.feed(host, feedFirst)

	select {
	case r := <-resultCh:
		if r.err != nil {
			cancel()
			t.Fatalf("GET %s: %v", path, r.err)
		}
		return r.resp, cancel
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("did not receive response headers within 3s of feeding a keyframe")
		return nil, cancel
	}
}

// TestHandleStream_InitSegmentFragmentsAndCodecString feeds the whole
// fixture clip through the stream handler and confirms: the codec string
// header is derived from the fixture's actual SPS (not hardcoded), no-cache
// headers are set, and the response body contains a valid fMP4 init segment
// ("ftyp"/"moov") followed by at least one fragment ("moof"/"mdat") once a
// second GOP's keyframe has crossed the (shrunk) fragment duration.
func TestHandleStream_InitSegmentFragmentsAndCodecString(t *testing.T) {
	prevFrag := streamFragmentDuration
	streamFragmentDuration = time.Millisecond
	t.Cleanup(func() { streamFragmentDuration = prevFrag })

	fixture := loadViewerFixture(t)
	if fixture.keyframes < 2 {
		t.Fatalf("fixture needs at least 2 keyframes (GOPs), got %d", fixture.keyframes)
	}

	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	resp, cancel := getStreamResponse(t, srv, hub, "192.0.2.10", "/stream/k2.mp4?token="+token, fixture.aus[0])
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	wantCodec, err := fmp4.CodecString(fixture.sps)
	if err != nil {
		t.Fatalf("fmp4.CodecString: %v", err)
	}
	if got := resp.Header.Get("X-Video-Codec"); got != wantCodec {
		t.Fatalf("X-Video-Codec = %q, want %q (derived from the fixture's actual SPS)", got, wantCodec)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type = %q, want video/mp4", ct)
	}

	var body bytes.Buffer
	var mu sync.Mutex
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				mu.Lock()
				body.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	for _, au := range fixture.aus[1:] {
		hub.feed("192.0.2.10", au)
	}

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		got := body.String()
		mu.Unlock()
		if strings.Contains(got, "ftyp") && strings.Contains(got, "moov") &&
			strings.Contains(got, "moof") && strings.Contains(got, "mdat") {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("did not observe an init segment and a fragment within 3s; got %d bytes so far", len(got))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestHandleStream_ParameterChangeEndsStream confirms that a keyframe access
// unit whose SPS differs from the one the stream started with (T11b's
// fmp4.ErrParametersChanged case) ends the response body, rather than
// hanging or silently continuing with stale parameters: a browser's MSE
// page is expected to see the fetch stream complete and reconnect.
func TestHandleStream_ParameterChangeEndsStream(t *testing.T) {
	fixture := loadViewerFixture(t)

	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	resp, cancel := getStreamResponse(t, srv, hub, "192.0.2.10", "/stream/k2.mp4?token="+token, fixture.aus[0])
	defer cancel()
	defer resp.Body.Close()

	mutated := camera.AccessUnit{
		Data:     mutateViewerSPS(t, fixture.aus[0].Data),
		Keyframe: true,
		PTS:      fixture.aus[0].PTS + 1,
	}
	hub.feed("192.0.2.10", mutated)

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading the stream body after a parameter change: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end within 3s of a keyframe with changed SPS")
	}
}

// TestHandleStream_NoKeyframeRequestsKeyframeViaHub confirms that once
// fmp4.ErrNoKeyframe fires (no keyframe within the configured wait), the
// handler asks the hub to request one (a PLI, on the real upstream
// session), so the printer's encoder is nudged to produce a fresh keyframe.
func TestHandleStream_NoKeyframeRequestsKeyframeViaHub(t *testing.T) {
	prevFrag, prevWait := streamFragmentDuration, streamMaxKeyframeWait
	streamFragmentDuration = time.Hour // never flush a fragment on its own
	streamMaxKeyframeWait = 5 * time.Millisecond
	t.Cleanup(func() { streamFragmentDuration = prevFrag; streamMaxKeyframeWait = prevWait })

	fixture := loadViewerFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	resp, cancel := getStreamResponse(t, srv, hub, "192.0.2.10", "/stream/k2.mp4?token="+token, fixture.aus[0])
	defer cancel()
	defer resp.Body.Close()
	go io.Copy(io.Discard, resp.Body)

	nonKey := fixture.aus[1]
	deadline := time.After(3 * time.Second)
	for i := 0; i < 200; i++ {
		au := nonKey
		au.PTS = fixture.aus[0].PTS + uint64(i+1)*100
		hub.feed("192.0.2.10", au)
		if hub.keyframeRequests("192.0.2.10") > 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("handler never asked the hub for a keyframe after ErrNoKeyframe")
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatal("handler never asked the hub for a keyframe after ErrNoKeyframe")
}

// TestHandleStream_NoKeyframeAsksOnceThenStopsUntilResolved confirms that
// once fmp4.ErrNoKeyframe fires, the handler asks the hub for a keyframe
// exactly once per gap and does not ask again on every subsequent
// non-keyframe access unit while the same gap is still open: since review
// backlog item 51 removed this handler's own per-consumer PLI retry loop
// (item 44's earlier fix), repeated retrying while a keyframe is still
// missing is now the hub's own responsibility (coalescing plus its own
// automatic recovery retry, internal/daemon/camera_hub_test.go), not
// something this handler, or the fakeViewerHub double it talks to here,
// is meant to do on its own any more.
func TestHandleStream_NoKeyframeAsksOnceThenStopsUntilResolved(t *testing.T) {
	prevFrag, prevWait := streamFragmentDuration, streamMaxKeyframeWait
	streamFragmentDuration = time.Hour // never flush a fragment on its own
	streamMaxKeyframeWait = 5 * time.Millisecond
	t.Cleanup(func() {
		streamFragmentDuration = prevFrag
		streamMaxKeyframeWait = prevWait
	})

	fixture := loadViewerFixture(t)
	if len(fixture.aus) < 2 {
		t.Fatalf("fixture needs at least 2 access units")
	}

	hub := newFakeViewerHub()
	srv, token := newTestViewerServer(t, hub, fakePrinterLister{printers: testViewerPrinters()})

	resp, cancel := getStreamResponse(t, srv, hub, "192.0.2.10", "/stream/k2.mp4?token="+token, fixture.aus[0])
	defer cancel()
	defer resp.Body.Close()
	go io.Copy(io.Discard, resp.Body)

	nonKey := fixture.aus[1]
	pts := fixture.aus[0].PTS + 100
	deadline := time.After(3 * time.Second)
	for hub.keyframeRequests("192.0.2.10") < 1 {
		au := nonKey
		au.PTS = pts
		pts += 100
		hub.feed("192.0.2.10", au)
		select {
		case <-deadline:
			t.Fatal("handler never asked the hub for a keyframe after ErrNoKeyframe")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Keep feeding non-keyframe access units into the same still-open gap:
	// the request count must not climb past 1 while nothing resolves it.
	for i := 0; i < 20; i++ {
		au := nonKey
		au.PTS = pts
		pts += 100
		hub.feed("192.0.2.10", au)
		time.Sleep(time.Millisecond)
	}
	if got := hub.keyframeRequests("192.0.2.10"); got != 1 {
		t.Fatalf("keyframe requests = %d while one gap stayed open, want exactly 1 (the handler must not retry on its own any more)", got)
	}

	// A real keyframe resumes writing; the handler must not ask again.
	kf := fixture.aus[0]
	kf.PTS = pts
	hub.feed("192.0.2.10", kf)
	time.Sleep(20 * time.Millisecond)
	if got := hub.keyframeRequests("192.0.2.10"); got != 1 {
		t.Errorf("PLI kept being requested after a keyframe resumed writing: got %d, want 1 (unchanged)", got)
	}
}
