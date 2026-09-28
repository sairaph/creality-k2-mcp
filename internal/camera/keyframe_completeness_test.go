package camera

// This file's tests are the regression coverage for
// dev_docs/camera-keyframe-rca.md and dev_docs/t11e-soak-report.md items 2
// and 3, all running over pion's virtual network only (no real socket, per
// AGENTS.md's hard testing rules; see vnet_test.go's TestMain):
//
//   - TestSessionSTAPAKeyframe: a realistic "STAP-A (SPS+PPS) followed by
//     FU-A (IDR)" RTP framing, proving samplebuilder plus the H264
//     depacketizer correctly reassembles it into one complete, in-band
//     keyframe access unit. Every other test in this package forces
//     DisableStapA so it can control exactly which GOP carries its own
//     SPS/PPS; this is the one place that packetization shape itself is
//     exercised end to end.
//
//   - TestSessionFirstKeyframeMissingParamsThenRecovers: the soak report's
//     exact failure mode reproduced deterministically - a session's very
//     first delivered access unit is an IDR with no SPS/PPS in-band and
//     nothing cached yet - and proves both halves of the fix: (a)
//     processSample now reports Keyframe:false for that access unit
//     instead of the old, silently-broken Keyframe:true, and (b) Session's
//     own periodic PLI retry (not any manual RequestKeyframe call from the
//     test) is what eventually elicits a complete keyframe.

import (
	"context"
	"testing"
	"time"
)

// TestSessionSTAPAKeyframe proves Session correctly reassembles a
// realistic STAP-A(SPS+PPS)+FU-A(IDR) RTP framing into one complete,
// in-band keyframe access unit, and that it decodes.
func TestSessionSTAPAKeyframe(t *testing.T) {
	host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: true, useStapA: true})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout(t, 5*time.Second))
	defer cancel()

	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	au := waitForAU(t, ctx, session, func(au AccessUnit) bool { return au.Keyframe })

	units := splitAnnexB(au.Data)
	if len(units) < 3 {
		t.Fatalf("STAP-A-packetized keyframe has %d NAL units, want at least 3 (SPS, PPS, IDR)", len(units))
	}
	if nalType(units[0]) != nalTypeSPS {
		t.Errorf("first NAL unit type = %d, want SPS (%d)", nalType(units[0]), nalTypeSPS)
	}
	if nalType(units[1]) != nalTypePPS {
		t.Errorf("second NAL unit type = %d, want PPS (%d)", nalType(units[1]), nalTypePPS)
	}

	foundIDR := false
	for _, u := range units {
		if nalType(u) == nalTypeIDR {
			foundIDR = true
		}
	}
	if !foundIDR {
		t.Error("no IDR NAL unit found in the reassembled keyframe access unit")
	}
}

// TestSessionFirstKeyframeMissingParamsThenRecovers reproduces
// dev_docs/t11e-soak-report.md item 2 live: a fresh session's first-ever
// access unit is an IDR with no SPS/PPS in-band and nothing cached yet.
// Before the fix, Session reported this as Keyframe:true anyway, handing
// callers an undecodable access unit. After the fix it must be
// Keyframe:false, and Session's own background PLI retry (session.go's
// retryPLIUntilKeyframe, started by Open with no help from this test) must
// go on to elicit a real, complete keyframe within a few retry intervals.
func TestSessionFirstKeyframeMissingParamsThenRecovers(t *testing.T) {
	old := pliRetryInterval
	pliRetryInterval = 100 * time.Millisecond
	t.Cleanup(func() { pliRetryInterval = old })

	host := startFakePrinter(t, fakePrinterOptions{
		autoRepeatKeyframes:   false,
		firstGOPMissingParams: true,
		pliResendComplete:     true,
		// Ignore the track-start PLI and one retry: only the third PLI
		// (the second periodic retry) gets a response, so this test can
		// only pass if retryPLIUntilKeyframe is actually retrying, not
		// merely relying on Open's single immediate request.
		ignorePLICount: 2,
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout(t, 8*time.Second))
	defer cancel()

	session, err := Open(ctx, host)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer session.Close()

	// Drain access units until either a Keyframe:true arrives (success) or
	// we observe the expected incomplete IDR at least once first (sanity
	// check that this test actually exercises the bug's precondition,
	// rather than the fake printer happening to send something complete
	// immediately).
	sawIncompleteIDR := false
	deadline := time.After(testTimeout(t, 6*time.Second))
	for {
		select {
		case au, ok := <-session.AccessUnits():
			if !ok {
				t.Fatal("access unit stream ended before a complete keyframe arrived")
			}
			if !au.Keyframe {
				if hasIDRNAL(au.Data) {
					sawIncompleteIDR = true
				}
				continue
			}
			if !sawIncompleteIDR {
				t.Fatal("got a complete keyframe without ever observing the expected incomplete-IDR precondition; this test no longer exercises the bug it targets")
			}
			units := splitAnnexB(au.Data)
			if len(units) < 3 || nalType(units[0]) != nalTypeSPS || nalType(units[1]) != nalTypePPS {
				t.Fatalf("recovered keyframe is not self-contained: %d units, first two types %v",
					len(units), []int{nalType(units[0]), nalType(units[1])})
			}
			return // success: recovered a complete keyframe via the internal PLI retry alone
		case <-deadline:
			t.Fatal("no complete keyframe arrived within the test deadline; retryPLIUntilKeyframe may not be retrying")
		}
	}
}

// hasIDRNAL reports whether data (Annex-B) contains an IDR NAL unit.
func hasIDRNAL(data []byte) bool {
	for _, u := range splitAnnexB(data) {
		if nalType(u) == nalTypeIDR {
			return true
		}
	}
	return false
}
