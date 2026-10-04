package board

// Issue #17: the board marks a primary that is absent or unproven. The marker is
// a presentation of one persisted fact (task.Project.PrimaryAttention): drawing
// it reads the store, calls no Herdr command, writes nothing and moves no card.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

const sampleAbsence = "the recorded primary pane w1:p1 is absent from Herdr's snapshot"

// markerCards is a board with one card per interesting stage, so a marker that
// disturbed a column would show.
func markerCards() []Card {
	return []Card{
		{Task: sampleTask("orch_r2", nil), Stage: task.Running},
		{Task: sampleTask("orch_n3", func(x *task.Task) { x.Blocker = "which error contract?" }), Stage: task.NeedsInput},
	}
}

func TestProjectPrimaryAttentionReadsTheRecordedFact(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_r2", nil))
	ctx := context.Background()

	if got, err := ProjectPrimaryAttention(ctx, f.store, f.project); err != nil || got != "" {
		t.Fatalf("a project with nothing recorded = (%q, %v), want none", got, err)
	}
	if err := f.store.UpdateProject(ctx, f.project, func(p *task.Project) error {
		p.PrimaryAttention = "  " + sampleAbsence + "\n"
		return nil
	}); err != nil {
		t.Fatalf("record the primary attention: %v", err)
	}
	if got, err := ProjectPrimaryAttention(ctx, f.store, f.project); err != nil || got != sampleAbsence {
		t.Fatalf("ProjectPrimaryAttention = (%q, %v), want the trimmed reason", got, err)
	}
	// The board is opened on a bound project: one the store does not know is a
	// read error, never "nothing recorded".
	if got, err := ProjectPrimaryAttention(ctx, f.store, `C:\work\not-bound`); !errors.Is(err, state.ErrNotFound) || got != "" {
		t.Fatalf("an unknown project = (%q, %v), want state.ErrNotFound", got, err)
	}
	if _, err := ProjectPrimaryAttention(ctx, nil, f.project); err == nil {
		t.Fatal("a nil store was accepted")
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("reading the fact made Herdr calls: %v", calls)
	}
}

func TestPrimaryMarkerHeadsEveryLayoutWithinTheWidth(t *testing.T) {
	themes := map[string]Theme{
		"ascii, no color": {Profile: termenv.Ascii},
		"color":           {Profile: termenv.TrueColor, Unicode: true},
	}
	for name, th := range themes {
		for _, width := range []int{120, 100, 80, 60, 40, 24, 12, 4, 1} {
			marked := Render(NewBoard(markerCards()).WithPrimaryAttention(sampleAbsence), Detail{SelectedID: "orch_r2"}, width, th)
			lines := strings.Split(marked, "\n")
			if !utf8.ValidString(marked) {
				t.Fatalf("%s, width %d: invalid UTF-8", name, width)
			}
			for _, line := range lines {
				if got := visibleWidth(line); got > width {
					t.Errorf("%s, width %d: line %q is %d columns wide", name, width, line, got)
				}
			}
			block := markerLines(marked)
			if width >= 12 && (len(block) == 0 || !strings.HasPrefix(block[0], "Primary not")) {
				t.Errorf("%s, width %d: the board does not open with the marker, head %q", name, width, block)
			}
			// The marker block itself - not the rest of the board - carries the
			// reason wherever the width allows a readable line.
			if flat := strings.Join(strings.Fields(strings.Join(block, " ")), " "); width >= 24 && !strings.Contains(flat, "absent from Herdr's snapshot") {
				t.Errorf("%s, width %d: the marker does not carry the reason:\n%s", name, width, marked)
			}
		}
	}
}

func TestPrimaryMarkerIsLegibleWithoutColorOrGlyphs(t *testing.T) {
	plain := Render(NewBoard(markerCards()).WithPrimaryAttention(sampleAbsence), Detail{}, 100, Theme{Profile: termenv.Ascii})
	if strings.Contains(plain, "\x1b") {
		t.Errorf("the no-color board emitted an escape sequence:\n%q", plain)
	}
	for _, glyph := range []string{"─", "·", "•", "⚠", "…"} {
		if strings.Contains(plain, glyph) {
			t.Errorf("the ASCII board emitted the glyph %q:\n%s", glyph, plain)
		}
	}
	if !strings.HasPrefix(plain, "Primary not verified: the recorded primary pane w1:p1 is absent") {
		t.Errorf("the marker is not plain words at the head of the board:\n%s", plain)
	}

	loud := Render(NewBoard(markerCards()).WithPrimaryAttention(sampleAbsence), Detail{}, 100, Theme{Profile: termenv.TrueColor, Unicode: true})
	if first := strings.Split(loud, "\n")[0]; !strings.Contains(first, attentionEscape) {
		t.Errorf("the marker is not drawn in the attention color %s:\n%q", Attention, first)
	}
}

func TestPrimaryMarkerMovesNoCardAndAddsNothingWhenHealthy(t *testing.T) {
	th := Theme{Profile: termenv.Ascii}
	for _, width := range []int{120, 100, 80, 40, 24} {
		for _, d := range []Detail{{}, {SelectedID: "orch_r2"}, {SelectedID: "orch_n3", Expanded: true}, {TicketWrites: true}} {
			bare := Render(NewBoard(markerCards()), d, width, th)
			if cleared := Render(NewBoard(markerCards()).WithPrimaryAttention("  "), d, width, th); cleared != bare {
				t.Fatalf("width %d: a blank attention changed the board:\n%s\n--- vs ---\n%s", width, cleared, bare)
			}
			marked := Render(NewBoard(markerCards()).WithPrimaryAttention(sampleAbsence), d, width, th)
			// Everything below the marker is exactly the board without it: no card
			// changed column, text, selection or detail.
			if !strings.HasSuffix(marked, "\n\n"+bare) {
				t.Fatalf("width %d: the marker changed the board under it:\n%s\n--- bare ---\n%s", width, marked, bare)
			}
		}
	}
	board := NewBoard(markerCards()).WithPrimaryAttention(sampleAbsence)
	for _, stage := range Columns {
		if got, want := len(board.Column(stage)), len(NewBoard(markerCards()).Column(stage)); got != want {
			t.Errorf("column %q holds %d cards with the marker, want %d", StageLabel(stage), got, want)
		}
	}
	if nilBoard := (*Board)(nil); nilBoard.WithPrimaryAttention(sampleAbsence) != nil || nilBoard.PrimaryAttention() != "" {
		t.Error("a nil board grew a marker")
	}
}

func TestPrimaryMarkerShowsOnABoardWithNothingElse(t *testing.T) {
	th := Theme{Profile: termenv.Ascii}
	empty := Render(NewBoard(nil).WithPrimaryAttention(sampleAbsence), Detail{}, 80, th)
	if !strings.HasPrefix(empty, "Primary not verified: ") || !strings.Contains(empty, "No tasks yet") {
		t.Errorf("an empty board hides the marker or its own message:\n%s", empty)
	}
	ticketsOnly := Render(NewBoardWithTickets(nil, []TicketCard{ticketCardOf("tkt_a1", "first", task.TicketBacklog)}).WithPrimaryAttention(sampleAbsence), Detail{}, 100, th)
	if !strings.HasPrefix(ticketsOnly, "Primary not verified: ") || !strings.Contains(ticketsOnly, "[tkt_a1] first") {
		t.Errorf("a board with only tickets hides the marker or a ticket:\n%s", ticketsOnly)
	}
}

func TestPrimaryMarkerIsBoundedAndSanitized(t *testing.T) {
	th := Theme{Profile: termenv.Ascii}
	long := strings.Repeat("a very long diagnostic ", 80)
	for _, width := range []int{80, 24, 10} {
		out := Render(NewBoard(markerCards()).WithPrimaryAttention(long), Detail{}, width, th)
		lines := strings.Split(out, "\n")
		blank := -1
		for i, line := range lines {
			if line == "" {
				blank = i
				break
			}
		}
		if blank < 0 {
			t.Fatalf("width %d: the marker is not followed by a blank line:\n%s", width, out)
		}
		if blank > primaryMarkerMaxLines {
			t.Errorf("width %d: the marker is %d lines, want at most %d:\n%s", width, blank, primaryMarkerMaxLines, out)
		}
		if last := lines[primaryMarkerMaxLines-1]; !strings.HasSuffix(last, "...") {
			t.Errorf("width %d: a cut reason does not say so: %q", width, last)
		}
		for _, line := range lines {
			if got := runeWidth(line); got > width {
				t.Errorf("width %d: line %q is %d columns wide", width, line, got)
			}
		}
	}

	hostile := "pane gone \x1b]0;owned\x07 \x1b[31mred\x1b[0m \u202etxet"
	out := Render(NewBoard(nil).WithPrimaryAttention(hostile), Detail{}, 100, th)
	for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Errorf("a recorded reason reached the terminal with %q in it:\n%q", bad, out)
		}
	}
}

func TestBoardModelShowsAndRetiresThePrimaryMarkerFromTheStore(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	running := sampleTask("orch_r2", nil)
	f.seed(t, running)
	m := f.model(t, Options{})
	before := f.snapshot()
	project, err := f.store.Project(context.Background(), f.project)
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}

	if strings.Contains(m.View(), "Primary not verified") {
		t.Fatalf("a project with nothing recorded is marked:\n%s", m.View())
	}

	record := func(reason string) {
		t.Helper()
		if err := f.store.UpdateProject(context.Background(), f.project, func(p *task.Project) error {
			p.PrimaryAttention = reason
			return nil
		}); err != nil {
			t.Fatalf("record the primary attention: %v", err)
		}
		m.refresh()
	}

	record(sampleAbsence)
	view := m.View()
	if !strings.HasPrefix(view, "Primary not verified: the recorded primary pane w1:p1 is absent") {
		t.Fatalf("the board does not mark the absent primary:\n%s", view)
	}
	if !strings.Contains(view, "[orch_r2]") {
		t.Fatalf("the marker hid the worker:\n%s", view)
	}
	if card, ok := m.board.Find(running.ID); !ok || card.Stage != task.Running {
		t.Fatalf("the absent primary moved the worker out of %q: %+v", task.Running, card)
	}

	record("")
	if strings.Contains(m.View(), "Primary not verified") {
		t.Fatalf("the marker outlived the fact:\n%s", m.View())
	}

	// Reading the fact is all the board did: no task changed, the project kept
	// every other fact, and Herdr was never asked anything.
	if after := f.snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("showing the marker wrote to a task:\n got %+v\nwant %+v", after, before)
	}
	if now, err := f.store.Project(context.Background(), f.project); err != nil || !reflect.DeepEqual(now, project) {
		t.Fatalf("the project after the marker came and went = (%+v, %v), want %+v", now, err, project)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("the marker made Herdr calls: %v", calls)
	}
}

// markerLines is the marker block at the head of a render: its lines up to the
// first blank one, colour stripped.
func markerLines(out string) []string {
	var block []string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			break
		}
		block = append(block, ansiEscape.ReplaceAllString(line, ""))
	}
	return block
}

// The expanded task page replaces the board, so it must carry the marker too: a
// reader who opens a worker still sees that the primary is not verified.
func TestPrimaryMarkerLeadsTheDetailPage(t *testing.T) {
	th := Theme{Profile: termenv.Ascii}
	for _, selected := range []string{"orch_r2", "tkt_a1"} {
		for _, width := range []int{100, 40, 24} {
			b := func(reason string) *Board {
				return NewBoardWithTickets(markerCards(), []TicketCard{ticketCardOf("tkt_a1", "first", task.TicketBacklog)}).WithPrimaryAttention(reason)
			}
			d := Detail{SelectedID: selected, Expanded: true}
			bare := RenderPage(b(""), d, width, 0, th)
			marked := RenderPage(b(sampleAbsence), d, width, 0, th)

			block := markerLines(marked)
			if len(block) == 0 || !strings.HasPrefix(block[0], "Primary not verified:") {
				t.Fatalf("%s, width %d: the page does not open with the marker:\n%s", selected, width, marked)
			}
			if flat := strings.Join(strings.Fields(strings.Join(block, " ")), " "); !strings.Contains(flat, "absent from Herdr's snapshot") {
				t.Errorf("%s, width %d: the page marker does not carry the reason:\n%s", selected, width, marked)
			}
			if !strings.HasSuffix(marked, "\n\n"+bare) {
				t.Errorf("%s, width %d: the marker changed the page under it:\n%s\n--- bare ---\n%s", selected, width, marked, bare)
			}
			for _, line := range strings.Split(marked, "\n") {
				if got := runeWidth(line); got > width {
					t.Errorf("%s, width %d: line %q is %d columns wide", selected, width, line, got)
				}
			}
		}
	}

	gone := RenderPage(NewBoard(markerCards()).WithPrimaryAttention(sampleAbsence), Detail{SelectedID: "orch_missing"}, 60, 0, th)
	if !strings.HasPrefix(gone, "Primary not verified: ") || !strings.Contains(gone, "no longer on this board") {
		t.Errorf("a page for a task that left the board hides the marker or its own message:\n%s", gone)
	}
}

// The page and its scroll clamp must measure the same lines: with the marker at
// the head of the page, End reaches the last line of the task's page and Home the
// first line of the marker.
func TestPrimaryMarkerKeepsTheDetailPageScrollConsistent(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_r2", func(x *task.Task) { x.Brief = strings.Repeat("a rather long brief that wraps ", 12) }))
	if err := f.store.UpdateProject(context.Background(), f.project, func(p *task.Project) error {
		p.PrimaryAttention = sampleAbsence
		return nil
	}); err != nil {
		t.Fatalf("record the primary attention: %v", err)
	}
	m := f.model(t, Options{})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.expanded {
		t.Fatal("the details page did not open")
	}

	lines, ok := pageLinesOf(m.board, m.selected, m.width, m.opts.Theme)
	if !ok {
		t.Fatal("the selected task has no page")
	}
	rows, _, _ := pageChrome(m.height, false)
	if len(lines) <= rows {
		t.Fatalf("the page (%d lines) fits in %d rows, so it proves nothing about scrolling", len(lines), rows)
	}
	if !strings.HasPrefix(lines[0], "Primary not verified:") {
		t.Fatalf("the measured page does not start with the marker: %q", lines[0])
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if want := len(lines) - rows; m.scroll != want {
		t.Fatalf("scroll after End = %d, want %d (the page is %d lines in %d rows)", m.scroll, want, len(lines), rows)
	}
	if got := strings.Split(m.View(), "\n")[rows-1]; got != lines[len(lines)-1] {
		t.Fatalf("the last visible line after End = %q, want the last line of the page %q", got, lines[len(lines)-1])
	}

	m.Update(tea.KeyMsg{Type: tea.KeyHome})
	if got := strings.Split(m.View(), "\n")[0]; got != lines[0] {
		t.Fatalf("the first visible line after Home = %q, want the marker %q", got, lines[0])
	}
}
