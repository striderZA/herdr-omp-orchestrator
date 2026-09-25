package state_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

const (
	launchPane = "w1:p2"
	sessionOne = "omp-session-1"
	sessionTwo = "omp-session-2"
)

func open(t *testing.T, dir string) *state.Store {
	t.Helper()
	s, err := state.Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func seedTask(id string) task.Task {
	return task.Task{
		ID:                  id,
		ProjectKey:          "repo-a",
		Title:               "wire the dispatcher",
		Brief:               "do the thing",
		BaseRevision:        "abc123",
		Branch:              "task/" + id,
		Checks:              []string{"go test ./..."},
		Dependencies:        []string{"task-0"},
		ReservedPaths:       []string{"internal/"},
		WorktreePath:        "E:/wt/" + id,
		WorktreeWorkspaceID: "w1",
		CurrentWorkspaceID:  "w1",
		TerminalID:          "term-" + id,
		LaunchPaneID:        launchPane,
		PublicPaneID:        launchPane,
		AgentName:           "omp-" + id,
		Phase:               task.Intent,
	}
}

func TestStoreUsesStateDirDatabase(t *testing.T) {
	dir := t.TempDir()
	open(t, dir)

	path := filepath.Join(dir, "orchestrator.sqlite")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state database not created under state dir: %v", err)
	}

	// The database the store opens must stay in WAL mode: a bare handle on the
	// same file reports the journal mode actually persisted by the store's DSN.
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Errorf("close raw handle: %v", err)
		}
	})

	var journalMode string
	if err := raw.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("journal_mode = %q, want %q: the store DSN must keep _pragma=journal_mode(WAL)", journalMode, "wal")
	}
}

func TestStoreCreateTaskRoundTripsFacts(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	want := seedTask("t1")

	if err := s.CreateTask(ctx, want); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if got.Revision != 1 {
		t.Errorf("new task revision = %d, want 1", got.Revision)
	}
	if got.CreatedAt <= 0 || got.UpdatedAt <= 0 {
		t.Errorf("timestamps not recorded: created=%d updated=%d", got.CreatedAt, got.UpdatedAt)
	}
	want.Revision, want.CreatedAt, want.UpdatedAt = 1, got.CreatedAt, got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}

	byPane, err := s.ByLaunchPane(ctx, launchPane)
	if err != nil {
		t.Fatalf("ByLaunchPane: %v", err)
	}
	if byPane.ID != "t1" {
		t.Errorf("ByLaunchPane = %q, want t1", byPane.ID)
	}
	if _, err := s.ByLaunchPane(ctx, "w9:p9"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown launch pane err = %v, want ErrNotFound", err)
	}
}

func TestStoreRejectsInvalidIdentities(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	noID := seedTask("")
	if err := s.CreateTask(ctx, noID); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("empty task ID err = %v, want ErrInvalid", err)
	}
	noProject := seedTask("t1")
	noProject.ProjectKey = ""
	if err := s.CreateTask(ctx, noProject); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("empty project key err = %v, want ErrInvalid", err)
	}

	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask t1: %v", err)
	}
	if err := s.CreateTask(ctx, seedTask("t1")); !errors.Is(err, state.ErrConflict) {
		t.Errorf("duplicate task ID err = %v, want ErrConflict", err)
	}
	if err := s.CreateTask(ctx, seedTask("t2")); !errors.Is(err, state.ErrConflict) {
		t.Errorf("duplicate launch pane err = %v, want ErrConflict", err)
	}

	// Unbound panes are NULL, so several queued tasks without a pane coexist.
	for _, id := range []string{"q1", "q2"} {
		unbound := seedTask(id)
		unbound.LaunchPaneID, unbound.PublicPaneID = "", ""
		if err := s.CreateTask(ctx, unbound); err != nil {
			t.Fatalf("CreateTask %s without pane: %v", id, err)
		}
		if _, err := s.ByLaunchPane(ctx, ""); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("empty launch pane lookup err = %v, want ErrNotFound", err)
		}
	}
}

func TestStoreUpdateTaskRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	seed := seedTask("t1")
	seed.Title = "original"
	if err := s.CreateTask(ctx, seed); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	_, err := s.UpdateTask(ctx, "t1", 0, func(x *task.Task) error {
		x.Title = "stale write"
		return nil
	})
	if !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale revision err = %v, want ErrConflict", err)
	}
	after, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Title != "original" || after.Revision != 1 {
		t.Fatalf("failed update changed data: title=%q revision=%d", after.Title, after.Revision)
	}

	updated, err := s.UpdateTask(ctx, "t1", 1, func(x *task.Task) error {
		x.Title = "renamed"
		x.Phase = task.PromptSubmitted
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Revision != 2 || updated.Title != "renamed" || updated.Phase != task.PromptSubmitted {
		t.Errorf("update result = %+v, want revision 2 renamed prompt_submitted", updated)
	}
	if updated.UpdatedAt < updated.CreatedAt {
		t.Errorf("update timestamp moved backwards: %d < %d", updated.UpdatedAt, updated.CreatedAt)
	}

	if _, err := s.UpdateTask(ctx, "t1", 1, func(*task.Task) error { return nil }); !errors.Is(err, state.ErrConflict) {
		t.Errorf("replayed revision err = %v, want ErrConflict", err)
	}
	if _, err := s.UpdateTask(ctx, "gone", 1, func(*task.Task) error { return nil }); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown task err = %v, want ErrNotFound", err)
	}
}

func TestStoreUpdateTaskRejectsIdentityChange(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if _, err := s.UpdateTask(ctx, "t1", 1, func(x *task.Task) error {
		x.ID = "t2"
		return nil
	}); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("task ID change err = %v, want ErrInvalid", err)
	}
	if _, err := s.UpdateTask(ctx, "t1", 1, func(x *task.Task) error {
		x.ProjectKey = "repo-b"
		return nil
	}); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("project key change err = %v, want ErrInvalid", err)
	}

	after, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.ID != "t1" || after.ProjectKey != "repo-a" || after.Revision != 1 {
		t.Errorf("rejected update wrote data: %+v", after)
	}
	if _, err := s.Task(ctx, "t2"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("renamed task exists: %v", err)
	}
}

func TestMovePublicPaneKeepsLaunchIdentity(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	other := seedTask("t2")
	other.LaunchPaneID, other.PublicPaneID = "w3:p1", "w3:p1"
	other.WorktreeWorkspaceID, other.CurrentWorkspaceID = "w3", "w3"
	if err := s.CreateTask(ctx, other); err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}

	if err := s.MovePublicPane(ctx, launchPane, launchPane, "w2:p4"); err != nil {
		t.Fatalf("MovePublicPane: %v", err)
	}

	moved, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if moved.LaunchPaneID != launchPane {
		t.Errorf("launch pane = %q, want %q", moved.LaunchPaneID, launchPane)
	}
	if moved.WorktreeWorkspaceID != "w1" {
		t.Errorf("worktree workspace = %q, want w1", moved.WorktreeWorkspaceID)
	}
	if moved.PublicPaneID != "w2:p4" {
		t.Errorf("public pane = %q, want w2:p4", moved.PublicPaneID)
	}
	if moved.CurrentWorkspaceID != "w2" {
		t.Errorf("current workspace = %q, want w2", moved.CurrentWorkspaceID)
	}
	if byLaunch, err := s.ByLaunchPane(ctx, launchPane); err != nil || byLaunch.ID != "t1" {
		t.Errorf("task no longer reachable by launch pane: %+v, %v", byLaunch, err)
	}
	untouched, err := s.Task(ctx, "t2")
	if err != nil {
		t.Fatalf("Task t2: %v", err)
	}
	if untouched.PublicPaneID != "w3:p1" || untouched.CurrentWorkspaceID != "w3" {
		t.Errorf("unrelated task moved: %+v", untouched)
	}

	if err := s.MovePublicPane(ctx, launchPane, launchPane, "w4:p9"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("stale public pane err = %v, want ErrConflict", err)
	}
	if err := s.MovePublicPane(ctx, "w9:p9", "w9:p9", "w2:p4"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown launch pane err = %v, want ErrNotFound", err)
	}
}

func TestStoreInboxDeduplicates(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	note := task.Inbox{ID: "i1", ProjectKey: "repo-a", TaskID: "t1", Kind: "blocker", Summary: "need an answer", Delivery: "pending"}

	created, err := s.AddInbox(ctx, note)
	if err != nil {
		t.Fatalf("AddInbox: %v", err)
	}
	if !created {
		t.Error("first AddInbox reported duplicate")
	}
	created, err = s.AddInbox(ctx, note)
	if err != nil {
		t.Fatalf("duplicate AddInbox: %v", err)
	}
	if created {
		t.Error("second AddInbox reported a new row")
	}
	other := note
	other.ID = "i2"
	if created, err = s.AddInbox(ctx, other); err != nil || !created {
		t.Errorf("distinct inbox add = (%v, %v), want (true, nil)", created, err)
	}
	if created, err = s.AddInbox(ctx, task.Inbox{Summary: "no id"}); !errors.Is(err, state.ErrInvalid) || created {
		t.Errorf("empty inbox ID = (%v, %v), want (false, ErrInvalid)", created, err)
	}
}

func TestStoreReadyHandshake(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if err := s.RecordReady(ctx, launchPane, sessionOne, 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	for _, session := range []string{sessionOne, sessionTwo} {
		ready, err := s.Ready(ctx, launchPane, session)
		if err != nil {
			t.Fatalf("Ready(%s): %v", session, err)
		}
		if ready {
			t.Errorf("RecordReady alone made %s ready", session)
		}
	}
	if err := s.ConfirmReady(ctx, launchPane, sessionTwo); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("ConfirmReady with another session err = %v, want ErrNotFound", err)
	}
	ready, err := s.Ready(ctx, launchPane, sessionOne)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Error("failed ConfirmReady verified the recorded session")
	}

	if err := s.ConfirmReady(ctx, launchPane, sessionOne); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}
	if ready, err = s.Ready(ctx, launchPane, sessionOne); err != nil || !ready {
		t.Errorf("Ready(confirmed) = (%v, %v), want (true, nil)", ready, err)
	}
	if ready, err = s.Ready(ctx, launchPane, sessionTwo); err != nil || ready {
		t.Errorf("Ready(other session) = (%v, %v), want (false, nil)", ready, err)
	}

	// A restarted extension announces a new session ID; the old handshake must not pass.
	if err := s.RecordReady(ctx, launchPane, sessionTwo, 1); err != nil {
		t.Fatalf("RecordReady(session two): %v", err)
	}
	if ready, err = s.Ready(ctx, launchPane, sessionOne); err != nil || ready {
		t.Errorf("stale verified session = (%v, %v), want (false, nil)", ready, err)
	}
	if err := s.ConfirmReady(ctx, launchPane, sessionTwo); err != nil {
		t.Fatalf("ConfirmReady(session two): %v", err)
	}
	if ready, err = s.Ready(ctx, launchPane, sessionTwo); err != nil || !ready {
		t.Errorf("Ready(session two) = (%v, %v), want (true, nil)", ready, err)
	}
}

func TestStoreByOMPSessionBindsOneRole(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	project := task.Project{
		Key:                "repo-a",
		RepoWorkspaceID:    "w1",
		CurrentWorkspaceID: "w1",
		TerminalID:         "term-primary",
		LaunchPaneID:       "w1:p1",
		PublicPaneID:       "w1:p1",
		AgentName:          "omp-primary",
		OMPSessionID:       "omp-primary-1",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	worker := seedTask("t1")
	worker.OMPSessionID = "omp-worker-1"
	if err := s.CreateTask(ctx, worker); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	primary, err := s.ByOMPSession(ctx, "omp-primary-1")
	if err != nil {
		t.Fatalf("ByOMPSession(primary): %v", err)
	}
	if primary.Role != state.RolePrimary || primary.Project.Key != "repo-a" {
		t.Errorf("primary binding = %+v", primary)
	}
	boundWorker, err := s.ByOMPSession(ctx, "omp-worker-1")
	if err != nil {
		t.Fatalf("ByOMPSession(worker): %v", err)
	}
	if boundWorker.Role != state.RoleWorker || boundWorker.Task.ID != "t1" {
		t.Errorf("worker binding = %+v", boundWorker)
	}
	if _, err := s.ByOMPSession(ctx, "omp-unknown"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown session err = %v, want ErrNotFound", err)
	}

	clash := seedTask("t2")
	clash.LaunchPaneID, clash.PublicPaneID = "w3:p1", "w3:p1"
	clash.OMPSessionID = "omp-worker-1"
	if err := s.CreateTask(ctx, clash); err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}
	if _, err := s.ByOMPSession(ctx, "omp-worker-1"); !errors.Is(err, state.ErrAmbiguous) {
		t.Errorf("ambiguous session err = %v, want ErrAmbiguous", err)
	}
}

func TestStoreProjectRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if _, err := s.Project(ctx, "repo-a"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown project err = %v, want ErrNotFound", err)
	}
	if err := s.CreateProject(ctx, task.Project{}); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("empty project key err = %v, want ErrInvalid", err)
	}
	project := task.Project{Key: "repo-a", RepoWorkspaceID: "w1", OMPSessionID: "omp-primary-1"}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := s.CreateProject(ctx, project); !errors.Is(err, state.ErrConflict) {
		t.Errorf("duplicate project err = %v, want ErrConflict", err)
	}
	if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.CurrentWorkspaceID = "w2"
		p.PublicPaneID = "w2:p1"
		return nil
	}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err := s.Project(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got.CurrentWorkspaceID != "w2" || got.PublicPaneID != "w2:p1" || got.OMPSessionID != "omp-primary-1" {
		t.Errorf("project after update = %+v", got)
	}
	if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
		p.Key = "repo-b"
		return nil
	}); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("project key change err = %v, want ErrInvalid", err)
	}
	after, err := s.Project(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if after.Key != "repo-a" {
		t.Errorf("project key changed to %q", after.Key)
	}
}

func TestStoreConcurrentUpdateHasOneWinner(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := open(t, dir)
	second := open(t, dir)
	if err := first.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// Both connections observe revision 1 before either writes.
	for i, s := range []*state.Store{first, second} {
		got, err := s.Task(ctx, "t1")
		if err != nil {
			t.Fatalf("Task (conn %d): %v", i, err)
		}
		if got.Revision != 1 {
			t.Fatalf("conn %d starts at revision %d, want 1", i, got.Revision)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, s := range []*state.Store{first, second} {
		wg.Add(1)
		go func(i int, s *state.Store) {
			defer wg.Done()
			<-start
			_, err := s.UpdateTask(ctx, "t1", 1, func(x *task.Task) error {
				x.Activity = fmt.Sprintf("writer-%d", i)
				return nil
			})
			errs <- err
		}(i, s)
	}
	close(start)
	wg.Wait()
	close(errs)

	var winners, conflicts, other int
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, state.ErrConflict):
			conflicts++
		default:
			other++
			t.Errorf("unexpected update error: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 || other != 0 {
		t.Fatalf("concurrent update = %d winners, %d conflicts, %d other; want 1, 1, 0", winners, conflicts, other)
	}

	final, err := first.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if final.Revision != 2 {
		t.Errorf("final revision = %d, want 2", final.Revision)
	}
}
