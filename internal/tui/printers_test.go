package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func keyRune(r rune) tea.KeyMsg        { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func keyType(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

// initPrintersScreen runs Init and the reachability gather it always kicks
// off, leaving the screen ready with rows built and reach known, the same
// state a person sees right after opening Printers.
func initPrintersScreen(t *testing.T, ctx context.Context, deps Deps) *printersScreen {
	t.Helper()
	s := newPrintersScreen()
	msgs := drainCmd(s.Init(ctx, deps))
	loaded, ok := firstOfType[printersRegistryLoadedMsg](msgs)
	if !ok {
		t.Fatal("Init did not produce printersRegistryLoadedMsg")
	}
	reachCmd := s.Update(ctx, deps, loaded)
	if !s.ps.Ready {
		t.Fatal("screen not ready after registry load")
	}
	for _, m := range drainCmd(reachCmd) {
		s.Update(ctx, deps, m)
	}
	return s
}

func TestPrintersScreenListsEnabledControlReachable(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true, AllowControl: true,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)
	if len(s.ps.Rows) != 1 {
		t.Fatalf("len(Rows) = %d, want 1", len(s.ps.Rows))
	}
	row := s.ps.Rows[0]
	if !row.Printer.Enabled || !row.Printer.AllowControl {
		t.Errorf("row = %+v, want enabled+control", row.Printer)
	}
	ri, ok := s.reach["k2-5885"]
	if !ok || !ri.reachable {
		t.Errorf("reach[k2-5885] = %+v, ok=%v, want reachable", ri, ok)
	}
	view := s.View()
	if !containsAll(view, "K2-5885", "192.168.1.10", "yes") {
		t.Errorf("view missing expected content:\n%s", view)
	}
}

func TestPrintersScreenToggleEnabledPersists(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: false,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)
	saveCmd := s.Update(ctx, deps, keyType(tea.KeySpace))
	if !s.ps.Saving {
		t.Fatal("expected Saving after toggling enabled")
	}
	for _, m := range drainCmd(saveCmd) {
		s.Update(ctx, deps, m)
	}
	if s.ps.Saving {
		t.Error("still saving after save completed")
	}
	if !s.ps.Rows[0].Printer.Enabled {
		t.Error("row not enabled after toggle")
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Printers[0].Enabled {
		t.Error("registry on disk was not updated")
	}
}

func TestPrintersScreenControlOnNeedsConfirm(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)

	// Pressing c must not persist anything yet: it opens a confirmation.
	if cmd := s.Update(ctx, deps, keyRune('c')); cmd != nil {
		t.Error("expected no command from opening the control confirmation")
	}
	if s.mode != printersModeConfirmControl {
		t.Fatalf("mode = %v, want printersModeConfirmControl", s.mode)
	}
	reg, _, _, _ := domain.LoadRegistry("")
	if reg.Printers[0].AllowControl {
		t.Fatal("control must not be persisted before confirmation")
	}

	saveCmd := s.Update(ctx, deps, keyRune('y'))
	for _, m := range drainCmd(saveCmd) {
		s.Update(ctx, deps, m)
	}
	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Printers[0].AllowControl {
		t.Error("control was not persisted after confirming")
	}
}

func TestPrintersScreenControlOnCancelled(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)
	s.Update(ctx, deps, keyRune('c'))
	s.Update(ctx, deps, keyType(tea.KeyEsc))
	if s.mode != printersModeNormal {
		t.Fatalf("mode = %v, want printersModeNormal after cancel", s.mode)
	}
	reg, _, _, _ := domain.LoadRegistry("")
	if reg.Printers[0].AllowControl {
		t.Error("control must stay off after cancelling")
	}
}

func TestPrintersScreenRenamePersists(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)
	s.Update(ctx, deps, keyRune('n'))
	if s.mode != printersModeRenaming {
		t.Fatalf("mode = %v, want printersModeRenaming", s.mode)
	}
	s.renameInput = ""
	for _, r := range "Office K2" {
		s.Update(ctx, deps, keyRune(r))
	}
	saveCmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	for _, m := range drainCmd(saveCmd) {
		s.Update(ctx, deps, m)
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if reg.Printers[0].Name != "Office K2" {
		t.Errorf("Name = %q, want %q", reg.Printers[0].Name, "Office K2")
	}
}

func TestPrintersScreenRemovePersists(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-5885", Name: "K2-5885", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-5885", Enabled: true,
	})
	ctx := context.Background()
	deps := Deps{PrinterClients: fakePrinterClients(nil)}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)
	s.Update(ctx, deps, keyRune('d'))
	if s.mode != printersModeConfirmRemove {
		t.Fatalf("mode = %v, want printersModeConfirmRemove", s.mode)
	}
	saveCmd := s.Update(ctx, deps, keyRune('y'))
	for _, m := range drainCmd(saveCmd) {
		s.Update(ctx, deps, m)
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Printers) != 0 {
		t.Errorf("len(Printers) = %d, want 0 after remove", len(reg.Printers))
	}
	if len(s.ps.Rows) != 0 {
		t.Errorf("len(Rows) = %d, want 0 after remove", len(s.ps.Rows))
	}
}

func TestPrintersScreenAddHostPersists(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	deps := Deps{
		PrinterClients: fakePrinterClients(nil),
		Probe: func(ctx context.Context, host string, port int) (discovery.Result, error) {
			return discovery.Result{Host: host, Hostname: "K2-NEW", Model: "K2", IdentifiedK2: true}, nil
		},
	}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)
	s.Update(ctx, deps, keyRune('m'))
	if !s.ps.Adding {
		t.Fatal("expected Adding after m")
	}
	for _, r := range "192.168.1.50" {
		s.Update(ctx, deps, keyRune(r))
	}
	probeCmd := s.Update(ctx, deps, keyType(tea.KeyEnter))
	if !s.ps.Probing {
		t.Fatal("expected Probing after enter")
	}
	var saveCmd tea.Cmd
	for _, m := range drainCmd(probeCmd) {
		saveCmd = s.Update(ctx, deps, m)
	}
	for _, m := range drainCmd(saveCmd) {
		s.Update(ctx, deps, m)
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Printers) != 1 {
		t.Fatalf("len(Printers) = %d, want 1", len(reg.Printers))
	}
	if reg.Printers[0].Host != "192.168.1.50" || reg.Printers[0].AllowControl {
		t.Errorf("added printer = %+v, want host 192.168.1.50, control off", reg.Printers[0])
	}
}

func TestPrintersScreenScanNeverSavesByItself(t *testing.T) {
	isolateHome(t)
	registerTestPrinter(t, "", domain.Printer{
		ID: "k2-existing", Name: "K2-Existing", Host: "192.168.1.10", MoonrakerPort: 7125,
		Hostname: "K2-Existing", Enabled: true,
	})
	ctx := context.Background()
	deps := Deps{
		PrinterClients: fakePrinterClients(nil),
		Discover: func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
			return discovery.Report{
				Results: []discovery.Result{
					{Host: "192.168.1.20", Hostname: "K2-New", Model: "K2", IdentifiedK2: true},
				},
				Scanned: 1, Total: 1,
			}, nil
		},
	}.withDefaults()

	s := initPrintersScreen(t, ctx, deps)
	scanCmd := s.Update(ctx, deps, keyRune('r'))
	if !s.ps.Scanning {
		t.Fatal("expected Scanning after r")
	}
	for _, m := range drainCmd(scanCmd) {
		s.Update(ctx, deps, m)
	}
	if s.ps.Scanning {
		t.Error("still scanning after scan completed")
	}
	if len(s.ps.Rows) != 2 {
		t.Fatalf("len(Rows) = %d, want 2 (existing + discovered)", len(s.ps.Rows))
	}

	reg, _, _, err := domain.LoadRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Printers) != 1 {
		t.Errorf("len(Printers) = %d, want 1 (scan must never save)", len(reg.Printers))
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
