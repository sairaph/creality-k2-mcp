// Settings/config, in the same file style as this author's other mcp-wizard
// projects (interactive-terminal-mcp, sana-mcp): a versioned TOML document
// under the per-user application directory, defaults returned when the file
// is missing, atomic save, and a Validate that never writes.
package domain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"
)

const settingsFileName = "config.toml"
const settingsVersion = 1

// idleHeatMinutesMin and idleHeatMinutesMax bound idle_heat_minutes
// (safety-architecture D2). The floor keeps the watchdog meaningful; the
// ceiling keeps "idle heating" from becoming "heating left on indefinitely"
// by configuration alone.
const (
	idleHeatMinutesMin = 1
	idleHeatMinutesMax = 240
)

// ToolPreset selects which categories of tools are registered
// (safety-architecture D6).
type ToolPreset string

const (
	// PresetMonitor: status, files read, history, console.
	PresetMonitor ToolPreset = "monitor"
	// PresetCamera: monitor plus camera.
	PresetCamera ToolPreset = "camera"
	// PresetControl: camera plus control tools.
	PresetControl ToolPreset = "control"
)

func (p ToolPreset) valid() bool {
	switch p {
	case PresetMonitor, PresetCamera, PresetControl:
		return true
	default:
		return false
	}
}

// ToolCategory classifies a tool for the preset calculation. It is coarser
// than the tool surface in the plan: registry/status/files/history/console
// tools are ToolCategoryMonitor, camera tools are ToolCategoryCamera, and
// print/temperature/fan/factor/light/exclude/upload/delete-write tools are
// ToolCategoryControl.
type ToolCategory string

const (
	ToolCategoryMonitor ToolCategory = "monitor"
	ToolCategoryCamera  ToolCategory = "camera"
	ToolCategoryControl ToolCategory = "control"
)

// ToolInfo is what EnabledTools needs to know about one registerable tool.
type ToolInfo struct {
	Name     string
	Category ToolCategory
}

// Bands are the mid-print setpoint bands (safety-architecture D1): a change
// inside the band applies directly; outside it the tool refuses and reports
// the configured band. Defaults are engineering judgment, not
// evidence-derived, and are user-configurable.
type Bands struct {
	// NozzleBandC bounds a nozzle temperature change to +-this many degrees
	// C of the currently active target.
	NozzleBandC float64 `toml:"nozzle_band_c"`
	// BedBandC bounds a bed temperature change to +-this many degrees C of
	// the currently active target.
	BedBandC float64 `toml:"bed_band_c"`
	// PartFanMinPercentOfCurrent bounds how far a part fan change may drop
	// the fan below its current percent, e.g. 50 means a running 80% fan may
	// not be set below 40% in one call.
	PartFanMinPercentOfCurrent float64 `toml:"part_fan_min_percent_of_current"`
	// SpeedFactorMinPercent and SpeedFactorMaxPercent bound M220.
	SpeedFactorMinPercent float64 `toml:"speed_factor_min_percent"`
	SpeedFactorMaxPercent float64 `toml:"speed_factor_max_percent"`
	// FlowFactorMinPercent and FlowFactorMaxPercent bound M221.
	FlowFactorMinPercent float64 `toml:"flow_factor_min_percent"`
	FlowFactorMaxPercent float64 `toml:"flow_factor_max_percent"`
}

// DefaultBands are the safety-architecture D1 defaults.
func DefaultBands() Bands {
	return Bands{
		NozzleBandC:                10,
		BedBandC:                   5,
		PartFanMinPercentOfCurrent: 50,
		SpeedFactorMinPercent:      50,
		SpeedFactorMaxPercent:      150,
		FlowFactorMinPercent:       90,
		FlowFactorMaxPercent:       110,
	}
}

// ToolSettings is the tool preset plus per-tool overrides
// (safety-architecture D6).
type ToolSettings struct {
	Preset ToolPreset `toml:"preset"`
	// Overrides forces a tool's enabled state regardless of preset, keyed by
	// exact tool name.
	Overrides map[string]bool `toml:"overrides,omitempty"`
}

// Settings is the complete persisted settings document.
type Settings struct {
	Version int `toml:"version"`

	Bands Bands `toml:"bands"`

	// IdleHeatMinutes is how long a heater set while idle runs before the
	// idle-heat watchdog turns it off automatically (safety-architecture D2).
	IdleHeatMinutes int `toml:"idle_heat_minutes"`

	Tools ToolSettings `toml:"tools"`
}

// DefaultSettings returns the recommended configuration. The tool preset
// defaults to "camera": monitor and camera tools only, with control tools
// not registered at all. This is a conservative default, not a redundant
// one: per-printer allow_control (registry.go) and the per-action policy
// (a later task) are additional gates on top of a control tool being
// registered, not a substitute for it, so defaulting to the full surface
// would expose control tools to any client before the user has decided
// they want that. Control is enabled explicitly, by the user, in the
// install wizard or in settings.
func DefaultSettings() Settings {
	return Settings{
		Version:         settingsVersion,
		Bands:           DefaultBands(),
		IdleHeatMinutes: 15,
		Tools: ToolSettings{
			Preset: PresetCamera,
		},
	}
}

// SettingsPath is ~/.creality-k2-mcp/config.toml.
func SettingsPath() (string, error) {
	dir, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, settingsFileName), nil
}

// LoadSettings reads settings from path, writing the defaults first when no
// file exists so a fresh install has a readable, editable document
// immediately (matching this author's other mcp-wizard projects).
func LoadSettings(path string) (Settings, error) {
	cfg := DefaultSettings()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := SaveSettings(path, cfg); err != nil {
			return Settings{}, err
		}
		return normalizeSettings(cfg)
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings %s: %w", path, err)
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	return normalizeSettings(cfg)
}

// ReadSettings reads settings from path without ever writing anything: a
// missing file yields DefaultSettings() and leaves it to the caller to save
// it (SaveSettings), unlike LoadSettings, which persists the defaults
// immediately when the file is missing. This is what the interactive
// Settings step uses to open the step, including in --dry-run, without
// creating config.toml just by looking at it.
func ReadSettings(path string) (Settings, error) {
	cfg := DefaultSettings()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return normalizeSettings(cfg)
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings %s: %w", path, err)
	}
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	return normalizeSettings(cfg)
}

// SaveSettings validates and atomically publishes settings to path with mode
// 0600.
func SaveSettings(path string, cfg Settings) error {
	if _, err := normalizeSettings(cfg); err != nil {
		return err
	}
	raw, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	return WriteFileAtomic(path, raw, 0o600)
}

// ValidateSettings checks every persisted setting without writing anything.
func ValidateSettings(cfg Settings) error {
	_, err := normalizeSettings(cfg)
	return err
}

func normalizeSettings(cfg Settings) (Settings, error) {
	if cfg.Version != settingsVersion {
		return Settings{}, fmt.Errorf("unsupported settings version %d; this binary understands version %d", cfg.Version, settingsVersion)
	}
	if err := validateBands(cfg.Bands); err != nil {
		return Settings{}, err
	}
	if cfg.IdleHeatMinutes < idleHeatMinutesMin || cfg.IdleHeatMinutes > idleHeatMinutesMax {
		return Settings{}, fmt.Errorf("idle_heat_minutes must be between %d and %d", idleHeatMinutesMin, idleHeatMinutesMax)
	}
	if !cfg.Tools.Preset.valid() {
		return Settings{}, fmt.Errorf("tools.preset must be one of %q, %q, %q", PresetMonitor, PresetCamera, PresetControl)
	}
	return cfg, nil
}

func validateBands(b Bands) error {
	if b.NozzleBandC <= 0 || b.NozzleBandC > 100 {
		return fmt.Errorf("bands.nozzle_band_c must be between 0 and 100")
	}
	if b.BedBandC <= 0 || b.BedBandC > 100 {
		return fmt.Errorf("bands.bed_band_c must be between 0 and 100")
	}
	if b.PartFanMinPercentOfCurrent < 0 || b.PartFanMinPercentOfCurrent > 100 {
		return fmt.Errorf("bands.part_fan_min_percent_of_current must be between 0 and 100")
	}
	if b.SpeedFactorMinPercent <= 0 || b.SpeedFactorMinPercent > b.SpeedFactorMaxPercent {
		return fmt.Errorf("bands.speed_factor_min_percent must be positive and at most speed_factor_max_percent")
	}
	if b.SpeedFactorMaxPercent > 1000 {
		return fmt.Errorf("bands.speed_factor_max_percent must be at most 1000")
	}
	if b.FlowFactorMinPercent <= 0 || b.FlowFactorMinPercent > b.FlowFactorMaxPercent {
		return fmt.Errorf("bands.flow_factor_min_percent must be positive and at most flow_factor_max_percent")
	}
	if b.FlowFactorMaxPercent > 1000 {
		return fmt.Errorf("bands.flow_factor_max_percent must be at most 1000")
	}
	return nil
}

// EnabledTools computes which tools should be registered given the full tool
// list: a tool's category decides its default from the preset (monitor tools
// are always on; camera tools are on for the camera and control presets;
// control tools are on only for the control preset), and a per-tool override
// wins over the preset when present.
func (s Settings) EnabledTools(tools []ToolInfo) map[string]bool {
	enabled := make(map[string]bool, len(tools))
	for _, t := range tools {
		if override, ok := s.Tools.Overrides[t.Name]; ok {
			enabled[t.Name] = override
			continue
		}
		enabled[t.Name] = s.presetIncludes(t.Category)
	}
	return enabled
}

// UnknownToolOverrides returns, sorted, every name in
// s.Tools.Overrides that matches no tool in tools. Settings stay valid
// either way (normalizeSettings never rejects an override by name;
// EnabledTools simply never consults one that names nothing real), so this
// is only a warning helper: the MCP server calls it once at startup to warn
// on stderr about a likely config.toml typo, and `doctor` (a later task)
// reuses it so both report the same names the same way.
func (s Settings) UnknownToolOverrides(tools []ToolInfo) []string {
	known := make(map[string]bool, len(tools))
	for _, t := range tools {
		known[t.Name] = true
	}
	var unknown []string
	for name := range s.Tools.Overrides {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	return unknown
}

func (s Settings) presetIncludes(category ToolCategory) bool {
	switch category {
	case ToolCategoryMonitor:
		return true
	case ToolCategoryCamera:
		return s.Tools.Preset == PresetCamera || s.Tools.Preset == PresetControl
	case ToolCategoryControl:
		return s.Tools.Preset == PresetControl
	default:
		return false
	}
}
