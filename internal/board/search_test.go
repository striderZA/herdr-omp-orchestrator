package board

import (
	"context"
	"herdr-omp-orchestrator/internal/task"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"
)

func TestBoardSearchFiltersTasksAndMovesFilteredOutSelection(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	hidden := sampleTask("orch_a_hidden", nil)
	match := sampleTask("orch_b_match", func(x *task.Task) { x.Title = "Parse the compiler" })
	f.seed(t, hidden, match)
	m := f.model(t, Options{})
	if got := m.SelectedID(); got != hidden.ID {
		t.Fatalf("initial selection = %q, want first task %q", got, hidden.ID)
	}

	typeText(t, m, "/")
	typeText(t, m, "compiler")
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if got := m.SelectedID(); got != match.ID {
		t.Fatalf("search selection = %q, want the first matching task %q", got, match.ID)
	}
	view := m.View()
	if !strings.Contains(view, "Filter: compiler") {
		t.Errorf("the active filter is not visible:\n%s", view)
	}
	if !strings.Contains(view, match.ID) {
		t.Errorf("the matching task is missing:\n%s", view)
	}
	if strings.Contains(view, hidden.ID) {
		t.Errorf("the nonmatching task is still visible:\n%s", view)
	}
}

func applySearch(t *testing.T, m *Model, query string) {
	t.Helper()
	typeText(t, m, "/")
	press(t, m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if query != "" {
		typeText(t, m, query)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
}

func TestBoardSearchMatchesRequestedTaskFields(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	target := sampleTask("orch_idfield", func(x *task.Task) {
		x.Title = "titlefield"
		x.Brief = "brieffield"
		x.Blocker = "blockerfield"
		x.Attention = "attentionfield"
		x.Report = &task.Report{
			Kind:     "reportkind",
			Summary:  "reportsummary",
			Branch:   "reportbranch",
			Evidence: []string{"reportevidence"},
			Checks: []task.Check{{
				Name:    "checkname",
				Outcome: task.CheckPassed,
				Detail:  "checkdetail",
			}},
			Risk: "riskfield",
		}
	})
	rejected := sampleTask("orch_dependency", func(x *task.Task) {
		x.Outcome = task.OutcomeRejected
		x.Disposition = "not accepted"
	})
	dependent := sampleTask("orch_dependent", func(x *task.Task) {
		x.Phase = task.Intent
		x.Dependencies = []string{rejected.ID}
	})
	unrelated := sampleTask("orch_unrelated", nil)
	f.seed(t, target, rejected, dependent, unrelated)
	m := f.model(t, Options{})

	for _, tc := range []struct {
		query string
		want  string
	}{
		{"  ORCH_IDFIELD  ", target.ID},
		{"TITLEFIELD", target.ID},
		{"brieffield", target.ID},
		{"blockerfield", target.ID},
		{"attentionfield", target.ID},
		{"reportkind", target.ID},
		{"reportsummary", target.ID},
		{"reportbranch", target.ID},
		{"reportevidence", target.ID},
		{"checkname", target.ID},
		{"passed", target.ID},
		{"checkdetail", target.ID},
		{"riskfield", target.ID},
		{"re-plan", dependent.ID},
	} {
		applySearch(t, m, tc.query)
		cards := m.board.Cards()
		if len(cards) != 1 || cards[0].ID() != tc.want {
			t.Errorf("query %q matched task IDs %v, want only %q", tc.query, cardIDs(cards), tc.want)
		}
	}
}

func cardIDs(cards []Card) []string {
	ids := make([]string, 0, len(cards))
	for _, card := range cards {
		ids = append(ids, card.ID())
	}
	return ids
}

func TestBoardSearchNoResultsAndClearRestoresSelection(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	original := sampleTask("orch_a_original", nil)
	matching := sampleTask("orch_b_other", func(x *task.Task) { x.Title = "Compiler changes" })
	f.seed(t, original, matching)
	m := f.model(t, Options{})

	applySearch(t, m, "not-present")
	if got := m.SelectedID(); got != "" {
		t.Fatalf("no-result selection = %q, want none", got)
	}
	view := m.View()
	if !strings.Contains(view, `No tasks match "not-present"`) {
		t.Errorf("the empty-result state is not explicit:\n%s", view)
	}
	if strings.Contains(view, "No tasks yet") {
		t.Errorf("a filtered empty result was described as an empty project:\n%s", view)
	}

	applySearch(t, m, "")
	if got := m.SelectedID(); got != original.ID {
		t.Errorf("clearing the filter selected %q, want the original selection %q", got, original.ID)
	}
	if len(m.board.Cards()) != 2 || strings.Contains(m.View(), "Filter:") {
		t.Errorf("clearing the filter did not restore the board:\n%s", m.View())
	}
}

func TestBoardSearchClearRestoresSelectionAfterAResultWasSelected(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	original := sampleTask("orch_a_original", nil)
	matching := sampleTask("orch_b_match", func(x *task.Task) { x.Title = "compiler changes" })
	f.seed(t, original, matching)
	m := f.model(t, Options{})

	applySearch(t, m, "compiler")
	if got := m.SelectedID(); got != matching.ID {
		t.Fatalf("filtered selection = %q, want %q", got, matching.ID)
	}
	applySearch(t, m, "")
	if got := m.SelectedID(); got != original.ID {
		t.Errorf("clearing the filter selected %q, want the original task %q", got, original.ID)
	}
}

func TestBoardSearchLeavesTicketsAndDerivedTaskStagesUnchanged(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	needsInput := sampleTask("orch_attention", func(x *task.Task) { x.Blocker = "needle in blocker" })
	other := sampleTask("orch_unmatched", nil)
	f.seed(t, needsInput, other)
	f.addTicket(t, "tkt_visible", task.ColumnBacklog)
	ctx := context.Background()
	tasksBefore := f.snapshot()
	ticketsBefore, err := f.store.Tickets(ctx, f.project)
	if err != nil {
		t.Fatalf("read tickets before search: %v", err)
	}
	m := f.model(t, Options{})
	selectedTicket := m.SelectedID()

	applySearch(t, m, "needle")
	if got := cardIDs(m.board.Cards()); len(got) != 1 || got[0] != needsInput.ID {
		t.Fatalf("filtered task IDs = %v, want only %q", got, needsInput.ID)
	}
	if got := m.board.Cards()[0].Stage; got != task.NeedsInput {
		t.Errorf("the filtered task stage = %q, want its derived stage %q", got, task.NeedsInput)
	}
	if got := m.SelectedID(); got != selectedTicket {
		t.Errorf("filtering tasks changed selected ticket from %q to %q", selectedTicket, got)
	}
	if !strings.Contains(m.View(), "/ search") {
		t.Errorf("the selected ticket does not advertise the task filter key:\n%s", m.View())
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded || !strings.Contains(m.View(), "Filter: needle") || !strings.Contains(m.View(), "/ search") {
		t.Errorf("the ticket page lost its filter indicator or search key:\n%s", m.View())
	}
	if tickets := m.board.Tickets(); len(tickets) != 1 || tickets[0].ID() != "tkt_visible" {
		t.Errorf("task search changed ticket cards: %+v", tickets)
	}
	if after := f.snapshot(); !reflect.DeepEqual(after, tasksBefore) {
		t.Errorf("search wrote task facts: before=%+v after=%+v", tasksBefore, after)
	}
	ticketsAfter, err := f.store.Tickets(ctx, f.project)
	if err != nil {
		t.Fatalf("read tickets after search: %v", err)
	}
	if !reflect.DeepEqual(ticketsAfter, ticketsBefore) {
		t.Errorf("search wrote ticket facts: before=%+v after=%+v", ticketsBefore, ticketsAfter)
	}
	applySearch(t, m, "no-match")
	view := m.View()
	if !strings.Contains(view, `No tasks match "no-match"`) || !strings.Contains(view, "tkt_visible") {
		t.Errorf("a no-match task search must keep tickets and explain the empty result:\n%s", view)
	}
	if strings.Contains(view, "No tasks yet") {
		t.Errorf("a task filter with visible tickets was described as an empty project:\n%s", view)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("search ran Herdr commands: %v", calls)
	}
}

func TestBoardSearchReappliesAfterRefresh(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	first := sampleTask("orch_first", func(x *task.Task) { x.Title = "compiler first" })
	hidden := sampleTask("orch_hidden", func(x *task.Task) { x.Title = "documentation" })
	f.seed(t, first, hidden)
	m := f.model(t, Options{})
	applySearch(t, m, "compiler")

	added := sampleTask("orch_added", func(x *task.Task) { x.Title = "compiler second" })
	added.ProjectKey = f.project
	if err := f.store.CreateTask(context.Background(), added); err != nil {
		t.Fatalf("add matching task before refresh: %v", err)
	}
	press(t, m, tickMsg{})

	if got := cardIDs(m.board.Cards()); len(got) != 2 || !strings.Contains(strings.Join(got, ","), first.ID) || !strings.Contains(strings.Join(got, ","), added.ID) {
		t.Errorf("refresh did not reapply the active filter: %v", got)
	}
	if strings.Contains(strings.Join(cardIDs(m.board.Cards()), ","), hidden.ID) {
		t.Errorf("refresh exposed a nonmatching task: %v", cardIDs(m.board.Cards()))
	}
	if !strings.Contains(m.View(), "Filter: compiler") {
		t.Errorf("refresh lost the visible query:\n%s", m.View())
	}
}

func TestBoardSearchCancelKeepsTheAppliedQueryAndCapturesBoardKeys(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	match := sampleTask("orch_match", func(x *task.Task) { x.Title = "compiler work" })
	other := sampleTask("orch_other", nil)
	f.seed(t, match, other)
	m := f.model(t, Options{})
	applySearch(t, m, "compiler")
	selected := m.SelectedID()

	typeText(t, m, "/")
	typeText(t, m, "qjmn")
	press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.searchOpen || m.searchQuery != "compiler" {
		t.Errorf("cancel changed the applied filter or left editing open: open=%v query=%q", m.searchOpen, m.searchQuery)
	}
	if got := m.SelectedID(); got != selected {
		t.Errorf("typing board keys into search changed selection to %q, want %q", got, selected)
	}
	if got := cardIDs(m.board.Cards()); len(got) != 1 || got[0] != match.ID {
		t.Errorf("cancel changed visible tasks: %v", got)
	}
}

func TestBoardSearchQueryRemainsVisibleOnDetailPage(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	card := sampleTask("orch_detail", func(x *task.Task) { x.Title = "compiler work" })
	f.seed(t, card)
	m := f.model(t, Options{})
	applySearch(t, m, "compiler")
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if !m.expanded || !strings.Contains(m.View(), "Filter: compiler") {
		t.Errorf("the detail page hid the active filter:\n%s", m.View())
	}
}

func TestBoardSearchDetailKeepsFilterInFixedFooterWhileBodyScrolls(t *testing.T) {
	query := strings.Repeat("filterterm", 20)
	f := newBoardFixture(t, fixturePlan{})
	card := sampleTask("orch_longfilter", func(x *task.Task) { x.Title = query })
	f.seed(t, card)
	if err := f.store.UpdateProject(context.Background(), f.project, func(p *task.Project) error {
		p.PrimaryAttention = "primary pane absent"
		return nil
	}); err != nil {
		t.Fatalf("record primary attention: %v", err)
	}
	m := f.model(t, Options{})
	press(t, m, tea.WindowSizeMsg{Width: 40, Height: 8})
	applySearch(t, m, query)
	if got := cardIDs(m.board.Cards()); len(got) != 1 || got[0] != card.ID || m.SelectedID() != card.ID || m.searchQuery != query {
		t.Fatalf("long search was not applied to its task: ids=%v selected=%q query=%q", got, m.SelectedID(), m.searchQuery)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded {
		t.Fatal("enter did not open the selected task's detail page")
	}

	assertFilterFooter := func(expectMarker bool) []string {
		t.Helper()
		lines := strings.Split(m.View(), "\n")
		if expectMarker && (len(lines) == 0 || !strings.HasPrefix(lines[0], "Primary not verified:")) {
			t.Errorf("the primary marker no longer leads the detail page:\n%s", m.View())
		}
		filterAt := -1
		for i, line := range lines {
			if strings.HasPrefix(line, "Filter: ") {
				filterAt = i
			}
			if width := visibleWidth(line); width > 40 {
				t.Errorf("detail line %q is %d columns wide, want at most 40", line, width)
			}
		}
		if filterAt != len(lines)-3 {
			t.Errorf("the filter is not on its own fixed row above the separator and help:\n%s", m.View())
		}
		if len(lines) > 0 && !strings.Contains(lines[len(lines)-1], "Esc back") {
			t.Errorf("the query hid the detail page's back key:\n%s", m.View())
		}
		if len(lines) > 8 {
			t.Errorf("filter status exceeded the detail-page height: got %d lines", len(lines))
		}
		return lines
	}
	assertFilterFooter(true)
	press(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.scroll == 0 {
		t.Fatal("the long detail page did not scroll")
	}
	assertFilterFooter(false)
}

func TestBoardSearchPromptAndNoMatchFitNarrowASCII(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_search", nil))
	m := f.model(t, Options{Theme: Theme{Profile: termenv.Ascii}})
	press(t, m, tea.WindowSizeMsg{Width: 24, Height: 8})
	typeText(t, m, "/")
	typeText(t, m, "no-match")

	checkWidth := func(view string) {
		t.Helper()
		for _, line := range strings.Split(view, "\n") {
			if width := visibleWidth(line); width > 24 {
				t.Errorf("line %q is %d columns wide, want at most 24", line, width)
			}
		}
		if strings.Contains(view, "\x1b") {
			t.Errorf("ASCII search output contains an escape sequence:\n%s", view)
		}
	}
	prompt := m.View()
	checkWidth(prompt)
	if !strings.Contains(prompt, "Search tasks: no-match_") {
		t.Errorf("the search query is not visible while editing:\n%s", prompt)
	}

	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	filtered := m.View()
	checkWidth(filtered)
	if !strings.Contains(filtered, "Filter: no-match") || !strings.Contains(filtered, "No tasks match") {
		t.Errorf("the applied query or empty-result message is missing:\n%s", filtered)
	}
}
