package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// TestExecute_IdentityMismatch_RefusesAllWrites pins review backlog item 24:
// a registry-backed printer whose live printer/info hostname disagrees with
// the persisted one must derive as identity_mismatch/U, and Execute must
// refuse every write on it, including set_light (which is normally allowed
// in every U state but offline: here the printer's physical identity itself
// is uncertain, so even that must be refused).
func TestExecute_IdentityMismatch_RefusesAllWrites(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	f.addFile("model.gcode")
	p := New()
	printer := testPrinter(f, "K2-mismatch")
	f.setPrinterInfo("K2-different", nil) // overrides testPrinter's match; disagrees with the persisted hostname

	for _, tc := range []struct {
		name   string
		action ActionName
		params Params
	}{
		{"set_light", ActionSetLight, Params{On: true}},
		{"start_print", ActionStartPrint, Params{Filename: "model.gcode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), tc.action, tc.params, "")
			perr, ok := err.(*Error)
			if !ok || perr.Code != CodeUnavailable {
				t.Fatalf("%s err = %#v, want CodeUnavailable (identity mismatch)", tc.name, err)
			}
		})
	}
	if f.led != 0 {
		t.Fatal("led was changed, want set_light to have been refused before ever reaching the printer")
	}
	if start, _, _, _ := f.counts(); start != 0 {
		t.Errorf("PrintStart called %d times, want 0", start)
	}
}

// TestExecute_IdentityUnverified_RefusesWrites pins the other half of item
// 24: a registry-backed printer whose fresh printer/info read fails
// entirely (not just disagrees) must also refuse every write, not merely
// fall back to the persisted hostname.
func TestExecute_IdentityUnverified_RefusesWrites(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	p := New()
	printer := testPrinter(f, "K2-unverified")
	f.setPrinterInfo("", errors.New("connection refused")) // overrides testPrinter's match

	_, err := p.Execute(context.Background(), f.deps(), printer, testSettings(), ActionSetLight, Params{On: true}, "")
	perr, ok := err.(*Error)
	if !ok || perr.Code != CodeUnavailable {
		t.Fatalf("err = %#v, want CodeUnavailable (identity unverified)", err)
	}
	if f.led != 0 {
		t.Fatal("led was changed, want set_light to have been refused before ever reaching the printer")
	}
}

// TestExecute_IdentityMatch_BehavesNormally pins the "match" case: once the
// live hostname agrees with the persisted one (testPrinter's own default via
// setPrinterInfo), Execute behaves exactly as it did before item 24 - the
// identity check is invisible on the happy path.
func TestExecute_IdentityMatch_BehavesNormally(t *testing.T) {
	setTestHome(t)
	f := newFakePrinter()
	p := New()
	printer := testPrinter(f, "K2-match")

	res := mustExecute(t, p, f, printer, ActionSetLight, Params{On: true}, "")
	if res.Effect != "confirmed" {
		t.Fatalf("set_light result = %+v, want confirmed", res)
	}
	if res.After.Hostname != "K2-match" || res.After.VerifiedHostname != "K2-match" {
		t.Fatalf("After = %+v, want Hostname and VerifiedHostname both K2-match", res.After)
	}
}

// TestAvailable_IdentityMismatch_StillReads pins P1/P6 "reads are always
// allowed": Available (the read-only display path, dev_docs/safety-
// architecture.md 3.2) must still return a full gate list during an
// identity mismatch, every one of them blocked, rather than panicking or
// returning nothing.
func TestAvailable_IdentityMismatch_StillReads(t *testing.T) {
	f := newFakePrinter()
	p := New()
	printer := testPrinter(f, "K2-mismatch-read")
	f.setPrinterInfo("K2-different", nil) // overrides testPrinter's match

	snap := printerstate.Take(context.Background(), f.deps().stateDeps(), printer)
	derived := printerstate.DeriveActivityState(snap, nil)
	if derived.State != printerstate.StateIdentityMismatch || derived.Bucket != printerstate.BucketU {
		t.Fatalf("state/bucket = %s/%s, want identity_mismatch/U (reasons: %v)", derived.State, derived.Bucket, derived.Reasons)
	}

	gates := p.Available(printer, snap, testSettings())
	if len(gates) != len(Actions) {
		t.Fatalf("len(gates) = %d, want %d (Available must still enumerate every action on a mismatch)", len(gates), len(Actions))
	}
	for _, g := range gates {
		if g.Status != "blocked" {
			t.Errorf("action %s status = %q, want blocked while identity is mismatched", g.Name, g.Status)
		}
	}
}
