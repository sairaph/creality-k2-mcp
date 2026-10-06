package doctorchecks

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/sairaph/mcp-wizard/doctor"
)

// ExecutableCheck is mcp-wizard's doctor.ExecutableCheck with one correction:
// on Windows a file has no execute permission bits (Go reports the mode as
// 0666 or 0444), so mcp-wizard v0.1.1's mode&0111 test fails every Windows
// install. On Windows this check only requires a regular file; elsewhere it
// is mcp-wizard's own check.
type ExecutableCheck struct {
	Executable string // path to check (empty = use os.Executable)
}

func (ExecutableCheck) Name() string { return "Executable" }

func (c ExecutableCheck) Run(ctx context.Context) doctor.Result {
	if runtime.GOOS != "windows" {
		return doctor.ExecutableCheck{Executable: c.Executable}.Run(ctx)
	}
	return checkRegularFile(c.Name(), c.Executable)
}

// checkRegularFile is the Windows check: the executable exists and is a
// regular file.
func checkRegularFile(name, path string) doctor.Result {
	if path == "" {
		var err error
		if path, err = os.Executable(); err != nil {
			return doctor.Result{Name: name, Status: doctor.Fail, Detail: fmt.Sprintf("cannot determine executable: %v", err)}
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return doctor.Result{Name: name, Status: doctor.Fail, Detail: fmt.Sprintf("cannot stat %s: %v", path, err)}
	}
	if !info.Mode().IsRegular() {
		return doctor.Result{Name: name, Status: doctor.Fail, Detail: fmt.Sprintf("%s is not a regular file", path)}
	}
	return doctor.Result{Name: name, Status: doctor.OK, Detail: path}
}
