// Package board is the terminal projection of the orchestrator's facts: tasks in
// their five stages, and the tickets that have not started in two columns
// before them.
//
// The board is read-only for tasks. It derives every card's stage through
// task.StageOf (a ticket's through task.TicketStageOf) and reads through
// state.Store's read methods only: it never writes a task, never dispatches,
// reassigns, approves, merges or cleans anything up. Its only writes are the two
// the human asks for with a keypress, creating a ticket and moving a backlog
// ticket to todo, and they go through the injected TicketOps interface (the
// coordinator, in the executable): never to the store from here, and never to
// Herdr. Its one other side-effecting key focuses a worker pane, and only after
// re-reading the task and proving against Herdr that the pane, terminal,
// checkout and OMP session are still the recorded ones.
//
// A primary that is absent or unproven is marked at the head of the board. The
// marker is the PrimaryAttention fact on task.Project (written by reconciliation
// and the paths that probe the primary), read from the store like every other
// fact: it makes no Herdr call and moves no card.
//
// Herdr's own activity (working, idle, blocked, done, unknown) is shown on the
// card as a separate indicator. A terminal that stopped is not a task that
// finished, so activity never moves a card between columns.
package board

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"

	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

// Palette is the board's fallback palette. The board never sets a background and
// never guesses the terminal's own theme, so these are the only colors it uses;
// attention is the single strong accent.
const (
	Ink       = "#D8DEE9"
	Muted     = "#77839A"
	Focus     = "#4EA8DE"
	Attention = "#E6A23C"
	Success   = "#69B578"
	Failure   = "#D46A6A"
)

// Columns are the five board stages, in the order a task moves through them.
// They are exactly the stages task.StageOf projects.
var Columns = []task.Stage{
	task.Queued,
	task.Running,
	task.NeedsInput,
	task.ReadyForReview,
	task.Done,
}

// stageLabels are the plain sentence-case column headings.
var stageLabels = map[task.Stage]string{
	task.Queued:         "Queued",
	task.Running:        "Running",
	task.NeedsInput:     "Needs input",
	task.ReadyForReview: "Ready for review",
	task.Done:           "Done",
}

// StageLabel is the board's heading for one stage. A stage this build does not
// know keeps its own name rather than being folded into another column.
func StageLabel(stage task.Stage) string {
	if label, ok := stageLabels[stage]; ok {
		return label
	}
	return string(stage)
}

// Theme is the board's palette and glyph set. A terminal without color and
// without Unicode gets a board made of ASCII: no escape sequences, no box
// drawing.
type Theme struct {
	Profile termenv.Profile
	Unicode bool
}

// NewTheme resolves a theme from a terminal color profile. Color and Unicode are
// assumed together: a terminal that cannot render color is the conservative
// case for glyphs too.
func NewTheme(profile termenv.Profile) Theme {
	return Theme{Profile: profile, Unicode: profile != termenv.Ascii}
}

// paint applies one palette color to already-clipped text. The Ascii profile
// yields the text unchanged, which is what makes the fallback free of escapes.
func (t Theme) paint(hex, text string) string {
	if text == "" || t.Profile == termenv.Ascii {
		return text
	}
	return t.Profile.String(text).Foreground(t.Profile.Color(hex)).String()
}

// bold sets a heading apart by weight rather than by another palette color, so
// the palette keeps its single strong accent. The text keeps the terminal's own
// color, and the Ascii profile yields it unchanged.
func (t Theme) bold(text string) string {
	if text == "" || t.Profile == termenv.Ascii {
		return text
	}
	return t.Profile.String(text).Bold().String()
}

func (t Theme) rule() string {
	if t.Unicode {
		return "─"
	}
	return "-"
}

func (t Theme) separator() string {
	if t.Unicode {
		return " · "
	}
	return " | "
}

// Card is one task as the board shows it. The task facts are carried verbatim;
// Stage is the only derived field and comes from task.StageOf.
type Card struct {
	Task  task.Task
	Stage task.Stage
	// DependencyBlocks lists dependencies closed with a non-accepting outcome.
	DependencyBlocks []string
	// Ticket is the ticket this task was dispatched for, when it names one: its
	// title and derived stage, so the task's detail can say where the ticket
	// stands even once the ticket is no longer a card.
	Ticket TicketRef
	// Review is the card's wait in ready_for_review, read from its task's history.
	// Every other card leaves it zero (see review.go).
	Review ReviewWait
}

// ID is the card's identity. The board selects, focuses and drills down by this
// ID, never by a title or another mutable label.
func (c Card) ID() string { return c.Task.ID }

// Title is the recorded human-readable name.
func (c Card) Title() string { return c.Task.Title }

// Branch is the recorded working branch.
func (c Card) Branch() string { return c.Task.Branch }

// Pane is the current public pane id the worker was last seen on.
func (c Card) Pane() string { return c.Task.PublicPaneID }

// Activity is Herdr's own word for what the terminal is doing. It is never a
// stage.
func (c Card) Activity() string { return strings.TrimSpace(c.Task.Activity) }

// Attention is what needs a human: the worker's blocker when it recorded one,
// otherwise the attention the plugin raised.
func (c Card) Attention() string {
	if blocker := strings.TrimSpace(c.Task.Blocker); blocker != "" {
		return blocker
	}
	return strings.TrimSpace(c.Task.Attention)
}

// Observation renders the last recorded Herdr observation: the activity word
// with when it was seen. Timestamps are recorded in Unix milliseconds and shown
// in UTC, so a pane opened anywhere reads the same instant.
func (c Card) Observation() string {
	activity := c.Activity()
	if activity == "" {
		return ""
	}
	if c.Task.ActivitySeenAt <= 0 {
		return activity
	}
	return activity + " at " + time.UnixMilli(c.Task.ActivitySeenAt).UTC().Format("2006-01-02 15:04")
}

// ProjectCards projects one project's tasks onto cards. It is a read-only
// projection: the store's durable facts are never touched, and the order is the
// store's stable oldest-first order.
func ProjectCards(ctx context.Context, store *state.Store, projectKey string) ([]Card, error) {
	return projectCardsAt(ctx, store, projectKey, time.Now())
}

// projectCardsAt is ProjectCards with the instant the waits are measured to, so
// a test can fix it.
func projectCardsAt(ctx context.Context, store *state.Store, projectKey string, asOf time.Time) ([]Card, error) {
	if store == nil {
		return nil, fmt.Errorf("board: no store")
	}
	key := strings.TrimSpace(projectKey)
	tasks, err := store.Tasks(ctx, key)
	if err != nil {
		return nil, err
	}
	refs, err := ticketRefs(ctx, store, key, tasks)
	if err != nil {
		return nil, err
	}
	cards := make([]Card, 0, len(tasks))
	for _, fact := range tasks {
		stage := task.StageOf(fact)
		card := Card{
			Task:             fact,
			Stage:            stage,
			DependencyBlocks: dependencyBlocks(tasks, fact),
			Ticket:           refs[fact.TicketID],
		}
		if stage == task.ReadyForReview {
			wait, err := readReviewWait(ctx, store, fact.ID, stage, asOf)
			if err != nil {
				return nil, err
			}
			card.Review = wait
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// dependencyBlocks derives, from persisted dependencies and outcomes, why a
// queued task cannot start: a dependency closed with a non-accepting outcome
// never releases it. Nothing is stored; a dependency that is merely not done
// yet is waiting, not blocked.
func dependencyBlocks(all []task.Task, fact task.Task) []string {
	if task.StageOf(fact) != task.Queued {
		return nil
	}
	var out []string
	for _, id := range fact.Dependencies {
		for _, dep := range all {
			if dep.ID == id && dep.Disposition != "" && !dep.Accepted() {
				if dep.Outcome == task.OutcomeSuperseded && dep.Successor != "" {
					out = append(out, fmt.Sprintf("dependency %s was closed as superseded; the primary can re-point this task to its successor %s, or re-plan and re-dispatch it without that dependency", dep.ID, dep.Successor))
				} else {
					out = append(out, fmt.Sprintf("dependency %s was closed as %s; the primary must re-plan and re-dispatch this task without that dependency", dep.ID, dep.Outcome))
				}
			}
		}
	}
	return out
}

// Board is the renderable projection: task cards grouped into their five
// columns, and the tickets that have not started yet grouped into their two. A
// ticket column is its own kind, never a sixth and seventh task stage.
type Board struct {
	cards    []Card
	unplaced []Card
	byStage  map[task.Stage][]Card

	tickets         []TicketCard
	unplacedTickets []TicketCard
	ticketsByStage  map[task.TicketStage][]TicketCard

	// primaryAttention is why the project's primary is not verified (see
	// primary.go); empty draws no marker.
	primaryAttention string
}

// NewBoard groups cards into columns. Cards() is in column order; a card whose
// stage is not one of the five columns is kept and rendered separately, so a
// card this build cannot place is never silently dropped.
func NewBoard(cards []Card) *Board { return NewBoardWithTickets(cards, nil) }

// NewBoardWithTickets is NewBoard plus the tickets that have not started. A
// ticket whose stage has no ticket column is kept and rendered separately, for
// the same reason: nothing is silently dropped. With no tickets the board is
// exactly NewBoard's.
func NewBoardWithTickets(cards []Card, tickets []TicketCard) *Board {
	b := &Board{byStage: map[task.Stage][]Card{}, ticketsByStage: map[task.TicketStage][]TicketCard{}}
	for _, card := range cards {
		if _, ok := stageLabels[card.Stage]; ok {
			b.byStage[card.Stage] = append(b.byStage[card.Stage], card)
			continue
		}
		b.unplaced = append(b.unplaced, card)
	}
	for _, stage := range Columns {
		b.cards = append(b.cards, b.byStage[stage]...)
	}
	b.cards = append(b.cards, b.unplaced...)
	for _, card := range tickets {
		if ticketColumnIndex(card.Stage) >= 0 {
			b.ticketsByStage[card.Stage] = append(b.ticketsByStage[card.Stage], card)
			continue
		}
		b.unplacedTickets = append(b.unplacedTickets, card)
	}
	for _, stage := range TicketColumns {
		b.tickets = append(b.tickets, b.ticketsByStage[stage]...)
	}
	return b
}

// filterTaskBoard returns a display-only board containing tasks that match query.
// Ticket cards, task stages, unknown cards and the primary marker are preserved.
func filterTaskBoard(source *Board, query string) *Board {
	if source == nil {
		return NewBoard(nil)
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return source
	}
	cards := make([]Card, 0, len(source.cards))
	for _, card := range source.cards {
		if taskMatchesQuery(card, needle) {
			cards = append(cards, card)
		}
	}
	tickets := make([]TicketCard, 0, len(source.tickets)+len(source.unplacedTickets))
	tickets = append(tickets, source.tickets...)
	tickets = append(tickets, source.unplacedTickets...)
	filtered := NewBoardWithTickets(cards, tickets)
	filtered.primaryAttention = source.primaryAttention
	return filtered
}

// taskMatchesQuery searches task fields, displayed dependency blocks and report labels.
func taskMatchesQuery(card Card, needle string) bool {
	fields := []string{
		card.Task.ID,
		card.Task.Title,
		card.Task.Brief,
		card.Task.Blocker,
		card.Task.Attention,
	}
	fields = append(fields, card.DependencyBlocks...)
	if report := card.Task.Report; report != nil {
		fields = append(fields, report.Kind, report.Summary, report.Branch, report.Risk)
		fields = append(fields, report.Evidence...)
		for _, check := range report.Checks {
			fields = append(fields, check.Label())
		}
	}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(strings.TrimSpace(field)), needle) {
			return true
		}
	}
	return false
}

// Cards returns every card in column order, with the cards this build could not
// place last.
func (b *Board) Cards() []Card {
	if b == nil {
		return nil
	}
	return b.cards
}

// Column returns the cards of one stage in the store's order.
func (b *Board) Column(stage task.Stage) []Card {
	if b == nil {
		return nil
	}
	return b.byStage[stage]
}

// Find returns one card by task ID.
func (b *Board) Find(id string) (Card, bool) {
	if b == nil {
		return Card{}, false
	}
	for _, card := range b.cards {
		if card.ID() == id {
			return card, true
		}
	}
	return Card{}, false
}

// Detail is the selection the board renders below its columns.
type Detail struct {
	// SelectedID is the selected task's ID, or empty when no card is selected.
	SelectedID string
	// Expanded asks for the task's full facts instead of the one-line summary.
	Expanded bool
	// Scroll is the first visible line of the dedicated detail page.
	Scroll int
	// Notice is one actionable line about the last board action (for example a
	// refused focus). Err marks it as a failure.
	Notice string
	Err    bool
	// TicketWrites says the board can create and move tickets in this session. It
	// only affects what is drawn: the empty ticket columns are shown so a new
	// ticket has somewhere to appear.
	TicketWrites bool
}

// columnMinWidth is the narrowest usable card cell, so the five columns only
// share a row when the terminal can hold them.
const (
	columnMinWidth = 16
	columnGap      = 1
	// MinWideWidth is the narrowest terminal that holds five stage columns side
	// by side. Narrower terminals get one-column stage sections instead.
	MinWideWidth = 5*columnMinWidth + 4*columnGap
)

// Render draws the board at width columns. Every line is clipped or wrapped to
// width, in both layouts.
func Render(b *Board, d Detail, width int, th Theme) string {
	return renderWithSearch(b, d, width, th, "")
}

func renderWithSearch(b *Board, d Detail, width int, th Theme, query string) string {
	if width < 1 {
		width = 1
	}
	query = strings.TrimSpace(query)
	// Keep the primary marker first; search status follows it.
	lines := primaryMarkerBlock(b, width, th)
	if query != "" {
		lines = append(lines, th.paint(Focus, clip("Filter: "+singleLine(query), width)))
	}
	if len(b.Cards()) == 0 && !b.hasTickets() && !d.TicketWrites {
		lines = append(lines, wrap(taskEmptyMessage(query), width)...)
		lines = append(lines, "")
		if notice := noticeLine(d, th, width); notice != "" {
			lines = append(lines, notice)
		}
		return strings.Join(append(lines, clip("/ search, q close", width)), "\n")
	}
	if b.showsTickets(d) {
		lines = append(lines, ticketColumnLines(b, d, width, th)...)
		lines = append(lines, "")
	}
	switch {
	case len(b.Cards()) == 0:
		lines = append(lines, wrap(taskEmptyMessage(query), width)...)
	case width >= MinWideWidth:
		lines = append(lines, wideColumns(b, d, width, th)...)
	default:
		lines = append(lines, narrowSections(b, d, width, th)...)
	}
	// A card this build cannot place is still shown rather than silently dropped.
	if b != nil && (len(b.unplaced) > 0 || len(b.unplacedTickets) > 0) {
		lines = append(lines, "", clip("Not placed by this build:", width))
		for _, card := range b.unplaced {
			lines = append(lines, clip(cardText(card, card.ID() == d.SelectedID), width))
		}
		for _, card := range b.unplacedTickets {
			lines = append(lines, clip(ticketText(card, card.ID() == d.SelectedID), width))
		}
	}
	lines = append(lines, strings.Repeat(th.rule(), width))
	lines = append(lines, detailLines(b, d, width, th)...)
	return strings.Join(lines, "\n")
}

func taskEmptyMessage(query string) string {
	if strings.TrimSpace(query) == "" {
		return noTasksMessage
	}
	return fmt.Sprintf("No tasks match %q", singleLine(query))
}

// noTasksMessage is what a board with no task says; it is also drawn under the
// ticket columns when there are tickets but no tasks.
const noTasksMessage = "No tasks yet. Ask your primary to delegate a task"

// wideColumns draws the five stage columns side by side, each cell clipped to
// its own width so no line is wider than the terminal.
func wideColumns(b *Board, d Detail, width int, th Theme) []string {
	cell := (width - columnGap*(len(Columns)-1)) / len(Columns)
	texts := make([][]string, len(Columns))
	for i, stage := range Columns {
		for _, card := range b.Column(stage) {
			texts[i] = append(texts[i], cardText(card, card.ID() == d.SelectedID))
		}
	}
	rows := 0
	for _, column := range texts {
		if len(column) > rows {
			rows = len(column)
		}
	}

	heads := make([]string, len(Columns))
	for i, stage := range Columns {
		heads[i] = padded(th.bold(clip(StageLabel(stage), cell)), cell)
	}
	out := []string{strings.Join(heads, " ")}

	for row := range rows {
		parts := make([]string, len(Columns))
		for i, stage := range Columns {
			if row >= len(texts[i]) {
				parts[i] = padded("", cell)
				continue
			}
			text := clip(texts[i][row], cell)
			parts[i] = padded(th.paint(cardColor(rowCard(b, stage, row), d.SelectedID), text), cell)
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

// rowCard is the card a column row belongs to, so the row can be colored the
// way the card is.
func rowCard(b *Board, stage task.Stage, row int) Card {
	cards := b.Column(stage)
	if row < 0 || row >= len(cards) {
		return Card{}
	}
	return cards[row]
}

// narrowSections draws one section per stage, which is the layout a terminal
// narrower than MinWideWidth gets.
func narrowSections(b *Board, d Detail, width int, th Theme) []string {
	var out []string
	for i, stage := range Columns {
		if i > 0 {
			out = append(out, "")
		}
		cards := b.Column(stage)
		heading := fmt.Sprintf("%s (%d)", StageLabel(stage), len(cards))
		out = append(out, th.bold(clip(heading, width)))
		for _, card := range cards {
			out = append(out, th.paint(cardColor(card, d.SelectedID), clip(cardText(card, card.ID() == d.SelectedID), width)))
		}
	}
	return out
}

// cardColor is the one accent a card may carry: the selection, an attention, or
// nothing loud at all.
func cardColor(card Card, selected string) string {
	switch {
	case card.ID() == selected:
		return Focus
	case card.Stage == task.NeedsInput:
		return Attention
	case card.Stage == task.Done:
		return Muted
	default:
		return Ink
	}
}

// cardText is one card's single line: its selection marker, a text attention
// marker for needs-input cards, its title, its ID and Herdr's activity. The title
// comes first, so a narrow column clips the identifier, not the words a reader
// scans for.
func cardText(card Card, selected bool) string {
	marker := "  "
	if selected {
		marker = "> "
	}
	if card.Stage == task.NeedsInput {
		marker += "! "
	}
	text := identifiedText(marker, card.Title(), card.ID())
	if activity := card.Activity(); activity != "" {
		text += " (" + activity + ")"
	}
	return text
}

// identifiedText is a card's label: the title, then its ID in brackets. Both
// layouts use it, so a task and a ticket read the same way.
func identifiedText(marker, title, id string) string {
	title = singleLine(title)
	if title == "" {
		return marker + "[" + id + "]"
	}
	return marker + title + " [" + id + "]"
}

// singleLine collapses a human-entered value onto one line. A title or branch
// with a newline in it must not become two board lines. Stored text is also
// written by agents, and a human acts on what the board shows, so what must never
// reach a terminal is dropped here (see unsafeRune).
func singleLine(text string) string {
	return strings.Join(strings.Fields(strings.Map(unsafeRune, text)), " ")
}

// unsafeRune drops control characters (an escape sequence can move the cursor,
// retitle the window or hide text) and the bidirectional overrides and isolates
// (they reorder what the reader sees). Whitespace stays so that a tab, a carriage
// return or a next-line character still separates words; Fields folds it. Every
// other rune, including non-ASCII spaces and emoji joiners, is kept.
func unsafeRune(r rune) rune {
	if unicode.IsSpace(r) {
		return r
	}
	if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
		return -1
	}
	return r
}

// detailLines draws the selected task's facts and the board's keys.
func detailLines(b *Board, d Detail, width int, th Theme) []string {
	if ticket, ok := b.FindTicket(d.SelectedID); ok {
		return ticketDetailLines(ticket, d, width, th)
	}
	card, ok := b.Find(d.SelectedID)
	if d.SelectedID == "" || !ok {
		out := []string{clip("No task selected. tab picks a stage, up/down a card.", width)}
		if notice := noticeLine(d, th, width); notice != "" {
			out = append(out, notice)
		}
		return append(out, clip(noSelectionHelp(d), width), clip("", width))
	}

	sep := th.separator()
	header := "Selected " + card.ID() + sep + "branch " + orNone(singleLine(card.Branch()), "none") +
		sep + strings.ToLower(StageLabel(card.Stage)) + sep + "worker " + orNone(singleLine(card.Pane()), "none")
	out := []string{th.paint(Ink, clip(header, width))}
	if line := ticketLine(card); line != "" {
		out = append(out, wrap(line, width)...)
	}
	out = append(out, reviewWaitLines(card, width)...)

	if d.Expanded {
		out = append(out, wrap("Brief: "+orNone(singleLine(card.Task.Brief), "none"), width)...)
		if len(card.DependencyBlocks) > 0 {
			out = append(out, wrap("Blocked: "+singleLine(strings.Join(card.DependencyBlocks, "; ")), width)...)
		}
		if question := card.Attention(); question != "" {
			label := "Attention: "
			if strings.TrimSpace(card.Task.Blocker) != "" {
				label = "Blocked: "
			}
			out = append(out, wrap(label+singleLine(question), width)...)
		}
		if report := card.Task.Report; report != nil {
			out = append(out, wrap(fmt.Sprintf("Report: %s: %s", orNone(singleLine(report.Kind), "report"), orNone(singleLine(report.Summary), "no summary")), width)...)
			if len(report.Evidence) > 0 {
				out = append(out, wrap("Evidence: "+singleLine(strings.Join(report.Evidence, ", ")), width)...)
			}
			if len(report.Checks) > 0 {
				out = append(out, wrap("Checks: "+singleLine(strings.Join(task.CheckLabels(report.Checks), ", ")), width)...)
			}
			if risk := singleLine(report.Risk); risk != "" {
				out = append(out, wrap("Risk: "+risk, width)...)
			}
		}
		if observed := card.Observation(); observed != "" {
			out = append(out, wrap("Last observation: "+observed, width)...)
		}
	} else {
		if summary := cardSummary(card); summary != "" {
			out = append(out, wrap(summary, width)...)
		}
	}

	if notice := noticeLine(d, th, width); notice != "" {
		out = append(out, notice)
	}
	out = append(out, clip(helpLine(d), width))
	return out
}

// cardSummary is the one line the collapsed details show: what needs a human
// first, then what the worker reported, then what it was asked to do.
func cardSummary(card Card) string {
	if len(card.DependencyBlocks) > 0 {
		return "Blocked: " + singleLine(strings.Join(card.DependencyBlocks, "; "))
	}
	if question := card.Attention(); question != "" {
		if strings.TrimSpace(card.Task.Blocker) != "" {
			return "Blocked: " + singleLine(question)
		}
		return "Attention: " + singleLine(question)
	}
	if report := card.Task.Report; report != nil {
		if summary := strings.TrimSpace(report.Summary); summary != "" {
			return fmt.Sprintf("Report: %s: %s", orNone(singleLine(report.Kind), "report"), singleLine(summary))
		}
	}
	return singleLine(card.Task.Brief)
}

// noSelectionHelp is the help when nothing is selected: only the keys that act
// without a selection. n is the way forward on an empty board, so it is offered
// whenever ticket writes are enabled; a read-only board offers only q.
func noSelectionHelp(d Detail) string {
	if d.TicketWrites {
		return "/ search, n new ticket, q close"
	}
	return "/ search, q close"
}

// helpLine is the short keyboard help: the board has no other chrome.
func helpLine(d Detail) string {
	if d.Expanded {
		return "Esc collapse, / search, f focus, q close"
	}
	if d.TicketWrites {
		return "Enter details, / search, f focus, n new ticket, q close"
	}
	return "Enter details, / search, f focus, q close"
}

// noticeLine renders the last action's outcome, if it said anything, as one line
// that fits the terminal: the text is free-form (a refusal carries whatever the
// coordinator or Herdr said), so it is folded and clipped like every other line.
func noticeLine(d Detail, th Theme, width int) string {
	notice := singleLine(d.Notice)
	if notice == "" {
		return ""
	}
	if d.Err {
		return th.paint(Failure, clip("Error: "+notice, width))
	}
	return th.paint(Success, clip(notice, width))
}

// padded puts text in a cell of exactly width columns. The text is painted
// before it is padded, so padding never carries color and the visible width is
// independent of the escape sequences.
func padded(text string, width int) string {
	if visible := displayWidth(stripEscapes(text)); visible < width {
		return text + strings.Repeat(" ", width-visible)
	}
	return text
}

// stripEscapes removes SGR sequences so a painted string can be measured.
func stripEscapes(text string) string {
	if !strings.ContainsRune(text, '\x1b') {
		return text
	}
	var out strings.Builder
	escaped := false
	for _, r := range text {
		switch {
		case escaped && r == 'm':
			escaped = false
		case escaped:
		case r == '\x1b':
			escaped = true
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// displayWidth counts the terminal columns text occupies. It is not the rune
// count: a wide (CJK) rune takes two columns, so a title the user wrote in
// another script cannot push a line past the terminal.
func displayWidth(text string) int { return runewidth.StringWidth(text) }

// clip cuts text to at most width columns. Cells and detail lines are cut rather
// than wrapped, so long text never pushes a line past the terminal. A rune that
// would straddle the bound is dropped rather than half-rendered; the caller pads
// a cell if it needs one.
func clip(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(text) <= width {
		return text
	}
	var out strings.Builder
	used := 0
	for _, r := range text {
		size := runewidth.RuneWidth(r)
		if used+size > width {
			break
		}
		out.WriteRune(r)
		used += size
	}
	return out.String()
}

// wrap folds text onto lines of at most width columns. A single word wider than
// the line is cut, because the board is bounded on any terminal. It drops what
// must never reach a terminal (see unsafeRune), so text that is wrapped without
// first going through singleLine, such as a ticket body, is covered too.
func wrap(text string, width int) []string {
	if width <= 0 {
		return nil
	}
	words := strings.Fields(strings.Map(unsafeRune, text))
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	line := ""
	for _, word := range words {
		for displayWidth(word) > width {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			piece := clip(word, width)
			if piece == "" {
				// One rune wider than the whole line: emit it alone rather than
				// loop on a word that can never be cut down.
				runes := []rune(word)
				piece, word = string(runes[0]), string(runes[1:])
			} else {
				// piece is a whole-rune prefix of word, so its byte length is the
				// offset of the next rune. Advancing by a rune count here would
				// split a multibyte rune and lose or duplicate the text around the
				// cut.
				word = word[len(piece):]
			}
			lines = append(lines, piece)
		}
		switch {
		case line == "":
			line = word
		case displayWidth(line)+1+displayWidth(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func orNone(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
