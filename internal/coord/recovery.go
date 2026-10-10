package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

const recoveryRetryDelay = time.Second

// RecoverUntilIdle runs identity-checked task recovery without inbox delivery.
// When taskIDs is supplied, it recovers only those claims from one completed
// operation; an empty scope is reserved for startup-wide reconciliation.
func (c *Coordinator) RecoverUntilIdle(ctx context.Context, taskIDs ...string) error {
	for {
		delay, pending, err := c.store.NextRecoveryDelay(ctx, c.opts.StepLease, taskIDs...)
		if err != nil {
			return fmt.Errorf("find the next provisioning recovery: %w", err)
		}
		if !pending {
			return nil
		}
		if delay <= 0 {
			if len(taskIDs) == 0 {
				err = c.reconcile(ctx, false)
			} else {
				err = c.reconcileRecoveryTasks(ctx, taskIDs)
			}
			if err != nil {
				return fmt.Errorf("recover provisioning claims: %w", err)
			}
			delay, pending, err = c.store.NextRecoveryDelay(ctx, c.opts.StepLease, taskIDs...)
			if err != nil {
				return fmt.Errorf("find the next provisioning recovery: %w", err)
			}
			if !pending {
				return nil
			}
			if delay <= 0 {
				delay = recoveryRetryDelay
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Coordinator) reconcileRecoveryTasks(ctx context.Context, taskIDs []string) error {
	var snapshot *snapshotView
	for _, taskID := range taskIDs {
		st, err := c.store.Task(ctx, taskID)
		if err != nil {
			return fmt.Errorf("read task %s for recovery: %w", taskID, err)
		}
		if st.Blocker != "" || st.Disposition != "" {
			continue
		}
		if st.Phase == task.Intent {
			if _, err := c.drainQueuedTask(ctx, st.ProjectKey, st.ID); err != nil {
				return fmt.Errorf("drain recovered task %s: %w", st.ID, err)
			}
			continue
		}
		if !resumableStep(st) {
			continue
		}
		project, err := c.store.Project(ctx, st.ProjectKey)
		if err != nil {
			return fmt.Errorf("read project %s for task recovery: %w", st.ProjectKey, err)
		}
		if snapshot == nil {
			snapshot, err = c.snapshotView(ctx)
			if err != nil {
				return fmt.Errorf("read Herdr identity snapshot for task recovery: %w", err)
			}
		}
		listed, err := c.worktreeList(ctx, project.Key)
		if err != nil {
			return fmt.Errorf("list worktrees for task %s recovery: %w", st.ID, err)
		}
		decision, err := c.reconcileTask(ctx, project, st, snapshot, listed)
		if err != nil {
			return fmt.Errorf("verify task %s before recovery: %w", st.ID, err)
		}
		if !decision.Continue {
			continue
		}
		current, err := c.store.Task(ctx, st.ID)
		if err != nil {
			return fmt.Errorf("re-read task %s before recovery: %w", st.ID, err)
		}
		if !resumableStep(current) {
			continue
		}
		if _, err := c.resumeDispatch(ctx, current.ID, false); err != nil && !errors.Is(err, ErrStepInFlight) {
			return fmt.Errorf("resume task %s from its recorded phase: %w", current.ID, err)
		}
	}
	return nil
}

// CompleteRecoveryClaims fences only the task claims owned by a completed
// operation. A board process can remain alive and start later ticket moves, so
// the optional ticketID narrows its completion fence to that planner task.
func (c *Coordinator) CompleteRecoveryClaims(ctx context.Context, owner state.ProcessIdentity, ticketID string) ([]string, error) {
	taskIDs, err := c.store.RecoverableTaskIDs(ctx, owner, ticketID)
	if err != nil {
		return nil, fmt.Errorf("read claims owned by process %s: %w", owner, err)
	}
	for _, taskID := range taskIDs {
		c.store.MarkTaskComplete(owner, taskID)
	}
	return taskIDs, nil
}
