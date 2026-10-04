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

// TestDetailPageScrollIsReclampedWhenTheViewportGrows guards against a stale
// offset: after the terminal grows, the first Up must move the visible page.
func TestDetailPageScrollIsReclampedWhenTheViewportGrows(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_d4", func(x *task.Task) { x.Brief = longBrief() }))
	m := f.model(t, Options{})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 6})
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	press(t, m, tea.KeyMsg{Type: tea.KeyEnd})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	max := len(pageLines(mustCard(t, m), m.width, m.opts.Theme)) - 28
	if max < 0 {
		max = 0
	}
	if m.scroll > max {
		t.Fatalf("scroll %d exceeds the new maximum %d", m.scroll, max)
	}
	before := m.View()
	if max > 0 {
		press(t, m, tea.KeyMsg{Type: tea.KeyUp})
		if m.View() == before {
			t.Error("Up after a resize did not move the page")
		}
	}
}

func mustCard(t *testing.T, m *Model) Card {
	t.Helper()
	card, ok := m.board.Find(m.selected)
	if !ok {
		t.Fatal("selected task missing")
	}
	return card
}

// A report stored with free-text checks shows them exactly as it always did; a
// report that states outcomes shows each outcome (and its detail) next to the
// check, on the detail page and in the expanded card. Both come from one stored
// task, so this is also the "legacy rows render unchanged" guarantee.
func TestCheckOutcomesAreShownAndLegacyChecksAreNot(t *testing.T) {
	report := func(checks ...task.Check) func(*task.Task) {
		return func(x *task.Task) {
			x.Report = &task.Report{
				Kind: "result", Summary: "done", Branch: x.Branch,
				Evidence: []string{"commit 4a5b6c7"}, Checks: checks, Risk: "none",
			}
		}
	}
	legacy := Card{Task: sampleTask("orch_lg", report(
		task.Check{Name: "go test ./internal/parser ok"},
		task.Check{Name: "gofmt -l . (no output)"},
	))}
	stated := Card{Task: sampleTask("orch_st", report(
		task.Check{Name: "go test ./...", Outcome: task.CheckPassed},
		task.Check{Name: "go vet ./...", Outcome: task.CheckFailed, Detail: "2 findings"},
		task.Check{Name: "golangci-lint run", Outcome: task.CheckSkipped, Detail: "not installed"},
	))}

	page := func(card Card) string {
		return strings.Join(pageLines(card, 200, Theme{}), "\n")
	}
	for _, want := range []string{"Check: go test ./internal/parser ok", "Check: gofmt -l . (no output)"} {
		if got := page(legacy); !strings.Contains(got, want) {
			t.Errorf("a legacy report's page changed; it no longer shows %q:\n%s", want, got)
		}
	}
	for _, want := range []string{
		"Check: go test ./...: passed",
		"Check: go vet ./...: failed (2 findings)",
		"Check: golangci-lint run: skipped (not installed)",
	} {
		if got := page(stated); !strings.Contains(got, want) {
			t.Errorf("the page does not show %q:\n%s", want, got)
		}
	}
	if got := page(legacy); strings.Contains(got, "passed") || strings.Contains(got, "failed") || strings.Contains(got, "skipped") {
		t.Errorf("a legacy report's page claims an outcome nobody recorded:\n%s", got)
	}

	card := func(c Card) string {
		return Render(NewBoard([]Card{c}), Detail{SelectedID: c.ID(), Expanded: true}, 200, Theme{})
	}
	if got := card(legacy); !strings.Contains(got, "Checks: go test ./internal/parser ok, gofmt -l . (no output)") {
		t.Errorf("a legacy report's expanded card changed:\n%s", got)
	}
	if got := card(stated); !strings.Contains(got,
		"Checks: go test ./...: passed, go vet ./...: failed (2 findings), golangci-lint run: skipped (not installed)") {
		t.Errorf("the expanded card does not show the outcomes:\n%s", got)
	}
}
