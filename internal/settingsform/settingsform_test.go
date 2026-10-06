package settingsform

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
)

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }
func run(r rune) tea.KeyMsg        { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func newForm() *Form {
	f := &Form{}
	f.Load(domain.DefaultSettings())
	return f
}

func press(f *Form, msgs ...tea.KeyMsg) Event {
	var ev Event
	for _, m := range msgs {
		_, ev = f.Update(m)
	}
	return ev
}

func hintKeys(f *Form) string {
	var keys []string
	for _, h := range f.Hints() {
		keys = append(keys, h.Keys+" "+h.Label)
	}
	return strings.Join(keys, " | ")
}

func bodyText(f *Form, w int) string { return ansi.Strip(strings.Join(f.Body(w), "\n")) }

func TestLoadShowsDefaultsAndIsClean(t *testing.T) {
	f := newForm()
	if f.Dirty() || f.Typing() {
		t.Fatal("a freshly loaded form is clean and not typing")
	}
	if got := f.Values(); got.IdleHeatMinutes != 15 || got.Tools.Preset != domain.PresetCamera {
		t.Errorf("Values = %+v, want the defaults", got)
	}
	out := bodyText(f, 80)
	for _, want := range []string{"Tool preset", "< camera >", "Idle heat timeout (minutes)", "15", "Flow factor max (%)"} {
		if !strings.Contains(out, want) {
			t.Errorf("body missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Which tools the AI can use") {
		t.Errorf("body should explain the highlighted preset row:\n%s", out)
	}
}

func TestEditNumericFieldAndDirty(t *testing.T) {
	f := newForm()
	press(f, key(tea.KeyDown), run('e'))
	if !f.Typing() || f.input != "15" {
		t.Fatalf("typing=%v input=%q, want editing the current value 15", f.Typing(), f.input)
	}
	f.input = ""
	press(f, run('3'), run('0'), key(tea.KeyEnter))
	if f.Typing() {
		t.Fatal("still typing after confirming a valid value")
	}
	if f.Values().IdleHeatMinutes != 30 {
		t.Fatalf("IdleHeatMinutes = %d, want 30", f.Values().IdleHeatMinutes)
	}
	if !f.Dirty() {
		t.Error("an edited form must be dirty")
	}
}

func TestInvalidEditKeepsEditorOpenWithReason(t *testing.T) {
	f := newForm()
	press(f, key(tea.KeyDown), run('e'))
	f.input = "not a number"
	press(f, key(tea.KeyEnter))
	if !f.Typing() {
		t.Error("an invalid value must keep the editor open")
	}
	if !strings.Contains(bodyText(f, 80), "must be a whole number") {
		t.Errorf("body should show the reason inline:\n%s", bodyText(f, 80))
	}
	if f.Values().IdleHeatMinutes != 15 {
		t.Error("an invalid edit must not be applied")
	}
}

func TestOutOfRangeBandIsRejected(t *testing.T) {
	f := newForm()
	press(f, key(tea.KeyDown), key(tea.KeyDown), run('e')) // nozzle_band_c
	f.input = "500"
	press(f, key(tea.KeyEnter))
	if !f.Typing() || f.Values().Bands.NozzleBandC == 500 {
		t.Errorf("typing=%v band=%v: an out-of-range band must never be applied", f.Typing(), f.Values().Bands.NozzleBandC)
	}
}

func TestPresetChangesInPlace(t *testing.T) {
	f := newForm() // camera
	if !strings.Contains(hintKeys(f), "←→ change") {
		t.Errorf("hints = %s, want the preset row to say ←→ change", hintKeys(f))
	}
	press(f, key(tea.KeyRight))
	if f.Typing() {
		t.Fatal("changing the preset must not open an edit mode")
	}
	if got := f.Values().Tools.Preset; got != domain.PresetControl {
		t.Fatalf("after right: %q, want control", got)
	}
	press(f, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}) // wraps to monitor
	if got := f.Values().Tools.Preset; got != domain.PresetMonitor {
		t.Fatalf("after space: %q, want monitor", got)
	}
	press(f, key(tea.KeyLeft)) // wraps back to control
	if got := f.Values().Tools.Preset; got != domain.PresetControl {
		t.Fatalf("after left: %q, want control", got)
	}
}

func TestTypingANumberStartsEditing(t *testing.T) {
	f := newForm()
	press(f, key(tea.KeyDown), run('3'), run('0'))
	if !f.Typing() || f.input != "30" {
		t.Fatalf("typing=%v input=%q, want editing with 30", f.Typing(), f.input)
	}
	press(f, key(tea.KeyEnter))
	if f.Typing() || f.Values().IdleHeatMinutes != 30 {
		t.Fatalf("typing=%v minutes=%d, want 30 confirmed", f.Typing(), f.Values().IdleHeatMinutes)
	}
}

func TestQIsACharacterWhileEditingAndEscCancels(t *testing.T) {
	f := newForm()
	press(f, key(tea.KeyDown), run('e'))
	press(f, run('q'))
	if f.input != "15q" {
		t.Fatalf("input = %q, want q typed as a character", f.input)
	}
	press(f, key(tea.KeyEsc))
	if f.Typing() {
		t.Fatal("esc must cancel the field")
	}
	if f.Values().IdleHeatMinutes != 15 || f.Dirty() {
		t.Error("cancelling must drop the edit")
	}
	if f.CancelEdit() {
		t.Error("CancelEdit with no editor open should report false")
	}
	press(f, run('e'))
	if !f.CancelEdit() || f.Typing() {
		t.Error("CancelEdit should close an open editor")
	}
}

func TestRestoreDefaults(t *testing.T) {
	f := newForm()
	cfg := f.Values()
	cfg.IdleHeatMinutes = 99
	cfg.Tools.Preset = domain.PresetControl
	f.Load(cfg)
	press(f, run('r'))
	if got := f.Values(); got.IdleHeatMinutes != 15 || got.Tools.Preset != domain.PresetCamera {
		t.Errorf("restore did not reset to domain.DefaultSettings(): %+v", got)
	}
	if !f.Dirty() {
		t.Error("restoring over a loaded custom value is a change")
	}
	if !strings.Contains(bodyText(f, 80), "Recommended defaults restored") {
		t.Errorf("body should say defaults were restored:\n%s", bodyText(f, 80))
	}
}

func TestEnterAsksTheCallerToSaveAndSavedResetsDirty(t *testing.T) {
	f := newForm()
	press(f, key(tea.KeyDown), run('2'), key(tea.KeyEnter))
	if f.Values().IdleHeatMinutes != 2 || !f.Dirty() {
		t.Fatalf("setup: minutes=%d dirty=%v", f.Values().IdleHeatMinutes, f.Dirty())
	}
	if ev := press(f, key(tea.KeyEnter)); ev != EventSave {
		t.Fatalf("enter on the list gave event %v, want EventSave", ev)
	}
	f.Saved()
	if f.Dirty() {
		t.Error("Saved must make the form clean")
	}
	if !strings.Contains(bodyText(f, 80), "Saved.") {
		t.Errorf("body should read Saved.:\n%s", bodyText(f, 80))
	}
	f.SetError("disk full")
	if !strings.Contains(bodyText(f, 80), "disk full") {
		t.Errorf("body should show the save error:\n%s", bodyText(f, 80))
	}
}

func TestListLeavesQAndEscToTheCaller(t *testing.T) {
	f := newForm()
	for _, m := range []tea.KeyMsg{run('q'), key(tea.KeyEsc)} {
		if ev := press(f, m); ev != EventNone || f.Typing() || f.Dirty() {
			t.Errorf("%q changed the form (event %v)", m.String(), ev)
		}
	}
	if _, ev := f.Update(tea.WindowSizeMsg{Width: 80, Height: 24}); ev != EventNone {
		t.Error("non-key messages must be ignored")
	}
}

func TestCursorWrapsAndHintsFollowTheRow(t *testing.T) {
	f := newForm()
	press(f, key(tea.KeyUp)) // wraps to the last row
	if f.cursor != len(rows)-1 {
		t.Fatalf("cursor = %d, want the last row", f.cursor)
	}
	if !strings.Contains(hintKeys(f), "e edit") || strings.Contains(hintKeys(f), "←→") {
		t.Errorf("hints = %s, want e edit on a numeric row", hintKeys(f))
	}
	if !strings.Contains(hintKeys(f), "enter save") {
		t.Errorf("hints = %s, want enter save by default", hintKeys(f))
	}
	f.EnterLabel = "continue"
	if !strings.Contains(hintKeys(f), "enter continue") {
		t.Errorf("hints = %s, want the caller's enter label", hintKeys(f))
	}
	press(f, run('e'))
	if got := hintKeys(f); got != "enter confirm | esc cancel" {
		t.Errorf("editing hints = %s", got)
	}
}

func TestBodyFitsNarrowWidthsAndExplainsTheRow(t *testing.T) {
	f := newForm()
	for _, w := range []int{60, 80, 120} {
		for cursor := 0; cursor < len(rows); cursor++ {
			f.cursor = cursor
			for _, line := range f.Body(w) {
				if ansi.StringWidth(line) > w-1 {
					t.Errorf("w=%d row %d: %q is wider than w-1", w, cursor, line)
				}
			}
		}
	}
	f.cursor = 2
	if !strings.Contains(bodyText(f, 80), "your gcode is never limited") {
		t.Errorf("body should explain the highlighted band:\n%s", bodyText(f, 80))
	}
	if n := len(f.Body(80)); n > 21 {
		t.Errorf("body is %d rows; it must fit the 21 body rows of an 80x24 screen", n)
	}
}
