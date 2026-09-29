// Package domain holds the business logic for creality-k2-mcp: the printer
// registry, printer resolution for tools, settings, fan mapping, temperature
// limits and lock identity. It does not talk to Moonraker, port 9999 or the
// network; that is internal/moonraker, internal/crealityws and
// internal/discovery.
package domain

import (
	"fmt"
)

// Identity used by install, doctor and update. Owner and Repo point at the
// GitHub repository that publishes releases.
const (
	ServerName = "creality-k2-mcp"
	BinaryName = "creality-k2-mcp"
	Owner      = "sairaph"
	Repo       = "creality-k2-mcp"
)

// AssetName maps a GOOS/GOARCH pair to the release asset published by
// .goreleaser.yml, which names binaries <project>-<os>-<arch>[.exe].
func AssetName(goos, goarch string) string {
	name := fmt.Sprintf("%s-%s-%s", BinaryName, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// DefaultEnv returns the environment variables written into AI client
// configs when the server is registered. The server reads TRANSPORT (stdio
// or http) and ADDR (listen address for http) at startup.
func DefaultEnv() map[string]string {
	return map[string]string{
		"TRANSPORT": "stdio",
	}
}
