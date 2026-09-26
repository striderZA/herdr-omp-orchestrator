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
	"time"

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

// TestOpenMigratesAnInboxWrittenByAnEarlierRevision pins the upgrade path a
// shipped installation takes. The inbox of an earlier revision has no delivery
// state, so the index on that column cannot be created with the tables: doing so
// makes every later open fail with "no such column", which is exactly what the
// first real hook run of Task 6 hit.
func TestOpenMigratesAnInboxWrittenByAnEarlierRevision(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	raw := rawState(t, dir)
	if _, err := raw.Exec(`
		CREATE TABLE inbox (
			id TEXT PRIMARY KEY,
			project_key TEXT NOT NULL,
			task_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			summary TEXT NOT NULL,
			delivery TEXT NOT NULL
		)`); err != nil {
		t.Fatalf("seed the earlier revision: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO inbox (id, project_key, task_id, kind, summary, delivery)
		 VALUES ('i1', 'repo-a', 't1', 'result', 'done', 'worker result')`); err != nil {
		t.Fatalf("seed the earlier note: %v", err)
	}

	s := open(t, dir)
	note, err := s.Inbox(ctx, "i1")
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if note.State() != task.DeliveryPending {
		t.Errorf("migrated note state = %q, want %q", note.State(), task.DeliveryPending)
	}
	if note.Delivery != "worker result" {
		t.Errorf("migrated note delivery label = %q, want the recorded label", note.Delivery)
	}
	claimed, err := s.ClaimInbox(ctx, []string{"i1"})
	if err != nil {
		t.Fatalf("ClaimInbox on a migrated note: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("ClaimInbox took %d notes, want 1", len(claimed))
	}
	if _, err := s.SettleInbox(ctx, []string{"i1"}, task.DeliveryDelivered, "prompted"); err != nil {
		t.Fatalf("SettleInbox on a migrated note: %v", err)
	}
}

func TestOpenMigratesSlotTokenForAnExistingProvisioningClaim(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	raw := rawState(t, dir)
	if _, err := raw.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY,
		project_key TEXT NOT NULL,
		launch_pane_id TEXT NULL,
		public_pane_id TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL,
		slot_claimed_at INTEGER NOT NULL DEFAULT 0,
		data JSON NOT NULL
	)`); err != nil {
		t.Fatalf("create previous task schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO tasks
		(id, project_key, launch_pane_id, public_pane_id, revision, slot_claimed_at, data)
		VALUES ('t1', 'repo-a', NULL, '', 1, 123, '{"ID":"t1","ProjectKey":"repo-a","Phase":"intent"}')`); err != nil {
		t.Fatalf("insert previous task row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy database handle: %v", err)
	}

	s := open(t, dir)
	took, token, err := s.ClaimProvisioningSlot(ctx, "t1", 1)
	if err != nil || took || token != 0 {
		t.Fatalf("ClaimProvisioningSlot on the legacy held claim = (%v, %d, %v), want (false, 0, nil)", took, token, err)
	}
	stepTaken, err := s.ClaimStep(ctx, "t1", task.Intent, "legacy-resume", time.Minute)
	if err != nil || !stepTaken {
		t.Fatalf("ClaimStep on the migrated Intent task = (%v, %v), want (true, nil)", stepTaken, err)
	}
	token, err = s.RenewProvisioningSlot(ctx, "t1", "legacy-resume")
	if err != nil || token == 0 {
		t.Fatalf("RenewProvisioningSlot on the legacy claim = (%d, %v), want a fresh token", token, err)
	}
	if err := s.ReleaseStep(ctx, "t1", "legacy-resume"); err != nil {
		t.Fatalf("ReleaseStep on the migrated task: %v", err)
	}
	if err := s.ReleaseProvisioningSlot(ctx, "t1", token); err != nil {
		t.Fatalf("release the migrated legacy claim: %v", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 0 {
		t.Fatalf("ProvisioningSlots after migrated release = (%d, %v), want 0", slots, err)
	}
}

// rawState opens the state database the way a migration or an operator tool
// would: a second handle on the same file, which is what lets a test make one
// table refuse a write while everything else stays usable.
func rawState(t *testing.T, dir string) *sql.DB {
	t.Helper()
	path := filepath.Join(dir, "orchestrator.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close raw handle: %v", err)
		}
	})
	return db
}

// countNotes reports how many durable notes of one kind exist.
func countNotes(t *testing.T, db *sql.DB, kind string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM inbox WHERE kind = ?`, kind).Scan(&count); err != nil {
		t.Fatalf("count %s notes: %v", kind, err)
	}
	return count
}

// TestUpdateTaskWithNoteIsOneTransaction pins the store primitive a worker's
// final report stands on: the task's compare-and-swap update and the note that
// announces it are one transaction, so a task can never be recorded as ready for
// review without the primary being told about it.
func TestUpdateTaskWithNoteIsOneTransaction(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.CreateTask(ctx, seedTask("task-1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	before, err := s.Task(ctx, "task-1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	note := task.Inbox{
		ID: "result:task-1", ProjectKey: before.ProjectKey, TaskID: before.ID,
		Kind: "result", Summary: "done", Delivery: "worker result",
	}
	record := func(next *task.Task) error {
		next.Report = &task.Report{Kind: "result", Summary: "done", Branch: before.Branch}
		return nil
	}

	raw := rawState(t, dir)
	trigger := func(query string) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, query); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}
	trigger(`CREATE TRIGGER refuse_inbox BEFORE INSERT ON inbox BEGIN SELECT RAISE(ABORT, 'inbox is not writable'); END`)

	// The note cannot be written, so the update must not be either.
	if _, _, err := s.UpdateTaskWithNote(ctx, "task-1", before.Revision, record, note); err == nil {
		t.Fatal("UpdateTaskWithNote succeeded although its note could not be written")
	}
	rolledBack, err := s.Task(ctx, "task-1")
	if err != nil {
		t.Fatalf("Task after the refused update: %v", err)
	}
	if rolledBack.Report != nil || rolledBack.Revision != before.Revision {
		t.Errorf("a refused update left a partial write: %+v", rolledBack)
	}
	if got := countNotes(t, raw, "result"); got != 0 {
		t.Errorf("a refused update left %d result notes", got)
	}

	trigger(`DROP TRIGGER refuse_inbox`)

	// Together they land, and both are reported as new.
	updated, inserted, err := s.UpdateTaskWithNote(ctx, "task-1", before.Revision, record, note)
	if err != nil {
		t.Fatalf("UpdateTaskWithNote: %v", err)
	}
	if !inserted {
		t.Error("the first note was not reported as new")
	}
	if updated.Report == nil || updated.Revision != before.Revision+1 {
		t.Errorf("updated task = %+v, want the report on revision %d", updated, before.Revision+1)
	}
	if got := countNotes(t, raw, "result"); got != 1 {
		t.Errorf("result notes = %d, want 1", got)
	}

	// Repeating the note is deduplicated: the task side still applies, and the
	// duplicate is reported so a caller can answer "nothing new".
	again, reinserted, err := s.UpdateTaskWithNote(ctx, "task-1", updated.Revision, func(*task.Task) error { return nil }, note)
	if err != nil {
		t.Fatalf("UpdateTaskWithNote (repeat): %v", err)
	}
	if reinserted {
		t.Error("a repeated note was reported as new")
	}
	if again.Revision != updated.Revision+1 {
		t.Errorf("revision = %d, want %d", again.Revision, updated.Revision+1)
	}
	if got := countNotes(t, raw, "result"); got != 1 {
		t.Errorf("result notes after the repeat = %d, want 1", got)
	}

	// A stale revision is still a conflict, and neither side is written.
	if _, _, err := s.UpdateTaskWithNote(ctx, "task-1", before.Revision, record, task.Inbox{
		ID: "result:task-1:stale", ProjectKey: before.ProjectKey, TaskID: before.ID,
		Kind: "result", Summary: "stale", Delivery: "worker result",
	}); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale revision err = %v, want ErrConflict", err)
	}
	if got := countNotes(t, raw, "result"); got != 1 {
		t.Errorf("a conflicted update wrote a note: %d result notes", got)
	}

	// An empty note identity is refused before anything is written.
	if _, _, err := s.UpdateTaskWithNote(ctx, "task-1", again.Revision, record, task.Inbox{}); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("empty note id err = %v, want ErrInvalid", err)
	}
	if _, err := s.Task(ctx, "task-1"); err != nil {
		t.Fatalf("Task after the refused calls: %v", err)
	}
}

func TestStoreReadyHandshake(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionOne}, 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	for _, session := range []string{sessionOne, sessionTwo} {
		ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: session})
		if err != nil {
			t.Fatalf("Ready(%s): %v", session, err)
		}
		if ready {
			t.Errorf("RecordReady alone made %s ready", session)
		}
	}
	if err := s.ConfirmReady(ctx, launchPane, task.SessionRef{Kind: task.SessionKindID, Value: sessionTwo}); !errors.Is(err, state.ErrMismatch) {
		t.Errorf("ConfirmReady with another session err = %v, want ErrMismatch", err)
	}
	ready, err := s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Error("failed ConfirmReady verified the recorded session")
	}

	if err := s.ConfirmReady(ctx, launchPane, task.SessionRef{Kind: task.SessionKindID, Value: sessionOne}); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}
	if ready, err = s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne}); err != nil || !ready {
		t.Errorf("Ready(confirmed) = (%v, %v), want (true, nil)", ready, err)
	}
	if ready, err = s.Ready(ctx, launchPane, task.Announcement{ID: sessionTwo}); err != nil || ready {
		t.Errorf("Ready(other session) = (%v, %v), want (false, nil)", ready, err)
	}

	// A restarted extension announces a new session ID; the old handshake must not pass.
	if err := s.RecordReady(ctx, launchPane, task.Announcement{ID: sessionTwo}, 1); err != nil {
		t.Fatalf("RecordReady(session two): %v", err)
	}
	if ready, err = s.Ready(ctx, launchPane, task.Announcement{ID: sessionOne}); err != nil || ready {
		t.Errorf("stale verified session = (%v, %v), want (false, nil)", ready, err)
	}
	if err := s.ConfirmReady(ctx, launchPane, task.SessionRef{Kind: task.SessionKindID, Value: sessionTwo}); err != nil {
		t.Fatalf("ConfirmReady(session two): %v", err)
	}
	if ready, err = s.Ready(ctx, launchPane, task.Announcement{ID: sessionTwo}); err != nil || !ready {
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

	primary, err := s.ByOMPSession(ctx, task.Announcement{ID: "omp-primary-1"})
	if err != nil {
		t.Fatalf("ByOMPSession(primary): %v", err)
	}
	if primary.Role != state.RolePrimary || primary.Project.Key != "repo-a" {
		t.Errorf("primary binding = %+v", primary)
	}
	boundWorker, err := s.ByOMPSession(ctx, task.Announcement{ID: "omp-worker-1"})
	if err != nil {
		t.Fatalf("ByOMPSession(worker): %v", err)
	}
	if boundWorker.Role != state.RoleWorker || boundWorker.Task.ID != "t1" {
		t.Errorf("worker binding = %+v", boundWorker)
	}
	if _, err := s.ByOMPSession(ctx, task.Announcement{ID: "omp-unknown"}); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown session err = %v, want ErrNotFound", err)
	}

	clash := seedTask("t2")
	clash.LaunchPaneID, clash.PublicPaneID = "w3:p1", "w3:p1"
	clash.OMPSessionID = "omp-worker-1"
	if err := s.CreateTask(ctx, clash); err != nil {
		t.Fatalf("CreateTask t2: %v", err)
	}
	if _, err := s.ByOMPSession(ctx, task.Announcement{ID: "omp-worker-1"}); !errors.Is(err, state.ErrAmbiguous) {
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

func TestStoreTasksListsOneProjectInStableOrder(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	// Creation order dominates the id: t-z is created last but its id sorts
	// first, and the distinct creation times make the expectation exact.
	// The panes stay unbound: the launch-pane index admits one task per pane,
	// which this listing test does not exercise.
	seed := func(id string) task.Task {
		st := seedTask(id)
		st.LaunchPaneID = ""
		return st
	}
	for _, id := range []string{"t-a", "t-b"} {
		if err := s.CreateTask(ctx, seed(id)); err != nil {
			t.Fatalf("CreateTask(%s): %v", id, err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	other := seed("t-other")
	other.ProjectKey = "repo-b"
	if err := s.CreateTask(ctx, other); err != nil {
		t.Fatalf("CreateTask(t-other): %v", err)
	}
	if err := s.CreateTask(ctx, seed("t-z")); err != nil {
		t.Fatalf("CreateTask(t-z): %v", err)
	}

	tasks, err := s.Tasks(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	var ids []string
	for _, st := range tasks {
		ids = append(ids, st.ID)
		if st.ProjectKey != "repo-a" {
			t.Errorf("Tasks returned a task of %q", st.ProjectKey)
		}
	}
	if want := []string{"t-a", "t-b", "t-z"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("Tasks order = %v, want %v", ids, want)
	}
	// Whatever the clock gave the tasks, the listing is sorted by creation time
	// and then by id, so repeated reads cannot reorder the same facts.
	for i := 1; i < len(tasks); i++ {
		previous, current := tasks[i-1], tasks[i]
		if previous.CreatedAt > current.CreatedAt {
			t.Errorf("task %s (created %d) sorts before %s (created %d)",
				previous.ID, previous.CreatedAt, current.ID, current.CreatedAt)
		}
		if previous.CreatedAt == current.CreatedAt && previous.ID > current.ID {
			t.Errorf("same-millisecond tasks are not id-ordered: %s before %s", previous.ID, current.ID)
		}
	}

	projectB, err := s.Tasks(ctx, "repo-b")
	if err != nil {
		t.Fatalf("Tasks(repo-b): %v", err)
	}
	if len(projectB) != 1 || projectB[0].ID != "t-other" {
		t.Errorf("Tasks(repo-b) = %+v, want only t-other", projectB)
	}
	if _, err := s.Tasks(ctx, "  "); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("Tasks(\"\") err = %v, want ErrInvalid", err)
	}
}

func TestStoreProjectByLaunchPane(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	project := task.Project{
		Key:          "E:/repo-a",
		LaunchPaneID: "w3:p1",
		PublicPaneID: "w3:p1",
		AgentName:    "omp",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	found, err := s.ProjectByLaunchPane(ctx, "w3:p1")
	if err != nil {
		t.Fatalf("ProjectByLaunchPane: %v", err)
	}
	if found.Key != project.Key || found.LaunchPaneID != project.LaunchPaneID {
		t.Errorf("found = %+v, want %+v", found, project)
	}
	if _, err := s.ProjectByLaunchPane(ctx, "w9:p9"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown pane err = %v, want ErrNotFound", err)
	}
	if _, err := s.ProjectByLaunchPane(ctx, ""); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("empty pane err = %v, want ErrInvalid", err)
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
