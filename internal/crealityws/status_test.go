package crealityws

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestBuildStatus_PartialSafetyFieldsListedAsMissing exercises buildStatus
// directly with a raw map that has some, but not all, safety-relevant fields
// (dev_docs/safety-architecture.md P1: presence must be part of the type for
// every one of them, not just the all-or-nothing cases the fixture-based
// tests happen to cover).
func TestBuildStatus_PartialSafetyFieldsListedAsMissing(t *testing.T) {
	raw := map[string]any{
		"state":         1,
		"upgradeStatus": 0,
		"lightSw":       1,
		// deviceState, feedState, repoPlrStatus, powerLoss, materialStatus,
		// cfsConnect, err and printId are all absent.
	}
	got := buildStatus(raw)

	if got.State != (Int{Value: 1, Present: true}) {
		t.Errorf("State = %+v, want {1 true}", got.State)
	}
	if got.UpgradeStatus != (Int{Value: 0, Present: true}) {
		t.Errorf("UpgradeStatus = %+v, want {0 true} (a present, genuine zero)", got.UpgradeStatus)
	}
	if got.CfsConnect.Present {
		t.Errorf("CfsConnect.Present = true, want false (absent from raw)")
	}
	if got.Err.Present {
		t.Errorf("Err.Present = true, want false (absent from raw)")
	}
	if got.PrintID.Present {
		t.Errorf("PrintID.Present = true, want false (absent from raw)")
	}

	want := []string{
		"deviceState", "feedState", "repoPlrStatus", "powerLoss",
		"materialStatus", "cfsConnect", "err", "printId",
	}
	gotMissing := got.MissingSafetyFields()
	if len(gotMissing) != len(want) {
		t.Fatalf("MissingSafetyFields() = %v, want %v", gotMissing, want)
	}
	for i, name := range want {
		if gotMissing[i] != name {
			t.Errorf("MissingSafetyFields()[%d] = %q, want %q", i, gotMissing[i], name)
		}
	}
}

// TestBuildStatus_UnparsableSafetyFieldsTreatedAsAbsent exercises buildStatus
// with safety-relevant fields that are present in the raw frame but not in a
// shape asOptInt/asStatusErr can decode: a non-numeric string, a JSON object,
// and a JSON array (decoded by json.Unmarshal into map[string]any as
// []any). dev_docs/safety-architecture.md P1 requires these be treated
// exactly like an absent field (Present false, listed by
// MissingSafetyFields), never guessed forward as a genuine zero.
func TestBuildStatus_UnparsableSafetyFieldsTreatedAsAbsent(t *testing.T) {
	raw := map[string]any{
		"state":          "not-a-number",         // non-numeric string
		"deviceState":    map[string]any{"x": 1}, // object
		"feedState":      []any{1, 2, 3},         // array
		"upgradeStatus":  0,                      // present, genuine zero (control)
		"repoPlrStatus":  "also-not-a-number",    // non-numeric string
		"powerLoss":      map[string]any{},       // empty object
		"materialStatus": []any{},                // empty array
		"cfsConnect":     1,                      // present (control)
		"lightSw":        1,                      // present (control)
		// err and printId absent entirely, for comparison.
	}
	got := buildStatus(raw)

	if got.State.Present {
		t.Errorf("State.Present = true for a non-numeric string, want false")
	}
	if got.DeviceState.Present {
		t.Errorf("DeviceState.Present = true for an object, want false")
	}
	if got.FeedState.Present {
		t.Errorf("FeedState.Present = true for an array, want false")
	}
	if got.RepoPlrStatus.Present {
		t.Errorf("RepoPlrStatus.Present = true for a non-numeric string, want false")
	}
	if got.PowerLoss.Present {
		t.Errorf("PowerLoss.Present = true for an empty object, want false")
	}
	if got.MaterialStatus.Present {
		t.Errorf("MaterialStatus.Present = true for an empty array, want false")
	}
	if !got.UpgradeStatus.Present || got.UpgradeStatus.Value != 0 {
		t.Errorf("UpgradeStatus = %+v, want {0 true} (a present, genuine zero, control case)", got.UpgradeStatus)
	}
	if !got.CfsConnect.Present {
		t.Errorf("CfsConnect.Present = false, want true (control case)")
	}
	if !got.LightSw.Present {
		t.Errorf("LightSw.Present = false, want true (control case)")
	}

	want := []string{
		"state", "deviceState", "feedState", "repoPlrStatus",
		"powerLoss", "materialStatus", "err", "printId",
	}
	gotMissing := got.MissingSafetyFields()
	if len(gotMissing) != len(want) {
		t.Fatalf("MissingSafetyFields() = %v, want %v", gotMissing, want)
	}
	for i, name := range want {
		if gotMissing[i] != name {
			t.Errorf("MissingSafetyFields()[%d] = %q, want %q", i, gotMissing[i], name)
		}
	}
}

// TestBuildStatus_UnparsableErrTreatedAsAbsent covers the same rule as
// TestBuildStatus_UnparsableSafetyFieldsTreatedAsAbsent for the "err" field
// specifically, since it decodes through asStatusErr rather than asOptInt/
// asOptString and normally accepts either an object or a bare int (see
// asStatusErr's doc comment): a shape that is neither (a non-numeric string,
// or an array) must still come out absent.
func TestBuildStatus_UnparsableErrTreatedAsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  any
	}{
		{"non-numeric string", "boom"},
		{"array", []any{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := map[string]any{"err": tc.val}
			got := buildStatus(raw)
			if got.Err.Present {
				t.Errorf("Err.Present = true for %s %#v, want false", tc.name, tc.val)
			}
			missing := got.MissingSafetyFields()
			found := false
			for _, m := range missing {
				if m == "err" {
					found = true
				}
			}
			if !found {
				t.Errorf("MissingSafetyFields() = %v, want it to include %q", missing, "err")
			}
		})
	}
}

func TestReadStatus_IdleRealCapture(t *testing.T) {
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		if err := conn.Write(context.Background(), websocket.MessageText, idleFullPushJSON); err != nil {
			t.Errorf("fake server write idle push: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := client.ReadStatus(ctx)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}

	want := Status{
		State:           Int{Value: 0, Present: true},
		DeviceState:     Int{Value: 0, Present: true},
		FeedState:       Int{Value: 0, Present: true},
		UpgradeStatus:   Int{Value: 0, Present: true},
		RepoPlrStatus:   Int{Value: 0, Present: true},
		PowerLoss:       Int{Value: 1, Present: true},
		MaterialStatus:  Int{Value: 0, Present: true},
		CfsConnect:      Int{Value: 0, Present: true},
		LightSw:         Int{Value: 0, Present: true},
		Err:             StatusErr{ErrCode: 0, Key: 0, Value: "", Present: true},
		PrintID:         Str{Value: "6ab97686faee9c1a19d178e6", Present: true},
		Model:           "F021",
		Hostname:        "K2-5885",
		WebrtcSupport:   1,
		Video:           1,
		ModelFanPct:     0,
		CaseFanPct:      0,
		AuxiliaryFanPct: 0,
		CurFeedratePct:  100,
		CurFlowratePct:  100,
		PrintProgress:   100,
		PrintFileName:   "/mnt/UDISK/printer_data/gcodes/FigureJoints5mmball.stl_PLA_41m51s.gcode",
	}
	assertStatusFields(t, got, want)

	if got.Raw == nil || got.Raw["nozzleTemp"] != "29.360000" {
		t.Errorf("Raw missing decimal-string field nozzleTemp: %#v", got.Raw["nozzleTemp"])
	}

	if missing := got.MissingSafetyFields(); len(missing) != 0 {
		t.Errorf("MissingSafetyFields() = %v, want none for a full first push", missing)
	}
}

func TestReadStatus_PrintingRealExcerpt(t *testing.T) {
	frame := printingPushFields(t)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		if err := conn.Write(context.Background(), websocket.MessageText, frame); err != nil {
			t.Errorf("fake server write printing push: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := client.ReadStatus(ctx)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.State != (Int{Value: 1, Present: true}) {
		t.Errorf("State = %+v, want {1 true} (printing)", got.State)
	}
	if got.DeviceState != (Int{Value: 1, Present: true}) {
		t.Errorf("DeviceState = %+v, want {1 true}", got.DeviceState)
	}
	if got.PrintProgress != 9 {
		t.Errorf("PrintProgress = %d, want 9", got.PrintProgress)
	}
	if got.PrintFileName != "/mnt/UDISK/printer_data/gcodes/k2mcp_test.gcode" {
		t.Errorf("PrintFileName = %q", got.PrintFileName)
	}
	if missing := got.MissingSafetyFields(); len(missing) != 0 {
		t.Errorf("MissingSafetyFields() = %v, want none (built on a full push)", missing)
	}
}

func TestReadStatus_PausedRealExcerpt(t *testing.T) {
	frame := pausedPushFields(t)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		if err := conn.Write(context.Background(), websocket.MessageText, frame); err != nil {
			t.Errorf("fake server write paused push: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := client.ReadStatus(ctx)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.State != (Int{Value: 5, Present: true}) {
		t.Errorf("State = %+v, want {5 true} (paused)", got.State)
	}
	if got.DeviceState != (Int{Value: 1, Present: true}) {
		t.Errorf("DeviceState = %+v, want {1 true}", got.DeviceState)
	}
	if got.PrintProgress != 14 {
		t.Errorf("PrintProgress = %d, want 14", got.PrintProgress)
	}
	if missing := got.MissingSafetyFields(); len(missing) != 0 {
		t.Errorf("MissingSafetyFields() = %v, want none (built on a full push)", missing)
	}
}

func TestReadStatus_CancelledRealExcerpt(t *testing.T) {
	frame := cancelledPushFields(t)
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		if err := conn.Write(context.Background(), websocket.MessageText, frame); err != nil {
			t.Errorf("fake server write cancelled push: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := client.ReadStatus(ctx)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.State != (Int{Value: 4, Present: true}) {
		t.Errorf("State = %+v, want {4 true} (cancelled)", got.State)
	}
	if got.DeviceState != (Int{Value: 0, Present: true}) {
		t.Errorf("DeviceState = %+v, want {0 true}", got.DeviceState)
	}
	if got.PrintProgress != 14 {
		t.Errorf("PrintProgress = %d, want 14", got.PrintProgress)
	}
	if missing := got.MissingSafetyFields(); len(missing) != 0 {
		t.Errorf("MissingSafetyFields() = %v, want none (built on a full push)", missing)
	}
}

func TestReadStatus_MergesAcrossFrames(t *testing.T) {
	// A minimal second frame carrying only the identifying key fields, none
	// of which overlap tempDeltaJSON's fields, so a passing test proves
	// fields accumulate across frames rather than only the last frame
	// surviving.
	idFrame := []byte(`{"state":0,"deviceState":0,"model":"F021","hostname":"K2-5885"}`)

	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		// Real partial delta first (no identifying fields), then a frame
		// with only the identifying fields, mirroring how this protocol
		// interleaves telemetry across many small pushes.
		if err := conn.Write(ctx, websocket.MessageText, tempDeltaJSON); err != nil {
			t.Errorf("fake server write temp delta: %v", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, idFrame); err != nil {
			t.Errorf("fake server write id frame: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := client.ReadStatus(ctx)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.Hostname != "K2-5885" || got.Model != "F021" {
		t.Errorf("id frame fields not merged: hostname=%q model=%q", got.Hostname, got.Model)
	}
	if got.Raw["nozzleTemp"] != "29.110000" {
		t.Errorf("earlier delta field not retained in Raw after merging a later frame: %#v", got.Raw["nozzleTemp"])
	}
}

func TestReadStatus_GarbageFramesSkipped(t *testing.T) {
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		if err := conn.Write(ctx, websocket.MessageText, []byte("not json at all")); err != nil {
			t.Errorf("fake server write garbage text: %v", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte("[1,2,3]")); err != nil {
			t.Errorf("fake server write garbage array: %v", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageBinary, []byte{0xff, 0x00, 0xfe}); err != nil {
			t.Errorf("fake server write garbage binary: %v", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, idleFullPushJSON); err != nil {
			t.Errorf("fake server write idle push: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := client.ReadStatus(ctx)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.Hostname != "K2-5885" {
		t.Errorf("Hostname = %q, want K2-5885 (garbage frames should be skipped, not fatal)", got.Hostname)
	}
}

func TestReadStatus_HeartbeatFramesSkipped(t *testing.T) {
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		if err := conn.Write(ctx, websocket.MessageText, []byte("ok")); err != nil {
			t.Errorf("fake server write heartbeat: %v", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, idleFullPushJSON); err != nil {
			t.Errorf("fake server write idle push: %v", err)
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte("ok")); err != nil {
			t.Errorf("fake server write trailing heartbeat: %v", err)
		}
	})

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got, err := client.ReadStatus(ctx)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.Hostname != "K2-5885" {
		t.Errorf("Hostname = %q, want K2-5885 (heartbeat frames should be skipped)", got.Hostname)
	}
}

func TestReadStatus_MissingFieldsTimesOutWithPartialStatus(t *testing.T) {
	done := make(chan struct{})
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		// Only ever sends a partial delta with none of the identifying
		// fields (a real capture line), so ReadStatus must time out rather
		// than hang, and still return what it has.
		if err := conn.Write(ctx, websocket.MessageText, tempDeltaJSON); err != nil {
			t.Errorf("fake server write temp delta: %v", err)
		}
		<-done
	})
	defer close(done)

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), StatusReadTimeout+2*time.Second)
	defer cancel()

	start := time.Now()
	got, err := client.ReadStatus(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if elapsed < StatusReadTimeout {
		t.Errorf("ReadStatus returned after %v, want at least the %v merge budget", elapsed, StatusReadTimeout)
	}
	if got.Hostname != "" || got.Model != "" || got.State.Present {
		t.Errorf("expected zero-value identity fields for a status that never saw them, got %+v", got)
	}
	if got.Raw["nozzleTemp"] != "29.110000" {
		t.Errorf("expected the one delta that was received to still be merged into Raw, got %#v", got.Raw["nozzleTemp"])
	}

	// tempDeltaJSON carries none of the safety-relevant fields, so all of
	// them must come back absent and listed, in MissingSafetyFields' fixed
	// order (dev_docs/safety-architecture.md P1: fail closed, never guess a
	// never-seen field forward as its zero value).
	wantMissing := []string{
		"state", "deviceState", "feedState", "upgradeStatus", "repoPlrStatus",
		"powerLoss", "materialStatus", "cfsConnect", "lightSw", "err", "printId",
	}
	gotMissing := got.MissingSafetyFields()
	if len(gotMissing) != len(wantMissing) {
		t.Fatalf("MissingSafetyFields() = %v, want %v", gotMissing, wantMissing)
	}
	for i, name := range wantMissing {
		if gotMissing[i] != name {
			t.Errorf("MissingSafetyFields()[%d] = %q, want %q", i, gotMissing[i], name)
		}
	}
}

func TestReadStatus_NoUsableFrameEver(t *testing.T) {
	done := make(chan struct{})
	host, port := startFakeServer(t, func(t *testing.T, conn *websocket.Conn) {
		ctx := context.Background()
		if err := conn.Write(ctx, websocket.MessageText, []byte("garbage only")); err != nil {
			t.Errorf("fake server write garbage: %v", err)
		}
		<-done
	})
	defer close(done)

	client := New(host, port)
	ctx, cancel := context.WithTimeout(context.Background(), StatusReadTimeout+2*time.Second)
	defer cancel()

	_, err := client.ReadStatus(ctx)
	if err == nil {
		t.Fatal("ReadStatus: want error when no usable frame is ever received, got nil")
	}
}

func assertStatusFields(t *testing.T, got, want Status) {
	t.Helper()
	if got.State != want.State {
		t.Errorf("State = %+v, want %+v", got.State, want.State)
	}
	if got.DeviceState != want.DeviceState {
		t.Errorf("DeviceState = %+v, want %+v", got.DeviceState, want.DeviceState)
	}
	if got.FeedState != want.FeedState {
		t.Errorf("FeedState = %+v, want %+v", got.FeedState, want.FeedState)
	}
	if got.UpgradeStatus != want.UpgradeStatus {
		t.Errorf("UpgradeStatus = %+v, want %+v", got.UpgradeStatus, want.UpgradeStatus)
	}
	if got.RepoPlrStatus != want.RepoPlrStatus {
		t.Errorf("RepoPlrStatus = %+v, want %+v", got.RepoPlrStatus, want.RepoPlrStatus)
	}
	if got.PowerLoss != want.PowerLoss {
		t.Errorf("PowerLoss = %+v, want %+v", got.PowerLoss, want.PowerLoss)
	}
	if got.MaterialStatus != want.MaterialStatus {
		t.Errorf("MaterialStatus = %+v, want %+v", got.MaterialStatus, want.MaterialStatus)
	}
	if got.CfsConnect != want.CfsConnect {
		t.Errorf("CfsConnect = %+v, want %+v", got.CfsConnect, want.CfsConnect)
	}
	if got.LightSw != want.LightSw {
		t.Errorf("LightSw = %+v, want %+v", got.LightSw, want.LightSw)
	}
	if got.Err != want.Err {
		t.Errorf("Err = %+v, want %+v", got.Err, want.Err)
	}
	if got.PrintID != want.PrintID {
		t.Errorf("PrintID = %+v, want %+v", got.PrintID, want.PrintID)
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if got.Hostname != want.Hostname {
		t.Errorf("Hostname = %q, want %q", got.Hostname, want.Hostname)
	}
	if got.WebrtcSupport != want.WebrtcSupport {
		t.Errorf("WebrtcSupport = %d, want %d", got.WebrtcSupport, want.WebrtcSupport)
	}
	if got.Video != want.Video {
		t.Errorf("Video = %d, want %d", got.Video, want.Video)
	}
	if got.ModelFanPct != want.ModelFanPct {
		t.Errorf("ModelFanPct = %d, want %d", got.ModelFanPct, want.ModelFanPct)
	}
	if got.CaseFanPct != want.CaseFanPct {
		t.Errorf("CaseFanPct = %d, want %d", got.CaseFanPct, want.CaseFanPct)
	}
	if got.AuxiliaryFanPct != want.AuxiliaryFanPct {
		t.Errorf("AuxiliaryFanPct = %d, want %d", got.AuxiliaryFanPct, want.AuxiliaryFanPct)
	}
	if got.CurFeedratePct != want.CurFeedratePct {
		t.Errorf("CurFeedratePct = %d, want %d", got.CurFeedratePct, want.CurFeedratePct)
	}
	if got.CurFlowratePct != want.CurFlowratePct {
		t.Errorf("CurFlowratePct = %d, want %d", got.CurFlowratePct, want.CurFlowratePct)
	}
	if got.PrintProgress != want.PrintProgress {
		t.Errorf("PrintProgress = %d, want %d", got.PrintProgress, want.PrintProgress)
	}
	if got.PrintFileName != want.PrintFileName {
		t.Errorf("PrintFileName = %q, want %q", got.PrintFileName, want.PrintFileName)
	}
}
