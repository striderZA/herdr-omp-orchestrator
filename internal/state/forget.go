package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"herdr-omp-orchestrator/internal/task"
)

var ErrForgetBlocked = errors.New("state: the project has work that must settle before it can be forgotten")

type ForgetCounts struct{ Tasks, Tickets, Notes, ReadySessions int }

// ForgetFacts reads one project's durable facts without writing.
func (s *Store) ForgetFacts(ctx context.Context, key string) (task.ForgetFacts, error) {
	facts, err := readForgetFacts(ctx, s.db, key)
	if err != nil {
		return task.ForgetFacts{}, fmt.Errorf("read forget facts for %q: %w", key, err)
	}
	return facts, nil
}

// ForgetProject rechecks the planned facts and removes the project's records in
// one transaction. Herdr resources, worktrees and Git state are not touched.
func (s *Store) ForgetProject(ctx context.Context, key, fingerprint string) (ForgetCounts, error) {
	tx, err := s.begin(ctx, "forget project "+key)
	if err != nil {
		return ForgetCounts{}, err
	}
	defer tx.rollback()

	facts, err := readForgetFacts(ctx, tx, key)
	if err != nil {
		return ForgetCounts{}, fmt.Errorf("read forget facts for %q: %w", key, err)
	}
	currentFingerprint, err := task.ForgetFingerprint(facts)
	if err != nil {
		return ForgetCounts{}, fmt.Errorf("fingerprint forget facts for %q: %w", key, err)
	}
	if currentFingerprint != fingerprint {
		return ForgetCounts{}, fmt.Errorf("%w: facts for project %q changed since the forget plan", ErrConflict, key)
	}
	if blockers := task.ForgetBlockers(facts); len(blockers) != 0 {
		return ForgetCounts{}, fmt.Errorf("%w: %s", ErrForgetBlocked, strings.Join(blockers, "; "))
	}

	counts := ForgetCounts{
		Tasks:         len(facts.Tasks),
		Tickets:       len(facts.Tickets),
		Notes:         len(facts.Notes),
		ReadySessions: len(facts.ReadySessionPanes),
	}
	for _, pane := range facts.ReadySessionPanes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ready_sessions WHERE launch_pane_id = ?`, pane); err != nil {
			return ForgetCounts{}, fmt.Errorf("delete ready session for pane %q while forgetting %q: %w", pane, key, err)
		}
	}
	for _, deletion := range []struct {
		table string
		what  string
	}{
		{table: "inbox", what: "inbox notes"},
		{table: "tickets", what: "tickets"},
		{table: "tasks", what: "tasks"},
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+deletion.table+` WHERE project_key = ?`, key); err != nil {
			return ForgetCounts{}, fmt.Errorf("delete %s for project %q: %w", deletion.what, key, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE key = ?`, key)
	if err != nil {
		return ForgetCounts{}, fmt.Errorf("delete project %q: %w", key, err)
	}
	if err := requireOneRow(res, ErrNotFound, "forget project "+key); err != nil {
		return ForgetCounts{}, err
	}
	if err := tx.commit("forget project " + key); err != nil {
		return ForgetCounts{}, err
	}
	return counts, nil
}

type forgetFactQuerier interface {
	querier
	rowQueryer
}

func readForgetFacts(ctx context.Context, q forgetFactQuerier, key string) (task.ForgetFacts, error) {
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM projects WHERE key = ?`, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return task.ForgetFacts{}, fmt.Errorf("%w: project %q", ErrNotFound, key)
	}
	if err != nil {
		return task.ForgetFacts{}, fmt.Errorf("read project %q: %w", key, err)
	}
	var project task.Project
	if err := json.Unmarshal(data, &project); err != nil {
		return task.ForgetFacts{}, fmt.Errorf("decode project %q: %w", key, err)
	}

	tasks, err := queryFacts[task.Task](ctx, q,
		`SELECT data FROM tasks WHERE project_key = ? ORDER BY rowid, id`, key)
	if err != nil {
		return task.ForgetFacts{}, fmt.Errorf("read tasks for project %q: %w", key, err)
	}

	claimedRows, err := q.QueryContext(ctx,
		`SELECT id FROM tasks WHERE project_key = ? AND (slot_claimed_at > 0 OR step_claim_owner <> '') ORDER BY id`, key)
	if err != nil {
		return task.ForgetFacts{}, fmt.Errorf("read claimed tasks for project %q: %w", key, err)
	}
	claimedTaskIDs := make([]string, 0)
	for claimedRows.Next() {
		var id string
		if err := claimedRows.Scan(&id); err != nil {
			_ = claimedRows.Close()
			return task.ForgetFacts{}, fmt.Errorf("scan claimed task for project %q: %w", key, err)
		}
		claimedTaskIDs = append(claimedTaskIDs, id)
	}
	if err := claimedRows.Err(); err != nil {
		_ = claimedRows.Close()
		return task.ForgetFacts{}, fmt.Errorf("iterate claimed tasks for project %q: %w", key, err)
	}
	if err := claimedRows.Close(); err != nil {
		return task.ForgetFacts{}, fmt.Errorf("close claimed tasks for project %q: %w", key, err)
	}

	notes, err := inboxRows(ctx, q, "inbox notes for project "+fmt.Sprintf("%q", key),
		`SELECT `+inboxSelect+` FROM inbox WHERE project_key = ? ORDER BY rowid, id`, key)
	if err != nil {
		return task.ForgetFacts{}, err
	}
	tickets, err := queryFacts[task.Ticket](ctx, q,
		`SELECT data FROM tickets WHERE project_key = ? ORDER BY rowid, id`, key)
	if err != nil {
		return task.ForgetFacts{}, fmt.Errorf("read tickets for project %q: %w", key, err)
	}

	var intentCount int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM bind_intents WHERE key = ?`, key).Scan(&intentCount); err != nil {
		return task.ForgetFacts{}, fmt.Errorf("read bind intent for project %q: %w", key, err)
	}
	readySessionPanes, err := forgetReadySessionPanes(ctx, q, project, tasks)
	if err != nil {
		return task.ForgetFacts{}, fmt.Errorf("read ready sessions for project %q: %w", key, err)
	}

	return task.ForgetFacts{
		Project:           project,
		Tasks:             tasks,
		ClaimedTaskIDs:    claimedTaskIDs,
		ReadySessionPanes: readySessionPanes,
		Notes:             notes,
		Tickets:           tickets,
		BindIntentOpen:    intentCount > 0,
	}, nil
}

// forgetReadySessionPanes is shared by the read-only plan snapshot and the
// transactional recheck so both fingerprints include precisely the same rows.
func forgetReadySessionPanes(ctx context.Context, q rowQueryer, project task.Project, tasks []task.Task) ([]string, error) {
	candidates := make([]string, 0, len(tasks)+1)
	candidates = append(candidates, project.LaunchPaneID)
	for _, task := range tasks {
		candidates = append(candidates, task.LaunchPaneID)
	}

	seen := make(map[string]struct{}, len(candidates))
	panes := make([]string, 0, len(candidates))
	for _, pane := range candidates {
		if pane == "" {
			continue
		}
		if _, ok := seen[pane]; ok {
			continue
		}
		seen[pane] = struct{}{}
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM ready_sessions WHERE launch_pane_id = ?`, pane).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read ready session for pane %q: %w", pane, err)
		}
		panes = append(panes, pane)
	}
	sort.Strings(panes)
	return panes, nil
}
