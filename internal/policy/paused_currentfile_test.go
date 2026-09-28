package policy

import (
	"context"
	"testing"
)

// This file covers upload/delete's "never touch the current print file"
// rule (checkDelete, evaluateUpload in actions.go) specifically while the
// printer is paused or preparing, not just printing: the check itself keys
// only on print_stats.filename, with no bucket condition, so it already
// applies in every bucket that carries a filename - these tests pin that
// down for the two buckets an operator is most likely to also be uploading
// or deleting files from (D3, 10-hazard-analysis.md 2.3).

func TestExecute_Upload_RefusesOverCurrentPrintFileWhilePaused(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("active.gcode")
	f.setPaused()
	p := New()
	printer := testPrinter(f, "upload-paused")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionUploadGCodeFile, Params{Filename: "active.gcode", LocalPath: "/tmp/x"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict", err)
	}
}

func TestExecute_Delete_RefusesCurrentPrintFileWhilePaused(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.setPrinting("active.gcode")
	f.setPaused()
	p := New()
	printer := testPrinter(f, "delete-paused")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionDeleteGCodeFile, Params{Filename: "active.gcode"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict", err)
	}
}

func TestExecute_Upload_RefusesOverCurrentPrintFileWhilePreparing(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.withLock(func() {
		f.printState = "printing"
		f.sdActive = true
		f.printDuration = 0 // preparing sub-state: bucket PP, not P
		f.filename = "active.gcode"
	})
	p := New()
	printer := testPrinter(f, "upload-preparing")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionUploadGCodeFile, Params{Filename: "active.gcode", LocalPath: "/tmp/x"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict", err)
	}
}

func TestExecute_Delete_RefusesCurrentPrintFileWhilePreparing(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.withLock(func() {
		f.printState = "printing"
		f.sdActive = true
		f.printDuration = 0 // preparing sub-state: bucket PP, not P
		f.filename = "active.gcode"
	})
	p := New()
	printer := testPrinter(f, "delete-preparing")

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionDeleteGCodeFile, Params{Filename: "active.gcode"}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeConflict {
		t.Fatalf("err = %#v, want CodeConflict", err)
	}
}
