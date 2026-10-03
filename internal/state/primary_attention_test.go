package state_test

// Issue #17: task.Project.PrimaryAttention is an additive fact. The project is
// stored as JSON, so a row written before the field existed must read as "no
// attention", and the fact must survive every other project update untouched.

import (
	"context"
	"testing"

	"herdr-omp-orchestrator/internal/task"
)

const legacyProjectJSON = `{"Key":"repo-legacy","RepoWorkspaceID":"w1","CurrentWorkspaceID":"w1","TerminalID":"term-1",` +
	`"LaunchPaneID":"w1:p1","PublicPaneID":"w1:p1","AgentName":"omp-primary","OMPSessionID":"omp-primary-1"}`

func TestStoreLegacyProjectRowReadsAsNoPrimaryAttention(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)

	raw := rawState(t, dir)
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects (key, data) VALUES (?, ?)`, "repo-legacy", legacyProjectJSON); err != nil {
		t.Fatalf("insert the legacy project row: %v", err)
	}

	got, err := s.Project(ctx, "repo-legacy")
	if err != nil {
		t.Fatalf("Project(legacy): %v", err)
	}
	if got.PrimaryAttention != "" {
		t.Errorf("legacy project PrimaryAttention = %q, want none", got.PrimaryAttention)
	}
	if got.OMPSessionID != "omp-primary-1" || got.LaunchPaneID != "w1:p1" {
		t.Errorf("legacy project lost its facts: %+v", got)
	}
	byPane, err := s.ProjectByLaunchPane(ctx, "w1:p1")
	if err != nil || byPane.PrimaryAttention != "" {
		t.Errorf("ProjectByLaunchPane(legacy) = (%+v, %v), want a project with no attention", byPane, err)
	}
	all, err := s.Projects(ctx)
	if err != nil || len(all) != 1 || all[0].PrimaryAttention != "" {
		t.Errorf("Projects() = (%+v, %v), want the legacy project with no attention", all, err)
	}
}

func TestStoreProjectPrimaryAttentionRoundTripsAndOtherUpdatesKeepIt(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	project := task.Project{Key: "repo-a", RepoWorkspaceID: "w1", LaunchPaneID: launchPane, PublicPaneID: launchPane, OMPSessionID: "omp-primary-1"}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if got, err := s.Project(ctx, "repo-a"); err != nil || got.PrimaryAttention != "" {
		t.Fatalf("a new project = (%+v, %v), want no attention", got, err)
	}

	const reason = "the recorded primary pane w1:p2 is absent from Herdr's snapshot"
	if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.PrimaryAttention = reason
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(record): %v", err)
	}
	got, err := s.Project(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got.PrimaryAttention != reason {
		t.Fatalf("PrimaryAttention = %q, want %q", got.PrimaryAttention, reason)
	}

	// Another writer that knows nothing of the fact (a pane move, a rebind) must
	// not erase it: the fact is only retired by a write that means to.
	if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.PublicPaneID = "w2:p1"
		p.CurrentWorkspaceID = "w2"
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(unrelated): %v", err)
	}
	moved, err := s.Project(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Project after the move: %v", err)
	}
	if moved.PrimaryAttention != reason || moved.PublicPaneID != "w2:p1" {
		t.Errorf("an unrelated update changed the fact: %+v", moved)
	}
	if byPane, err := s.ProjectByLaunchPane(ctx, launchPane); err != nil || byPane.PrimaryAttention != reason {
		t.Errorf("ProjectByLaunchPane = (%+v, %v), want the recorded attention", byPane, err)
	}

	if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.PrimaryAttention = ""
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(clear): %v", err)
	}
	cleared, err := s.Project(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Project after clearing: %v", err)
	}
	if cleared.PrimaryAttention != "" || cleared.OMPSessionID != "omp-primary-1" {
		t.Errorf("cleared project = %+v, want no attention and every other fact kept", cleared)
	}
}
