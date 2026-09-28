// The ad-hoc single-printer override read from the environment (decision 2).
package domain

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables for the single ad-hoc printer override. Set
// K2_MCP_HOST to bypass the registry entirely and talk to one printer;
// K2_MCP_PORT, K2_MCP_API_KEY and K2_MCP_ALLOW_CONTROL are optional.
const (
	EnvHost         = "K2_MCP_HOST"
	EnvPort         = "K2_MCP_PORT"
	EnvAPIKey       = "K2_MCP_API_KEY"
	EnvAllowControl = "K2_MCP_ALLOW_CONTROL"
)

// EnvPrinterID is the fixed id of the printer built from the environment
// override.
const EnvPrinterID = "env"

// EnvOverride builds the single-printer registry described by K2_MCP_HOST and
// friends. ok is false when K2_MCP_HOST is unset, meaning the caller should
// fall back to the file-based registry. Control is off unless
// K2_MCP_ALLOW_CONTROL is exactly "1".
//
// The returned Printer has no Hostname: it is built from the host the user
// gave us, before anything has talked to the printer, so there is no Klipper
// hostname yet to put in it. This is intentionally the one Printer value in
// the codebase that does not satisfy ValidatePrinter (which requires a
// hostname); that is fine because it is never passed to AddPrinter or
// SaveRegistry, and never loaded back through LoadRegistryFile. Its identity
// for locking is resolved at runtime instead: see lock.go.
func EnvOverride() (printer Printer, ok bool, err error) {
	host := strings.TrimSpace(os.Getenv(EnvHost))
	if host == "" {
		return Printer{}, false, nil
	}
	if err := ValidateHost(host); err != nil {
		return Printer{}, false, fmt.Errorf("%s: %w", EnvHost, err)
	}

	port := DefaultMoonrakerPort
	if raw := strings.TrimSpace(os.Getenv(EnvPort)); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return Printer{}, false, fmt.Errorf("%s must be a whole number", EnvPort)
		}
		if err := ValidatePort(parsed); err != nil {
			return Printer{}, false, fmt.Errorf("%s: %w", EnvPort, err)
		}
		port = parsed
	}

	return Printer{
		ID:            EnvPrinterID,
		Name:          EnvPrinterID,
		Host:          host,
		MoonrakerPort: port,
		APIKey:        os.Getenv(EnvAPIKey),
		Enabled:       true,
		AllowControl:  os.Getenv(EnvAllowControl) == "1",
		AddedAt:       time.Now().UTC(),
	}, true, nil
}
