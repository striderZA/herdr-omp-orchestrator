package board

import (
	"context"
	"fmt"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"
)

// DefaultRefreshInterval is how often the board re-reads the store. It is a
// read: the board still writes nothing.
const DefaultRefreshInterval = 2 * time.Second

// Herdr is the Herdr boundary the board needs: the reads it proves a worker
// pane with, and the one command it may run. Every implementation of this
// interface is a CLI-shaped boundary, so the board cannot call anything else.
type Herdr interface {
	PaneGet(ctx context.Context, paneID string) (*herdr.PaneInfo, error)
	AgentGet(ctx context.Context, target string) (*herdr.AgentInfo, error)
	AgentSession(ctx context.Context, target string) (kind, value string, ok bool, err error)
	AgentFocus(ctx context.Context, target string) (*herdr.AgentInfo, error)
}

// Options tunes one board program. A zero value reads the environment's color
// profile and refreshes every DefaultRefreshInterval.
type Options struct {
	// Input and Output override the terminal the program runs on. A nil reader
	// or writer leaves Bubble Tea's own choice alone.
	Input  io.Reader
	Output io.Writer
	// Theme is the palette and glyph set. The zero value is resolved from the
	// environment.
	Theme Theme
	// RefreshInterval is how often the store is re-read. A negative value
	// disables the tick.
	RefreshInterval time.Duration
	// Tickets is the board's only write path: creating a ticket and moving a
	// backlog ticket to todo. A nil value keeps the board fully read-only, and the
	// board then says so instead of offering the keys.
	Tickets TicketOps
}

func (o Options) withDefaults() Options {
	if o.Theme == (Theme{}) {
		o.Theme = NewTheme(termenv.ColorProfile())
	}
	if o.RefreshInterval == 0 {
		o.RefreshInterval = DefaultRefreshInterval
	}
	return o
}

// tickMsg asks the board to re-read the store.
type tickMsg time.Time

// focusMsg is the outcome of one attempted focus.
type focusMsg struct {
	taskID string
	pane   string
	err    error
}

// ticketWriteMsg is the outcome of one ticket write, which runs off the UI
// goroutine like a focus does. Either way the board re-reads the store afterwards.
type ticketWriteMsg struct {
	success string
	failure string
	err     error
	// moved is the ticket a move write is for; it is empty for a create.
	moved string
}

const (
	// ticketTitleLimit mirrors the coordinator's title bound (160 bytes). The
	// board imports nothing from the coordinator, so the prompt keeps its own copy;
	// the coordinator stays the authority and refuses anything longer.
	ticketTitleLimit = 160

	ticketWritesUnavailable = "Ticket writes are unavailable in this session"
)

// Model is the keyboard-driven board. Its selection is a task ID: nothing about
// the display can rename a task into another task.
type Model struct {
	store   *state.Store
	client  Herdr
	project string
	opts    Options
	ctx     context.Context

	// board is the visible projection; sourceBoard is the latest unfiltered project read.
	board        *Board
	sourceBoard  *Board
	searchQuery  string
	searchDraft  string
	searchReturn string // selection restored if search is cleared
	searchOpen   bool
	cursors      []int
	stage        int
	// inTickets says the reader is in the ticket area (the ticket columns, drawn
	// before the five task columns when they are shown); ticketStage and
	// ticketCursors are its position there, as stage and cursors are for tasks.
	inTickets     bool
	ticketStage   int
	ticketCursors []int
	selected      string
	expanded      bool
	scroll        int
	height        int

	width     int
	notice    string
	noticeErr bool
	loadErr   error

	// promptOpen says the new-ticket title prompt is open. While it is, every key
	// edits promptText and nothing navigates, focuses, writes or quits.
	promptOpen bool
	promptText string

	// moving is the set of tickets a move write is in flight for. While a ticket is
	// in it, m on that ticket starts nothing: a repeat would lose the
	// compare-and-swap and its refusal would replace the notice of the move that
	// succeeded. Each ticket is tracked on its own, so a move for another ticket
	// neither is blocked by it nor makes the model forget it.
	moving map[string]bool
}

// NewModel opens the board on one project. It performs the first read itself,
// so a caller can render a loaded board without running a program; a read
// failure is kept and rendered instead of being swallowed.
func NewModel(ctx context.Context, store *state.Store, client Herdr, projectKey string, opts Options) (*Model, error) {
	if store == nil {
		return nil, fmt.Errorf("board: no store")
	}
	if client == nil {
		return nil, fmt.Errorf("board: no Herdr boundary")
	}
	if strings.TrimSpace(projectKey) == "" {
		return nil, fmt.Errorf("board: no project key")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m := &Model{
		store:   store,
		client:  client,
		project: strings.TrimSpace(projectKey),
		opts:    opts.withDefaults(),
		ctx:     ctx,
		cursors: make([]int, len(Columns)),
		// The reader starts in the ticket area. Without tickets to show it falls back
		// to the first task column on the first read.
		ticketCursors: make([]int, len(TicketColumns)),
		inTickets:     true,
		// A width is assumed until the terminal reports its own, so the first
		// frame is already bounded.
		width: minWideWidthFallback,
	}
	m.refresh()
	return m, nil
}

// minWideWidthFallback is the width assumed before tea.WindowSizeMsg arrives.
const minWideWidthFallback = 80

// Init starts the read-only refresh tick.
func (m *Model) Init() tea.Cmd {
	if m.opts.RefreshInterval < 0 {
		return nil
	}
	interval := m.opts.RefreshInterval
	return tea.Tick(interval, func(now time.Time) tea.Msg { return tickMsg(now) })
}

// Update handles terminal size, navigation and the one side-effecting key.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := msg.(type) {
	case tea.WindowSizeMsg:
		if typed.Width > 0 {
			m.width = typed.Width
		}
		if typed.Height > 0 {
			m.height = typed.Height
		}
		m.scrollBy(0)
		return m, nil

	case tea.KeyMsg:
		return m, m.key(typed)

	case tickMsg:
		m.refresh()
		return m, m.Init()

	case focusMsg:
		if typed.err != nil {
			m.notice = fmt.Sprintf("Cannot focus %s: %v", typed.taskID, typed.err)
			m.noticeErr = true
		} else {
			m.notice = "Focused " + typed.pane
			m.noticeErr = false
		}
		m.scrollBy(0)
		return m, nil

	case ticketWriteMsg:
		if typed.moved != "" {
			delete(m.moving, typed.moved)
		}
		if typed.err != nil {
			m.notice = typed.failure + ": " + shortError(typed.err)
			m.noticeErr = true
		} else {
			m.notice = typed.success
			m.noticeErr = false
		}
		m.refresh()
		return m, nil
	}
	return m, nil
}

// key applies one keystroke. Outside this model only two things can change: "f"
// focuses a pane FocusVerified proved, and "n" and "m" create or move a ticket
// through Options.Tickets, which is the human's own keypress and nothing else.
func (m *Model) key(key tea.KeyMsg) tea.Cmd {
	if m.promptOpen {
		return m.promptKey(key)
	}
	if m.searchOpen {
		return m.searchKey(key)
	}
	switch key.Type {
	case tea.KeyCtrlC:
		return tea.Quit

	case tea.KeyEsc:
		if m.expanded {
			m.expanded = false
			m.scroll = 0
			return nil
		}
		return tea.Quit

	case tea.KeyTab:
		if !m.expanded {
			m.walkColumns(1)
		}
		return nil

	case tea.KeyShiftTab:
		if !m.expanded {
			m.walkColumns(-1)
		}
		return nil

	case tea.KeyUp:
		m.step(-1)
		return nil

	case tea.KeyDown:
		m.step(1)
		return nil

	case tea.KeyPgUp:
		m.scrollBy(-m.pageSize())
		return nil

	case tea.KeyPgDown:
		m.scrollBy(m.pageSize())
		return nil

	case tea.KeyHome:
		m.scrollBy(-1 << 30)
		return nil

	case tea.KeyEnd:
		m.scrollBy(1 << 30)
		return nil

	case tea.KeyEnter:
		if m.selected != "" {
			m.expanded = !m.expanded
			m.scroll = 0
		}
		return nil

	case tea.KeyRunes:
		switch string(key.Runes) {
		case "q":
			return tea.Quit
		case "k":
			m.step(-1)
		case "j":
			m.step(1)
		case "f":
			return m.focus()
		case "n":
			return m.openPrompt()
		case "m":
			return m.moveTicket()
		case "r":
			m.notice = ""
			m.noticeErr = false
			m.refresh()
		case "/":
			return m.openSearch()
		}
	}
	return nil
}

// refresh re-reads the project's tasks. The selection survives a refresh by
// task ID, so a card that moved columns is still the selected one.
func (m *Model) refresh() {
	cards, err := ProjectCards(m.ctx, m.store, m.project)
	if err != nil {
		m.loadErr = err
		return
	}
	tickets, err := ProjectTickets(m.ctx, m.store, m.project)
	if err != nil {
		m.loadErr = err
		return
	}
	attention, err := ProjectPrimaryAttention(m.ctx, m.store, m.project)
	if err != nil {
		m.loadErr = err
		return
	}
	m.loadErr = nil
	before := m.selected
	m.sourceBoard = NewBoardWithTickets(cards, tickets).WithPrimaryAttention(attention)
	m.board = filterTaskBoard(m.sourceBoard, m.searchQuery)
	if m.searchQuery != "" {
		m.reselectSearch(before)
	} else {
		m.reselect()
	}
	if m.selected != before {
		m.scroll = 0
	}
	m.scrollBy(0)
}

// reselect keeps the selected card selected when it still exists, and otherwise
// picks the first card of the column the reader is looking at, or the first card
// of the board. A ticket keeps its selection when it moves columns, and the
// reader's position follows it.
func (m *Model) reselect() {
	if !m.showsTickets() {
		m.inTickets = false
	}
	if _, ok := m.board.Find(m.selected); ok {
		return
	}
	if ticket, ok := m.board.FindTicket(m.selected); ok && m.showsTickets() {
		m.followTicket(ticket)
		return
	}
	if m.inTickets {
		if cards := m.board.TicketColumn(TicketColumns[m.ticketStage]); len(cards) > 0 {
			m.ticketCursors[m.ticketStage] = 0
			m.selected = cards[0].ID()
			return
		}
	} else if cards := m.board.Column(Columns[m.stage]); len(cards) > 0 {
		m.cursors[m.stage] = 0
		m.selected = cards[0].ID()
		return
	}
	m.selectFirstCard()
}

// reselectSearch keeps a task or ticket selected when it remains visible. A
// task filtered out by the query falls to the first matching task; an empty
// result has no task selection rather than jumping to an unrelated ticket.
func (m *Model) reselectSearch(previous string) {
	if card, ok := m.board.Find(previous); ok {
		m.selectTaskCard(card)
		return
	}
	if ticket, ok := m.board.FindTicket(previous); ok && m.showsTickets() {
		m.selected = ticket.ID()
		m.followTicket(ticket)
		return
	}
	if m.searchQuery == "" {
		m.reselect()
		return
	}
	cards := m.board.Cards()
	if len(cards) == 0 {
		m.selected = ""
		m.inTickets = false
		return
	}
	m.selectTaskCard(cards[0])
}

func (m *Model) selectTaskCard(card Card) {
	m.selected = card.ID()
	m.inTickets = false
	stage := stageIndex(card.Stage)
	if stage < 0 {
		return
	}
	m.stage = stage
	for i, inColumn := range m.board.Column(card.Stage) {
		if inColumn.ID() == card.ID() {
			m.cursors[stage] = i
			return
		}
	}
}

// followTicket puts the reader's position on the column a ticket is in now.
func (m *Model) followTicket(card TicketCard) {
	stage := ticketColumnIndex(card.Stage)
	if stage < 0 {
		return
	}
	m.inTickets = true
	m.ticketStage = stage
	for i, inColumn := range m.board.TicketColumn(card.Stage) {
		if inColumn.ID() == card.ID() {
			m.ticketCursors[stage] = i
			break
		}
	}
}

// selectFirstCard selects the first card in display order: the first ticket when
// the ticket columns are shown, otherwise the first task of the first column that
// has one.
func (m *Model) selectFirstCard() {
	if m.showsTickets() {
		for stage, column := range TicketColumns {
			if cards := m.board.TicketColumn(column); len(cards) > 0 {
				m.inTickets = true
				m.ticketStage = stage
				m.ticketCursors[stage] = 0
				m.selected = cards[0].ID()
				return
			}
		}
	}
	m.inTickets = false
	for stage := range Columns {
		if column := m.board.Column(Columns[stage]); len(column) > 0 {
			m.stage = stage
			m.cursors[stage] = 0
			m.selected = column[0].ID()
			return
		}
	}
	m.selected = ""
}

// showsTickets reports whether the ticket columns are part of the board the
// reader sees, and so of the cursor space.
func (m *Model) showsTickets() bool { return m.board.showsTickets(m.detail()) }

// walkColumns moves the reader one column through the cursor space: the ticket
// columns when they are shown, then the five task columns, wrapping at both
// ends. It selects the card the reader last had there.
func (m *Model) walkColumns(delta int) {
	ticketColumns := 0
	if m.showsTickets() {
		ticketColumns = len(TicketColumns)
	}
	total := ticketColumns + len(Columns)
	at := ticketColumns + m.stage
	if m.inTickets && ticketColumns > 0 {
		at = m.ticketStage
	}
	at = ((at+delta)%total + total) % total
	if at < ticketColumns {
		m.inTickets = true
		m.ticketStage = at
		m.selected = m.ticketAt(at, m.ticketCursors[at])
	} else {
		m.inTickets = false
		m.stage = at - ticketColumns
		m.selected = m.cardAt(m.stage, m.cursors[m.stage])
	}
	m.expanded = false
	m.scroll = 0
}

// detail is what the board is asked to draw: the selection, the open page, the
// last notice, and whether ticket writes are enabled.
func (m *Model) detail() Detail {
	return Detail{
		SelectedID:   m.selected,
		Expanded:     m.expanded,
		Scroll:       m.scroll,
		Notice:       m.notice,
		Err:          m.noticeErr,
		TicketWrites: m.opts.Tickets != nil,
	}
}

// step moves the card cursor on the board and scrolls the detail page when it
// is open, so the arrows always act on what the reader is looking at.
func (m *Model) step(delta int) {
	if m.expanded {
		m.scrollBy(delta)
		return
	}
	m.moveCursor(delta)
}

// pageSize is how far PgUp/PgDn move the detail page.
func (m *Model) pageSize() int {
	rows, _, _ := pageChrome(m.height, strings.TrimSpace(m.notice) != "")
	if m.searchQuery != "" && rows > 0 {
		rows--
	}
	if rows > 1 {
		return rows - 1
	}
	return 1
}

// scrollBy moves the detail page, clamped to its extent.
func (m *Model) scrollBy(delta int) {
	if !m.expanded {
		return
	}
	lines, ok := pageLinesOf(m.board, m.selected, m.width, m.opts.Theme)
	if !ok {
		return
	}
	total := len(lines)
	max := 0
	rows, _, _ := pageChrome(m.height, strings.TrimSpace(m.notice) != "")
	if m.searchQuery != "" && rows > 0 {
		rows--
	}
	if rows >= 0 && total > rows {
		max = total - rows
	}
	m.scroll += delta
	if m.scroll > max {
		m.scroll = max
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// moveCursor walks the board's cards in display order (the tickets that have a
// column when the ticket columns are shown, then the tasks by column) and stops
// at either end. The reader's column follows the card, so tab continues from
// where the arrows left off.
func (m *Model) moveCursor(delta int) {
	order := m.order()
	if len(order) == 0 {
		m.selected = ""
		return
	}
	index := 0
	for i, id := range order {
		if id == m.selected {
			index = i
			break
		}
	}
	index += delta
	if index < 0 {
		index = 0
	}
	if index >= len(order) {
		index = len(order) - 1
	}
	m.selected = order[index]
	if ticket, ok := m.board.FindTicket(m.selected); ok {
		m.followTicket(ticket)
		return
	}
	m.inTickets = false
	card, _ := m.board.Find(m.selected)
	if stage := stageIndex(card.Stage); stage >= 0 {
		m.stage = stage
		for i, inColumn := range m.board.Column(card.Stage) {
			if inColumn.ID() == card.ID() {
				m.cursors[stage] = i
				break
			}
		}
	}
}

// order is every selectable card's ID in display order.
func (m *Model) order() []string {
	var ids []string
	if m.showsTickets() {
		for _, card := range m.board.Tickets() {
			ids = append(ids, card.ID())
		}
	}
	for _, card := range m.board.Cards() {
		ids = append(ids, card.ID())
	}
	return ids
}

// stageIndex is a stage's column position, or -1 for a stage this build does
// not place.
func stageIndex(stage task.Stage) int {
	for index, column := range Columns {
		if column == stage {
			return index
		}
	}
	return -1
}

// cardAt is the card at one position in a stage, or "" when the stage is empty.
func (m *Model) cardAt(stage, cursor int) string {
	cards := m.board.Column(Columns[stage])
	if len(cards) == 0 {
		return ""
	}
	if cursor < 0 || cursor >= len(cards) {
		cursor = 0
		m.cursors[stage] = 0
	}
	return cards[cursor].ID()
}

// ticketAt is the ticket at one position in a ticket column, or "" when the
// column is empty.
func (m *Model) ticketAt(stage, cursor int) string {
	cards := m.board.TicketColumn(TicketColumns[stage])
	if len(cards) == 0 {
		return ""
	}
	if cursor < 0 || cursor >= len(cards) {
		cursor = 0
		m.ticketCursors[stage] = 0
	}
	return cards[cursor].ID()
}

// SelectedID is the selected task's ID, or "" when no card is selected.
func (m *Model) SelectedID() string { return m.selected }

// focus is the only side-effecting key. The verification happens off the UI
// goroutine, and the task is re-read there: the card the reader sees may
// already be stale.
func (m *Model) focus() tea.Cmd {
	id := m.selected
	if id == "" {
		m.notice = "No task is selected, so there is no worker pane to focus"
		m.noticeErr = true
		return nil
	}
	if _, isTicket := m.board.FindTicket(id); isTicket {
		// A ticket is not a worker: there is no pane to focus and nothing to ask Herdr.
		m.notice = "Tickets have no pane to focus"
		m.noticeErr = true
		return nil
	}
	store, client, ctx := m.store, m.client, m.ctx
	return func() tea.Msg {
		pane, err := FocusVerified(ctx, store, client, id)
		return focusMsg{taskID: id, pane: pane, err: err}
	}
}

// View draws the board at the terminal's current width.
func (m *Model) View() string {
	if m.loadErr != nil {
		return strings.Join([]string{
			clip("The project's tasks could not be read. "+m.loadErr.Error(), m.width),
			clip("r retry, q close", m.width),
		}, "\n")
	}
	d := m.detail()
	var out string
	if m.expanded {
		out = renderPageWithSearch(m.board, d, m.width, m.height, m.opts.Theme, m.searchQuery)
	} else {
		out = renderWithSearch(m.board, d, m.width, m.opts.Theme, m.searchQuery)
	}
	if m.searchOpen {
		out += "\n" + strings.Join(m.searchPromptLines(), "\n")
	} else if m.promptOpen {
		out += "\n" + strings.Join(m.promptLines(), "\n")
	}
	return out
}

func (m *Model) openSearch() tea.Cmd {
	m.expanded = false
	m.scroll = 0
	m.searchOpen = true
	m.searchDraft = m.searchQuery
	m.notice = ""
	m.noticeErr = false
	return nil
}

// openPrompt opens the new-ticket title prompt. The prompt belongs to the board,
// not to a page, so an open details page is closed first.
func (m *Model) openPrompt() tea.Cmd {
	if m.opts.Tickets == nil {
		m.notice = ticketWritesUnavailable
		m.noticeErr = true
		return nil
	}
	m.expanded = false
	m.scroll = 0
	m.promptOpen = true
	m.promptText = ""
	m.notice = ""
	m.noticeErr = false
	return nil
}

// promptKey is every key while the prompt is open: enter submits, esc (or ctrl+c)
// cancels, backspace deletes, and everything else is typed, including the keys
// that mean something on the board (q, j, tab). Nothing here navigates or quits.
func (m *Model) promptKey(key tea.KeyMsg) tea.Cmd {
	switch key.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.promptOpen = false
		m.promptText = ""
	case tea.KeyEnter:
		return m.submitPrompt()
	case tea.KeyBackspace:
		if runes := []rune(m.promptText); len(runes) > 0 {
			m.promptText = string(runes[:len(runes)-1])
		}
	case tea.KeyTab:
		m.typeIntoPrompt("\t")
	case tea.KeySpace:
		m.typeIntoPrompt(" ")
	case tea.KeyRunes:
		m.typeIntoPrompt(string(key.Runes))
	}
	return nil
}

func (m *Model) searchKey(key tea.KeyMsg) tea.Cmd {
	switch key.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.searchOpen = false
		m.searchDraft = ""
	case tea.KeyEnter:
		m.applySearch()
	case tea.KeyBackspace:
		if runes := []rune(m.searchDraft); len(runes) > 0 {
			m.searchDraft = string(runes[:len(runes)-1])
		}
	case tea.KeyCtrlU:
		m.searchDraft = ""
	case tea.KeySpace:
		m.typeIntoSearch(" ")
	case tea.KeyRunes:
		m.typeIntoSearch(string(key.Runes))
	}
	return nil
}

func (m *Model) typeIntoSearch(text string) {
	for _, r := range text {
		if unicode.IsSpace(r) {
			r = ' '
		} else if unsafeRune(r) < 0 {
			continue
		}
		m.searchDraft += string(r)
	}
}

func (m *Model) applySearch() {
	query := strings.TrimSpace(m.searchDraft)
	previous := m.selected
	m.searchOpen = false
	m.searchDraft = ""
	m.expanded = false
	m.scroll = 0
	m.notice = ""
	m.noticeErr = false

	if query == "" {
		m.searchQuery = ""
		m.board = m.sourceBoard
		if m.searchReturn != "" {
			previous = m.searchReturn
		}
		m.searchReturn = ""
	} else {
		if m.searchQuery == "" {
			m.searchReturn = previous
		}
		m.searchQuery = query
		m.board = filterTaskBoard(m.sourceBoard, query)
	}
	m.reselectSearch(previous)
}

func (m *Model) searchPromptLines() []string {
	return []string{
		m.opts.Theme.paint(Focus, clip("Search tasks: "+singleLine(m.searchDraft)+"_", m.width)),
		clip("Enter apply, Esc cancel, Ctrl+U clear", m.width),
	}
}

// typeIntoPrompt appends text, up to the title bound and never in the middle of a
// rune. A tab key is kept as typed (the prompt shows it as ^I). Any other control
// whitespace (newline, carriage return, vertical tab, form feed, next-line) cannot
// be part of a one-line title, so it becomes a space; every other control
// character, such as a pasted escape sequence, and every bidirectional override is
// dropped (see unsafeRune). Text that is merely not ASCII, such as a joiner or a
// no-break space, is kept.
func (m *Model) typeIntoPrompt(text string) {
	for _, r := range text {
		switch {
		case r == '\t':
		case unicode.IsControl(r) && unicode.IsSpace(r):
			r = ' '
		case unsafeRune(r) < 0:
			continue
		}
		if len(m.promptText)+utf8.RuneLen(r) > ticketTitleLimit {
			break
		}
		m.promptText += string(r)
	}
}

// submitPrompt writes the typed title through Options.Tickets, once. A blank
// title is refused with the prompt still open; a real one closes the prompt
// before the write starts, so a second enter cannot submit it again.
func (m *Model) submitPrompt() tea.Cmd {
	title := strings.TrimSpace(m.promptText)
	if title == "" {
		m.notice = "A ticket needs a title"
		m.noticeErr = true
		return nil
	}
	m.promptOpen = false
	m.promptText = ""
	ops, ctx, project := m.opts.Tickets, m.ctx, m.project
	return func() tea.Msg {
		return ticketWriteMsg{
			success: "Created a ticket",
			failure: "Cannot create the ticket",
			err:     ops.CreateTicket(ctx, project, title),
		}
	}
}

// moveTicket moves the selected backlog ticket to todo, sending the digest of the
// card exactly as the board displays it: if the ticket changed since the board
// last read it, the coordinator refuses and the board says so. Anything other
// than a backlog ticket is refused here, before a write is attempted.
func (m *Model) moveTicket() tea.Cmd {
	if m.opts.Tickets == nil {
		m.notice = ticketWritesUnavailable
		m.noticeErr = true
		return nil
	}
	card, ok := m.board.FindTicket(m.selected)
	if !ok || card.Stage != task.TicketBacklog {
		m.notice = "Select a backlog ticket to move it to todo"
		m.noticeErr = true
		return nil
	}
	ops, ctx, project := m.opts.Tickets, m.ctx, m.project
	id, digest := card.ID(), card.Ticket.Digest()
	if m.moving[id] {
		return nil
	}
	if m.moving == nil {
		m.moving = map[string]bool{}
	}
	m.moving[id] = true
	return func() tea.Msg {
		return ticketWriteMsg{
			success: "Moved " + id + " to todo",
			failure: "Cannot move " + id,
			moved:   id,
			err:     ops.MoveTicketToTodo(ctx, project, id, digest),
		}
	}
}

// promptLines is the prompt as drawn under the board. A tab in the title is
// shown as ^I so no raw tab reaches the terminal.
func (m *Model) promptLines() []string {
	shown := strings.ReplaceAll(m.promptText, "\t", "^I")
	return []string{
		m.opts.Theme.paint(Focus, clip("New ticket title: "+shown+"_", m.width)),
		clip("Enter create, Esc cancel", m.width),
	}
}

// shortError is one bounded line of a failure for a notice. The coordinator's
// refusals carry ids and prose; the notice keeps the start, which names the cause.
func shortError(err error) string {
	return clip(singleLine(err.Error()), 120)
}

// Run opens the board as a keyboard-driven Bubble Tea program and blocks until
// the reader closes it (q, Esc, or Ctrl+C).
func Run(ctx context.Context, store *state.Store, client Herdr, projectKey string, opts Options) error {
	model, err := NewModel(ctx, store, client, projectKey, opts)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	programOptions := []tea.ProgramOption{tea.WithContext(ctx)}
	if opts.Input != nil {
		programOptions = append(programOptions, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		programOptions = append(programOptions, tea.WithOutput(opts.Output))
	}
	// The pane is Herdr's chrome: no alternate screen, no mouse, no motion.
	if _, err := tea.NewProgram(model, programOptions...).Run(); err != nil {
		return fmt.Errorf("board: %w", err)
	}
	return nil
}

// FocusVerified re-reads one task, proves against Herdr that its recorded pane
// still hosts the recorded worker, and focuses that pane. It returns the pane it
// focused.
//
// A verified worker is one whose recorded launch pane, pane, checkout, terminal,
// workspace and OMP session are all present, whose session handshake was
// announced and confirmed for that launch pane, and whose live pane still
// matches all of them: a public ID that resolves to a different pane (the record
// is stale), a different terminal, workspace or checkout, a pane that reports no
// working directory, a missing session reference, or a session that the
// confirmed handshake does not correlate with the recorded value are all
// refusals. A task that records none of those facts, or whose handshake was
// never confirmed, never reached a verified worker pane, so it is refused before
// Herdr is called at all.
//
// The session comparison goes through the handshake rather than through
// SameSessionReference: the record holds whichever native identity Herdr
// reported (an id or the session file), and only the announcement carries both.
// The handshake is read once (Store.VerifiedAnnouncement), so the announcement
// and the confirmation that judged it always come from the same row.
//
// Nothing is compared approximately and nothing is skipped when it is absent: a
// refusal focuses nothing, so a stale or under-recorded card can never move the
// user's focus onto somebody else's terminal.
func FocusVerified(ctx context.Context, store *state.Store, client Herdr, taskID string) (string, error) {
	if store == nil || client == nil {
		return "", fmt.Errorf("the board has no store or Herdr boundary")
	}
	fact, err := store.Task(ctx, strings.TrimSpace(taskID))
	if err != nil {
		return "", fmt.Errorf("the task facts could not be read: %w", err)
	}
	paneID := strings.TrimSpace(fact.PublicPaneID)
	launchPaneID := strings.TrimSpace(fact.LaunchPaneID)
	checkout := strings.TrimSpace(fact.WorktreePath)
	terminal := strings.TrimSpace(fact.TerminalID)
	workspace := recordedWorkspace(fact)
	session := strings.TrimSpace(fact.OMPSessionID)
	for _, recorded := range []struct{ field, value string }{
		{"worker pane", paneID},
		{"launch pane", launchPaneID},
		{"checkout", checkout},
		{"terminal", terminal},
		{"workspace", workspace},
		{"OMP session", session},
	} {
		if recorded.value == "" {
			return "", fmt.Errorf("the task records no %s, so it is not a verified worker", recorded.field)
		}
	}

	// The session identity is proved through the handshake, not by comparing the
	// recorded value with Herdr's reference directly: OMP exposes two native
	// identities for one session (an id and the session file), and a record keeps
	// whichever one Herdr reported when the handshake was confirmed. Only the
	// confirmed handshake carries both, so only it can bridge them.
	//
	// The handshake is read once, as one row: pairing an announcement from one
	// read with a confirmation from another would let a refresh that kept the
	// confirmed form (a new id for the same session file) be judged against the
	// stale identity. A row that is absent, never confirmed, or whose confirmation
	// no longer describes its own announcement proves nothing, so the board
	// refuses before it reads anything from Herdr.
	announced, verified, err := store.VerifiedAnnouncement(ctx, launchPaneID)
	if err != nil {
		return "", fmt.Errorf("the verified OMP session handshake for the task's launch pane %s could not be read: %w", launchPaneID, err)
	}
	if !verified {
		return "", fmt.Errorf("the task's launch pane %s has no verified OMP session handshake, so the pane cannot be proved to be the recorded worker", launchPaneID)
	}

	pane, err := client.PaneGet(ctx, paneID)
	if err != nil {
		return "", fmt.Errorf("the recorded worker pane %s could not be read: %w", paneID, err)
	}
	if reported := strings.TrimSpace(pane.PaneID); reported != paneID {
		return "", fmt.Errorf("pane %s now reports itself as pane %s, so the recorded pane is stale", paneID, reported)
	}
	if got := strings.TrimSpace(pane.TerminalID); got != terminal {
		return "", fmt.Errorf("pane %s is on terminal %s, not the recorded terminal %s", paneID, got, terminal)
	}
	if got := strings.TrimSpace(pane.WorkspaceID); got != workspace {
		return "", fmt.Errorf("pane %s is in workspace %s, not the recorded workspace %s", paneID, got, workspace)
	}
	cwd := strings.TrimSpace(pane.Cwd)
	if cwd == "" {
		return "", fmt.Errorf("pane %s reports no working directory, so the recorded checkout %s cannot be proved", paneID, checkout)
	}
	if !task.SameSessionReference(task.SessionKindPath, cwd, checkout) {
		return "", fmt.Errorf("pane %s works in %s, not the recorded checkout %s", paneID, cwd, checkout)
	}

	agent, err := client.AgentGet(ctx, paneID)
	if err != nil {
		return "", fmt.Errorf("the agent on pane %s could not be read: %w", paneID, err)
	}
	if reported := strings.TrimSpace(agent.PaneID); reported != paneID {
		return "", fmt.Errorf("pane %s hosts the agent Herdr reports on pane %s", paneID, reported)
	}

	kind, value, ok, err := client.AgentSession(ctx, paneID)
	if err != nil {
		return "", fmt.Errorf("the OMP session on pane %s could not be read: %w", paneID, err)
	}
	if !ok {
		// The board never assumes a pane is the recorded worker: an unpublished
		// session reference is an identity it cannot prove.
		return "", fmt.Errorf("pane %s reports no OMP session reference, so it cannot be proved to be the recorded worker session %s", paneID, session)
	}
	if !announced.Proves(session, task.SessionRef{Kind: kind, Value: value}) {
		return "", fmt.Errorf("pane %s runs %s session %s, which is not the recorded worker session %s", paneID, kind, value, session)
	}

	focused, err := client.AgentFocus(ctx, paneID)
	if err != nil {
		return "", fmt.Errorf("Herdr refused the focus: %w", err)
	}
	if focused != nil {
		if reported := strings.TrimSpace(focused.PaneID); reported != paneID {
			return "", fmt.Errorf("Herdr reported the focus on pane %s instead of %s", reported, paneID)
		}
	}
	return paneID, nil
}

// recordedWorkspace is the workspace the task's worker is expected to be in.
// The current workspace wins: a move updates it, and the workspace a worktree
// was created in may have been closed by that very move.
func recordedWorkspace(fact task.Task) string {
	if current := strings.TrimSpace(fact.CurrentWorkspaceID); current != "" {
		return current
	}
	return strings.TrimSpace(fact.WorktreeWorkspaceID)
}

// ProjectFor maps the Herdr context a board route was opened from back to the
// one bound project it belongs to: a workspace, a checkout, or a worker's
// worktree. It refuses rather than guessing when nothing or more than one
// project matches.
func ProjectFor(ctx context.Context, store *state.Store, workspaceID, dir string) (task.Project, error) {
	if store == nil {
		return task.Project{}, fmt.Errorf("board: no store")
	}
	workspace := strings.TrimSpace(workspaceID)
	checkout := strings.TrimSpace(dir)
	if workspace == "" && checkout == "" {
		return task.Project{}, fmt.Errorf("board: the invocation names no workspace or working directory to resolve")
	}
	projects, err := store.Projects(ctx)
	if err != nil {
		return task.Project{}, err
	}

	matches := map[string]task.Project{}
	for _, project := range projects {
		switch {
		case workspace != "" && (project.RepoWorkspaceID == workspace || project.CurrentWorkspaceID == workspace):
			matches[project.Key] = project
		case checkout != "" && task.SameSessionReference(task.SessionKindPath, project.Key, checkout):
			matches[project.Key] = project
		}
	}

	// A pane inside a worker's worktree names neither the project's workspace nor
	// its checkout, so the worker's own recorded identities have to resolve it.
	if len(matches) == 0 && (workspace != "" || checkout != "") {
		for _, project := range projects {
			tasks, err := store.Tasks(ctx, project.Key)
			if err != nil {
				return task.Project{}, err
			}
			for _, fact := range tasks {
				switch {
				case workspace != "" && (fact.CurrentWorkspaceID == workspace || fact.WorktreeWorkspaceID == workspace):
					matches[project.Key] = project
				case checkout != "" && task.SameSessionReference(task.SessionKindPath, fact.WorktreePath, checkout):
					matches[project.Key] = project
				}
			}
		}
	}

	switch len(matches) {
	case 1:
		for _, project := range matches {
			return project, nil
		}
	case 0:
		return task.Project{}, fmt.Errorf("board: no bound project matches workspace %q or directory %q; bind the repository first",
			workspace, checkout)
	default:
		keys := make([]string, 0, len(matches))
		for key := range matches {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return task.Project{}, fmt.Errorf("board: %s all match; open the board from inside the project you want", strings.Join(keys, ", "))
	}
	// Unreachable: every branch above returns. The compiler needs a final
	// statement, and returning the empty project would be a lie.
	return task.Project{}, fmt.Errorf("board: no bound project matches the invocation")
}
