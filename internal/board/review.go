package board

import (
	"context"
	"fmt"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// ReviewWait is how long a card has waited in ready_for_review, as its task's
// history records it. The history is the only source: UpdatedAt moves on every
// write, so it cannot mark the moment a task entered review.
type ReviewWait struct {
	// Known is false when the history cannot say when the card entered review: no
	// stage row enters review, or the newest stage row enters another stage. A task
	// that was already in review when history began stays unknown until its next
	// stage change.
	Known bool
	// Since is when the newest stage row that enters review was recorded.
	Since time.Time
	// Waited is the time from Since to the read that produced the card.
	Waited time.Duration
}

// readReviewWait reads one card's history and judges it. Only a card in
// ready_for_review reads history, so the board reads it for the cards that show a
// wait and for no others.
func readReviewWait(ctx context.Context, store *state.Store, taskID string, stage task.Stage, asOf time.Time) (ReviewWait, error) {
	history, err := store.History(ctx, taskID)
	if err != nil {
		return ReviewWait{}, err
	}
	return reviewWaitFrom(history, stage, asOf), nil
}

// reviewWaitFrom is the review wait the history proves for a card in stage, measured
// to asOf. Only ready_for_review has one, so any other stage is unknown. The newest
// stage row is the last stage change in record order (Seq), not the latest clock
// reading; it proves the entry only when that change is into the card's stage.
func reviewWaitFrom(history []state.Transition, stage task.Stage, asOf time.Time) ReviewWait {
	if stage != task.ReadyForReview {
		return ReviewWait{}
	}
	var newest *state.Transition
	for i := range history {
		if history[i].Kind != state.HistoryStage {
			continue
		}
		if newest == nil || history[i].Seq > newest.Seq {
			newest = &history[i]
		}
	}
	if newest == nil || newest.To != string(stage) {
		return ReviewWait{}
	}
	since := time.UnixMilli(newest.At)
	return ReviewWait{Known: true, Since: since, Waited: asOf.Sub(since)}
}

// formatWaited states a wait in its two largest units: "under a minute", "13m",
// "2h 13m", "2d 1h". A negative wait, which a row stamped after its read would
// give, reads as under a minute.
func formatWaited(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	}
}

// utcStamp is an absolute time in UTC, as the board shows every recorded time.
func utcStamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04") + " UTC" }

// reviewWaitLines is the wait line of a card in review, wrapped to width. Every
// other card has none.
func reviewWaitLines(card Card, width int) []string {
	if card.Stage != task.ReadyForReview {
		return nil
	}
	if !card.Review.Known {
		return wrap("Waiting for review: unknown", width)
	}
	return wrap(fmt.Sprintf("Waiting for review: %s (since %s)",
		formatWaited(card.Review.Waited), utcStamp(card.Review.Since)), width)
}
