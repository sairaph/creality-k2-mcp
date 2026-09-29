package moonraker

import (
	"context"
	"net/url"
	"time"
)

// This file holds the print-lifecycle write methods. Moonraker answers all
// four of these with {"result": "ok"} regardless of printer state
// (control_test_20260928.md: pause-while-paused, cancel-while-idle and
// resume-while-idle were all accepted), so this package sends exactly what
// it is asked to send and reports Moonraker's answer. Refusing a
// state-inconsistent request (the single most load-bearing guard in this
// project, per safety-architecture.md's resume_print hazard) is the
// caller's job in a later task, not this package's: this is the command
// layer, not the policy layer.

// PrintStart starts printing filename, which must already exist in the
// gcodes root (Moonraker resolves it there).
func (c *Client) PrintStart(ctx context.Context, filename string) error {
	query := url.Values{"filename": {filename}}
	return c.postForm(ctx, "PrintStart", "/printer/print/start", query, timeoutStatus, nil)
}

// timeoutLifecycle bounds pause, resume and cancel. Moonraker answers these
// only after the whole macro has run, and on a K2 with a CFS PAUSE parks,
// retracts and wipes the nozzle, which took longer than timeoutStatus in the
// supervised print (2026-09-29): the pause happened but the call timed out.
const timeoutLifecycle = 120 * time.Second

// PrintPause pauses the current print.
func (c *Client) PrintPause(ctx context.Context) error {
	return c.postForm(ctx, "PrintPause", "/printer/print/pause", nil, timeoutLifecycle, nil)
}

// PrintResume resumes the current print.
func (c *Client) PrintResume(ctx context.Context) error {
	return c.postForm(ctx, "PrintResume", "/printer/print/resume", nil, timeoutLifecycle, nil)
}

// PrintCancel cancels the current print.
func (c *Client) PrintCancel(ctx context.Context) error {
	return c.postForm(ctx, "PrintCancel", "/printer/print/cancel", nil, timeoutLifecycle, nil)
}
