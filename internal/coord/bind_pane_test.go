package coord

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// The trigger from issue 25: the named pane hosted another repository's
// primary, that agent exited, and the shell now sits in this repository. The
// pane is still recorded for the other project, so bind must not adopt it: a
// second record on it would make the bridge resolve either project. The new
// primary gets its own workspace and the other project's record is untouched.
func TestBindDoesNotAdoptAPaneAnotherProjectRecords(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{BindPaneID: "w9:p2", BindPaneWorkspace: "w9"})
	f.plan.BindPaneCwd = f.root
	f.plan.BindPaneAgent = ""
	f.writePlan()
	other := task.Project{
		Key:                filepath.Join(t.TempDir(), "repo-a"),
		RepoWorkspaceID:    "w9",
		CurrentWorkspaceID: "w9",
		LaunchPaneID:       "w9:p2",
		PublicPaneID:       "w9:p2",
		OMPSessionID:       "omp-session-of-a",
	}
	if err := f.store.CreateProject(ctx, other); err != nil {
		t.Fatalf("record the other project: %v", err)
	}

	project, err := f.bind("w9", "w9:p2")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if project.LaunchPaneID == "w9:p2" || project.PublicPaneID == "w9:p2" || project.RepoWorkspaceID == "w9" {
		t.Fatalf("project = %+v, want a dedicated workspace, not the pane project A records", project)
	}
	if f.countCalls("workspace create") != 1 {
		t.Errorf("workspace create calls = %d, want 1", f.countCalls("workspace create"))
	}
	for _, call := range f.calls() {
		if len(call.Argv) >= 5 && call.Argv[0] == "agent" && call.Argv[1] == "start" && call.Argv[4] == "w9:p2" {
			t.Fatalf("agent start used the pane another project records: %q", call.Argv)
		}
	}
	owner, err := f.store.ProjectByLaunchPane(ctx, "w9:p2")
	if err != nil || owner.Key != other.Key || owner.OMPSessionID != other.OMPSessionID {
		t.Errorf("ProjectByLaunchPane(w9:p2) = (%+v, %v), want project A unchanged", owner, err)
	}
	if binding, err := f.store.ByPane(ctx, project.LaunchPaneID); err != nil || binding.Project.Key != project.Key {
		t.Errorf("ByPane(%s) = (%+v, %v), want only the new primary", project.LaunchPaneID, binding, err)
	}
}

// A pane a worker records is not adopted either.
func TestBindDoesNotAdoptAPaneATaskRecords(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{BindPaneID: "w9:p2", BindPaneWorkspace: "w9"})
	f.plan.BindPaneCwd = f.root
	f.plan.BindPaneAgent = ""
	f.writePlan()
	if err := f.store.CreateTask(ctx, task.Task{
		ID: "orch_worker", ProjectKey: filepath.Join(t.TempDir(), "repo-a"), Phase: task.Intent,
		LaunchPaneID: "w9:p2", PublicPaneID: "w9:p2",
	}); err != nil {
		t.Fatalf("record the worker: %v", err)
	}

	project, err := f.bind("w9", "w9:p2")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if project.LaunchPaneID == "w9:p2" {
		t.Fatalf("project = %+v, want a dedicated workspace, not the worker's pane", project)
	}
}

// The store is the backstop: when the pane bind would record is already
// another project's (here the created workspace's root pane, as after Herdr
// reused an id a stale record still names), bind is refused with a stable
// error before any agent starts, and nothing is recorded for this root.
func TestBindRefusesWhenTheStoreReportsThePaneTaken(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	// The fixture's first `workspace create` answers w1 with root pane w1:p1.
	stale := task.Project{Key: filepath.Join(t.TempDir(), "repo-a"), LaunchPaneID: "w1:p1", PublicPaneID: "w1:p1"}
	if err := f.store.CreateProject(ctx, stale); err != nil {
		t.Fatalf("record the stale project: %v", err)
	}

	_, err := f.bind("", "")
	if !errors.Is(err, ErrAlreadyBound) || !errors.Is(err, state.ErrPaneTaken) {
		t.Fatalf("Bind = %v, want ErrAlreadyBound wrapping state.ErrPaneTaken", err)
	}
	if f.countCalls("agent start") != 0 {
		t.Errorf("agent start calls = %d, want 0 for a refused bind", f.countCalls("agent start"))
	}
	if _, err := f.store.Project(ctx, f.root); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the refused bind recorded a project: %v", err)
	}
}
