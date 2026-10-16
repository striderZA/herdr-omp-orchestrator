package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// ErrNoteNotUncertain reports a preview or a settlement for an inbox note whose
// delivery is not uncertain, so there is no unresolved outcome for a user to decide.
var ErrNoteNotUncertain = errors.New("coord: the inbox note is not uncertain")

// The uncertain-note decisions a user makes. They are the same words settle_input
// uses for a worker input whose outcome is unknown.
//
// A "delivered" decision ends the note, and it is never sent again. A
// "not_delivered" decision queues the note for the primary's next idle turn, as
// any new note is delivered: the user has said it did not arrive, so a delivery
// is wanted.
const (
	inboxDetailDelivered    = "the user confirmed the notification reached the primary"
	inboxDetailNotDelivered = "the user confirmed the notification did not reach the primary; it waits for the next idle turn"
)

// InboxPreviewParams is the inbox_preview request body: the note to show.
type InboxPreviewParams struct {
	NoteID string `json:"noteId"`
}

// InboxSettlePreview is the read-only view a user decides an uncertain note on.
// Digest covers every fact shown, so a settlement quotes back exactly what was
// inspected. Summary is the worker's own text, bounded for display. Each
// *Truncated flag reports that the display cut its text to the bound. The digest
// still covers the whole text, so the dialog must say which texts were cut. The
// flags are always present, so a reader that needs them can refuse a preview that
// lacks them.
type InboxSettlePreview struct {
	NoteID                  string `json:"noteId"`
	TaskID                  string `json:"taskId,omitempty"`
	Kind                    string `json:"kind"`
	Label                   string `json:"label,omitempty"`
	LabelTruncated          bool   `json:"labelTruncated"`
	Summary                 string `json:"summary"`
	SummaryTruncated        bool   `json:"summaryTruncated"`
	DeliveryState           string `json:"deliveryState"`
	DeliveryDetail          string `json:"deliveryDetail"`
	DeliveryDetailTruncated bool   `json:"deliveryDetailTruncated"`
	Digest                  string `json:"digest"`
}

// InboxSettleParams is the inbox_settle request body: the note, the digest the user
// was shown, and the decision the user made.
type InboxSettleParams struct {
	NoteID         string `json:"noteId"`
	ObservedDigest string `json:"observedDigest"`
	Decision       string `json:"decision"`
}

// InboxSettleResult is the answer to one settlement: which note, what the user
// decided, and where the note stands now.
type InboxSettleResult struct {
	NoteID        string `json:"noteId"`
	Decision      string `json:"decision"`
	DeliveryState string `json:"deliveryState"`
}

// InboxPreviewFor reads one uncertain note of a project for the user's decision.
// The project comes from the verified principal, and the call writes nothing.
func (c *Coordinator) InboxPreviewFor(ctx context.Context, projectKey, noteID string) (InboxSettlePreview, error) {
	note, err := c.uncertainNote(ctx, projectKey, noteID)
	if err != nil {
		return InboxSettlePreview{}, err
	}
	summary := strings.TrimSpace(note.Summary)
	label := truncateRunes(note.Delivery, inspectTextLimit)
	shownSummary := truncateRunes(summary, inspectTextLimit)
	detail := truncateRunes(note.DeliveryDetail, inspectDetailLimit)
	return InboxSettlePreview{
		NoteID:                  note.ID,
		TaskID:                  note.TaskID,
		Kind:                    note.Kind,
		Label:                   label,
		LabelTruncated:          label != note.Delivery,
		Summary:                 shownSummary,
		SummaryTruncated:        shownSummary != summary,
		DeliveryState:           note.State(),
		DeliveryDetail:          detail,
		DeliveryDetailTruncated: detail != note.DeliveryDetail,
		Digest:                  note.Digest(),
	}, nil
}

// SettleInboxFor records the user's decision about one uncertain note. The note
// must still be uncertain, belong to the project, and match the digest the user
// was shown, or nothing is written. The write itself also re-checks the note's
// detail, so a settlement that raced another one is refused rather than applied.
func (c *Coordinator) SettleInboxFor(ctx context.Context, projectKey string, p InboxSettleParams) (InboxSettleResult, error) {
	decision := strings.TrimSpace(p.Decision)
	if decision != decisionDelivered && decision != decisionNotDelivered {
		return InboxSettleResult{}, fmt.Errorf("%w: %q is not a note decision; the user reports that the note either %s or %s",
			ErrInvalidRequest, p.Decision, decisionDelivered, decisionNotDelivered)
	}
	note, err := c.uncertainNote(ctx, projectKey, p.NoteID)
	if err != nil {
		return InboxSettleResult{}, err
	}
	if observed := strings.TrimSpace(p.ObservedDigest); observed == "" || observed != note.Digest() {
		return InboxSettleResult{}, fmt.Errorf("%w: note %s shows something other than what was inspected", ErrEvidenceChanged, note.ID)
	}

	next, detail := task.DeliveryDelivered, inboxDetailDelivered
	if decision == decisionNotDelivered {
		next, detail = task.DeliveryPending, inboxDetailNotDelivered
	}
	settled, err := c.store.SettleUncertainInbox(ctx, note.ProjectKey, note.ID, note.DeliveryDetail, next, detail)
	switch {
	case errors.Is(err, state.ErrConflict):
		return InboxSettleResult{}, fmt.Errorf("%w: note %s changed while the decision was recorded", ErrEvidenceChanged, note.ID)
	case errors.Is(err, state.ErrNotFound):
		return InboxSettleResult{}, fmt.Errorf("%w: no inbox note %q belongs to project %q", ErrInvalidRequest, note.ID, note.ProjectKey)
	case err != nil:
		return InboxSettleResult{}, err
	}
	return InboxSettleResult{NoteID: settled.ID, Decision: decision, DeliveryState: settled.State()}, nil
}

// uncertainNote reads one note of the project and refuses a note the project does
// not own, or one that is not uncertain. The project comes from the caller's
// verified principal, never from the note, so a note of another project is
// refused as unknown rather than revealed.
func (c *Coordinator) uncertainNote(ctx context.Context, projectKey, noteID string) (task.Inbox, error) {
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return task.Inbox{}, err
	}
	id := strings.TrimSpace(noteID)
	if id == "" {
		return task.Inbox{}, fmt.Errorf("%w: a note id is required", ErrInvalidRequest)
	}
	note, err := c.store.Inbox(ctx, id)
	switch {
	case errors.Is(err, state.ErrNotFound), err == nil && note.ProjectKey != project.Key:
		return task.Inbox{}, fmt.Errorf("%w: no inbox note %q belongs to project %q", ErrInvalidRequest, id, project.Key)
	case err != nil:
		return task.Inbox{}, fmt.Errorf("read inbox note %q: %w", id, err)
	case note.State() != task.DeliveryUncertain:
		return task.Inbox{}, fmt.Errorf("%w: note %s is %s", ErrNoteNotUncertain, id, note.State())
	}
	return note, nil
}
