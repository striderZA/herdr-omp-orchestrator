package board

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/muesli/termenv"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// reviewWaited is the wait the tests measure: two hours and thirteen minutes
// after the entry.
const reviewWaited = 2*time.Hour + 13*time.Minute

// enteredReview is the instant the render tests' history records a card
// entering review.
var enteredReview = time.Date(2026, 10, 10, 14, 8, 0, 0, time.UTC)

// waitLine is the detail line a card entered at enteredReview shows after reviewWaited.
const waitLine = "Waiting for review: 2h 13m (since 2026-10-10 14:08 UTC)"

// stageRow is one recorded stage change, as a stage-changing write leaves it.
func stageRow(seq, at int64, from, to task.Stage) state.Transition {
	return state.Transition{Seq: seq, Kind: state.HistoryStage, From: string(from), To: string(to), At: at}
}

// resultReport is the final report that moves a task into ready_for_review.
func resultReport() *task.Report {
	return &task.Report{Kind: "result", Summary: "the change is done"}
}

// TestReviewWaitIsReadFromTheNewestStageRow pins which history proves a wait: the
// newest stage row in record order, and only when that row enters the card's stage.
func TestReviewWaitIsReadFromTheNewestStageRow(t *testing.T) {
	asOf := time.UnixMilli(10_000)
	known := func(ms int64) ReviewWait {
		since := time.UnixMilli(ms)
		return ReviewWait{Known: true, Since: since, Waited: asOf.Sub(since)}
	}
	cases := []struct {
		name    string
		history []state.Transition
		want    ReviewWait
	}{
		{"a stage row that enters review gives its time", []state.Transition{
			{Seq: 1, Kind: state.HistoryCreated, To: string(task.Queued), At: 100},
			stageRow(2, 200, task.Queued, task.Running),
			stageRow(3, 300, task.Running, task.ReadyForReview),
		}, known(300)},
		{"after rework the newest entry is shown, not the first", []state.Transition{
			stageRow(1, 300, task.Running, task.ReadyForReview),
			stageRow(2, 400, task.ReadyForReview, task.Running),
			stageRow(3, 500, task.Running, task.ReadyForReview),
		}, known(500)},
		{"record order decides, not the clock", []state.Transition{
			stageRow(1, 500, task.Running, task.ReadyForReview),
			stageRow(2, 400, task.ReadyForReview, task.Running),
		}, ReviewWait{}},
		{"a newest stage row into another stage is unknown", []state.Transition{
			stageRow(1, 300, task.Running, task.ReadyForReview),
			stageRow(2, 400, task.ReadyForReview, task.NeedsInput),
		}, ReviewWait{}},
		{"no stage row is unknown", []state.Transition{
			{Seq: 1, Kind: state.HistoryCreated, To: string(task.Queued), At: 100},
			{Seq: 2, Kind: state.HistoryReport, To: "result", At: 200},
		}, ReviewWait{}},
		{"no history is unknown", nil, ReviewWait{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewWaitFrom(tc.history, task.ReadyForReview, asOf)
			if got.Known != tc.want.Known || !got.Since.Equal(tc.want.Since) || got.Waited != tc.want.Waited {
				t.Errorf("reviewWaitFrom = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestReviewWaitOnlyJudgesReadyForReview: the review wait is a review fact. A
// stage row into any other stage never yields a known wait, even though it is the
// newest row and matches that stage.
func TestReviewWaitOnlyJudgesReadyForReview(t *testing.T) {
	history := []state.Transition{stageRow(1, 300, task.Queued, task.Running)}
	if got := reviewWaitFrom(history, task.Running, time.UnixMilli(10_000)); got.Known {
		t.Errorf("a running card has a review wait: %+v", got)
	}
}

// TestFormatWaitedStatesTheTwoLargestUnits keeps the wait short enough for every
// layout and never negative.
func TestFormatWaitedStatesTheTwoLargestUnits(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Minute:                    "under a minute", // a row stamped after its read
		30 * time.Second:                "under a minute",
		time.Minute:                     "1m",
		59*time.Minute + 59*time.Second: "59m",
		time.Hour:                       "1h 0m",
		reviewWaited:                    "2h 13m",
		24 * time.Hour:                  "1d 0h",
		49*time.Hour + 30*time.Minute:   "2d 1h",
	}
	for d, want := range cases {
		if got := formatWaited(d); got != want {
			t.Errorf("formatWaited(%v) = %q, want %q", d, got, want)
		}
	}
}

// TestWaitLineIsInEveryDetailView: the collapsed panel, the expanded panel and the
// page of a card in review show its wait, or unknown. No other card shows one.
func TestWaitLineIsInEveryDetailView(t *testing.T) {
	th := Theme{Profile: termenv.Ascii}
	known := Card{
		Task:   sampleTask("orch_w1", func(x *task.Task) { x.Report = resultReport() }),
		Stage:  task.ReadyForReview,
		Review: ReviewWait{Known: true, Since: enteredReview, Waited: reviewWaited},
	}
	unknown := Card{Task: sampleTask("orch_w2", func(x *task.Task) { x.Report = resultReport() }), Stage: task.ReadyForReview}
	running := Card{Task: sampleTask("orch_w3", nil), Stage: task.Running}
	board := NewBoard([]Card{known, unknown, running})

	for _, tc := range []struct {
		card Card
		want string
	}{
		{known, waitLine},
		{unknown, "Waiting for review: unknown"},
	} {
		for _, d := range []Detail{{SelectedID: tc.card.ID()}, {SelectedID: tc.card.ID(), Expanded: true}} {
			lineContaining(t, strings.Split(Render(board, d, 120, th), "\n"), tc.want)
		}
		lineContaining(t, pageLines(tc.card, 120, th), tc.want)
	}

	if out := Render(board, Detail{SelectedID: running.ID(), Expanded: true}, 120, th); strings.Contains(out, "Waiting for review") {
		t.Errorf("a running card shows a review wait:\n%s", out)
	}
	if page := strings.Join(pageLines(running, 120, th), "\n"); strings.Contains(page, "Waiting for review") {
		t.Errorf("a running card's page shows a review wait:\n%s", page)
	}
}

// TestWaitLineFitsNarrowLayouts: no line of the detail panel or the page is wider
// than the terminal, even when the wait wraps.
func TestWaitLineFitsNarrowLayouts(t *testing.T) {
	th := Theme{Profile: termenv.Ascii}
	card := Card{
		Task:   sampleTask("orch_w1", func(x *task.Task) { x.Report = resultReport() }),
		Stage:  task.ReadyForReview,
		Review: ReviewWait{Known: true, Since: enteredReview, Waited: reviewWaited},
	}
	board := NewBoard([]Card{card})
	for _, width := range []int{80, 24, 12} {
		for _, line := range strings.Split(Render(board, Detail{SelectedID: card.ID()}, width, th), "\n") {
			if got := runeWidth(line); got > width {
				t.Errorf("width %d: a %d-column line: %q", width, got, line)
			}
		}
		for _, line := range pageLines(card, width, th) {
			if got := runeWidth(line); got > width {
				t.Errorf("page at width %d: a %d-column line: %q", width, got, line)
			}
		}
	}
}

// revise applies one write to a seeded task, the way a coordinator write does.
func (f *boardFixture) revise(t *testing.T, id string, mutate func(*task.Task)) {
	t.Helper()
	current := f.task(id)
	if _, err := f.store.UpdateTask(context.Background(), id, current.Revision, func(x *task.Task) error {
		mutate(x)
		return nil
	}); err != nil {
		t.Fatalf("revise %s: %v", id, err)
	}
}

// lastStageRow is the task's newest recorded stage change.
func (f *boardFixture) lastStageRow(t *testing.T, id string) state.Transition {
	t.Helper()
	history := f.history(t, id)
	var last *state.Transition
	for i := range history {
		if history[i].Kind == state.HistoryStage {
			last = &history[i]
		}
	}
	if last == nil {
		t.Fatalf("%s has no stage row in %+v", id, history)
	}
	return *last
}

func (f *boardFixture) history(t *testing.T, id string) []state.Transition {
	t.Helper()
	history, err := f.store.History(context.Background(), id)
	if err != nil {
		t.Fatalf("History of %s: %v", id, err)
	}
	return history
}

// stamp is an entry time the way the detail line writes it.
func stamp(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04") + " UTC" }

// TestReviewWaitIsTheStoreTimeOfTheEntry: the wait is the newest entry row's time,
// measured to the read. A later write that leaves the stage alone moves UpdatedAt
// but not the wait, and reading the board writes nothing.
func TestReviewWaitIsTheStoreTimeOfTheEntry(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_v1", nil))
	f.revise(t, "orch_v1", func(x *task.Task) { x.Report = resultReport() })
	entry := f.lastStageRow(t, "orch_v1")
	if entry.To != string(task.ReadyForReview) {
		t.Fatalf("the newest stage row enters %q, want ready_for_review", entry.To)
	}

	rows, revision := len(f.history(t, "orch_v1")), f.task("orch_v1").Revision
	cards, err := projectCardsAt(ctx, f.store, f.project, time.UnixMilli(entry.At).Add(reviewWaited))
	if err != nil {
		t.Fatalf("projectCardsAt: %v", err)
	}
	card := cardByID(t, cards, "orch_v1")
	if !card.Review.Known || card.Review.Since.UnixMilli() != entry.At || card.Review.Waited != reviewWaited {
		t.Fatalf("review wait = %+v, want the entry row %d after %v", card.Review, entry.At, reviewWaited)
	}
	if after, rev := len(f.history(t, "orch_v1")), f.task("orch_v1").Revision; after != rows || rev != revision {
		t.Errorf("reading the board wrote: history %d -> %d rows, revision %d -> %d", rows, after, revision, rev)
	}
	out := Render(NewBoard(cards), Detail{SelectedID: "orch_v1"}, 120, Theme{Profile: termenv.Ascii})
	lineContaining(t, strings.Split(out, "\n"), "Waiting for review: 2h 13m (since "+stamp(entry.At)+")")

	time.Sleep(20 * time.Millisecond)
	f.revise(t, "orch_v1", func(x *task.Task) { x.Title = "renamed while in review" })
	if updated := f.task("orch_v1").UpdatedAt; updated <= entry.At {
		t.Fatalf("UpdatedAt %d did not pass the entry %d, so this cannot tell them apart", updated, entry.At)
	}
	cards, err = projectCardsAt(ctx, f.store, f.project, time.Now())
	if err != nil {
		t.Fatalf("projectCardsAt after the rename: %v", err)
	}
	if got := cardByID(t, cards, "orch_v1").Review.Since.UnixMilli(); got != entry.At {
		t.Errorf("a stage-neutral write moved the wait to %d, want the entry %d", got, entry.At)
	}
}

// TestReworkShowsTheSecondEntryIntoReview: a rework takes the task out of review and
// back in, and the wait starts at the newest entry, not the first.
func TestReworkShowsTheSecondEntryIntoReview(t *testing.T) {
	ctx := context.Background()
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_v1", nil))
	f.revise(t, "orch_v1", func(x *task.Task) { x.Report = resultReport() })
	first := f.lastStageRow(t, "orch_v1")

	f.revise(t, "orch_v1", func(x *task.Task) { x.Report = &task.Report{Kind: "partial", Summary: "reworking"} })
	time.Sleep(20 * time.Millisecond)
	f.revise(t, "orch_v1", func(x *task.Task) { x.Report = resultReport() })
	second := f.lastStageRow(t, "orch_v1")
	if second.Seq <= first.Seq || second.At == first.At {
		t.Fatalf("the two entries are not told apart: first %+v, second %+v", first, second)
	}

	cards, err := projectCardsAt(ctx, f.store, f.project, time.UnixMilli(second.At).Add(time.Minute))
	if err != nil {
		t.Fatalf("projectCardsAt: %v", err)
	}
	if got := cardByID(t, cards, "orch_v1").Review; !got.Known || got.Since.UnixMilli() != second.At {
		t.Errorf("the wait starts at %d, want the second entry %d, not the first %d", got.Since.UnixMilli(), second.At, first.At)
	}
}

// TestTaskAlreadyInReviewShowsUnknown: a task seeded in review has a creation row
// for that stage and no stage row entering it, so its wait is unknown. Its
// UpdatedAt must not stand in for the time.
func TestTaskAlreadyInReviewShowsUnknown(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_v1", func(x *task.Task) { x.Report = resultReport() }))
	time.Sleep(20 * time.Millisecond)
	f.revise(t, "orch_v1", func(x *task.Task) { x.Title = "renamed while in review" })

	cards, err := projectCardsAt(context.Background(), f.store, f.project, time.Now())
	if err != nil {
		t.Fatalf("projectCardsAt: %v", err)
	}
	card := cardByID(t, cards, "orch_v1")
	if card.Stage != task.ReadyForReview || card.Review.Known {
		t.Fatalf("stage %q, review wait %+v: want ready_for_review and unknown", card.Stage, card.Review)
	}
	out := Render(NewBoard(cards), Detail{SelectedID: "orch_v1"}, 120, Theme{Profile: termenv.Ascii})
	lineContaining(t, strings.Split(out, "\n"), "Waiting for review: unknown")
}

// TestBoardShowsTheWaitOfTheSelectedReviewCard drives the Model's own refresh, so
// the wait reaches the view the reader sees.
func TestBoardShowsTheWaitOfTheSelectedReviewCard(t *testing.T) {
	f := newBoardFixture(t, fixturePlan{})
	f.seed(t, sampleTask("orch_v1", nil))
	f.revise(t, "orch_v1", func(x *task.Task) { x.Report = resultReport() })
	entry := f.lastStageRow(t, "orch_v1")

	m := f.model(t, Options{})
	if got := m.SelectedID(); got != "orch_v1" {
		t.Fatalf("selected %q, want the review card", got)
	}
	line := lineContaining(t, strings.Split(m.View(), "\n"), "Waiting for review:")
	if !strings.Contains(line, "(since "+stamp(entry.At)+")") {
		t.Errorf("the board's wait line %q does not carry the entry time %q", line, stamp(entry.At))
	}
}
