package moonraker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHistoryListDecodesRealCapture(t *testing.T) {
	var gotLimit, gotStart string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		gotStart = r.URL.Query().Get("start")
		w.Write(readFixture(t, "server_history_list_limit_10.json"))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	list, err := c.HistoryList(context.Background(), 10, 0)
	if err != nil {
		t.Fatalf("HistoryList: %v", err)
	}
	if gotLimit != "10" || gotStart != "0" {
		t.Fatalf("query = limit=%s start=%s, want limit=10 start=0", gotLimit, gotStart)
	}
	if list.Count != 12 {
		t.Fatalf("Count = %d, want 12", list.Count)
	}
	if len(list.Jobs) == 0 {
		t.Fatal("Jobs is empty")
	}
	if list.Jobs[0].Status != "completed" && list.Jobs[0].Status != "cancelled" {
		t.Fatalf("Jobs[0].Status = %q, want completed or cancelled", list.Jobs[0].Status)
	}
}

func TestHistoryTotalsDecodesRealCaptureAndToleratesMissingAuxiliary(t *testing.T) {
	ts := fixtureServer(t, readFixture(t, "server_history_totals.json"))
	c := New(ts.URL, "")
	totals, err := c.HistoryTotals(context.Background())
	if err != nil {
		t.Fatalf("HistoryTotals: %v", err)
	}
	if totals.JobTotals.TotalJobs != 12 {
		t.Fatalf("TotalJobs = %d, want 12", totals.JobTotals.TotalJobs)
	}
	if totals.AuxiliaryTotals != nil {
		t.Fatalf("AuxiliaryTotals = %v, want nil (this fork omits the key entirely)", totals.AuxiliaryTotals)
	}
}

func TestGCodeStoreDecodesRealCapture(t *testing.T) {
	var gotCount string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCount = r.URL.Query().Get("count")
		w.Write(readFixture(t, "server_gcode_store.json"))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	entries, err := c.GCodeStore(context.Background(), 20)
	if err != nil {
		t.Fatalf("GCodeStore: %v", err)
	}
	if gotCount != "20" {
		t.Fatalf("count query param = %q, want %q", gotCount, "20")
	}
	if len(entries) != 20 {
		t.Fatalf("len(entries) = %d, want 20", len(entries))
	}
	if entries[0].Type != "response" {
		t.Fatalf("entries[0].Type = %q, want %q", entries[0].Type, "response")
	}
}

func TestGCodeStoreOmitsCountWhenNotPositive(t *testing.T) {
	var sawCount bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawCount = r.URL.Query()["count"]
		w.Write([]byte(`{"result": {"gcode_store": []}}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	if _, err := c.GCodeStore(context.Background(), 0); err != nil {
		t.Fatalf("GCodeStore: %v", err)
	}
	if sawCount {
		t.Fatal("count query param should be omitted when count <= 0")
	}
}

func TestJobQueueStatusDecodesRealCapture(t *testing.T) {
	ts := fixtureServer(t, readFixture(t, "server_job_queue_status.json"))
	c := New(ts.URL, "")
	status, err := c.JobQueueStatus(context.Background())
	if err != nil {
		t.Fatalf("JobQueueStatus: %v", err)
	}
	if status.QueueState != "paused" {
		t.Fatalf("QueueState = %q, want %q", status.QueueState, "paused")
	}
	if len(status.QueuedJobs) != 0 {
		t.Fatalf("QueuedJobs = %v, want empty", status.QueuedJobs)
	}
}
