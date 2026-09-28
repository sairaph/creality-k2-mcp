package wizard

import (
	"context"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"

	"github.com/sairaph/creality_k2_mcp/internal/discovery"
	"github.com/sairaph/creality_k2_mcp/internal/domain"
)

// printersTestState is a minimal flow consumer state, the same shape main.go's
// AppState uses, so the step can be driven through flow.Step[T] and
// baseStateOf can find the embedded flow.BaseState.
type printersTestState struct {
	flow.BaseState
	Printers PrinterState
}

func printersTestStateFn(s *printersTestState) *PrinterState { return &s.Printers }

// newPrintersStep builds a *printersStep directly (white-box, same package)
// so tests can drive Update without running a real tea.Program or letting
// tui.Spinner()'s 110ms tick actually elapse.
func newPrintersStep(discover DiscoverFunc, probe ProbeFunc) *printersStep[printersTestState] {
	return &printersStep[printersTestState]{
		stateFn: printersTestStateFn,
		opts:    PrintersStepOptions{Discover: discover, Probe: probe},
		ctx:     context.Background(),
	}
}

// flattenCmd runs cmd (and, recursively, every command inside a
// tea.BatchMsg) and collects every non-nil message produced. It is used
// sparingly, only where a test genuinely needs the async round trip,
// because it also runs tui.Spinner()'s real 110ms tick when one is batched
// in.
func flattenCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, flattenCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func firstOfType[M any](msgs []tea.Msg) (M, bool) {
	for _, m := range msgs {
		if typed, ok := m.(M); ok {
			return typed, true
		}
	}
	var zero M
	return zero, false
}

func TestPrintersStepIDAndTitle(t *testing.T) {
	s := newPrintersStep(nil, nil)
	if s.ID() != "printers" {
		t.Errorf("ID() = %q, want %q", s.ID(), "printers")
	}
	st := &printersTestState{}
	if !strings.Contains(s.Title(st), "Printers") {
		t.Errorf("Title() = %q, want it to mention Printers", s.Title(st))
	}

	proj := newPrintersStep(nil, nil)
	proj.opts.Dir = "/some/project"
	if title := proj.Title(st); !strings.Contains(title, "/some/project") {
		t.Errorf("project Title() = %q, want it to mention the directory", title)
	}
}

func TestPrintersStepInitThenScanEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	fakeDiscover := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		return discovery.Report{Results: []discovery.Result{k2Result("192.168.1.10", "K2-5885", "K2")}}, nil
	}
	s := newPrintersStep(fakeDiscover, nil)
	st := &printersTestState{}

	initCmd := s.Init(st)
	if initCmd == nil {
		t.Fatal("Init returned a nil cmd")
	}
	msgs := flattenCmd(initCmd)
	loaded, ok := firstOfType[registryLoadedMsg](msgs)
	if !ok {
		t.Fatal("Init did not produce a registryLoadedMsg")
	}

	directive, scanCmd := s.Update(loaded, st)
	if directive != flow.Continue {
		t.Fatalf("directive after registryLoadedMsg = %v, want Continue", directive)
	}
	if !st.Printers.Ready {
		t.Error("Ready should be true once the registry has loaded")
	}
	if !st.Printers.Scanning {
		t.Error("Scanning should be true once the scan has started")
	}
	if scanCmd == nil {
		t.Fatal("expected a non-nil cmd to start the scan")
	}

	scanMsgs := flattenCmd(scanCmd)
	done, ok := firstOfType[scanDoneMsg](scanMsgs)
	if !ok {
		t.Fatal("scan did not produce a scanDoneMsg")
	}
	if _, cmd := s.Update(done, st); cmd != nil {
		t.Error("scanDoneMsg should not chain another command")
	}
	if st.Printers.Scanning {
		t.Error("Scanning should be false once scanDoneMsg is handled")
	}
	if len(st.Printers.Rows) != 1 || st.Printers.Rows[0].Printer.Host != "192.168.1.10" {
		t.Fatalf("Rows = %+v, want one row for 192.168.1.10", st.Printers.Rows)
	}
	if !st.Printers.Rows[0].Printer.Enabled {
		t.Error("a freshly discovered K2 should default to Enabled")
	}
}

func TestPrintersStepEscStopsScanKeepingResults(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	st.Printers.Scanning = true
	st.Printers.Rows = []PrinterRow{{Printer: domain.Printer{ID: "k2-1", Name: "K2-1", Host: "192.168.1.10", Enabled: true}, Addable: true}}
	var cancelled bool
	st.Printers.cancelScan = func() { cancelled = true }

	directive, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEsc}, st)
	if directive != flow.Continue {
		t.Errorf("directive = %v, want Continue", directive)
	}
	if cmd != nil {
		t.Error("esc should not start a new command")
	}
	if !cancelled {
		t.Error("esc while scanning should call cancelScan")
	}
	if len(st.Printers.Rows) != 1 {
		t.Error("esc must keep the results found so far")
	}
}

func TestPrintersStepEscWhenIdleGoesBack(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true

	directive, _ := s.Update(tea.KeyMsg{Type: tea.KeyEsc}, st)
	if directive != flow.Back {
		t.Errorf("directive = %v, want Back", directive)
	}
}

func TestPrintersStepSpaceAndControlToggle(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	st.Printers.Rows = []PrinterRow{
		{Printer: domain.Printer{ID: "k2-1", Name: "K2-1", Enabled: false, AllowControl: false}, Addable: true},
		{Printer: domain.Printer{Name: "192.168.1.99", Host: "192.168.1.99"}, Addable: false, Reason: "not identified"},
	}

	s.Update(tea.KeyMsg{Type: tea.KeySpace}, st)
	if !st.Printers.Rows[0].Printer.Enabled {
		t.Error("space should enable the addable row under the cursor")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")}, st)
	if !st.Printers.Rows[0].Printer.AllowControl {
		t.Error("c should toggle allow-control on the addable row under the cursor")
	}

	// Move onto the not-addable row and confirm neither key touches it.
	st.Printers.Cursor = 1
	s.Update(tea.KeyMsg{Type: tea.KeySpace}, st)
	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")}, st)
	if st.Printers.Rows[1].Printer.Enabled || st.Printers.Rows[1].Printer.AllowControl {
		t.Error("space/c must never toggle a not-addable row")
	}
}

func TestToggleControlRefusesOnDisabledRow(t *testing.T) {
	ps := &PrinterState{
		Rows: []PrinterRow{
			{Printer: domain.Printer{ID: "k2-1", Name: "K2-1", Enabled: false, AllowControl: false}, Addable: true},
		},
	}

	ToggleControl(ps)
	if ps.Rows[0].Printer.AllowControl {
		t.Error("ToggleControl must refuse to turn on control for a disabled row")
	}
	if ps.Message == "" {
		t.Error("ToggleControl on a disabled row should leave a status message explaining why")
	}
}

func TestToggleControlWorksOnEnabledRow(t *testing.T) {
	ps := &PrinterState{
		Rows: []PrinterRow{
			{Printer: domain.Printer{ID: "k2-1", Name: "K2-1", Enabled: true, AllowControl: false}, Addable: true},
		},
	}

	ToggleControl(ps)
	if !ps.Rows[0].Printer.AllowControl {
		t.Error("ToggleControl should turn on control for an enabled row")
	}
	ToggleControl(ps)
	if ps.Rows[0].Printer.AllowControl {
		t.Error("ToggleControl should turn control back off")
	}
}

func TestToggleEnabledClearsAllowControlWhenDisabling(t *testing.T) {
	ps := &PrinterState{
		Rows: []PrinterRow{
			{Printer: domain.Printer{ID: "k2-1", Name: "K2-1", Enabled: true, AllowControl: true}, Addable: true},
		},
	}

	ToggleEnabled(ps)
	if ps.Rows[0].Printer.Enabled {
		t.Fatal("ToggleEnabled should have disabled the row")
	}
	if ps.Rows[0].Printer.AllowControl {
		t.Error("disabling a row must clear AllowControl")
	}
}

func TestPrintersStepManualAddFlow(t *testing.T) {
	fakeProbe := func(ctx context.Context, host string, port int) (discovery.Result, error) {
		return k2Result(host, "K2-MANUAL", "K2"), nil
	}
	s := newPrintersStep(nil, fakeProbe)
	st := &printersTestState{}
	st.Printers.Ready = true

	s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")}, st)
	if !st.Printers.Adding {
		t.Fatal("m should enter manual-add mode")
	}

	for _, r := range "192.168.1.77" {
		s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, st)
	}
	if st.Printers.Input != "192.168.1.77" {
		t.Fatalf("Input = %q, want 192.168.1.77", st.Printers.Input)
	}

	directive, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if directive != flow.Continue || cmd == nil {
		t.Fatal("enter with a valid host should start an async probe")
	}
	if !st.Printers.Probing {
		t.Error("Probing should be true while the probe runs")
	}

	msgs := flattenCmd(cmd)
	done, ok := firstOfType[probeDoneMsg](msgs)
	if !ok {
		t.Fatal("probe did not produce a probeDoneMsg")
	}
	s.Update(done, st)
	if st.Printers.Probing || st.Printers.Adding {
		t.Error("Probing and Adding should both be false once the probe result lands")
	}
	if len(st.Printers.Rows) != 1 || st.Printers.Rows[0].Printer.Host != "192.168.1.77" {
		t.Fatalf("Rows = %+v, want one row for the manually probed host", st.Printers.Rows)
	}
}

func TestPrintersStepManualAddRejectsBadHost(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	st.Printers.Adding = true
	st.Printers.Input = "" // empty host

	directive, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if directive != flow.Continue || cmd != nil {
		t.Error("an empty host must not start a probe")
	}
	if st.Printers.Message == "" {
		t.Error("an empty host should leave a message explaining why")
	}
	if !st.Printers.Adding {
		t.Error("manual add mode should stay open after a validation error")
	}
}

func TestPrintersStepEnterSavesAndAdvances(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	st.Printers.Rows = []PrinterRow{
		{Printer: domain.NewPrinter("K2-5885", "192.168.1.10"), Addable: true, Existing: true},
	}

	directive, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if directive != flow.Continue {
		t.Fatalf("directive = %v, want Continue (the save runs as a tea.Cmd)", directive)
	}
	if cmd == nil {
		t.Fatal("enter should return a non-nil cmd that performs the save")
	}
	if !st.Printers.Saving {
		t.Error("Saving should be true while the save cmd is in flight")
	}
	// While saving, keys other than ctrl+c are ignored.
	if d, c := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Continue || c != nil {
		t.Error("keys must be ignored while Saving")
	}

	msgs := flattenCmd(cmd)
	saved, ok := firstOfType[printersSavedMsg](msgs)
	if !ok {
		t.Fatal("save cmd did not produce a printersSavedMsg")
	}
	if saved.err != nil {
		t.Fatalf("printersSavedMsg.err = %v", saved.err)
	}

	directive, cmd = s.Update(saved, st)
	if directive != flow.Next {
		t.Fatalf("directive after printersSavedMsg = %v, want Next", directive)
	}
	if cmd != nil {
		t.Error("handling printersSavedMsg should not chain another command")
	}
	if st.Printers.Saving {
		t.Error("Saving should be false once printersSavedMsg is handled")
	}
	if st.Printers.Path == "" {
		t.Error("Path should be set after a successful save")
	}
	if _, err := os.Stat(st.Printers.Path); err != nil {
		t.Errorf("expected registry file to exist: %v", err)
	}
}

func TestPrintersStepEnterKeepsGoingOnSaveError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	dup := domain.NewPrinter("K2-DUP", "192.168.1.10")
	st.Printers.Rows = []PrinterRow{
		{Printer: dup, Addable: true},
		{Printer: dup, Addable: true}, // duplicate id -> ValidateRegistry rejects it
	}

	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if cmd == nil {
		t.Fatal("expected a non-nil save cmd")
	}
	msgs := flattenCmd(cmd)
	saved, ok := firstOfType[printersSavedMsg](msgs)
	if !ok {
		t.Fatal("save cmd did not produce a printersSavedMsg")
	}
	if saved.err == nil {
		t.Fatal("expected the duplicate-id save to fail")
	}

	directive, _ := s.Update(saved, st)
	if directive != flow.Continue {
		t.Errorf("directive = %v, want Continue on a save error", directive)
	}
	if st.Printers.Message == "" {
		t.Error("a save error should be reported in Message")
	}
	if st.Printers.Saving {
		t.Error("Saving should be false once the error is handled")
	}
}

func TestPrintersStepDryRunNeverWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := newPrintersStep(nil, nil)
	s.opts.DryRun = true
	st := &printersTestState{}
	st.Printers.Ready = true
	st.Printers.Rows = []PrinterRow{
		{Printer: domain.NewPrinter("K2-5885", "192.168.1.10"), Addable: true, Existing: true},
	}

	directive, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if directive != flow.Continue || cmd == nil {
		t.Fatal("dry-run enter should still return a cmd, without writing yet")
	}
	msgs := flattenCmd(cmd)
	saved, ok := firstOfType[printersSavedMsg](msgs)
	if !ok {
		t.Fatal("save cmd did not produce a printersSavedMsg")
	}
	if saved.err != nil {
		t.Fatalf("printersSavedMsg.err = %v", saved.err)
	}
	if !saved.dryRun {
		t.Error("printersSavedMsg.dryRun should be true in dry-run mode")
	}

	directive, _ = s.Update(saved, st)
	if directive != flow.Next {
		t.Fatalf("directive after dry-run save = %v, want Next", directive)
	}
	if st.Printers.Message == "" {
		t.Error("dry-run should leave a message describing what would be written")
	}

	path, err := domain.GlobalRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dry-run must never create %s, stat err = %v", path, err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dry-run must never touch the filesystem, found %v under %s", entries, home)
	}
}

func TestPrintersStepViewShowsReasonForNotAddable(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	st.Printers.Rows = []PrinterRow{
		{Printer: domain.Printer{Name: "192.168.1.30", Host: "192.168.1.30"}, Addable: false, Reason: "not a Creality K2"},
	}
	out := s.View(st)
	if !strings.Contains(out, "not a Creality K2") {
		t.Errorf("View output missing the not-addable reason:\n%s", out)
	}
}

func TestPrintersStepViewShowsManualAddPrompt(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	st.Printers.Adding = true
	st.Printers.Input = "192.168"
	out := s.View(st)
	if !strings.Contains(out, "192.168") {
		t.Errorf("View output missing the in-progress input:\n%s", out)
	}
}
