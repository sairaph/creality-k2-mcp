package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestViewIsEmptyUntilTheFirstSizeMessage(t *testing.T) {
	isolateHome(t)
	m := newModel(t.Context(), "0.4.0-test", Deps{})
	if got := m.View(); got != "" {
		t.Fatalf("View before any WindowSizeMsg = %q, want empty", got)
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.View() == "" {
		t.Fatal("View after the size message is empty")
	}
}

func TestMenuFrameAtBothSizes(t *testing.T) {
	for _, sz := range sizes {
		isolateHome(t)
		h := newHarness(t, Deps{}, sz[0], sz[1])
		h.requireFrame("menu")
		ls := h.lines()
		if got := ls[0]; got != "creality-k2-mcp  Menu  0.4.0-test" {
			t.Errorf("%dx%d: header = %q", sz[0], sz[1], got)
		}
		for i, item := range menuItems {
			if want := item.label; !strings.Contains(ls[2+i], want) {
				t.Errorf("%dx%d: row %d = %q, want it to hold %q", sz[0], sz[1], 3+i, ls[2+i], want)
			}
		}
		if got := strings.TrimSpace(h.footer()); got != "↑↓ move · enter open · q quit" {
			t.Errorf("%dx%d: footer = %q", sz[0], sz[1], got)
		}
		if !strings.HasPrefix(ls[2], "  > Printers") {
			t.Errorf("%dx%d: first row = %q, want the cursor on Printers", sz[0], sz[1], ls[2])
		}
		hasDesc := strings.Contains(h.text(), "live state of a printer")
		if want := sz[0] >= menuDescMinWidth; hasDesc != want {
			t.Errorf("%dx%d: descriptions shown = %v, want %v", sz[0], sz[1], hasDesc, want)
		}
	}
}

func TestMenuKeys(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, Deps{}, 120, 36)

	h.key("down", "down")
	if !strings.HasPrefix(h.lines()[4], "  > Camera") {
		t.Errorf("after two downs: %q, want the cursor on Camera", h.lines()[4])
	}
	h.key("up")
	if !strings.HasPrefix(h.lines()[3], "  > Status") {
		t.Errorf("after up: %q, want the cursor on Status", h.lines()[3])
	}
	for i := 0; i < 20; i++ {
		h.key("up")
	}
	if !strings.HasPrefix(h.lines()[2], "  > Printers") {
		t.Errorf("the cursor must stop at the first item: %q", h.lines()[2])
	}

	// esc has nowhere to go from the menu: documented no-op.
	h.key("esc")
	if !strings.Contains(h.lines()[0], "Menu") || h.quit {
		t.Errorf("esc on the menu changed something: header %q quit=%v", h.lines()[0], h.quit)
	}
}

func TestMenuOpensEachScreenAndEscReturns(t *testing.T) {
	cases := []struct{ item, header string }{
		{"Printers", "creality-k2-mcp  Printers"},
		{"Status", "creality-k2-mcp  Status"},
		{"Camera", "creality-k2-mcp  Camera"},
		{"Recordings", "creality-k2-mcp  Recordings"},
		{"Settings", "creality-k2-mcp  Settings"},
		{"Doctor", "creality-k2-mcp  Doctor"},
	}
	for _, sz := range sizes {
		for _, tc := range cases {
			isolateHome(t)
			h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil), Daemon: &fakeDaemonClient{}}, sz[0], sz[1])
			h.open(tc.item)
			if !strings.HasPrefix(h.lines()[0], tc.header) {
				t.Fatalf("%s at %dx%d: header = %q, want it to start with %q", tc.item, sz[0], sz[1], h.lines()[0], tc.header)
			}
			h.requireFrame(tc.item)

			h.key("esc")
			if !strings.Contains(h.lines()[0], "Menu") {
				t.Errorf("%s at %dx%d: esc did not return to the menu: %q", tc.item, sz[0], sz[1], h.lines()[0])
			}
			if h.quit {
				t.Errorf("%s: esc must not quit", tc.item)
			}
			// The menu keeps its cursor on the item that was open.
			if !strings.Contains(h.text(), "> "+tc.item) {
				t.Errorf("%s at %dx%d: the menu lost its cursor:\n%s", tc.item, sz[0], sz[1], h.text())
			}
		}
	}
}

func TestQQuitsFromEveryScreen(t *testing.T) {
	for _, item := range []string{"Printers", "Status", "Camera", "Recordings", "Settings", "Doctor"} {
		isolateHome(t)
		h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil), Daemon: &fakeDaemonClient{}}, 120, 36)
		h.open(item)
		h.key("q")
		if !h.quit {
			t.Errorf("q did not quit from %s", item)
		}
	}
	isolateHome(t)
	h := newHarness(t, Deps{}, 80, 24)
	h.key("q")
	if !h.quit {
		t.Error("q did not quit from the menu")
	}
}

func TestCtrlCQuitsFromEveryMode(t *testing.T) {
	isolateHome(t)
	addPrinter(t, "k2-5885", "K2-5885", "192.168.1.10", true, false)
	h := newHarness(t, idleDeps(map[string]string{"k2-5885": "K2-5885"}, nil), 120, 36)
	h.open("Printers")
	h.key("m") // typing
	h.key("ctrl+c")
	if !h.quit {
		t.Error("ctrl+c did not quit while typing")
	}

	h = newHarness(t, idleDeps(nil, nil), 120, 36)
	h.key("ctrl+c")
	if !h.quit {
		t.Error("ctrl+c did not quit on the menu")
	}
}

func TestQuitItemQuits(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, Deps{}, 80, 24)
	h.open("Quit")
	if !h.quit {
		t.Error("the Quit item did not quit")
	}
}

func TestTooSmallTerminalReplacesEveryView(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, Deps{PrinterClients: fakePrinterClients(nil)}, 80, 24)
	h.open("Settings")

	h.resize(59, 24)
	got := ansi.Strip(h.m.View())
	if got != "  Terminal is 59x24.\n  creality-k2-mcp needs at least 60x16." {
		t.Errorf("too-small view = %q", got)
	}
	h.resize(80, 15)
	if !strings.Contains(ansi.Strip(h.m.View()), "Terminal is 80x15.") {
		t.Errorf("too-small view = %q", h.m.View())
	}

	// Keys other than q and ctrl+c do nothing while the terminal is too small.
	h.key("esc")
	if h.m.screen == Screen(h.m.menu) {
		t.Error("esc acted while the terminal was too small")
	}

	h.resize(80, 24)
	h.requireFrame("settings after growing again")
	h.resize(40, 10)
	h.key("q")
	if !h.quit {
		t.Error("q must keep working on a too-small terminal")
	}
}

func TestResizeReclampsWithoutLosingState(t *testing.T) {
	isolateHome(t)
	h := newHarness(t, Deps{}, 120, 36)
	h.key("down", "down", "down", "down", "down")
	h.resize(80, 16) // 13 body rows still hold the 7 items
	h.requireFrame("menu 80x16")
	if !strings.Contains(h.text(), "> Doctor") {
		t.Errorf("the cursor row is gone after the resize:\n%s", h.text())
	}
	h.resize(60, 16)
	h.requireFrame("menu 60x16")
}
