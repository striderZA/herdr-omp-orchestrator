package state

import (
	"context"
	"fmt"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/task"
)

// RecoveryClaim is one task's provisioning claims as recovery reads them.
type RecoveryClaim struct {
	ID string
	// Due is when the claims may be retried: the later of their lease ends. It is the
	// zero time when no claim applies to the task's phase.
	Due time.Time
	// Live reports that a live or unverifiable owner holds one of the claims, and then
	// they are never taken over. A claim with no owner identity (a legacy row) is not
	// live: it is due when its lease ends, like a dead owner's claim.
	Live bool
}

// RecoveryClaims classifies the task claims in scope, limited to taskIDs when given.
// Recovery schedules and judges claims from this one classification, so the rules
// for a live owner, a dead owner and a legacy row live in a single place.
// Bind intents use their separate explicit proof path and are not reported.
func (s *Store) RecoveryClaims(ctx context.Context, stepLease time.Duration, taskIDs ...string) ([]RecoveryClaim, error) {
	if stepLease <= 0 {
		return nil, fmt.Errorf("%w: a positive step lease is required", ErrInvalid)
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
	query += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read task claims for recovery: %w", err)
	}
	var claims []RecoveryClaim
	for rows.Next() {
		var id, phase, blocker, disposition, owner, stepPhase string
		var claimedAt, slotOwnerPID, slotOwnerStart, stepUntil, stepOwnerPID, stepOwnerStart int64
		if err := rows.Scan(
			&id, &phase, &blocker, &disposition,
			&claimedAt, &slotOwnerPID, &slotOwnerStart,
			&owner, &stepPhase, &stepUntil, &stepOwnerPID, &stepOwnerStart,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read task claim for recovery: %w", err)
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
			live := s.taskOwnerAlive(slotOwnerPID, slotOwnerStart, id)
			// SlotClaimAbandoned also excludes any live step claim on an intent, whatever
			// its phase: a live step owner keeps the intent's claim live, and a dead one
			// leaves the claim to its lease.
			if strings.TrimSpace(owner) != "" {
				if s.taskOwnerAlive(stepOwnerPID, stepOwnerStart, id) {
					live = true
				} else if stepUntil > due {
					due = stepUntil
				}
			}
			claims = append(claims, RecoveryClaim{ID: id, Due: millisTime(due), Live: live})
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
			live := s.taskOwnerAlive(slotOwnerPID, slotOwnerStart, id) ||
				s.taskOwnerAlive(stepOwnerPID, stepOwnerStart, id)
			claims = append(claims, RecoveryClaim{ID: id, Due: millisTime(due), Live: live})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read task claim for recovery: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close task claims for recovery: %w", err)
	}
	return claims, nil
}

// millisTime converts a stored Unix-millisecond instant; zero means none.
func millisTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
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
