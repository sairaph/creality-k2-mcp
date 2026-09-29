package printerstate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

func TestTake_HappyPath_DecodesEveryObject(t *testing.T) {
	raw := idleObjectsRaw(t)
	deps := Deps{
		Moonraker: &fakeMoonrakerClient{
			serverInfo: idleServerInfo(),
			objects:    raw,
			history: moonraker.HistoryList{Jobs: []moonraker.HistoryJob{
				{JobID: "00000C", StartTime: 1},
			}},
			gcodeStore: []moonraker.GCodeStoreEntry{{Message: "// hello", Type: "response"}},
		},
		WS9999: &fakeWS9999Client{status: idleWS9999Status()},
	}

	snap := Take(context.Background(), deps, domain.Printer{ID: "k2-5885"})

	if snap.ServerInfoErr != nil {
		t.Fatalf("ServerInfoErr = %v, want nil", snap.ServerInfoErr)
	}
	if snap.ObjectsErr != nil {
		t.Fatalf("ObjectsErr = %v, want nil", snap.ObjectsErr)
	}
	if len(snap.DecodeErrs) != 0 {
		t.Fatalf("DecodeErrs = %v, want none", snap.DecodeErrs)
	}
	if snap.PrintStats == nil || snap.PrintStats.State != "standby" {
		t.Fatalf("PrintStats not decoded as expected: %+v", snap.PrintStats)
	}
	if snap.Box == nil || snap.Box.State == nil || *snap.Box.State != "disconnect" {
		t.Fatalf("Box not decoded as expected: %+v", snap.Box)
	}
	if !snap.WS9999Reachable {
		t.Fatal("WS9999Reachable = false, want true")
	}
	if snap.HistoryErr != nil || len(snap.History.Jobs) != 1 {
		t.Fatalf("History = %+v, err = %v", snap.History, snap.HistoryErr)
	}
	if snap.GCodeStoreErr != nil || len(snap.GCodeStoreTail) != 1 {
		t.Fatalf("GCodeStoreTail = %+v, err = %v", snap.GCodeStoreTail, snap.GCodeStoreErr)
	}
	if snap.Printer.ID != "k2-5885" {
		t.Fatalf("Printer.ID = %q, want k2-5885", snap.Printer.ID)
	}
	if snap.Taken.IsZero() {
		t.Fatal("Taken is zero, want a timestamp")
	}
}

// TestTake_RecordsPerSourceErrorsIndependently pins that a failure in one
// source (server/info) does not prevent the other sources (objects query,
// 9999, history, gcode_store) from being gathered and recorded, and that
// DeriveActivityState still reads the Snapshot as "offline" from
// ServerInfoErr alone (P1: reads are always allowed, the gather itself never
// fails).
func TestTake_RecordsPerSourceErrorsIndependently(t *testing.T) {
	wantErr := errors.New("connection refused")
	deps := Deps{
		Moonraker: &fakeMoonrakerClient{
			serverInfoErr: wantErr,
			objects:       idleObjectsRaw(t),
			history:       moonraker.HistoryList{},
			gcodeStore:    nil,
		},
		WS9999: &fakeWS9999Client{status: idleWS9999Status()},
	}

	snap := Take(context.Background(), deps, domain.Printer{})

	if !errors.Is(snap.ServerInfoErr, wantErr) {
		t.Fatalf("ServerInfoErr = %v, want %v", snap.ServerInfoErr, wantErr)
	}
	// The objects query and 9999 read must still have succeeded and been
	// decoded, independent of server/info's failure.
	if snap.PrintStats == nil {
		t.Fatal("PrintStats not decoded even though the objects query succeeded independently of server/info")
	}
	if !snap.WS9999Reachable {
		t.Fatal("WS9999Reachable = false, want true (independent of server/info's failure)")
	}

	d := DeriveActivityState(snap, nil)
	if d.State != StateOffline {
		t.Fatalf("State = %q, want %q", d.State, StateOffline)
	}
}

func TestTake_ObjectsQueryFailureLeavesEveryTypedFieldNil(t *testing.T) {
	deps := Deps{
		Moonraker: &fakeMoonrakerClient{
			serverInfo: idleServerInfo(),
			objectsErr: errors.New("boom"),
			objects:    nil,
		},
		WS9999: &fakeWS9999Client{status: idleWS9999Status()},
	}
	snap := Take(context.Background(), deps, domain.Printer{})
	if snap.ObjectsErr == nil {
		t.Fatal("ObjectsErr = nil, want an error")
	}
	if snap.PrintStats != nil || snap.Webhooks != nil || snap.Box != nil {
		t.Fatal("typed fields should stay nil when the objects query itself failed")
	}
	d := DeriveActivityState(snap, nil)
	if d.State != StateUnknown || d.Class != ClassUnknownFailClosed {
		t.Fatalf("state/class = %s/%s, want unknown/unknown_fail_closed", d.State, d.Class)
	}
}

func TestTake_MalformedObjectRecordsDecodeErrButLeavesOthersDecoded(t *testing.T) {
	raw := map[string]json.RawMessage{}
	for k, v := range idleObjectsRaw(t) {
		raw[k] = v
	}
	raw["print_stats"] = json.RawMessage(`{"state": 12345}`) // wrong type for a string field

	deps := Deps{
		Moonraker: &fakeMoonrakerClient{serverInfo: idleServerInfo(), objects: raw},
		WS9999:    &fakeWS9999Client{status: idleWS9999Status()},
	}
	snap := Take(context.Background(), deps, domain.Printer{})

	if snap.PrintStats != nil {
		t.Fatalf("PrintStats = %+v, want nil after a decode failure", snap.PrintStats)
	}
	if _, ok := snap.DecodeErrs["print_stats"]; !ok {
		t.Fatalf("DecodeErrs = %v, want an entry for print_stats", snap.DecodeErrs)
	}
	if snap.Webhooks == nil {
		t.Fatal("Webhooks should still decode even though print_stats failed")
	}
}

func TestTake_WS9999UnreachableIsRecordedAndBudgeted(t *testing.T) {
	start := time.Now()
	deps := Deps{
		Moonraker: &fakeMoonrakerClient{serverInfo: idleServerInfo(), objects: idleObjectsRaw(t)},
		WS9999:    &fakeWS9999Client{blocked: true},
	}
	snap := Take(context.Background(), deps, domain.Printer{})
	elapsed := time.Since(start)

	if snap.WS9999Reachable {
		t.Fatal("WS9999Reachable = true, want false")
	}
	if snap.WS9999Err == nil {
		t.Fatal("WS9999Err = nil, want a context-deadline error")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("Take took %v, want it to respect the ~3s ws9999Budget rather than hang", elapsed)
	}

	// A printer that is otherwise fully idle over Moonraker must still
	// derive as idle: 9999 being unreachable must not fail-close an
	// otherwise Moonraker-answerable decision (11-state-model.md 2.3).
	d := DeriveActivityState(snap, nil)
	if d.State != StateIdle {
		t.Fatalf("State = %q, want %q", d.State, StateIdle)
	}
}

func TestTake_RunsSourcesConcurrentlyNotSequentially(t *testing.T) {
	const perCallDelay = 200 * time.Millisecond
	deps := Deps{
		Moonraker: &fakeMoonrakerClient{
			serverInfo: idleServerInfo(),
			objects:    idleObjectsRaw(t),
			delay:      perCallDelay,
		},
		WS9999: &fakeWS9999Client{status: idleWS9999Status(), delay: perCallDelay},
	}

	start := time.Now()
	Take(context.Background(), deps, domain.Printer{})
	elapsed := time.Since(start)

	// Five sources gathered sequentially would take >= 5*perCallDelay
	// (ServerInfo, QueryObjects, HistoryList and GCodeStore all share the
	// same fake, each sleeping perCallDelay; ws9999 sleeps it once more).
	// Concurrently, the whole gather should finish in well under half that.
	if elapsed >= 3*perCallDelay {
		t.Fatalf("Take took %v, want well under %v if its five sources run concurrently", elapsed, 3*perCallDelay)
	}
}

func TestTake_HistoryHeadPicksGreatestStartTime(t *testing.T) {
	raw := idleObjectsRaw(t)
	deps := Deps{
		Moonraker: &fakeMoonrakerClient{
			serverInfo: idleServerInfo(),
			objects:    raw,
			history: moonraker.HistoryList{Jobs: []moonraker.HistoryJob{
				{JobID: "older", StartTime: 5},
				{JobID: "newest", StartTime: 999},
				{JobID: "middle", StartTime: 100},
			}},
		},
		WS9999: &fakeWS9999Client{status: idleWS9999Status()},
	}
	snap := Take(context.Background(), deps, domain.Printer{})
	job, ok := historyHead(snap.History)
	if !ok || job.JobID != "newest" {
		t.Fatalf("historyHead = %+v, ok=%v, want job_id=newest", job, ok)
	}
}

// Snapshot.Taken must carry the monotonic clock reading (no .UTC(), .Round() or
// serialisation), because every in-flight record (start, pause, resume, pending
// action) compares against it with time.Now() values: a wall-clock step (WSL2
// drift, NTP) must not change what "before" or "after" means. The state block
// still renders it in UTC.
func TestTakenKeepsTheMonotonicReading(t *testing.T) {
	deps := Deps{
		Moonraker: &fakeMoonrakerClient{serverInfo: idleServerInfo(), objects: idleObjectsRaw(t)},
		WS9999:    &fakeWS9999Client{status: idleWS9999Status()},
	}
	before := time.Now()
	snap := Take(context.Background(), deps, domain.Printer{})
	if !strings.Contains(snap.Taken.String(), "m=") {
		t.Fatalf("Taken = %q carries no monotonic reading", snap.Taken.String())
	}
	if snap.Taken.Before(before) {
		t.Fatalf("Taken %v is before the call started %v", snap.Taken, before)
	}
	block := BuildStateBlock(snap, Derived{}, nil)
	if !strings.HasSuffix(block.SnapshotTime, "Z") {
		t.Errorf("snapshot_time = %q, want UTC", block.SnapshotTime)
	}
}
