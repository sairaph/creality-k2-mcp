package wizard

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sairaph/mcp-wizard/flow"

	"github.com/sairaph/creality-k2-mcp/internal/discovery"
	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func printerRows(n int) []PrinterRow {
	rows := make([]PrinterRow, n)
	for i := range rows {
		p := domain.NewPrinter(fmt.Sprintf("K2-%04d", i), fmt.Sprintf("192.168.1.%d", 10+i))
		p.Enabled = i%2 == 0
		p.AllowControl = i == 0
		rows[i] = PrinterRow{Printer: p, Addable: true, Existing: i%2 == 0}
	}
	return rows
}

func TestPrintersStepFrame(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}, {60, 16}} {
		w, h := size[0], size[1]
		s := newPrintersStep(nil, nil)
		st := &printersTestState{}
		st.Width, st.Height = w, h
		st.Printers.Ready = true
		st.Printers.Rows = append(printerRows(2), PrinterRow{Printer: domain.Printer{Name: "192.168.1.99", Host: "192.168.1.99"}, Reason: "not a Creality K2"})

		out := s.View(st)
		lines := assertFrame(t, out, w, h)
		if head := ansi.Strip(lines[0]); !strings.Contains(head, "creality-k2-mcp setup") || !strings.Contains(head, "Printers") || !strings.Contains(head, "step 1 of 4") {
			t.Errorf("%dx%d: header = %q", w, h, head)
		}
		plain := ansi.Strip(out)
		if !strings.Contains(plain, "Which Creality K2 printers should the AI be able to use?") {
			t.Errorf("%dx%d: the question is the first body line:\n%s", w, h, plain)
		}
		for _, banned := range []string{"press v", "AI clients", "No AI clients"} {
			if strings.Contains(plain, banned) {
				t.Errorf("%dx%d: the Printers step must not contain %q:\n%s", w, h, banned, plain)
			}
		}
		for _, want := range []string{"K2-0000", "K2-0001", "not a Creality K2"} {
			if !strings.Contains(plain, want) {
				t.Errorf("%dx%d: missing %q:\n%s", w, h, want, plain)
			}
		}
		footer := footerOf(out)
		if !strings.Contains(footer, "enter continue") || !strings.HasSuffix(footer, "q cancel") {
			t.Errorf("%dx%d: footer = %q, want enter continue ... q cancel", w, h, footer)
		}
		if strings.Contains(" "+footer, " esc ") {
			t.Errorf("%dx%d: step 1 has no esc back, and esc stop scan only shows while scanning: %q", w, h, footer)
		}
	}
}

func TestPrintersStepFooterWhileScanning(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Width, st.Height = 120, 36
	st.Printers.Ready = true
	st.Printers.Scanning = true
	st.Printers.Total, st.Printers.Scanned, st.Printers.Found = 254, 100, 1
	out := s.View(st)
	assertFrame(t, out, 120, 36)
	footer := footerOf(out)
	if !strings.Contains(footer, "esc stop scan") || strings.Contains(footer, "rescan") {
		t.Errorf("footer while scanning = %q", footer)
	}
	if !strings.Contains(ansi.Strip(out), "Scanning the network... 100/254 hosts, 1 found") {
		t.Errorf("scan progress line missing:\n%s", ansi.Strip(out))
	}

	// Narrow: the short variant keeps enter continue and q cancel.
	st.Width, st.Height = 60, 16
	short := footerOf(s.View(st))
	if !strings.Contains(short, "enter continue") || !strings.HasSuffix(short, "q cancel") {
		t.Errorf("narrow footer = %q", short)
	}
}

func TestPrintersStepEmptyStates(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Width, st.Height = 120, 36
	st.Printers.Ready = true
	plain := ansi.Strip(s.View(st))
	if !strings.Contains(plain, "No printers found yet. Press m to add one by its address, or r to scan again.") {
		t.Errorf("empty sentence missing:\n%s", plain)
	}
	st.Printers.Scanning = true
	plain = ansi.Strip(s.View(st))
	if strings.Contains(plain, "No printers found yet") || !strings.Contains(plain, "Scanning the network...") {
		t.Errorf("while scanning the screen shows the spinner line, not the empty sentence:\n%s", plain)
	}

	st.Printers.Ready = false
	st.Printers.Scanning = false
	plain = ansi.Strip(s.View(st))
	if !strings.Contains(plain, "Loading the printer registry...") {
		t.Errorf("loading line missing:\n%s", plain)
	}
}

func TestPrintersStepListScrollsToTheCursor(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Width, st.Height = 80, 16
	st.Printers.Ready = true
	st.Printers.Rows = printerRows(30)
	for _, cursor := range []int{0, 15, 29} {
		st.Printers.Cursor = cursor
		out := s.View(st)
		assertFrame(t, out, 80, 16)
		want := fmt.Sprintf("K2-%04d", cursor)
		var marked string
		for _, line := range strings.Split(ansi.Strip(out), "\n") {
			if strings.HasPrefix(line, "  > ") {
				marked = line
			}
		}
		if !strings.Contains(marked, want) {
			t.Errorf("cursor %d: marked row = %q, want it to hold %s", cursor, marked, want)
		}
	}
}

func TestPrintersStepNotesStayVisibleBelowALongList(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Width, st.Height = 80, 16
	st.Printers.Ready = true
	st.Printers.Rows = printerRows(30)
	st.Printers.Message = "enable the printer before allowing control"
	if !strings.Contains(ansi.Strip(s.View(st)), "enable the printer before allowing control") {
		t.Error("the message must stay visible under a long list")
	}
}

func TestPrintersStepAddingScreen(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Width, st.Height = 120, 36
	st.Printers.Ready = true
	st.Printers.Adding = true
	out := s.View(st)
	assertFrame(t, out, 120, 36)
	plain := ansi.Strip(out)
	if !strings.Contains(plain, "Host name or IP address of the printer:") || !strings.Contains(plain, "e.g. 192.168.1.50") {
		t.Errorf("add prompt missing:\n%s", plain)
	}
	if got := footerOf(out); got != "enter probe · esc cancel" {
		t.Errorf("footer = %q", got)
	}

	// q is a character in the field; the footer shows no q cancel.
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Continue || st.Printers.Input != "q" {
		t.Errorf("q in the field: directive %v input %q", d, st.Printers.Input)
	}
}

// blockingProbe is a fake ProbeFunc that reports its context on started and
// then blocks. With honourCtx it returns when the context is cancelled;
// without, only when release is closed (a probe that answers late, after the
// user cancelled it).
func blockingProbe(started chan<- context.Context, release <-chan struct{}, honourCtx bool) ProbeFunc {
	return func(ctx context.Context, host string, port int) (discovery.Result, error) {
		started <- ctx
		if honourCtx {
			select {
			case <-ctx.Done():
				return discovery.Result{}, ctx.Err()
			case <-release:
			}
		} else {
			<-release
		}
		return k2Result(host, "K2-LATE", "K2"), nil
	}
}

// startProbe types a host into the add field and presses enter, runs the
// probe command in the background and returns its context and the channel its
// messages arrive on.
func startProbe(t *testing.T, s *printersStep[printersTestState], st *printersTestState, started <-chan context.Context) (context.Context, <-chan []tea.Msg) {
	t.Helper()
	st.Printers.Ready, st.Printers.Adding, st.Printers.Input = true, true, "192.168.1.5"
	d, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
	if d != flow.Continue || cmd == nil || !st.Printers.Probing {
		t.Fatalf("enter did not start the probe (directive %v, probing %v)", d, st.Printers.Probing)
	}
	msgs := make(chan []tea.Msg, 1)
	go func() { msgs <- flattenCmd(cmd) }()
	select {
	case ctx := <-started:
		return ctx, msgs
	case <-time.After(5 * time.Second):
		t.Fatal("the probe did not start")
		return nil, nil
	}
}

func requireDone(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: the context was not cancelled", what)
	}
}

func TestPrintersStepProbeEscAndQCancel(t *testing.T) {
	started := make(chan context.Context, 2)
	release := make(chan struct{})
	defer close(release)
	s := newPrintersStep(nil, blockingProbe(started, release, true))
	st := &printersTestState{}
	st.Width, st.Height = 120, 36

	ctx, msgs := startProbe(t, s, st, started)
	if got := footerOf(s.View(st)); got != "probing... · esc cancel · q cancel" {
		t.Errorf("footer = %q", got)
	}
	if d, c := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st); d != flow.Continue || c != nil {
		t.Error("enter while probing must not start a second probe")
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyEsc}, st); d != flow.Continue {
		t.Errorf("esc while probing: directive %v, want Continue", d)
	}
	requireDone(t, ctx, "esc")
	if st.Printers.Probing || !st.Printers.Adding || st.Printers.Input != "192.168.1.5" {
		t.Errorf("esc must stop the probe and stay in the field: %+v", st.Printers)
	}
	// The cancelled probe answers with its context error: nothing changes.
	for _, m := range <-msgs {
		if _, ok := m.(probeDoneMsg); ok {
			if d, c := s.Update(m, st); d != flow.Continue || c != nil || st.Printers.Message != "" {
				t.Errorf("a cancelled probe's result changed the step: %v %v %q", d, c, st.Printers.Message)
			}
		}
	}

	ctx2, _ := startProbe(t, s, st, started)
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Quit {
		t.Errorf("q while probing: directive %v, want Quit", d)
	}
	requireDone(t, ctx2, "q")
}

func TestPrintersStepLateProbeResultChangesNothing(t *testing.T) {
	isolateHome(t)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	s := newPrintersStep(nil, blockingProbe(started, release, false))
	st := &printersTestState{}
	st.Width, st.Height = 120, 36

	ctx, msgs := startProbe(t, s, st, started)
	s.Update(tea.KeyMsg{Type: tea.KeyEsc}, st)
	requireDone(t, ctx, "esc")
	close(release)
	var late probeDoneMsg
	for _, m := range <-msgs {
		if d, ok := m.(probeDoneMsg); ok {
			late = d
		}
	}
	if late.err != nil || late.merged.Discovered.Host == "" {
		t.Fatalf("the fake probe should have answered late with a result: %+v", late)
	}
	if d, c := s.Update(late, st); d != flow.Continue || c != nil {
		t.Errorf("late result: directive %v, cmd %v; want Continue and no command", d, c != nil)
	}
	if len(st.Printers.Rows) != 0 || st.Printers.Saving || st.Printers.Probing || st.Printers.Message != "" {
		t.Errorf("a late probe result changed the step: %+v", st.Printers)
	}
	if _, err := os.Stat(registryFile(t)); err == nil {
		t.Error("a late probe result saved the registry")
	}
}

// registryFile is where a save would write the global registry.
func registryFile(t *testing.T) string {
	t.Helper()
	path, _, err := domain.RegistryPath("")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// q and esc work while a scan runs, and enter cancels the scan before it saves.
func TestPrintersStepKeysWorkWhileScanning(t *testing.T) {
	isolateHome(t)
	started := make(chan context.Context, 1)
	discover := func(ctx context.Context, opts discovery.Options) (discovery.Report, error) {
		started <- ctx
		<-ctx.Done()
		return discovery.Report{}, ctx.Err()
	}
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("q")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyEnter},
	} {
		s := newPrintersStep(discover, nil)
		st := &printersTestState{}
		st.Printers.Ready = true
		done := make(chan struct{})
		go func(cmd tea.Cmd) {
			flattenCmd(cmd)
			close(done)
		}(s.startScan(&st.Printers))
		ctx := <-started
		d, _ := s.Update(key, st)
		requireDone(t, ctx, key.String())
		switch key.String() {
		case "q":
			if d != flow.Quit {
				t.Errorf("q while scanning: directive %v, want Quit", d)
			}
		case "esc":
			if d != flow.Continue || st.Printers.Message != "Stopping scan..." {
				t.Errorf("esc while scanning: directive %v, message %q", d, st.Printers.Message)
			}
		case "enter":
			if d != flow.Continue || !st.Printers.Saving {
				t.Errorf("enter while scanning: directive %v, saving %v; want it to save", d, st.Printers.Saving)
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the scan did not stop", key.String())
		}
	}
}

func TestPrintersStepSmallTerminalAcceptsOnlyQAndCtrlC(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Width, st.Height = 40, 10
	st.Printers.Ready = true
	st.Printers.Rows = printerRows(2)
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyEnter}, {Type: tea.KeySpace}, {Type: tea.KeyEsc}, {Type: tea.KeyRunes, Runes: []rune("m")},
	} {
		if d, c := s.Update(k, st); d != flow.Continue || c != nil || st.Printers.Saving || st.Printers.Adding || st.Printers.Rows[0].Printer.Enabled != true {
			t.Errorf("%q acted on a too-small terminal (directive %v)", k.String(), d)
		}
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Quit {
		t.Error("q must still quit")
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyCtrlC}, st); d != flow.Quit {
		t.Error("ctrl+c must still quit")
	}
	st.Printers.Adding = true
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Continue || st.Printers.Input != "" {
		t.Error("q while typing must be ignored on a too-small terminal")
	}
}

func TestSpinnerTickChainStartsOnce(t *testing.T) {
	var tk ticker
	if tk.start() == nil {
		t.Fatal("the first start must return a tick")
	}
	if tk.start() != nil {
		t.Error("a second start while a chain runs must return nothing")
	}
	if tk.next(true) == nil {
		t.Error("a tick while active must keep the chain going")
	}
	if tk.next(false) != nil || tk.start() == nil {
		t.Error("an idle tick must end the chain so a later start can begin a new one")
	}
}

func TestPrintersStepHintKeysAreHandled(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	for _, h := range s.Hints(st) {
		switch h.Key {
		case "↑↓", "space", "c", "m", "r", "enter", "q":
		default:
			t.Errorf("footer lists key %q that Update does not handle", h.Key)
		}
	}
}

func TestPrintersStepDryRunHeaderAndProjectTitle(t *testing.T) {
	s := newPrintersStep(nil, nil)
	s.chrome = newChrome("creality-k2-mcp project setup", true)
	s.opts.Dir = t.TempDir()
	st := &printersTestState{}
	st.Width, st.Height = 120, 36
	st.Printers.Ready = true
	lines := assertFrame(t, s.View(st), 120, 36)
	head := ansi.Strip(lines[0])
	if !strings.Contains(head, "creality-k2-mcp project setup") || !strings.Contains(head, "dry run") {
		t.Errorf("header = %q", head)
	}
	if !strings.Contains(ansi.Strip(lines[2]), "Register in this project") { // a long temp path wraps after this
		t.Errorf("project question = %q", ansi.Strip(lines[2]))
	}
}

func TestPrintersStepTooSmallAndNoSize(t *testing.T) {
	s := newPrintersStep(nil, nil)
	st := &printersTestState{}
	st.Printers.Ready = true
	if out := s.View(st); out != "" {
		t.Errorf("View before the first size = %q, want empty", out)
	}
	st.Width, st.Height = 59, 24
	if out := s.View(st); !strings.Contains(out, "Terminal is 59x24.") {
		t.Errorf("too-small message = %q", out)
	}
	if d, _ := s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, st); d != flow.Quit {
		t.Errorf("q must keep working when the terminal is too small, got %v", d)
	}
}

func TestPrintersStepWroteOnlyAfterARealSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	for _, dry := range []bool{true, false} {
		s := newPrintersStep(nil, nil)
		s.opts.DryRun = dry
		st := &printersTestState{}
		st.Printers.Ready = true
		st.Printers.Rows = printerRows(1)
		_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}, st)
		saved, ok := firstOfType[printersSavedMsg](flattenCmd(cmd))
		if !ok || saved.err != nil {
			t.Fatalf("dry=%v: save failed: %+v", dry, saved)
		}
		s.Update(saved, st)
		if st.Printers.Wrote == dry {
			t.Errorf("dry=%v: Wrote = %v", dry, st.Printers.Wrote)
		}
	}
}

func TestSettingsStepFrame(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		w, h := size[0], size[1]
		s := newSettingsStep()
		st := readySettingsState()
		st.Width, st.Height = w, h
		out := s.View(st)
		lines := assertFrame(t, out, w, h)
		if head := ansi.Strip(lines[0]); !strings.Contains(head, "Settings") || !strings.Contains(head, "step 2 of 4") {
			t.Errorf("%dx%d: header = %q", w, h, head)
		}
		plain := ansi.Strip(out)
		for _, want := range []string{"Safety limits and which tools the AI can use:", "Tool preset", "Idle heat timeout"} {
			if !strings.Contains(plain, want) {
				t.Errorf("%dx%d: missing %q:\n%s", w, h, want, plain)
			}
		}
		footer := footerOf(out)
		for _, want := range []string{"enter continue", "esc back", "q cancel"} {
			if !strings.Contains(footer, want) {
				t.Errorf("%dx%d: footer = %q, want %q", w, h, footer, want)
			}
		}
	}
}

func TestSettingsStepTypingFooterHasNoQuit(t *testing.T) {
	s := newSettingsStep()
	st := readySettingsState()
	s.Update(keyType(tea.KeyDown), st)
	s.Update(keyRune('4'), st)
	footer := footerOf(s.View(st))
	if strings.Contains(footer, "q cancel") || !strings.Contains(footer, "esc cancel") {
		t.Errorf("footer while typing = %q", footer)
	}
}

func TestSettingsStepSavingFooterAndLoading(t *testing.T) {
	s := newSettingsStep()
	st := readySettingsState()
	st.Settings.Saving = true
	if got := footerOf(s.View(st)); got != "saving... · ctrl+c quit" {
		t.Errorf("footer while saving = %q", got)
	}
	st.Settings.Saving, st.Settings.Ready = false, false
	out := s.View(st)
	assertFrame(t, out, 120, 36)
	if !strings.Contains(ansi.Strip(out), "Loading settings...") {
		t.Errorf("loading line missing:\n%s", ansi.Strip(out))
	}
	if got := footerOf(out); got != "q cancel" {
		t.Errorf("footer while loading = %q", got)
	}
}

func TestSettingsStepWroteOnlyAfterARealSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, err := domain.SettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		s := newSettingsStep()
		s.opts.DryRun = dry
		st := readySettingsState()
		st.Settings.Path = path
		_, cmd := s.Update(keyType(tea.KeyEnter), st)
		saved, ok := cmd().(settingsSavedMsg)
		if !ok || saved.err != nil {
			t.Fatalf("dry=%v: save failed: %+v", dry, saved)
		}
		s.Update(saved, st)
		if st.Settings.Wrote == dry {
			t.Errorf("dry=%v: Wrote = %v", dry, st.Settings.Wrote)
		}
	}
}

func TestListWindowKeepsTheCursorBlockVisible(t *testing.T) {
	blocks := make([][]string, 10)
	for i := range blocks {
		blocks[i] = []string{fmt.Sprintf("row%d", i), fmt.Sprintf("note%d", i)}
	}
	for cursor := range blocks {
		got := listWindow(blocks, cursor, 5)
		if len(got) > 5 {
			t.Fatalf("cursor %d: %d lines, limit 5", cursor, len(got))
		}
		if !strings.Contains(strings.Join(got, ","), fmt.Sprintf("row%d,note%d", cursor, cursor)) {
			t.Errorf("cursor %d: window %v lacks the whole cursor block", cursor, got)
		}
	}
	if got := listWindow(blocks[:2], 0, 10); len(got) != 4 {
		t.Errorf("a list that fits is shown whole, got %v", got)
	}
	if got := listWindow(nil, 0, 5); got != nil {
		t.Errorf("empty list gave %v", got)
	}
}
