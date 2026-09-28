package clicmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sairaph/creality_k2_mcp/internal/discovery"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
	"github.com/sairaph/creality_k2_mcp/internal/printerstate"
)

func printersTestDeps(t *testing.T, stdout, stderr *bytes.Buffer) Deps {
	t.Helper()
	return Deps{
		Stdout: stdout,
		Stderr: stderr,
		PrinterClients: func(p domain.Printer) printerstate.Deps {
			return printerstate.Deps{
				Moonraker: &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()},
				WS9999:    fakeWS9999Client{},
			}
		},
	}
}

func TestRunPrintersUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	code := RunPrinters(context.Background(), deps, []string{"bogus"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunPrintersHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	code := RunPrinters(context.Background(), deps, []string{"help"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "printers scan") {
		t.Errorf("help output missing subcommand list:\n%s", stdout.String())
	}
}

func TestRunPrintersListEmpty(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)

	code := RunPrinters(context.Background(), deps, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No printers registered") {
		t.Errorf("output = %q, want it to say no printers are registered", stdout.String())
	}
}

func TestRunPrintersListShowsEnabledControlReachability(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-online", Name: "K2-Online", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-Online", Enabled: true, AllowControl: true,
	})
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-offline", Name: "K2-Offline", Host: "192.168.1.11", MoonrakerPort: 7125,
		Hostname: "K2-Offline", Enabled: false,
	})

	var stdout, stderr bytes.Buffer
	deps := Deps{
		Stdout: &stdout,
		Stderr: &stderr,
		PrinterClients: func(p domain.Printer) printerstate.Deps {
			moon := &fakeMoonrakerClient{serverInfo: fakeIdleServerInfo()}
			if p.ID == "k2-offline" {
				moon = &fakeMoonrakerClient{serverInfoErr: fmt.Errorf("connection refused")}
			}
			return printerstate.Deps{Moonraker: moon, WS9999: fakeWS9999Client{}}
		},
	}

	code := RunPrinters(context.Background(), deps, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ID") || !strings.Contains(out, "REACHABLE") {
		t.Errorf("missing table header:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var onlineLine, offlineLine string
	for _, l := range lines {
		if strings.Contains(l, "k2-online") {
			onlineLine = l
		}
		if strings.Contains(l, "k2-offline") {
			offlineLine = l
		}
	}
	if onlineLine == "" || offlineLine == "" {
		t.Fatalf("missing rows for both printers:\n%s", out)
	}
	fields := strings.Fields(onlineLine)
	if fields[3] != "yes" || fields[4] != "yes" || fields[5] != "yes" {
		t.Errorf("online row = %q, want enabled/control/reachable all yes", onlineLine)
	}
	fields = strings.Fields(offlineLine)
	if fields[3] != "no" || fields[4] != "no" || fields[5] != "no" {
		t.Errorf("offline row = %q, want enabled/control/reachable all no", offlineLine)
	}
}

func TestRunPrintersScanNeverModifiesRegistry(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-existing", Name: "K2-Existing", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-Existing", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	deps.Discover = func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{
			Results: []discovery.Result{
				{Host: "192.168.1.10", Hostname: "K2-Existing", Model: "K2", IdentifiedK2: true},
				{Host: "192.168.1.20", Hostname: "K2-New", Model: "K2", IdentifiedK2: true},
			},
			Scanned: 2, Total: 2,
		}, nil
	}

	code := RunPrinters(context.Background(), deps, []string{"scan"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "192.168.1.10") || !strings.Contains(out, "192.168.1.20") {
		t.Errorf("scan output missing a discovered host:\n%s", out)
	}
	if !strings.Contains(out, "yes (k2-existing)") {
		t.Errorf("scan output should mark 192.168.1.10 already registered:\n%s", out)
	}
	if !strings.Contains(out, "never changes the registry") {
		t.Errorf("scan output missing the no-write guidance:\n%s", out)
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Printers) != 1 {
		t.Fatalf("registry has %d printers after scan, want 1 (scan must never write)", len(reg.Printers))
	}
}

func TestRunPrintersAddNewPrinter(t *testing.T) {
	isolateHome(t)

	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	deps.Probe = func(ctx context.Context, host string, port int) (discovery.Result, error) {
		return discovery.Result{Host: host, Hostname: "K2-NEW", Model: "K2", IdentifiedK2: true}, nil
	}

	code := RunPrinters(context.Background(), deps, []string{"add", "192.168.1.50"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "control off") {
		t.Errorf("add output should mention control off:\n%s", stdout.String())
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Printers) != 1 {
		t.Fatalf("len(reg.Printers) = %d, want 1", len(reg.Printers))
	}
	p := reg.Printers[0]
	if !p.Enabled || p.AllowControl {
		t.Errorf("added printer = %+v, want Enabled=true AllowControl=false", p)
	}
}

func TestRunPrintersAddAlreadyRegistered(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-existing", Name: "K2-Existing", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-Existing", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	deps.Probe = func(ctx context.Context, host string, port int) (discovery.Result, error) {
		return discovery.Result{Host: host, Hostname: "K2-Existing", Model: "K2", IdentifiedK2: true}, nil
	}

	code := RunPrinters(context.Background(), deps, []string{"add", "192.168.1.10"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already registered") {
		t.Errorf("output = %q, want it to say already registered", stdout.String())
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Printers) != 1 {
		t.Fatalf("len(reg.Printers) = %d, want 1 (must stay unchanged)", len(reg.Printers))
	}
}

func TestRunPrintersAddNotIdentified(t *testing.T) {
	isolateHome(t)

	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	deps.Probe = func(ctx context.Context, host string, port int) (discovery.Result, error) {
		return discovery.Result{Host: host, IdentifiedK2: false, Reason: "not a Creality K2"}, nil
	}

	code := RunPrinters(context.Background(), deps, []string{"add", "192.168.1.99"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not a Creality K2") {
		t.Errorf("stderr = %q, want the probe's reason", stderr.String())
	}
}

func TestRunPrintersAddUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	code := RunPrinters(context.Background(), deps, []string{"add"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunPrintersEnableDisable(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: false,
	})

	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)

	if code := RunPrinters(context.Background(), deps, []string{"enable", "k2-5885"}); code != 0 {
		t.Fatalf("enable exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Printers[0].Enabled {
		t.Fatal("printer not enabled after `printers enable`")
	}

	stdout.Reset()
	if code := RunPrinters(context.Background(), deps, []string{"enable", "k2-5885"}); code != 0 {
		t.Fatalf("idempotent enable exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "already") {
		t.Errorf("idempotent enable output = %q, want it to say already enabled", stdout.String())
	}

	// Turn control on, then disable: disabling must clear control too.
	if code := RunPrinters(context.Background(), deps, []string{"control", "on", "k2-5885"}); code != 0 {
		t.Fatalf("control on exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if code := RunPrinters(context.Background(), deps, []string{"disable", "k2-5885"}); code != 0 {
		t.Fatalf("disable exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	reg, _, _, err = domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Printers[0].Enabled {
		t.Error("printer still enabled after `printers disable`")
	}
	if reg.Printers[0].AllowControl {
		t.Error("disabling must also clear AllowControl")
	}
}

func TestRunPrintersEnableNotFound(t *testing.T) {
	isolateHome(t)
	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	code := RunPrinters(context.Background(), deps, []string{"enable", "no-such-printer"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestRunPrintersControlOnRequiresEnabled(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: false,
	})

	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	code := RunPrinters(context.Background(), deps, []string{"control", "on", "k2-5885"})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "disabled") {
		t.Errorf("stderr = %q, want it to explain the printer is disabled", stderr.String())
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Printers[0].AllowControl {
		t.Error("control must not have been turned on")
	}
}

func TestRunPrintersControlOnPrintsWarningAndPersists(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})

	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	code := RunPrinters(context.Background(), deps, []string{"control", "on", "k2-5885"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Control lets the AI") {
		t.Errorf("output missing the control warning:\n%s", stdout.String())
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Printers[0].AllowControl {
		t.Error("control was not persisted as on")
	}

	// Idempotent: turning it on again must not re-print the warning as a
	// state change, just report it is already on.
	stdout.Reset()
	code = RunPrinters(context.Background(), deps, []string{"control", "on", "k2-5885"})
	if code != 0 {
		t.Fatalf("idempotent control on exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "already has control on") {
		t.Errorf("idempotent output = %q, want it to say already on", stdout.String())
	}

	// And turning it off again works and clears the flag.
	code = RunPrinters(context.Background(), deps, []string{"control", "off", "k2-5885"})
	if code != 0 {
		t.Fatalf("control off exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	reg, _, _, err = domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Printers[0].AllowControl {
		t.Error("control still on after `printers control off`")
	}
}

func TestRunPrintersControlUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := printersTestDeps(t, &stdout, &stderr)
	code := RunPrinters(context.Background(), deps, []string{"control", "sideways", "k2-5885"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
