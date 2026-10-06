package userhome_test

import (
	"os"
	"path/filepath"
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

func TestDirPanicsOnTheRealHomeEvenWithAStaleMarker(t *testing.T) {
	real, err := userhome.RealHome()
	if err != nil {
		t.Skipf("real home not resolvable: %v", err)
	}
	t.Setenv(userhome.RealHomeEnv, filepath.Join(t.TempDir(), "stale"))
	t.Setenv("HOME", real)
	t.Setenv("USERPROFILE", real)
	defer mustPanic(t, "REAL home")
	userhome.Dir()
}

// A marker left in the environment (by an enclosing process, or forged)
// must not make testhome skip isolation or record the wrong real home.
func TestRunFuncIgnoresAPreSetMarker(t *testing.T) {
	real, err := userhome.RealHome()
	if err != nil {
		t.Skipf("real home not resolvable: %v", err)
	}
	stale := filepath.Join(t.TempDir(), "stale")
	t.Setenv(userhome.RealHomeEnv, stale)
	before := os.Getenv("USERPROFILE")

	ran := false
	code := testhome.RunFunc(func() int {
		ran = true
		if got := os.Getenv(userhome.RealHomeEnv); !strings.EqualFold(got, real) {
			t.Errorf("marker = %q, want the freshly computed real home %q", got, real)
		}
		if got := os.Getenv("USERPROFILE"); got == before || strings.EqualFold(got, real) {
			t.Errorf("USERPROFILE = %q: not isolated", got)
		}
		return 0
	})
	if !ran || code != 0 {
		t.Fatalf("ran = %v, code = %d", ran, code)
	}
	if got := os.Getenv(userhome.RealHomeEnv); got != stale {
		t.Fatalf("marker not restored: %q", got)
	}
	if got := os.Getenv("USERPROFILE"); got != before {
		t.Fatalf("USERPROFILE not restored: %q", got)
	}
}

func TestShortenReplacesTheHomePrefixOfPaths(t *testing.T) {
	home, err := userhome.Dir()
	if err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.Separator)
	cases := []struct{ in, want string }{
		{home + sep + "snap.jpg", "~" + sep + "snap.jpg"},
		{"opened " + home + " now", "opened ~ now"},
		{"a " + home + sep + "x and " + home + sep + "y", "a ~" + sep + "x and ~" + sep + "y"},
		{home + "2" + sep + "other", home + "2" + sep + "other"},
		{"no path here", "no path here"},
	}
	for _, tc := range cases {
		if got := userhome.Shorten(tc.in); got != tc.want {
			t.Errorf("Shorten(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
