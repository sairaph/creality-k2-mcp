package camera

import (
	"context"
	"fmt"
	"image"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/camera/decode"
)

// snapshotBudget is the overall time budget for Snapshot: opening a
// session, requesting and receiving a keyframe, and decoding it. 20s, not a
// shorter value: a snapshot opens a fresh session each time, and live
// testing against a real K2 observed cold-connect latency (session open to
// first usable keyframe) up to 16s, so a shorter budget cuts off attempts
// that would have succeeded (dev_docs/t11e-soak-report.md, dev_docs/
// camera-keyframe-rca.md).
const snapshotBudget = 20 * time.Second

// SnapshotResult is one decoded camera frame plus its capture metadata.
type SnapshotResult struct {
	Image      image.Image
	CapturedAt time.Time
	Width      int
	Height     int
}

// Snapshot opens a short-lived Session against host's camera, waits for a
// complete keyframe access unit, decodes it, and returns the resulting
// image. The whole operation (session open, keyframe wait, decode) is
// bounded by snapshotBudget regardless of ctx's own deadline.
func Snapshot(ctx context.Context, host string) (*SnapshotResult, error) {
	ctx, cancel := context.WithTimeout(ctx, snapshotBudget)
	defer cancel()

	session, err := Open(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("camera: snapshot: open session: %w", err)
	}
	defer session.Close()

	// No manual nudge needed here: Open already requested a keyframe as
	// soon as the track started, and Session keeps retrying that request
	// once a second on its own (session.go's retryPLIUntilKeyframe) until
	// its first complete keyframe arrives, which is what waitForKeyframe
	// below is waiting for. See dev_docs/camera-keyframe-rca.md.
	au, err := waitForKeyframe(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("camera: snapshot: %w", err)
	}

	// decode.DecodeKeyframe (and OpenH264 underneath it) only recognizes a
	// picture as complete once it sees the first NAL unit of the following
	// picture (see that package's doc comment); Session delivers one
	// access unit per picture, so the keyframe's own access unit alone is
	// never enough to produce a decoded frame. Append the next access unit
	// (whatever it is) purely to give the decoder that boundary; only the
	// keyframe's own bytes end up in the returned image.
	trailer, err := waitForNextAccessUnit(ctx, session)
	if err != nil {
		return nil, fmt.Errorf("camera: snapshot: %w", err)
	}
	buf := append(append([]byte(nil), au.Data...), trailer.Data...)

	dec, err := decode.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("camera: snapshot: create decoder: %w", err)
	}
	defer dec.Close(ctx)

	img, err := dec.DecodeKeyframe(ctx, buf)
	if err != nil {
		return nil, fmt.Errorf("camera: snapshot: decode keyframe: %w", err)
	}

	bounds := img.Bounds()
	return &SnapshotResult{
		Image:      img,
		CapturedAt: time.Now(),
		Width:      bounds.Dx(),
		Height:     bounds.Dy(),
	}, nil
}

// waitForKeyframe drains session's access units until a keyframe arrives,
// ctx is done, or the access unit stream ends.
func waitForKeyframe(ctx context.Context, session *Session) (AccessUnit, error) {
	for {
		select {
		case au, ok := <-session.AccessUnits():
			if !ok {
				return AccessUnit{}, fmt.Errorf("access unit stream ended before a keyframe arrived")
			}
			if au.Keyframe {
				return au, nil
			}
		case <-ctx.Done():
			return AccessUnit{}, fmt.Errorf("no keyframe received within budget: %w", ctx.Err())
		}
	}
}

// waitForNextAccessUnit returns the next access unit delivered by session,
// whatever it is.
func waitForNextAccessUnit(ctx context.Context, session *Session) (AccessUnit, error) {
	select {
	case au, ok := <-session.AccessUnits():
		if !ok {
			return AccessUnit{}, fmt.Errorf("access unit stream ended before a trailing access unit arrived")
		}
		return au, nil
	case <-ctx.Done():
		return AccessUnit{}, fmt.Errorf("no trailing access unit received within budget: %w", ctx.Err())
	}
}
