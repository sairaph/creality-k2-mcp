package policy

import (
	"context"

	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

// MoonrakerClient is everything Execute needs from a printer's Moonraker
// connection: printerstate.MoonrakerClient (so this package can take its own
// fresh snapshots, P5) plus the write methods and the file-listing reads
// the action table's Send/precondition logic uses. *moonraker.Client
// satisfies this interface structurally; tests supply a fake instead
// (fakeprinter_test.go) so they never open a real socket
// (AGENTS.md "hard testing rules").
type MoonrakerClient interface {
	printerstate.MoonrakerClient

	// PrinterInfo fetches the live Klipper hostname (dev_docs/safety-
	// architecture.md 3.4, review backlog item 24). Execute
	// (resolveExecuteIdentity) uses this to resolve and verify the lock
	// identity for every printer, registry-backed or the K2_MCP_HOST
	// environment override alike: a registry-backed printer's persisted
	// hostname is only trusted once this call confirms it is still the one
	// actually answering.
	PrinterInfo(ctx context.Context) (moonraker.PrinterInfoResult, error)

	PrintStart(ctx context.Context, filename string) error
	PrintPause(ctx context.Context) error
	PrintResume(ctx context.Context) error
	PrintCancel(ctx context.Context) error
	RunTemplate(ctx context.Context, t moonraker.Template, args map[string]string) error
	Upload(ctx context.Context, localPath, remoteName string) (moonraker.UploadResult, error)
	Delete(ctx context.Context, filename string) (moonraker.DeleteResult, error)
	List(ctx context.Context) ([]moonraker.GCodeFile, error)
	Metadata(ctx context.Context, filename string) (moonraker.FileMetadata, error)
}

// WS9999Client is everything Execute needs from a printer's port-9999
// connection: printerstate.WS9999Client (so Take can read it) plus SetLight,
// the sole write this protocol offers. *crealityws.Client satisfies this
// interface structurally.
type WS9999Client interface {
	printerstate.WS9999Client

	SetLight(ctx context.Context, on bool) (confirmed bool, err error)
}

// Watchdog is D2's idle-heat watchdog (dev_docs/safety-architecture.md
// section 10 D2): a background daemon component, separate from this
// package, that turns an idle-bucket heater off automatically after
// IdleHeatArmRequest.ArmMinutes. Execute enforces D2 itself rather than
// trusting a caller to arm the daemon after the fact: an idle-bucket
// temperature write is refused outright unless the daemon reports itself
// alive, and a write that passes every other check still arms it
// synchronously, before the heater command is ever sent, refusing without
// sending if the arm call itself fails. Tests supply a fake so they never
// open a real socket (AGENTS.md hard testing rule).
type Watchdog interface {
	// Alive reports whether the background daemon is reachable right now.
	Alive(ctx context.Context) bool
	// Arm asks the daemon to turn req.Heater off after req.ArmMinutes,
	// unless a job starts or the target is changed by anyone else first
	// (D2). A non-nil error means the daemon did not accept the arm request.
	Arm(ctx context.Context, req IdleHeatArmRequest) error
	// Disarm cancels every armed idle-heat watchdog for identity
	// immediately. start_print calls this itself, synchronously, before it
	// sends anything (D2: "cancelled atomically by start_print before it
	// sends anything"), so a heater armed while idle is never turned off
	// out from under a print that is now starting. It is best-effort: an
	// error here must never block start_print (a print starting must never
	// be gated by the background daemon), and disarming when nothing is
	// armed for identity is always a no-op, never an error.
	Disarm(ctx context.Context, identity string) error
}

// Deps bundles the clients Execute needs for one printer, matching
// printerstate.Deps's shape for the Moonraker/WS9999 pair so a caller can
// build both from the same pair of concrete clients. Watchdog is optional:
// a nil value means the background daemon is not wired up at all, which
// Execute treats exactly like "not alive" (idle heating refused).
type Deps struct {
	Moonraker MoonrakerClient
	WS9999    WS9999Client
	Watchdog  Watchdog
}

// stateDeps narrows Deps to printerstate.Deps for calling
// printerstate.Take. Both fields satisfy the narrower interfaces implicitly
// because MoonrakerClient and WS9999Client above embed them.
func (d Deps) stateDeps() printerstate.Deps {
	return printerstate.Deps{Moonraker: d.Moonraker, WS9999: d.WS9999}
}
