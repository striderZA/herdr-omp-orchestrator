package state

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"herdr-omp-orchestrator/internal/task"
)

// ErrDependentUnavailable reports a dependent that cannot be re-pointed: it has
// started, or it already holds a provisioning slot. Its dependencies were
// checked at admission, so the write changed nothing.
var ErrDependentUnavailable = errors.New("state: the dependent cannot be re-pointed")

// ErrDependencyCycle reports a re-point that would make the project's
// dependencies cyclic. The write changed nothing.
var ErrDependencyCycle = errors.New("state: the re-point would create a dependency cycle")

// RepointTarget is one queued dependent a re-point names, with the revision the
// user confirmed. A write that finds another revision refuses.
type RepointTarget struct {
	TaskID           string
	ExpectedRevision int64
}

// RepointDependents makes each named queued dependent wait on successorID where
// it waited on supersededID, and changes nothing else. Every check is repeated
// here, inside the transaction that writes them (ADR 0002 decision 4): the
// superseded task must still be superseded by successorID; each dependent must be
// unchanged since its confirmed revision, still queued and not holding a slot,
// and still waiting on supersededID; and the project's dependencies must stay
// acyclic with the re-points applied. Any refusal writes nothing.
func (s *Store) RepointDependents(ctx context.Context, supersededID, successorID string, targets []RepointTarget) ([]task.Task, error) {
	if supersededID == "" || successorID == "" || len(targets) == 0 {
		return nil, fmt.Errorf("%w: a re-point needs the superseded task, its successor and at least one dependent", ErrInvalid)
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if seen[target.TaskID] {
			return nil, fmt.Errorf("%w: dependent %q is named twice", ErrInvalid, target.TaskID)
		}
		seen[target.TaskID] = true
	}

	tx, err := s.begin(ctx, "repoint dependents of "+supersededID)
	if err != nil {
		return nil, err
	}
	defer tx.rollback()

	superseded, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, supersededID),
		"task "+supersededID)
	if err != nil {
		return nil, err
	}
	if superseded.Outcome != task.OutcomeSuperseded || superseded.Successor != successorID {
		return nil, fmt.Errorf("%w: task %q no longer names %q as its successor", ErrConflict, supersededID, successorID)
	}
	successor, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, successorID),
		"successor task "+successorID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("%w: successor task %q does not exist", ErrSuccessorUnavailable, successorID)
	case err != nil:
		return nil, err
	case successor.ProjectKey != superseded.ProjectKey:
		return nil, fmt.Errorf("%w: successor task %q belongs to another project", ErrSuccessorUnavailable, successorID)
	}

	all, err := queryFacts[task.Task](ctx, tx, `SELECT data FROM tasks WHERE project_key = ?`, superseded.ProjectKey)
	if err != nil {
		return nil, fmt.Errorf("read the tasks of %q: %w", superseded.ProjectKey, err)
	}
	byID := make(map[string]task.Task, len(all))
	for _, x := range all {
		byID[x.ID] = x
	}

	rewritten := make(map[string][]string, len(targets))
	for _, target := range targets {
		current, ok := byID[target.TaskID]
		if !ok {
			return nil, fmt.Errorf("%w: dependent task %q does not exist", ErrNotFound, target.TaskID)
		}
		if current.Revision != target.ExpectedRevision {
			return nil, fmt.Errorf("%w: task %q is at revision %d, expected %d", ErrConflict, current.ID, current.Revision, target.ExpectedRevision)
		}
		if stage := task.StageOf(current); stage != task.Queued {
			return nil, fmt.Errorf("%w: task %q has started (%s), so its dependencies were checked at admission",
				ErrDependentUnavailable, current.ID, stage)
		}
		holds, err := taskHoldsSlot(ctx, tx, current.ID)
		if err != nil {
			return nil, err
		}
		if holds {
			return nil, fmt.Errorf("%w: task %q holds a provisioning slot", ErrDependentUnavailable, current.ID)
		}
		if !slices.Contains(current.Dependencies, supersededID) {
			return nil, fmt.Errorf("%w: task %q no longer waits on %q", ErrConflict, current.ID, supersededID)
		}
		rewritten[current.ID] = task.ReplaceDependency(current.Dependencies, supersededID, successorID)
	}
	if cycle := task.DependencyCycle(all, rewritten); cycle != nil {
		return nil, fmt.Errorf("%w: %v", ErrDependencyCycle, cycle)
	}

	updated := make([]task.Task, 0, len(targets))
	for _, target := range targets {
		deps := rewritten[target.TaskID]
		next, err := updateTask(ctx, tx, target.TaskID, target.ExpectedRevision, func(x *task.Task) error {
			x.Dependencies = deps
			return nil
		})
		if err != nil {
			return nil, err
		}
		updated = append(updated, next)
	}
	if err := tx.commit("repoint dependents of " + supersededID); err != nil {
		return nil, err
	}
	return updated, nil
}
