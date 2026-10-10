package coord

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"herdr-omp-orchestrator/internal/task"
)

// supersededWithSuccessor reviews one worker as superseded by a successor
// dispatched in the same project, and returns both.
func supersededWithSuccessor(t *testing.T, f *coordFixture, pane string) (superseded, successor task.Task) {
	t.Helper()
	superseded = reviewedWorker(t, f, "superseded")
	successor = f.mustDispatch("successor", "The replacement brief.")
	if _, err := f.coord.Review(context.Background(), pane, superseded.ID, task.OutcomeSuperseded, "replaced", successor.ID); err != nil {
		t.Fatalf("supersede %s by %s: %v", superseded.ID, successor.ID, err)
	}
	return superseded, successor
}

// queuedDependent dispatches a task that waits on the given dependencies.
func queuedDependent(t *testing.T, f *coordFixture, title string, deps ...string) task.Task {
	t.Helper()
	st, err := f.dispatch(title, "Brief for "+title+".", []string{"go test ./... ok"}, deps, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch %s: %v", title, err)
	}
	return st
}

func repointDependents(ids ...task.Task) []RepointDependent {
	out := make([]RepointDependent, 0, len(ids))
	for _, x := range ids {
		out = append(out, RepointDependent{TaskID: x.ID, ExpectedRevision: x.Revision})
	}
	return out
}

func TestRepointPreviewIsReadOnlyAndShowsTheNamedDependents(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	superseded, successor := supersededWithSuccessor(t, f, pane)
	dependent := queuedDependent(t, f, "dependent", superseded.ID)
	before := f.taskByID(t, dependent.ID)

	preview, err := f.coord.RepointPreview(ctx, pane, RepointPreviewParams{SupersededTaskID: superseded.ID, DependentIDs: []string{dependent.ID}})
	if err != nil {
		t.Fatalf("RepointPreview: %v", err)
	}
	if preview.SupersededTaskID != superseded.ID || preview.SuccessorTaskID != successor.ID {
		t.Fatalf("preview names %s -> %s, want %s -> %s", preview.SupersededTaskID, preview.SuccessorTaskID, superseded.ID, successor.ID)
	}
	if len(preview.Dependents) != 1 || preview.Dependents[0].TaskID != dependent.ID ||
		preview.Dependents[0].Revision != dependent.Revision || preview.Dependents[0].Stage != task.Queued {
		t.Fatalf("preview dependents = %+v, want the queued %s at revision %d", preview.Dependents, dependent.ID, dependent.Revision)
	}
	if after := f.taskByID(t, dependent.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("a preview changed the dependent:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestRepointRewritesOnlyTheNamedDependentsAndReleasesThemWhenTheSuccessorAccepts(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	superseded, successor := supersededWithSuccessor(t, f, pane)
	first := queuedDependent(t, f, "first", superseded.ID)
	other := f.mustDispatch("other", "Another brief.")
	second := queuedDependent(t, f, "second", superseded.ID, other.ID)
	unnamed := queuedDependent(t, f, "unnamed", superseded.ID)
	unnamedBefore := f.taskByID(t, unnamed.ID)

	preview, err := f.coord.RepointPreview(ctx, pane, RepointPreviewParams{SupersededTaskID: superseded.ID, DependentIDs: []string{first.ID, second.ID}})
	if err != nil {
		t.Fatalf("RepointPreview: %v", err)
	}
	result, err := f.coord.Repoint(ctx, pane, RepointParams{
		SupersededTaskID: superseded.ID,
		SuccessorTaskID:  preview.SuccessorTaskID,
		Dependents:       []RepointDependent{{TaskID: first.ID, ExpectedRevision: preview.Dependents[0].Revision}, {TaskID: second.ID, ExpectedRevision: preview.Dependents[1].Revision}},
	})
	if err != nil {
		t.Fatalf("Repoint: %v", err)
	}
	if !reflect.DeepEqual(result.TaskIDs, []string{first.ID, second.ID}) {
		t.Fatalf("Repoint named %v, want the two dependents", result.TaskIDs)
	}
	if got := f.taskByID(t, first.ID).Dependencies; !reflect.DeepEqual(got, []string{successor.ID}) {
		t.Fatalf("first waits on %v, want its successor", got)
	}
	if got := f.taskByID(t, second.ID).Dependencies; !reflect.DeepEqual(got, []string{successor.ID, other.ID}) {
		t.Fatalf("second waits on %v, want the successor in the superseded entry's place", got)
	}
	if after := f.taskByID(t, unnamed.ID); !reflect.DeepEqual(after, unnamedBefore) {
		t.Fatalf("an unnamed dependent changed:\nbefore %+v\nafter  %+v", unnamedBefore, after)
	}

	// The successor is not accepting yet, so the re-pointed dependents stay queued.
	if stage := task.StageOf(f.taskByID(t, first.ID)); stage != task.Queued {
		t.Fatalf("a re-pointed dependent of a successor not yet accepted is %q, want queued", stage)
	}
	f.satisfyReview(f.taskByID(t, successor.ID))
	if _, err := f.coord.Review(ctx, pane, successor.ID, task.OutcomeIntegrated, "accepted", ""); err != nil {
		t.Fatalf("accept the successor: %v", err)
	}
	if phase := f.taskByID(t, first.ID).Phase; phase == task.Intent {
		t.Fatalf("accepting the successor did not release the re-pointed dependent (phase %q)", phase)
	}
	if after := f.taskByID(t, unnamed.ID); task.StageOf(after) != task.Queued {
		t.Fatalf("an unnamed dependent was released by the re-point: %q", task.StageOf(after))
	}
}

func TestRepointRefusesWhatItMustNotRewrite(t *testing.T) {
	cases := map[string]func(t *testing.T, f *coordFixture, pane string) (superseded task.Task, dependents []task.Task){
		"a dependent that has started": func(t *testing.T, f *coordFixture, pane string) (task.Task, []task.Task) {
			superseded, _ := supersededWithSuccessor(t, f, pane)
			started := queuedDependent(t, f, "started", superseded.ID)
			f.block(started, func(next *task.Task) { next.Phase = task.PromptSubmitted })
			return superseded, []task.Task{f.taskByID(t, started.ID)}
		},
		"a dependent that does not wait on the superseded task": func(t *testing.T, f *coordFixture, pane string) (task.Task, []task.Task) {
			superseded, _ := supersededWithSuccessor(t, f, pane)
			unrelated := queuedDependent(t, f, "unrelated")
			return superseded, []task.Task{unrelated}
		},
		"a dependent another project owns": func(t *testing.T, f *coordFixture, pane string) (task.Task, []task.Task) {
			superseded, _ := supersededWithSuccessor(t, f, pane)
			foreign := task.Task{ID: "orch_foreign_dep", ProjectKey: foreignProject, Title: "someone else's", Dependencies: []string{superseded.ID}, Phase: task.Intent}
			if err := f.store.CreateTask(context.Background(), foreign); err != nil {
				t.Fatalf("seed another project's dependent: %v", err)
			}
			return superseded, []task.Task{foreign}
		},
		"a superseded task that names no successor": func(t *testing.T, f *coordFixture, pane string) (task.Task, []task.Task) {
			legacy := reviewedWorker(t, f, "legacy")
			f.seedLegacyTask(legacy.ID, func(next *task.Task) {
				next.Outcome, next.Disposition = task.OutcomeSuperseded, "replaced by hand"
			})
			dependent := queuedDependent(t, f, "dependent", legacy.ID)
			return legacy, []task.Task{dependent}
		},
		"a rejected task, which never names a successor": func(t *testing.T, f *coordFixture, pane string) (task.Task, []task.Task) {
			rejected := reviewedWorker(t, f, "rejected")
			if _, err := f.coord.Review(context.Background(), pane, rejected.ID, task.OutcomeRejected, "no", ""); err != nil {
				t.Fatalf("reject: %v", err)
			}
			dependent := queuedDependent(t, f, "dependent", rejected.ID)
			return rejected, []task.Task{dependent}
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newCoordFixture(t, fixturePlan{})
			pane := primaryPane(t, f)
			superseded, dependents := arrange(t, f, pane)
			before := snapshotTaskFacts(t, f, superseded, dependents)
			ids := make([]string, 0, len(dependents))
			for _, x := range dependents {
				ids = append(ids, x.ID)
			}
			if _, err := f.coord.RepointPreview(ctx, pane, RepointPreviewParams{SupersededTaskID: superseded.ID, DependentIDs: ids}); !errors.Is(err, ErrRepointRefused) && !errors.Is(err, ErrOwnership) {
				t.Fatalf("RepointPreview = %v, want a refusal", err)
			}
			if _, err := f.coord.Repoint(ctx, pane, RepointParams{SupersededTaskID: superseded.ID, SuccessorTaskID: "tkt_none", Dependents: repointDependents(dependents...)}); err == nil {
				t.Fatal("Repoint was accepted")
			}
			if after := snapshotTaskFacts(t, f, superseded, dependents); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused re-point changed the facts:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// snapshotTaskFacts reads the tasks a re-point could touch.
func snapshotTaskFacts(t *testing.T, f *coordFixture, superseded task.Task, dependents []task.Task) map[string]task.Task {
	t.Helper()
	out := map[string]task.Task{superseded.ID: f.taskByID(t, superseded.ID)}
	for _, x := range dependents {
		out[x.ID] = f.taskByID(t, x.ID)
	}
	return out
}

func TestRepointRefusesACycleIncludingOneThroughARepointedDependency(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	superseded, successor := supersededWithSuccessor(t, f, pane)
	dependent := queuedDependent(t, f, "dependent", superseded.ID)
	successor = f.block(successor, func(next *task.Task) { next.Dependencies = []string{dependent.ID} })

	preview := RepointParams{SupersededTaskID: superseded.ID, SuccessorTaskID: successor.ID, Dependents: repointDependents(dependent)}
	if _, err := f.coord.Repoint(ctx, pane, preview); !errors.Is(err, ErrRepointCycle) {
		t.Fatalf("a successor that waits on the dependent = %v, want ErrRepointCycle", err)
	}
	if got := f.taskByID(t, dependent.ID).Dependencies; !reflect.DeepEqual(got, []string{superseded.ID}) {
		t.Fatalf("a refused cycle rewrote the dependent: %v", got)
	}

	// The cycle closes only through a second, unnamed task: successor -> middle -> dependent -> successor.
	f.block(successor, func(next *task.Task) { next.Dependencies = []string{"orch_middle"} })
	middle := task.Task{ID: "orch_middle", ProjectKey: f.projectKey(), Title: "middle", Dependencies: []string{dependent.ID}, Phase: task.Intent}
	if err := f.store.CreateTask(ctx, middle); err != nil {
		t.Fatalf("seed the middle task: %v", err)
	}
	if _, err := f.coord.Repoint(ctx, pane, preview); !errors.Is(err, ErrRepointCycle) {
		t.Fatalf("a cycle through an unnamed task = %v, want ErrRepointCycle", err)
	}
}

func TestRepointRefusesAConfirmationThatNoLongerMatches(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	superseded, successor := supersededWithSuccessor(t, f, pane)
	dependent := queuedDependent(t, f, "dependent", superseded.ID)
	confirmed := repointDependents(dependent)

	f.block(dependent, func(next *task.Task) { next.Title = "changed after the preview" })
	if _, err := f.coord.Repoint(ctx, pane, RepointParams{SupersededTaskID: superseded.ID, SuccessorTaskID: successor.ID, Dependents: confirmed}); !errors.Is(err, ErrRepointChanged) {
		t.Fatalf("a dependent that changed after its preview = %v, want ErrRepointChanged", err)
	}

	other := f.mustDispatch("another successor", "Some other replacement brief.")
	fresh := f.taskByID(t, dependent.ID)
	if _, err := f.coord.Repoint(ctx, pane, RepointParams{SupersededTaskID: superseded.ID, SuccessorTaskID: other.ID, Dependents: repointDependents(fresh)}); !errors.Is(err, ErrRepointChanged) {
		t.Fatalf("a successor other than the one confirmed = %v, want ErrRepointChanged", err)
	}
	if got := f.taskByID(t, dependent.ID).Dependencies; !reflect.DeepEqual(got, []string{superseded.ID}) {
		t.Fatalf("a refused re-point rewrote the dependent: %v", got)
	}
}
