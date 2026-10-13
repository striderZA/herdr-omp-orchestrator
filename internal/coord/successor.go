package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// Errors of the successor rules. Callers compare them with errors.Is.
var (
	// ErrSuccessorRequired refuses a superseded decision that names no successor:
	// it must say where the work went.
	ErrSuccessorRequired = errors.New("coord: a superseded decision must name the successor that replaces the work")
	// ErrSuccessorNotAllowed refuses a successor on any outcome but superseded.
	ErrSuccessorNotAllowed = errors.New("coord: only a superseded decision may name a successor")
	// ErrSuccessorUnknown refuses a successor task that is missing or belongs to
	// another project. The two answers are the same, so an id cannot be probed
	// across projects (the rule ownedTicket already applies to tickets).
	ErrSuccessorUnknown = errors.New("coord: unknown successor task")
	// ErrSuccessorSelf refuses a decision that names its own subject as successor.
	ErrSuccessorSelf = errors.New("coord: a decision cannot name its own subject as the successor")
)

// successorLimit bounds a successor id. Ids are short, so anything longer is not one.
const successorLimit = 64

// decisionSubject is the record a decision is about: a task or a ticket of one
// project. A ticket's successor is a ticket; a task's successor is a task.
type decisionSubject struct {
	projectKey string
	id         string
	ticket     bool
}

// normalizeSuccessor trims a successor id and bounds it. An empty id means none.
func normalizeSuccessor(value string) (string, error) {
	text := strings.TrimSpace(value)
	if task.TextLength(text) > successorLimit {
		return "", fmt.Errorf("%w: the successor id is longer than %d characters", ErrInvalidRequest, successorLimit)
	}
	return text, nil
}

// checkSuccessor is the one validation point for a decision's successor, shared by
// task and ticket decisions (ADR 0001 decision 4; issue #61). It requires exactly
// the successors an outcome allows, refuses the subject itself, and names an
// existing record of the same project. Every decision path passes through it
// before anything is written, and the store repeats the same rules inside the
// transaction that writes them (ADR 0002 decision 4). It returns the normalized id.
func (c *Coordinator) checkSuccessor(ctx context.Context, subject decisionSubject, outcome, successor string) (string, error) {
	id, err := normalizeSuccessor(successor)
	if err != nil {
		return "", err
	}
	switch {
	case outcome == task.OutcomeSuperseded && id == "":
		return "", fmt.Errorf("%w: %s", ErrSuccessorRequired, subject.id)
	case outcome != task.OutcomeSuperseded && id != "":
		return "", fmt.Errorf("%w: %s records %q, not superseded", ErrSuccessorNotAllowed, subject.id, outcome)
	case id == "":
		return "", nil
	case id == subject.id:
		return "", fmt.Errorf("%w: %s", ErrSuccessorSelf, subject.id)
	}
	if subject.ticket {
		if _, err := c.ownedTicket(ctx, subject.projectKey, id); err != nil {
			return "", err
		}
		return id, nil
	}
	if _, err := c.successorTask(ctx, subject.projectKey, id); err != nil {
		return "", err
	}
	return id, nil
}

// successorTask reads a task a decision or a re-point names. A missing task and a
// task of another project are the same ErrSuccessorUnknown.
func (c *Coordinator) successorTask(ctx context.Context, projectKey, id string) (task.Task, error) {
	st, err := c.store.Task(ctx, id)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return task.Task{}, fmt.Errorf("%w: %s", ErrSuccessorUnknown, id)
	case err != nil:
		return task.Task{}, err
	case !samePath(st.ProjectKey, projectKey):
		return task.Task{}, fmt.Errorf("%w: %s", ErrSuccessorUnknown, id)
	}
	return st, nil
}
