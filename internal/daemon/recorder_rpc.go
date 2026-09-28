package daemon

import "time"

// The recording.* JSON-RPC methods Server registers on its socket
// (T11c), alongside the ping/watchdog/viewer methods in rpc_types.go.
// internal/daemon/client is the sole production caller; tests dial the
// socket directly.
const (
	MethodRecordingStart  = "recording.start"
	MethodRecordingStop   = "recording.stop"
	MethodRecordingList   = "recording.list"
	MethodRecordingDelete = "recording.delete"
)

// RecordingStartParams is MethodRecordingStart's request: the wire shape of
// StartRecordingRequest. Identity is only required when Until is
// "print_end". MaxDurationSeconds of zero uses recorderDefaultMaxDuration
// (12 hours).
type RecordingStartParams struct {
	PrinterID          string `json:"printer_id"`
	Host               string `json:"host"`
	Identity           string `json:"identity,omitempty"`
	Mode               string `json:"mode"`
	Until              string `json:"until"`
	MaxDurationSeconds int    `json:"max_duration_seconds,omitempty"`
}

// RecordingStartResult answers MethodRecordingStart.
type RecordingStartResult struct {
	Recording RecordingInfo `json:"recording"`
}

// RecordingStopParams is MethodRecordingStop's request.
type RecordingStopParams struct {
	ID string `json:"id"`
}

// RecordingStopResult answers MethodRecordingStop.
type RecordingStopResult struct {
	Recording RecordingInfo `json:"recording"`
}

// RecordingListParams is MethodRecordingList's request (currently empty).
type RecordingListParams struct{}

// RecordingListResult answers MethodRecordingList.
type RecordingListResult struct {
	Recordings     []RecordingInfo `json:"recordings"`
	DiskUsageBytes int64           `json:"disk_usage_bytes"`
}

// RecordingDeleteParams is MethodRecordingDelete's request.
type RecordingDeleteParams struct {
	ID string `json:"id"`
}

// RecordingDeleteResult answers MethodRecordingDelete.
type RecordingDeleteResult struct {
	OK bool `json:"ok"`
}

// maxDurationFrom converts RecordingStartParams.MaxDurationSeconds into a
// time.Duration, 0 meaning "use the recorder's own default".
func maxDurationFrom(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
