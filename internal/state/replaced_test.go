package state_test

// Issue #18: task.Project.Replaced is an additive fact. The project is stored as
// JSON, so a row written before the field existed must read as "nothing was
// replaced", and the displaced identities a reattach records must survive both a
// read and every other project update.

import (
	"context"
	"reflect"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestStoreProjectReplacedRoundTripsAndOtherUpdatesKeepIt(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	project := task.Project{Key: "repo-a", RepoWorkspaceID: "w1", LaunchPaneID: launchPane, PublicPaneID: launchPane, OMPSessionID: "omp-primary-1"}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if got, err := s.Project(ctx, "repo-a"); err != nil || len(got.Replaced) != 0 {
		t.Fatalf("a new project = (%+v, %v), want nothing replaced", got, err)
	}

	displaced := task.ReplacedPrimary{
		LaunchPaneID: "w1:p1", PublicPaneID: "w1:p1", TerminalID: "term-1", WorkspaceID: "w1",
		OMPSessionID: "omp-primary-0", Reason: "the recorded primary pane w1:p1 is absent from Herdr's snapshot", At: 1753000000000,
	}
	if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.Replaced = append(p.Replaced, displaced)
		p.LaunchPaneID, p.PublicPaneID, p.OMPSessionID = "w2:p1", "w2:p1", "omp-primary-1"
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(replace): %v", err)
	}
	got, err := s.Project(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !reflect.DeepEqual(got.Replaced, []task.ReplacedPrimary{displaced}) {
		t.Fatalf("Replaced = %+v, want %+v", got.Replaced, displaced)
	}

	// Another writer that knows nothing of the record (a pane move, an attention
	// write) must not lose it.
	if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.PublicPaneID = "w3:p1"
		p.PrimaryAttention = "unrelated"
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(unrelated): %v", err)
	}
	moved, err := s.Project(ctx, "repo-a")
	if err != nil || !reflect.DeepEqual(moved.Replaced, []task.ReplacedPrimary{displaced}) || moved.PublicPaneID != "w3:p1" {
		t.Fatalf("an unrelated update changed the record: (%+v, %v)", moved, err)
	}
	all, err := s.Projects(ctx)
	if err != nil || len(all) != 1 || !reflect.DeepEqual(all[0].Replaced, []task.ReplacedPrimary{displaced}) {
		t.Fatalf("Projects() = (%+v, %v), want the record kept", all, err)
	}
}

func TestStoreLegacyProjectRowReadsAsNothingReplaced(t *testing.T) {
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
	if got.Replaced != nil || got.ReattachNotice != (task.NoticeAttempt{}) {
		t.Errorf("legacy project Replaced = %+v, ReattachNotice = %+v, want none", got.Replaced, got.ReattachNotice)
	}
}

// The notice policy of a reattach - a notice that was sent, or may have been, is
// never sent again - rests on this fact outliving the process that recorded it.
// It is therefore read back from a store that was closed and opened again, with
// the session-reference kind it was recorded with.
func TestStoreProjectReattachNoticeSurvivesAReopenedStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := state.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	project := task.Project{Key: "repo-a", RepoWorkspaceID: "w2", LaunchPaneID: "w2:p1", PublicPaneID: "w2:p1", OMPSessionID: `C:\omp\sessions\one.jsonl`}
	if err := first.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	attempt := task.NoticeAttempt{
		PaneID: "w2:p1", Session: `C:\omp\sessions\one.jsonl`, Kind: task.SessionKindPath,
		State: task.NoticeUncertain, At: 1753000000123,
	}
	if err := first.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.ReattachNotice = attempt
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(record the attempt): %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := state.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()
	got, err := second.Project(ctx, "repo-a")
	if err != nil || got.ReattachNotice != attempt {
		t.Fatalf("Project after a reopen = (%+v, %v), want the attempt %+v with its kind", got.ReattachNotice, err, attempt)
	}
	if byPane, err := second.ProjectByLaunchPane(ctx, "w2:p1"); err != nil || byPane.ReattachNotice != attempt {
		t.Fatalf("ProjectByLaunchPane = (%+v, %v), want the attempt kept", byPane.ReattachNotice, err)
	}
	if all, err := second.Projects(ctx); err != nil || len(all) != 1 || all[0].ReattachNotice != attempt {
		t.Fatalf("Projects() = (%+v, %v), want the attempt kept", all, err)
	}

	// A writer that knows nothing of the attempt (an attention write, a pane move)
	// must not lose it.
	if err := second.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.PrimaryAttention = "unrelated"
		p.PublicPaneID = "w3:p1"
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(unrelated): %v", err)
	}
	if kept, err := second.Project(ctx, "repo-a"); err != nil || kept.ReattachNotice != attempt {
		t.Fatalf("an unrelated update changed the attempt: (%+v, %v)", kept.ReattachNotice, err)
	}

	// Withdrawing it is the zero value, and reads back as none.
	if err := second.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.ReattachNotice = task.NoticeAttempt{}
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject(withdraw): %v", err)
	}
	if cleared, err := second.Project(ctx, "repo-a"); err != nil || cleared.ReattachNotice != (task.NoticeAttempt{}) {
		t.Fatalf("a withdrawn attempt = (%+v, %v), want none", cleared.ReattachNotice, err)
	}
}
