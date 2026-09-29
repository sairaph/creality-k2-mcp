package wizard

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

// isolateHome points domain's baseDir (~/.creality-k2-mcp) at a fresh temp
// directory, the same pattern internal/domain's own tests use, so a wizard
// test never reads or writes a real user's registry or settings file.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows reads USERPROFILE
	return home
}

func k2Result(host, hostname, model string) discovery.Result {
	return discovery.Result{Host: host, Port: domain.DefaultMoonrakerPort, Hostname: hostname, Model: model, IdentifiedK2: true}
}

func nonK2Result(host, reason string) discovery.Result {
	return discovery.Result{Host: host, Port: domain.DefaultMoonrakerPort, IdentifiedK2: false, Reason: reason}
}

func TestBuildRowsClassifiesEveryKind(t *testing.T) {
	existing := domain.NewPrinter("K2-5885", "192.168.1.10")
	existing.Enabled = true
	existing.AllowControl = false
	reg := domain.Registry{Version: 1, Printers: []domain.Printer{existing}}

	results := []discovery.Result{
		k2Result("192.168.1.10", "K2-5885", "K2"), // matches existing
		k2Result("192.168.1.20", "K2-9999", "K2"), // new, proposable
		nonK2Result("192.168.1.30", "not a Creality K2"),
		{Host: "192.168.1.40", IdentifiedK2: true}, // K2 with no hostname
	}
	merged := discovery.Merge(results, reg, true)
	rows := BuildRows(reg, merged, nil)

	if len(rows) != 4 {
		t.Fatalf("len(rows) = %d, want 4", len(rows))
	}

	byHost := make(map[string]PrinterRow, len(rows))
	for _, r := range rows {
		byHost[r.Printer.Host] = r
	}

	matched := byHost["192.168.1.10"]
	if !matched.Addable || !matched.Existing {
		t.Errorf("matched row = %+v, want Addable and Existing", matched)
	}
	if !matched.Printer.Enabled {
		t.Error("matched row should keep the registry's Enabled=true")
	}

	proposed := byHost["192.168.1.20"]
	if !proposed.Addable || proposed.Existing {
		t.Errorf("proposed row = %+v, want Addable and not Existing", proposed)
	}
	if !proposed.Printer.Enabled {
		t.Error("newly proposed row should default Enabled=true (enabledByDefault)")
	}
	if proposed.Printer.AllowControl {
		t.Error("newly proposed row must never default AllowControl=true")
	}

	notK2 := byHost["192.168.1.30"]
	if notK2.Addable {
		t.Error("a non-K2 host must not be Addable")
	}
	if notK2.Reason == "" {
		t.Error("a non-K2 host must carry a Reason")
	}

	noHostname := byHost["192.168.1.40"]
	if noHostname.Addable {
		t.Error("a K2 without a Klipper hostname must not be Addable")
	}
	if !strings.Contains(noHostname.Reason, "hostname") {
		t.Errorf("noHostname.Reason = %q, want it to mention hostname", noHostname.Reason)
	}
}

func TestBuildRowsCarriesForwardPreviousToggles(t *testing.T) {
	existing := domain.NewPrinter("K2-5885", "192.168.1.10")
	existing.Enabled = true
	existing.AllowControl = false
	reg := domain.Registry{Version: 1, Printers: []domain.Printer{existing}}

	results := []discovery.Result{
		k2Result("192.168.1.10", "K2-5885", "K2"),
		k2Result("192.168.1.20", "K2-9999", "K2"),
	}
	merged := discovery.Merge(results, reg, true)
	first := BuildRows(reg, merged, nil)

	// Simulate the user toggling allow-control on the matched row and
	// disabling the freshly proposed one.
	for i := range first {
		switch first[i].Printer.Host {
		case "192.168.1.10":
			first[i].Printer.AllowControl = true
		case "192.168.1.20":
			first[i].Printer.Enabled = false
		}
	}

	rescanned := BuildRows(reg, merged, first)
	for _, r := range rescanned {
		switch r.Printer.Host {
		case "192.168.1.10":
			if !r.Printer.AllowControl {
				t.Error("rescan discarded the user's allow-control toggle")
			}
		case "192.168.1.20":
			if r.Printer.Enabled {
				t.Error("rescan discarded the user's disable toggle")
			}
		}
	}
}

func TestBuildRowsKeepsUndiscoveredRegistryEntries(t *testing.T) {
	offline := domain.NewPrinter("K2-OFFLINE", "192.168.1.99")
	offline.Enabled = true
	found := domain.NewPrinter("K2-ONLINE", "192.168.1.11")
	found.Enabled = true
	reg := domain.Registry{Version: 1, Printers: []domain.Printer{offline, found}}

	merged := discovery.Merge([]discovery.Result{k2Result("192.168.1.11", "K2-ONLINE", "K2")}, reg, true)
	rows := BuildRows(reg, merged, nil)

	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (offline entry must not be dropped)", len(rows))
	}
	var sawOffline bool
	for _, r := range rows {
		if r.Printer.Host == "192.168.1.99" {
			sawOffline = true
			if !r.Addable || !r.Existing {
				t.Errorf("offline row = %+v, want Addable and Existing", r)
			}
		}
	}
	if !sawOffline {
		t.Error("offline registry entry missing from rows")
	}
}

func TestMergeRowUpsertsByKey(t *testing.T) {
	reg := domain.Registry{}
	var rows []PrinterRow

	rows = MergeRow(rows, discovery.Merge([]discovery.Result{k2Result("192.168.1.50", "K2-1234", "K2")}, reg, true)[0])
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 after first probe", len(rows))
	}
	rows[0].Printer.AllowControl = true

	// Probing the same host again must update the row in place, not append
	// a second one, and must carry the user's control toggle forward.
	rows = MergeRow(rows, discovery.Merge([]discovery.Result{k2Result("192.168.1.50", "K2-1234", "K2")}, reg, true)[0])
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 after re-probing the same host", len(rows))
	}
	if !rows[0].Printer.AllowControl {
		t.Error("re-probing the same host discarded the allow-control toggle")
	}

	rows = MergeRow(rows, discovery.Merge([]discovery.Result{nonK2Result("192.168.1.60", "not identified")}, reg, true)[0])
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 after probing a second host", len(rows))
	}
}

func TestSaveSelectionWritesOnlyAddableRowsAtomically(t *testing.T) {
	isolateHome(t)

	addable := domain.NewPrinter("K2-5885", "192.168.1.10")
	addable.Enabled = true
	notAddable := PrinterRow{
		Printer: domain.Printer{Name: "192.168.1.99", Host: "192.168.1.99"},
		Addable: false,
		Reason:  "not identified",
	}
	rows := []PrinterRow{
		{Printer: addable, Existing: true, Addable: true},
		notAddable,
	}

	reg, path, err := SaveSelection("", rows)
	if err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	if len(reg.Printers) != 1 {
		t.Fatalf("len(reg.Printers) = %d, want 1 (not-addable row must never be saved)", len(reg.Printers))
	}
	if reg.Printers[0].ID != addable.ID {
		t.Errorf("saved printer id = %q, want %q", reg.Printers[0].ID, addable.ID)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved registry: %v", err)
	}
	var onDisk domain.Registry
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("decode saved registry: %v", err)
	}
	if len(onDisk.Printers) != 1 {
		t.Fatalf("on-disk printers = %d, want 1", len(onDisk.Printers))
	}
}

func TestSaveSelectionClearsAllowControlOnDisabledRow(t *testing.T) {
	isolateHome(t)

	// A row can reach SaveSelection with Enabled=false and AllowControl=true
	// (a stale in-session toggle, or a hand-edited registry loaded back into
	// a row) despite ToggleControl refusing that combination through the
	// UI; SaveSelection must still never persist it.
	disabled := domain.NewPrinter("K2-DISABLED", "192.168.1.10")
	disabled.Enabled = false
	disabled.AllowControl = true

	reg, path, err := SaveSelection("", []PrinterRow{{Printer: disabled, Addable: true}})
	if err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	if len(reg.Printers) != 1 {
		t.Fatalf("len(reg.Printers) = %d, want 1", len(reg.Printers))
	}
	if reg.Printers[0].AllowControl {
		t.Error("SaveSelection must clear AllowControl on a disabled printer")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved registry: %v", err)
	}
	var onDisk domain.Registry
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("decode saved registry: %v", err)
	}
	if len(onDisk.Printers) != 1 || onDisk.Printers[0].AllowControl {
		t.Errorf("on-disk printer = %+v, want AllowControl false", onDisk.Printers)
	}
}

func TestSaveSelectionProjectScope(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()

	p := domain.NewPrinter("K2-PROJ", "10.0.0.5")
	p.Enabled = true
	_, path, err := SaveSelection(dir, []PrinterRow{{Printer: p, Addable: true}})
	if err != nil {
		t.Fatalf("SaveSelection: %v", err)
	}
	// The very first save for a project directory has no project file yet,
	// so domain.RegistryPath resolves to the global file (project-over-global,
	// domain.RegistryPath's documented behavior) -- confirm SaveSelection
	// followed that resolution rather than inventing its own.
	want, err := domain.GlobalRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	// Once a project file exists, a second save must target it.
	projectPath := domain.ProjectRegistryPath(dir)
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(`{"version":1,"printers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, path2, err := SaveSelection(dir, []PrinterRow{{Printer: p, Addable: true}})
	if err != nil {
		t.Fatalf("SaveSelection (project file present): %v", err)
	}
	if path2 != projectPath {
		t.Errorf("path2 = %q, want project path %q", path2, projectPath)
	}
}

func TestScanAndMergeUsesInjectedDiscoverAndDefaultEnabled(t *testing.T) {
	fake := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{Results: []discovery.Result{k2Result("192.168.1.10", "K2-5885", "K2")}}, nil
	}
	report, merged, err := ScanAndMerge(context.Background(), fake, discovery.Options{}, domain.Registry{}, true)
	if err != nil {
		t.Fatalf("ScanAndMerge: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("len(report.Results) = %d, want 1", len(report.Results))
	}
	if len(merged) != 1 || merged[0].Proposed == nil {
		t.Fatalf("merged = %+v, want one Proposed entry", merged)
	}
	if !merged[0].Proposed.Enabled {
		t.Error("Proposed.Enabled should be true (enabledByDefault=true)")
	}
}

func TestProbeAndMergeUsesInjectedProbe(t *testing.T) {
	var gotHost string
	fake := func(ctx context.Context, host string, port int) (discovery.Result, error) {
		gotHost = host
		return k2Result(host, "K2-7777", "K2"), nil
	}
	res, merged, err := ProbeAndMerge(context.Background(), fake, "192.168.1.77", 0, domain.Registry{})
	if err != nil {
		t.Fatalf("ProbeAndMerge: %v", err)
	}
	if gotHost != "192.168.1.77" {
		t.Errorf("probe called with host %q, want 192.168.1.77", gotHost)
	}
	if !res.IdentifiedK2 {
		t.Error("expected IdentifiedK2 true")
	}
	if merged.Proposed == nil {
		t.Fatal("expected a Proposed entry for a new K2")
	}
}

func TestUnattendedInstallEnablesIdentifiedK2sControlOff(t *testing.T) {
	isolateHome(t)

	already := domain.NewPrinter("K2-OLD", "192.168.1.10")
	already.Enabled = false
	already.AllowControl = true // must be forced off by the unattended path
	reg := domain.Registry{Version: 1, Printers: []domain.Printer{already}}
	path, err := domain.GlobalRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.SaveRegistry(path, reg); err != nil {
		t.Fatal(err)
	}

	fake := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{Results: []discovery.Result{
			k2Result("192.168.1.10", "K2-OLD", "K2"),
			k2Result("192.168.1.20", "K2-NEW", "K2"),
			nonK2Result("192.168.1.30", "not a Creality K2"),
		}}, nil
	}

	res, err := UnattendedInstall(context.Background(), fake, "")
	if err != nil {
		t.Fatalf("UnattendedInstall: %v", err)
	}
	if res.Identified != 2 {
		t.Errorf("Identified = %d, want 2", res.Identified)
	}
	if len(res.Added) != 1 || res.Added[0].Host != "192.168.1.20" {
		t.Errorf("Added = %+v, want one entry for 192.168.1.20", res.Added)
	}
	if len(res.Enabled) != 1 || res.Enabled[0].Host != "192.168.1.10" {
		t.Errorf("Enabled = %+v, want one entry for 192.168.1.10", res.Enabled)
	}

	saved, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range saved.Printers {
		if !p.Enabled {
			t.Errorf("printer %q not enabled after unattended install", p.ID)
		}
		if p.AllowControl {
			t.Errorf("printer %q has control on after unattended install, want off", p.ID)
		}
	}
	if len(saved.Printers) != 2 {
		t.Fatalf("len(saved.Printers) = %d, want 2", len(saved.Printers))
	}

	settingsPath, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsPath); err != nil {
		t.Errorf("expected default settings file to exist: %v", err)
	}
}

func TestUnattendedInstallNothingFound(t *testing.T) {
	isolateHome(t)
	fake := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{}, nil
	}
	res, err := UnattendedInstall(context.Background(), fake, "")
	if err != nil {
		t.Fatalf("UnattendedInstall: %v", err)
	}
	if res.Identified != 0 || len(res.Added) != 0 || len(res.Enabled) != 0 {
		t.Errorf("res = %+v, want a fully empty result", res)
	}
}

func TestPrintUnattendedResultNothingFoundMentionsEnvHost(t *testing.T) {
	var buf bytes.Buffer
	PrintUnattendedResult(&buf, UnattendedResult{})
	out := buf.String()
	if !strings.Contains(out, domain.EnvHost) {
		t.Errorf("output missing %s hint:\n%s", domain.EnvHost, out)
	}
}

func TestPrintUnattendedResultReportsAddedAndEnabled(t *testing.T) {
	var buf bytes.Buffer
	PrintUnattendedResult(&buf, UnattendedResult{
		RegistryPath: "/tmp/printers.json",
		Identified:   2,
		Added:        []domain.Printer{{Name: "K2-NEW", Host: "192.168.1.20"}},
		Enabled:      []domain.Printer{{Name: "K2-OLD", Host: "192.168.1.10"}},
	})
	out := buf.String()
	for _, want := range []string{"K2-NEW", "192.168.1.20", "K2-OLD", "192.168.1.10", "/tmp/printers.json"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
