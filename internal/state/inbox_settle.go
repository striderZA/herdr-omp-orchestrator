package state

import (
	"context"
	"fmt"
	"strings"

	"herdr-omp-orchestrator/internal/task"
)

// SettleUncertainInbox records a user's decision about one uncertain note of a
// project and returns the note as it now stands. The two outcomes are delivered,
// which ends the note, and pending, which queues it for the next safe flush. It is
// the only write that moves a note out of uncertain by a human decision: no other
// path resends or settles an uncertain note.
//
// The write changes the note only while it is still uncertain, belongs to
// projectKey, and carries observedDetail, the detail the decision was made on. A
// note has no revision, so its detail is what a settlement changes: a note that
// has since settled, or was recorded under another detail, matches nothing and the
// write refuses with ErrConflict. A note that is missing, or belongs to another
// project, is ErrNotFound, so one project cannot learn another's notes through it.
func (s *Store) SettleUncertainInbox(ctx context.Context, projectKey, id, observedDetail, next, detail string) (task.Inbox, error) {
	if strings.TrimSpace(projectKey) == "" || strings.TrimSpace(id) == "" {
		return task.Inbox{}, fmt.Errorf("%w: project key and inbox id are required", ErrInvalid)
	}
	settled, err := inboxState(next, "")
	if err != nil {
		return task.Inbox{}, err
	}
	if settled != task.DeliveryDelivered && settled != task.DeliveryPending {
		return task.Inbox{}, fmt.Errorf("%w: an uncertain note settles to %q or %q, not %q",
			ErrInvalid, task.DeliveryDelivered, task.DeliveryPending, next)
	}
	tx, err := s.begin(ctx, "settle uncertain inbox note "+id)
	if err != nil {
		return task.Inbox{}, err
	}
	defer tx.rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE inbox SET delivery_state = ?, delivery_detail = ?
		 WHERE id = ? AND project_key = ? AND delivery_state = ? AND delivery_detail = ?`,
		settled, detail, id, projectKey, task.DeliveryUncertain, observedDetail)
	if err != nil {
		return task.Inbox{}, fmt.Errorf("settle inbox %q: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return task.Inbox{}, fmt.Errorf("settle inbox %q: %w", id, err)
	}
	if rows == 1 {
		updated, err := scanInbox(tx.QueryRowContext(ctx, `SELECT `+inboxSelect+` FROM inbox WHERE id = ?`, id),
			"settled inbox "+id)
		if err != nil {
			return task.Inbox{}, err
		}
		if err := tx.commit("settle uncertain inbox note " + id); err != nil {
			return task.Inbox{}, err
		}
		return updated, nil
	}

	// Nothing matched, so nothing was written. Say why from the note as it stands.
	existing, err := scanInbox(tx.QueryRowContext(ctx, `SELECT `+inboxSelect+` FROM inbox WHERE id = ?`, id),
		"inbox "+id)
	if err != nil {
		return task.Inbox{}, err
	}
	switch {
	case existing.ProjectKey != projectKey:
		return task.Inbox{}, fmt.Errorf("%w: inbox %s", ErrNotFound, id)
	case existing.State() != task.DeliveryUncertain:
		return task.Inbox{}, fmt.Errorf("%w: inbox %s is %s, not uncertain", ErrConflict, id, existing.State())
	default:
		return task.Inbox{}, fmt.Errorf("%w: inbox %s changed since the decision was made", ErrConflict, id)
	}
}
