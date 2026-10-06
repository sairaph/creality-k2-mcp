package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

type menuItem struct {
	label  string
	action string
	desc   string
}

var menuItems = []menuItem{
	{"Printers", "printers", "find, enable and control printers"},
	{"Status", "status", "live state of a printer"},
	{"Camera", "camera", "live view, snapshots and recordings"},
	{"Recordings", "recordings", "list, open and delete recordings"},
	{"Settings", "settings", "tool preset and safety limits"},
	{"Doctor", "doctor", "check the installation"},
	{"Quit", "quit", ""},
}

// menuDescMinWidth is the terminal width from which each item shows its
// one-line description.
const menuDescMinWidth = 90

// menuScreen is the main menu. It never leaves; the root reads the item the
// user opened through take.
type menuScreen struct {
	version string
	list    selectList
	chosen  string
}

func newMenuScreen(version string) *menuScreen { return &menuScreen{version: version} }

func (s *menuScreen) Init() tea.Cmd { return nil }

// take returns the action the user opened since the last call, and clears it.
func (s *menuScreen) take() string {
	c := s.chosen
	s.chosen = ""
	return c
}

func (s *menuScreen) Update(msg tea.Msg) (tea.Cmd, Nav) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil, NavNone
	}
	if s.list.key(k, len(menuItems), 1) {
		return nil, NavNone
	}
	if k.String() == "enter" {
		item := menuItems[s.list.cursor]
		if item.action == "quit" {
			return nil, NavQuit
		}
		s.chosen = item.action
	}
	return nil, NavNone
}

// Back is a no-op on the menu: there is nowhere to go back to.
func (s *menuScreen) Back() bool { return true }

func (s *menuScreen) Body(w, h int) []string {
	from, to := s.list.window(len(menuItems), h)
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		item := menuItems[i]
		line := frame.Marker(i == s.list.cursor) + frame.Pad(item.label, 12)
		if w >= menuDescMinWidth && item.desc != "" {
			line += styleDim.Render(item.desc)
		}
		out = append(out, line)
	}
	return out
}

func (s *menuScreen) Hints(w, h int) []frame.Hint {
	return []frame.Hint{
		{Keys: "↑↓", Label: "move", Priority: 70},
		{Keys: "enter", Label: "open", Priority: 80},
		frame.Quit(),
	}
}

func (s *menuScreen) Header() frame.Header {
	return frame.Header{Name: "Menu", Context: s.version}
}

func (s *menuScreen) Mode() Mode { return ModeNormal }
