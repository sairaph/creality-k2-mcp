package doctorchecks

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

func TestSettingsCheckDefaultsAreOK(t *testing.T) {
	setTestHome(t)

	res := SettingsCheck{}.Run(context.Background())
	if res.Status != doctor.OK {
		t.Fatalf("Status = %v, Detail = %q, want OK", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "tool preset: "+string(domain.PresetCamera)) {
		t.Errorf("Detail = %q, want the default camera preset", res.Detail)
	}
	if !strings.Contains(res.Detail, "idle heat minutes: 15") {
		t.Errorf("Detail = %q, want the default idle heat minutes", res.Detail)
	}
}

// ReadSettings, not LoadSettings, must be used: doctor never writes
// config.toml just by looking at it (AGENTS.md/plan-v0.1.0.md's "no writes
// to printers or files").
func TestSettingsCheckNeverWritesConfigFile(t *testing.T) {
	setTestHome(t)

	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	SettingsCheck{}.Run(context.Background())
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("config.toml exists at %s after Run; SettingsCheck must never write it", path)
	}
}

func TestSettingsCheckUnverifiableOverridesAreNotedNotFlaggedUnknown(t *testing.T) {
	setTestHome(t)
	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg := domain.DefaultSettings()
	cfg.Tools.Overrides = map[string]bool{"totally_made_up_tool": true}
	if err := domain.SaveSettings(path, cfg); err != nil {
		t.Fatal(err)
	}

	res := SettingsCheck{}.Run(context.Background())
	if res.Status != doctor.OK {
		t.Fatalf("Status = %v, want OK when KnownTools is empty (nothing to compare against)", res.Status)
	}
	if !strings.Contains(res.Detail, "totally_made_up_tool") {
		t.Errorf("Detail = %q, want the configured override name reported", res.Detail)
	}
}

func TestSettingsCheckFlagsUnknownOverridesWhenKnownToolsGiven(t *testing.T) {
	setTestHome(t)
	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg := domain.DefaultSettings()
	cfg.Tools.Overrides = map[string]bool{"list_printers": true, "not_a_real_tool": false}
	if err := domain.SaveSettings(path, cfg); err != nil {
		t.Fatal(err)
	}

	res := SettingsCheck{KnownTools: []domain.ToolInfo{
		{Name: "list_printers", Category: domain.ToolCategoryMonitor},
	}}.Run(context.Background())
	if res.Status != doctor.Warn {
		t.Fatalf("Status = %v, Detail = %q, want Warn", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "not_a_real_tool") {
		t.Errorf("Detail = %q, want the unknown override named", res.Detail)
	}
	if strings.Contains(res.Detail, "names unknown tool(s): list_printers") {
		t.Errorf("Detail = %q, must not flag a known override as unknown", res.Detail)
	}
}
