package printerstate

// JobIdentity is the cross-checked job identity from
// references/analysis/11-state-model.md section 5.2 and section 6:
// (filename, metadata uuid, start_time) as the primary triple, plus the
// history job_id and 9999 printId as independent cross-checks. Two
// consecutive snapshots must agree on the primary triple before a
// pause/resume/cancel write proceeds (section 5.3); JobIdentityDiff is how a
// caller checks that.
type JobIdentity struct {
	Filename      string  `yaml:"filename"`
	UUID          string  `yaml:"uuid,omitempty"`
	StartTime     float64 `yaml:"start_time,omitempty"`
	HistoryJobID  string  `yaml:"history_job_id,omitempty"`
	Ws9999PrintID string  `yaml:"ws9999_print_id,omitempty"`
}

// JobIdentityFrom builds a JobIdentity from a Snapshot, or returns nil when
// no job identity is known at all (no filename from either print_stats or
// the last cur_print_data). Filename prefers print_stats.filename (the live,
// currently-loaded job) and falls back to virtual_sdcard.cur_print_data's own
// Filename, which stays populated after a job ends
// (11-state-model.md section 5.2); UUID and StartTime always come from
// cur_print_data.metadata/cur_print_data, since print_stats carries no
// per-file-version or per-job-instance identity of its own.
func JobIdentityFrom(snap Snapshot) *JobIdentity {
	var filename, uuid, historyJobID, printID string
	var startTime float64

	if snap.PrintStats != nil && snap.PrintStats.Filename != "" {
		filename = snap.PrintStats.Filename
	}

	if snap.VirtualSDCard != nil && snap.VirtualSDCard.CurPrintData != nil {
		cur := snap.VirtualSDCard.CurPrintData
		if filename == "" {
			filename = cur.Filename
		}
		startTime = cur.StartTime
		if cur.Metadata != nil {
			uuid = cur.Metadata.UUID
		}
	}

	if job, ok := historyHead(snap.History); ok {
		historyJobID = job.JobID
	}

	if snap.WS9999.PrintID.Present {
		printID = snap.WS9999.PrintID.Value
	}

	if filename == "" && uuid == "" && historyJobID == "" && printID == "" {
		return nil
	}

	return &JobIdentity{
		Filename:      filename,
		UUID:          uuid,
		StartTime:     startTime,
		HistoryJobID:  historyJobID,
		Ws9999PrintID: printID,
	}
}

// JobIdentityDiff lists which fields differ between before and after, using
// the names a caller should surface to explain a conflict
// (11-state-model.md section 5.3: "the job changed under us between the
// check and the write"). A nil before or after is reported as the single
// field "job" changing (no job -> a job, or a job -> no job), since there is
// nothing more specific to compare.
func JobIdentityDiff(before, after *JobIdentity) []string {
	if before == nil && after == nil {
		return nil
	}
	if before == nil || after == nil {
		return []string{"job"}
	}
	var changed []string
	if before.Filename != after.Filename {
		changed = append(changed, "filename")
	}
	if before.UUID != after.UUID {
		changed = append(changed, "uuid")
	}
	if before.StartTime != after.StartTime {
		changed = append(changed, "start_time")
	}
	if before.HistoryJobID != after.HistoryJobID {
		changed = append(changed, "history_job_id")
	}
	if before.Ws9999PrintID != after.Ws9999PrintID {
		changed = append(changed, "ws9999_print_id")
	}
	return changed
}
