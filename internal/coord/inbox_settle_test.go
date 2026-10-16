package coord

import (
	"context"
	"errors"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/task"
)

// uncertainNoteAfterAmbiguousPrompt delivers one worker note through FlushInbox
// while the primary's notification prompt is ambiguous, so the note ends
// uncertain on the same path a real timeout takes.
func uncertainNoteAfterAmbiguousPrompt(t *testing.T, f *coordFixture) string {
	t.Helper()
	worker := f.boundWorker(t)
	f.updatePlan(func(plan *fixturePlan) { plan.PromptAmbiguous = true })
	noteID := f.queueNote(t, worker, "result", "finished", "worker result").ID
	if err := f.coord.FlushInbox(context.Background(), f.projectKey()); !errors.Is(err, ErrAmbiguousStep) {
		t.Fatalf("FlushInbox err = %v, want ErrAmbiguousStep", err)
	}
	if got := f.noteState(t, noteID); got != task.DeliveryUncertain {
		t.Fatalf("state = %q, want uncertain", got)
	}
	return noteID
}

// TestInboxPreviewShowsOnlyAnUncertainNoteOfTheProject pins what the user is asked
// to decide on: one uncertain note of this project, with the digest that binds the
// decision to exactly what was shown. Previewing writes nothing.
func TestInboxPreviewShowsOnlyAnUncertainNoteOfTheProject(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	uncertain := uncertainNoteAfterAmbiguousPrompt(t, f)
	pending := f.queueInboxNote(f.projectKey(), "t1", "progress", "halfway", "worker progress")
	foreign := f.queueInboxNote(foreignProject, "t9", "result", "elsewhere", "worker result")
	f.settleInboxNote(foreign, task.DeliveryUncertain, "inspect the pane")

	preview, err := f.coord.InboxPreviewFor(ctx, f.projectKey(), uncertain)
	if err != nil {
		t.Fatalf("InboxPreviewFor: %v", err)
	}
	if preview.NoteID != uncertain || preview.DeliveryState != task.DeliveryUncertain || preview.Digest == "" ||
		!strings.Contains(preview.DeliveryDetail, "agent get") {
		t.Fatalf("preview = %+v", preview)
	}
	if _, err := f.coord.InboxPreviewFor(ctx, f.projectKey(), pending); !errors.Is(err, ErrNoteNotUncertain) {
		t.Fatalf("preview of a pending note = %v, want ErrNoteNotUncertain", err)
	}
	if _, err := f.coord.InboxPreviewFor(ctx, f.projectKey(), foreign); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("preview of another project's note = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.coord.InboxPreviewFor(ctx, f.projectKey(), "no-such-note"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("preview of a missing note = %v, want ErrInvalidRequest", err)
	}
	if got := f.noteState(t, uncertain); got != task.DeliveryUncertain {
		t.Fatalf("preview changed the note to %q", got)
	}
}

// TestInboxSettleDeliveredEndsTheUncertaintyWithoutResendingTheNote pins the
// confirmed "it arrived" decision: the note is delivered, a later flush never
// sends it again, and it stops blocking forgetting.
func TestInboxSettleDeliveredEndsTheUncertaintyWithoutResendingTheNote(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	noteID := uncertainNoteAfterAmbiguousPrompt(t, f)
	primary := f.boundProject(t).PublicPaneID
	preview, err := f.coord.InboxPreviewFor(ctx, f.projectKey(), noteID)
	if err != nil {
		t.Fatalf("InboxPreviewFor: %v", err)
	}

	result, err := f.coord.SettleInboxFor(ctx, f.projectKey(),
		InboxSettleParams{NoteID: noteID, ObservedDigest: preview.Digest, Decision: "delivered"})
	if err != nil || result.NoteID != noteID || result.Decision != "delivered" || result.DeliveryState != task.DeliveryDelivered {
		t.Fatalf("SettleInboxFor = %+v, %v", result, err)
	}
	if err := f.coord.FlushInbox(ctx, f.projectKey()); err != nil {
		t.Fatalf("FlushInbox: %v", err)
	}
	if got := len(f.promptsTo(t, primary)); got != 1 {
		t.Fatalf("notification prompts = %d, want only the ambiguous attempt", got)
	}
	plan, err := f.coord.PlanForget(ctx, f.projectKey())
	if err != nil {
		t.Fatalf("PlanForget: %v", err)
	}
	for _, blocker := range plan.Blockers {
		if strings.HasPrefix(blocker, "note ") {
			t.Fatalf("a settled note still blocks forgetting: %q", blocker)
		}
	}
}

// TestInboxSettleNotDeliveredRequeuesTheNoteForTheNextIdleTurn pins the "it did not
// arrive" decision: the note goes back to pending, and the next safe flush delivers
// it once.
func TestInboxSettleNotDeliveredRequeuesTheNoteForTheNextIdleTurn(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	noteID := uncertainNoteAfterAmbiguousPrompt(t, f)
	primary := f.boundProject(t).PublicPaneID
	preview, err := f.coord.InboxPreviewFor(ctx, f.projectKey(), noteID)
	if err != nil {
		t.Fatalf("InboxPreviewFor: %v", err)
	}

	result, err := f.coord.SettleInboxFor(ctx, f.projectKey(),
		InboxSettleParams{NoteID: noteID, ObservedDigest: preview.Digest, Decision: "not_delivered"})
	if err != nil || result.DeliveryState != task.DeliveryPending {
		t.Fatalf("SettleInboxFor = %+v, %v", result, err)
	}

	// The prompt that made the primary busy is over: its next idle turn is what
	// delivers the requeued note, as any pending note.
	f.setAgentStatus(t, primary, "idle")
	f.updatePlan(func(plan *fixturePlan) { plan.PromptAmbiguous = false })
	if err := f.coord.FlushInbox(ctx, f.projectKey()); err != nil {
		t.Fatalf("FlushInbox: %v", err)
	}
	if got := f.noteState(t, noteID); got != task.DeliveryDelivered {
		t.Fatalf("state after the requeued flush = %q, want delivered", got)
	}
	if got := len(f.promptsTo(t, primary)); got != 2 {
		t.Fatalf("notification prompts = %d, want the ambiguous attempt and one requeued delivery", got)
	}
}

// TestInboxSettleRefusesAStaleDigestAndAnUnknownDecision pins the refusals: a
// digest the user did not inspect, a decision the user cannot make, and a second
// settlement all leave the note as it was, and the refusal names the cause.
func TestInboxSettleRefusesAStaleDigestAndAnUnknownDecision(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	noteID := uncertainNoteAfterAmbiguousPrompt(t, f)
	preview, err := f.coord.InboxPreviewFor(ctx, f.projectKey(), noteID)
	if err != nil {
		t.Fatalf("InboxPreviewFor: %v", err)
	}

	if _, err := f.coord.SettleInboxFor(ctx, f.projectKey(),
		InboxSettleParams{NoteID: noteID, ObservedDigest: "stale", Decision: "delivered"}); !errors.Is(err, ErrEvidenceChanged) {
		t.Fatalf("stale digest = %v, want ErrEvidenceChanged", err)
	}
	if _, err := f.coord.SettleInboxFor(ctx, f.projectKey(),
		InboxSettleParams{NoteID: noteID, ObservedDigest: preview.Digest, Decision: "maybe"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown decision = %v, want ErrInvalidRequest", err)
	}
	if got := f.noteState(t, noteID); got != task.DeliveryUncertain {
		t.Fatalf("refused settlements changed the note to %q", got)
	}

	if _, err := f.coord.SettleInboxFor(ctx, f.projectKey(),
		InboxSettleParams{NoteID: noteID, ObservedDigest: preview.Digest, Decision: "delivered"}); err != nil {
		t.Fatalf("SettleInboxFor: %v", err)
	}
	if _, err := f.coord.SettleInboxFor(ctx, f.projectKey(),
		InboxSettleParams{NoteID: noteID, ObservedDigest: preview.Digest, Decision: "delivered"}); !errors.Is(err, ErrNoteNotUncertain) {
		t.Fatalf("repeat settlement = %v, want ErrNoteNotUncertain", err)
	}
}

// TestInboxPreviewSaysWhichTextItShortens pins the evidence the settle dialog
// gives. The digest covers each full text, so a preview that cuts the label, the
// summary or the recorded detail must say which one. A text that fits is shown
// whole and is not flagged.
func TestInboxPreviewSaysWhichTextItShortens(t *testing.T) {
	long := strings.Repeat("x", inspectTextLimit+inspectDetailLimit)
	for _, tc := range []struct {
		name                            string
		summary, label, detail          string
		labelCut, summaryCut, detailCut bool
	}{
		{"nothing is shortened", "finished", "worker result", "short detail", false, false, false},
		{"the label is shortened", "finished", long, "short detail", true, false, false},
		{"the summary is shortened", long, "worker result", "short detail", false, true, false},
		{"the detail is shortened", "finished", "worker result", long, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			id := f.queueNote(t, f.boundWorker(t), "result", tc.summary, tc.label).ID
			f.settleInboxNote(id, task.DeliveryUncertain, tc.detail)
			preview, err := f.coord.InboxPreviewFor(context.Background(), f.projectKey(), id)
			if err != nil {
				t.Fatalf("InboxPreviewFor: %v", err)
			}
			if preview.LabelTruncated != tc.labelCut || preview.SummaryTruncated != tc.summaryCut ||
				preview.DeliveryDetailTruncated != tc.detailCut {
				t.Fatalf("truncated label %t, summary %t, detail %t; want %t, %t, %t",
					preview.LabelTruncated, preview.SummaryTruncated, preview.DeliveryDetailTruncated,
					tc.labelCut, tc.summaryCut, tc.detailCut)
			}
		})
	}
}
