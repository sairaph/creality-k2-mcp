package camera

import (
	"context"
	"testing"
	"time"
)

func TestSnapshotDecodesTo1280x720(t *testing.T) {
	host := startFakePrinter(t, fakePrinterOptions{autoRepeatKeyframes: true})

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()

	before := time.Now()
	result, err := Snapshot(ctx, host)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	after := time.Now()

	if result.Image == nil {
		t.Fatal("Snapshot result has a nil Image")
	}
	if result.Width != 1280 || result.Height != 720 {
		t.Fatalf("Snapshot result is %dx%d, want 1280x720", result.Width, result.Height)
	}
	bounds := result.Image.Bounds()
	if bounds.Dx() != result.Width || bounds.Dy() != result.Height {
		t.Fatalf("Image bounds %dx%d do not match reported %dx%d", bounds.Dx(), bounds.Dy(), result.Width, result.Height)
	}
	if result.CapturedAt.Before(before) || result.CapturedAt.After(after) {
		t.Fatalf("CapturedAt %v is not between %v and %v", result.CapturedAt, before, after)
	}
}

func TestSnapshotFailsWhenNoPrinterAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// Port 1 is reserved and nothing listens there, so this must fail
	// promptly rather than hang for the full 20s snapshot budget.
	start := time.Now()
	_, err := Snapshot(ctx, "127.0.0.1:1")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Snapshot against an unreachable host: want error, got nil")
	}
	if elapsed > 6*time.Second {
		t.Fatalf("Snapshot against an unreachable host took %s, want a prompt failure", elapsed)
	}
}
