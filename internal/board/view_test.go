package board

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"

	"herdr-omp-orchestrator/internal/task"
)

// ---------------------------------------------------------------------------
// projection
// ---------------------------------------------------------------------------

// TestProjectionKeepsFiveColumnsAndNeverDerivesStageFromActivity pins the two
// rules the board exists to keep apart: the columns are exactly the five stages
// task.StageOf projects, and Herdr's activity - including "done" - is carried
// on the card without ever moving it between columns.
func TestProjectionKeepsFiveColumnsAndNeverDerivesStageFromActivity(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	queued := sampleTask("orch_q1", func(x *task.Task) {
		x.Phase = task.Intent
		x.Activity = ""
		x.ActivitySeenAt = 0
	})
	// Herdr reports "done" for this worker's terminal. The task is still
	// Running: a terminal that stopped is not a task that finished.
	running := sampleTask("orch_r2", func(x *task.Task) { x.Activity = "done" })
	needs := sampleTask("orch_n3", func(x *task.Task) { x.Blocker = "Which error contract should this use?" })
	review := sampleTask("orch_v4", func(x *task.Task) {
		x.Report = &task.Report{
			Kind:     "result",
			Summary:  "the parser and its tests are done",
			Branch:   "orch/orch_v4",
			Evidence: []string{"commit 4a5b6c7 on orch/orch_v4", "file internal/parser/parse.go"},
			Checks:   []string{"go test ./internal/parser ok"},
			Risk:     "no coverage for malformed input",
		}
	})
	closed := sampleTask("orch_d5", func(x *task.Task) {
		x.Report = &task.Report{Kind: "result", Summary: "finished"}
		x.Disposition = "user chose to keep the branch"
	})
	f.seed(t, queued, running, needs, review, closed)

	before := f.task(running.ID)
	cards, err := ProjectCards(context.Background(), f.store, f.project)
	if err != nil {
		t.Fatalf("ProjectCards: %v", err)
	}
	if len(cards) != 5 {
		t.Fatalf("cards = %d, want one per task", len(cards))
	}

	want := map[string]task.Stage{
		queued.ID:  task.Queued,
		running.ID: task.Running,
		needs.ID:   task.NeedsInput,
		review.ID:  task.ReadyForReview,
		closed.ID:  task.Done,
	}
	byID := map[string]Card{}
	for _, card := range cards {
		byID[card.ID()] = card
		if card.Task.ID != card.ID() {
			t.Errorf("card %q carries task %q", card.ID(), card.Task.ID)
		}
		if got := want[card.ID()]; got != card.Stage {
			t.Errorf("card %s stage = %q, want %q", card.ID(), card.Stage, got)
		}
		if card.Branch() != "orch/"+card.ID() {
			t.Errorf("card %s branch = %q, want the recorded branch", card.ID(), card.Branch())
		}
		if card.Pane() != samplePane(card.ID()) {
			t.Errorf("card %s pane = %q, want the recorded public pane id", card.ID(), card.Pane())
		}
	}

	if got := byID[running.ID].Activity(); got != "done" {
		t.Errorf("card activity = %q, want Herdr's own word %q", got, "done")
	}
	if got := byID[needs.ID].Attention(); !strings.Contains(got, "Which error contract") {
		t.Errorf("card attention = %q, want the worker's blocker", got)
	}

	board := NewBoard(cards)
	if len(Columns) != 5 {
		t.Fatalf("columns = %d, want 5", len(Columns))
	}
	stages := []task.Stage{task.Queued, task.Running, task.NeedsInput, task.ReadyForReview, task.Done}
	for _, stage := range stages {
		if got := len(board.Column(stage)); got != 1 {
			t.Errorf("column %q holds %d cards, want 1", StageLabel(stage), got)
		}
	}
	if got := board.Column(task.Queued)[0].ID(); got != queued.ID {
		t.Errorf("queued column = %q, want %q", got, queued.ID)
	}

	after := f.task(running.ID)
	if after.Revision != before.Revision || after.UpdatedAt != before.UpdatedAt {
		t.Errorf("the projection wrote to the store: revision %d/%d, updated %d/%d",
			before.Revision, after.Revision, before.UpdatedAt, after.UpdatedAt)
	}
}

// TestProjectionAnUnknownStageIsNotAColumn keeps a card whose facts this build
// cannot place out of the five columns instead of inventing a sixth one.
func TestProjectionAnUnknownStageIsNotAColumn(t *testing.T) {
	board := NewBoard([]Card{{Task: task.Task{ID: "orch_x", Title: "x"}, Stage: task.Stage("unknown")}})
	for _, stage := range Columns {
		if got := len(board.Column(stage)); got != 0 {
			t.Errorf("column %q holds %d cards, want none", StageLabel(stage), got)
		}
	}
	if !strings.Contains(Render(board, Detail{}, 120, Theme{Profile: termenv.Ascii}), "orch_x") {
		t.Error("the card is not rendered anywhere")
	}
}

// ---------------------------------------------------------------------------
// selected task details
// ---------------------------------------------------------------------------

// TestBoardWrapKeepsMultibyteWordsWhole pins the wrap boundary: a word wider
// than the line is cut on rune boundaries, so no line ever carries half a rune.
// Advancing by a byte offset computed from a rune count would split a CJK or
// emoji word and lose or duplicate every rune around the cut.
func TestBoardWrapKeepsMultibyteWordsWhole(t *testing.T) {
	words := []string{
		"日本語日本語日本語日本語日本語",
		strings.Repeat("🎉", 12),
		"a日本b語c日d本e語",
		"日本語🎉日本語🎉日本語",
	}
	for _, word := range words {
		for _, width := range []int{1, 2, 3, 4, 5, 7, 11, 16, 24} {
			lines := wrap(word, width)
			joined := strings.Join(lines, "")
			if !utf8.ValidString(joined) {
				t.Errorf("wrap(%q, %d) is not valid UTF-8: %q", word, width, lines)
			}
			if joined != word {
				t.Errorf("wrap(%q, %d) = %q, want the whole word with no rune lost or duplicated", word, width, joined)
			}
			for _, line := range lines {
				// Only a single rune that cannot fit in the line may be over it.
				if got := runeWidth(line); got > width && utf8.RuneCountInString(line) > 1 {
					t.Errorf("wrap(%q, %d) produced %q, %d columns wide", word, width, line, got)
				}
			}
		}
	}
}

// TestBoardNarrowLayoutKeepsMultibyteTextWhole renders a card whose title, brief
// and blocker are single unspaced wide-script words longer than the pane, and
// checks the board's own output: valid UTF-8, no replacement rune, every input
// rune preserved, and every line inside the width.
func TestBoardNarrowLayoutKeepsMultibyteTextWhole(t *testing.T) {
	title := strings.Repeat("日本語", 8)
	brief := strings.Repeat("🎉", 20)
	blocker := strings.Repeat("日本語🎉", 10)
	card := sampleTask("orch_mb", func(x *task.Task) {
		x.Title = title
		x.Brief = brief
		x.Blocker = blocker
	})
	board := NewBoard([]Card{{Task: card, Stage: task.NeedsInput}})
	const width = 24
	out := Render(board, Detail{SelectedID: card.ID, Expanded: true}, width, Theme{Profile: termenv.Ascii})
	if !utf8.ValidString(out) {
		t.Fatalf("the board rendered invalid UTF-8:\n%q", out)
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Errorf("the board rendered a replacement rune:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if got := runeWidth(line); got > width && utf8.RuneCountInString(line) > 1 {
			t.Errorf("line %q is %d columns wide, want at most %d", line, got, width)
		}
	}

	// The brief and the blocker are wrapped word for word, so the lines they own
	// reconstruct the original text exactly: nothing was lost, duplicated or
	// replaced at a cut. A word wider than the line pushes its label onto its own
	// line, which is why the labels are matched without their trailing space.
	lines := strings.Split(out, "\n")
	if got := wrappedField(t, lines, "Brief:", "Blocked:"); got != brief {
		t.Errorf("the wrapped brief = %q, want %q", got, brief)
	}
	if got := wrappedField(t, lines, "Blocked:", "Last observation:"); got != blocker {
		t.Errorf("the wrapped blocker = %q, want %q", got, blocker)
	}
}

// wrappedField is the text one wrapped label owns: from its own line up to the
// line where the next label begins, joined back together.
func wrappedField(t *testing.T, lines []string, label, next string) string {
	t.Helper()
	start := indexContaining(t, lines, label)
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], next) {
			end = i
			break
		}
	}
	if end == len(lines) {
		t.Fatalf("no line after %q begins with %q:\n%s", label, next, strings.Join(lines, "\n"))
	}
	return strings.TrimPrefix(strings.Join(lines[start:end], ""), label)
}

// TestBoardWideRunesAndNewlinesStayOnOneBoundedLine keeps the bound true for the
// text a user actually writes: a title in a wide script takes two columns per
// rune, and an embedded newline must not become an extra board line.
func TestBoardWideRunesAndNewlinesStayOnOneBoundedLine(t *testing.T) {
	card := sampleTask("orch_w2", func(x *task.Task) {
		x.Title = "日本語のタスク\nsecond line"
		x.Brief = "fix the 日本語 parser\nand its tests"
		x.Blocker = "which 例外 contract?"
	})
	board := NewBoard([]Card{{Task: card, Stage: task.NeedsInput}})

	for _, width := range []int{24, 40, 80, 120} {
		out := Render(board, Detail{SelectedID: card.ID, Expanded: true}, width, Theme{Profile: termenv.Ascii})
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			if got := runeWidth(line); got > width {
				t.Errorf("width %d: line %q is %d columns wide", width, line, got)
			}
		}
		if width < 60 || width >= MinWideWidth {
			// The bound is what matters at every width; the content checks below
			// need a narrow section, where a card gets the whole line.
			continue
		}
		if got := strings.Count(out, "second line"); got != 1 {
			t.Errorf("width %d: a newline in the title produced %d lines", width, got)
		}
		cardLine := lineContaining(t, lines, "["+card.ID+"]")
		if !strings.Contains(cardLine, "日本語") || !strings.Contains(cardLine, "second line") {
			t.Errorf("width %d: the collapsed title did not fit one line: %q", width, cardLine)
		}
		if !strings.Contains(out, "Brief: fix the 日本語 parser and its tests") {
			t.Errorf("width %d: the brief is not shown as one wrapped sentence:\n%s", width, out)
		}
		if !strings.Contains(out, "Blocked: which 例外 contract?") {
			t.Errorf("width %d: the blocker is not shown as one line:\n%s", width, out)
		}
	}
}

// TestBoardSelectionShowsTheTaskFacts covers the drill-down: the selected card
// is identified by its task ID (never by a mutable label) and the details show
// the brief, the branch, the report with its evidence, and the last Herdr
// observation - in the collapsed and expanded forms.
func TestBoardSelectionShowsTheTaskFacts(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	observed := sampleTask("orch_r2", func(x *task.Task) {
		x.Brief = "implement the parser and its tests"
		x.Report = &task.Report{
			Kind:     "result",
			Summary:  "the parser and its tests are done",
			Branch:   "orch/orch_r2",
			Evidence: []string{"commit 4a5b6c7 on orch/orch_r2", "file internal/parser/parse.go"},
			Checks:   []string{"go test ./internal/parser ok"},
			Risk:     "no coverage for malformed input",
		}
		x.Activity = "working"
		x.ActivitySeenAt = time.Date(2026, 9, 25, 11, 59, 0, 0, time.UTC).UnixMilli()
	})
	f.seed(t, observed)

	cards, err := ProjectCards(context.Background(), f.store, f.project)
	if err != nil {
		t.Fatalf("ProjectCards: %v", err)
	}
	board := NewBoard(cards)

	collapsed := Render(board, Detail{SelectedID: observed.ID}, 120, Theme{Profile: termenv.Ascii})
	for _, want := range []string{
		observed.ID,
		"branch orch/orch_r2",
		"the parser and its tests are done",
		"Enter details",
		"f focus",
		"q close",
	} {
		if !strings.Contains(collapsed, want) {
			t.Errorf("the collapsed board does not show %q:\n%s", want, collapsed)
		}
	}

	expanded := Render(board, Detail{SelectedID: observed.ID, Expanded: true}, 120, Theme{Profile: termenv.Ascii})
	for _, want := range []string{
		observed.ID,
		"branch orch/orch_r2",
		"Brief: implement the parser and its tests",
		"Report: result",
		"the parser and its tests are done",
		"Evidence: commit 4a5b6c7 on orch/orch_r2, file internal/parser/parse.go",
		"Checks: go test ./internal/parser ok",
		"Risk: no coverage for malformed input",
		"Last observation: working at 2026-09-25 11:59",
		"Esc collapse",
	} {
		if !strings.Contains(expanded, want) {
			t.Errorf("the expanded board does not show %q:\n%s", want, expanded)
		}
	}

	// The task's title is not its identity: a card still renders after the
	// title changes, and the details keep naming the ID.
	renamed, err := f.store.UpdateTask(context.Background(), observed.ID, f.task(observed.ID).Revision,
		func(next *task.Task) error { next.Title = "a different mutable label"; return nil })
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	fresh, err := ProjectCards(context.Background(), f.store, f.project)
	if err != nil {
		t.Fatalf("ProjectCards after the rename: %v", err)
	}
	if got := Render(NewBoard(fresh), Detail{SelectedID: renamed.ID, Expanded: true}, 120, Theme{Profile: termenv.Ascii}); !strings.Contains(got, "Brief: implement the parser") {
		t.Errorf("the renamed task lost its details:\n%s", got)
	}
}

// TestBoardEmptyStateNamesTheWayOut covers the board with nothing to show.
func TestBoardEmptyStateNamesTheWayOut(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t)
	out := Render(NewBoard(nil), Detail{}, 100, Theme{Profile: termenv.Ascii})
	if !strings.Contains(out, "Ask your primary to delegate a task") {
		t.Errorf("the empty board does not name the way out:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// layout
// ---------------------------------------------------------------------------

// TestNarrowLayoutUsesOneSectionPerStageAtEightyColumns pins the compact
// layout: at 80 columns the five stages become single-column sections with
// their own headings, activity stays a separate indicator, and no rendered line
// is wider than the terminal.
func TestNarrowLayoutUsesOneSectionPerStageAtEightyColumns(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	queued := sampleTask("orch_q1", func(x *task.Task) { x.Phase = task.Intent })
	running := sampleTask("orch_r2", func(x *task.Task) { x.Activity = "done" })
	second := sampleTask("orch_r3", nil)
	needs := sampleTask("orch_n4", func(x *task.Task) { x.Blocker = "Which error contract should this use?" })
	review := sampleTask("orch_v5", func(x *task.Task) {
		x.Report = &task.Report{Kind: "result", Summary: "done", Branch: "orch/orch_v5"}
	})
	closed := sampleTask("orch_d6", func(x *task.Task) { x.Disposition = "kept" })
	f.seed(t, queued, running, second, needs, review, closed)

	cards, err := ProjectCards(context.Background(), f.store, f.project)
	if err != nil {
		t.Fatalf("ProjectCards: %v", err)
	}
	const width = 80
	out := Render(NewBoard(cards), Detail{SelectedID: needs.ID, Expanded: true}, width, Theme{Profile: termenv.Ascii})

	for _, want := range []string{
		StageLabel(task.Queued) + " (1)",
		StageLabel(task.Running) + " (2)",
		StageLabel(task.NeedsInput) + " (1)",
		StageLabel(task.ReadyForReview) + " (1)",
		StageLabel(task.Done) + " (1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the narrow board does not show the section %q:\n%s", want, out)
		}
	}

	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if got := runeWidth(line); got > width {
			t.Errorf("line %q is %d columns wide, want at most %d", line, got, width)
		}
	}
	// One section per line: two stage headings never share a line, and a
	// heading is never truncated.
	for _, line := range lines {
		seen := 0
		for _, stage := range Columns {
			if strings.Contains(line, StageLabel(stage)) {
				seen++
			}
		}
		if seen > 1 {
			t.Errorf("line %q carries %d stage headings, want one column of stages", line, seen)
		}
	}
	// The running card keeps Herdr's activity word while its section stays
	// Running.
	runningLine := lineContaining(t, lines, "["+running.ID+"]")
	if !strings.Contains(runningLine, "done") {
		t.Errorf("the running card %q lost Herdr's activity", runningLine)
	}
	if !strings.Contains(lineContaining(t, lines, StageLabel(task.Running)), StageLabel(task.Running)) {
		t.Errorf("the running card is not under its own section")
	}
	// A card sits under its own section heading, not another stage's.
	needsSection := indexContaining(t, lines, StageLabel(task.NeedsInput)+" (1)")
	needsCard := indexContaining(t, lines, "["+needs.ID+"]")
	reviewSection := indexContaining(t, lines, StageLabel(task.ReadyForReview)+" (1)")
	if !(needsSection < needsCard && needsCard < reviewSection) {
		t.Errorf("the needs-input card is not inside its section (%d, %d, %d)", needsSection, needsCard, reviewSection)
	}
}

// TestNarrowLayoutBoundsEveryLineAtSmallWidths keeps the bound when the
// terminal is too narrow even for a heading.
func TestNarrowLayoutBoundsEveryLineAtSmallWidths(t *testing.T) {
	card := sampleTask("orch_r2", func(x *task.Task) {
		x.Brief = strings.Repeat("a rather long brief that must be wrapped ", 6)
	})
	for _, width := range []int{1, 12, 24, 40, 60, 83} {
		out := Render(NewBoard([]Card{{Task: card, Stage: task.Running}}), Detail{SelectedID: card.ID, Expanded: true}, width, Theme{Profile: termenv.Ascii})
		for _, line := range strings.Split(out, "\n") {
			if got := runeWidth(line); got > width {
				t.Errorf("width %d: line %q is %d columns wide", width, line, got)
			}
		}
	}
}

// TestBoardWideLayoutSharesOneHeaderRow pins the wide layout: five stage columns on
// one header row, and the bound still holds with color on (escape sequences are
// not columns).
func TestBoardWideLayoutSharesOneHeaderRow(t *testing.T) {
	card := sampleTask("orch_r2", nil)
	board := NewBoard([]Card{{Task: card, Stage: task.Running}})
	out := Render(board, Detail{SelectedID: card.ID}, 100, Theme{Profile: termenv.TrueColor, Unicode: true})
	header := strings.Split(out, "\n")[0]
	for _, stage := range Columns {
		if !strings.Contains(header, StageLabel(stage)) {
			t.Errorf("the header %q does not name %q", header, StageLabel(stage))
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if got := visibleWidth(line); got > 100 {
			t.Errorf("line %q is %d columns wide, want at most 100", line, got)
		}
	}
}

// ---------------------------------------------------------------------------
// theme
// ---------------------------------------------------------------------------

// TestBoardColorsFollowTheTheme pins the fallback palette: a terminal without
// color and without Unicode gets an ASCII board with no escape sequences at
// all, and a truecolor terminal gets the documented accents - with attention
// the only strong accent on a card.
func TestBoardColorsFollowTheTheme(t *testing.T) {
	needs := sampleTask("orch_n3", func(x *task.Task) { x.Blocker = "which error contract?" })
	running := sampleTask("orch_r2", nil)
	board := NewBoard([]Card{
		{Task: needs, Stage: task.NeedsInput},
		{Task: running, Stage: task.Running},
	})

	plain := Render(board, Detail{SelectedID: running.ID, Expanded: true}, 60, Theme{Profile: termenv.Ascii})
	if strings.Contains(plain, "\x1b") {
		t.Errorf("the ASCII fallback emitted an escape sequence:\n%q", plain)
	}
	for _, glyph := range []string{"─", "·", "•"} {
		if strings.Contains(plain, glyph) {
			t.Errorf("the ASCII fallback emitted the glyph %q:\n%s", glyph, plain)
		}
	}

	loud := Render(board, Detail{SelectedID: running.ID, Expanded: true}, 60, Theme{Profile: termenv.TrueColor, Unicode: true})
	if !strings.Contains(loud, "\x1b[") {
		t.Error("a truecolor terminal got no color at all")
	}
	if !strings.Contains(lineContaining(t, strings.Split(loud, "\n"), "["+needs.ID+"]"), attentionEscape) {
		t.Errorf("the needs-input card is not drawn in the attention color %s:\n%q", Attention, loud)
	}
	if strings.Contains(lineContaining(t, strings.Split(loud, "\n"), "["+running.ID+"]"), attentionEscape) {
		t.Errorf("a running card carries the attention accent:\n%q", loud)
	}
	if !strings.Contains(lineContaining(t, strings.Split(loud, "\n"), "["+running.ID+"]"), focusEscape) {
		t.Errorf("the selected card is not drawn in the focus color %s:\n%q", Focus, loud)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

// The 24-bit foreground sequences termenv emits for two palette entries:
// attention #E6A23C and focus #4EA8DE. They are spelled out so the test fails
// if the palette changes silently.
const (
	attentionEscape = "\x1b[38;2;230;162;60m"
	focusEscape     = "\x1b[38;2;78;168;222m"
)

// visibleWidth counts the columns a rendered line occupies, ignoring color.
func visibleWidth(line string) int {
	return runewidth.StringWidth(ansiEscape.ReplaceAllString(line, ""))
}

// runeWidth is the same count for an uncolored line.
func runeWidth(line string) int { return runewidth.StringWidth(line) }

func lineContaining(t *testing.T, lines []string, want string) string {
	t.Helper()
	index := indexContaining(t, lines, want)
	return lines[index]
}

func indexContaining(t *testing.T, lines []string, want string) int {
	t.Helper()
	for i, line := range lines {
		if strings.Contains(line, want) {
			return i
		}
	}
	t.Fatalf("no rendered line carries %q:\n%s", want, strings.Join(lines, "\n"))
	return -1
}
