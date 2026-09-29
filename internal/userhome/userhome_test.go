package userhome_test

import (
	"os"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/userhome"
	"github.com/sairaph/creality-k2-mcp/internal/userhome/testhome"
)

func TestMain(m *testing.M) {
	os.Exit(testhome.Run(m))
}

func mustPanic(t *testing.T, wantSubstr string) {
	t.Helper()
	r := recover()
	if r == nil {
		t.Fatal("Dir did not panic")
	}
	if s, _ := r.(string); !strings.Contains(s, wantSubstr) {
		t.Fatalf("panic = %v, want it to mention %q", r, wantSubstr)
	}
}

func TestDirReturnsTheIsolatedHome(t *testing.T) {
	home, err := userhome.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if real := os.Getenv(userhome.RealHomeEnv); real == "" || strings.EqualFold(home, real) {
		t.Fatalf("home = %q, real = %q: TestMain did not isolate", home, real)
	}
}

func TestDirPanicsOnTheRealHome(t *testing.T) {
	real := os.Getenv(userhome.RealHomeEnv)
	t.Setenv("HOME", real)
	t.Setenv("USERPROFILE", real)
	defer mustPanic(t, "REAL home")
	userhome.Dir()
}

func TestDirPanicsWhenTheBinaryWasNotIsolated(t *testing.T) {
	t.Setenv(userhome.RealHomeEnv, "")
	defer mustPanic(t, "TestMain did not isolate")
	userhome.Dir()
}
