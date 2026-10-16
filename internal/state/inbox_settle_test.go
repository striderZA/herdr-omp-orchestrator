package state_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// uncertainNoteFixture records three notes and leaves two of them uncertain with
// the detail a live ambiguous prompt records: one of repo-a's, and one of repo-b's.
func uncertainNoteFixture(t *testing.T, s *state.Store, detail string) {
	t.Helper()
	ctx := context.Background()
	for _, note := range []task.Inbox{
		{ID: "uncertain", ProjectKey: "repo-a", TaskID: "t1", Kind: "result", Summary: "finished", Delivery: "worker result"},
		{ID: "foreign", ProjectKey: "repo-b", TaskID: "t9", Kind: "result", Summary: "elsewhere", Delivery: "worker result"},
		{ID: "pending", ProjectKey: "repo-a", TaskID: "t1", Kind: "progress", Summary: "halfway", Delivery: "worker progress"},
	} {
		if _, err := s.AddInbox(ctx, note); err != nil {
			t.Fatalf("AddInbox(%s): %v", note.ID, err)
		}
	}
	for _, id := range []string{"uncertain", "foreign"} {
		if claimed, err := s.ClaimInbox(ctx, []string{id}); err != nil || len(claimed) != 1 {
			t.Fatalf("ClaimInbox(%s) = %d, %v", id, len(claimed), err)
		}
		if settled, err := s.SettleInbox(ctx, []string{id}, task.DeliveryUncertain, detail); err != nil || settled != 1 {
			t.Fatalf("SettleInbox(%s, uncertain) = %d, %v", id, settled, err)
		}
	}
}

// TestSettleUncertainInboxChangesOnlyAnUncertainNoteOfTheProject pins the write
// behind a user's decision: it moves one note only while the note is uncertain,
// belongs to the project asked, and still carries the detail the decision was
// made on. Every refused settlement writes nothing.
func TestSettleUncertainInboxChangesOnlyAnUncertainNoteOfTheProject(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	const observed = "may already have been delivered; inspect the pane"
	uncertainNoteFixture(t, s, observed)

	// A note that is not uncertain has no unresolved outcome to decide.
	if _, err := s.SettleUncertainInbox(ctx, "repo-a", "pending", "", task.DeliveryDelivered, "x"); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("settling a pending note = %v, want ErrConflict", err)
	}
	// Another project's note is not found from this project.
	if _, err := s.SettleUncertainInbox(ctx, "repo-a", "foreign", observed, task.DeliveryDelivered, "x"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("settling another project's note = %v, want ErrNotFound", err)
	}
	// The detail the user inspected must still be the recorded one.
	if _, err := s.SettleUncertainInbox(ctx, "repo-a", "uncertain", "an older detail", task.DeliveryDelivered, "x"); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("settling on a changed detail = %v, want ErrConflict", err)
	}
	// Only the two decisions exist: delivered and pending.
	if _, err := s.SettleUncertainInbox(ctx, "repo-a", "uncertain", observed, task.DeliverySending, "x"); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("settling to sending = %v, want ErrInvalid", err)
	}
	if _, err := s.SettleUncertainInbox(ctx, "repo-a", "missing", observed, task.DeliveryDelivered, "x"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("settling a missing note = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"uncertain", "foreign"} {
		if got, err := s.Inbox(ctx, id); err != nil || got.State() != task.DeliveryUncertain || got.DeliveryDetail != observed {
			t.Fatalf("a refused settlement changed %s: %+v, %v", id, got, err)
		}
	}

	// The user's decision ends the uncertainty and returns the note as it now stands.
	const confirmed = "the user confirmed it arrived in the primary's pane"
	settled, err := s.SettleUncertainInbox(ctx, "repo-a", "uncertain", observed, task.DeliveryDelivered, confirmed)
	if err != nil || settled.ID != "uncertain" || settled.State() != task.DeliveryDelivered || settled.DeliveryDetail != confirmed {
		t.Fatalf("delivered settlement = %+v, %v", settled, err)
	}
	// Settled once, it is no longer uncertain and cannot be settled again.
	if _, err := s.SettleUncertainInbox(ctx, "repo-a", "uncertain", confirmed, task.DeliveryPending, "again"); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("second settlement = %v, want ErrConflict", err)
	}
}

// TestSettleUncertainInboxNotDeliveredRequeuesTheNote pins the other decision: the
// note returns to pending, so the next safe flush delivers it, and it is no longer
// uncertain.
func TestSettleUncertainInboxNotDeliveredRequeuesTheNote(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	const observed = "may already have been delivered; inspect the pane"
	uncertainNoteFixture(t, s, observed)

	settled, err := s.SettleUncertainInbox(ctx, "repo-a", "uncertain", observed, task.DeliveryPending, "the user confirmed it did not arrive")
	if err != nil || settled.State() != task.DeliveryPending {
		t.Fatalf("not-delivered settlement = %+v, %v", settled, err)
	}
	pending, err := s.PendingInbox(ctx, "repo-a")
	if err != nil {
		t.Fatalf("PendingInbox: %v", err)
	}
	ids := make([]string, 0, len(pending))
	for _, note := range pending {
		ids = append(ids, note.ID)
	}
	if want := []string{"uncertain", "pending"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("pending notes = %v, want %v: the requeued note is delivered like any other pending note", ids, want)
	}
}
