package moonraker

import (
	"reflect"
	"sort"
	"testing"
)

// TestClientExportedMethodSet pins the exact exported method set of
// *Client, per safety-architecture.md 3.2 point 3 ("The Moonraker client
// type has no method for update_mesh, machine reboot/shutdown/services,
// update manager, config file writes, or the config file root; a test
// asserts the exported method set") and this task's constraint that
// RunTemplate, PrintStart/Pause/Resume/Cancel, Upload and Delete are the
// only write methods. If this test needs to change, that change is the
// review question: does the new method belong on the closed command
// surface, not just "does it compile".
func TestClientExportedMethodSet(t *testing.T) {
	want := []string{
		"Delete",
		"Directory",
		"GCodeStore",
		"HeadRange",
		"HistoryList",
		"HistoryTotals",
		"JobQueueStatus",
		"List",
		"Metadata",
		"ObjectsList",
		"PrintCancel",
		"PrintPause",
		"PrintResume",
		"PrintStart",
		"PrinterInfo",
		"QueryObjects",
		"RunTemplate",
		"ServerInfo",
		"TailRange",
		"Upload",
	}
	sort.Strings(want)

	typ := reflect.TypeOf(&Client{})
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exported *Client methods =\n%v\nwant\n%v", got, want)
	}
}
