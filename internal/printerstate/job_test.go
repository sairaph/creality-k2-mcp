package printerstate

import (
	"reflect"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/crealityws"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

func TestJobIdentityFrom_RealPrintingCapture(t *testing.T) {
	snap := printingSnapshot(t)
	job := JobIdentityFrom(snap)
	if job == nil {
		t.Fatal("JobIdentityFrom returned nil, want a job identity")
	}
	if job.Filename != "k2mcp_test.gcode" {
		t.Fatalf("Filename = %q, want %q", job.Filename, "k2mcp_test.gcode")
	}
	if job.UUID != "e091d41a-40fc-4ea7-86fc-93cdf76b9549" {
		t.Fatalf("UUID = %q, want the metadata uuid from control_test_20260928.log", job.UUID)
	}
	if job.StartTime != 1790554656.2828565 {
		t.Fatalf("StartTime = %v, want the cur_print_data start_time", job.StartTime)
	}
}

func TestJobIdentityFrom_NoJobKnown(t *testing.T) {
	snap := Snapshot{} // nothing decoded at all
	if job := JobIdentityFrom(snap); job != nil {
		t.Fatalf("JobIdentityFrom = %+v, want nil when nothing identifies a job", job)
	}
}

func TestJobIdentityFrom_FallsBackToCurPrintDataFilenameAfterJobEnds(t *testing.T) {
	// print_stats.filename goes empty once idle again, but cur_print_data
	// keeps the last job's identity (11-state-model.md section 5.2).
	snap := Snapshot{
		PrintStats: &moonraker.PrintStats{Filename: ""},
		VirtualSDCard: &moonraker.VirtualSDCard{
			CurPrintData: &moonraker.CurPrintData{
				Filename:  "last_job.gcode",
				StartTime: 100.0,
				Metadata:  &moonraker.CrealityPrintMetadata{UUID: "abc-123"},
			},
		},
	}
	job := JobIdentityFrom(snap)
	if job == nil {
		t.Fatal("JobIdentityFrom = nil, want the last job's identity")
	}
	if job.Filename != "last_job.gcode" || job.UUID != "abc-123" || job.StartTime != 100.0 {
		t.Fatalf("job = %+v, want filename/uuid/start_time from cur_print_data", job)
	}
}

func TestJobIdentityFrom_HistoryAndWs9999CrossChecks(t *testing.T) {
	snap := Snapshot{
		PrintStats: &moonraker.PrintStats{Filename: "f.gcode"},
		History: moonraker.HistoryList{Jobs: []moonraker.HistoryJob{
			{JobID: "000001", StartTime: 10},
			{JobID: "00000C", StartTime: 50}, // most recent: historyHead must pick this one
			{JobID: "000005", StartTime: 30},
		}},
		WS9999: crealityws.Status{PrintID: crealityws.Str{Value: "6ab97686faee9c1a19d178e6", Present: true}},
	}
	job := JobIdentityFrom(snap)
	if job.HistoryJobID != "00000C" {
		t.Fatalf("HistoryJobID = %q, want the job with the greatest start_time (00000C)", job.HistoryJobID)
	}
	if job.Ws9999PrintID != "6ab97686faee9c1a19d178e6" {
		t.Fatalf("Ws9999PrintID = %q, want the 9999 printId", job.Ws9999PrintID)
	}
}

func TestJobIdentityDiff(t *testing.T) {
	a := &JobIdentity{Filename: "a.gcode", UUID: "u1", StartTime: 1}
	b := &JobIdentity{Filename: "a.gcode", UUID: "u2", StartTime: 2}

	tests := []struct {
		name    string
		before  *JobIdentity
		after   *JobIdentity
		changed []string
	}{
		{"identical", a, &JobIdentity{Filename: "a.gcode", UUID: "u1", StartTime: 1}, nil},
		{"uuid and start_time changed", a, b, []string{"uuid", "start_time"}},
		{"both nil", nil, nil, nil},
		{"idle to printing", nil, a, []string{"job"}},
		{"printing to idle", a, nil, []string{"job"}},
		{"filename changed", a, &JobIdentity{Filename: "b.gcode", UUID: "u1", StartTime: 1}, []string{"filename"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := JobIdentityDiff(tc.before, tc.after)
			if !reflect.DeepEqual(got, tc.changed) {
				t.Fatalf("JobIdentityDiff = %v, want %v", got, tc.changed)
			}
		})
	}
}
