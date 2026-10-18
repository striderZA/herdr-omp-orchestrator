package board

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// TicketColumns are the two columns a ticket rests in before any task exists for
// it. They are a separate kind from the five task stages (Columns), drawn before
// them, and never a sixth and seventh task stage: a ticket that has tasks is
// shown through them, not as a card of its own (see ProjectTickets).
var TicketColumns = []task.TicketStage{
	task.TicketBacklog,
	task.TicketTodo,
}

// ticketStageLabels are the plain headings and words for every ticket stage. Only
// the first two are columns; the rest are how a task's detail names its ticket.
var ticketStageLabels = map[task.TicketStage]string{
	task.TicketBacklog:        "Backlog",
	task.TicketTodo:           "Todo",
	task.TicketPlanning:       "Planning",
	task.TicketPlanReady:      "Plan ready",
	task.TicketInProgress:     "In progress",
	task.TicketReadyForReview: "Ready for review",
	task.TicketDone:           "Done",
}

// TicketStageLabel is the board's word for one ticket stage. A stage this build
// does not know keeps its own name.
func TicketStageLabel(stage task.TicketStage) string {
	if label, ok := ticketStageLabels[stage]; ok {
		return label
	}
	return string(stage)
}

func ticketColumnIndex(stage task.TicketStage) int {
	for index, column := range TicketColumns {
		if column == stage {
			return index
		}
	}
	return -1
}

// TicketRef is the ticket a task was dispatched for, as its card names it: the
// title and the derived stage, so a task's detail can say where its ticket stands
// even though a ticket past todo is no longer a card.
type TicketRef struct {
	ID    string
	Title string
	Stage task.TicketStage
}

// TicketCard is one ticket as the board shows it. The ticket facts are carried
// verbatim; Stage is the only derived field and comes from task.TicketStageOf.
type TicketCard struct {
	Ticket task.Ticket
	Stage  task.TicketStage
	// Tasks are the tasks linked to the ticket, oldest first. A ticket that is a
	// card has none by construction (any linked task moves it past todo); the
	// field is what the detail lists when a board is built with some.
	Tasks []task.Task
}

// ID is the card's identity. The board selects and drills down by this ID.
func (c TicketCard) ID() string { return c.Ticket.ID }

// ProjectTickets projects one project's tickets onto cards. Only a ticket whose
// derived stage is backlog or todo is a card: once it has tasks it is shown
// through them, and a decided ticket is history. It is read-only and keeps the
// store's stable oldest-first order.
func ProjectTickets(ctx context.Context, store *state.Store, projectKey string) ([]TicketCard, error) {
	if store == nil {
		return nil, fmt.Errorf("board: no store")
	}
	key := strings.TrimSpace(projectKey)
	tickets, err := store.Tickets(ctx, key)
	if err != nil {
		return nil, err
	}
	tasks, err := store.Tasks(ctx, key)
	if err != nil {
		return nil, err
	}
	cards := make([]TicketCard, 0, len(tickets))
	for _, ticket := range tickets {
		linked := task.LinkedTasks(tasks, ticket.ID)
		stage := task.TicketStageOf(ticket, linked)
		if ticketColumnIndex(stage) < 0 {
			continue
		}
		cards = append(cards, TicketCard{Ticket: ticket, Stage: stage, Tasks: linked})
	}
	return cards, nil
}

// ticketRefs reads the tickets that the given tasks name, keyed by ticket id. It
// reads nothing when no task names a ticket, so a project that never used tickets
// costs the board no extra read.
func ticketRefs(ctx context.Context, store *state.Store, projectKey string, tasks []task.Task) (map[string]TicketRef, error) {
	named := false
	for _, fact := range tasks {
		if fact.TicketID != "" {
			named = true
			break
		}
	}
	if !named {
		return nil, nil
	}
	tickets, err := store.Tickets(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]TicketRef, len(tickets))
	for _, ticket := range tickets {
		stage := task.TicketStageOf(ticket, task.LinkedTasks(tasks, ticket.ID))
		refs[ticket.ID] = TicketRef{ID: ticket.ID, Title: ticket.Title, Stage: stage}
	}
	return refs, nil
}

// TicketColumn returns the tickets of one column in the store's order.
func (b *Board) TicketColumn(stage task.TicketStage) []TicketCard {
	if b == nil {
		return nil
	}
	return b.ticketsByStage[stage]
}

// Tickets returns every ticket that has a column, in column order.
func (b *Board) Tickets() []TicketCard {
	if b == nil {
		return nil
	}
	return b.tickets
}

// FindTicket returns one ticket card by id, including a ticket this build could
// not place in a column.
func (b *Board) FindTicket(id string) (TicketCard, bool) {
	if b == nil || id == "" {
		return TicketCard{}, false
	}
	for _, card := range b.tickets {
		if card.ID() == id {
			return card, true
		}
	}
	for _, card := range b.unplacedTickets {
		if card.ID() == id {
			return card, true
		}
	}
	return TicketCard{}, false
}

// hasTickets reports whether the board holds any ticket at all, placed or not.
func (b *Board) hasTickets() bool {
	return b != nil && len(b.tickets)+len(b.unplacedTickets) > 0
}

// showsTickets reports whether the ticket columns are drawn: only when there is a
// ticket to put in one, or when writes are enabled and the empty columns show
// where a new ticket would go. Otherwise the board renders exactly as it did
// before tickets existed.
func (b *Board) showsTickets(d Detail) bool {
	return d.TicketWrites || (b != nil && len(b.tickets) > 0)
}

// ticketText is one ticket card's single line: its title, then its ID (the
// identity), the same order a task card uses.
func ticketText(card TicketCard, selected bool) string {
	marker := "  "
	if selected {
		marker = "> "
	}
	return identifiedText(marker, card.Ticket.Title, card.ID())
}

func ticketColor(card TicketCard, selected string) string {
	if card.ID() == selected {
		return Focus
	}
	return Ink
}

// ticketColumnLines draws the two ticket columns: side by side when the terminal
// holds the five task columns, as sections otherwise. Every line is clipped to
// width, like a task's.
func ticketColumnLines(b *Board, d Detail, width int, th Theme) []string {
	if width >= MinWideWidth {
		return wideTicketColumns(b, d, width, th)
	}
	return narrowTicketSections(b, d, width, th)
}

func wideTicketColumns(b *Board, d Detail, width int, th Theme) []string {
	cell := (width - columnGap*(len(TicketColumns)-1)) / len(TicketColumns)
	texts := make([][]string, len(TicketColumns))
	rows := 0
	for i, stage := range TicketColumns {
		for _, card := range b.TicketColumn(stage) {
			texts[i] = append(texts[i], ticketText(card, card.ID() == d.SelectedID))
		}
		rows = max(rows, len(texts[i]))
	}

	heads := make([]string, len(TicketColumns))
	for i, stage := range TicketColumns {
		heads[i] = padded(th.bold(clip(TicketStageLabel(stage), cell)), cell)
	}
	out := []string{strings.Join(heads, " ")}
	for row := range rows {
		parts := make([]string, len(TicketColumns))
		for i, stage := range TicketColumns {
			if row >= len(texts[i]) {
				parts[i] = padded("", cell)
				continue
			}
			card := b.TicketColumn(stage)[row]
			parts[i] = padded(th.paint(ticketColor(card, d.SelectedID), cutText(texts[i][row], cell)), cell)
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

func narrowTicketSections(b *Board, d Detail, width int, th Theme) []string {
	var out []string
	for i, stage := range TicketColumns {
		if i > 0 {
			out = append(out, "")
		}
		cards := b.TicketColumn(stage)
		out = append(out, th.bold(clip(fmt.Sprintf("%s (%d)", TicketStageLabel(stage), len(cards)), width)))
		for _, card := range cards {
			out = append(out, th.paint(ticketColor(card, d.SelectedID), cutText(ticketText(card, card.ID() == d.SelectedID), width)))
		}
	}
	return out
}

// ticketLine is the line a task's detail shows for the ticket it was dispatched
// for: the ticket and where it stands. A link to a ticket the board could not
// read is still shown rather than hidden.
func ticketLine(card Card) string {
	id := strings.TrimSpace(card.Task.TicketID)
	if id == "" {
		return ""
	}
	ref := card.Ticket
	if ref.ID == "" {
		return "Ticket: " + singleLine(id) + " (not found)"
	}
	return "Ticket: " + singleLine(ref.ID) + " " + singleLine(ref.Title) + " (" + strings.ToLower(TicketStageLabel(ref.Stage)) + ")"
}

// linkedTasks lists a ticket's tasks with the stage each is in, or says there are
// none.
func linkedTasks(card TicketCard) string {
	if len(card.Tasks) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(card.Tasks))
	for _, linked := range card.Tasks {
		parts = append(parts, linked.ID+" ("+strings.ToLower(StageLabel(task.StageOf(linked)))+")")
	}
	return strings.Join(parts, ", ")
}

// ticketWriteKeys are the write keys worth offering for a selected ticket: m
// moves a backlog ticket to todo and starts planning, n creates a ticket. A
// read-only board offers neither.
func ticketWriteKeys(card TicketCard, d Detail) []string {
	if !d.TicketWrites {
		return nil
	}
	var keys []string
	if card.Stage == task.TicketBacklog {
		keys = append(keys, "m move to todo + start planner")
	}
	return append(keys, "n new ticket")
}

// ticketHelpLine is the keyboard help for a selected ticket. A ticket has no
// worker pane, so focus is not offered.
func ticketHelpLine(card TicketCard, d Detail) string {
	parts := []string{"Enter details"}
	if d.Expanded {
		parts = []string{"Esc collapse"}
	}
	parts = append(parts, "r refresh", "/ search", "Tab next column", "Shift+Tab previous column")
	parts = append(parts, ticketWriteKeys(card, d)...)
	return strings.Join(append(parts, "q close"), ", ")
}

// ticketDetailLines draws the selected ticket's facts and the board's keys below
// the columns: the collapsed form clips the body to one line, the expanded form
// wraps all of it.
func ticketDetailLines(card TicketCard, d Detail, width int, th Theme) []string {
	sep := th.separator()
	header := "Selected " + card.ID() + sep + strings.ToLower(TicketStageLabel(card.Stage)) +
		sep + "revision " + strconv.FormatInt(card.Ticket.Revision, 10)
	out := []string{th.paint(Ink, clip(header, width))}
	out = append(out, wrap("Title: "+orNone(singleLine(card.Ticket.Title), "none"), width)...)
	body := "Body: " + orNone(singleLine(card.Ticket.Body), "none")
	if d.Expanded {
		out = append(out, wrap(body, width)...)
	} else {
		out = append(out, clip(body, width))
	}
	out = append(out, wrap("Linked tasks: "+linkedTasks(card), width)...)
	if notice := noticeLine(d, th, width); notice != "" {
		out = append(out, notice)
	}
	return append(out, clipHelpLine(ticketHelpLine(card, d), width))
}

// ticketPageLines draws every fact of one ticket, wrapped to width: the body of
// the dedicated detail page, which scrolls when it is taller than the terminal.
func ticketPageLines(card TicketCard, width int, th Theme) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	add := func(label, value string) {
		out = append(out, wrap(label+": "+orNone(singleLine(value), "none"), width)...)
	}
	section := func(name string) {
		out = append(out, "", th.paint(Ink, clip(name, width)))
	}
	out = append(out, th.paint(Ink, clip(card.ID()+th.separator()+strings.ToLower(TicketStageLabel(card.Stage)), width)))
	add("Title", card.Ticket.Title)
	add("Column", string(card.Ticket.Column))
	add("Revision", strconv.FormatInt(card.Ticket.Revision, 10))
	if card.Ticket.Decided() {
		add("Decision", card.Ticket.Outcome+": "+card.Ticket.Disposition)
		if card.Ticket.Successor != "" {
			add("Successor", card.Ticket.Successor)
		}
	}
	section("Body")
	out = append(out, wrap(orNone(strings.TrimSpace(card.Ticket.Body), "none"), width)...)
	section("Linked tasks")
	if len(card.Tasks) == 0 {
		out = append(out, "none")
	}
	for _, linked := range card.Tasks {
		out = append(out, wrap(linked.ID+" ("+strings.ToLower(StageLabel(task.StageOf(linked)))+") "+singleLine(linked.Title), width)...)
	}
	return out
}

// pageLinesOf is the dedicated page of the selected task or ticket, and whether
// the selection is on the board at all. The detail page and its scrolling both
// measure the same lines, so the primary marker that leads the page is counted
// here as well as drawn by RenderPage.
func pageLinesOf(b *Board, id string, width int, th Theme) ([]string, bool) {
	marker := primaryMarkerBlock(b, width, th)
	if ticket, ok := b.FindTicket(id); ok {
		return append(marker, ticketPageLines(ticket, width, th)...), true
	}
	if card, ok := b.Find(id); ok {
		return append(marker, pageLines(card, width, th)...), true
	}
	return nil, false
}

// TicketOps is the board's only write path: create a ticket and move a backlog
// ticket to todo. The human's keypress confirms the move and planner start; the
// coordinator checks the displayed digest, records the task and starts or queues
// it. The board itself never writes a task or calls Herdr. A nil TicketOps keeps
// the board fully read-only.
type TicketOps interface {
	CreateTicket(ctx context.Context, projectKey, title string) error
	MoveTicketToTodo(ctx context.Context, projectKey, ticketID, digest string) error
}

// MovedTicketError is what a TicketOps move returns when it recorded the move but
// starting the ticket's planner afterwards failed. The ticket did move, so the
// board says so and shows why the planner has not started instead of reporting a
// failed move. Err is that cause.
type MovedTicketError struct{ Err error }

func (e *MovedTicketError) Error() string {
	return "the ticket moved, but its planner did not start: " + e.Err.Error()
}

func (e *MovedTicketError) Unwrap() error { return e.Err }

type MovedTicketSignalError struct{ Err error }

func (e *MovedTicketSignalError) Error() string {
	return "the ticket moved, but recovery signalling failed: " + e.Err.Error()
}

func (e *MovedTicketSignalError) Unwrap() error { return e.Err }
