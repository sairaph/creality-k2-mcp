package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// resume_print with a CFS connected (dev_docs/plan-v0.2.0.md sections 3.5 and
// 8a.5, decision V4). Resuming after a CFS runout or error is unverified on
// this printer, and Creality's own client checks nothing before it sends the
// resume, so a resume from this server is allowed only for a clean pause that
// THIS server process issued: the pause is recorded (job identity, box.map,
// box.enable, a hash of every slot definition) only when the CFS was Known
// and error free when the pause settled, and resume requires the record for
// the same job, a clean CFS, and the recorded values unchanged. The record
// lives in process memory: a second MCP process or a restart refuses to
// resume, which is safe and is said in the refusal.

// noPauseRecordReason is the refusal for a resume with no pause record; the gate
// (checkCFS, so the actions list and Execute agree) and resumeBinding share it.
const noPauseRecordReason = "no clean pause was recorded by this server for this job"

const resumeRefusal = "this pause was not issued by this server, or the CFS reported an error/runout, or something was changed at the printer since; resume on the printer screen or in Creality Print"

// pauseRecord is what a clean CFS pause leaves behind.
type pauseRecord struct {
	at       time.Time // when the record was made; older snapshots never clear it
	job      *printerstate.JobIdentity
	mapStr   string
	enable   string
	slotHash string
}

func (l *printerLock) setPauseRec(r *pauseRecord) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	l.pauseRec = r
}

func (l *printerLock) getPauseRec() *pauseRecord {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	return l.pauseRec
}

func (l *printerLock) clearPauseRecIf(rec *pauseRecord) {
	l.pendingMu.Lock()
	defer l.pendingMu.Unlock()
	if l.pauseRec == rec {
		l.pauseRec = nil
	}
}

// observePause clears the pause record whenever a snapshot for this identity
// POSITIVELY shows the job not paused (print_stats and pause_resume both read
// and not paused) or a different job (filename, uuid or start_time), so a later
// pause at the printer screen (or a runout pause after our resume) can never
// match a stale "paused by this server" record (plan 8a.5). A partial snapshot
// (a Moonraker field missing) clears nothing (safety review m7), and a snapshot
// taken before the record was made never clears it (m4). The clear is a
// compare-and-clear under the lock.
func (l *printerLock) observePause(snap printerstate.Snapshot) {
	rec := l.getPauseRec()
	if rec == nil || snap.Taken.Before(rec.at) {
		return
	}
	if snap.PrintStats == nil || snap.PauseResume == nil || snap.PauseResume.IsPaused == nil {
		return
	}
	notPaused := snap.PrintStats.State != "paused" || !*snap.PauseResume.IsPaused
	jobChanged := snap.PrintStats.Filename != "" && startJobChanged(rec.job, printerstate.JobIdentityFrom(snap))
	if notPaused || jobChanged {
		l.clearPauseRecIf(rec)
	}
}

// pauseRecordReadTimeout bounds the 9999 read taken under the per-printer lock
// on the pause and resume paths (safety review M4): a slow or half-dead 9999
// must not keep a following cancel waiting behind the lock. A variable so a
// test can shrink it.
var pauseRecordReadTimeout = 2 * time.Second

// boxMapString renders box.map deterministically, or "absent".
func boxMapString(box *moonraker.Box) string {
	if box == nil || box.Map == nil {
		return "absent"
	}
	keys := make([]string, 0, len(box.Map))
	for k := range box.Map {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + box.Map[k] + ";")
	}
	return b.String()
}

// boxEnableString renders box.enable, or "absent".
func boxEnableString(box *moonraker.Box) string {
	if box == nil || box.Enable == nil {
		return "absent"
	}
	return fmt.Sprint(*box.Enable)
}

// slotsHash hashes every slot definition of every unit (rfid, colour, state,
// selected, type, vendor, name) plus the printer's same_material groups, so a
// change to ANY slot, mapped or not, changes the hash (plan 8a.3).
func slotsHash(b crealityws.BoxsInfo) string {
	h := sha256.New()
	ptr := func(p *int) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	for _, box := range b.MaterialBoxs {
		fmt.Fprintf(h, "box %d type %d state %d\n", box.ID, box.Type, box.State)
		for _, m := range box.Materials {
			fmt.Fprintf(h, "slot %d rfid %s color %s state %s selected %s type %s vendor %s name %s\n",
				m.ID, m.RFID, strings.ToLower(m.Color), ptr(m.State), ptr(m.Selected), m.Type, m.Vendor, m.Name)
		}
	}
	if !b.SameMaterialOK {
		fmt.Fprintln(h, "same_material unknown")
	}
	for _, g := range b.SameMaterial {
		fmt.Fprintf(h, "group %s %s %s %v\n", g.Code, g.Color, g.Name, g.Slots)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// recordPause stores the pause record after a pause settled, only when the
// CFS was Known and error free at that moment and the slot definitions could
// be read (plan 8a.5).
func recordPause(ctx context.Context, deps Deps, pl *printerLock, snap printerstate.Snapshot, derived printerstate.Derived) {
	pl.setPauseRec(nil)
	if !derived.CFSConnected || !derived.CFSKnown || derived.CFSError || snap.Box == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, pauseRecordReadTimeout)
	defer cancel()
	boxs, err := deps.WS9999.BoxsInfo(rctx)
	if err != nil {
		return
	}
	pl.setPauseRec(&pauseRecord{
		at:       time.Now(),
		job:      printerstate.JobIdentityFrom(snap),
		mapStr:   boxMapString(snap.Box),
		enable:   boxEnableString(snap.Box),
		slotHash: slotsHash(boxs),
	})
}

// resumeBinding checks every section 3.5 / 8a.5 condition for resume_print
// with a CFS connected and returns the values bound into the proposal token.
// Known and no error are already enforced by checkCFS (cfsResume); this adds
// the ones that need the snapshot, the pause record and a fresh boxsInfo.
func resumeBinding(ctx context.Context, deps Deps, pl *printerLock, snap printerstate.Snapshot, derived printerstate.Derived) (*binding, *Error) {
	refuse := func(why string) *Error {
		return &Error{Action: ActionResumePrint, Code: CodeUnavailable, Message: "resume_print with a CFS connected is refused: " + why + ". " + resumeRefusal}
	}
	rec := pl.getPauseRec()
	if rec == nil {
		return nil, refuse(noPauseRecordReason)
	}
	if len(printerstate.JobIdentityDiff(rec.job, printerstate.JobIdentityFrom(snap))) > 0 {
		return nil, refuse("the recorded pause belongs to a different job")
	}
	ws := snap.WS9999
	if !ws.DeviceState.Present || ws.DeviceState.Value == 10 || ws.DeviceState.Value == 11 {
		return nil, refuse(fmt.Sprintf("9999 deviceState is %s", optInt(ws.DeviceState)))
	}
	if !ws.FeedState.Present || !feedIdle(ws.FeedState.Value) {
		return nil, refuse(fmt.Sprintf("9999 feedState is %s, not at rest", optInt(ws.FeedState)))
	}
	if !ws.RepoPlrStatus.Present || ws.RepoPlrStatus.Value != 0 {
		return nil, refuse(fmt.Sprintf("9999 repoPlrStatus is %s, not 0", optInt(ws.RepoPlrStatus)))
	}
	if !ws.UpgradeStatus.Present || ws.UpgradeStatus.Value != 0 {
		return nil, refuse(fmt.Sprintf("9999 upgradeStatus is %s, not 0", optInt(ws.UpgradeStatus)))
	}
	if snap.PauseResume == nil || snap.PauseResume.ResumeErr == nil || *snap.PauseResume.ResumeErr {
		return nil, refuse("pause_resume.resume_err is true or absent")
	}
	if got := boxMapString(snap.Box); got != rec.mapStr {
		return nil, refuse("box.map changed since the pause")
	}
	if got := boxEnableString(snap.Box); got != rec.enable {
		return nil, refuse("box.enable changed since the pause")
	}
	rctx, cancel := context.WithTimeout(ctx, pauseRecordReadTimeout)
	defer cancel()
	boxs, err := deps.WS9999.BoxsInfo(rctx)
	if err != nil {
		return nil, refuse("the slot definitions could not be read: " + err.Error())
	}
	hash := slotsHash(boxs)
	if hash != rec.slotHash {
		return nil, refuse("a slot definition changed since the pause")
	}
	return &binding{extra: "resume|" + rec.mapStr + "|" + rec.enable + "|" + hash}, nil
}

func optInt(v crealityws.Int) string {
	if !v.Present {
		return "absent"
	}
	return fmt.Sprint(v.Value)
}

// feedIdle mirrors printerstate's idle feedState set (0, 3, 14, 100).
func feedIdle(v int) bool { return v == 0 || v == 3 || v == 14 || v == 100 }
