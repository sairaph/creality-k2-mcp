// The printer registry: on-disk locations, load/save, add and dedup by
// hostname (decision 2, safety-architecture 3.4).
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultMoonrakerPort is used when a printer entry does not specify one.
const DefaultMoonrakerPort = 7125

// registryFileName is the file name used at both the global and project
// locations.
const registryFileName = "printers.json"

// registryVersion is written to every saved registry and checked on load.
const registryVersion = 1

// Printer is one entry in the registry. Id is derived once from the Klipper
// hostname and never changes; Name defaults to the hostname and is
// user-editable. Hostname is the Klipper hostname read from Moonraker
// printer/info; ValidatePrinter requires it to be non-empty for any entry
// saved to or loaded from the registry file, since the per-printer lock and
// hostname dedup (safety-architecture.md 3.4) both need it as a stable
// identity. Model is filled in by discovery once the printer has been
// identified and may be empty.
//
// The one exception is the ad hoc K2_MCP_HOST environment override
// (env.go): it builds a Printer with no Hostname and is never saved to the
// registry file, so it never goes through ValidatePrinter's hostname check.
// See lock.go for how its identity is resolved at runtime instead.
type Printer struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Host          string    `json:"host"`
	MoonrakerPort int       `json:"moonraker_port"`
	APIKey        string    `json:"api_key,omitempty"`
	Model         string    `json:"model,omitempty"`
	Hostname      string    `json:"hostname,omitempty"`
	Enabled       bool      `json:"enabled"`
	AllowControl  bool      `json:"allow_control"`
	AddedAt       time.Time `json:"added_at"`
}

// NewPrinter builds a printer entry with the id derived from hostname, the
// name defaulted to hostname (the caller may rename it later), and the
// default Moonraker port. hostname is the Klipper hostname and must be
// non-empty for the result to pass ValidatePrinter and be saved to the
// registry file; the ad hoc K2_MCP_HOST environment override does not go
// through NewPrinter (see env.go).
func NewPrinter(hostname, host string) Printer {
	return Printer{
		ID:            DeriveID(hostname),
		Name:          hostname,
		Host:          host,
		MoonrakerPort: DefaultMoonrakerPort,
		Hostname:      hostname,
		AddedAt:       time.Now().UTC(),
	}
}

// Registry is the persisted set of known printers.
type Registry struct {
	Version  int       `json:"version"`
	Printers []Printer `json:"printers"`
}

// GlobalRegistryPath is ~/.creality-k2-mcp/printers.json.
func GlobalRegistryPath() (string, error) {
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, registryFileName), nil
}

// ProjectRegistryPath is <dir>/.creality-k2-mcp/printers.json.
func ProjectRegistryPath(dir string) string {
	return filepath.Join(dir, appDirName, registryFileName)
}

// RegistryPath resolves which file a caller should load and later save to:
// the project file when dir is non-empty and that file exists, otherwise the
// global file (whether or not it exists yet). isProject reports which one was
// chosen.
func RegistryPath(dir string) (path string, isProject bool, err error) {
	if dir != "" {
		project := ProjectRegistryPath(dir)
		if _, statErr := os.Stat(project); statErr == nil {
			return project, true, nil
		}
	}
	global, err := GlobalRegistryPath()
	if err != nil {
		return "", false, err
	}
	return global, false, nil
}

// LoadRegistryFile reads and validates the registry at path. A missing file
// is not an error: it yields an empty registry, matching a fresh install.
// Entries that fail ValidatePrinter, or that resolve to a hostname already
// seen earlier in the file, are dropped and reported as warnings rather than
// failing the whole load, so a hand-edited or legacy file does not lock the
// user out of every printer, and a malformed entry never reaches a tool.
func LoadRegistryFile(path string) (Registry, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Registry{Version: registryVersion}, nil, nil
	}
	if err != nil {
		return Registry{}, nil, fmt.Errorf("read registry %s: %w", path, err)
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return Registry{}, nil, fmt.Errorf("parse registry %s: %w", path, err)
	}
	reg, invalidWarnings := dropInvalidEntries(reg)
	reg, dedupWarnings, err := dedupeByHostname(reg)
	if err != nil {
		return Registry{}, nil, err
	}
	return reg, append(invalidWarnings, dedupWarnings...), nil
}

// dropInvalidEntries removes entries that fail ValidatePrinter on their own
// (no comparison against other entries; that is dedupeByHostname's job), so a
// hand-edited or corrupted entry is reported and skipped rather than reaching
// a tool.
func dropInvalidEntries(reg Registry) (Registry, []string) {
	kept := make([]Printer, 0, len(reg.Printers))
	var warnings []string
	for _, p := range reg.Printers {
		if err := ValidatePrinter(p); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"printer %q ignored: %v", p.ID, err))
			continue
		}
		kept = append(kept, p)
	}
	reg.Printers = kept
	return reg, warnings
}

// LoadRegistry resolves the effective registry file for dir (project file
// when present, global file otherwise) and loads it. path is the file that
// was read (or would be created by a later Save if it does not exist yet).
func LoadRegistry(dir string) (reg Registry, path string, warnings []string, err error) {
	path, _, err = RegistryPath(dir)
	if err != nil {
		return Registry{}, "", nil, err
	}
	reg, warnings, err = LoadRegistryFile(path)
	return reg, path, warnings, err
}

// dedupeByHostname runs after dropInvalidEntries, so every p here already
// passed ValidatePrinter and has a non-empty hostname; the key == "" branch
// below is defensive only and never taken from a file load.
func dedupeByHostname(reg Registry) (Registry, []string, error) {
	seen := make(map[string]string, len(reg.Printers))
	kept := make([]Printer, 0, len(reg.Printers))
	var warnings []string
	for _, p := range reg.Printers {
		key := hostnameKey(p.Hostname)
		if key == "" {
			kept = append(kept, p)
			continue
		}
		if existingID, ok := seen[key]; ok {
			warnings = append(warnings, fmt.Sprintf(
				"printer %q ignored: hostname %q is already registered as %q",
				p.ID, p.Hostname, existingID))
			continue
		}
		seen[key] = p.ID
		kept = append(kept, p)
	}
	reg.Printers = kept
	return reg, warnings, nil
}

// SaveRegistry validates and atomically publishes the registry to path with
// mode 0600 (it may hold an api_key).
func SaveRegistry(path string, reg Registry) error {
	if err := ValidateRegistry(reg); err != nil {
		return err
	}
	if reg.Version == 0 {
		reg.Version = registryVersion
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	return WriteFileAtomic(path, data, 0o600)
}

// AddPrinter appends p to reg, rejecting it if it is invalid or resolves to
// the same hostname as an existing entry. It does not save; the caller saves
// the returned registry.
func AddPrinter(reg Registry, p Printer) (Registry, error) {
	if err := ValidatePrinter(p); err != nil {
		return reg, err
	}
	if key := hostnameKey(p.Hostname); key != "" {
		for _, existing := range reg.Printers {
			if hostnameKey(existing.Hostname) == key {
				return reg, fmt.Errorf(
					"a printer with hostname %q is already registered as %q",
					p.Hostname, existing.ID)
			}
		}
	}
	for _, existing := range reg.Printers {
		if existing.ID == p.ID {
			return reg, fmt.Errorf("a printer with id %q is already registered", p.ID)
		}
	}
	reg.Printers = append(reg.Printers, p)
	return reg, nil
}

// Enabled returns the enabled printers, in registry order.
func (r Registry) Enabled() []Printer {
	var out []Printer
	for _, p := range r.Printers {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}
