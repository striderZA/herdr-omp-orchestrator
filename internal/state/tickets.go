package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/task"
)

// Errors that belong to the ticket/task gate. Callers compare them with errors.Is.
var (
	// ErrTicketUnavailable reports a task that names a ticket which cannot take
	// new work: it does not exist, belongs to another project or is decided.
	// The write that hit it changed nothing.
	ErrTicketUnavailable = errors.New("state: the ticket cannot take new tasks")
	// ErrTicketHasOpenTasks reports a decision for a ticket that still has a
	// linked task without a disposition. The decision changed nothing.
	ErrTicketHasOpenTasks = errors.New("state: the ticket still has open tasks")
)

// Tickets live in their own table beside tasks. A ticket is keyed by its own id
// and exists before any Herdr resource, so nothing here touches panes, worktrees
// or history: history records a task's stage transitions, and a ticket has no
// stored stage to transition (see task.TicketStageOf).

// CreateTicket records a new, undecided ticket as revision 1.
func (s *Store) CreateTicket(ctx context.Context, t task.Ticket) error {
	if t.ID == "" || t.ProjectKey == "" {
		return fmt.Errorf("%w: ticket id and project key are required", ErrInvalid)
	}
	if err := checkTicketShape(t); err != nil {
		return err
	}
	if t.Decided() || t.Outcome != "" {
		return fmt.Errorf("%w: ticket %q cannot be created already decided", ErrInvalid, t.ID)
	}
	now := time.Now().UnixMilli()
	t.Revision = 1
	t.CreatedAt, t.UpdatedAt = now, now
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode ticket %q: %w", t.ID, err)
	}

	tx, err := s.begin(ctx, "create ticket "+t.ID)
	if err != nil {
		return err
	}
	defer tx.rollback()

	if taken, err := rowExists(ctx, tx, `SELECT 1 FROM tickets WHERE id = ?`, t.ID); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("%w: ticket %q already exists", ErrConflict, t.ID)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO tickets (id, project_key, revision, data) VALUES (?, ?, ?, ?)`,
		t.ID, t.ProjectKey, t.Revision, string(data),
	); err != nil {
		return fmt.Errorf("create ticket %q: %w", t.ID, err)
	}
	return tx.commit("create ticket " + t.ID)
}

// Ticket reads one ticket by id.
func (s *Store) Ticket(ctx context.Context, id string) (task.Ticket, error) {
	return scanTicket(s.db.QueryRowContext(ctx, `SELECT data FROM tickets WHERE id = ?`, id), "ticket "+id)
}

// Tickets reads every ticket of one project, oldest first and then by id so a
// caller always sees the same order. It is a read-only projection.
func (s *Store) Tickets(ctx context.Context, projectKey string) ([]task.Ticket, error) {
	if strings.TrimSpace(projectKey) == "" {
		return nil, fmt.Errorf("%w: project key is required", ErrInvalid)
	}
	tickets, err := queryFacts[task.Ticket](ctx, s.db,
		`SELECT data FROM tickets WHERE project_key = ?
		 ORDER BY json_extract(data, '$.CreatedAt'), id`, projectKey)
	if err != nil {
		return nil, fmt.Errorf("read the tickets of %q: %w", projectKey, err)
	}
	return tickets, nil
}

// UpdateTicket applies mutate to the current facts and stores them as one
// revision later, but only while the stored revision still matches
// expectedRevision.
//
// It cannot record a decision: a mutation that changes Outcome or Disposition is
// refused, because DecideTicket is the one writer that proves no linked task is
// open in the same transaction. Identity (id, project key) is immutable too.
func (s *Store) UpdateTicket(ctx context.Context, id string, expectedRevision int64, mutate func(*task.Ticket) error) (task.Ticket, error) {
	if mutate == nil {
		return task.Ticket{}, fmt.Errorf("%w: nil mutation for ticket %q", ErrInvalid, id)
	}
	tx, err := s.begin(ctx, "update ticket "+id)
	if err != nil {
		return task.Ticket{}, err
	}
	defer tx.rollback()

	current, err := readTicketForUpdate(ctx, tx, id, expectedRevision)
	if err != nil {
		return task.Ticket{}, err
	}
	next := current
	if err := mutate(&next); err != nil {
		return task.Ticket{}, fmt.Errorf("mutate ticket %q: %w", id, err)
	}
	if next.ID != current.ID || next.ProjectKey != current.ProjectKey {
		return task.Ticket{}, fmt.Errorf("%w: ticket %q id and project key are immutable", ErrInvalid, id)
	}
	if next.Outcome != current.Outcome || next.Disposition != current.Disposition {
		return task.Ticket{}, fmt.Errorf("%w: ticket %q can only be decided through DecideTicket", ErrInvalid, id)
	}
	if err := checkTicketShape(next); err != nil {
		return task.Ticket{}, err
	}
	next, err = writeTicket(ctx, tx, current, next)
	if err != nil {
		return task.Ticket{}, err
	}
	if err := tx.commit("update ticket " + id); err != nil {
		return task.Ticket{}, err
	}
	return next, nil
}

// MoveTicketToTodoWithPlannerTask records the human's move and the planner's
// zero-slot intent in one transaction. A crash can therefore leave neither fact
// or both, never a todo ticket without the task this move promised.
func (s *Store) MoveTicketToTodoWithPlannerTask(
	ctx context.Context, id string, expectedRevision int64, planner task.Task,
) (task.Ticket, task.Task, error) {
	if strings.TrimSpace(id) == "" {
		return task.Ticket{}, task.Task{}, fmt.Errorf("%w: ticket id is required", ErrInvalid)
	}
	if err := checkTaskRole(planner); err != nil {
		return task.Ticket{}, task.Task{}, err
	}
	if planner.Role != task.TaskRolePlan || planner.TicketID != id ||
		planner.Phase != task.Intent || planner.Report != nil ||
		planner.Outcome != "" || planner.Disposition != "" {
		return task.Ticket{}, task.Task{}, fmt.Errorf("%w: planner intent does not match ticket %q", ErrInvalid, id)
	}
	operation := "move ticket " + id + " to todo with planner " + planner.ID
	tx, err := s.begin(ctx, operation)
	if err != nil {
		return task.Ticket{}, task.Task{}, err
	}
	defer tx.rollback()

	current, err := readTicketForUpdate(ctx, tx, id, expectedRevision)
	if err != nil {
		return task.Ticket{}, task.Task{}, err
	}
	if current.Decided() {
		return task.Ticket{}, task.Task{}, fmt.Errorf("%w: ticket %q is already decided", ErrTicketUnavailable, id)
	}
	if current.Column != task.ColumnBacklog {
		return task.Ticket{}, task.Task{}, fmt.Errorf("%w: ticket %q is not in the backlog", ErrConflict, id)
	}
	if planner.ProjectKey != current.ProjectKey {
		return task.Ticket{}, task.Task{}, fmt.Errorf("%w: planner task project does not match ticket %q", ErrInvalid, id)
	}
	next := current
	next.Column = task.ColumnTodo
	moved, err := writeTicket(ctx, tx, current, next)
	if err != nil {
		return task.Ticket{}, task.Task{}, err
	}
	_, recorded, _, err := s.claimTaskTx(ctx, tx, planner, 0, false)
	if err != nil {
		return task.Ticket{}, task.Task{}, err
	}
	if err := tx.commit(operation); err != nil {
		return task.Ticket{}, task.Task{}, err
	}
	return moved, recorded, nil
}

// DecideTicket records the user's decision for a ticket as one revision later,
// in one write transaction that also proves no linked task is open.
//
// A task is open until it records a disposition (ADR 0001), and it is linked
// when its stored TicketID is the ticket's id. Counting those tasks inside the
// transaction that writes the decision is what makes the rule hold: CreateTask
// and ClaimTask check the ticket inside their own immediate write transactions,
// so a decision and a dispatch for one ticket run one after the other, in either
// order and across processes. If the dispatch lands first the decision is
// refused with ErrTicketHasOpenTasks; if the decision lands first the dispatch
// is refused with ErrTicketUnavailable and records nothing.
//
// The first decision stands: deciding a decided ticket is ErrConflict, even at
// its current revision. The column is neither read nor changed.
func (s *Store) DecideTicket(ctx context.Context, id string, expectedRevision int64, outcome, disposition string) (task.Ticket, error) {
	tx, err := s.begin(ctx, "decide ticket "+id)
	if err != nil {
		return task.Ticket{}, err
	}
	defer tx.rollback()

	current, err := readTicketForUpdate(ctx, tx, id, expectedRevision)
	if err != nil {
		return task.Ticket{}, err
	}
	if strings.TrimSpace(outcome) == "" || strings.TrimSpace(disposition) == "" {
		return task.Ticket{}, fmt.Errorf("%w: a ticket decision needs an outcome and a disposition", ErrInvalid)
	}
	if current.Decided() {
		return task.Ticket{}, fmt.Errorf("%w: ticket %q is already decided", ErrConflict, id)
	}
	var open int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks
		 WHERE project_key = ? AND json_extract(data, '$.TicketID') = ?
		   AND COALESCE(json_extract(data, '$.Disposition'), '') = ''`,
		current.ProjectKey, id).Scan(&open); err != nil {
		return task.Ticket{}, fmt.Errorf("count the open tasks of ticket %q: %w", id, err)
	}
	if open > 0 {
		return task.Ticket{}, fmt.Errorf("%w: ticket %q has %d open task(s)", ErrTicketHasOpenTasks, id, open)
	}

	next := current
	next.Outcome, next.Disposition = outcome, disposition
	next, err = writeTicket(ctx, tx, current, next)
	if err != nil {
		return task.Ticket{}, err
	}
	if err := tx.commit("decide ticket " + id); err != nil {
		return task.Ticket{}, err
	}
	return next, nil
}

// checkTicketUsable is the ticket half of the dispatch gate. CreateTask and
// ClaimTask call it inside their own transaction when a task names a ticket: the
// ticket must exist, belong to the task's project and be undecided, and linked
// work other than the planner itself needs an accepted plan - at least one
// planner linked to the ticket, each with an accepting disposition. The column
// is deliberately not looked at. A task without a ticket id passes untouched.
func checkTicketUsable(ctx context.Context, tx *tx, t task.Task) error {
	if t.TicketID == "" {
		return nil
	}
	ticket, err := scanTicket(tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE id = ?`, t.TicketID), "ticket "+t.TicketID)
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: ticket %q does not exist", ErrTicketUnavailable, t.TicketID)
	case err != nil:
		return err
	case ticket.ProjectKey != t.ProjectKey:
		return fmt.Errorf("%w: ticket %q belongs to another project", ErrTicketUnavailable, t.TicketID)
	case ticket.Decided():
		return fmt.Errorf("%w: ticket %q is already decided", ErrTicketUnavailable, t.TicketID)
	}
	if t.Role == task.TaskRolePlan {
		return nil
	}
	linked, err := queryFacts[task.Task](ctx, tx,
		`SELECT data FROM tasks WHERE project_key = ?`, t.ProjectKey)
	if err != nil {
		return fmt.Errorf("read the planner tasks of ticket %q: %w", t.TicketID, err)
	}
	planned := false
	for _, existing := range linked {
		if existing.TicketID != t.TicketID || existing.Role != task.TaskRolePlan {
			continue
		}
		if !existing.Accepted() {
			return fmt.Errorf("%w: planner task %s for ticket %s has no accepting disposition",
				ErrTicketUnavailable, existing.ID, t.TicketID)
		}
		planned = true
	}
	if !planned {
		return fmt.Errorf("%w: ticket %s has no planner, so no plan for it was accepted", ErrTicketUnavailable, t.TicketID)
	}
	return nil
}

// checkTaskRole accepts the empty role for legacy and ordinary work, and the
// plan role only when the task is linked to a ticket.
func checkTaskRole(t task.Task) error {
	switch t.Role {
	case "":
		return nil
	case task.TaskRolePlan:
		if strings.TrimSpace(t.TicketID) != "" {
			return nil
		}
		return fmt.Errorf("%w: a planner task needs a ticket id", ErrInvalid)
	default:
		return fmt.Errorf("%w: task %q has unknown role %q", ErrInvalid, t.ID, t.Role)
	}
}

// checkTicketShape rejects a ticket whose stored shape would be unreadable to
// the projections: a blank title or a column other than the two it may rest in.
func checkTicketShape(t task.Ticket) error {
	if strings.TrimSpace(t.Title) == "" {
		return fmt.Errorf("%w: ticket %q needs a title", ErrInvalid, t.ID)
	}
	if t.Column != task.ColumnBacklog && t.Column != task.ColumnTodo {
		return fmt.Errorf("%w: ticket %q column %q must be %s or %s",
			ErrInvalid, t.ID, t.Column, task.ColumnBacklog, task.ColumnTodo)
	}
	return nil
}

// readTicketForUpdate reads a ticket inside tx and proves it is still at the
// revision the caller read.
func readTicketForUpdate(ctx context.Context, tx *tx, id string, expectedRevision int64) (task.Ticket, error) {
	current, err := scanTicket(tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE id = ?`, id), "ticket "+id)
	if err != nil {
		return task.Ticket{}, err
	}
	if current.Revision != expectedRevision {
		return task.Ticket{}, fmt.Errorf("%w: ticket %q is at revision %d, expected %d",
			ErrConflict, id, current.Revision, expectedRevision)
	}
	return current, nil
}

// writeTicket stores next as the revision after current with a compare-and-swap
// on current's revision, and returns the stored facts.
func writeTicket(ctx context.Context, tx *tx, current, next task.Ticket) (task.Ticket, error) {
	next.Revision = current.Revision + 1
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = time.Now().UnixMilli()
	data, err := json.Marshal(next)
	if err != nil {
		return task.Ticket{}, fmt.Errorf("encode ticket %q: %w", current.ID, err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE tickets SET data = ?, revision = revision + 1 WHERE id = ? AND revision = ?`,
		string(data), current.ID, current.Revision)
	if err != nil {
		return task.Ticket{}, fmt.Errorf("update ticket %q: %w", current.ID, err)
	}
	if err := requireOneRow(res, ErrConflict, "update ticket "+current.ID); err != nil {
		return task.Ticket{}, err
	}
	return next, nil
}

func scanTicket(row interface{ Scan(...any) error }, what string) (task.Ticket, error) {
	var data []byte
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return task.Ticket{}, fmt.Errorf("%w: %s", ErrNotFound, what)
		}
		return task.Ticket{}, fmt.Errorf("read %s: %w", what, err)
	}
	var t task.Ticket
	if err := json.Unmarshal(data, &t); err != nil {
		return task.Ticket{}, fmt.Errorf("decode %s: %w", what, err)
	}
	return t, nil
}
