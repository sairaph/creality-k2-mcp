package wizard

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sairaph/mcp-wizard/flow"
	"github.com/sairaph/mcp-wizard/installer"
)

// HarnessStep wraps installer.HarnessStep so a client toggled off with space
// is also shown as off. mcp-wizard v0.1.1's View marks every key of
// HarnessState.Selected as checked and ignores the value, so a deselected
// client kept its filled dot even though installer.ApplyStep (which reads
// the value) would skip it. Dropping false entries after every Update keeps
// the list truthful without changing what gets registered.
func HarnessStep[T any](inner flow.Step[T], stateFn func(*T) *installer.HarnessState) flow.Step[T] {
	return &harnessStep[T]{inner: inner, stateFn: stateFn}
}

type harnessStep[T any] struct {
	inner   flow.Step[T]
	stateFn func(*T) *installer.HarnessState
}

func (s *harnessStep[T]) ID() string            { return s.inner.ID() }
func (s *harnessStep[T]) Title(state *T) string { return s.inner.Title(state) }
func (s *harnessStep[T]) Init(state *T) tea.Cmd { return s.inner.Init(state) }
func (s *harnessStep[T]) View(state *T) string  { return s.inner.View(state) }
func (s *harnessStep[T]) Hints(state *T) []struct{ Key, Label string } {
	return s.inner.Hints(state)
}

func (s *harnessStep[T]) Update(msg tea.Msg, state *T) (flow.Directive, tea.Cmd) {
	directive, cmd := s.inner.Update(msg, state)
	if s.stateFn != nil {
		if hs := s.stateFn(state); hs != nil {
			for id, on := range hs.Selected {
				if !on {
					delete(hs.Selected, id)
				}
			}
		}
	}
	return directive, cmd
}
