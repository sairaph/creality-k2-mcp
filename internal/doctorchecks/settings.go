package doctorchecks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// SettingsCheck reports config.toml's path, tool preset, bands, idle heat
// minutes and any tools.overrides entry that names no known tool. It never
// writes: it uses domain.ReadSettings (not LoadSettings), which never
// creates config.toml on a fresh install just because doctor looked at it.
type SettingsCheck struct {
	// KnownTools, when non-empty, is compared against settings.Tools.Overrides
	// via domain.Settings.UnknownToolOverrides to name every override that
	// matches no real tool. It is empty in production today: the full tool
	// list (domain.ToolInfo per registered tool) is accumulated privately by
	// internal/mcpserver.newServer (its unexported allToolInfos), which has
	// no exported accessor doctor can call without starting a whole server.
	// Wiring this up needs one small export from internal/mcpserver (e.g. a
	// Server.ToolInfos() method); until then, this check reports whichever
	// override names are configured without claiming to know which are
	// unknown.
	KnownTools []domain.ToolInfo
}

func (SettingsCheck) Name() string { return "Settings" }

func (c SettingsCheck) Run(_ context.Context) doctor.Result {
	path, err := domain.SettingsPath()
	if err != nil {
		return doctor.Result{Name: "Settings", Status: doctor.Fail, Detail: err.Error()}
	}
	cfg, err := domain.ReadSettings(path)
	if err != nil {
		return doctor.Result{Name: "Settings", Status: doctor.Fail, Detail: fmt.Sprintf("%s: %v", path, err)}
	}

	lines := []string{
		path,
		fmt.Sprintf("tool preset: %s", cfg.Tools.Preset),
		fmt.Sprintf("bands: nozzle +/-%g C, bed +/-%g C, part fan floor %g%% of current, speed %g-%g%%, flow %g-%g%%",
			cfg.Bands.NozzleBandC, cfg.Bands.BedBandC, cfg.Bands.PartFanMinPercentOfCurrent,
			cfg.Bands.SpeedFactorMinPercent, cfg.Bands.SpeedFactorMaxPercent,
			cfg.Bands.FlowFactorMinPercent, cfg.Bands.FlowFactorMaxPercent),
		fmt.Sprintf("idle heat minutes: %d", cfg.IdleHeatMinutes),
	}

	status := doctor.OK
	names := overrideNames(cfg)
	switch {
	case len(names) == 0:
		// no overrides configured; nothing to warn about.
	case len(c.KnownTools) == 0:
		lines = append(lines, "tools.overrides configured: "+strings.Join(names, ", ")+
			" (doctor cannot check these against the registered tool set in this build)")
	default:
		unknown := cfg.UnknownToolOverrides(c.KnownTools)
		if len(unknown) > 0 {
			status = doctor.Warn
			lines = append(lines, "tools.overrides names unknown tool(s): "+strings.Join(unknown, ", "))
		} else {
			lines = append(lines, "tools.overrides configured: "+strings.Join(names, ", "))
		}
	}

	return doctor.Result{Name: "Settings", Status: status, Detail: strings.Join(lines, "\n")}
}

// overrideNames returns cfg.Tools.Overrides's keys, sorted, matching the
// order domain.Settings.UnknownToolOverrides already sorts its own result
// in.
func overrideNames(cfg domain.Settings) []string {
	names := make([]string, 0, len(cfg.Tools.Overrides))
	for name := range cfg.Tools.Overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
