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
	"runtime"
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
	// ErrBudgetExhausted reports that a project already holds every provisioning
	// slot its concurrency budget allows. The claim changes nothing.
	ErrBudgetExhausted = errors.New("state: the provisioning budget is exhausted")
	// ErrReservedConflict reports reserved paths that a task of the same project
	// already holds. The claim changes nothing either.
	ErrReservedConflict = errors.New("state: reserved paths overlap another task")
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
	slot_claimed_at INTEGER NOT NULL DEFAULT 0,
	slot_token INTEGER NOT NULL DEFAULT 0,
	step_claim_owner TEXT NOT NULL DEFAULT '',
	step_claim_phase TEXT NOT NULL DEFAULT '',
	step_claim_until INTEGER NOT NULL DEFAULT 0,
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
	delivery TEXT NOT NULL,
	delivery_state TEXT NOT NULL DEFAULT 'pending',
	delivery_detail TEXT NOT NULL DEFAULT ''
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

// inboxColumns are the columns an inbox note needs beyond its first release
// shape. The state and its detail are additive: a note written by an older
// revision is pending, and its Delivery label keeps whatever it said.
var inboxColumns = []struct{ name, ddl string }{
	{"delivery_state", `ALTER TABLE inbox ADD COLUMN delivery_state TEXT NOT NULL DEFAULT 'pending'`},
	{"delivery_detail", `ALTER TABLE inbox ADD COLUMN delivery_detail TEXT NOT NULL DEFAULT ''`},
}

const inboxRecoveryDetail = "the plugin stopped while this delivery was in flight; inspect `agent get`/`agent read` before retrying"

// taskColumns are the columns a task needs beyond its first release shape. The
// provisioning slot is the one the dispatch gate uses: an older state database
// has no claimed slots, and every existing task is judged by the phase it
// recorded, which the gate reads as well.
var taskColumns = []struct{ name, ddl string }{
	{"slot_claimed_at", `ALTER TABLE tasks ADD COLUMN slot_claimed_at INTEGER NOT NULL DEFAULT 0`},
	{"slot_token", `ALTER TABLE tasks ADD COLUMN slot_token INTEGER NOT NULL DEFAULT 0`},
	{"step_claim_owner", `ALTER TABLE tasks ADD COLUMN step_claim_owner TEXT NOT NULL DEFAULT ''`},
	{"step_claim_phase", `ALTER TABLE tasks ADD COLUMN step_claim_phase TEXT NOT NULL DEFAULT ''`},
	{"step_claim_until", `ALTER TABLE tasks ADD COLUMN step_claim_until INTEGER NOT NULL DEFAULT 0`},
}

// migratedColumns are the additive columns of every table, in the order they are
// applied. SQLite has no ADD COLUMN IF NOT EXISTS, so each table's existing
// columns are read before the missing ones are added.
var migratedColumns = []struct {
	table   string
	columns []struct{ name, ddl string }
}{
	{"ready_sessions", readySessionColumns},
	{"inbox", inboxColumns},
	{"tasks", taskColumns},
}

// postMigrateIndexes creates the indexes that name a column an older state
// database only gets from the additive migration above. Creating them with the
// tables would make an existing installation fail to open: SQLite rejects an
// index on a column the table does not have yet.
const postMigrateIndexes = `
CREATE INDEX IF NOT EXISTS inbox_pending ON inbox (project_key, delivery_state);
`

// migrate holds SQLite's immediate write lock across column discovery and every
// ALTER. Otherwise two plugin processes can both observe a missing column and
// the second fails with a duplicate-column error after the first adds it.
func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema migration: %w", err)
	}
	defer tx.Rollback()
	for _, table := range migratedColumns {
		existing, err := columnNames(ctx, tx, table.table)
		if err != nil {
			return err
		}
		for _, column := range table.columns {
			if existing[column.name] {
				continue
			}
			if _, err := tx.ExecContext(ctx, column.ddl); err != nil {
				return fmt.Errorf("add %s.%s: %w", table.table, column.name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}

// columnNames reads the column names of one table within the migration lock.
func columnNames(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", table, err)
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
			return nil, fmt.Errorf("read %s columns: %w", table, err)
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read %s columns: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close %s columns: %w", table, err)
	}
	return existing, nil
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
	if _, err := db.Exec(schema + historySchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %q: %w", path, err)
	}
	if err := migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %q: %w", path, err)
	}
	// The indexes that depend on migrated columns come after the migration, not
	// with the tables: an existing installation has the old table shape.
	if _, err := db.Exec(postMigrateIndexes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("index %q: %w", path, err)
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
	if err := recordCreated(ctx, tx, t); err != nil {
		return err
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

	next, err := updateTask(ctx, tx, id, expectedRevision, mutate)
	if err != nil {
		return task.Task{}, err
	}
	if err := tx.commit("update task " + id); err != nil {
		return task.Task{}, err
	}
	return next, nil
}

// UpdateTaskWithNote applies mutate to a task and stores one deduplicated inbox
// note in the same transaction: either both are durable or neither is.
//
// Some facts are only meaningful together. A worker's final report and the note
// that announces it are one fact, and a store that could record the report
// without the note would show a task ready for review that the primary was never
// told about. The task side keeps every guarantee UpdateTask gives (revision
// compare-and-swap, immutable id and project key, pane exclusivity) and the note
// keeps AddInbox's deduplication: repeating an existing note changes nothing and
// reports false.
func (s *Store) UpdateTaskWithNote(
	ctx context.Context,
	id string,
	expectedRevision int64,
	mutate func(*task.Task) error,
	note task.Inbox,
) (task.Task, bool, error) {
	if mutate == nil {
		return task.Task{}, false, fmt.Errorf("%w: nil mutation for task %q", ErrInvalid, id)
	}
	if note.ID == "" {
		return task.Task{}, false, fmt.Errorf("%w: inbox id is required", ErrInvalid)
	}
	tx, err := s.begin(ctx, "update task "+id+" with note "+note.ID)
	if err != nil {
		return task.Task{}, false, err
	}
	defer tx.rollback()

	next, err := updateTask(ctx, tx, id, expectedRevision, mutate)
	if err != nil {
		return task.Task{}, false, err
	}
	inserted, err := addInbox(ctx, tx, note)
	if err != nil {
		return task.Task{}, false, err
	}
	if err := tx.commit("update task " + id + " with note " + note.ID); err != nil {
		return task.Task{}, false, err
	}
	return next, inserted, nil
}

// updateTask applies one compare-and-swap update inside tx, leaving the commit to
// its caller so a step can be part of a larger transaction.
func updateTask(ctx context.Context, tx *tx, id string, expectedRevision int64, mutate func(*task.Task) error) (task.Task, error) {
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
	if err := appendHistory(ctx, tx, next, diffTransitions(current, next)); err != nil {
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
	if err := appendHistory(ctx, tx, moved, diffTransitions(current, moved)); err != nil {
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

// VerifiedAnnouncement reads the handshake a launch pane announced, and returns it
// only when that very row's confirmation describes that same row's announcement.
//
// It is the one-read form of the handshake, and the only form a caller deciding
// identity or liveness may use: Announcement.Proves bridges two native identities,
// so pairing an announcement from one read with a confirmation from another would
// let a refresh that kept the confirmed form (a new id for the same session file)
// be judged against the stale identity. A row that was never confirmed, a
// confirmation whose kind this build cannot read, and a confirmation left behind
// by an earlier announcement all report false.
func (s *Store) VerifiedAnnouncement(ctx context.Context, launchPaneID string) (task.Announcement, bool, error) {
	snapshot, err := s.Readiness(ctx, launchPaneID)
	if err != nil {
		return task.Announcement{}, false, err
	}
	if !snapshot.Verified {
		return task.Announcement{}, false, nil
	}
	return snapshot.Announcement, true, nil
}

// HandshakeSnapshot is one launch pane's stored readiness row.
// StoredVerified preserves the raw database flag; Verified is the effective
// verifiedRef result used by identity checks. They can differ for malformed or
// legacy rows, which is diagnostic evidence—not permission to relax a proof.
type HandshakeSnapshot struct {
	Announcement   task.Announcement
	ConfirmedRef   task.SessionRef
	Found          bool
	StoredVerified int
	Verified       bool
	SeenAt         int64
}

// Readiness reads the announcement, confirmation and update time from one row.
// It retains an unverified announcement so a refusal can distinguish “no proof”
// from “a different confirmed session” without composing two racing reads.
func (s *Store) Readiness(ctx context.Context, launchPaneID string) (HandshakeSnapshot, error) {
	if strings.TrimSpace(launchPaneID) == "" {
		return HandshakeSnapshot{}, fmt.Errorf("%w: launch pane is required", ErrInvalid)
	}
	recorded, found, err := s.readyRecord(ctx, launchPaneID)
	if err != nil {
		return HandshakeSnapshot{}, err
	}
	if !found {
		return HandshakeSnapshot{}, nil
	}
	snapshot := HandshakeSnapshot{
		Announcement:   recorded.announcement,
		Found:          true,
		StoredVerified: recorded.storedVerified,
		SeenAt:         recorded.seenAt,
	}
	_, confirmed, verified := recorded.verifiedRef()
	if verified {
		snapshot.ConfirmedRef = confirmed
		snapshot.Verified = true
	}
	return snapshot, nil
}

func (s *Store) Ready(ctx context.Context, launchPaneID string, announced task.Announcement) (bool, error) {
	identity, ok := task.NewAnnouncement(announced.ID, announced.File, announced.OnDisk)
	if !ok {
		return false, nil
	}
	recorded, found, err := s.readyRecord(ctx, launchPaneID)
	if err != nil || !found {
		return false, err
	}
	_, confirmed, ok := recorded.verifiedRef()
	if !ok {
		return false, nil
	}
	announcedValue, ok := identity.Ref(confirmed.Kind)
	if !ok {
		return false, nil
	}
	return task.SameSessionReference(confirmed.Kind, confirmed.Value, announcedValue.Value), nil
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
	announcement   task.Announcement
	verifiedKind   string
	verifiedValue  string
	storedVerified int
	seenAt         int64
}

// verifiedRef reports the reference this row's confirmation recorded, but only
// when that confirmation describes this row's own announcement.
//
// It is the single rule both readers below share, and it works on the row that was
// read — never on a row read separately — so a concurrent RecordReady cannot make
// a caller judge one announcement with another announcement's confirmation.
func (r readyRecord) verifiedRef() (task.Announcement, task.SessionRef, bool) {
	if r.verifiedKind == "" || r.verifiedValue == "" {
		return task.Announcement{}, task.SessionRef{}, false
	}
	announced, ok := r.announcement.Ref(r.verifiedKind)
	if !ok {
		return task.Announcement{}, task.SessionRef{}, false
	}
	if !task.SameSessionReference(r.verifiedKind, announced.Value, r.verifiedValue) {
		return task.Announcement{}, task.SessionRef{}, false
	}
	return r.announcement, task.SessionRef{Kind: r.verifiedKind, Value: r.verifiedValue}, true
}

func (s *Store) readyRecord(ctx context.Context, launchPaneID string) (readyRecord, bool, error) {
	var row readyRecord
	var onDisk, storedVerified int
	err := s.db.QueryRowContext(ctx,
		`SELECT omp_session_id, omp_session_file, omp_session_on_disk, verified_kind, verified_value, verified, seen_at
		 FROM ready_sessions WHERE launch_pane_id = ?`, launchPaneID).
		Scan(&row.announcement.ID, &row.announcement.File, &onDisk, &row.verifiedKind, &row.verifiedValue, &storedVerified, &row.seenAt)
	row.announcement.OnDisk = onDisk == 1
	row.storedVerified = storedVerified
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
	return addInbox(ctx, s.db, in)
}

// AddInboxForTask stores a note once, but only after reading the task the note
// belongs to and letting the caller decide whether the task may still be notified.
//
// Some notes may only exist while their task is in a state the caller owns: a
// worker's progress, question or report note is refused once the primary has
// disposed of the task. Checking that outside this call would leave the window
// open — the primary's decision could commit between the read and the insert — so
// the read, the guard and the insert are one transaction, and the store's
// `_txlock=immediate` DSN takes the write lock when it begins. The task itself is
// never written and its revision never moves: this is not a task change.
//
// The guard's error is returned as it is, so the caller's own refusal (for example
// a closed-task error it classifies for the bridge) survives. The note keeps
// AddInbox's deduplication: repeating an existing note reports false and writes
// nothing.
func (s *Store) AddInboxForTask(ctx context.Context, in task.Inbox, guard func(task.Task) error) (bool, error) {
	if in.ID == "" {
		return false, fmt.Errorf("%w: inbox id is required", ErrInvalid)
	}
	if strings.TrimSpace(in.TaskID) == "" {
		return false, fmt.Errorf("%w: inbox task id is required", ErrInvalid)
	}
	if guard == nil {
		return false, fmt.Errorf("%w: nil guard for note %q", ErrInvalid, in.ID)
	}
	tx, err := s.begin(ctx, "add note "+in.ID+" for task "+in.TaskID)
	if err != nil {
		return false, err
	}
	defer tx.rollback()

	current, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, in.TaskID),
		"task "+in.TaskID)
	if err != nil {
		return false, err
	}
	if err := guard(current); err != nil {
		return false, err
	}
	if current.ProjectKey != in.ProjectKey {
		return false, fmt.Errorf("%w: note %q names project %q, but task %s belongs to %q",
			ErrInvalid, in.ID, in.ProjectKey, current.ID, current.ProjectKey)
	}
	inserted, err := addInbox(ctx, tx, in)
	if err != nil {
		return false, err
	}
	if err := tx.commit("add note " + in.ID + " for task " + in.TaskID); err != nil {
		return false, err
	}
	return inserted, nil
}

// execer is the write surface shared by the database handle and one write
// transaction, so a helper can be used on its own or as part of a larger
// transaction without a second implementation of the same statement.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// addInbox stores one deduplicated note through q and reports whether it was
// new. The note's id is the identity: an insert that would only repeat it
// changes nothing.
//
// A note is always created pending: the caller that records a fact does not
// deliver it, and a flush claims it later. The state a caller passes is
// therefore validated and must be pending, so no path can start a note in a
// state only the claim machinery may produce.
func addInbox(ctx context.Context, q execer, in task.Inbox) (bool, error) {
	if in.ID == "" {
		return false, fmt.Errorf("%w: inbox id is required", ErrInvalid)
	}
	state, err := inboxState(in.DeliveryState, task.DeliveryPending)
	if err != nil {
		return false, err
	}
	res, err := q.ExecContext(ctx, `
		INSERT INTO inbox (id, project_key, task_id, kind, summary, delivery, delivery_state, delivery_detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		in.ID, in.ProjectKey, in.TaskID, in.Kind, in.Summary, in.Delivery, state, in.DeliveryDetail)
	if err != nil {
		return false, fmt.Errorf("add inbox %q: %w", in.ID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("add inbox %q: %w", in.ID, err)
	}
	return rows == 1, nil
}

// The columns every inbox read selects, in scanInbox's order.
const inboxSelect = `id, project_key, task_id, kind, summary, delivery, delivery_state, delivery_detail`

// inboxState validates one delivery state, defaulting an empty value.
func inboxState(state, fallback string) (string, error) {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return fallback, nil
	}
	switch trimmed {
	case task.DeliveryPending, task.DeliverySending, task.DeliveryDelivered, task.DeliveryUncertain:
		return trimmed, nil
	default:
		return "", fmt.Errorf("%w: %q is not a delivery state", ErrInvalid, state)
	}
}

func scanInbox(row interface{ Scan(...any) error }, what string) (task.Inbox, error) {
	var in task.Inbox
	err := row.Scan(&in.ID, &in.ProjectKey, &in.TaskID, &in.Kind, &in.Summary, &in.Delivery,
		&in.DeliveryState, &in.DeliveryDetail)
	if errors.Is(err, sql.ErrNoRows) {
		return task.Inbox{}, fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	if err != nil {
		return task.Inbox{}, fmt.Errorf("read %s: %w", what, err)
	}
	return in, nil
}

// Inbox reads one durable note by id, including its delivery state.
func (s *Store) Inbox(ctx context.Context, id string) (task.Inbox, error) {
	if strings.TrimSpace(id) == "" {
		return task.Inbox{}, fmt.Errorf("%w: inbox id is required", ErrInvalid)
	}
	return scanInbox(s.db.QueryRowContext(ctx, `SELECT `+inboxSelect+` FROM inbox WHERE id = ?`, id),
		"inbox "+id)
}

// PendingInbox reads the notes of one project that no flush has claimed yet,
// oldest first, so a flush batches a backlog in the order it happened.
func (s *Store) PendingInbox(ctx context.Context, projectKey string) ([]task.Inbox, error) {
	if strings.TrimSpace(projectKey) == "" {
		return nil, fmt.Errorf("%w: project key is required", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+inboxSelect+` FROM inbox WHERE project_key = ? AND delivery_state = ?
		 ORDER BY rowid`, projectKey, task.DeliveryPending)
	if err != nil {
		return nil, fmt.Errorf("read the pending inbox of %q: %w", projectKey, err)
	}
	defer func() { _ = rows.Close() }()
	var out []task.Inbox
	for rows.Next() {
		in, err := scanInbox(rows, "pending inbox row")
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the pending inbox of %q: %w", projectKey, err)
	}
	return out, nil
}

// ClaimInbox moves the named notes from pending to sending and returns the ones
// this caller claimed, in the order they were named.
//
// The claim is one compare-and-swap per note, so two flushes racing over the
// same backlog end with exactly one owner per note: a note another caller
// already claimed is simply not returned. A claimed note is never pending
// again until its attempt is settled, which is what keeps a retried flush from
// delivering the same note twice.
func (s *Store) ClaimInbox(ctx context.Context, ids []string) ([]task.Inbox, error) {
	claimed := make([]task.Inbox, 0, len(ids))
	tx, err := s.begin(ctx, "claim inbox notes")
	if err != nil {
		return nil, err
	}
	defer tx.rollback()
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: inbox id is required", ErrInvalid)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE inbox SET delivery_state = ? WHERE id = ? AND delivery_state = ?`,
			task.DeliverySending, id, task.DeliveryPending)
		if err != nil {
			return nil, fmt.Errorf("claim inbox %q: %w", id, err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("claim inbox %q: %w", id, err)
		}
		if rows != 1 {
			continue
		}
		in, err := scanInbox(tx.QueryRowContext(ctx, `SELECT `+inboxSelect+` FROM inbox WHERE id = ?`, id),
			"claimed inbox "+id)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, in)
	}
	if err := tx.commit("claim inbox notes"); err != nil {
		return nil, err
	}
	return claimed, nil
}

// SettleInbox moves a claimed note to its outcome and returns how many were
// settled. A sending note is owned by the caller; an uncertain note is settleable
// only when it carries the exact startup-recovery detail, so the original live
// caller can record its eventual result without overwriting unrelated uncertainty.
func (s *Store) SettleInbox(ctx context.Context, ids []string, state, detail string) (int, error) {
	next, err := inboxState(state, "")
	if err != nil {
		return 0, err
	}
	if next == task.DeliveryPending && strings.TrimSpace(detail) == "" {
		detail = "released for a later attempt"
	}
	tx, err := s.begin(ctx, "settle inbox notes")
	if err != nil {
		return 0, err
	}
	defer tx.rollback()
	settled := 0
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return 0, fmt.Errorf("%w: inbox id is required", ErrInvalid)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE inbox SET delivery_state = ?, delivery_detail = ?
			 WHERE id = ? AND (delivery_state = ? OR (delivery_state = ? AND delivery_detail = ?))`,
			next, detail, id, task.DeliverySending, task.DeliveryUncertain, inboxRecoveryDetail)
		if err != nil {
			return 0, fmt.Errorf("settle inbox %q: %w", id, err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("settle inbox %q: %w", id, err)
		}
		settled += int(rows)
	}
	if err := tx.commit("settle inbox notes"); err != nil {
		return 0, err
	}
	return settled, nil
}

// RecoverInbox marks sending notes uncertain during startup. If the original
// caller is still alive, its later SettleInbox may replace only this exact
// recovery marker with the result it observed; unrelated uncertainty remains
// untouched. A recovered note with no later result stays uncertain for a human
// to inspect before anything is retried. It reports how many notes it recovered.
func (s *Store) RecoverInbox(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE inbox SET delivery_state = ?, delivery_detail = ?
		 WHERE delivery_state = ?`,
		task.DeliveryUncertain,
		inboxRecoveryDetail,
		task.DeliverySending)
	if err != nil {
		return 0, fmt.Errorf("recover inbox deliveries: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recover inbox deliveries: %w", err)
	}
	return int(rows), nil
}

// ---------------------------------------------------------------------------
// the durable dispatch gate
// ---------------------------------------------------------------------------

// slotHolder is the condition under which a task holds one of its project's
// provisioning slots: it claimed one explicitly, or it already recorded a phase
// that owns a checkout and a pane, and nothing has closed it.
//
// The phase is part of the rule as well as the claim, because a state database
// written before the claim existed has rows whose resources are visible only
// through their phase, and a record that reached a resource-owning phase must
// never stop counting as one.
const slotHolder = `(slot_claimed_at > 0 OR ` + resourcePhaseSQL + `)
	AND COALESCE(json_extract(data, '$.Disposition'), '') = ''`

// resourcePhaseSQL selects the phases that own a checkout and a pane. Such a task
// holds its slot by its phase, so its claim is never handed back: releasing it
// would give away a slot whose worktree and worker still exist.
const resourcePhaseSQL = `COALESCE(json_extract(data, '$.Phase'), '') IN ('worktree_created', 'agent_started', 'prompt_submitted')`

// ownsResources reports whether one recorded phase already owns a checkout and a
// pane. It is the Go-side twin of resourcePhaseSQL, used where the phase is read
// as a value.
func ownsResources(phase string) bool {
	switch task.Phase(phase) {
	case task.WorktreeCreated, task.AgentStarted, task.PromptSubmitted:
		return true
	default:
		return false
	}
}

// claimedSlots counts the provisioning slots a project holds, ignoring one task
// (the task being claimed, which is judged by its own facts instead).
func claimedSlots(ctx context.Context, q rowQueryer, projectKey, excludeID string) (int, error) {
	var count int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE project_key = ? AND id <> ? AND `+slotHolder,
		projectKey, excludeID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count the provisioning slots of %q: %w", projectKey, err)
	}
	return count, nil
}

// taskHoldsSlot reports whether one recorded task already holds a slot.
func taskHoldsSlot(ctx context.Context, q rowQueryer, id string) (bool, error) {
	var holds int
	err := q.QueryRowContext(ctx,
		`SELECT CASE WHEN `+slotHolder+` THEN 1 ELSE 0 END FROM tasks WHERE id = ?`, id).Scan(&holds)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	if err != nil {
		return false, fmt.Errorf("read the slot of task %s: %w", id, err)
	}
	return holds == 1, nil
}

// ClaimTask records a new task and, when the project still has a provisioning
// slot, claims one for it in the same write transaction.
//
// This is the dispatch gate: the concurrency budget and the reserved paths are
// decided against the database's current contents inside one transaction, so two
// coordinator processes cannot both pass and provision workers past the limit,
// and cannot both take the same reserved paths. Both outcomes are durable: a task
// whose claim was refused for the budget is recorded (a queued task holds no
// resource) and must be claimed again before it is provisioned, while a
// reservation that another task holds refuses the whole request and records
// nothing.
//
// The claim it took is returned as its token (the slot_token column), so the
// caller that acquired the capacity is the one that can give it back. A refused
// slot reports a zero token: this call took none.
func (s *Store) ClaimTask(ctx context.Context, t task.Task, budget int) (bool, task.Task, int64, error) {
	if t.ID == "" || t.ProjectKey == "" {
		return false, task.Task{}, 0, fmt.Errorf("%w: task id and project key are required", ErrInvalid)
	}
	if budget < 1 {
		return false, task.Task{}, 0, fmt.Errorf("%w: a provisioning budget of at least one is required", ErrInvalid)
	}
	now := time.Now().UnixMilli()
	t.Revision = 1
	t.CreatedAt, t.UpdatedAt = now, now
	data, err := json.Marshal(t)
	if err != nil {
		return false, task.Task{}, 0, fmt.Errorf("encode task %q: %w", t.ID, err)
	}

	tx, err := s.begin(ctx, "claim task "+t.ID)
	if err != nil {
		return false, task.Task{}, 0, err
	}
	defer tx.rollback()

	if taken, err := rowExists(ctx, tx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID); err != nil {
		return false, task.Task{}, 0, err
	} else if taken {
		return false, task.Task{}, 0, fmt.Errorf("%w: task %q already exists", ErrConflict, t.ID)
	}
	if err := checkPaneFree(ctx, tx, t.LaunchPaneID, t.ID); err != nil {
		return false, task.Task{}, 0, err
	}
	if len(t.ReservedPaths) > 0 {
		existing, err := queryFacts[task.Task](ctx, tx, `SELECT data FROM tasks WHERE project_key = ?`, t.ProjectKey)
		if err != nil {
			return false, task.Task{}, 0, fmt.Errorf("read the tasks of %q: %w", t.ProjectKey, err)
		}
		if owner, mine, theirs, ok := reservedConflict(existing, t.ReservedPaths); ok {
			return false, task.Task{}, 0, fmt.Errorf("%w: task %s reserved %q (requested %q)",
				ErrReservedConflict, owner, theirs, mine)
		}
	}
	count, err := claimedSlots(ctx, tx, t.ProjectKey, t.ID)
	if err != nil {
		return false, task.Task{}, 0, err
	}
	claimed := count < budget
	var token int64
	if claimed {
		// A brand-new record has no previous claim, so the token is simply now.
		token = nextSlotToken(0)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (id, project_key, launch_pane_id, public_pane_id, revision, slot_claimed_at, slot_token, data)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.ProjectKey, nullablePane(t.LaunchPaneID), t.PublicPaneID, t.Revision, token, token, string(data),
	); err != nil {
		return false, task.Task{}, 0, fmt.Errorf("claim task %q: %w", t.ID, err)
	}
	if err := recordCreated(ctx, tx, t); err != nil {
		return false, task.Task{}, 0, err
	}
	if err := tx.commit("claim task " + t.ID); err != nil {
		return false, task.Task{}, 0, err
	}
	return claimed, t, token, nil
}

// nextSlotToken returns the next value of a task's slot claim identity. That value
// is the token that says *which* claim a caller holds, so it must always increase:
// the clock alone could repeat a millisecond, and a token that a released claim
// already used would let a stale release match a fresh claim. The token column is
// never reset, so previous is the greatest value the task has ever handed out.
func nextSlotToken(previous int64) int64 {
	if now := time.Now().UnixMilli(); now > previous {
		return now
	}
	return previous + 1
}

// ClaimProvisioningSlot claims a slot for a task that is already recorded, before
// any side effect touches Herdr. It reports whether *this call* took the claim and
// the token of the claim it holds afterwards:
//
//   - true — the claim was free and this call now holds it, so this caller owns
//     provisioning that task from here on; the token is the claim it just took.
//   - false — the task already held the claim (another call is provisioning it,
//     or it is the retry of an attempt that was interrupted), so this caller must
//     not provision it, and no token is reported: a deliberate resume is the one
//     caller that may continue with a claim it did not take, and it does so by
//     renewing it (RenewProvisioningSlot), which is what makes its ownership
//     visible to the attempt that failed.
//
// A project that holds every slot its budget allows reports ErrBudgetExhausted
// and changes nothing.
func (s *Store) ClaimProvisioningSlot(ctx context.Context, id string, budget int) (bool, int64, error) {
	if strings.TrimSpace(id) == "" {
		return false, 0, fmt.Errorf("%w: task id is required", ErrInvalid)
	}
	if budget < 1 {
		return false, 0, fmt.Errorf("%w: a provisioning budget of at least one is required", ErrInvalid)
	}
	tx, err := s.begin(ctx, "claim provisioning slot for "+id)
	if err != nil {
		return false, 0, err
	}
	defer tx.rollback()

	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, id), "task "+id)
	if err != nil {
		return false, 0, err
	}
	holds, err := taskHoldsSlot(ctx, tx, id)
	if err != nil {
		return false, 0, err
	}
	if holds {
		// Somebody already holds this task's claim: this call must not provision it.
		return false, 0, tx.commit("claim provisioning slot for " + id)
	}
	count, err := claimedSlots(ctx, tx, t.ProjectKey, id)
	if err != nil {
		return false, 0, err
	}
	if count >= budget {
		return false, 0, fmt.Errorf("%w: %q already holds %d of %d provisioning slots",
			ErrBudgetExhausted, t.ProjectKey, count, budget)
	}
	var previous int64
	if err := tx.QueryRowContext(ctx, `SELECT slot_token FROM tasks WHERE id = ?`, id).Scan(&previous); err != nil {
		return false, 0, fmt.Errorf("read the slot token of %s: %w", id, err)
	}
	token := nextSlotToken(previous)
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET slot_claimed_at = ?, slot_token = ? WHERE id = ? AND slot_claimed_at = 0`,
		token, token, id)
	if err != nil {
		return false, 0, fmt.Errorf("claim provisioning slot for %s: %w", id, err)
	}
	if err := requireOneRow(res, ErrConflict, "claim provisioning slot for "+id); err != nil {
		return false, 0, err
	}
	if err := tx.commit("claim provisioning slot for " + id); err != nil {
		return false, 0, err
	}
	return true, token, nil
}

// RenewProvisioningSlot makes the step owner's capacity claim its own, and returns
// the fresh token.
//
// It exists for the one caller that deliberately continues with a claim it did not
// take: a resume of an interrupted attempt. Between that decision and the step the
// resume runs there is a window in which the claim is only *inferred* to be in use
// (the row still says somebody holds it), and the attempt that failed may still be
// on its way to giving the slot back. Writing the renewal closes that window: the
// claim the resume relies on is now a token that no older attempt holds, so the late
// release of the failed attempt (which presents the token it acquired) cannot clear
// it.
//
// The renewal is fenced on owning the task's live step claim, and that fence is the
// point of the ordering: renewing before the step were taken would let a resume that
// then loses the step (ErrStepInFlight) leave the capacity held by nobody — the
// attempt that failed cannot release a token that was renewed past it, and no drain
// may provision a task whose claim somebody holds. So a caller that owns the step is
// exactly the caller allowed to renew, and owner is the identity of that claim.
//
// The renewal is not a new claim: it changes no count, takes nothing from the
// budget, and a task that holds no slot at all (neither a claim nor a
// resource-owning phase) is refused with ErrConflict so the caller claims instead.
func (s *Store) RenewProvisioningSlot(ctx context.Context, id, owner string) (int64, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(owner) == "" {
		return 0, fmt.Errorf("%w: task id and owner are required", ErrInvalid)
	}
	tx, err := s.begin(ctx, "renew the provisioning slot of "+id)
	if err != nil {
		return 0, err
	}
	defer tx.rollback()

	var claimedAt, current, heldUntil int64
	var phase, holder string
	err = tx.QueryRowContext(ctx,
		`SELECT slot_claimed_at, slot_token, step_claim_owner, step_claim_until,
		        COALESCE(json_extract(data, '$.Phase'), '')
		 FROM tasks WHERE id = ?`, id).
		Scan(&claimedAt, &current, &holder, &heldUntil, &phase)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	if err != nil {
		return 0, fmt.Errorf("read the slot of task %s: %w", id, err)
	}
	if holder != owner || heldUntil <= time.Now().UnixMilli() {
		return 0, fmt.Errorf("%w: task %s is running its %s step for %q, not %q, so %q may not renew its slot claim",
			ErrConflict, id, phase, holder, owner, owner)
	}
	if claimedAt == 0 && !ownsResources(phase) {
		return 0, fmt.Errorf("%w: task %s holds no provisioning slot to renew", ErrConflict, id)
	}
	token := nextSlotToken(current)
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET slot_token = ? WHERE id = ? AND slot_token = ?`,
		token, id, current)
	if err != nil {
		return 0, fmt.Errorf("renew the provisioning slot of %s: %w", id, err)
	}
	if err := requireOneRow(res, ErrConflict, "renew the provisioning slot of "+id); err != nil {
		return 0, err
	}
	if err := tx.commit("renew the provisioning slot of " + id); err != nil {
		return 0, err
	}
	return token, nil
}

// ReleaseProvisioningSlot gives a claimed slot back, for a task whose
// provisioning failed before it recorded any resource. The phase still decides
// the slot for a task that reached one, so releasing a claim never frees a
// checkout or a live worker.
//
// The release is atomic and fenced on the claim it belongs to, so it can only ever
// give back the capacity this caller took:
//
//   - token must be the value this caller acquired (from ClaimTask or
//     ClaimProvisioningSlot); a claim that has since been taken by somebody else,
//     renewed by a deliberate resume, or already released no longer matches, and
//     the release is refused as ErrConflict. That is what keeps a retry's capacity
//     claim from being erased by the attempt that failed before it — including the
//     window between the retry's decision to reuse the claim and its first step,
//     where nothing else about the task would show the reuse. The token column is
//     never reset (a released claim keeps its value), so a token is never reused by
//     a later claim of the same task and can never match one;
//   - a task whose step somebody holds right now is being provisioned by that
//     owner, and the capacity is theirs: refused as ErrConflict as well;
//   - the phase, the claim token and the release are read and written in one write
//     transaction, so no retry can slip between the fence and the clear.
func (s *Store) ReleaseProvisioningSlot(ctx context.Context, id string, token int64) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: task id is required", ErrInvalid)
	}
	tx, err := s.begin(ctx, "release the provisioning slot of "+id)
	if err != nil {
		return err
	}
	defer tx.rollback()

	var phase, holder string
	var current, heldUntil int64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(json_extract(data, '$.Phase'), ''), slot_token, step_claim_owner, step_claim_until
		 FROM tasks WHERE id = ?`, id).Scan(&phase, &current, &holder, &heldUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: task %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("read the phase of task %s: %w", id, err)
	}
	switch {
	case ownsResources(phase):
		return fmt.Errorf("%w: task %s recorded a phase that owns its resources", ErrInvalid, id)
	case token == 0 || current != token:
		return fmt.Errorf("%w: task %s holds the slot claim %d, not the %d this caller acquired",
			ErrConflict, id, current, token)
	case holder != "" && heldUntil > time.Now().UnixMilli():
		return fmt.Errorf("%w: task %s is running its %s step for another owner, so its slot is not this caller's to release",
			ErrConflict, id, phase)
	}
	// The same conditions are repeated in the statement: the fence and the clear are
	// one write, so even a takeover between the read above and this update cannot be
	// erased by it. The token column stays as it is: it is the identity of the claim
	// that was given back, and a later claim takes a strictly greater one.
	res, err := tx.ExecContext(ctx,
		`UPDATE tasks SET slot_claimed_at = 0
		  WHERE id = ? AND slot_token = ?
		    AND COALESCE(json_extract(data, '$.Phase'), '') NOT IN ('worktree_created', 'agent_started', 'prompt_submitted')
		    AND NOT (step_claim_owner <> '' AND step_claim_until > ?)`,
		id, token, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("release the provisioning slot of %s: %w", id, err)
	}
	if err := requireOneRow(res, ErrConflict, "release the provisioning slot of "+id); err != nil {
		return err
	}
	return tx.commit("release the provisioning slot of " + id)
}

// ProvisioningSlots counts the slots one project currently holds, so a caller can
// describe why a task is queued.
func (s *Store) ProvisioningSlots(ctx context.Context, projectKey string) (int, error) {
	if strings.TrimSpace(projectKey) == "" {
		return 0, fmt.Errorf("%w: project key is required", ErrInvalid)
	}
	return claimedSlots(ctx, s.db, projectKey, "")
}

// reservedConflict finds the first pair of overlapping reservations between a
// request and the tasks of one project: the same path, or one path containing the
// other. Paths are compared as stored (repository-relative, forward slashes,
// case-folded on Windows), and the comparison follows the platform's rules.
func reservedConflict(existing []task.Task, requested []string) (owner, mine, theirs string, ok bool) {
	for _, other := range existing {
		if other.Disposition != "" || len(other.ReservedPaths) == 0 {
			continue
		}
		for _, want := range requested {
			for _, held := range other.ReservedPaths {
				if overlap, found := pathsOverlap(want, held); found {
					return other.ID, want, overlap, true
				}
			}
		}
	}
	return "", "", "", false
}

// pathsOverlap reports whether one reservation covers the other: the same path,
// or one is a directory containing the other.
func pathsOverlap(a, b string) (string, bool) {
	a, b = filepath.ToSlash(a), filepath.ToSlash(b)
	if runtime.GOOS == "windows" {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	switch {
	case a == b:
		return a, true
	case strings.HasPrefix(b, a+"/"):
		return a, true
	case strings.HasPrefix(a, b+"/"):
		return b, true
	default:
		return "", false
	}
}

// rowQueryer is the read surface shared by *sql.DB and one write transaction.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ---------------------------------------------------------------------------
// the durable step claim
// ---------------------------------------------------------------------------

// ClaimStep takes the exclusive right to run the provisioning step of one task at
// one phase, for owner, and reports whether *this call* got it.
//
// A slot claim says a task may hold worker capacity; it does not say who is
// running the task's next step. Two callers can both be inside a resume of the
// same task (a startup pass and a user's adjudication, or two startup passes), and
// the phase alone cannot tell them apart, so this claim makes the step exclusive
// durably:
//
//   - a claim is keyed by the phase it was taken for, so a step that advanced does
//     not block the next step;
//   - a claim that is older than lease can be taken over, which is what keeps a
//     crash recoverable: the process that died left the claim, and the retry —
//     minutes later, or from another process — is allowed to continue from the
//     recorded phase;
//   - a live claim for the same phase is never stolen, so only one caller runs the
//     step and only one side effect (a worktree create, an agent start, a brief) is
//     ever issued per step.
func (s *Store) ClaimStep(ctx context.Context, taskID string, phase task.Phase, owner string, lease time.Duration) (bool, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(owner) == "" {
		return false, fmt.Errorf("%w: task id and owner are required", ErrInvalid)
	}
	if strings.TrimSpace(string(phase)) == "" || lease <= 0 {
		return false, fmt.Errorf("%w: a phase and a positive lease are required", ErrInvalid)
	}
	tx, err := s.begin(ctx, "claim step of "+taskID)
	if err != nil {
		return false, err
	}
	defer tx.rollback()

	var holder, heldPhase string
	var heldUntil int64
	// The step is eligible only while the task still records this phase, so a
	// caller holding a stale read can never re-run a step the task has moved past
	// (the loser of a concurrent resume, for example).
	current, err := scanTask(tx.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, taskID), "task "+taskID)
	if err != nil {
		return false, err
	}
	if current.Phase != phase {
		return false, tx.commit("claim step of " + taskID)
	}
	err = tx.QueryRowContext(ctx,
		`SELECT step_claim_owner, step_claim_phase, step_claim_until FROM tasks WHERE id = ?`, taskID).
		Scan(&holder, &heldPhase, &heldUntil)
	if err != nil {
		return false, fmt.Errorf("read the step claim of %s: %w", taskID, err)
	}
	now := time.Now().UnixMilli()
	live := holder != "" && heldPhase == string(phase) && now < heldUntil
	if live {
		// Another caller is running this very step. Commit without changing anything
		// so the read did not hold a write lock longer than it had to.
		return false, tx.commit("claim step of " + taskID)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET step_claim_owner = ?, step_claim_phase = ?, step_claim_until = ? WHERE id = ?`,
		owner, string(phase), now+lease.Milliseconds(), taskID); err != nil {
		return false, fmt.Errorf("claim step of %s: %w", taskID, err)
	}
	if err := tx.commit("claim step of " + taskID); err != nil {
		return false, err
	}
	return true, nil
}

// RetireStepWithNote applies a compare-and-swap update to one task and retires the
// step claim that the update resolved, storing the note that records the decision —
// all in one transaction.
//
// The single transaction is the point, and it covers three things that are one fact.
// A step claim is what stops two callers from running the same step, so a decision
// that resolves a step (a user who inspected it and said what happened) must retire
// the claim at the same instant it makes the task eligible again: retiring the claim
// first would leave a decision with no effect, and retiring it afterwards would let a
// concurrent resume take the step in between and then have its live claim removed
// underneath it, submitting the brief twice. The note belongs to the same fact
// because it is the audit trail of the decision, so a decision that cannot be
// recorded must not be half stored — the caller sees the error and the task is
// exactly as decidable as it was.
//
// This retires any owner's claim, which only a user's decision may do; a caller that
// failed its own step uses RecordStepFailure, which releases only its own claim.
func (s *Store) RetireStepWithNote(
	ctx context.Context,
	id string,
	expectedRevision int64,
	mutate func(*task.Task) error,
	note task.Inbox,
) (task.Task, error) {
	if mutate == nil {
		return task.Task{}, fmt.Errorf("%w: nil mutation for task %q", ErrInvalid, id)
	}
	tx, err := s.begin(ctx, "decide task "+id)
	if err != nil {
		return task.Task{}, err
	}
	defer tx.rollback()

	next, err := updateTask(ctx, tx, id, expectedRevision, mutate)
	if err != nil {
		return task.Task{}, err
	}
	if _, err := clearStepClaim(ctx, tx, id, "", ""); err != nil {
		return task.Task{}, err
	}
	if _, err := addInbox(ctx, tx, note); err != nil {
		return task.Task{}, err
	}
	if err := tx.commit("decide task " + id); err != nil {
		return task.Task{}, err
	}
	return next, nil
}

// ErrStepNotHeld reports that a caller no longer holds the step it is trying to
// resolve, so its outcome describes state another caller owns. It is an ErrConflict:
// the caller's read is stale and the safe answer is to write nothing.
var ErrStepNotHeld = fmt.Errorf("%w: the step is not held by this caller", ErrConflict)

// RecordStepFailure records a failed step on its own task and releases this owner's
// step claim for that exact phase, in one transaction. It reports whether the claim
// was released.
//
// The fence for the whole write is the claim row: the step must not be in another
// caller's hands, or a late failure from a caller whose lease expired — a slow step, a
// paused process — would come back after another coordinator had taken the same step
// and overwrite the newer state with a failure that describes the one it ran. A step
// claimed by another owner for another phase is ErrStepNotHeld, which writes nothing
// and leaves the task and that claim exactly as they were; a step nobody holds is still
// this caller's to report on, so an undecided outcome keeps its blocker instead of
// going missing.
//
// Persisting the failure and releasing the claim cannot be separate writes: a claim
// released before the failure is stored lets a concurrent resume take the step and
// advance the task, after which this call would write a stale failure onto whatever
// phase the task reached. The caller's mutation owns the phase check (it refuses a
// task that moved on), and a mutation that refuses rolls the release back with it, so
// an uncertain failure leaves the claim held and the task untouched.
//
// release is false for an undecided outcome (a timeout, an ambiguity): the claim then
// stays until its lease expires, and the failure is recorded as a blocker that
// refuses automatic retries.
func (s *Store) RecordStepFailure(
	ctx context.Context,
	id string,
	expectedRevision int64,
	mutate func(*task.Task) error,
	owner string,
	phase task.Phase,
	release bool,
) (task.Task, bool, error) {
	if mutate == nil {
		return task.Task{}, false, fmt.Errorf("%w: nil mutation for task %q", ErrInvalid, id)
	}
	tx, err := s.begin(ctx, "record the failure of task "+id)
	if err != nil {
		return task.Task{}, false, err
	}
	defer tx.rollback()

	if err := requireOwnStep(ctx, tx, id, owner); err != nil {
		return task.Task{}, false, err
	}
	next, err := updateTask(ctx, tx, id, expectedRevision, mutate)
	if err != nil {
		return task.Task{}, false, err
	}
	released := false
	if release {
		released, err = clearStepClaim(ctx, tx, id, owner, phase)
		if err != nil {
			return task.Task{}, false, err
		}
	}
	if err := tx.commit("record the failure of task " + id); err != nil {
		return task.Task{}, false, err
	}
	return next, released, nil
}

// requireOwnStep reports whether this step is still the caller's to resolve: either
// the claim row is empty (nobody is running the step) or it names the caller. The read
// happens inside the same transaction as the write it guards, so a takeover cannot
// slip between the fence and the mutation.
//
// The claim's own phase is deliberately not compared: a step may record the next phase
// itself while it is still running (starting the worker records agent_started before
// the worker has been confirmed ready), so the claim is for the step, not for whatever
// phase the task currently stands on. What matters is whose hand the step is in.
//
// An expired lease is deliberately NOT a refusal on its own either. The hazard is
// another owner running the step — a late failure must not overwrite what that owner
// records — but a step nobody holds is still described by the phase this caller ran,
// and dropping an undecided outcome there would leave the task resumable with no
// blocker and nobody allowed to retry it safely. The release that follows is separate
// and stays lease- and phase-gated, so an expired claim is never freed from under a
// live holder.
func requireOwnStep(ctx context.Context, tx *tx, id, owner string) error {
	var holder, heldPhase string
	var heldUntil int64
	if err := tx.QueryRowContext(ctx,
		`SELECT step_claim_owner, step_claim_phase, step_claim_until FROM tasks WHERE id = ?`, id).
		Scan(&holder, &heldPhase, &heldUntil); err != nil {
		return fmt.Errorf("read the step claim of %s: %w", id, err)
	}
	if holder != "" && holder != owner {
		return fmt.Errorf("%w: task %s is claimed by %q for step %s until %d, not by %q",
			ErrStepNotHeld, id, holder, heldPhase, heldUntil, owner)
	}
	return nil
}

// clearStepClaim drops the step claim of one task through q. An empty owner clears
// whatever claim is there (a user's decision); otherwise only the claim that the
// owner and phase still match is cleared, so a caller can never take a step away
// from the caller that holds it.
func clearStepClaim(ctx context.Context, q execer, id, owner string, phase task.Phase) (bool, error) {
	query := `UPDATE tasks SET step_claim_owner = '', step_claim_phase = '', step_claim_until = 0 WHERE id = ?`
	args := []any{id}
	if owner != "" || phase != "" {
		query += ` AND step_claim_owner = ? AND step_claim_phase = ? AND step_claim_until > ?`
		args = append(args, owner, phase, time.Now().UnixMilli())
	}
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("clear the step claim of %s: %w", id, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("clear the step claim of %s: %w", id, err)
	}
	return rows == 1, nil
}

// ReleaseStep gives up this owner's step claim, for a step that failed without
// changing the task's phase: a retry must not have to wait out the lease. A claim
// another owner holds is left alone.
func (s *Store) ReleaseStep(ctx context.Context, taskID, owner string) error {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(owner) == "" {
		return fmt.Errorf("%w: task id and owner are required", ErrInvalid)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET step_claim_owner = '', step_claim_phase = '', step_claim_until = 0
		  WHERE id = ? AND step_claim_owner = ?`, taskID, owner)
	if err != nil {
		return fmt.Errorf("release the step claim of %s: %w", taskID, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release the step claim of %s: %w", taskID, err)
	}
	if rows == 0 {
		// Either the task is gone or another owner holds the claim: both are
		// reported, because neither is this caller's step to release.
		if _, err := s.Task(ctx, taskID); err != nil {
			return err
		}
		return fmt.Errorf("%w: task %s holds a step claim this owner did not take", ErrConflict, taskID)
	}
	return nil
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

// ByPane finds the single project or task that owns a pane, either by its
// launch-time id or by its current public id.
//
// The pairing is deliberate: a pane's launch-time id is what its process
// inherited, while the current public id is what Herdr's events and CLI calls
// name after a move, and a hook payload may carry either. A pane claimed by more
// than one record is ambiguous, and nothing may be derived from it: the caller
// refuses instead of picking one.
func (s *Store) ByPane(ctx context.Context, paneID string) (Binding, error) {
	pane := strings.TrimSpace(paneID)
	if pane == "" {
		return Binding{}, fmt.Errorf("%w: pane id is required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Binding{}, fmt.Errorf("begin read for pane %q: %w", pane, err)
	}
	defer func() { _ = tx.Rollback() }()

	workers, err := queryFacts[task.Task](ctx, tx,
		`SELECT data FROM tasks WHERE launch_pane_id = ? OR public_pane_id = ?`, pane, pane)
	if err != nil {
		return Binding{}, fmt.Errorf("read tasks for pane %q: %w", pane, err)
	}
	primaries, err := queryFacts[task.Project](ctx, tx,
		`SELECT data FROM projects
		  WHERE json_extract(data, '$.LaunchPaneID') = ? OR json_extract(data, '$.PublicPaneID') = ?`,
		pane, pane)
	if err != nil {
		return Binding{}, fmt.Errorf("read projects for pane %q: %w", pane, err)
	}
	var matches []Binding
	for _, worker := range workers {
		matches = append(matches, Binding{Role: RoleWorker, Task: worker})
	}
	for _, primary := range primaries {
		matches = append(matches, Binding{Role: RolePrimary, Project: primary})
	}
	switch len(matches) {
	case 0:
		return Binding{}, fmt.Errorf("%w: pane %q", ErrNotFound, pane)
	case 1:
		return matches[0], nil
	default:
		return Binding{}, fmt.Errorf("%w: pane %q is claimed by %d records", ErrAmbiguous, pane, len(matches))
	}
}

// Projects reads every bound primary project, ordered by key so a startup pass
// always walks them in the same order.
func (s *Store) Projects(ctx context.Context) ([]task.Project, error) {
	projects, err := queryFacts[task.Project](ctx, s.db, `SELECT data FROM projects ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("read the bound projects: %w", err)
	}
	return projects, nil
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
