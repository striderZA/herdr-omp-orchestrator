package board

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"herdr-omp-orchestrator/internal/task"
)

var ticketTheme = Theme{Profile: termenv.Ascii}

func ticketCardOf(id, title string, stage task.TicketStage) TicketCard {
	column := task.ColumnBacklog
	if stage == task.TicketTodo {
		column = task.ColumnTodo
	}
	return TicketCard{
		Ticket: task.Ticket{ID: id, ProjectKey: "p", Title: title, Body: "body of " + id, Column: column, Revision: 3},
		Stage:  stage,
	}
}

func indexOfLine(lines []string, want string) int {
	for i, line := range lines {
		if strings.Contains(line, want) {
			return i
		}
	}
	return -1
}

func TestBoardWithoutTicketsRendersExactlyAsBefore(t *testing.T) {
	cards := []Card{
		{Task: sampleTask("orch_a", nil), Stage: task.Running},
		{Task: sampleTask("orch_b", func(x *task.Task) { x.Disposition = "integrated" }), Stage: task.Done},
	}
	for _, width := range []int{100, 80} {
		for _, d := range []Detail{{}, {SelectedID: "orch_a"}, {SelectedID: "orch_a", Expanded: true}} {
			plain := Render(NewBoard(cards), d, width, ticketTheme)
			withNone := Render(NewBoardWithTickets(cards, nil), d, width, ticketTheme)
			if plain != withNone {
				t.Fatalf("width %d: NewBoardWithTickets(cards, nil) must render exactly like NewBoard(cards)", width)
			}
			if strings.Contains(plain, "Backlog") || strings.Contains(plain, "Todo") {
				t.Fatalf("width %d: a board with no tickets must draw no ticket columns:\n%s", width, plain)
			}
		}
	}
	// Nothing is drawn before the five stage headings, and an empty board keeps
	// its own message.
	if wide := Render(NewBoard(cards), Detail{}, 100, ticketTheme); !strings.HasPrefix(wide, "Queued") {
		t.Fatalf("the wide board must open with the Queued heading, got %q", strings.SplitN(wide, "\n", 2)[0])
	}
	empty := Render(NewBoard(nil), Detail{}, 80, ticketTheme)
	if !strings.Contains(empty, "No tasks yet. Ask your primary to delegate a task") || strings.Contains(empty, "Backlog") {
		t.Fatalf("an empty board with no tickets keeps its message:\n%s", empty)
	}
	if empty != Render(NewBoardWithTickets(nil, nil), Detail{}, 80, ticketTheme) {
		t.Fatal("an empty board must render the same through NewBoardWithTickets")
	}
}

func TestTicketColumnsAppearOnlyWithATicketOrEnabledWrites(t *testing.T) {
	cards := []Card{{Task: sampleTask("orch_a", nil), Stage: task.Running}}
	// No ticket, writes enabled: the empty columns show where a new ticket goes.
	writes := Render(NewBoard(cards), Detail{TicketWrites: true}, 100, ticketTheme)
	lines := strings.Split(writes, "\n")
	backlog, queued := indexOfLine(lines, "Backlog"), indexOfLine(lines, "Queued")
	if backlog < 0 || indexOfLine(lines, "Todo") < 0 || queued < 0 || backlog >= queued {
		t.Fatalf("enabled ticket writes must draw empty Backlog and Todo columns before Queued:\n%s", writes)
	}
	// A ticket, writes disabled: the columns are drawn because there is a card.
	board := NewBoardWithTickets(cards, []TicketCard{ticketCardOf("tkt_a1", "first", task.TicketBacklog)})
	if out := Render(board, Detail{}, 100, ticketTheme); !strings.Contains(out, "[tkt_a1] first") {
		t.Fatalf("a backlog ticket must be drawn even when writes are disabled:\n%s", out)
	}
	// Tickets and no tasks at all: the ticket columns replace nothing and the
	// task message still says there are no tasks.
	only := Render(NewBoardWithTickets(nil, []TicketCard{ticketCardOf("tkt_a1", "first", task.TicketBacklog)}), Detail{}, 100, ticketTheme)
	if !strings.Contains(only, "[tkt_a1] first") || !strings.Contains(only, "No tasks yet") {
		t.Fatalf("a board with tickets and no tasks shows both:\n%s", only)
	}
}

func TestTicketColumnsRenderBeforeQueued(t *testing.T) {
	cards := []Card{{Task: sampleTask("orch_a", nil), Stage: task.Running}}
	tickets := []TicketCard{
		ticketCardOf("tkt_b1", "write the docs", task.TicketBacklog),
		ticketCardOf("tkt_t1", "ship the thing", task.TicketTodo),
	}
	board := NewBoardWithTickets(cards, tickets)
	for _, width := range []int{100, 80} {
		out := Render(board, Detail{}, width, ticketTheme)
		lines := strings.Split(out, "\n")
		backlog, todo, queued := indexOfLine(lines, "Backlog"), indexOfLine(lines, "Todo"), indexOfLine(lines, "Queued")
		if backlog < 0 || todo < 0 || queued < 0 || backlog >= queued || todo >= queued {
			t.Fatalf("width %d: Backlog (%d) and Todo (%d) must precede Queued (%d):\n%s", width, backlog, todo, queued, out)
		}
		if indexOfLine(lines, "[tkt_b1] write the docs") < 0 || indexOfLine(lines, "[tkt_t1] ship the thing") < 0 {
			t.Fatalf("width %d: both tickets must be drawn:\n%s", width, out)
		}
		if got := len(board.TicketColumn(task.TicketBacklog)); got != 1 {
			t.Fatalf("Backlog column holds %d tickets, want 1", got)
		}
		if got := len(board.TicketColumn(task.TicketTodo)); got != 1 {
			t.Fatalf("Todo column holds %d tickets, want 1", got)
		}
	}
	// In the narrow layout each ticket sits under its own heading.
	narrow := strings.Split(Render(board, Detail{}, 80, ticketTheme), "\n")
	if at := indexOfLine(narrow, "Backlog (1)"); at < 0 || !strings.Contains(narrow[at+1], "[tkt_b1]") {
		t.Fatalf("the narrow Backlog section must list its ticket right below the heading:\n%s", strings.Join(narrow, "\n"))
	}
	if at := indexOfLine(narrow, "Todo (1)"); at < 0 || !strings.Contains(narrow[at+1], "[tkt_t1]") {
		t.Fatalf("the narrow Todo section must list its ticket right below the heading:\n%s", strings.Join(narrow, "\n"))
	}
}

func TestTicketCardStaysOneBoundedLine(t *testing.T) {
	tickets := []TicketCard{
		ticketCardOf("tkt_cjk", "日本語\nsecond", task.TicketBacklog),
		ticketCardOf("tkt_long", strings.Repeat("l", 160), task.TicketTodo),
	}
	board := NewBoardWithTickets(nil, tickets)
	for _, width := range []int{100, 80, 40, 24, 12} {
		// Selected or not, no line is wider than the terminal and the text is valid.
		for _, selected := range []string{"", "tkt_cjk"} {
			out := Render(board, Detail{SelectedID: selected}, width, ticketTheme)
			if !utf8.ValidString(out) {
				t.Fatalf("width %d (selected %q): the board is not valid UTF-8", width, selected)
			}
			for _, line := range strings.Split(out, "\n") {
				if got := runeWidth(line); got > width {
					t.Errorf("width %d (selected %q): line %q is %d columns wide", width, selected, line, got)
				}
				// The detail area wraps a long title by design (as it does a task's
				// brief), so the newline check is about the cards: unselected.
				if selected == "" && strings.TrimSpace(line) == "second" {
					t.Errorf("width %d: the title's newline became its own card line", width)
				}
			}
		}
	}
	// Where the cell holds it, every rune of the title is intact on one line.
	out := Render(board, Detail{}, 100, ticketTheme)
	if !strings.Contains(out, "[tkt_cjk] 日本語 second") {
		t.Fatalf("the wide title must be one intact line:\n%s", out)
	}
}

func TestTicketDetailShowsFactsAndLinkedTasks(t *testing.T) {
	card := ticketCardOf("tkt_d1", "document the board", task.TicketTodo)
	card.Ticket.Body = "A long body.\nSecond line of the body."
	card.Tasks = []task.Task{sampleTask("orch_l1", nil), sampleTask("orch_l2", nil)}
	board := NewBoardWithTickets(nil, []TicketCard{card})

	expanded := Render(board, Detail{SelectedID: "tkt_d1", Expanded: true}, 100, ticketTheme)
	for _, fact := range []string{"tkt_d1", "document the board", "A long body. Second line of the body.", "todo", "revision 3", "orch_l1", "orch_l2"} {
		if !strings.Contains(expanded, fact) {
			t.Errorf("the expanded ticket detail must show %q:\n%s", fact, expanded)
		}
	}
	if strings.Contains(expanded, "branch") || strings.Contains(expanded, "worker") {
		t.Errorf("a ticket has no branch or worker, so its detail must not show either:\n%s", expanded)
	}
	collapsed := Render(board, Detail{SelectedID: "tkt_d1"}, 100, ticketTheme)
	if !strings.Contains(collapsed, "document the board") || !strings.Contains(collapsed, "tkt_d1") {
		t.Errorf("the collapsed ticket detail must show the id and title:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "f focus") {
		t.Errorf("a ticket has no pane to focus, so the help line must not offer it:\n%s", collapsed)
	}

	page := RenderPage(board, Detail{SelectedID: "tkt_d1"}, 100, 40, ticketTheme)
	for _, fact := range []string{"tkt_d1", "document the board", "Second line of the body.", "todo", "orch_l1", "orch_l2"} {
		if !strings.Contains(page, fact) {
			t.Errorf("the ticket page must show %q:\n%s", fact, page)
		}
	}
	if strings.Contains(page, "f focus") {
		t.Errorf("the ticket page must not offer focus:\n%s", page)
	}
	if gone := RenderPage(board, Detail{SelectedID: "tkt_gone"}, 100, 40, ticketTheme); !strings.Contains(gone, "no longer on this board") {
		t.Errorf("a selection that is neither task nor ticket reads as gone:\n%s", gone)
	}

	bare := Render(NewBoardWithTickets(nil, []TicketCard{ticketCardOf("tkt_d2", "no tasks", task.TicketBacklog)}),
		Detail{SelectedID: "tkt_d2", Expanded: true}, 100, ticketTheme)
	if !strings.Contains(bare, "none") {
		t.Errorf("a ticket with no linked tasks says so:\n%s", bare)
	}
}

func TestTaskDetailNamesItsTicket(t *testing.T) {
	linked := sampleTask("orch_k1", func(x *task.Task) { x.TicketID = "tkt_k1" })
	card := Card{Task: linked, Stage: task.Running, Ticket: TicketRef{ID: "tkt_k1", Title: "Add search", Stage: task.TicketInProgress}}
	board := NewBoard([]Card{card})
	for _, d := range []Detail{{SelectedID: "orch_k1"}, {SelectedID: "orch_k1", Expanded: true}} {
		out := Render(board, d, 120, ticketTheme)
		if !strings.Contains(out, "Ticket: tkt_k1 Add search (in progress)") {
			t.Errorf("a linked task's detail must name its ticket and stage (expanded=%v):\n%s", d.Expanded, out)
		}
	}
	if page := RenderPage(board, Detail{SelectedID: "orch_k1"}, 120, 40, ticketTheme); !strings.Contains(page, "Ticket: tkt_k1 Add search (in progress)") {
		t.Errorf("the task page must name its ticket:\n%s", page)
	}

	// A task that names a ticket this board could not read says so rather than hiding the link.
	orphan := Card{Task: sampleTask("orch_k2", func(x *task.Task) { x.TicketID = "tkt_missing" }), Stage: task.Running}
	if out := Render(NewBoard([]Card{orphan}), Detail{SelectedID: "orch_k2"}, 120, ticketTheme); !strings.Contains(out, "Ticket: tkt_missing (not found)") {
		t.Errorf("a link to an unreadable ticket must still be shown:\n%s", out)
	}
	// An unlinked task shows no Ticket line at all.
	plain := Card{Task: sampleTask("orch_k3", nil), Stage: task.Running}
	if out := Render(NewBoard([]Card{plain}), Detail{SelectedID: "orch_k3", Expanded: true}, 120, ticketTheme); strings.Contains(out, "Ticket:") {
		t.Errorf("an unlinked task must not show a Ticket line:\n%s", out)
	}
}

func TestProjectTicketsOmitsTicketsPastTodo(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	tickets := map[string]task.Column{
		"tkt_a_backlog":  task.ColumnBacklog,
		"tkt_b_todo":     task.ColumnTodo,
		"tkt_c_progress": task.ColumnTodo,
		"tkt_d_done":     task.ColumnTodo,
	}
	for _, id := range []string{"tkt_a_backlog", "tkt_b_todo", "tkt_c_progress", "tkt_d_done"} {
		if err := f.store.CreateTicket(ctx, task.Ticket{ID: id, ProjectKey: f.project, Title: "title " + id, Column: tickets[id]}); err != nil {
			t.Fatalf("CreateTicket %s: %v", id, err)
		}
	}
	f.seed(
		t,
		acceptedPlanner("orch_plan_c", "tkt_c_progress"),
		acceptedPlanner("orch_plan_d", "tkt_d_done"),
		sampleTask("orch_open", func(x *task.Task) { x.TicketID = "tkt_c_progress" }),
		sampleTask("orch_closed", func(x *task.Task) {
			x.TicketID = "tkt_d_done"
			x.Outcome, x.Disposition = task.OutcomeIntegrated, task.OutcomeIntegrated
			x.Report = &task.Report{Kind: "result", Summary: "done"}
		}),
	)
	done, err := f.store.Ticket(ctx, "tkt_d_done")
	if err != nil {
		t.Fatalf("read the ticket: %v", err)
	}
	if _, err := f.store.DecideTicket(ctx, done.ID, done.Revision, task.OutcomeIntegrated, "shipped", ""); err != nil {
		t.Fatalf("decide: %v", err)
	}

	cards, err := ProjectTickets(ctx, f.store, f.project)
	if err != nil {
		t.Fatalf("ProjectTickets: %v", err)
	}
	if len(cards) != 2 || cards[0].ID() != "tkt_a_backlog" || cards[0].Stage != task.TicketBacklog ||
		cards[1].ID() != "tkt_b_todo" || cards[1].Stage != task.TicketTodo {
		t.Fatalf("only the backlog and todo tickets are cards, oldest first: %+v", cards)
	}

	// Their tasks are still cards, and each names its ticket and the ticket's stage.
	tasks, err := ProjectCards(ctx, f.store, f.project)
	if err != nil {
		t.Fatalf("ProjectCards: %v", err)
	}
	refs := map[string]TicketRef{}
	for _, card := range tasks {
		refs[card.ID()] = card.Ticket
	}
	if len(tasks) != 4 {
		t.Fatalf("every task, the accepted planners included, must still be a card: %+v", tasks)
	}
	if got := refs["orch_open"]; got.ID != "tkt_c_progress" || got.Stage != task.TicketInProgress || got.Title != "title tkt_c_progress" {
		t.Errorf("the open task's ticket reference = %+v, want tkt_c_progress in progress", got)
	}
	if got := refs["orch_closed"]; got.ID != "tkt_d_done" || got.Stage != task.TicketDone {
		t.Errorf("the closed task's ticket reference = %+v, want tkt_d_done done", got)
	}

	if other, err := ProjectTickets(ctx, f.store, "C:/elsewhere"); err != nil || len(other) != 0 {
		t.Errorf("another project has no ticket cards: %+v, %v", other, err)
	}
	if _, err := ProjectTickets(ctx, nil, f.project); err == nil {
		t.Error("ProjectTickets without a store must fail, not panic")
	}
}

func TestAnUnplacedTicketIsShownNotDropped(t *testing.T) {
	stranded := ticketCardOf("tkt_s1", "stranded", task.TicketInProgress)
	board := NewBoardWithTickets(nil, []TicketCard{stranded})
	out := Render(board, Detail{}, 100, ticketTheme)
	if !strings.Contains(out, "Not placed by this build:") || !strings.Contains(out, "[tkt_s1] stranded") {
		t.Fatalf("a ticket this build cannot place must still be shown:\n%s", out)
	}
	if strings.Contains(out, "Backlog") {
		t.Fatalf("an unplaced ticket alone must not draw the ticket columns:\n%s", out)
	}
	if got := len(board.TicketColumn(task.TicketBacklog)) + len(board.TicketColumn(task.TicketTodo)); got != 0 {
		t.Fatalf("an unplaced ticket is in no column, got %d", got)
	}
	if _, ok := board.FindTicket("tkt_s1"); !ok {
		t.Fatal("an unplaced ticket can still be found by id")
	}
}

// addTicket records a ticket of the fixture's project straight into the store.
func (f *boardFixture) addTicket(t *testing.T, id string, column task.Column) {
	t.Helper()
	ticket := task.Ticket{ID: id, ProjectKey: f.project, Title: "title " + id, Body: "body " + id, Column: column}
	if err := f.store.CreateTicket(context.Background(), ticket); err != nil {
		t.Fatalf("CreateTicket %s: %v", id, err)
	}
}

// acceptedPlanner is a ticket's planner whose plan the user accepted. Linked
// implementation work needs one (spec §5), and it is closed, so it never keeps
// its ticket open.
func acceptedPlanner(id, ticketID string) task.Task {
	return sampleTask(id, func(x *task.Task) {
		x.TicketID, x.Role = ticketID, task.TaskRolePlan
		x.Outcome, x.Disposition = task.OutcomeIntegrated, "accepted the plan"
		x.Report = &task.Report{Kind: "result", Summary: "the plan"}
	})
}

func TestTabWalksTicketColumnsThenTaskColumns(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.addTicket(t, "tkt_b_todo", task.ColumnTodo)
	queued := sampleTask("orch_q1", func(x *task.Task) { x.Phase = task.Intent })
	running := sampleTask("orch_r1", nil)
	f.seed(t, queued, running)
	m := f.model(t, Options{})

	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("the board opens on the first card in display order, got %q", got)
	}
	// Backlog, Todo, then the five task columns (the last three are empty), and round.
	forward := []string{"tkt_b_todo", queued.ID, running.ID, "", "", "", "tkt_a_backlog"}
	for i, want := range forward {
		press(t, m, tea.KeyMsg{Type: tea.KeyTab})
		if got := m.SelectedID(); got != want {
			t.Fatalf("tab %d selected %q, want %q", i+1, got, want)
		}
	}
	backward := []string{"", "", "", running.ID, queued.ID, "tkt_b_todo", "tkt_a_backlog"}
	for i, want := range backward {
		press(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
		if got := m.SelectedID(); got != want {
			t.Fatalf("shift+tab %d selected %q, want %q", i+1, got, want)
		}
	}

	// The arrows walk the cards in the same order and stop at both ends.
	press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("up at the first card moved to %q", got)
	}
	for _, want := range []string{"tkt_b_todo", queued.ID, running.ID, running.ID} {
		press(t, m, tea.KeyMsg{Type: tea.KeyDown})
		if got := m.SelectedID(); got != want {
			t.Fatalf("down selected %q, want %q", got, want)
		}
	}
	for _, want := range []string{queued.ID, "tkt_b_todo", "tkt_a_backlog"} {
		press(t, m, tea.KeyMsg{Type: tea.KeyUp})
		if got := m.SelectedID(); got != want {
			t.Fatalf("up selected %q, want %q", got, want)
		}
	}
	// After the arrows the reader's column follows the card, so tab continues from it.
	press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.SelectedID(); got != "tkt_b_todo" {
		t.Fatalf("tab after the arrows selected %q, want the next column's card", got)
	}
}

func TestBoardNavigationWithoutTicketsIsUnchanged(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	only := sampleTask("orch_only", nil)
	f.seed(t, only)
	m := f.model(t, Options{})
	if got := m.SelectedID(); got != only.ID {
		t.Fatalf("the board opens on the only card, got %q", got)
	}
	// With no tickets the cursor space is exactly the five task columns: five tabs
	// come back to the same card, and no ticket heading is ever drawn.
	for i := 1; i <= len(Columns); i++ {
		press(t, m, tea.KeyMsg{Type: tea.KeyTab})
		want := ""
		if i == len(Columns) {
			want = only.ID
		}
		if got := m.SelectedID(); got != want {
			t.Fatalf("tab %d selected %q, want %q", i, got, want)
		}
	}
	if view := m.View(); strings.Contains(view, "Backlog") || strings.Contains(view, "Todo") {
		t.Fatalf("a board with no tickets draws no ticket columns:\n%s", view)
	}
}

func TestTicketSelectionSurvivesRefresh(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	queued := sampleTask("orch_q1", func(x *task.Task) { x.Phase = task.Intent })
	f.seed(t, queued)
	m := f.model(t, Options{})
	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("selected %q, want the backlog ticket", got)
	}

	// The ticket moves to todo between two refreshes: it is still the selected card.
	ticket, err := f.store.Ticket(ctx, "tkt_a_backlog")
	if err != nil {
		t.Fatalf("read the ticket: %v", err)
	}
	if _, err := f.store.UpdateTicket(ctx, ticket.ID, ticket.Revision, func(next *task.Ticket) error {
		next.Column = task.ColumnTodo
		return nil
	}); err != nil {
		t.Fatalf("move the ticket: %v", err)
	}
	press(t, m, keyPress("r"))
	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("the selected ticket changed across a refresh: %q", got)
	}
	if len(m.board.TicketColumn(task.TicketTodo)) != 1 || len(m.board.TicketColumn(task.TicketBacklog)) != 0 {
		t.Fatalf("the refresh must have re-read the ticket into the todo column")
	}
	// The reader's column followed the ticket: tab leaves the todo column for the
	// first task column, not for todo again.
	press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.SelectedID(); got != queued.ID {
		t.Fatalf("tab after the ticket moved to todo selected %q, want the queued task", got)
	}
}

func TestASelectedTicketThatStartsWorkFallsBackToAnotherCard(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.addTicket(t, "tkt_b_todo", task.ColumnTodo)
	f.seed(t)
	m := f.model(t, Options{})
	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("selected %q, want the backlog ticket", got)
	}
	// The selected ticket moves to todo and records its planner, the way a
	// confirmed move does: it is planning now, so it is no longer a card.
	backlog, err := f.store.Ticket(ctx, "tkt_a_backlog")
	if err != nil {
		t.Fatalf("read the selected ticket: %v", err)
	}
	planner := sampleTask("orch_l1", func(x *task.Task) {
		x.ProjectKey, x.TicketID, x.Role, x.Phase = f.project, backlog.ID, task.TaskRolePlan, task.Intent
	})
	if _, _, err := f.store.MoveTicketToTodoWithPlannerTask(ctx, backlog.ID, backlog.Revision, planner); err != nil {
		t.Fatalf("move the ticket and record its planner: %v", err)
	}
	press(t, m, keyPress("r"))
	got := m.SelectedID()
	if got == "tkt_a_backlog" || got == "" {
		t.Fatalf("a ticket that started work cannot stay selected as a card, got %q", got)
	}
	if _, ok := m.board.FindTicket(got); !ok {
		if _, ok := m.board.Find(got); !ok {
			t.Fatalf("the fallback selection %q is not a card on the board", got)
		}
	}
}

func TestFocusOnATicketRefusesBeforeHerdr(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t)
	m := f.model(t, Options{})
	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("selected %q, want the ticket", got)
	}
	if cmd := press(t, m, keyPress("f")); cmd != nil {
		t.Fatal("focusing a ticket must not start a command")
	}
	if !m.noticeErr || !strings.Contains(m.notice, "Tickets have no pane to focus") {
		t.Fatalf("notice = %q (error=%v), want the ticket refusal", m.notice, m.noticeErr)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("focusing a ticket called Herdr: %v", calls)
	}
}

func TestEnterOpensATicketPageThatScrolls(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	ticket := task.Ticket{
		ID: "tkt_long", ProjectKey: f.project, Title: "long one", Column: task.ColumnBacklog,
		Body: strings.Repeat("word ", 400),
	}
	if err := f.store.CreateTicket(context.Background(), ticket); err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	f.seed(t)
	m := f.model(t, Options{})
	press(t, m, tea.WindowSizeMsg{Width: 60, Height: 8})
	press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded {
		t.Fatal("enter did not open the ticket page")
	}
	first := m.View()
	if !strings.Contains(first, "Esc back, q close") && !strings.Contains(first, "up/down scroll") {
		t.Fatalf("the ticket page must offer back and close, not focus:\n%s", first)
	}
	if strings.Contains(first, "f focus") {
		t.Fatalf("the ticket page must not offer focus:\n%s", first)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.scroll == 0 {
		t.Fatal("the ticket page did not scroll: scrollBy must know ticket pages")
	}
	if second := m.View(); second == first {
		t.Fatal("scrolling a ticket page did not change what is shown")
	}
}

// fakeTicketOps records what the board asked to write. It writes nothing itself,
// so a test can tell a call the board made from a change it did not.
type fakeTicketOps struct {
	creates   []fakeCreate
	moves     []fakeMove
	createErr error
	moveErr   error
	onMove    func()
}

type fakeCreate struct{ project, title string }

type fakeMove struct{ project, id, digest string }

func (f *fakeTicketOps) CreateTicket(_ context.Context, projectKey, title string) error {
	f.creates = append(f.creates, fakeCreate{projectKey, title})
	return f.createErr
}

func (f *fakeTicketOps) MoveTicketToTodo(_ context.Context, projectKey, ticketID, digest string) error {
	f.moves = append(f.moves, fakeMove{projectKey, ticketID, digest})
	if f.onMove != nil {
		f.onMove()
	}
	return f.moveErr
}

// typeText sends text to the board as the runes one keystroke would carry.
func typeText(t *testing.T, m *Model, text string) tea.Cmd {
	t.Helper()
	return press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

func TestNewTicketPromptCapturesKeysAndSubmitsOnce(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t, sampleTask("orch_q1", func(x *task.Task) { x.Phase = task.Intent }))
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})
	selected := m.SelectedID()

	if cmd := typeText(t, m, "n"); cmd != nil {
		t.Fatal("opening the prompt must not start a command")
	}
	// Keys that normally quit or navigate are text here; none of them quits (a
	// quit would be a non-nil command) and none moves the selection.
	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")},
		tea.KeyMsg{Type: tea.KeyTab},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")},
		tea.KeyMsg{Type: tea.KeyBackspace},
		tea.KeyMsg{Type: tea.KeyDown},
		tea.KeyMsg{Type: tea.KeyUp},
		tea.KeyMsg{Type: tea.KeyShiftTab},
		tea.KeyMsg{Type: tea.KeyPgDown},
	} {
		if cmd := press(t, m, msg); cmd != nil {
			t.Fatalf("key %v inside the prompt started a command (a quit or a write)", msg)
		}
	}
	if got := m.SelectedID(); got != selected {
		t.Fatalf("typing into the prompt moved the selection from %q to %q", selected, got)
	}
	// Tab is typed like q and j: the title holds a tab character, which the prompt
	// shows as ^I so no raw tab ever reaches the terminal.
	view := m.View()
	if !strings.Contains(view, "New ticket title: q^Ij") {
		t.Fatalf("the prompt must show what was typed (tab as ^I, backspace deleting):\n%s", view)
	}
	if strings.Contains(view, "\t") {
		t.Fatalf("a raw tab must never reach the terminal:\n%q", view)
	}
	if len(ops.creates) != 0 {
		t.Fatalf("nothing is written before enter: %+v", ops.creates)
	}

	cmd := press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must start the write")
	}
	press(t, m, cmd())
	if len(ops.creates) != 1 || ops.creates[0].title != "q\tj" || ops.creates[0].project != f.project {
		t.Fatalf("enter must make exactly one CreateTicket call with the exact title for this project: %+v", ops.creates)
	}
	// The prompt is closed: another enter opens the details and starts nothing. If
	// it did return a command it is run here, so a second submit could not hide.
	if again := press(t, m, tea.KeyMsg{Type: tea.KeyEnter}); again != nil {
		press(t, m, again())
	}
	if len(ops.creates) != 1 {
		t.Fatalf("a second enter wrote again: %+v", ops.creates)
	}
	if m.noticeErr || !strings.Contains(m.notice, "Created") {
		t.Fatalf("the board must say the ticket was created: %q (error=%v)", m.notice, m.noticeErr)
	}
}

func TestEscCancelsThePromptWithoutWriting(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_q1", nil))
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})

	typeText(t, m, "n")
	typeText(t, m, "abc")
	if cmd := press(t, m, tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Fatal("esc inside the prompt must cancel it, not quit")
	}
	if len(ops.creates) != 0 {
		t.Fatalf("a cancelled prompt wrote %+v", ops.creates)
	}
	if view := m.View(); strings.Contains(view, "New ticket title") {
		t.Fatalf("a cancelled prompt must disappear:\n%s", view)
	}
	// The prompt is gone and its text with it: q quits again, and a fresh prompt is empty.
	if cmd := typeText(t, m, "q"); cmd == nil {
		t.Fatal("after cancelling, q must quit the board again")
	}
	typeText(t, m, "n")
	if view := m.View(); !strings.Contains(view, "New ticket title: _") {
		t.Fatalf("a new prompt must start empty:\n%s", view)
	}
	// Ctrl+C also cancels the prompt rather than quitting out from under it.
	if cmd := press(t, m, tea.KeyMsg{Type: tea.KeyCtrlC}); cmd != nil {
		t.Fatal("ctrl+c inside the prompt must cancel it, not quit")
	}
}

func TestBlankTitleIsNotSubmitted(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_q1", nil))
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})

	typeText(t, m, "n")
	if cmd := press(t, m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("an empty title must not start a write")
	}
	for i := 0; i < 3; i++ {
		press(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	}
	if cmd := press(t, m, tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("a title of spaces must not start a write")
	}
	if len(ops.creates) != 0 || !m.noticeErr {
		t.Fatalf("a blank title writes nothing and says why: creates=%+v notice=%q err=%v", ops.creates, m.notice, m.noticeErr)
	}
	if view := m.View(); !strings.Contains(view, "New ticket title") {
		t.Fatalf("the prompt stays open after a blank title:\n%s", view)
	}
	typeText(t, m, "ok")
	cmd := press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a real title must start the write")
	}
	press(t, m, cmd())
	if len(ops.creates) != 1 || ops.creates[0].title != "ok" {
		t.Fatalf("the title is trimmed: %+v", ops.creates)
	}
}

func TestMoveKeyActsOnlyOnASelectedBacklogTicket(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.addTicket(t, "tkt_b_todo", task.ColumnTodo)
	queued := sampleTask("orch_q1", func(x *task.Task) { x.Phase = task.Intent })
	f.seed(t, queued)
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})

	// A todo ticket is not movable, and neither is a task.
	press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.SelectedID(); got != "tkt_b_todo" {
		t.Fatalf("selected %q, want the todo ticket", got)
	}
	if cmd := typeText(t, m, "m"); cmd != nil || !m.noticeErr || len(ops.moves) != 0 {
		t.Fatalf("m on a todo ticket must write nothing and say so: cmd=%v notice=%q err=%v moves=%+v", cmd != nil, m.notice, m.noticeErr, ops.moves)
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.SelectedID(); got != queued.ID {
		t.Fatalf("selected %q, want the task", got)
	}
	m.notice, m.noticeErr = "", false
	if cmd := typeText(t, m, "m"); cmd != nil || !m.noticeErr || len(ops.moves) != 0 {
		t.Fatalf("m on a task must write nothing and say so: cmd=%v notice=%q err=%v moves=%+v", cmd != nil, m.notice, m.noticeErr, ops.moves)
	}

	// A backlog ticket: exactly one call, with the digest of the card as displayed.
	press(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	press(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("selected %q, want the backlog ticket", got)
	}
	stored, err := f.store.Ticket(ctx, "tkt_a_backlog")
	if err != nil {
		t.Fatalf("read the ticket: %v", err)
	}
	cmd := typeText(t, m, "m")
	if cmd == nil {
		t.Fatal("m on a backlog ticket must start the write")
	}
	press(t, m, cmd())
	want := fakeMove{project: f.project, id: "tkt_a_backlog", digest: stored.Digest()}
	if len(ops.moves) != 1 || ops.moves[0] != want {
		t.Fatalf("moves = %+v, want exactly %+v (the displayed card's digest)", ops.moves, want)
	}
	if !strings.Contains(strings.ToLower(m.notice), "planner") {
		t.Fatalf("successful move notice must say that a planner task was recorded: %q", m.notice)
	}
}

func TestTicketMoveHelpStatesItStartsPlanning(t *testing.T) {
	card := ticketCardOf("tkt_a", "move me", task.TicketBacklog)
	help := ticketHelpLine(card, Detail{TicketWrites: true})
	if !strings.Contains(help, "m move to todo + start planner") {
		t.Fatalf("ticket move help must state its consequence, got %q", help)
	}
}

func TestMoveRefusalBecomesANoticeAndRefreshes(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t)
	ops := &fakeTicketOps{moveErr: errors.New("ticket_changed: the ticket changed after its preview")}
	// Another writer moves the ticket first, which is why the digest is stale.
	ops.onMove = func() {
		ticket, err := f.store.Ticket(ctx, "tkt_a_backlog")
		if err != nil {
			t.Errorf("read the ticket: %v", err)
			return
		}
		if _, err := f.store.UpdateTicket(ctx, ticket.ID, ticket.Revision, func(next *task.Ticket) error {
			next.Column = task.ColumnTodo
			return nil
		}); err != nil {
			t.Errorf("move the ticket elsewhere: %v", err)
		}
	}
	m := f.model(t, Options{Tickets: ops})
	if len(m.board.TicketColumn(task.TicketBacklog)) != 1 {
		t.Fatal("the board starts with the ticket in the backlog")
	}
	cmd := typeText(t, m, "m")
	if cmd == nil {
		t.Fatal("m on a backlog ticket must start the write")
	}
	press(t, m, cmd())
	if !m.noticeErr || !strings.Contains(m.notice, "changed after its preview") {
		t.Fatalf("a refusal must become an error notice: %q (error=%v)", m.notice, m.noticeErr)
	}
	if len(m.board.TicketColumn(task.TicketTodo)) != 1 || len(m.board.TicketColumn(task.TicketBacklog)) != 0 {
		t.Fatal("a refusal must be followed by a refresh that re-reads the ticket")
	}
}

// A move the coordinator recorded is never reported as a failed move, even when
// its planner did not start: the notice says the ticket moved and why its planner
// has not started, and the board re-reads where the ticket now is.
func TestAMoveWhosePlannerDidNotStartStillSaysItMoved(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t)
	ops := &fakeTicketOps{moveErr: &MovedTicketError{Err: errors.New("create the worktree: herdr: not_git_worktree")}}
	ops.onMove = func() {
		// The coordinator recorded the move before its planner failed to start.
		ticket, err := f.store.Ticket(ctx, "tkt_a_backlog")
		if err != nil {
			t.Errorf("read the ticket: %v", err)
			return
		}
		if _, err := f.store.UpdateTicket(ctx, ticket.ID, ticket.Revision, func(next *task.Ticket) error {
			next.Column = task.ColumnTodo
			return nil
		}); err != nil {
			t.Errorf("record the move: %v", err)
		}
	}
	m := f.model(t, Options{Tickets: ops})
	cmd := typeText(t, m, "m")
	if cmd == nil {
		t.Fatal("m on a backlog ticket must start the write")
	}
	press(t, m, cmd())
	if !strings.HasPrefix(m.notice, "Moved tkt_a_backlog to todo") || strings.Contains(m.notice, "Cannot move") ||
		!strings.Contains(m.notice, "planner did not start") || !strings.Contains(m.notice, "not_git_worktree") {
		t.Fatalf("a recorded move must say it moved and why its planner did not start: %q", m.notice)
	}
	if !m.noticeErr {
		t.Fatalf("a planner that did not start must still stand out: %q", m.notice)
	}
	if len(m.board.TicketColumn(task.TicketTodo)) != 1 || len(m.board.TicketColumn(task.TicketBacklog)) != 0 {
		t.Fatal("the board must re-read the moved ticket")
	}
}
func TestAMoveWhoseRecoverySignalFailedStillSaysItMoved(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t)
	ops := &fakeTicketOps{moveErr: &MovedTicketSignalError{Err: errors.New("recovery signal unavailable")}}
	ops.onMove = func() {
		ticket, err := f.store.Ticket(ctx, "tkt_a_backlog")
		if err != nil {
			t.Errorf("read the ticket: %v", err)
			return
		}
		if _, err := f.store.UpdateTicket(ctx, ticket.ID, ticket.Revision, func(next *task.Ticket) error {
			next.Column = task.ColumnTodo
			return nil
		}); err != nil {
			t.Errorf("record the move: %v", err)
		}
	}
	m := f.model(t, Options{Tickets: ops})
	cmd := typeText(t, m, "m")
	if cmd == nil {
		t.Fatal("m on a backlog ticket must start the write")
	}
	press(t, m, cmd())
	if !strings.HasPrefix(m.notice, "Moved tkt_a_backlog to todo") || strings.Contains(m.notice, "Cannot move") ||
		!strings.Contains(m.notice, "recovery signal unavailable") {
		t.Fatalf("a committed move with a failed recovery signal must say it moved and why: %q", m.notice)
	}
	if !m.noticeErr {
		t.Fatalf("a failed recovery signal must stand out: %q", m.notice)
	}
	if len(m.board.TicketColumn(task.TicketTodo)) != 1 || len(m.board.TicketColumn(task.TicketBacklog)) != 0 {
		t.Fatal("the board must re-read the moved ticket")
	}
}

func TestBoardWithoutTicketOpsStaysReadOnly(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t, sampleTask("orch_q1", func(x *task.Task) { x.Phase = task.Intent }))
	ticketsBefore, err := f.store.Tickets(ctx, f.project)
	if err != nil {
		t.Fatalf("read the tickets: %v", err)
	}
	tasksBefore := f.snapshot()
	m := f.model(t, Options{})

	if m.detail().TicketWrites {
		t.Fatal("a board without TicketOps must not claim ticket writes")
	}
	for _, key := range []string{"n", "m"} {
		m.notice, m.noticeErr = "", false
		if cmd := typeText(t, m, key); cmd != nil {
			t.Fatalf("%q without TicketOps must not start a command", key)
		}
		if !m.noticeErr || m.notice != "Ticket writes are unavailable in this session" {
			t.Fatalf("%q notice = %q (error=%v), want the unavailable notice", key, m.notice, m.noticeErr)
		}
	}
	// n did not open a prompt: q still quits.
	if cmd := typeText(t, m, "q"); cmd == nil {
		t.Fatal("n without TicketOps must not have opened a prompt")
	}
	ticketsAfter, err := f.store.Tickets(ctx, f.project)
	if err != nil {
		t.Fatalf("read the tickets: %v", err)
	}
	if len(ticketsAfter) != len(ticketsBefore) || ticketsAfter[0] != ticketsBefore[0] {
		t.Fatalf("a read-only board changed the tickets: %+v -> %+v", ticketsBefore, ticketsAfter)
	}
	if after := f.snapshot(); len(after) != len(tasksBefore) {
		t.Fatalf("a read-only board changed the tasks: %+v -> %+v", tasksBefore, after)
	}
}

func TestEnabledTicketWritesShowEmptyColumnsAndTheirKeys(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_q1", nil))
	if view := f.model(t, Options{}).View(); strings.Contains(view, "Backlog") || strings.Contains(view, "n new ticket") {
		t.Fatalf("a read-only board with no tickets shows no ticket UI:\n%s", view)
	}
	view := f.model(t, Options{Tickets: &fakeTicketOps{}}).View()
	for _, want := range []string{"Backlog", "Todo", "n new ticket"} {
		if !strings.Contains(view, want) {
			t.Fatalf("a board with ticket writes must show %q:\n%s", want, view)
		}
	}
}

func TestTicketTitlePromptIsBounded(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_q1", nil))
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})
	typeText(t, m, "n")
	typeText(t, m, strings.Repeat("a", 170))
	cmd := press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a long title must still submit")
	}
	press(t, m, cmd())
	if len(ops.creates) != 1 || len(ops.creates[0].title) != 160 {
		t.Fatalf("the title is bounded at 160 bytes, the coordinator's limit: %+v", ops.creates)
	}

	typeText(t, m, "n")
	typeText(t, m, strings.Repeat("日", 100))
	cmd = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("a wide title must still submit")
	}
	press(t, m, cmd())
	title := ops.creates[1].title
	if !utf8.ValidString(title) || len(title) > 160 || len([]rune(title)) != 53 {
		t.Fatalf("a multi-byte title must be cut on a rune boundary within 160 bytes, got %d bytes / %d runes", len(title), len([]rune(title)))
	}

	// Pasted control characters never reach the title.
	typeText(t, m, "n")
	typeText(t, m, "one\ttwo\nthree\x1b[31mfour")
	cmd = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	press(t, m, cmd())
	// A pasted tab is the same typed tab; a newline cannot be part of a one-line
	// title, so it becomes a space; other control characters are dropped.
	if got := ops.creates[2].title; got != "one\ttwo three[31mfour" {
		t.Fatalf("a tab is kept, a newline becomes a space and other control characters are dropped, got %q", got)
	}
}

// A ticket's title and body are written by the primary agent, and the human who
// reads the board presses m on what it shows. An escape sequence in that text
// (cursor movement, a window retitle, hidden or reversed text) must not reach the
// terminal on any widget, width or page. Task text goes through the same path and
// gets the same protection.
func TestControlCharactersInStoredTextNeverReachTheScreen(t *testing.T) {
	const evil = "ev\x1b[2K\x1b]0;pwned\x07il\x1b[1;1H\u0085 \u202eesrever\u2069 end\x7f\r"
	ticket := ticketCardOf("tkt_evil", evil, task.TicketBacklog)
	ticket.Ticket.Body = "body " + evil
	worker := sampleTask("orch_evil", func(x *task.Task) {
		x.Title, x.Brief, x.TicketID = evil, evil, "tkt_evil"
	})
	card := Card{Task: worker, Stage: task.Running, Ticket: TicketRef{ID: "tkt_evil", Title: evil, Stage: task.TicketInProgress}}
	board := NewBoardWithTickets([]Card{card}, []TicketCard{ticket})
	for _, id := range []string{"tkt_evil", "orch_evil"} {
		for _, width := range []int{100, 40, 12} {
			for _, expanded := range []bool{false, true} {
				d := Detail{SelectedID: id, Expanded: expanded}
				for view, out := range map[string]string{
					"board": Render(board, d, width, ticketTheme),
					"page":  RenderPage(board, d, width, 40, ticketTheme),
				} {
					for _, r := range out {
						bidi := (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
						if r != '\n' && (unicode.IsControl(r) || bidi) {
							t.Fatalf("%s of %s at width %d (expanded %v) carries %U:\n%q", view, id, width, expanded, r, out)
						}
					}
				}
			}
		}
	}
}

// NewBoard now delegates to NewBoardWithTickets, so comparing the two renders (as
// TestBoardWithoutTicketsRendersExactlyAsBefore does) cannot catch a change in the
// no-ticket output. These SHA-256 digests record the board after f653bb6 added the
// search footer (the current base's no-ticket render) for these cards, widths, and
// selections. Ticket rendering must not change that baseline.
func TestBoardWithoutTicketsMatchesTheSearchBaseline(t *testing.T) {
	cards := []Card{
		{Task: sampleTask("orch_a", nil), Stage: task.Running},
		{Task: sampleTask("orch_b", func(x *task.Task) { x.Disposition = "integrated" }), Stage: task.Done},
	}
	details := map[string]Detail{
		"none":     {},
		"selected": {SelectedID: "orch_a"},
		"expanded": {SelectedID: "orch_a", Expanded: true},
	}
	want := map[string]string{
		"100/none":     "263c7331e70d55a87fd3ef4dd9a1385daa921577a6d31a026c3c7e15e94fb393",
		"100/selected": "3fc1d5f6585ec75ff3eb6d7f8af5d6d07b661e533d2f3e7b9a0b90588cfb064b",
		"100/expanded": "06eca67407fa95564743ee794a29f05ec46d547ba6c102942a88a63118bb02bd",
		"80/none":      "0f53761920731b01c96057d061a9021e10c8099ac63861075d1acd81a99c1c2b",
		"80/selected":  "3b7da3cd845c34e5515cf2ae5f17924baf9f846d69af161a613b03d198e0b10c",
		"80/expanded":  "8d5f702422c13760abd1e9a3c1caa88d4c845abbda0c68a0288550f860bad8ef",
	}
	digest := func(out string) string {
		sum := sha256.Sum256([]byte(out))
		return hex.EncodeToString(sum[:])
	}
	for _, width := range []int{100, 80} {
		for _, name := range []string{"none", "selected", "expanded"} {
			key := fmt.Sprintf("%d/%s", width, name)
			out := Render(NewBoard(cards), details[name], width, ticketTheme)
			if got := digest(out); got != want[key] {
				t.Errorf("%s: the no-ticket render changed (digest %s, want %s):\n%s", key, got, want[key], out)
			}
		}
	}
	const emptyWant = "2038e9b8bae0808a5a7020218b37603113c9f002ea5622ec3dffbc4d000bb054"
	if out := Render(NewBoard(nil), Detail{}, 80, ticketTheme); digest(out) != emptyWant {
		t.Errorf("the empty no-ticket board changed:\n%s", out)
	}
}

// A project with no tasks and no tickets is the first-use case: the board shows
// empty ticket columns and nothing is selected, and n is the only way forward. The
// help must say so whenever writes are enabled, and must not on a read-only board.
func TestNothingSelectedOffersNewTicketOnlyWhenWritesAreEnabled(t *testing.T) {
	boards := map[string]*Board{
		"no tasks and no tickets": NewBoardWithTickets(nil, nil),
		"a task but no selection": NewBoard([]Card{{Task: sampleTask("orch_a", nil), Stage: task.Running}}),
	}
	for name, board := range boards {
		if out := Render(board, Detail{TicketWrites: true}, 100, ticketTheme); !strings.Contains(out, "n new ticket") {
			t.Errorf("%s, writes enabled: the help must offer n new ticket:\n%s", name, out)
		}
		if out := Render(board, Detail{}, 100, ticketTheme); strings.Contains(out, "n new ticket") {
			t.Errorf("%s, read-only: the help must not offer n:\n%s", name, out)
		}
	}
}

// The title prompt drops what must not be stored (control characters and
// bidirectional overrides) but keeps text that is legitimately not ASCII: a
// zero-width non-joiner or joiner, a no-break space and an ideographic space are
// part of real titles. A form feed or a next-line character cannot be part of a
// one-line title, so each becomes a space like a newline does.
func TestTicketTitlePromptKeepsLegitimateNonASCIIText(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_q1", nil))
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})
	typeText(t, m, "n")
	typeText(t, m, "a\u200cb c\u00a0d\u3000e\u200df x\u202ey\fz p\u0085q")
	cmd := press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter must submit the title")
	}
	cmd()
	want := "a\u200cb c\u00a0d\u3000e\u200df xy z p q"
	if len(ops.creates) != 1 || ops.creates[0].title != want {
		t.Fatalf("the stored title must keep legitimate text and drop only unsafe runes:\n got %+q\nwant %+q", ops.creates, want)
	}
}

// A key repeat or a double tap of m before the first write finishes must not start
// a second move: the second would lose the compare-and-swap, and its refusal would
// replace the notice of the move that did succeed.
func TestRepeatedMoveKeyDoesNotStartASecondMoveWhileOneIsInFlight(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t)
	if got := f.model(t, Options{}).SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("the board must open on the backlog ticket, got %q", got)
	}
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})

	first := typeText(t, m, "m")
	if first == nil {
		t.Fatal("m on a backlog ticket must start the write")
	}
	if second := typeText(t, m, "m"); second != nil {
		t.Fatal("a second m while the first move is in flight must not start another write")
	}
	press(t, m, first())
	if len(ops.moves) != 1 {
		t.Fatalf("exactly one move may be written, got %+v", ops.moves)
	}
	if m.noticeErr || !strings.Contains(m.notice, "Moved tkt_a_backlog") {
		t.Fatalf("the notice must report the move that succeeded: %q (error=%v)", m.notice, m.noticeErr)
	}
}

// The in-flight guard is released when the write finishes, including when it
// fails, so a refused move does not leave m dead.
func TestMoveKeyWorksAgainAfterAFailedMove(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.seed(t)
	ops := &fakeTicketOps{moveErr: errors.New("ticket_changed: the ticket changed after its preview")}
	m := f.model(t, Options{Tickets: ops})

	first := typeText(t, m, "m")
	if first == nil {
		t.Fatal("m on a backlog ticket must start the write")
	}
	press(t, m, first())
	if !m.noticeErr {
		t.Fatalf("the refusal must be an error notice: %q", m.notice)
	}
	if again := typeText(t, m, "m"); again == nil {
		t.Fatal("after a failed move finished, m must be able to try again")
	}
}

// The guard is per ticket. Starting a move on A, then one on B, must not forget that
// A's write is still in flight: a repeat of m on A is still refused. A write for a
// different ticket is independent (its compare-and-swap cannot collide), so B is
// allowed to start while A is in flight.
func TestMoveGuardRemembersEveryTicketInFlight(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.addTicket(t, "tkt_a_backlog", task.ColumnBacklog)
	f.addTicket(t, "tkt_b_backlog", task.ColumnBacklog)
	f.seed(t)
	ops := &fakeTicketOps{}
	m := f.model(t, Options{Tickets: ops})

	if got := m.SelectedID(); got != "tkt_a_backlog" {
		t.Fatalf("selected %q, want the first backlog ticket", got)
	}
	startA := typeText(t, m, "m")
	if startA == nil {
		t.Fatal("m on the first backlog ticket must start the write")
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if got := m.SelectedID(); got != "tkt_b_backlog" {
		t.Fatalf("selected %q, want the second backlog ticket", got)
	}
	startB := typeText(t, m, "m")
	if startB == nil {
		t.Fatal("a move for a different ticket is independent and must be allowed while the first is in flight")
	}
	press(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if again := typeText(t, m, "m"); again != nil {
		t.Fatal("a repeat of m on the first ticket must still be refused while its write is in flight, even after another ticket started")
	}
	// The first write finishes while the second is still in flight: finishing one
	// ticket's write must not release the guard of another.
	press(t, m, startA())
	press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if got := m.SelectedID(); got != "tkt_b_backlog" {
		t.Fatalf("selected %q, want the second backlog ticket", got)
	}
	if again := typeText(t, m, "m"); again != nil {
		t.Fatal("the second ticket's write is still in flight: m on it must stay refused after the first write finished")
	}
	press(t, m, startB())
	if len(ops.moves) != 2 || ops.moves[0].id != "tkt_a_backlog" || ops.moves[1].id != "tkt_b_backlog" {
		t.Fatalf("exactly one move per ticket may be written, got %+v", ops.moves)
	}
	// Once its own write finished, the ticket is free again (the fake store did not
	// move it, so it is still a backlog ticket).
	if again := typeText(t, m, "m"); again == nil {
		t.Fatal("after the second ticket's own write finished, m on it must work again")
	}
}

// The last action's notice (a ticket write outcome or a focus refusal) is one line
// of free text. Like every other line the board draws, it must stay inside the
// terminal on every path that shows it: no selection, a task, a ticket, and the
// dedicated page of each.
func TestALongNoticeNeverMakesALineWiderThanTheTerminal(t *testing.T) {
	worker := sampleTask("orch_n1", nil)
	board := NewBoardWithTickets(
		[]Card{{Task: worker, Stage: task.Running}},
		[]TicketCard{ticketCardOf("tkt_n1", "a ticket", task.TicketBacklog)},
	)
	notice := "Cannot move tkt_n1: " + strings.Repeat("because-of-a-very-long-reason ", 12)
	for _, width := range []int{80, 40, 12} {
		for _, selected := range []string{"", "orch_n1", "tkt_n1"} {
			for _, expanded := range []bool{false, true} {
				for _, isErr := range []bool{true, false} {
					d := Detail{SelectedID: selected, Expanded: expanded, Notice: notice, Err: isErr, TicketWrites: true}
					for view, out := range map[string]string{
						"board": Render(board, d, width, ticketTheme),
						"page":  RenderPage(board, d, width, 30, ticketTheme),
					} {
						for _, line := range strings.Split(out, "\n") {
							if got := runeWidth(line); got > width {
								t.Fatalf("%s, selected %q, expanded %v, width %d: a line is %d columns wide:\n%q", view, selected, expanded, width, got, line)
							}
						}
					}
				}
			}
		}
	}
}

// A superseded ticket names its successor ticket on its detail page (issue #61).
func TestTicketDetailNamesItsSuccessorTicket(t *testing.T) {
	card := ticketCardOf("tkt_s1", "old plan", task.TicketDone)
	card.Ticket.Outcome, card.Ticket.Disposition, card.Ticket.Successor = task.OutcomeSuperseded, "replaced", "tkt_s2"
	page := strings.Join(ticketPageLines(card, 100, ticketTheme), "\n")
	if !strings.Contains(page, "Successor: tkt_s2") {
		t.Errorf("the superseded ticket's page does not name its successor:\n%s", page)
	}
}
