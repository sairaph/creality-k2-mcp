// The per-printer lock file identity (safety-architecture 3.4). No locking
// logic lives here yet; this only names the path so it can be agreed on
// before the locking mechanism (gofrs/flock) is added.
package domain

import (
	"fmt"
	"path/filepath"
	"strings"
)

// LockPath returns ~/.creality-k2-mcp/locks/<identity>.lock.
//
// identity must be the printer's resolved identity: the live Klipper
// hostname from a fresh printer/info read, verified against the registry's
// own persisted hostname when the printer has one. It is deliberately not
// the registry id, so two registry entries that somehow point at the same
// physical printer still serialize against each other rather than racing,
// and deliberately not a fallback to the configured host: a write is
// refused, never locked on the host, whenever the live hostname is not yet
// known or does not match what was persisted (review backlog item 24 - a
// registry entry's address can outlive the physical printer it was set up
// for, e.g. DHCP moving the IP to a different machine).
//
// This is how the K2_MCP_HOST environment override (env.go) gets a lock
// identity despite its Printer having no Hostname field: it is never
// persisted, so there is no registry entry to read a hostname from. The
// caller (internal/policy Execute, resolveExecuteIdentity) takes a fresh
// printer/info read before locking and uses its Klipper hostname as
// identity for every printer, override or registry-backed alike, refusing
// the write instead of locking on the host whenever that read fails, comes
// back empty, or (for a registry-backed printer) disagrees with the
// persisted hostname - so the lock is never taken under one identity (host,
// or a stale persisted hostname) only to be replaced by another (the live
// hostname) as soon as printer/info answers, and two processes can never
// briefly hold different lock keys for the same physical printer.
func LockPath(identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", fmt.Errorf("lock identity must not be empty")
	}
	if strings.ContainsAny(identity, `/\`) {
		return "", fmt.Errorf("lock identity %q must not contain a path separator", identity)
	}
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "locks", identity+".lock"), nil
}
