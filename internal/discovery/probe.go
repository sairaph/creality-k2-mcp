package discovery

import (
	"context"
	"fmt"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// ProbeHost identifies one manually entered printer without scanning
// (plan-v0.1.0.md decision 3): the wizard's "add a host manually" key, the
// TUI's add-by-host action, and a CLI or tool call that names a host
// directly all share this path instead of re-deriving the identification
// sequence. host is validated with domain.ValidateHost before anything is
// dialed. port 0 defaults to domain.DefaultMoonrakerPort.
func ProbeHost(ctx context.Context, host string, port int) (Result, error) {
	return ProbeHostAt(ctx, host, port, 0)
}

// ProbeHostAt is ProbeHost with the port 9999 client's port also
// overridable, so tests can point it at a fake server on another port
// instead of the real 9999. wsPort 0 defaults to crealityws.DefaultPort.
func ProbeHostAt(ctx context.Context, host string, port, wsPort int) (Result, error) {
	if err := domain.ValidateHost(host); err != nil {
		return Result{}, fmt.Errorf("discovery: probe host: %w", err)
	}
	if port == 0 {
		port = domain.DefaultMoonrakerPort
	}
	if wsPort == 0 {
		wsPort = crealityws.DefaultPort
	}
	return identify(ctx, host, port, wsPort), nil
}
