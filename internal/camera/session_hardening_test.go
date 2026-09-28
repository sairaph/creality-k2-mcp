package camera

// This file covers two regressions found in session.go's Open/Close path:
//
//   - Production newSessionAPI never disabled pion's mDNS candidate
//     gathering, so every real Open opened a real 224.0.0.0:5353
//     multicast UDP socket (see disableMulticastDNS's doc comment in
//     session.go). TestNewSessionAPIDisablesMulticastDNS proves the fix by
//     applying newSessionAPI's exact mDNS-disabling call to a bare
//     SettingEngine and inspecting the result, without touching a real
//     socket at all (this package's tests run over vnet regardless - see
//     vnet_test.go - but restrictICEToRoute's real UDP dial is still
//     avoided here on principle, since it plays no part in what this test
//     checks).
//
//   - A late Open failure (negotiate, SetRemoteDescription, or
//     waitConnected erroring) canceled and closed the PeerConnection but
//     returned before the readTrack goroutine it may have started had
//     actually exited. TestOpenCloseGoroutineCountStable is a coarser
//     regression guard for the same class of leak, across repeated
//     Open/Close cycles on the happy path.

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// multicastDNSModeOf reads the unexported candidates.MulticastDNSMode field
// pion/webrtc's SettingEngine stores SetICEMulticastDNSMode's value in.
// There is no exported getter (see settingengine.go: SetICEMulticastDNSMode
// is write-only), so this uses reflect+unsafe to read it back rather than
// relying on some indirect, harder-to-verify proxy for the same fact. This
// is fragile by nature - it reaches past the package boundary pion actually
// promises to keep stable - but the field.IsValid() check below makes it
// fail loudly (t.Fatal, not a silent false negative) the moment pion renames
// or restructures the field, rather than passing on a mode this test never
// actually observed. Revisit this once pion/webrtc adds an exported getter
// for the mDNS mode and drop the reflect+unsafe read in favor of it.
func multicastDNSModeOf(t *testing.T, se *webrtc.SettingEngine) ice.MulticastDNSMode {
	t.Helper()

	field := reflect.ValueOf(se).Elem().FieldByName("candidates").FieldByName("MulticastDNSMode")
	if !field.IsValid() {
		t.Fatal("webrtc.SettingEngine.candidates.MulticastDNSMode not found; pion/webrtc internals changed")
	}
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())). //nolint:gosec
										Elem().Interface().(ice.MulticastDNSMode)
}

// TestNewSessionAPIDisablesMulticastDNS asserts that the exact call
// newSessionAPI makes in production - disableMulticastDNS - actually turns
// mDNS off, and (as a baseline, so this test would fail against the old
// code) that a bare SettingEngine does not already default to disabled.
func TestNewSessionAPIDisablesMulticastDNS(t *testing.T) {
	zero := webrtc.SettingEngine{}
	if mode := multicastDNSModeOf(t, &zero); mode == ice.MulticastDNSModeDisabled {
		t.Fatal("a bare SettingEngine{} already reports mDNS disabled; this test no longer exercises the bug it targets")
	}

	se := webrtc.SettingEngine{}
	disableMulticastDNS(&se)

	if mode := multicastDNSModeOf(t, &se); mode != ice.MulticastDNSModeDisabled {
		t.Fatalf("after disableMulticastDNS, MulticastDNSMode = %v, want MulticastDNSModeDisabled (%v)",
			mode, ice.MulticastDNSModeDisabled)
	}
}

// settledGoroutineCount returns runtime.NumGoroutine() once it has been
// unchanged for a few consecutive samples, rather than after one fixed
// sleep: pion's own internal teardown (ICE agent task loop, mux read
// loops, connectivity-check tickers) runs on its own goroutines that keep
// unwinding for a little while after PeerConnection.Close returns, so a
// single short sleep after Close is not reliably enough to reach steady
// state. This still bounds total wait time so a real leak fails the test
// instead of hanging it.
func settledGoroutineCount() int {
	const (
		sampleInterval  = 100 * time.Millisecond
		stableSamples   = 4
		maxTotalSamples = 40 // ~4s worst case
	)

	last := -1
	stableFor := 0
	for i := 0; i < maxTotalSamples; i++ {
		runtime.Gosched()
		time.Sleep(sampleInterval)
		runtime.GC()
		n := runtime.NumGoroutine()
		if n == last {
			stableFor++
			if stableFor >= stableSamples {
				return n
			}
		} else {
			stableFor = 0
		}
		last = n
	}
	return last
}

// TestOpenCloseGoroutineCountStable drives repeated Open/Close cycles and
// asserts the goroutine count returns to (near) its baseline afterward,
// catching a readTrack (or other) goroutine leak that a single Open/Close
// test would not reliably surface.
func TestOpenCloseGoroutineCountStable(t *testing.T) {
	baseline := settledGoroutineCount()

	// Each cycle runs as its own subtest, with its own fake printer, so
	// that subtest's t.Cleanup (which closes that fake printer's httptest
	// server and every server-side PeerConnection it accumulated - see
	// fakeprinter_test.go) runs before the next cycle starts. Reusing one
	// fake printer for all 10 cycles would otherwise pile up 10 server-side
	// PeerConnections that only close at the very end of the test,
	// swamping any signal from the client-side Session/readTrack leak this
	// test actually targets.
	const cycles = 10
	for i := 0; i < cycles; i++ {
		t.Run(fmt.Sprintf("cycle%d", i), func(t *testing.T) {
			host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: true})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			session, err := Open(ctx, host)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			waitForAU(t, ctx, session, func(au AccessUnit) bool { return au.Keyframe })

			if err := session.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}

	after := settledGoroutineCount()

	// A handful of goroutines can legitimately be mid-teardown (Go's
	// runtime and net/http keep-alive machinery, GC workers, etc.); this
	// tolerance is for scheduling noise, not for a real per-cycle leak,
	// which over 10 cycles would show up as growth far larger than this.
	const tolerance = 5
	if after > baseline+tolerance {
		t.Fatalf("goroutine count grew from %d to %d over %d Open/Close cycles (tolerance %d): readTrack or another goroutine may be leaking",
			baseline, after, cycles, tolerance)
	}
}
