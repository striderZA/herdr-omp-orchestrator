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
	// ErrMismatch reports an announced session that is not the identity the
	// record was confirmed (or announced) with. It is never a reason to replace
	// the recorded identity: a human decides which process owns the pane.
	ErrMismatch = errors.New("state: announced session does not match the recorded reference")
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
	omp_session_id TEXT NOT NULL DEFAULT '',
	omp_session_file TEXT NOT NULL DEFAULT '',
	omp_session_on_disk INTEGER NOT NULL DEFAULT 0,
	protocol INTEGER NOT NULL,
	verified INTEGER NOT NULL DEFAULT 0,
	verified_kind TEXT NOT NULL DEFAULT '',
	verified_value TEXT NOT NULL DEFAULT '',
	seen_at INTEGER NOT NULL
);
`

// readySessionColumns are the columns a ready record needs beyond its first
// release shape. A state database written by an older revision is migrated in
// place, so an existing installation keeps its rows.
var readySessionColumns = []struct{ name, ddl string }{
	{"omp_session_file", `ALTER TABLE ready_sessions ADD COLUMN omp_session_file TEXT NOT NULL DEFAULT ''`},
	{"omp_session_on_disk", `ALTER TABLE ready_sessions ADD COLUMN omp_session_on_disk INTEGER NOT NULL DEFAULT 0`},
	{"verified_kind", `ALTER TABLE ready_sessions ADD COLUMN verified_kind TEXT NOT NULL DEFAULT ''`},
	{"verified_value", `ALTER TABLE ready_sessions ADD COLUMN verified_value TEXT NOT NULL DEFAULT ''`},
}

// migrate adds the columns an older state database is missing. SQLite has no
// ADD COLUMN IF NOT EXISTS, so the existing columns are read first.
func migrate(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(ready_sessions)`)
	if err != nil {
		return fmt.Errorf("inspect ready_sessions: %w", err)
	}
	existing := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultV   any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultV, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read ready_sessions columns: %w", err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read ready_sessions columns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close ready_sessions columns: %w", err)
	}
	for _, column := range readySessionColumns {
		if existing[column.name] {
			continue
		}
		if _, err := db.ExecContext(ctx, column.ddl); err != nil {
			return fmt.Errorf("add ready_sessions.%s: %w", column.name, err)
		}
	}
	return nil
}

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
	if err := migrate(context.Background(), db); err != nil {
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

// Tasks reads every task of one project, oldest first and then by id so a caller
// always sees the same order. It is a read-only projection: the coordinator uses
// it for the concurrency budget, dependency waiting and reserved-path ownership,
// and the board uses it for its columns.
func (s *Store) Tasks(ctx context.Context, projectKey string) ([]task.Task, error) {
	if strings.TrimSpace(projectKey) == "" {
		return nil, fmt.Errorf("%w: project key is required", ErrInvalid)
	}
	tasks, err := queryFacts[task.Task](ctx, s.db,
		`SELECT data FROM tasks WHERE project_key = ?
		 ORDER BY json_extract(data, '$.CreatedAt'), id`, projectKey)
	if err != nil {
		return nil, fmt.Errorf("read the tasks of %q: %w", projectKey, err)
	}
	return tasks, nil
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
func (s *Store) RecordReady(ctx context.Context, launchPaneID string, announced task.Announcement, protocol int) error {
	identity, ok := task.NewAnnouncement(announced.ID, announced.File, announced.OnDisk)
	if !ok {
		return fmt.Errorf("%w: an announced session carries no identity", ErrInvalid)
	}
	if strings.TrimSpace(launchPaneID) == "" {
		return fmt.Errorf("%w: launch pane is required", ErrInvalid)
	}
	// A new announcement invalidates a confirmation that no longer describes it:
	// a restart announces a different session, and the old confirmation must not
	// outlive it.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO ready_sessions (launch_pane_id, omp_session_id, omp_session_file, omp_session_on_disk,
		                        protocol, verified_kind, verified_value, verified, seen_at)
		VALUES (?, ?, ?, ?, ?, '', '', 0, ?)
		ON CONFLICT (launch_pane_id) DO UPDATE SET
			omp_session_id = excluded.omp_session_id,
			omp_session_file = excluded.omp_session_file,
			omp_session_on_disk = excluded.omp_session_on_disk,
			protocol = excluded.protocol,
			verified_kind = CASE
				WHEN ready_sessions.verified_kind = 'id' AND ready_sessions.verified_value = excluded.omp_session_id THEN ready_sessions.verified_kind
				WHEN ready_sessions.verified_kind = 'path' AND ready_sessions.verified_value = excluded.omp_session_file THEN ready_sessions.verified_kind
				ELSE ''
			END,
			verified_value = CASE
				WHEN ready_sessions.verified_kind = 'id' AND ready_sessions.verified_value = excluded.omp_session_id THEN ready_sessions.verified_value
				WHEN ready_sessions.verified_kind = 'path' AND ready_sessions.verified_value = excluded.omp_session_file THEN ready_sessions.verified_value
				ELSE ''
			END,
			verified = CASE
				WHEN ready_sessions.verified_kind = 'id' AND ready_sessions.verified_value = excluded.omp_session_id THEN ready_sessions.verified
				WHEN ready_sessions.verified_kind = 'path' AND ready_sessions.verified_value = excluded.omp_session_file THEN ready_sessions.verified
				ELSE 0
			END,
			seen_at = excluded.seen_at`,
		launchPaneID, identity.ID, identity.File, identity.OnDisk, protocol, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("record ready for launch pane %q: %w", launchPaneID, err)
	}
	return nil
}

// ConfirmReady records which reference form was verified for a launch pane, and
// only for a reference the pane actually announced: the recorded confirmation
// stays the exact identity `agent get` reported. A different form of the same
// announcement (its id when the file was verified, or the file when the id was)
// is accepted, because Herdr may report either.
func (s *Store) ConfirmReady(ctx context.Context, launchPaneID string, ref task.SessionRef) error {
	normalized, ok := ref.Normalized()
	if !ok {
		return fmt.Errorf("%w: session reference %q/%q is not usable", ErrInvalid, ref.Kind, ref.Value)
	}
	if strings.TrimSpace(launchPaneID) == "" {
		return fmt.Errorf("%w: launch pane is required", ErrInvalid)
	}
	recorded, found, err := s.readyRecord(ctx, launchPaneID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: no announced session for launch pane %q", ErrNotFound, launchPaneID)
	}
	if !recorded.announcement.Matches(normalized) {
		return fmt.Errorf("%w: launch pane %q announced %s, not the confirmed %s %q",
			ErrMismatch, launchPaneID, recorded.announcement.Describe(), normalized.Kind, normalized.Value)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE ready_sessions SET verified = 1, verified_kind = ?, verified_value = ?, seen_at = ?
		 WHERE launch_pane_id = ?`,
		normalized.Kind, normalized.Value, time.Now().UnixMilli(), launchPaneID)
	if err != nil {
		return fmt.Errorf("confirm ready for launch pane %q: %w", launchPaneID, err)
	}
	if err := requireOneRow(res, ErrNotFound, fmt.Sprintf("confirm ready for launch pane %q", launchPaneID)); err != nil {
		return err
	}
	return nil
}

// Ready reports whether an announced identity passed the handshake for a launch
// pane. The recorded confirmation decides which form is compared: an id is
// trimmed and compared case-insensitively, a path with the platform's path rules.
// A record that was never confirmed, or a confirmation whose kind this build does
// not understand, is never ready.
func (s *Store) Ready(ctx context.Context, launchPaneID string, announced task.Announcement) (bool, error) {
	identity, ok := task.NewAnnouncement(announced.ID, announced.File, announced.OnDisk)
	if !ok {
		return false, nil
	}
	recorded, found, err := s.readyRecord(ctx, launchPaneID)
	if err != nil || !found {
		return false, err
	}
	if recorded.verifiedKind == "" || recorded.verifiedValue == "" {
		return false, nil
	}
	value, ok := recorded.announcement.Ref(recorded.verifiedKind)
	if !ok {
		return false, nil
	}
	announcedValue, ok := identity.Ref(recorded.verifiedKind)
	if !ok {
		return false, nil
	}
	return task.SameSessionReference(recorded.verifiedKind, value.Value, announcedValue.Value), nil
}

// AnnouncedSession reads what a launch pane announced, including whether the
// runtime had materialized that session to disk. It reports whether anything was
// announced at all.
//
// The announced state is what a caller may report to a human; it is never proof
// of readiness (that stays ConfirmReady) and never proof of durability.
func (s *Store) AnnouncedSession(ctx context.Context, launchPaneID string) (task.Announcement, bool, error) {
	if strings.TrimSpace(launchPaneID) == "" {
		return task.Announcement{}, false, fmt.Errorf("%w: launch pane is required", ErrInvalid)
	}
	recorded, found, err := s.readyRecord(ctx, launchPaneID)
	if err != nil || !found {
		return task.Announcement{}, false, err
	}
	return recorded.announcement, true, nil
}

// readyRecord is the stored handshake of one launch pane.
type readyRecord struct {
	announcement  task.Announcement
	verifiedKind  string
	verifiedValue string
}

func (s *Store) readyRecord(ctx context.Context, launchPaneID string) (readyRecord, bool, error) {
	var row readyRecord
	var onDisk int
	err := s.db.QueryRowContext(ctx,
		`SELECT omp_session_id, omp_session_file, omp_session_on_disk, verified_kind, verified_value
		 FROM ready_sessions WHERE launch_pane_id = ?`, launchPaneID).
		Scan(&row.announcement.ID, &row.announcement.File, &onDisk, &row.verifiedKind, &row.verifiedValue)
	row.announcement.OnDisk = onDisk == 1
	if errors.Is(err, sql.ErrNoRows) {
		return readyRecord{}, false, nil
	}
	if err != nil {
		return readyRecord{}, false, fmt.Errorf("read ready for launch pane %q: %w", launchPaneID, err)
	}
	return row, true, nil
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

// ProjectByLaunchPane reads the primary project that owns a launch-time pane
// ID. A viewer of the pane (the bridge, after a restart) identifies the primary
// by the pane Herdr gave its process, so this lookup is the counterpart of
// ByLaunchPane for workers.
func (s *Store) ProjectByLaunchPane(ctx context.Context, launchPaneID string) (task.Project, error) {
	if strings.TrimSpace(launchPaneID) == "" {
		return task.Project{}, fmt.Errorf("%w: launch pane is required", ErrInvalid)
	}
	var data []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT data FROM projects WHERE json_extract(data, '$.LaunchPaneID') = ? LIMIT 1`, launchPaneID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Project{}, fmt.Errorf("%w: project on launch pane %q", ErrNotFound, launchPaneID)
	}
	if err != nil {
		return task.Project{}, fmt.Errorf("read the project on launch pane %q: %w", launchPaneID, err)
	}
	var p task.Project
	if err := json.Unmarshal(data, &p); err != nil {
		return task.Project{}, fmt.Errorf("decode the project on launch pane %q: %w", launchPaneID, err)
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

// ByOMPSession finds the single project or task that owns an announced session
// and refuses to guess when more than one claims it.
//
// An announcement carries both native identities, and a record stores whichever
// identity the coordinator recorded (Herdr reports an id on some builds and a
// session file on others), so either announced identity may match: the id is
// compared case-insensitively, the file with the platform's path rules. Rows
// whose recorded identity is empty never match.
func (s *Store) ByOMPSession(ctx context.Context, announced task.Announcement) (Binding, error) {
	identity, ok := task.NewAnnouncement(announced.ID, announced.File, announced.OnDisk)
	if !ok {
		return Binding{}, fmt.Errorf("%w: an announced session carries no identity", ErrInvalid)
	}
	// Both lookups run in one read-only transaction so they see a single
	// database snapshot: concurrent writers must not make a session that had
	// exactly one owner at every instant look missing or ambiguous.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Binding{}, fmt.Errorf("begin read for the announced session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	workers, err := queryFacts[task.Task](ctx, tx,
		`SELECT data FROM tasks WHERE json_extract(data, '$.OMPSessionID') IS NOT NULL
		   AND json_extract(data, '$.OMPSessionID') <> ''`)
	if err != nil {
		return Binding{}, fmt.Errorf("read tasks for the announced session: %w", err)
	}
	primaries, err := queryFacts[task.Project](ctx, tx,
		`SELECT data FROM projects WHERE json_extract(data, '$.OMPSessionID') IS NOT NULL
		   AND json_extract(data, '$.OMPSessionID') <> ''`)
	if err != nil {
		return Binding{}, fmt.Errorf("read projects for the announced session: %w", err)
	}

	var matches []Binding
	for _, worker := range workers {
		if announcedMatchesRecorded(identity, worker.OMPSessionID) {
			matches = append(matches, Binding{Role: RoleWorker, Task: worker})
		}
	}
	for _, primary := range primaries {
		if announcedMatchesRecorded(identity, primary.OMPSessionID) {
			matches = append(matches, Binding{Role: RolePrimary, Project: primary})
		}
	}
	switch len(matches) {
	case 0:
		return Binding{}, fmt.Errorf("%w: announced session %s", ErrNotFound, identity.Describe())
	case 1:
		return matches[0], nil
	default:
		return Binding{}, fmt.Errorf("%w: announced session %s matches %d records",
			ErrAmbiguous, identity.Describe(), len(matches))
	}
}

// announcedMatchesRecorded reports whether a recorded session identity is one of
// the announced ones, compared in the form the kind implies.
func announcedMatchesRecorded(announced task.Announcement, recorded string) bool {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return false
	}
	if announced.ID != "" && task.SameSessionReference(task.SessionKindID, announced.ID, recorded) {
		return true
	}
	return announced.File != "" && task.SameSessionReference(task.SessionKindPath, announced.File, recorded)
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
