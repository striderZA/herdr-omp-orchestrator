package board

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"herdr-omp-orchestrator/internal/task"
)

func longBrief() string {
	var words []string
	for i := range 120 {
		words = append(words, fmt.Sprintf("word%03d", i))
	}
	return strings.Join(words, " ")
}

// TestDetailPageScrollsAndReturns covers issue #1: Enter opens a page that
// holds a brief taller than the terminal, arrows/PgDn scroll it without moving
// the selection, and Esc returns to the board.
func TestDetailPageScrollsAndReturns(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	long := sampleTask("orch_d1", func(x *task.Task) { x.Brief = longBrief() })
	f.seed(t, long)
	m := f.model(t, Options{})
	before := f.snapshot()

	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	view := m.View()
	if lines := strings.Split(view, "\n"); len(lines) > 10 {
		t.Fatalf("page is %d lines, terminal is 10:\n%s", len(lines), view)
	}
	if !strings.Contains(view, "orch_d1") || strings.Contains(view, "word119") {
		t.Fatalf("first view should show the task header, not the end of the brief:\n%s", view)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if !strings.Contains(m.View(), "word000") {
		t.Fatalf("PgDn should reach the brief:\n%s", m.View())
	}

	press(t, m, tea.KeyMsg{Type: tea.KeyEnd})
	view = m.View()
	if !strings.Contains(view, "word119") || strings.Contains(view, "word000") {
		t.Fatalf("End should show the end of the brief:\n%s", view)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	press(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if !strings.Contains(m.View(), "word119") {
		t.Errorf("scrolling past the end must clamp:\n%s", m.View())
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyHome})
	if !strings.Contains(m.View(), "orch_d1 |") {
		t.Errorf("Home should return to the top:\n%s", m.View())
	}
	if m.SelectedID() != long.ID {
		t.Errorf("scrolling changed the selection to %q", m.SelectedID())
	}

	press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.expanded || !strings.Contains(m.View(), StageLabel(task.Queued)) && !strings.Contains(m.View(), StageLabel(task.Running)) {
		t.Errorf("Esc did not return to the board:\n%s", m.View())
	}
	if m.SelectedID() != long.ID {
		t.Errorf("returning changed the selection to %q", m.SelectedID())
	}
	if after := f.snapshot(); !reflect.DeepEqual(before, after) {
		t.Error("the detail page changed durable facts")
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("the detail page called Herdr: %v", calls)
	}
}

// TestDetailPageSurvivesRefreshAndNarrowTerminals keeps the page open on the
// same task ID across a refresh and never exceeds the terminal width.
func TestDetailPageSurvivesRefreshAndNarrowTerminals(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	card := sampleTask("orch_d2", func(x *task.Task) { x.Brief = longBrief() })
	f.seed(t, card)
	m := f.model(t, Options{})
	m.Update(tea.WindowSizeMsg{Width: 12, Height: 8})
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	press(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	m.Update(tickMsg{})
	if !m.expanded || m.SelectedID() != card.ID {
		t.Fatalf("refresh closed the page or moved selection: expanded=%v selected=%q", m.expanded, m.SelectedID())
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if got := runeWidth(line); got > 12 {
			t.Errorf("line %q is %d columns wide", line, got)
		}
	}
}

// TestDetailPageNeverExceedsTheTerminalHeight covers tiny terminals and a
// focus notice: the page, rule, notice and help together fit the height.
func TestDetailPageNeverExceedsTheTerminalHeight(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	card := sampleTask("orch_d3", func(x *task.Task) { x.Brief = longBrief() })
	f.seed(t, card)
	m := f.model(t, Options{})
	m.notice = "Cannot focus orch_d3: refused"
	m.noticeErr = true
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	for height := 1; height <= 12; height++ {
		m.Update(tea.WindowSizeMsg{Width: 30, Height: height})
		press(t, m, tea.KeyMsg{Type: tea.KeyEnd})
		view := m.View()
		if got := len(strings.Split(view, "\n")); got > height {
			t.Errorf("height %d: page is %d lines:\n%s", height, got, view)
		}
		for _, line := range strings.Split(view, "\n") {
			var from, to, total int
			if n, _ := fmt.Sscanf(line, "%d-%d/%d", &from, &to, &total); n == 3 && (from < 1 || to < from || to > total) {
				t.Errorf("height %d: impossible range in %q", height, line)
			}
		}
	}
}
