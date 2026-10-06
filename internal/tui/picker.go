package tui

import (
	"fmt"
	"sync/atomic"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sairaph/creality-k2-mcp/internal/domain"
	"github.com/sairaph/creality-k2-mcp/internal/tui/frame"
)

var nextPickerID atomic.Int64

// pickerLoadedMsg is the enabled printers read from the registry for one
// picker (the id keeps a late result of a screen already left out of the next
// screen).
type pickerLoadedMsg struct {
	id       int64
	printers []domain.Printer
	err      error
}

// printerPicker loads the enabled printers for Status and Camera and lets the
// user choose one. With exactly one enabled printer the screen opens it
// directly; the picker never probes a printer (the state is shown on Status).
type printerPicker struct {
	id       int64
	loaded   bool
	err      string
	printers []domain.Printer
	list     selectList
}

func newPrinterPicker() printerPicker { return printerPicker{id: nextPickerID.Add(1)} }

func (p *printerPicker) loadCmd(deps Deps) tea.Cmd {
	dir, id := deps.Dir, p.id
	return func() tea.Msg {
		reg, _, _, err := domain.LoadRegistry(dir)
		if err != nil {
			return pickerLoadedMsg{id: id, err: err}
		}
		return pickerLoadedMsg{id: id, printers: reg.Enabled()}
	}
}

// retry forgets the last result and loads again.
func (p *printerPicker) retry(deps Deps) tea.Cmd {
	p.loaded, p.err = false, ""
	return p.loadCmd(deps)
}

// apply takes the load result and reports whether the message was this
// picker's own.
func (p *printerPicker) apply(msg tea.Msg) bool {
	m, ok := msg.(pickerLoadedMsg)
	if !ok || m.id != p.id {
		return false
	}
	p.loaded = true
	if m.err != nil {
		p.err = m.err.Error()
		return true
	}
	p.printers = m.printers
	p.list.clamp(len(p.printers))
	return true
}

// only returns the printer to open directly: the single enabled one.
func (p *printerPicker) only() (domain.Printer, bool) {
	if p.loaded && p.err == "" && len(p.printers) == 1 {
		return p.printers[0], true
	}
	return domain.Printer{}, false
}

// key handles list movement and enter; it returns the chosen printer on enter.
func (p *printerPicker) key(k tea.KeyMsg, h int) (domain.Printer, bool) {
	if !p.loaded || p.err != "" || len(p.printers) == 0 {
		return domain.Printer{}, false
	}
	if p.list.key(k, len(p.printers), h) {
		return domain.Printer{}, false
	}
	if k.String() == "enter" {
		return p.printers[p.list.cursor], true
	}
	return domain.Printer{}, false
}

// body renders the picker states; spin is the loading spinner glyph.
func (p *printerPicker) body(w, h int, spin string) []string {
	switch {
	case !p.loaded:
		return []string{frame.Gutter + spin + " Loading printers..."}
	case p.err != "":
		return wrapStyled(w, styleError, p.err)
	case len(p.printers) == 0:
		return []string{frame.Gutter + "No printer is enabled. Enable one in Printers."}
	}
	nameW := 8
	for _, pr := range p.printers {
		nameW = max(nameW, len([]rune(pr.Name)))
	}
	from, to := p.list.window(len(p.printers), h)
	out := make([]string, 0, to-from)
	for i := from; i < to; i++ {
		pr := p.printers[i]
		out = append(out, frame.Marker(i == p.list.cursor)+frame.Pad(pr.Name, nameW+2)+styleDim.Render(pr.Host))
	}
	return out
}

// hints is the picker footer: move and open when there is something to pick,
// retry on a load error.
func (p *printerPicker) hints() []frame.Hint {
	var out []frame.Hint
	switch {
	case p.err != "":
		out = append(out, frame.Hint{Keys: "r", Label: "retry", Priority: 80})
	case p.loaded && len(p.printers) > 0:
		out = append(out,
			frame.Hint{Keys: "↑↓", Label: "move", Priority: 70},
			frame.Hint{Keys: "enter", Label: "open", Priority: 80})
	}
	return append(out, frame.Back(), frame.Quit())
}

// printerContext is the header context for a screen about one printer.
func printerContext(p domain.Printer) string {
	return fmt.Sprintf("%s (%s)", p.Name, p.Host)
}
