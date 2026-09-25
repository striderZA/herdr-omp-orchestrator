// Package state is the only writer of orchestrator facts.
//
// It keeps mutable facts in one SQLite database under the Herdr plugin state
// dir. Source checkouts and worktree checkouts never hold plugin state.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, so a CGO_ENABLED=0 build needs no C toolchain

	"herdr-omp-orchestrator/internal/task"
)

// Errors returned by Store. Callers compare them with errors.Is.
var (
	ErrConflict  = errors.New("state: conflicting revision")
	ErrNotFound  = errors.New("state: not found")
	ErrInvalid   = errors.New("state: invalid record")
	ErrAmbiguous = errors.New("state: ambiguous session binding")
)

// Role identifies whether an OMP session belongs to a primary project or a worker task.
type Role string

const (
	RolePrimary Role = "primary"
	RoleWorker  Role = "worker"
)

// Binding names exactly one project or task as the owner of an OMP session.
type Binding struct {
	Role    Role
	Project task.Project
	Task    task.Task
}

const dbFile = "orchestrator.sqlite"

// schema keeps every indexed identity in its own column and the durable facts as JSON.
//
// An unbound launch pane is SQL NULL, and the unique index is partial, so many
// queued tasks without a pane coexist while a bound pane belongs to one task.
const schema = `
CREATE TABLE IF NOT EXISTS projects (
	key TEXT PRIMARY KEY,
	data JSON NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	project_key TEXT NOT NULL,
	launch_pane_id TEXT NULL,
	public_pane_id TEXT NOT NULL DEFAULT '',
	revision INTEGER NOT NULL,
	data JSON NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS tasks_launch_pane_id
	ON tasks (launch_pane_id) WHERE launch_pane_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS tasks_project_key ON tasks (project_key);
CREATE TABLE IF NOT EXISTS inbox (
	id TEXT PRIMARY KEY,
	project_key TEXT NOT NULL,
	task_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	summary TEXT NOT NULL,
	delivery TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS ready_sessions (
	launch_pane_id TEXT PRIMARY KEY,
	omp_session_id TEXT NOT NULL,
	protocol INTEGER NOT NULL,
	verified INTEGER NOT NULL DEFAULT 0,
	seen_at INTEGER NOT NULL
);
`

// Store owns the orchestrator database.
type Store struct {
	db *sql.DB
}

// Open opens the orchestrator database in dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: empty state dir", ErrInvalid)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, dbFile)
	// _txlock=immediate takes the write lock when a transaction begins, so two
	// connections cannot both read a revision and then race to upgrade.
	dsn := "file:" + filepath.ToSlash(path) +
		"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// CreateTask records a new task and its revision 1 timestamps.
func (s *Store) CreateTask(ctx context.Context, t task.Task) error {
	if t.ID == "" || t.ProjectKey == "" {
		return fmt.Errorf("%w: task id and project key are required", ErrInvalid)
	}
	now := time.Now().UnixMilli()
	t.Revision = 1
	t.CreatedAt, t.UpdatedAt = now, now
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode task %q: %w", t.ID, err)
	}

	tx, err := s.begin(ctx, "create task "+t.ID)
	if err != nil {
		return err
	}
	defer tx.rollback()

	if taken, err := rowExists(ctx, tx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("%w: task %q already exists", ErrConflict, t.ID)
	}
	if err := checkPaneFree(ctx, tx, t.LaunchPaneID, t.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (id, project_key, launch_pane_id, public_pane_id, revision, data)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectKey, nullablePane(t.LaunchPaneID), t.PublicPaneID, t.Revision, string(data),
	); err != nil {
		return fmt.Errorf("create task %q: %w", t.ID, err)
	}
	return tx.commit("create task " + t.ID)
}

// Task reads one task by ID.
func (s *Store) Task(ctx context.Context, id string) (task.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, id), "task "+id)
}

// ByLaunchPane reads the task bound to its launch-time pane ID.
func (s *Store) ByLaunchPane(ctx context.Context, launchPaneID string) (task.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT data FROM tasks WHERE launch_pane_id = ?`, launchPaneID),
		"task on launch pane "+launchPaneID)
}

// UpdateTask applies mutate to the current facts and stores them as one revision
// later, but only while the stored revision still matches expectedRevision.
func (s *Store) UpdateTask(ctx context.Context, id string, expectedRevision int64, mutate func(*task.Task) error) (task.Task, error) {
	if mutate == nil {
		return task.Task{}, fmt.Errorf("%w: nil mutation for task %q", ErrInvalid, id)
	}
	tx, err := s.begin(ctx, "update task "+id)
	if err != nil {
		return task.Task{}, err
	}
	defer tx.rollback()

	current, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, id), "task "+id)
	if err != nil {
		return task.Task{}, err
	}
	if current.Revision != expectedRevision {
		return task.Task{}, fmt.Errorf("%w: task %q is at revision %d, expected %d",
			ErrConflict, id, current.Revision, expectedRevision)
	}

	next := current
	if err := mutate(&next); err != nil {
		return task.Task{}, fmt.Errorf("mutate task %q: %w", id, err)
	}
	if next.ID != current.ID || next.ProjectKey != current.ProjectKey {
		return task.Task{}, fmt.Errorf("%w: task %q id and project key are immutable", ErrInvalid, id)
	}
	if next.LaunchPaneID != current.LaunchPaneID {
		if err := checkPaneFree(ctx, tx, next.LaunchPaneID, id); err != nil {
			return task.Task{}, err
		}
	}
	next.Revision = current.Revision + 1
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = time.Now().UnixMilli()
	data, err := json.Marshal(next)
	if err != nil {
		return task.Task{}, fmt.Errorf("encode task %q: %w", id, err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET data = ?, revision = revision + 1, launch_pane_id = ?, public_pane_id = ?
		 WHERE id = ? AND revision = ?`,
		string(data), nullablePane(next.LaunchPaneID), next.PublicPaneID, id, expectedRevision)
	if err != nil {
		return task.Task{}, fmt.Errorf("update task %q: %w", id, err)
	}
	if err := requireOneRow(res, ErrConflict, "update task "+id); err != nil {
		return task.Task{}, err
	}
	if err := tx.commit("update task " + id); err != nil {
		return task.Task{}, err
	}
	return next, nil
}

// MovePublicPane records that Herdr moved a task's public pane, keeping the
// launch pane and worktree workspace identity it was dispatched with.
func (s *Store) MovePublicPane(ctx context.Context, launchPaneID, oldPublicPaneID, newPublicPaneID string) error {
	workspace, err := workspaceOf(newPublicPaneID)
	if err != nil {
		return err
	}
	if launchPaneID == "" {
		return fmt.Errorf("%w: launch pane is required", ErrInvalid)
	}
	tx, err := s.begin(ctx, "move public pane of "+launchPaneID)
	if err != nil {
		return err
	}
	defer tx.rollback()

	current, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE launch_pane_id = ?`, launchPaneID),
		"task on launch pane "+launchPaneID)
	if err != nil {
		return err
	}
	if current.PublicPaneID != oldPublicPaneID {
		return fmt.Errorf("%w: task %q public pane is %q, expected %q",
			ErrConflict, current.ID, current.PublicPaneID, oldPublicPaneID)
	}

	moved := current
	moved.PublicPaneID = newPublicPaneID
	moved.CurrentWorkspaceID = workspace
	moved.Revision = current.Revision + 1
	moved.UpdatedAt = time.Now().UnixMilli()
	data, err := json.Marshal(moved)
	if err != nil {
		return fmt.Errorf("encode task %q: %w", current.ID, err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET data = ?, revision = revision + 1, public_pane_id = ?
		 WHERE id = ? AND revision = ?`,
		string(data), newPublicPaneID, current.ID, current.Revision)
	if err != nil {
		return fmt.Errorf("move public pane of task %q: %w", current.ID, err)
	}
	if err := requireOneRow(res, ErrConflict, "move public pane of task "+current.ID); err != nil {
		return err
	}
	return tx.commit("move public pane of task " + current.ID)
}

// RecordReady stores the OMP session a launch pane just announced. The
// handshake stays unverified until ConfirmReady sees the same session.
func (s *Store) RecordReady(ctx context.Context, launchPaneID, ompSessionID string, protocol int) error {
	if launchPaneID == "" || ompSessionID == "" {
		return fmt.Errorf("%w: launch pane and OMP session id are required", ErrInvalid)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO ready_sessions (launch_pane_id, omp_session_id, protocol, verified, seen_at)
		VALUES (?, ?, ?, 0, ?)
		ON CONFLICT (launch_pane_id) DO UPDATE SET
			omp_session_id = excluded.omp_session_id,
			protocol = excluded.protocol,
			verified = CASE
				WHEN ready_sessions.omp_session_id = excluded.omp_session_id THEN ready_sessions.verified
				ELSE 0
			END,
			seen_at = excluded.seen_at`,
		launchPaneID, ompSessionID, protocol, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("record ready for launch pane %q: %w", launchPaneID, err)
	}
	return nil
}

// ConfirmReady marks a recorded handshake verified, and only for the exact OMP
// session that was announced.
func (s *Store) ConfirmReady(ctx context.Context, launchPaneID, ompSessionID string) error {
	if launchPaneID == "" || ompSessionID == "" {
		return fmt.Errorf("%w: launch pane and OMP session id are required", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE ready_sessions SET verified = 1, seen_at = ? WHERE launch_pane_id = ? AND omp_session_id = ?`,
		time.Now().UnixMilli(), launchPaneID, ompSessionID)
	if err != nil {
		return fmt.Errorf("confirm ready for launch pane %q: %w", launchPaneID, err)
	}
	if err := requireOneRow(res, ErrNotFound, fmt.Sprintf("confirm ready for launch pane %q session %q", launchPaneID, ompSessionID)); err != nil {
		return err
	}
	return nil
}

// Ready reports whether exactly this OMP session passed the handshake.
func (s *Store) Ready(ctx context.Context, launchPaneID, ompSessionID string) (bool, error) {
	var verified int
	err := s.db.QueryRowContext(ctx,
		`SELECT verified FROM ready_sessions WHERE launch_pane_id = ? AND omp_session_id = ?`,
		launchPaneID, ompSessionID).Scan(&verified)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read ready for launch pane %q: %w", launchPaneID, err)
	}
	return verified == 1, nil
}

// AddInbox stores a note once, reporting whether it was new.
func (s *Store) AddInbox(ctx context.Context, in task.Inbox) (bool, error) {
	if in.ID == "" {
		return false, fmt.Errorf("%w: inbox id is required", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO inbox (id, project_key, task_id, kind, summary, delivery)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		in.ID, in.ProjectKey, in.TaskID, in.Kind, in.Summary, in.Delivery)
	if err != nil {
		return false, fmt.Errorf("add inbox %q: %w", in.ID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("add inbox %q: %w", in.ID, err)
	}
	return rows == 1, nil
}

// CreateProject records a new primary project.
func (s *Store) CreateProject(ctx context.Context, p task.Project) error {
	if p.Key == "" {
		return fmt.Errorf("%w: project key is required", ErrInvalid)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode project %q: %w", p.Key, err)
	}
	tx, err := s.begin(ctx, "create project "+p.Key)
	if err != nil {
		return err
	}
	defer tx.rollback()

	if taken, err := rowExists(ctx, tx, `SELECT 1 FROM projects WHERE key = ?`, p.Key); err != nil {
		return err
	} else if taken {
		return fmt.Errorf("%w: project %q already exists", ErrConflict, p.Key)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects (key, data) VALUES (?, ?)`, p.Key, string(data)); err != nil {
		return fmt.Errorf("create project %q: %w", p.Key, err)
	}
	return tx.commit("create project " + p.Key)
}

// Project reads one primary project by key.
func (s *Store) Project(ctx context.Context, key string) (task.Project, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM projects WHERE key = ?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Project{}, fmt.Errorf("%w: project %q", ErrNotFound, key)
	}
	if err != nil {
		return task.Project{}, fmt.Errorf("read project %q: %w", key, err)
	}
	var p task.Project
	if err := json.Unmarshal(data, &p); err != nil {
		return task.Project{}, fmt.Errorf("decode project %q: %w", key, err)
	}
	return p, nil
}

// UpdateProject applies mutate to the stored project facts. Project keys are immutable.
func (s *Store) UpdateProject(ctx context.Context, key string, mutate func(*task.Project) error) error {
	if key == "" {
		return fmt.Errorf("%w: project key is required", ErrInvalid)
	}
	if mutate == nil {
		return fmt.Errorf("%w: nil mutation for project %q", ErrInvalid, key)
	}
	tx, err := s.begin(ctx, "update project "+key)
	if err != nil {
		return err
	}
	defer tx.rollback()

	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM projects WHERE key = ?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: project %q", ErrNotFound, key)
	}
	if err != nil {
		return fmt.Errorf("read project %q: %w", key, err)
	}
	var current task.Project
	if err := json.Unmarshal(data, &current); err != nil {
		return fmt.Errorf("decode project %q: %w", key, err)
	}

	next := current
	if err := mutate(&next); err != nil {
		return fmt.Errorf("mutate project %q: %w", key, err)
	}
	if next.Key != current.Key {
		return fmt.Errorf("%w: project %q key is immutable", ErrInvalid, key)
	}
	updated, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode project %q: %w", key, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE projects SET data = ? WHERE key = ?`, string(updated), key)
	if err != nil {
		return fmt.Errorf("update project %q: %w", key, err)
	}
	if err := requireOneRow(res, ErrNotFound, "update project "+key); err != nil {
		return err
	}
	return tx.commit("update project " + key)
}

// ByOMPSession finds the single project or task that owns an OMP session, and
// refuses to guess when more than one claims it.
func (s *Store) ByOMPSession(ctx context.Context, ompSessionID string) (Binding, error) {
	if ompSessionID == "" {
		return Binding{}, fmt.Errorf("%w: OMP session id is required", ErrInvalid)
	}
	// Both lookups run in one read-only transaction so they see a single
	// database snapshot: concurrent writers must not make a session that had
	// exactly one owner at every instant look missing or ambiguous.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Binding{}, fmt.Errorf("begin read for OMP session %q: %w", ompSessionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	workers, err := queryFacts[task.Task](ctx, tx,
		`SELECT data FROM tasks WHERE json_extract(data, '$.OMPSessionID') = ?`, ompSessionID)
	if err != nil {
		return Binding{}, fmt.Errorf("read tasks for OMP session %q: %w", ompSessionID, err)
	}
	primaries, err := queryFacts[task.Project](ctx, tx,
		`SELECT data FROM projects WHERE json_extract(data, '$.OMPSessionID') = ?`, ompSessionID)
	if err != nil {
		return Binding{}, fmt.Errorf("read projects for OMP session %q: %w", ompSessionID, err)
	}
	switch {
	case len(workers)+len(primaries) == 0:
		return Binding{}, fmt.Errorf("%w: OMP session %q", ErrNotFound, ompSessionID)
	case len(workers)+len(primaries) > 1:
		return Binding{}, fmt.Errorf("%w: OMP session %q matches %d records",
			ErrAmbiguous, ompSessionID, len(workers)+len(primaries))
	case len(primaries) == 1:
		return Binding{Role: RolePrimary, Project: primaries[0]}, nil
	default:
		return Binding{Role: RoleWorker, Task: workers[0]}, nil
	}
}

func scanTask(row interface{ Scan(...any) error }, what string) (task.Task, error) {
	var data []byte
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return task.Task{}, fmt.Errorf("%w: %s", ErrNotFound, what)
		}
		return task.Task{}, fmt.Errorf("read %s: %w", what, err)
	}
	var t task.Task
	if err := json.Unmarshal(data, &t); err != nil {
		return task.Task{}, fmt.Errorf("decode %s: %w", what, err)
	}
	return t, nil
}

// querier is the read surface shared by *sql.DB and a read-only *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryFacts[T any](ctx context.Context, q querier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func rowExists(ctx context.Context, tx *tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe %q: %w", query, err)
	}
	return true, nil
}

// checkPaneFree refuses a launch pane already bound to a different task.
func checkPaneFree(ctx context.Context, tx *tx, launchPaneID, taskID string) error {
	if launchPaneID == "" {
		return nil
	}
	taken, err := rowExists(ctx, tx, `SELECT 1 FROM tasks WHERE launch_pane_id = ? AND id <> ?`, launchPaneID, taskID)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("%w: launch pane %q is bound to another task", ErrConflict, launchPaneID)
	}
	return nil
}

// requireOneRow turns a guarded write that matched no row into sentinel.
func requireOneRow(res sql.Result, sentinel error, what string) error {
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s changed %d rows", sentinel, what, rows)
	}
	return nil
}

// nullablePane encodes an unbound pane as SQL NULL so the partial unique index ignores it.
func nullablePane(launchPaneID string) any {
	if launchPaneID == "" {
		return nil
	}
	return launchPaneID
}

// workspaceOf splits a "workspace:pane" pane ID into its workspace.
func workspaceOf(paneID string) (string, error) {
	workspace, _, ok := strings.Cut(paneID, ":")
	if !ok || workspace == "" {
		return "", fmt.Errorf("%w: pane id %q is not workspace:pane", ErrInvalid, paneID)
	}
	return workspace, nil
}

// tx wraps a write transaction so every error path can roll back.
type tx struct {
	*sql.Tx
}

func (s *Store) begin(ctx context.Context, what string) (*tx, error) {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin %s: %w", what, err)
	}
	return &tx{t}, nil
}

func (t *tx) commit(what string) error {
	if err := t.Tx.Commit(); err != nil {
		return fmt.Errorf("commit %s: %w", what, err)
	}
	return nil
}

func (t *tx) rollback() {
	_ = t.Tx.Rollback()
}
