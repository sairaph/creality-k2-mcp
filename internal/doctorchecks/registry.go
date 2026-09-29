package doctorchecks

import (
	"context"
	"fmt"
	"strings"

	"github.com/sairaph/mcp-wizard/doctor"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// RegistryCheck reports which registry file this process would use, how
// many printers it holds and how many are enabled, and any entry
// LoadRegistryFile dropped (invalid, or a hostname duplicate) rather than
// failing the whole load. It never writes.
type RegistryCheck struct{}

func (RegistryCheck) Name() string { return "Registry" }

func (RegistryCheck) Run(_ context.Context) doctor.Result {
	reg, source, warnings, err := loadEffectiveRegistry()
	if err != nil {
		return doctor.Result{Name: "Registry", Status: doctor.Fail, Detail: err.Error()}
	}

	enabled := reg.Enabled()
	detail := fmt.Sprintf("%s: %d printer(s), %d enabled", source, len(reg.Printers), len(enabled))
	if len(warnings) > 0 {
		detail += "\ndropped at load: " + strings.Join(warnings, "; ")
	}

	if len(enabled) == 0 {
		detail += "\nno printers are enabled; run `" + domain.BinaryName + " install` (or, in a project, `" +
			domain.BinaryName + " add`), `" + domain.BinaryName + " printers add <host>`, or run `" +
			domain.BinaryName + "` with no arguments for the TUI"
		return doctor.Result{Name: "Registry", Status: doctor.Warn, Detail: detail}
	}
	if len(warnings) > 0 {
		return doctor.Result{Name: "Registry", Status: doctor.Warn, Detail: detail}
	}
	return doctor.Result{Name: "Registry", Status: doctor.OK, Detail: detail}
}
