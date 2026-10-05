package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sairaph/creality-k2-mcp/internal/camera"
)

// Field feedback item 3 (plan-v0.3.1.md R5): the caller's budget bounds the
// daemon's wait, and a failed snapshot does not keep the camera connection
// open for the keep-warm minute.

func TestHub_Snapshot_CallerBudgetBoundsTheWait(t *testing.T) {
	origBudget := hubSnapshotBudget
	hubSnapshotBudget = 10 * time.Second
	t.Cleanup(func() { hubSnapshotBudget = origBudget })

	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	start := time.Now()
	_, err := h.Snapshot(context.Background(), "printer-budget", 80*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Snapshot waited %s, want about the caller's 80ms budget", elapsed)
	}
	var nk *camera.NoKeyframeError
	if !errors.As(err, &nk) || nk.Wait != 80*time.Millisecond {
		t.Fatalf("err = %v, want a NoKeyframeError with the caller's budget", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v does not match context.DeadlineExceeded", err)
	}
}

func TestHub_Snapshot_FailureIsNotKeptWarm(t *testing.T) {
	origBudget := hubSnapshotBudget
	hubSnapshotBudget = 60 * time.Millisecond
	t.Cleanup(func() { hubSnapshotBudget = origBudget })
	origWarm := hubKeepWarmDuration
	hubKeepWarmDuration = time.Minute
	t.Cleanup(func() { hubKeepWarmDuration = origWarm })

	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	if _, err := h.Snapshot(context.Background(), "printer-cold-fail", 0); err == nil {
		t.Fatal("Snapshot with no keyframe ever sent: want an error")
	}
	waitFor(t, 2*time.Second, func() bool { return sess.closeCount() == 1 })
	if h.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d after a failed snapshot, want 0 (not kept warm)", h.ActiveCount())
	}
}

// Review m2: a snapshot cancelled from outside is reported as cancelled, not as
// a camera that delivered no keyframe.
func TestHub_Snapshot_OutsideCancelIsNotNoKeyframe(t *testing.T) {
	origBudget := hubSnapshotBudget
	hubSnapshotBudget = 10 * time.Second
	t.Cleanup(func() { hubSnapshotBudget = origBudget })

	sess := newFakeSession()
	opener := &fakeOpener{}
	opener.queue(sess)
	h := NewHub(opener)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := h.Snapshot(ctx, "printer-cancelled", 0)
	var nk *camera.NoKeyframeError
	if errors.As(err, &nk) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled and no NoKeyframeError", err)
	}
}
