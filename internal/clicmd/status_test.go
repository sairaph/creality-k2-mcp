package clicmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

func registerTestPrinter(t *testing.T, dir string, p domain.Printer) {
	t.Helper()
	reg, path, _, err := domain.LoadRegistry(dir)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	reg, err = domain.AddPrinter(reg, p)
	if err != nil {
		t.Fatalf("AddPrinter: %v", err)
	}
	if err := domain.SaveRegistry(path, reg); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
}

func testDeps(t *testing.T, stdout, stderr *bytes.Buffer, moon *fakeMoonrakerClient) Deps {
	t.Helper()
	return Deps{
		Stdout: stdout,
		Stderr: stderr,
		PrinterClients: func(p domain.Printer) printerstate.Deps {
			return printerstate.Deps{Moonraker: moon, WS9999: fakeWS9999Client{}}
		},
	}
}

func TestRunStatusOfflinePrinterText(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	moon := &fakeMoonrakerClient{serverInfoErr: context.DeadlineExceeded}
	deps := testDeps(t, &stdout, &stderr, moon)

	code := RunStatus(context.Background(), deps, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "State: offline") {
		t.Errorf("output missing offline state:\n%s", out)
	}
	if !strings.Contains(out, "K2-5885") {
		t.Errorf("output missing printer name:\n%s", out)
	}
	if !strings.Contains(out, "Reasons:") {
		t.Errorf("output missing reasons section:\n%s", out)
	}
	if !strings.Contains(out, "CFS connected:") || !strings.Contains(out, "9999 reachable:") {
		t.Errorf("output missing CFS/9999 fields:\n%s", out)
	}
}

func TestRunStatusJSON(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	moon := &fakeMoonrakerClient{serverInfoErr: context.DeadlineExceeded}
	deps := testDeps(t, &stdout, &stderr, moon)

	code := RunStatus(context.Background(), deps, []string{"--json"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if decoded["ActivityState"] != printerstate.StateOffline {
		t.Errorf("ActivityState = %v, want %q", decoded["ActivityState"], printerstate.StateOffline)
	}
}

func TestRunStatusNoPrinterEnabled(t *testing.T) {
	isolateHome(t)

	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{})

	code := RunStatus(context.Background(), deps, nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no printer is enabled") {
		t.Errorf("stderr = %q, want it to mention no printer is enabled", stderr.String())
	}
}

func TestRunStatusUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{})

	code := RunStatus(context.Background(), deps, []string{"a", "b"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a second positional argument", code)
	}
}

// TestRunStatusHelpFlag confirms -h/--help prints usage to stdout and exits
// 0, matching the top-level --help's own contract (main.go's cli.ErrUsage
// handling: printUsage(os.Stdout); os.Exit(0)) rather than being treated as
// an ordinary usage error (stderr, exit 2).
func TestRunStatusHelpFlag(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := testDeps(t, &stdout, &stderr, &fakeMoonrakerClient{})
			code := RunStatus(context.Background(), deps, []string{flag})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty (usage must go to stdout)", stderr.String())
			}
			if !strings.Contains(stdout.String(), "usage: status") {
				t.Errorf("stdout = %q, want it to contain the usage string", stdout.String())
			}
		})
	}
}

func TestRunStatusReportsJobProgress(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	objects := map[string]json.RawMessage{
		"print_stats":    json.RawMessage(`{"filename":"benchy.gcode","state":"printing"}`),
		"pause_resume":   json.RawMessage(`{"is_paused":false}`),
		"virtual_sdcard": json.RawMessage(`{"is_active":true,"progress":0.42,"layer":5,"layer_count":20}`),
	}
	moon := &fakeMoonrakerClient{
		serverInfo: moonraker.ServerInfoResult{KlippyConnected: true, KlippyState: "ready"},
		objects:    objects,
	}
	var stdout, stderr bytes.Buffer
	deps := testDeps(t, &stdout, &stderr, moon)

	code := RunStatus(context.Background(), deps, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Job: benchy.gcode") {
		t.Errorf("output missing job filename:\n%s", out)
	}
	if !strings.Contains(out, "progress: 42.0%") {
		t.Errorf("output missing job progress:\n%s", out)
	}
	if !strings.Contains(out, "layer: 5/20") {
		t.Errorf("output missing layer:\n%s", out)
	}
}
