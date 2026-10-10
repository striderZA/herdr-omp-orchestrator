package state

import (
	"context"
	"fmt"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/task"
)

const recoveryOwnerPollInterval = time.Second

// NextRecoveryDelay reports how long until the earliest recoverable task claim
// may be retried. Live task owners are polled at a bounded interval so route
// completion or process exit can make their claims recoverable without waiting
// for a stale lease. Bind intents use their separate explicit proof path.
func (s *Store) NextRecoveryDelay(ctx context.Context, stepLease time.Duration, taskIDs ...string) (time.Duration, bool, error) {
	if stepLease <= 0 {
		return 0, false, fmt.Errorf("%w: a positive step lease is required", ErrInvalid)
	}
	now := time.Now().UnixMilli()
	var earliest int64
	consider := func(due int64) {
		if due > 0 && (earliest == 0 || due < earliest) {
			earliest = due
		}
	}
	pollLiveOwner := func() {
		consider(now + recoveryOwnerPollInterval.Milliseconds())
	}

	query := `SELECT id, COALESCE(json_extract(data, '$.Phase'), ''),
		        COALESCE(json_extract(data, '$.Blocker'), ''),
		        COALESCE(json_extract(data, '$.Disposition'), ''),
		        slot_claimed_at, slot_owner_pid, slot_owner_start,
		        step_claim_owner, step_claim_phase, step_claim_until, step_claim_pid, step_claim_start
		 FROM tasks WHERE (slot_claimed_at > 0 OR step_claim_owner != '')`
	args := make([]any, 0, len(taskIDs))
	if len(taskIDs) > 0 {
		placeholders := make([]string, len(taskIDs))
		for i, id := range taskIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += ` AND id IN (` + strings.Join(placeholders, ",") + `)`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, false, fmt.Errorf("read task claims for recovery: %w", err)
	}
	for rows.Next() {
		var id, phase, blocker, disposition, owner, stepPhase string
		var claimedAt, slotOwnerPID, slotOwnerStart, stepUntil, stepOwnerPID, stepOwnerStart int64
		if err := rows.Scan(
			&id, &phase, &blocker, &disposition,
			&claimedAt, &slotOwnerPID, &slotOwnerStart,
			&owner, &stepPhase, &stepUntil, &stepOwnerPID, &stepOwnerStart,
		); err != nil {
			_ = rows.Close()
			return 0, false, fmt.Errorf("read task claim for recovery: %w", err)
		}
		if strings.TrimSpace(blocker) != "" || strings.TrimSpace(disposition) != "" {
			continue
		}
		switch task.Phase(phase) {
		case task.Intent:
			if claimedAt <= 0 {
				continue
			}
			due := claimedAt + stepLease.Milliseconds()
			ownerAlive := s.taskOwnerAlive(slotOwnerPID, slotOwnerStart, id)
			// SlotClaimAbandoned also excludes any live step claim on an intent,
			// regardless of its phase. A dead step owner still retains the
			// existing lease fence; a live or unknown owner is polled instead.
			if strings.TrimSpace(owner) != "" {
				if s.taskOwnerAlive(stepOwnerPID, stepOwnerStart, id) {
					ownerAlive = true
				} else if stepUntil > due {
					due = stepUntil
				}
			}
			if ownerAlive {
				pollLiveOwner()
			} else {
				consider(due)
			}
		case task.WorktreeCreated, task.AgentStarted:
			if claimedAt <= 0 && strings.TrimSpace(owner) == "" {
				continue
			}
			due := int64(0)
			if claimedAt > 0 {
				due = claimedAt + stepLease.Milliseconds()
			}
			if stepUntil > due {
				due = stepUntil
			}
			if s.taskOwnerAlive(slotOwnerPID, slotOwnerStart, id) ||
				s.taskOwnerAlive(stepOwnerPID, stepOwnerStart, id) {
				pollLiveOwner()
			} else {
				consider(due)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, false, fmt.Errorf("read task claim for recovery: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, false, fmt.Errorf("close task claims for recovery: %w", err)
	}

	if earliest == 0 {
		return 0, false, nil
	}
	if earliest <= now {
		return 0, true, nil
	}
	return time.Duration(earliest-now) * time.Millisecond, true, nil
}

// RecoverableTaskIDs reports task claims owned by owner that task recovery may
// inspect. A non-empty ticketID narrows a long-lived board's completion fence to
// the planner task for that one move. Bind intents, blocked work, and tasks with
// recorded dispositions are excluded.
func (s *Store) RecoverableTaskIDs(ctx context.Context, owner ProcessIdentity, ticketIDs ...string) ([]string, error) {
	if !owner.valid() {
		return nil, fmt.Errorf("%w: a valid owner process identity is required", ErrInvalid)
	}
	if len(ticketIDs) > 1 {
		return nil, fmt.Errorf("%w: at most one ticket id is allowed", ErrInvalid)
	}
	ticketID := ""
	if len(ticketIDs) == 1 {
		ticketID = strings.TrimSpace(ticketIDs[0])
	}
	query := `SELECT id, COALESCE(json_extract(data, '$.Blocker'), ''),
		         COALESCE(json_extract(data, '$.Disposition'), '')
		 FROM tasks
		  WHERE (
		    (slot_owner_pid = ? AND slot_owner_start = ?
		     AND ((COALESCE(json_extract(data, '$.Phase'), '') = 'intent' AND slot_claimed_at > 0)
		          OR COALESCE(json_extract(data, '$.Phase'), '') IN ('worktree_created', 'agent_started')))
		    OR
		    (step_claim_owner <> '' AND step_claim_pid = ? AND step_claim_start = ?
		     AND step_claim_phase IN ('worktree_created', 'agent_started'))
		  )`
	args := []any{int64(owner.PID), owner.StartTime, int64(owner.PID), owner.StartTime}
	if strings.TrimSpace(ticketID) != "" {
		query += ` AND COALESCE(json_extract(data, '$.TicketID'), '') = ?`
		args = append(args, strings.TrimSpace(ticketID))
	}
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read claims owned by process %s: %w", owner, err)
	}
	var taskIDs []string
	for rows.Next() {
		var id, blocker, disposition string
		if err := rows.Scan(&id, &blocker, &disposition); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read claim owned by process %s: %w", owner, err)
		}
		if strings.TrimSpace(blocker) == "" && strings.TrimSpace(disposition) == "" {
			taskIDs = append(taskIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read claims owned by process %s: %w", owner, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close claims owned by process %s: %w", owner, err)
	}
	return taskIDs, nil
}

// HasRecoverableClaims is the boolean view retained for callers that need only
// to know whether this process owns any unfinished task claims.
func (s *Store) HasRecoverableClaims(ctx context.Context, owner ProcessIdentity) (bool, error) {
	taskIDs, err := s.RecoverableTaskIDs(ctx, owner)
	return len(taskIDs) > 0, err
}
