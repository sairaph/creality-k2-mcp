package doctorchecks

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sairaph/mcp-wizard/doctor"
)

// The doctor's Executable check must pass for a real executable on every OS
// (on Windows mcp-wizard v0.1.1's own check always failed: no execute bits).
func TestExecutableCheck_PassesForThisTestBinary(t *testing.T) {
	r := ExecutableCheck{}.Run(context.Background())
	if r.Status != doctor.OK {
		t.Fatalf("Executable check on the running test binary: %s %s", r.Status, r.Detail)
	}
}

func TestCheckRegularFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "creality-k2-mcp.exe")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := checkRegularFile("Executable", file); r.Status != doctor.OK {
		t.Errorf("regular file: %s %s", r.Status, r.Detail)
	}
	if r := checkRegularFile("Executable", dir); r.Status != doctor.Fail {
		t.Errorf("directory: %s, want fail", r.Status)
	}
	if r := checkRegularFile("Executable", filepath.Join(dir, "missing.exe")); r.Status != doctor.Fail {
		t.Errorf("missing file: %s, want fail", r.Status)
	}
	if runtime.GOOS == "windows" {
		// The case mcp-wizard's check gets wrong: a Windows file has no execute bits.
		if r := (doctor.ExecutableCheck{Executable: file}).Run(context.Background()); r.Status == doctor.OK {
			t.Log("mcp-wizard's ExecutableCheck now passes on Windows; the wrapper can be dropped")
		}
		if r := (ExecutableCheck{Executable: file}).Run(context.Background()); r.Status != doctor.OK {
			t.Errorf("wrapper on Windows: %s %s", r.Status, r.Detail)
		}
	}
}
