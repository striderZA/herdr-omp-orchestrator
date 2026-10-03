package state

import (
	"errors"
	"fmt"

	"herdr-omp-orchestrator/internal/task"
)

// ErrDuplicateOpen reports that an undecided task of the same project already
// holds the work a claim asked for (issue #4). The claim changes nothing; the
// DuplicateError it is wrapped in carries the task that holds the work. What
// counts as the same work is task.OpenHolderOf, which the coordinator's
// read-only pre-check uses too, so the two can never disagree.
var ErrDuplicateOpen = errors.New("state: an open task already holds this work")

// DuplicateError is the refusal of a claim for work an open task holds.
type DuplicateError struct {
	// Existing is the open task, as it is stored now.
	Existing task.Task
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("%v: task %s", ErrDuplicateOpen, e.Existing.ID)
}

func (e *DuplicateError) Unwrap() error { return ErrDuplicateOpen }
