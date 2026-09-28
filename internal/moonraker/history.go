package moonraker

import (
	"context"
	"net/url"
	"strconv"
)

// HistoryJob is one job record from server/history/list. Its embedded
// metadata is the thin server/files/metadata-style shape (no thumbnails or
// estimated_time), not the richer CrealityPrintMetadata
// (02-moonraker-api.md section 6).
type HistoryJob struct {
	JobID         string       `json:"job_id"`
	Exists        bool         `json:"exists"`
	Filename      string       `json:"filename"`
	Status        string       `json:"status"`
	StartTime     float64      `json:"start_time"`
	EndTime       float64      `json:"end_time"`
	PrintDuration float64      `json:"print_duration"`
	TotalDuration float64      `json:"total_duration"`
	FilamentUsed  float64      `json:"filament_used"`
	Metadata      FileMetadata `json:"metadata"`
}

// HistoryList is a page of job history.
type HistoryList struct {
	Count int          `json:"count"`
	Jobs  []HistoryJob `json:"jobs"`
}

// HistoryList fetches up to limit job records starting at start (both
// Moonraker's own pagination parameters; start is 0-based).
func (c *Client) HistoryList(ctx context.Context, limit, start int) (HistoryList, error) {
	query := url.Values{
		"limit": {strconv.Itoa(limit)},
		"start": {strconv.Itoa(start)},
	}
	var out HistoryList
	err := c.get(ctx, "HistoryList", "/server/history/list", query, timeoutStatus, &out)
	return out, err
}

// JobTotals is the lifetime job totals block of server/history/totals.
type JobTotals struct {
	TotalJobs         int     `json:"total_jobs"`
	TotalTime         float64 `json:"total_time"`
	TotalPrintTime    float64 `json:"total_print_time"`
	TotalFilamentUsed float64 `json:"total_filament_used"`
	LongestJob        float64 `json:"longest_job"`
	LongestPrint      float64 `json:"longest_print"`
}

// AuxiliaryTotal is one auxiliary sensor/spoolman aggregate. This fork has
// neither component configured, so auxiliary_totals is normally absent
// entirely rather than an empty array (02-moonraker-api.md section 6),
// hence the omitempty-tolerant nil default here.
type AuxiliaryTotal struct {
	Provider string  `json:"provider"`
	Field    string  `json:"field"`
	Maximum  float64 `json:"maximum"`
	Total    float64 `json:"total"`
}

// HistoryTotals is the response to server/history/totals.
type HistoryTotals struct {
	JobTotals       JobTotals        `json:"job_totals"`
	AuxiliaryTotals []AuxiliaryTotal `json:"auxiliary_totals"`
}

// HistoryTotals fetches the lifetime job totals.
func (c *Client) HistoryTotals(ctx context.Context) (HistoryTotals, error) {
	var out HistoryTotals
	err := c.get(ctx, "HistoryTotals", "/server/history/totals", nil, timeoutStatus, &out)
	return out, err
}

// GCodeStoreEntry is one cached console line from server/gcode_store.
type GCodeStoreEntry struct {
	Message string  `json:"message"`
	Time    float64 `json:"time"`
	Type    string  `json:"type"`
}

// GCodeStore fetches up to count cached console lines, newest last (the
// order Moonraker itself returns them in). count <= 0 asks Moonraker for
// its default (the full configured store).
func (c *Client) GCodeStore(ctx context.Context, count int) ([]GCodeStoreEntry, error) {
	query := url.Values{}
	if count > 0 {
		query.Set("count", strconv.Itoa(count))
	}
	var out struct {
		GCodeStore []GCodeStoreEntry `json:"gcode_store"`
	}
	err := c.get(ctx, "GCodeStore", "/server/gcode_store", query, timeoutStatus, &out)
	return out.GCodeStore, err
}

// JobQueueStatus is the response to server/job_queue/status.
type JobQueueStatus struct {
	QueuedJobs []QueuedJob `json:"queued_jobs"`
	QueueState string      `json:"queue_state"`
}

// QueuedJob is one entry in the job queue.
type QueuedJob struct {
	Filename    string  `json:"filename"`
	JobID       string  `json:"job_id"`
	TimeAdded   float64 `json:"time_added"`
	TimeInQueue float64 `json:"time_in_queue"`
}

// JobQueueStatus fetches the current job queue state. This project never
// enqueues jobs (job_queue.automatic_transition is false on this printer
// and no job_queue write method exists here); this read exists so a status
// tool can report the queue if the person has used it through another
// client.
func (c *Client) JobQueueStatus(ctx context.Context) (JobQueueStatus, error) {
	var out JobQueueStatus
	err := c.get(ctx, "JobQueueStatus", "/server/job_queue/status", nil, timeoutStatus, &out)
	return out, err
}
