package printerstate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// connectedBox decodes the real Moonraker box object captured with a CFS
// connected (dev_docs/captures/moonraker_box_filament_rack_20260929.json):
// state connect, enable 1, filament_useup 0, identity map, T1 connect.
func connectedBox(t *testing.T) *moonraker.Box {
	t.Helper()
	var envelope struct {
		Result struct {
			Status map[string]json.RawMessage `json:"status"`
		} `json:"result"`
	}
	if err := json.Unmarshal(readTestdata(t, "moonraker_box_filament_rack_20260929.json"), &envelope); err != nil {
		t.Fatalf("decode box capture: %v", err)
	}
	b, err := moonraker.DecodeBox(envelope.Result.Status["box"])
	if err != nil {
		t.Fatalf("DecodeBox: %v", err)
	}
	return &b
}

func present(v int) crealityws.Int { return crealityws.Int{Value: v, Present: true} }

// cfsIdle is a synthetic idle snapshot with a connected, fully known and
// quiescent CFS: every input cfsFlags reads is positively present.
func cfsIdle(t *testing.T) Snapshot {
	t.Helper()
	s := syntheticIdle()
	s.Box = connectedBox(t)
	s.PauseResume.ResumeErr = boolPtr(false)
	s.WS9999 = crealityws.Status{
		State:          present(0),
		DeviceState:    present(0),
		FeedState:      present(0),
		UpgradeStatus:  present(0),
		RepoPlrStatus:  present(0),
		MaterialStatus: present(0),
		CfsConnect:     present(1),
		Err:            crealityws.StatusErr{Present: true},
		WithSelfTest:   present(100),
	}
	return s
}

func hasReason(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func TestCFSFlags_NotConnectedIsKnownAndQuiescent(t *testing.T) {
	// The disconnected baseline: even a snapshot with no 9999 at all carries
	// no CFS reasons.
	s := syntheticIdle()
	s.WS9999 = crealityws.Status{}
	s.WS9999Reachable = false
	known, quiescent, errFlag, reasons := cfsFlags(s)
	if !known || !quiescent || errFlag || len(reasons) != 0 {
		t.Fatalf("flags = %v/%v/%v %v, want true/true/false and no reasons", known, quiescent, errFlag, reasons)
	}
}

func TestCFSFlags_ConnectedIdleIsKnownQuiescent(t *testing.T) {
	known, quiescent, errFlag, reasons := cfsFlags(cfsIdle(t))
	if !known || !quiescent || errFlag || len(reasons) != 0 {
		t.Fatalf("flags = %v/%v/%v %v, want true/true/false and no reasons", known, quiescent, errFlag, reasons)
	}
}

func TestCFSFlags_Table(t *testing.T) {
	tests := []struct {
		name                      string
		mutate                    func(s *Snapshot)
		known, quiescent, errFlag bool
		reason                    string
	}{
		{"9999 unreachable", func(s *Snapshot) { s.WS9999Reachable = false; s.WS9999 = crealityws.Status{} }, false, false, false, "port 9999 is unreachable"},
		{"deviceState absent", func(s *Snapshot) { s.WS9999.DeviceState = crealityws.Int{} }, false, false, false, "deviceState is absent"},
		{"feedState absent", func(s *Snapshot) { s.WS9999.FeedState = crealityws.Int{} }, false, false, false, "feedState is absent"},
		{"materialStatus absent", func(s *Snapshot) { s.WS9999.MaterialStatus = crealityws.Int{} }, false, false, false, "materialStatus is absent"},
		{"err absent", func(s *Snapshot) { s.WS9999.Err = crealityws.StatusErr{} }, false, false, false, "err is absent"},
		{"repoPlrStatus absent", func(s *Snapshot) { s.WS9999.RepoPlrStatus = crealityws.Int{} }, false, false, false, "repoPlrStatus is absent"},
		{"upgradeStatus absent", func(s *Snapshot) { s.WS9999.UpgradeStatus = crealityws.Int{} }, false, false, false, "upgradeStatus is absent"},
		{"cfsConnect absent", func(s *Snapshot) { s.WS9999.CfsConnect = crealityws.Int{} }, false, false, false, "cfsConnect is absent"},
		{"box object absent", func(s *Snapshot) { s.Box = nil }, false, false, false, "box object is absent"},
		{"box.state absent", func(s *Snapshot) { s.Box.State = nil }, false, false, false, "box.state is absent"},
		{"box.state not connect", func(s *Snapshot) { s.Box.State = strPtr("idle") }, false, false, false, `box.state is "idle"`},
		{"filament_useup absent", func(s *Snapshot) { s.Box.FilamentUseup = nil }, false, false, false, "filament_useup is absent"},
		{"resume_err absent", func(s *Snapshot) { s.PauseResume.ResumeErr = nil }, false, false, false, "resume_err is absent"},
		{"pause_resume absent", func(s *Snapshot) { s.PauseResume = nil }, false, false, false, "resume_err is absent"},
		{"cfsConnect disagrees with box.state", func(s *Snapshot) { s.WS9999.CfsConnect = present(0) }, false, false, false, "cfsConnect is 0"},
		{"err errcode", func(s *Snapshot) { s.WS9999.Err = crealityws.StatusErr{ErrCode: 5, Present: true} }, true, false, true, "errcode 5"},
		{"err key", func(s *Snapshot) { s.WS9999.Err = crealityws.StatusErr{Key: 7, Present: true} }, true, false, true, "key 7"},
		{"materialStatus", func(s *Snapshot) { s.WS9999.MaterialStatus = present(1) }, true, false, true, "materialStatus is 1"},
		{"resume_err true", func(s *Snapshot) { s.PauseResume.ResumeErr = boolPtr(true) }, true, false, true, "resume_err is true"},
		{"filament_useup", func(s *Snapshot) { s.Box.FilamentUseup = intPtr(1) }, true, false, true, "filament_useup is 1"},
		{"deviceState 1", func(s *Snapshot) { s.WS9999.DeviceState = present(1) }, true, false, false, "deviceState is 1"},
		{"feedState 2", func(s *Snapshot) { s.WS9999.FeedState = present(2) }, true, false, false, "feedState is 2"},
		{"feedState 77", func(s *Snapshot) { s.WS9999.FeedState = present(77) }, true, false, false, "feedState is 77"},
		{"repoPlrStatus 1", func(s *Snapshot) { s.WS9999.RepoPlrStatus = present(1) }, true, false, false, "repoPlrStatus is 1"},
		{"upgradeStatus 1", func(s *Snapshot) { s.WS9999.UpgradeStatus = present(1) }, true, false, false, "upgradeStatus is 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := cfsIdle(t)
			tc.mutate(&s)
			known, quiescent, errFlag, reasons := cfsFlags(s)
			if known != tc.known || quiescent != tc.quiescent || errFlag != tc.errFlag {
				t.Fatalf("flags = known %v quiescent %v error %v, want %v/%v/%v (reasons %v)", known, quiescent, errFlag, tc.known, tc.quiescent, tc.errFlag, reasons)
			}
			if !hasReason(reasons, tc.reason) {
				t.Fatalf("reasons %v do not name %q", reasons, tc.reason)
			}
		})
	}
}

func TestCFSFlags_IdleFeedStatesAreQuiescent(t *testing.T) {
	for _, fs := range []int{0, 3, 14, 100} {
		s := cfsIdle(t)
		s.WS9999.FeedState = present(fs)
		if _, quiescent, _, reasons := cfsFlags(s); !quiescent {
			t.Errorf("feedState %d: not quiescent (%v)", fs, reasons)
		}
	}
}

// Every return path of the derivation must carry the flags. The wrapper sets
// them once; this test runs every row of the state table twice. Against the
// disconnected baseline the flags must be Known and Quiescent with no error
// and no reasons. Against a snapshot with a connected CFS that reports an
// error, a Derived whose flags were left at their zero values (CFSError
// false: fail-open) is caught by CFSError being false or CFSQuiescent being
// true.
func TestCFSFlags_SetOnEveryReturnPath(t *testing.T) {
	for _, tc := range stateTable() {
		t.Run(tc.name+" (no CFS)", func(t *testing.T) {
			d := DeriveActivityState(tc.snap(), tc.pending)
			if d.CFSConnected {
				// Early-return rows (offline, klippy, identity) report the
				// CFS connected without inspecting the box (fail closed); the
				// flags must then be the connected-branch answer.
				known, quiescent, errFlag, _ := cfsFlagsFor(tc.snap(), true)
				if d.CFSKnown != known || d.CFSQuiescent != quiescent || d.CFSError != errFlag {
					t.Fatalf("early-return flags = %v/%v/%v, want the connected-branch %v/%v/%v",
						d.CFSKnown, d.CFSQuiescent, d.CFSError, known, quiescent, errFlag)
				}
				if d.CFSKnown {
					t.Fatalf("an early-return row reported CFSKnown with the box disconnected: %v", d.CFSReasons)
				}
				return
			}
			if !d.CFSKnown || !d.CFSQuiescent || d.CFSError || len(d.CFSReasons) != 0 {
				t.Fatalf("no CFS: flags = %v/%v/%v %v, want true/true/false none", d.CFSKnown, d.CFSQuiescent, d.CFSError, d.CFSReasons)
			}
		})
		t.Run(tc.name+" (CFS connected with an error)", func(t *testing.T) {
			s := tc.snap()
			s.Box = connectedBox(t)
			paused := s.PauseResume != nil && s.PauseResume.IsPaused != nil && *s.PauseResume.IsPaused
			s.PauseResume = &moonraker.PauseResume{IsPaused: boolPtr(paused), ResumeErr: boolPtr(false)}
			s.WS9999Reachable = true
			s.WS9999.DeviceState = present(0)
			s.WS9999.FeedState = present(0)
			s.WS9999.MaterialStatus = present(0)
			s.WS9999.CfsConnect = present(1)
			s.WS9999.Err = crealityws.StatusErr{ErrCode: 9, Present: true}
			d := DeriveActivityState(s, tc.pending)
			if !d.CFSConnected {
				t.Fatalf("CFSConnected = false, want true (state %s)", d.State)
			}
			if !d.CFSError {
				t.Fatalf("state %s: CFSError = false with 9999 err.errcode 9: a return path skipped the flags (fail-open)", d.State)
			}
			if d.CFSQuiescent {
				t.Fatalf("state %s: CFSQuiescent = true with an error present", d.State)
			}
			if !hasReason(d.CFSReasons, "errcode 9") {
				t.Fatalf("state %s: CFSReasons %v do not name the error", d.State, d.CFSReasons)
			}
		})
	}
}

// --- signal-fed filament_operation (plan 2.4, V6) ---

func TestSignalFedFilamentOperation(t *testing.T) {
	standbyFeeding := func(t *testing.T) Snapshot {
		s := cfsIdle(t)
		s.WS9999.FeedState = present(2)
		return s
	}
	t.Run("standby plus feeding is filament_operation, bucket U", func(t *testing.T) {
		d := DeriveActivityState(standbyFeeding(t), nil)
		if d.State != StateFilamentOperation || d.Bucket != BucketU || d.Class != ClassUnknownFailClosed {
			t.Fatalf("state/bucket/class = %s/%s/%s, want filament_operation/U/unknown_fail_closed", d.State, d.Bucket, d.Class)
		}
		if !hasReason(d.Reasons, "feedState is 2") {
			t.Fatalf("reasons %v do not name the signal", d.Reasons)
		}
	})
	for _, ds := range []int{10, 11} {
		t.Run(fmt.Sprintf("deviceState %d", ds), func(t *testing.T) {
			s := cfsIdle(t)
			s.WS9999.DeviceState = present(ds)
			d := DeriveActivityState(s, nil)
			if d.State != StateFilamentOperation || d.Bucket != BucketU {
				t.Fatalf("state/bucket = %s/%s, want filament_operation/U", d.State, d.Bucket)
			}
		})
	}
	t.Run("an unknown feedState value fires", func(t *testing.T) {
		s := cfsIdle(t)
		s.WS9999.FeedState = present(77)
		if d := DeriveActivityState(s, nil); d.State != StateFilamentOperation {
			t.Fatalf("state = %s, want filament_operation", d.State)
		}
	})
	t.Run("printing with feeding stays printing", func(t *testing.T) {
		s := standbyFeeding(t)
		s.PrintStats.State = "printing"
		s.PrintStats.PrintDuration = 10
		s.VirtualSDCard.IsActive = boolPtr(true)
		d := DeriveActivityState(s, nil)
		if d.State != StatePrinting || d.Bucket != BucketP {
			t.Fatalf("state/bucket = %s/%s, want printing/P (a feed during a print must not shadow it)", d.State, d.Bucket)
		}
		if d.CFSQuiescent {
			t.Fatal("CFSQuiescent = true with feedState 2")
		}
	})
	t.Run("paused with feeding stays paused", func(t *testing.T) {
		s := standbyFeeding(t)
		s.PrintStats.State = "paused"
		s.PauseResume.IsPaused = boolPtr(true)
		if d := DeriveActivityState(s, nil); d.State != StatePaused || d.Bucket != BucketZ {
			t.Fatalf("state/bucket = %s/%s, want paused/Z", d.State, d.Bucket)
		}
	})
	t.Run("preparing with feeding stays preparing", func(t *testing.T) {
		s := standbyFeeding(t)
		s.PrintStats.State = "printing"
		s.PrintStats.PrintDuration = 0
		s.VirtualSDCard.IsActive = boolPtr(true)
		if d := DeriveActivityState(s, nil); d.State != StatePreparing || d.Bucket != BucketPP {
			t.Fatalf("state/bucket = %s/%s, want preparing/PP", d.State, d.Bucket)
		}
	})
	for _, ps := range []string{"cancelled", "complete"} {
		t.Run(ps+" with feeding stays bucket I with CFSQuiescent false", func(t *testing.T) {
			s := standbyFeeding(t)
			s.PrintStats.State = ps
			s.VirtualSDCard.IsActive = boolPtr(false)
			d := DeriveActivityState(s, nil)
			if d.Bucket != BucketI || d.State != ps {
				t.Fatalf("state/bucket = %s/%s, want %s/I", d.State, d.Bucket, ps)
			}
			if d.CFSQuiescent || !d.CFSKnown || d.CFSError {
				t.Fatalf("flags = known %v quiescent %v error %v, want true/false/false", d.CFSKnown, d.CFSQuiescent, d.CFSError)
			}
		})
	}
	t.Run("no CFS connected: no signal", func(t *testing.T) {
		s := syntheticIdle()
		s.WS9999.FeedState = present(2)
		if d := DeriveActivityState(s, nil); d.State != StateIdle {
			t.Fatalf("state = %s, want idle with no CFS", d.State)
		}
	})
	t.Run("9999 unreachable: no signal", func(t *testing.T) {
		s := standbyFeeding(t)
		s.WS9999Reachable = false
		if d := DeriveActivityState(s, nil); d.State == StateFilamentOperation {
			t.Fatal("filament_operation fired without a reachable 9999")
		}
	})
	t.Run("idle feed values do not fire", func(t *testing.T) {
		for _, fs := range []int{0, 3, 14, 100} {
			s := cfsIdle(t)
			s.WS9999.FeedState = present(fs)
			if d := DeriveActivityState(s, nil); d.State != StateIdle {
				t.Fatalf("feedState %d: state = %s, want idle", fs, d.State)
			}
		}
	})
}

// --- start window (plan 8a.1, signal row) ---

func TestStartWindowSignalRow(t *testing.T) {
	t.Run("standby plus withSelfTest not 100 is preparing PP", func(t *testing.T) {
		s := cfsIdle(t)
		s.WS9999.WithSelfTest = present(40)
		d := DeriveActivityState(s, nil)
		if d.State != StatePreparing || d.Bucket != BucketPP || d.Class != ClassBusy {
			t.Fatalf("state/bucket/class = %s/%s/%s, want preparing/PP/busy", d.State, d.Bucket, d.Class)
		}
		if !hasReason(d.Reasons, "withSelfTest is 40") {
			t.Fatalf("reasons %v do not name the signal", d.Reasons)
		}
	})
	t.Run("also without a CFS connected", func(t *testing.T) {
		s := syntheticIdle()
		s.WS9999.WithSelfTest = present(0)
		if d := DeriveActivityState(s, nil); d.State != StatePreparing || d.Bucket != BucketPP {
			t.Fatalf("state/bucket = %s/%s, want preparing/PP", d.State, d.Bucket)
		}
	})
	t.Run("withSelfTest 100 or absent is not a signal", func(t *testing.T) {
		s := cfsIdle(t)
		if d := DeriveActivityState(s, nil); d.State != StateIdle {
			t.Fatalf("withSelfTest 100: state = %s, want idle", d.State)
		}
		s.WS9999.WithSelfTest = crealityws.Int{}
		if d := DeriveActivityState(s, nil); d.State != StateIdle {
			t.Fatalf("withSelfTest absent: state = %s, want idle", d.State)
		}
	})
	t.Run("non-identity box.map is preparing PP", func(t *testing.T) {
		s := cfsIdle(t)
		s.Box.Map["T1B"] = "T1C"
		d := DeriveActivityState(s, nil)
		if d.State != StatePreparing || d.Bucket != BucketPP {
			t.Fatalf("state/bucket = %s/%s, want preparing/PP", d.State, d.Bucket)
		}
		if !hasReason(d.Reasons, "T1B -> T1C") {
			t.Fatalf("reasons %v do not name the differing map entry", d.Reasons)
		}
	})
	t.Run("identity map is not a signal", func(t *testing.T) {
		if d := DeriveActivityState(cfsIdle(t), nil); d.State != StateIdle {
			t.Fatalf("state = %s, want idle", d.State)
		}
	})
	t.Run("evaluated before busy_command", func(t *testing.T) {
		s := cfsIdle(t)
		s.Box.Map["T1A"] = "T1B"
		s.IdleTimeout.State = strPtr("Printing")
		if d := DeriveActivityState(s, nil); d.State != StatePreparing {
			t.Fatalf("state = %s, want preparing ahead of busy_command", d.State)
		}
	})
	t.Run("never shadows printing, paused, cancelled or complete", func(t *testing.T) {
		cases := []struct {
			name  string
			setup func(s *Snapshot)
			state string
		}{
			{"printing", func(s *Snapshot) {
				s.PrintStats.State = "printing"
				s.PrintStats.PrintDuration = 10
				s.VirtualSDCard.IsActive = boolPtr(true)
			}, StatePrinting},
			{"paused", func(s *Snapshot) {
				s.PrintStats.State = "paused"
				s.PauseResume.IsPaused = boolPtr(true)
			}, StatePaused},
		}
		for _, tc := range cases {
			s := cfsIdle(t)
			s.WS9999.WithSelfTest = present(40)
			s.Box.Map["T1B"] = "T1C"
			tc.setup(&s)
			if d := DeriveActivityState(s, nil); d.State != tc.state {
				t.Errorf("%s with both signals: state = %s, want %s", tc.name, d.State, tc.state)
			}
		}
	})
	// Klipper keeps the previous job's complete or cancelled until the next job
	// starts, so the self-test of a new start shows as those states (safety
	// review M2): the signal row must win over the rows that grant bucket I.
	t.Run("stale complete or cancelled with a signal is the start window", func(t *testing.T) {
		for _, prev := range []string{"complete", "cancelled"} {
			for name, signal := range map[string]func(s *Snapshot){
				"withSelfTest 50":  func(s *Snapshot) { s.WS9999.WithSelfTest = present(50) },
				"non-identity map": func(s *Snapshot) { s.Box.Map["T1A"] = "T1D" },
			} {
				s := cfsIdle(t)
				s.PrintStats.State = prev
				signal(&s)
				d := DeriveActivityState(s, nil)
				if d.State != StatePreparing || d.Bucket != BucketPP || !d.StartWindow {
					t.Errorf("%s + %s: %s/%s window %v, want preparing/PP/true", prev, name, d.State, d.Bucket, d.StartWindow)
				}
				s = cfsIdle(t)
				s.PrintStats.State = prev
				if d := DeriveActivityState(s, nil); d.Bucket != BucketI || d.StartWindow {
					t.Errorf("%s without a signal: %s/%s window %v, want bucket I", prev, d.State, d.Bucket, d.StartWindow)
				}
			}
		}
	})
}

// --- cfsConnected hardening (plan 2.2) ---

func TestCFSConnected_UnitConnectKeepsRulesWhenBoxStateDrops(t *testing.T) {
	s := syntheticIdle()
	s.Box = &moonraker.Box{
		State: strPtr("disconnect"),
		Units: map[string]moonraker.BoxUnit{"T1": {State: "connect"}, "T2": {State: "None"}},
	}
	d := DeriveActivityState(s, nil)
	if !d.CFSConnected {
		t.Fatal("CFSConnected = false, want true: a unit still says connect while box.state dropped")
	}
	if d.CFSKnown {
		t.Fatal("CFSKnown = true with box.state disconnect, want false")
	}
	s.Box.Units["T1"] = moonraker.BoxUnit{State: "None"}
	if d := DeriveActivityState(s, nil); d.CFSConnected {
		t.Fatal("CFSConnected = true with no unit connected and box.state disconnect")
	}
}

// --- frontmatter cfs block (plan 2.5, 8a.6) ---

func TestBuildCFSBlock(t *testing.T) {
	if b := buildCFSBlock(Derived{CFSConnected: false, CFSKnown: true, CFSQuiescent: true}); b != nil {
		t.Fatalf("cfs block = %+v with no CFS, want nil", b)
	}
	d := func(bucket Bucket, known, quiescent, errFlag bool) Derived {
		return Derived{CFSConnected: true, Bucket: bucket, CFSKnown: known, CFSQuiescent: quiescent, CFSError: errFlag, CFSReasons: []string{"why"}}
	}
	tests := []struct {
		name string
		d    Derived
		want string
	}{
		{"idle", d(BucketI, true, true, false), CFSStateIdle},
		{"bucket I not quiescent is busy", d(BucketI, true, false, false), CFSStateBusy},
		{"bucket I error", d(BucketI, true, false, true), CFSStateError},
		{"bucket I unknown", d(BucketI, false, false, false), CFSStateUnknown},
		{"unknown beats error", d(BucketI, false, false, true), CFSStateUnknown},
		{"printing is in_print even when not quiescent", d(BucketP, true, false, false), CFSStateInPrint},
		{"preparing is in_print", d(BucketPP, true, false, false), CFSStateInPrint},
		{"paused is in_print", d(BucketZ, true, true, false), CFSStateInPrint},
		{"printing with an error", d(BucketP, true, false, true), CFSStateError},
		{"printing unknown", d(BucketP, false, false, false), CFSStateUnknown},
		{"bucket U quiescent is not idle", d(BucketU, true, true, false), CFSStateBusy},
		{"bucket B quiescent is not idle", d(BucketB, true, true, false), CFSStateBusy},
	}
	for _, tc := range tests {
		b := buildCFSBlock(tc.d)
		if b == nil || b.State != tc.want {
			t.Errorf("%s: cfs block = %+v, want state %s", tc.name, b, tc.want)
			continue
		}
		if len(b.Reasons) != 1 || b.Reasons[0] != "why" {
			t.Errorf("%s: reasons = %v", tc.name, b.Reasons)
		}
	}
}

func TestBuildStateBlock_CFSBlockOnlyWhenConnected(t *testing.T) {
	s := cfsIdle(t)
	block := BuildStateBlock(s, DeriveActivityState(s, nil), nil)
	if !block.CFSConnected || block.CFS == nil || block.CFS.State != CFSStateIdle {
		t.Fatalf("connected idle: cfs_connected=%v cfs=%+v, want idle", block.CFSConnected, block.CFS)
	}
	s.WS9999.FeedState = present(2)
	block = BuildStateBlock(s, DeriveActivityState(s, nil), nil)
	if block.CFS == nil || block.CFS.State != CFSStateBusy || len(block.CFS.Reasons) == 0 {
		t.Fatalf("feeding: cfs=%+v, want busy with reasons", block.CFS)
	}
	plain := syntheticIdle()
	if b := BuildStateBlock(plain, DeriveActivityState(plain, nil), nil); b.CFS != nil {
		t.Fatalf("no CFS: cfs=%+v, want nil", b.CFS)
	}
}
