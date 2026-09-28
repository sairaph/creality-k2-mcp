package domain

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestDefaultSettingsValid(t *testing.T) {
	if err := ValidateSettings(DefaultSettings()); err != nil {
		t.Errorf("DefaultSettings() is invalid: %v", err)
	}
}

func TestDefaultSettingsPresetIsCamera(t *testing.T) {
	if got := DefaultSettings().Tools.Preset; got != PresetCamera {
		t.Errorf("DefaultSettings().Tools.Preset = %v, want %v", got, PresetCamera)
	}
}

func TestLoadSettingsWritesDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleHeatMinutes != DefaultSettings().IdleHeatMinutes {
		t.Errorf("IdleHeatMinutes = %d, want %d", cfg.IdleHeatMinutes, DefaultSettings().IdleHeatMinutes)
	}
	if cfg.Tools.Preset != PresetCamera {
		t.Errorf("Tools.Preset = %v, want %v", cfg.Tools.Preset, PresetCamera)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected defaults to be written to %s: %v", path, err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Settings
	if err := toml.Unmarshal(written, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Tools.Preset != PresetCamera {
		t.Errorf("settings written to disk have Tools.Preset = %v, want %v", onDisk.Tools.Preset, PresetCamera)
	}
}

func TestReadSettingsNeverWritesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg, err := ReadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdleHeatMinutes != DefaultSettings().IdleHeatMinutes {
		t.Errorf("IdleHeatMinutes = %d, want %d", cfg.IdleHeatMinutes, DefaultSettings().IdleHeatMinutes)
	}
	if cfg.Tools.Preset != PresetCamera {
		t.Errorf("Tools.Preset = %v, want %v", cfg.Tools.Preset, PresetCamera)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadSettings must never create %s, stat err = %v", path, err)
	}
}

func TestReadSettingsReadsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := DefaultSettings()
	cfg.IdleHeatMinutes = 30
	if err := SaveSettings(path, cfg); err != nil {
		t.Fatal(err)
	}

	got, err := ReadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdleHeatMinutes != 30 {
		t.Errorf("IdleHeatMinutes = %d, want 30", got.IdleHeatMinutes)
	}
}

func TestSaveAndLoadSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	cfg := DefaultSettings()
	cfg.IdleHeatMinutes = 30
	cfg.Bands.NozzleBandC = 20
	cfg.Tools.Preset = PresetMonitor
	cfg.Tools.Overrides = map[string]bool{"get_camera_snapshot": true}

	if err := SaveSettings(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IdleHeatMinutes != 30 {
		t.Errorf("IdleHeatMinutes = %d, want 30", loaded.IdleHeatMinutes)
	}
	if loaded.Bands.NozzleBandC != 20 {
		t.Errorf("Bands.NozzleBandC = %v, want 20", loaded.Bands.NozzleBandC)
	}
	if loaded.Tools.Preset != PresetMonitor {
		t.Errorf("Tools.Preset = %v, want %v", loaded.Tools.Preset, PresetMonitor)
	}
	if !loaded.Tools.Overrides["get_camera_snapshot"] {
		t.Errorf("Tools.Overrides = %v, want get_camera_snapshot true", loaded.Tools.Overrides)
	}
}

func TestValidateSettingsRejectsOutOfRangeValues(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Settings)
	}{
		{"idle heat too low", func(c *Settings) { c.IdleHeatMinutes = 0 }},
		{"idle heat too high", func(c *Settings) { c.IdleHeatMinutes = 241 }},
		{"unknown preset", func(c *Settings) { c.Tools.Preset = "bogus" }},
		{"nozzle band zero", func(c *Settings) { c.Bands.NozzleBandC = 0 }},
		{"bed band negative", func(c *Settings) { c.Bands.BedBandC = -1 }},
		{"part fan floor over 100", func(c *Settings) { c.Bands.PartFanMinPercentOfCurrent = 101 }},
		{"speed min above max", func(c *Settings) { c.Bands.SpeedFactorMinPercent = 200; c.Bands.SpeedFactorMaxPercent = 150 }},
		{"flow min above max", func(c *Settings) { c.Bands.FlowFactorMinPercent = 120; c.Bands.FlowFactorMaxPercent = 110 }},
		{"wrong version", func(c *Settings) { c.Version = 99 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultSettings()
			c.mutate(&cfg)
			if err := ValidateSettings(cfg); err == nil {
				t.Error("ValidateSettings() = nil, want error")
			}
		})
	}
}

func TestEnabledToolsPresets(t *testing.T) {
	tools := []ToolInfo{
		{Name: "get_printer_status", Category: ToolCategoryMonitor},
		{Name: "get_camera_snapshot", Category: ToolCategoryCamera},
		{Name: "start_print", Category: ToolCategoryControl},
	}

	cases := []struct {
		preset ToolPreset
		want   map[string]bool
	}{
		{PresetMonitor, map[string]bool{"get_printer_status": true, "get_camera_snapshot": false, "start_print": false}},
		{PresetCamera, map[string]bool{"get_printer_status": true, "get_camera_snapshot": true, "start_print": false}},
		{PresetControl, map[string]bool{"get_printer_status": true, "get_camera_snapshot": true, "start_print": true}},
	}
	for _, c := range cases {
		t.Run(string(c.preset), func(t *testing.T) {
			cfg := DefaultSettings()
			cfg.Tools = ToolSettings{Preset: c.preset}
			got := cfg.EnabledTools(tools)
			for name, want := range c.want {
				if got[name] != want {
					t.Errorf("preset %s: EnabledTools()[%q] = %v, want %v", c.preset, name, got[name], want)
				}
			}
		})
	}
}

func TestEnabledToolsOverrideWinsOverPreset(t *testing.T) {
	tools := []ToolInfo{
		{Name: "start_print", Category: ToolCategoryControl},
		{Name: "get_printer_status", Category: ToolCategoryMonitor},
	}
	cfg := DefaultSettings()
	cfg.Tools = ToolSettings{
		Preset: PresetMonitor,
		Overrides: map[string]bool{
			"start_print":        true,  // force on despite monitor preset
			"get_printer_status": false, // force off despite monitor preset
		},
	}
	got := cfg.EnabledTools(tools)
	if !got["start_print"] {
		t.Error("start_print override = false, want true")
	}
	if got["get_printer_status"] {
		t.Error("get_printer_status override = true, want false")
	}
}

func TestUnknownToolOverridesReportsOnlyUnmatchedNames(t *testing.T) {
	tools := []ToolInfo{
		{Name: "start_print", Category: ToolCategoryControl},
		{Name: "get_printer_status", Category: ToolCategoryMonitor},
	}
	cfg := DefaultSettings()
	cfg.Tools.Overrides = map[string]bool{
		"start_print":         true,  // known: not reported
		"get_printer_statuss": false, // typo: unknown
		"set_nozle_temp":      true,  // unknown
	}

	got := cfg.UnknownToolOverrides(tools)
	want := []string{"get_printer_statuss", "set_nozle_temp"}
	if len(got) != len(want) {
		t.Fatalf("UnknownToolOverrides() = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("UnknownToolOverrides()[%d] = %q, want %q", i, got[i], name)
		}
	}
}

func TestUnknownToolOverridesEmptyWhenAllMatch(t *testing.T) {
	tools := []ToolInfo{{Name: "list_printers", Category: ToolCategoryMonitor}}
	cfg := DefaultSettings()
	cfg.Tools.Overrides = map[string]bool{"list_printers": false}

	if got := cfg.UnknownToolOverrides(tools); len(got) != 0 {
		t.Errorf("UnknownToolOverrides() = %v, want none", got)
	}
}

func TestSettingsPathUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	path, err := SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, appDirName, "config.toml")
	if path != want {
		t.Errorf("SettingsPath() = %q, want %q", path, want)
	}
}
