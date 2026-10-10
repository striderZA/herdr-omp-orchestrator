package state

import (
	"context"
	"errors"
	"fmt"

	"herdr-omp-orchestrator/internal/task"
)

// ErrSuccessorUnavailable reports a successor that cannot be recorded: it does
// not exist, or it belongs to another project. The write that hit it changed
// nothing.
var ErrSuccessorUnavailable = errors.New("state: the successor cannot be recorded")

// checkSuccessorTx repeats the successor rules inside the write that would record
// them (ADR 0002 decision 4). A decided task's successor is part of that decision
// and never changes. A superseded decision must name a successor; any other
// outcome names none; a successor is a task of the same project other than the
// task itself. Coordinator validation runs first, so a refusal here means the
// state changed under the caller.
func checkSuccessorTx(ctx context.Context, tx *tx, current, next task.Task) error {
	switch {
	case current.Disposition != "" && next.Successor != current.Successor:
		return fmt.Errorf("%w: task %q is already decided, so its successor is final", ErrInvalid, next.ID)
	case next.Outcome == task.OutcomeSuperseded && current.Outcome != task.OutcomeSuperseded && next.Successor == "":
		return fmt.Errorf("%w: task %q is superseded without naming its successor", ErrInvalid, next.ID)
	case next.Successor == "" || next.Successor == current.Successor:
		return nil
	case next.Outcome != task.OutcomeSuperseded:
		return fmt.Errorf("%w: task %q names a successor but its outcome is %q", ErrInvalid, next.ID, next.Outcome)
	case next.Successor == next.ID:
		return fmt.Errorf("%w: task %q cannot be its own successor", ErrInvalid, next.ID)
	}
	successor, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, next.Successor),
		"successor task "+next.Successor)
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: successor task %q does not exist", ErrSuccessorUnavailable, next.Successor)
	case err != nil:
		return err
	case successor.ProjectKey != next.ProjectKey:
		return fmt.Errorf("%w: successor task %q belongs to another project", ErrSuccessorUnavailable, next.Successor)
	}
	return nil
}

// checkTicketSuccessorTx is the ticket half of the successor gate, run inside
// DecideTicket's transaction with the same rules as checkSuccessorTx: a superseded
// decision names a successor ticket of the same project that is not the ticket
// itself, and no other outcome names one. A successor is a reference only; it
// creates no dependency and moves no linked task.
func checkTicketSuccessorTx(ctx context.Context, tx *tx, ticket task.Ticket, outcome, successor string) error {
	superseded := outcome == task.OutcomeSuperseded
	switch {
	case superseded && successor == "":
		return fmt.Errorf("%w: ticket %q is superseded without naming its successor", ErrInvalid, ticket.ID)
	case !superseded && successor != "":
		return fmt.Errorf("%w: ticket %q names a successor but its outcome is %q", ErrInvalid, ticket.ID, outcome)
	case successor == "":
		return nil
	case successor == ticket.ID:
		return fmt.Errorf("%w: ticket %q cannot be its own successor", ErrInvalid, ticket.ID)
	}
	named, err := scanTicket(tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE id = ?`, successor),
		"successor ticket "+successor)
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: successor ticket %q does not exist", ErrSuccessorUnavailable, successor)
	case err != nil:
		return err
	case named.ProjectKey != ticket.ProjectKey:
		return fmt.Errorf("%w: successor ticket %q belongs to another project", ErrSuccessorUnavailable, successor)
	}
	return nil
}
