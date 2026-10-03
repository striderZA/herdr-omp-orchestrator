package state

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/task"
)

// The kinds of transition the history records. Each is derived by comparing the
// facts before and after one revision, so every writer that goes through the
// store's task update paths is covered without each caller naming its own event.
const (
	HistoryCreated     = "created"
	HistoryStage       = "stage"
	HistoryPhase       = "phase"
	HistoryReport      = "report"
	HistoryBlocker     = "blocker"
	HistoryAttention   = "attention"
	HistoryDisposition = "disposition"
	HistoryOutcome     = "outcome"
	HistoryPublicPane  = "public_pane"
)

// historySchema is the append-only transition log. It is separate from the
// current facts: a task row says where a task is, this says how it got there.
// Triggers refuse UPDATE and DELETE so a row, once written, is never rewritten.
// Rows are small, one per changed fact, and are kept for the life of the state
// database: nothing else reads them for correctness, so there is no retention to
// get wrong.
const historySchema = `
CREATE TABLE IF NOT EXISTS task_history (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	project_key TEXT NOT NULL,
	revision INTEGER NOT NULL,
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	from_value TEXT NOT NULL DEFAULT '',
	to_value TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS task_history_task ON task_history (task_id, seq);
CREATE TRIGGER IF NOT EXISTS task_history_no_update BEFORE UPDATE ON task_history
BEGIN SELECT RAISE(ABORT, 'task_history is append-only'); END;
CREATE TRIGGER IF NOT EXISTS task_history_no_delete BEFORE DELETE ON task_history
BEGIN SELECT RAISE(ABORT, 'task_history is append-only'); END;
`

// Transition is one recorded change of a task fact. From is empty for a
// creation or when the fact was previously unset.
type Transition struct {
	Seq        int64
	TaskID     string
	ProjectKey string
	Revision   int64
	At         int64
	Kind       string
	From, To   string
}

type historyEntry struct{ kind, from, to string }

func reportLabel(r *task.Report) string {
	if r == nil {
		return ""
	}
	return r.Kind
}

// diffTransitions lists the recorded-fact changes between two revisions. Free
// text (blocker, attention, disposition) is stored as written; the stage and
// phase are the closed vocabularies of the model.
func diffTransitions(prev, next task.Task) []historyEntry {
	var out []historyEntry
	add := func(kind, from, to string) {
		if from != to {
			out = append(out, historyEntry{kind, from, to})
		}
	}
	add(HistoryStage, string(task.StageOf(prev)), string(task.StageOf(next)))
	add(HistoryPhase, string(prev.Phase), string(next.Phase))
	if !reflect.DeepEqual(prev.Report, next.Report) {
		out = append(out, historyEntry{HistoryReport, reportLabel(prev.Report), reportLabel(next.Report)})
	}
	add(HistoryBlocker, prev.Blocker, next.Blocker)
	add(HistoryAttention, prev.Attention, next.Attention)
	add(HistoryOutcome, prev.Outcome, next.Outcome)
	add(HistoryDisposition, prev.Disposition, next.Disposition)
	add(HistoryPublicPane, prev.PublicPaneID, next.PublicPaneID)
	return out
}

// appendHistory writes rows inside the caller's transaction so a transition is
// durable exactly when the revision that caused it is.
func appendHistory(ctx context.Context, tx *tx, t task.Task, entries []historyEntry) error {
	at := time.Now().UnixMilli()
	for _, e := range entries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_history (task_id, project_key, revision, at, kind, from_value, to_value)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.ProjectKey, t.Revision, at, e.kind, e.from, e.to); err != nil {
			return fmt.Errorf("record history of task %q: %w", t.ID, err)
		}
	}
	return nil
}

// recordCreated logs a new task and the stage it starts in.
func recordCreated(ctx context.Context, tx *tx, t task.Task) error {
	return appendHistory(ctx, tx, t, []historyEntry{
		{HistoryCreated, "", string(task.StageOf(t))},
	})
}

// History reads one task's transitions in the order they were recorded.
func (s *Store) History(ctx context.Context, taskID string) ([]Transition, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, fmt.Errorf("%w: task id is required", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, task_id, project_key, revision, at, kind, from_value, to_value
		 FROM task_history WHERE task_id = ? ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("read history of %q: %w", taskID, err)
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var x Transition
		if err := rows.Scan(&x.Seq, &x.TaskID, &x.ProjectKey, &x.Revision, &x.At, &x.Kind, &x.From, &x.To); err != nil {
			return nil, fmt.Errorf("read history of %q: %w", taskID, err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read history of %q: %w", taskID, err)
	}
	return out, nil
}
