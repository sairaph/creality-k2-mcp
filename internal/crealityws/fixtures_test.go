package crealityws

import (
	_ "embed"
	"encoding/json"
	"testing"
)

// idleFullPushJSON is the printer's real, unaltered first full-state push
// while idle, byte-for-byte from references/printer-snapshot/ws9999_messages.jsonl
// (BOM stripped). It is the only literal full-frame capture available in the
// repository; the other observed real states (printing/paused/cancelled)
// exist only as partial field excerpts in
// references/printer-snapshot/extra/control_test_20260928.log, so the
// fixtures for those states below are built by overriding this real base
// with those excerpted values, not replayed verbatim.
//
//go:embed testdata/idle_full_push.json
var idleFullPushJSON []byte

// connectionCountDeltaJSON is the real second frame from the same capture: a
// small, partial delta with none of the identifying fields.
//
//go:embed testdata/connection_count_delta.json
var connectionCountDeltaJSON []byte

// tempDeltaJSON is a real partial telemetry delta from
// references/printer-snapshot/extra/ws9999_capture_35s_20260928.jsonl,
// carrying only nozzleTemp/bedTemp0 as decimal strings and none of the
// identifying fields.
//
//go:embed testdata/temp_delta.json
var tempDeltaJSON []byte

// idleBaseFields decodes idleFullPushJSON into a map every test can clone
// and override, instead of hand-duplicating the full ~50-field object.
func idleBaseFields(t *testing.T) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(idleFullPushJSON, &fields); err != nil {
		t.Fatalf("decode idleFullPushJSON fixture: %v", err)
	}
	return fields
}

// withOverrides clones base and applies overrides on top, returning encoded
// JSON ready to send as a fake push frame.
func withOverrides(t *testing.T, base map[string]any, overrides map[string]any) []byte {
	t.Helper()
	clone := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		clone[k] = v
	}
	for k, v := range overrides {
		clone[k] = v
	}
	data, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("marshal fixture override: %v", err)
	}
	return data
}

// printingPushFields are drawn from control_test_20260928.log line 33
// ("ws9999 first push" logged right after POST /printer/print/start),
// layered onto the idle base since the log only recorded these fields
// changing.
func printingPushFields(t *testing.T) []byte {
	t.Helper()
	return withOverrides(t, idleBaseFields(t), map[string]any{
		"state":            1,
		"deviceState":      1,
		"printProgress":    9,
		"dProgress":        0,
		"printFileName":    "/mnt/UDISK/printer_data/gcodes/k2mcp_test.gcode",
		"printJobTime":     0,
		"printLeftTime":    0,
		"layer":            0,
		"TotalLayer":       0,
		"nozzleTemp":       "63.960000",
		"targetNozzleTemp": 0,
		"bedTemp0":         "30.820000",
		"targetBedTemp0":   0,
		"curFeedratePct":   100,
		"curFlowratePct":   100,
		"modelFanPct":      0,
		"err":              map[string]any{"errcode": 0, "key": 0, "value": ""},
	})
}

// pausedPushFields are drawn from control_test_20260928.log line 39 (logged
// right after POST /printer/print/pause).
func pausedPushFields(t *testing.T) []byte {
	t.Helper()
	return withOverrides(t, idleBaseFields(t), map[string]any{
		"state":            5,
		"deviceState":      1,
		"printProgress":    14,
		"dProgress":        0,
		"printFileName":    "/mnt/UDISK/printer_data/gcodes/k2mcp_test.gcode",
		"printJobTime":     0,
		"printLeftTime":    0,
		"layer":            0,
		"TotalLayer":       0,
		"nozzleTemp":       "66.630000",
		"targetNozzleTemp": 140,
		"bedTemp0":         "30.990000",
		"targetBedTemp0":   0,
		"curFeedratePct":   100,
		"curFlowratePct":   100,
		"modelFanPct":      0,
		"err":              map[string]any{"errcode": 0, "key": 0, "value": ""},
	})
}

// cancelledPushFields are drawn from control_test_20260928.log line 45
// (logged right after POST /printer/print/cancel).
func cancelledPushFields(t *testing.T) []byte {
	t.Helper()
	return withOverrides(t, idleBaseFields(t), map[string]any{
		"state":            4,
		"deviceState":      0,
		"printProgress":    14,
		"dProgress":        0,
		"printFileName":    "/mnt/UDISK/printer_data/gcodes/k2mcp_test.gcode",
		"printJobTime":     0,
		"printLeftTime":    0,
		"layer":            0,
		"TotalLayer":       0,
		"nozzleTemp":       "86.890000",
		"targetNozzleTemp": 0,
		"bedTemp0":         "30.620000",
		"targetBedTemp0":   0,
		"curFeedratePct":   100,
		"curFlowratePct":   100,
		"modelFanPct":      50,
		"err":              map[string]any{"errcode": 0, "key": 0, "value": ""},
	})
}
