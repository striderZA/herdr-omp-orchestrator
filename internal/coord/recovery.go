package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// recoveryOwnerPollInterval bounds how often a worker re-reads a claim a live owner holds.
const recoveryOwnerPollInterval = time.Second

// ErrRecoveryStalled reports that a recovery worker stopped because every claim left in
// scope has used its declines and none is live. Those claims stay recorded and keep their
// slots until startup, or a drain of a queued task, continues them. A decline, a checkout
// problem and a refused start or brief are recorded as the task's attention (a terminal
// refusal records a blocker instead). A failure to read Herdr or the store is only returned.
var ErrRecoveryStalled = errors.New("provisioning recovery stalled")

// recoveryStall is the stop once no claim is left that a worker can attempt or await.
// passes is the most declines any remaining claim took; last is the failure of the last
// pass, if it had one.
type recoveryStall struct {
	passes int
	last   error
}

func (e *recoveryStall) Error() string {
	if e.last != nil {
		return fmt.Sprintf("%v after %d declines: %v", ErrRecoveryStalled, e.passes, e.last)
	}
	return fmt.Sprintf("%v after %d declines: no remaining claim could be advanced", ErrRecoveryStalled, e.passes)
}

// Unwrap lets errors.Is match ErrRecoveryStalled and any failure the last pass returned.
func (e *recoveryStall) Unwrap() []error {
	if e.last != nil {
		return []error{ErrRecoveryStalled, e.last}
	}
	return []error{ErrRecoveryStalled}
}

// recoveryPace bounds how often a claim that a pass declined is attempted again. The
// wait after a claim's n-th consecutive decline is first, doubled n-1 times and capped
// at max. After limit declines the claim is exhausted: it stays recorded, with its slot, and
// this worker no longer attempts it.
type recoveryPace struct {
	first time.Duration
	max   time.Duration
	limit int
}

// defaultRecoveryPace is the production pace: waits of 1s, 2s, 4s, 8s, 16s and 30s
// between seven declines, so a claim that no pass can prove is exhausted in about a
// minute.
var defaultRecoveryPace = recoveryPace{first: time.Second, max: 30 * time.Second, limit: 7}

// backoff is the wait after the n-th consecutive decline, counting from 1.
func (p recoveryPace) backoff(n int) time.Duration {
	wait := p.first
	for i := 1; i < n && wait < p.max; i++ {
		wait *= 2
	}
	if wait > p.max {
		wait = p.max
	}
	return wait
}

// claimPacing is the retry state of one due claim that a pass declined.
type claimPacing struct {
	declines int
	retryAt  time.Time
}

// recoveryDeclined is the attention recorded on a provisioning step that a pass
// declined to continue: no pane proves it is the task's worker, and no empty pane of
// the task remains, so nothing was started or submitted.
func recoveryDeclined(taskID string) string {
	return fmt.Sprintf("task %s: recovery left the recorded step unresumed: no pane proves it is this task's worker "+
		"and no empty pane of the task remains, so nothing was started or submitted", taskID)
}

// recoveryNoInboxFlushKey marks notes written during a background pass as
// pending-only; startup and ordinary event paths own inbox delivery.
type recoveryNoInboxFlushKey struct{}

// RecoverUntilIdle runs identity-checked recovery of provisioning claims until none is
// left that it can attempt or await. An empty scope is every task, the scope a startup
// route's worker takes; a non-empty one is the claims of one completed operation.
//
// A pass judges only claims that are due and held by no live or unverifiable owner
// (state.RecoveryClaims). It never reconciles a task whose process still runs, an
// unrelated cleanup, an inbox delivery, or a task with no claim. Startup's Reconcile is
// the only whole-project pass.
//
// Each due claim is paced on its own. A claim the pass declined waits its own backoff
// before the next attempt, so a stuck claim never holds back another claim that comes
// due later. A claim that has used its declines is exhausted. The worker stops when no
// claim remains that it can attempt or await, with an error matching ErrRecoveryStalled.
// Waiting for a live owner is not a stall.
func (c *Coordinator) RecoverUntilIdle(ctx context.Context, taskIDs ...string) error {
	ctx = context.WithValue(ctx, recoveryNoInboxFlushKey{}, true)
	pacing := map[string]*claimPacing{}
	var lastErr error
	for {
		claims, err := c.store.RecoveryClaims(ctx, c.opts.StepLease, taskIDs...)
		if err != nil {
			return fmt.Errorf("find the next provisioning recovery: %w", err)
		}
		forgetClaims(pacing, claims)
		now := time.Now()
		var attempt []string
		var wake time.Time
		pending := false
		exhausted := 0
		for _, claim := range claims {
			if claim.Live {
				pending = true
				wake = soonerOf(wake, now.Add(recoveryOwnerPollInterval))
				continue
			}
			if claim.Due.IsZero() {
				continue
			}
			pending = true
			ready := claim.Due
			if p := pacing[claim.ID]; p != nil {
				if p.declines >= c.pace.limit {
					exhausted = max(exhausted, p.declines)
					continue
				}
				ready = laterOf(ready, p.retryAt)
			}
			if ready.After(now) {
				wake = soonerOf(wake, ready)
				continue
			}
			attempt = append(attempt, claim.ID)
		}
		if !pending {
			return lastErr
		}
		if len(attempt) == 0 {
			if wake.IsZero() {
				return &recoveryStall{passes: exhausted, last: lastErr}
			}
			if err := sleepRecovery(ctx, time.Until(wake)); err != nil {
				return err
			}
			continue
		}
		lastErr = c.reconcileRecoveryTasks(ctx, attempt)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.notePacing(ctx, pacing, attempt); err != nil {
			return err
		}
	}
}

// notePacing records how an attempted pass left each claim. The pass has returned, so no
// step of this process still runs: a claim this process still holds for an attempted task
// is abandoned. A failed step keeps the capacity it took under the identity that took it,
// and a live owner is never taken over, so the worker would wait on its own claim forever.
// Fencing it makes the claim due at its lease end. Every later acquisition by this process
// retires that fence (markTaskActive), so it is applied again after each attempt. A claim no
// longer due is resolved; one still due was declined and waits its backoff before the next attempt.
func (c *Coordinator) notePacing(ctx context.Context, pacing map[string]*claimPacing, attempted []string) error {
	self, err := state.CurrentProcessIdentity()
	if err != nil {
		return fmt.Errorf("read the recovery worker's identity: %w", err)
	}
	for _, id := range attempted {
		c.store.MarkTaskComplete(self, id)
	}
	after, err := c.store.RecoveryClaims(ctx, c.opts.StepLease, attempted...)
	if err != nil {
		return fmt.Errorf("re-read the attempted provisioning claims: %w", err)
	}
	stillDue := map[string]bool{}
	for _, claim := range after {
		if !claim.Live && !claim.Due.IsZero() {
			stillDue[claim.ID] = true
		}
	}
	done := time.Now()
	for _, id := range attempted {
		if !stillDue[id] {
			delete(pacing, id)
			continue
		}
		p := pacing[id]
		if p == nil {
			p = &claimPacing{}
			pacing[id] = p
		}
		p.declines++
		p.retryAt = done.Add(c.pace.backoff(p.declines))
	}
	return nil
}

// forgetClaims drops the pacing of any claim that has left the scope.
func forgetClaims(pacing map[string]*claimPacing, claims []state.RecoveryClaim) {
	present := make(map[string]bool, len(claims))
	for _, claim := range claims {
		present[claim.ID] = true
	}
	for id := range pacing {
		if !present[id] {
			delete(pacing, id)
		}
	}
}

// soonerOf returns the earlier of two instants, where the zero time means none.
func soonerOf(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// laterOf returns the later of two instants.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// reconcileRecoveryTasks runs one pass over the given claims. Each task is judged on
// its own: a refusal or failure is collected and the pass goes on, so one stuck claim
// cannot starve the others in its scope.
func (c *Coordinator) reconcileRecoveryTasks(ctx context.Context, taskIDs []string) error {
	var snapshot *snapshotView
	var failures []error
	for _, taskID := range taskIDs {
		st, err := c.store.Task(ctx, taskID)
		if err != nil {
			failures = append(failures, fmt.Errorf("read task %s for recovery: %w", taskID, err))
			continue
		}
		if err := c.recoverTask(ctx, st, &snapshot); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// recoverTask judges one due claim and continues it only where Herdr proves the step
// may continue. An intent is drained through the queue. A recorded step that no pane
// proves is declined: nothing is started, and the decline is recorded as the task's
// attention so a person can decide. The claim stays recorded and due.
func (c *Coordinator) recoverTask(ctx context.Context, st task.Task, snapshot **snapshotView) error {
	if st.Blocker != "" || st.Disposition != "" {
		return nil
	}
	if st.Phase == task.Intent {
		if _, err := c.drainQueuedTask(ctx, st.ProjectKey, st.ID); err != nil {
			return fmt.Errorf("drain recovered task %s: %w", st.ID, err)
		}
		return nil
	}
	if !resumableStep(st) {
		return nil
	}
	project, err := c.store.Project(ctx, st.ProjectKey)
	if err != nil {
		return fmt.Errorf("read project %s for task recovery: %w", st.ProjectKey, err)
	}
	if *snapshot == nil {
		view, err := c.snapshotView(ctx)
		if err != nil {
			return fmt.Errorf("read Herdr identity snapshot for task recovery: %w", err)
		}
		*snapshot = view
	}
	listed, err := c.worktreeList(ctx, project.Key)
	if err != nil {
		return fmt.Errorf("list worktrees for task %s recovery: %w", st.ID, err)
	}
	decision, err := c.reconcileTask(ctx, project, st, *snapshot, listed)
	if err != nil {
		return fmt.Errorf("verify task %s before recovery: %w", st.ID, err)
	}
	if !decision.Continue {
		return c.taskAttention(ctx, st, recoveryDeclined(st.ID))
	}
	current, err := c.store.Task(ctx, st.ID)
	if err != nil {
		return fmt.Errorf("re-read task %s before recovery: %w", st.ID, err)
	}
	if !resumableStep(current) {
		return nil
	}
	// A resumed step keeps its slot: a running worker holds capacity until a review
	// disposition frees it, so recovery does not walk the queue here. Queued work is
	// admitted by the operation that frees capacity, or by startup.
	if _, err := c.resumeDispatch(ctx, current.ID, false); err != nil && !errors.Is(err, ErrStepInFlight) {
		return fmt.Errorf("resume task %s from its recorded phase: %w", current.ID, err)
	}
	return nil
}

// sleepRecovery waits for d, or returns the context's error if it ends first.
func sleepRecovery(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
