package state_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// successorTask is a queued task of repo-a with its own pane, no reserved paths
// and the given dependencies. Two tasks of one project cannot share a pane or
// reserve the same paths, so every fixture task needs its own.
func successorTask(id string, n int, deps ...string) task.Task {
	x := seedTask(id)
	x.LaunchPaneID = fmt.Sprintf("w2:p%d", 200+n)
	x.PublicPaneID = x.LaunchPaneID
	x.ReservedPaths = nil
	x.Dependencies = deps
	return x
}

func createSuccessorTask(t *testing.T, s *state.Store, x task.Task) {
	t.Helper()
	if err := s.CreateTask(context.Background(), x); err != nil {
		t.Fatalf("CreateTask(%q): %v", x.ID, err)
	}
}

// decideTask closes a task through the same transaction a decision uses.
func decideTask(t *testing.T, s *state.Store, id, outcome, successor string) (task.Task, error) {
	t.Helper()
	ctx := context.Background()
	current, err := s.Task(ctx, id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return s.UpdateTask(ctx, id, current.Revision, func(x *task.Task) error {
		x.Outcome, x.Disposition, x.Successor = outcome, outcome, successor
		return nil
	})
}

func TestStoreSupersededTaskNamesASuccessorOfItsProject(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createSuccessorTask(t, s, successorTask("t1", 1))
	createSuccessorTask(t, s, successorTask("t2", 2))

	if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, "t2"); err != nil {
		t.Fatalf("supersede t1 by t2: %v", err)
	}
	got, err := s.Task(ctx, "t1")
	if err != nil || got.Outcome != task.OutcomeSuperseded || got.Successor != "t2" {
		t.Fatalf("stored decision = %+v, %v; want superseded naming t2", got, err)
	}
}

func TestStoreRefusesASuccessorThatIsMissingForeignOrSelfAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createSuccessorTask(t, s, successorTask("t1", 1))
	foreign := successorTask("f1", 9)
	foreign.ProjectKey = "repo-b"
	createSuccessorTask(t, s, foreign)
	before, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("read t1: %v", err)
	}

	for _, c := range []struct {
		name      string
		successor string
		want      error
	}{
		{"missing", "orch_missing", state.ErrSuccessorUnavailable},
		{"another project's task", "f1", state.ErrSuccessorUnavailable},
		{"the task itself", "t1", state.ErrInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, c.successor); !errors.Is(err, c.want) {
				t.Fatalf("superseded by %q = %v, want %v", c.successor, err, c.want)
			}
			if after, _ := s.Task(ctx, "t1"); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused successor changed t1:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

func TestStoreWritesASuccessorOnlyWithASupersededOutcomeAndAlwaysWithOne(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createSuccessorTask(t, s, successorTask("t1", 1))
	createSuccessorTask(t, s, successorTask("t2", 2))
	before, _ := s.Task(ctx, "t1")

	if _, err := decideTask(t, s, "t1", task.OutcomeRejected, "t2"); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("rejected naming a successor = %v, want ErrInvalid", err)
	}
	if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, ""); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("superseded without a successor = %v, want ErrInvalid", err)
	}
	if after, _ := s.Task(ctx, "t1"); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused decision changed t1:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestStoreDecidedTaskKeepsItsSuccessor(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createSuccessorTask(t, s, successorTask("t1", 1))
	createSuccessorTask(t, s, successorTask("t2", 2))
	createSuccessorTask(t, s, successorTask("t3", 3))
	if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, "t2"); err != nil {
		t.Fatalf("supersede t1 by t2: %v", err)
	}
	current, _ := s.Task(ctx, "t1")
	if _, err := s.UpdateTask(ctx, "t1", current.Revision, func(x *task.Task) error {
		x.Successor = "t3"
		return nil
	}); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("changing the successor of a decided task = %v, want ErrInvalid", err)
	}
	// An unrelated write to a decided task keeps the successor it was decided with.
	if _, err := s.UpdateTask(ctx, "t1", current.Revision, func(x *task.Task) error {
		x.Attention = "cleanup pending"
		return nil
	}); err != nil {
		t.Fatalf("an unrelated write to a superseded task: %v", err)
	}
	if after, _ := s.Task(ctx, "t1"); after.Successor != "t2" {
		t.Fatalf("successor after an unrelated write = %q, want t2", after.Successor)
	}
}

func TestStoreDecideTicketChecksItsSuccessorInTheWrite(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	mustCreateTicket(t, s, seedTicket("tkt_b", "repo-a"))
	mustCreateTicket(t, s, seedTicket("tkt_c", "repo-b"))

	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeSuperseded, "replaced", "tkt_c"); !errors.Is(err, state.ErrSuccessorUnavailable) {
		t.Fatalf("a successor of another project = %v, want ErrSuccessorUnavailable", err)
	}
	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeSuperseded, "replaced", "tkt_missing"); !errors.Is(err, state.ErrSuccessorUnavailable) {
		t.Fatalf("a missing successor = %v, want ErrSuccessorUnavailable", err)
	}
	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeSuperseded, "replaced", ""); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("superseded without a successor = %v, want ErrInvalid", err)
	}
	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeSuperseded, "replaced", "tkt_a"); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("a ticket as its own successor = %v, want ErrInvalid", err)
	}
	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeIntegrated, "merged", "tkt_b"); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("integrated naming a successor = %v, want ErrInvalid", err)
	}
	if after, _ := s.Ticket(ctx, "tkt_a"); after.Decided() || after.Revision != 1 {
		t.Fatalf("a refused decision changed the ticket: %+v", after)
	}

	decided, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeSuperseded, "replaced", "tkt_b")
	if err != nil || decided.Successor != "tkt_b" || decided.Outcome != task.OutcomeSuperseded {
		t.Fatalf("superseded by tkt_b = %+v, %v", decided, err)
	}
	if reread, _ := s.Ticket(ctx, "tkt_a"); reread.Successor != "tkt_b" {
		t.Fatalf("stored successor = %q, want tkt_b", reread.Successor)
	}
}

func TestStoreRepointDependentsRewritesOnlyTheNamedQueuedDependents(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createSuccessorTask(t, s, successorTask("t1", 1))
	createSuccessorTask(t, s, successorTask("t2", 2))
	createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
	createSuccessorTask(t, s, successorTask("d2", 4, "t1", "other"))
	createSuccessorTask(t, s, successorTask("d3", 5, "t1"))
	if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, "t2"); err != nil {
		t.Fatalf("supersede t1 by t2: %v", err)
	}
	d1, _ := s.Task(ctx, "d1")
	d2, _ := s.Task(ctx, "d2")
	d3Before, _ := s.Task(ctx, "d3")

	updated, err := s.RepointDependents(ctx, "t1", "t2", []state.RepointTarget{
		{TaskID: "d1", ExpectedRevision: d1.Revision},
		{TaskID: "d2", ExpectedRevision: d2.Revision},
	})
	if err != nil || len(updated) != 2 {
		t.Fatalf("RepointDependents = %d tasks, %v; want 2, nil", len(updated), err)
	}
	if got, _ := s.Task(ctx, "d1"); !reflect.DeepEqual(got.Dependencies, []string{"t2"}) || got.Revision != d1.Revision+1 {
		t.Fatalf("d1 = deps %v revision %d; want [t2] at revision %d", got.Dependencies, got.Revision, d1.Revision+1)
	}
	if got, _ := s.Task(ctx, "d2"); !reflect.DeepEqual(got.Dependencies, []string{"t2", "other"}) {
		t.Fatalf("d2 deps = %v, want the superseded entry replaced in place: [t2 other]", got.Dependencies)
	}
	if got, _ := s.Task(ctx, "d3"); !reflect.DeepEqual(got, d3Before) {
		t.Fatalf("an unnamed dependent changed:\nbefore %+v\nafter  %+v", d3Before, got)
	}
	if got, _ := s.Task(ctx, "t1"); got.Successor != "t2" || got.Outcome != task.OutcomeSuperseded {
		t.Fatalf("the superseded task changed: %+v", got)
	}
}

func TestStoreRepointDependentsRefusesAndWritesNothing(t *testing.T) {
	type setup func(t *testing.T, s *state.Store) (superseded, successor string, targets []state.RepointTarget)
	cases := map[string]setup{
		"a dependent that has started": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			started := successorTask("d1", 3, "t1")
			started.Phase = task.PromptSubmitted
			createSuccessorTask(t, s, started)
			return "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}}
		},
		"a dependent holding a provisioning slot": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
			if took, _, err := s.ClaimProvisioningSlot(context.Background(), "d1", 3); err != nil || !took {
				t.Fatalf("claim the slot of d1 = %v, %v", took, err)
			}
			return "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}}
		},
		"a revision the user did not confirm": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
			if _, err := s.UpdateTask(context.Background(), "d1", 1, func(x *task.Task) error {
				x.Title = "changed after the preview"
				return nil
			}); err != nil {
				t.Fatalf("change d1: %v", err)
			}
			return "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}}
		},
		"a dependency that no longer names the superseded task": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			createSuccessorTask(t, s, successorTask("d1", 3))
			return "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}}
		},
		"a successor the superseded task does not name": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
			return "t1", "t9", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}}
		},
		"a re-point that closes a cycle through the successor": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
			setDependencies(t, s, "t2", "d1")
			return "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}}
		},
		"a dependent that is its own successor": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			updated := setDependencies(t, s, "t2", "t1")
			return "t1", "t2", []state.RepointTarget{{TaskID: "t2", ExpectedRevision: updated.Revision}}
		},
		"the same dependent named twice": func(t *testing.T, s *state.Store) (string, string, []state.RepointTarget) {
			createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
			return "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}, {TaskID: "d1", ExpectedRevision: 1}}
		},
	}
	want := map[string]error{
		"a dependent that has started":                          state.ErrDependentUnavailable,
		"a dependent holding a provisioning slot":               state.ErrDependentUnavailable,
		"a revision the user did not confirm":                   state.ErrConflict,
		"a dependency that no longer names the superseded task": state.ErrConflict,
		"a successor the superseded task does not name":         state.ErrConflict,
		"a re-point that closes a cycle through the successor":  state.ErrDependencyCycle,
		"a dependent that is its own successor":                 state.ErrDependencyCycle,
		"the same dependent named twice":                        state.ErrInvalid,
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := open(t, t.TempDir())
			createSuccessorTask(t, s, successorTask("t1", 1))
			createSuccessorTask(t, s, successorTask("t2", 2))
			if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, "t2"); err != nil {
				t.Fatalf("supersede t1 by t2: %v", err)
			}
			superseded, successor, targets := arrange(t, s)
			before := snapshotTasks(t, s, "t1", "t2", "d1", "t9")

			_, err := s.RepointDependents(ctx, superseded, successor, targets)
			if !errors.Is(err, want[name]) {
				t.Fatalf("the refusal = %v, want %v", err, want[name])
			}
			if after := snapshotTasks(t, s, "t1", "t2", "d1", "t9"); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused re-point changed the store (%v):\nbefore %+v\nafter  %+v", err, before, after)
			}
		})
	}
}

// snapshotTasks reads the named tasks; a missing one is recorded as its absence
// so a write that creates it would show up as a change.
func snapshotTasks(t *testing.T, s *state.Store, ids ...string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, id := range ids {
		x, err := s.Task(context.Background(), id)
		if errors.Is(err, state.ErrNotFound) {
			out[id] = "absent"
			continue
		}
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		out[id] = x
	}
	return out
}

func TestStoreRepointDependentsRefusesCycleWithTheCycleErrorClass(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createSuccessorTask(t, s, successorTask("t1", 1))
	createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
	createSuccessorTask(t, s, successorTask("t2", 4, "d1"))
	if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, "t2"); err != nil {
		t.Fatalf("supersede t1 by t2: %v", err)
	}
	_, err := s.RepointDependents(ctx, "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}})
	if !errors.Is(err, state.ErrDependencyCycle) {
		t.Fatalf("a cycle through the successor = %v, want ErrDependencyCycle", err)
	}
}

func TestStoreRepointDependentsRefusesWhileAnotherWriterChangedTheDependent(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createSuccessorTask(t, s, successorTask("t1", 1))
	createSuccessorTask(t, s, successorTask("t2", 2))
	createSuccessorTask(t, s, successorTask("d1", 3, "t1"))
	if _, err := decideTask(t, s, "t1", task.OutcomeSuperseded, "t2"); err != nil {
		t.Fatalf("supersede t1 by t2: %v", err)
	}
	// Another writer re-points d1 first; the confirmation that followed the old revision must lose.
	if _, err := s.RepointDependents(ctx, "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}}); err != nil {
		t.Fatalf("first re-point: %v", err)
	}
	_, err := s.RepointDependents(ctx, "t1", "t2", []state.RepointTarget{{TaskID: "d1", ExpectedRevision: 1}})
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("a repeat against the old revision = %v, want ErrConflict", err)
	}
}

// setDependencies rewrites a queued task's dependencies at its current revision
// and returns the task as stored.
func setDependencies(t *testing.T, s *state.Store, id string, deps ...string) task.Task {
	t.Helper()
	current, err := s.Task(context.Background(), id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	stored, err := s.UpdateTask(context.Background(), id, current.Revision, func(x *task.Task) error {
		x.Dependencies = deps
		return nil
	})
	if err != nil {
		t.Fatalf("set the dependencies of %s: %v", id, err)
	}
	return stored
}
