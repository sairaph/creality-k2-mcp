package frame

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// styled is a hand-built ANSI line: lipgloss emits no escapes without a
// terminal, so the tests carry their own colour codes to prove the clipping is
// ANSI-aware.
func styled(s string) string { return "\x1b[1;38;5;81m" + s + "\x1b[0m" }

var testHeader = Header{Name: "Status", Context: "K2-5885 (192.168.1.102)"}

func TestScreenLayoutAtCommonSizes(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 36}} {
		w, h := size[0], size[1]
		body := []string{"  first", "  second"}
		out := Screen(w, h, testHeader, body, Footer(w, Back(), Quit()))
		lines := strings.Split(out, "\n")
		if len(lines) != h {
			t.Fatalf("%dx%d: %d lines, want exactly %d", w, h, len(lines), h)
		}
		if strings.HasSuffix(out, "\n") {
			t.Errorf("%dx%d: output has a trailing newline", w, h)
		}
		if !strings.HasPrefix(ansi.Strip(lines[0]), "creality-k2-mcp  Status  K2-5885") {
			t.Errorf("%dx%d: row 1 = %q, want the header", w, h, lines[0])
		}
		if lines[1] != "" {
			t.Errorf("%dx%d: row 2 = %q, want blank", w, h, lines[1])
		}
		if lines[2] != "  first" || lines[3] != "  second" {
			t.Errorf("%dx%d: body rows = %q, %q", w, h, lines[2], lines[3])
		}
		if got := ansi.Strip(lines[h-1]); got != "  esc back · q quit" {
			t.Errorf("%dx%d: last row = %q, want the footer", w, h, got)
		}
		for i := 4; i < h-1; i++ {
			if lines[i] != "" {
				t.Errorf("%dx%d: row %d = %q, want blank padding", w, h, i+1, lines[i])
			}
		}
	}
}

func TestScreenClipsBodyToRowsThatFit(t *testing.T) {
	w, h := 80, 24
	var body []string
	for i := 0; i < 100; i++ {
		body = append(body, "  line")
	}
	lines := strings.Split(Screen(w, h, testHeader, body, "  footer"), "\n")
	if len(lines) != h {
		t.Fatalf("%d lines, want %d", len(lines), h)
	}
	if ansi.Strip(lines[h-1]) != "  footer" {
		t.Errorf("last row = %q, the footer must never be clipped away", lines[h-1])
	}
	if lines[h-2] != "  line" {
		t.Errorf("row h-1 = %q, want the last body row (body is clipped to h-3 = %d rows)", lines[h-2], h-3)
	}
	count := 0
	for _, l := range lines[2 : h-1] {
		if l == "  line" {
			count++
		}
	}
	if count != h-3 {
		t.Errorf("body rows = %d, want %d", count, h-3)
	}
}

func TestScreenCutsEveryRowIncludingStyledOnes(t *testing.T) {
	w, h := 80, 24
	long := strings.Repeat("x", 300)
	body := []string{"  " + long, styled("  " + long), strings.Repeat("界", 100)}
	header := Header{Name: long, Context: long, DryRun: true}
	out := Screen(w, h, header, body, "  "+long)
	for i, line := range strings.Split(out, "\n") {
		if got := ansi.StringWidth(line); got > w-1 {
			t.Errorf("row %d is %d columns wide, want <= %d", i+1, got, w-1)
		}
	}
	if n := len(strings.Split(out, "\n")); n != h {
		t.Errorf("%d lines, want %d (an overflowing row must not add rows)", n, h)
	}
}

func TestScreenTinyHeights(t *testing.T) {
	for h := 1; h <= 4; h++ {
		out := Screen(80, h, testHeader, []string{"  a", "  b"}, "  foot")
		lines := strings.Split(out, "\n")
		if len(lines) != h {
			t.Fatalf("h=%d: %d lines", h, len(lines))
		}
		if h >= 3 && ansi.Strip(lines[h-1]) != "  foot" {
			t.Errorf("h=%d: last row = %q, want the footer", h, lines[h-1])
		}
	}
	if Screen(80, 0, testHeader, nil, "") != "" {
		t.Error("h=0 should draw nothing")
	}
}

func TestHeaderDryRunMarker(t *testing.T) {
	got := ansi.Strip(Header{App: "creality-k2-mcp setup", Name: "Printers", Context: "step 1 of 4", DryRun: true}.render())
	if got != "creality-k2-mcp setup  Printers  step 1 of 4  dry run" {
		t.Errorf("header = %q", got)
	}
}

func footerHints() []Hint {
	return []Hint{
		{Keys: "↑↓", Label: "move", Priority: 90},
		{Keys: "space", Label: "enabled", Priority: 80},
		{Keys: "c", Label: "control", Priority: 70},
		{Keys: "m", Label: "add host", Priority: 60},
		{Keys: "r", Label: "rescan", Priority: 20},
		Back(), Quit(),
	}
}

func TestFooterPicksTheLongestCandidateThatFits(t *testing.T) {
	hints := footerHints()
	full := "  ↑↓ move · space enabled · c control · m add host · r rescan · esc back · q quit"
	cases := []struct {
		w    int
		want string
	}{
		{200, full},
		// one column short of the full line: the lowest priority (r rescan) goes first
		{ansi.StringWidth(full), "  ↑↓ move · space enabled · c control · m add host · esc back · q quit"},
		{60, "  ↑↓ move · space enabled · c control · esc back · q quit"},
		{36, "  ↑↓ move · esc back · q quit"},
		{22, "  esc back · q quit"},
		{12, "  q quit"},
		{3, "  q quit"},
	}
	for _, tc := range cases {
		got := Footer(tc.w, hints...)
		if got != tc.want {
			t.Errorf("Footer(%d) = %q, want %q", tc.w, got, tc.want)
		}
		if tc.w > 12 && ansi.StringWidth(got) > tc.w-1 {
			t.Errorf("Footer(%d) is %d columns wide, want <= %d", tc.w, ansi.StringWidth(got), tc.w-1)
		}
	}
}

func TestFooterKeepsBackAndQuitLastAndDropsTiesRightToLeft(t *testing.T) {
	hints := []Hint{
		Back(), Quit(), // given first, must still end the line
		{Keys: "a", Label: "alpha", Priority: 10},
		{Keys: "b", Label: "beta", Priority: 10},
	}
	if got := Footer(200, hints...); got != "  a alpha · b beta · esc back · q quit" {
		t.Errorf("Footer = %q, want back and quit last", got)
	}
	// "  a alpha · esc back · q quit" is 29 wide: only the right-hand tie (beta) is dropped.
	if got := Footer(30, hints...); got != "  a alpha · esc back · q quit" {
		t.Errorf("Footer = %q, want the right-hand equal-priority hint dropped first", got)
	}
}

func TestFooterEdgeCases(t *testing.T) {
	if Footer(80) != "" {
		t.Error("no hints should give an empty footer")
	}
	busy := []Hint{{Label: "saving...", Priority: 50}, {Keys: "ctrl+c", Label: "quit", Priority: PriorityQuit}}
	if got := Footer(80, busy...); got != "  saving... · ctrl+c quit" {
		t.Errorf("Footer = %q", got)
	}
	if got := Footer(80, Cancel()); got != "  q cancel" {
		t.Errorf("Footer = %q", got)
	}
}

func TestRowsAlignAndWrapUnderTheValueColumn(t *testing.T) {
	w := 60
	rows := Rows(w, 16,
		KV{Label: "State", Value: "Idle"},
		KV{Label: "Details", Value: strings.Repeat("word ", 30)},
		KV{Label: styled("Styled"), Value: "a\nb"},
	)
	if rows[0] != "  State           Idle" {
		t.Errorf("row 0 = %q", rows[0])
	}
	pad := "  " + strings.Repeat(" ", 16)
	for i, r := range rows {
		if got := ansi.StringWidth(r); got > w-1 {
			t.Errorf("row %d is %d columns wide: %q", i, got, r)
		}
	}
	if len(rows) < 6 {
		t.Fatalf("rows = %q, want the long value wrapped onto several lines", rows)
	}
	for _, r := range rows[2 : len(rows)-2] {
		if !strings.HasPrefix(r, pad) {
			t.Errorf("continuation %q is not indented to the value column", r)
		}
	}
	last := rows[len(rows)-2:]
	if ansi.Strip(last[0]) != "  Styled          a" || last[1] != pad+"b" {
		t.Errorf("styled label rows = %q", last)
	}
}

func TestWrapIndentsAndRespectsWidth(t *testing.T) {
	lines := Wrap(40, "    ", "The quick brown fox jumps over the lazy dog and keeps running\nsecond paragraph")
	if len(lines) < 3 {
		t.Fatalf("lines = %q", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "    ") {
			t.Errorf("line %q lost its indent", l)
		}
		if ansi.StringWidth(l) > 39 {
			t.Errorf("line %q is wider than w-1", l)
		}
	}
	long := Wrap(20, "", strings.Repeat("x", 50))
	for _, l := range long {
		if ansi.StringWidth(l) > 19 {
			t.Errorf("an over-long word was not broken: %q", l)
		}
	}
	if got := Wrap(40, "  ", ""); len(got) != 1 || got[0] != "" {
		t.Errorf("Wrap of empty text = %q", got)
	}
}

func TestMarker(t *testing.T) {
	if got := ansi.Strip(Marker(true)); got != "  > " {
		t.Errorf("selected marker = %q", got)
	}
	if got := Marker(false); got != "    " {
		t.Errorf("plain marker = %q", got)
	}
}

func TestTooSmall(t *testing.T) {
	cases := []struct {
		w, h  int
		small bool
	}{
		{60, 16, false}, {80, 24, false}, {59, 24, true}, {80, 15, true}, {40, 10, true},
	}
	for _, tc := range cases {
		msg, small := TooSmall(tc.w, tc.h)
		if small != tc.small {
			t.Errorf("TooSmall(%d,%d) = %v, want %v", tc.w, tc.h, small, tc.small)
		}
		if small {
			lines := strings.Split(msg, "\n")
			if len(lines) != 2 || !strings.Contains(lines[0], "Terminal is") || !strings.Contains(lines[1], "needs at least 60x16.") {
				t.Errorf("message = %q", msg)
			}
		} else if msg != "" {
			t.Errorf("message = %q for a big enough terminal", msg)
		}
	}
	if msg, _ := TooSmall(50, 12); !strings.Contains(msg, "50x12") {
		t.Errorf("message = %q, want the real size", msg)
	}
}

func TestSpinnerCyclesAsciiGlyphs(t *testing.T) {
	seen := ""
	for i := 0; i < 4; i++ {
		seen += Spinner(i)
	}
	if seen != `-\|/` || Spinner(4) != "-" || Spinner(-1) != `\` {
		t.Errorf("spinner glyphs = %q", seen)
	}
}
