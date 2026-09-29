package printerstate

import (
	"errors"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/moonraker"
)

// TestDeriveActivityState_IdentityMismatch pins review backlog item 24: a
// registry-backed printer (persisted Hostname) whose fresh printer/info read
// answers with a different Klipper hostname must derive as
// identity_mismatch, bucket U, unknown_fail_closed, naming both hostnames.
func TestDeriveActivityState_IdentityMismatch(t *testing.T) {
	s := syntheticIdle()
	s.Printer = domain.Printer{ID: "k2-5885", Hostname: "K2-5885"}
	s.PrinterInfo = moonraker.PrinterInfoResult{State: "ready", Hostname: "K2-9999"}

	d := DeriveActivityState(s, nil)
	if d.State != StateIdentityMismatch {
		t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StateIdentityMismatch, d.Reasons)
	}
	if d.Bucket != BucketU || d.Class != ClassUnknownFailClosed {
		t.Fatalf("Bucket/Class = %s/%s, want U/unknown_fail_closed", d.Bucket, d.Class)
	}
	if canon := bucketOf[StateIdentityMismatch]; canon != BucketU {
		t.Fatalf("bucketOf[identity_mismatch] = %s, want U", canon)
	}
	joined := d.Reasons[0]
	if !containsAll(joined, "K2-5885", "K2-9999", "printers scan") {
		t.Fatalf("reason = %q, want it to name both hostnames and suggest re-running discovery", joined)
	}
}

// TestDeriveActivityState_IdentityMismatchIgnoresCaseAndTrailingDot pins
// HostnamesEqual's own normalisation.
func TestDeriveActivityState_IdentityMismatchIgnoresCaseAndTrailingDot(t *testing.T) {
	s := syntheticIdle()
	s.Printer = domain.Printer{ID: "k2-5885", Hostname: "k2-5885."}
	s.PrinterInfo = moonraker.PrinterInfoResult{State: "ready", Hostname: "K2-5885"}

	d := DeriveActivityState(s, nil)
	if d.State != StateIdle {
		t.Fatalf("State = %q, want %q (case/trailing-dot must still match, reasons: %v)", d.State, StateIdle, d.Reasons)
	}
}

// TestDeriveActivityState_IdentityUnverified_PrinterInfoErr pins the
// printer/info-unreachable half of item 24: reads still get a meaningful
// display state, but it is unknown_fail_closed / U, never the state the
// rest of the snapshot would otherwise derive to.
func TestDeriveActivityState_IdentityUnverified_PrinterInfoErr(t *testing.T) {
	s := syntheticIdle()
	s.Printer = domain.Printer{ID: "k2-5885", Hostname: "K2-5885"}
	s.PrinterInfoErr = errors.New("dial tcp: connection refused")

	d := DeriveActivityState(s, nil)
	if d.State != StateIdentityUnverified {
		t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StateIdentityUnverified, d.Reasons)
	}
	if d.Bucket != BucketU || d.Class != ClassUnknownFailClosed {
		t.Fatalf("Bucket/Class = %s/%s, want U/unknown_fail_closed", d.Bucket, d.Class)
	}
}

// TestDeriveActivityState_IdentityUnverified_EmptyHostname pins the other
// unverified case: printer/info answered but reported no hostname at all.
func TestDeriveActivityState_IdentityUnverified_EmptyHostname(t *testing.T) {
	s := syntheticIdle()
	s.Printer = domain.Printer{ID: "k2-5885", Hostname: "K2-5885"}
	s.PrinterInfo = moonraker.PrinterInfoResult{State: "ready"} // Hostname left empty

	d := DeriveActivityState(s, nil)
	if d.State != StateIdentityUnverified {
		t.Fatalf("State = %q, want %q (reasons: %v)", d.State, StateIdentityUnverified, d.Reasons)
	}
}

// TestDeriveActivityState_IdentityMatch_DerivesNormally pins the "match"
// case: once the live hostname agrees with the persisted one, derivation
// proceeds exactly as it would with no identity check at all.
func TestDeriveActivityState_IdentityMatch_DerivesNormally(t *testing.T) {
	s := syntheticIdle()
	s.Printer = domain.Printer{ID: "k2-5885", Hostname: "K2-5885"}
	s.PrinterInfo = moonraker.PrinterInfoResult{State: "ready", Hostname: "K2-5885"}

	d := DeriveActivityState(s, nil)
	if d.State != StateIdle || d.Bucket != BucketI || d.Class != ClassSafeToAct {
		t.Fatalf("state/bucket/class = %s/%s/%s, want idle/I/safe_to_act (reasons: %v)", d.State, d.Bucket, d.Class, d.Reasons)
	}
}

// TestDeriveActivityState_IdentityCheckSkippedWithNoPersistedHostname pins
// the K2_MCP_HOST environment-override case (domain/env.go): a Printer with
// no persisted Hostname has nothing to verify here, so an unreachable or
// empty printer/info read never forces identity_unverified; internal/policy
// resolves and verifies that printer's identity itself before locking or
// writing.
func TestDeriveActivityState_IdentityCheckSkippedWithNoPersistedHostname(t *testing.T) {
	s := syntheticIdle()
	s.Printer = domain.Printer{ID: domain.EnvPrinterID} // Hostname left empty
	s.PrinterInfoErr = errors.New("dial tcp: connection refused")

	d := DeriveActivityState(s, nil)
	if d.State != StateIdle {
		t.Fatalf("State = %q, want %q (no persisted hostname means nothing to verify here)", d.State, StateIdle)
	}
}

// TestDeriveActivityState_IdentityMismatchStillReads pins P1/P6 "reads are
// always allowed": BuildStateBlock still renders a full, informative state
// block on a mismatch, including the live (verified) hostname alongside the
// registry one, rather than an opaque failure.
func TestDeriveActivityState_IdentityMismatchStillReads(t *testing.T) {
	s := syntheticIdle()
	s.Printer = domain.Printer{ID: "k2-5885", Name: "Living Room K2", Hostname: "K2-5885"}
	s.PrinterInfo = moonraker.PrinterInfoResult{State: "ready", Hostname: "K2-9999"}

	d := DeriveActivityState(s, nil)
	block := BuildStateBlock(s, d, nil)
	if block.ActivityState != StateIdentityMismatch {
		t.Fatalf("ActivityState = %q, want %q", block.ActivityState, StateIdentityMismatch)
	}
	if block.Hostname != "K2-5885" {
		t.Fatalf("Hostname = %q, want the persisted registry hostname K2-5885", block.Hostname)
	}
	if block.VerifiedHostname != "K2-9999" {
		t.Fatalf("VerifiedHostname = %q, want the live hostname K2-9999", block.VerifiedHostname)
	}
	if block.PrinterName != "Living Room K2" {
		t.Fatalf("PrinterName = %q, want it still populated (reads keep working on mismatch)", block.PrinterName)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
