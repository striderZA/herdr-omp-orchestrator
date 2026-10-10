package state_test

import (
	"context"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestRecoveryClaimsReportALiveOwnerUntilItIsFenced(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	const lease = 10 * time.Second
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t-recovery-owner", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	identity, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	if _, err := rawState(t, dir).Exec(`UPDATE tasks SET slot_claimed_at = ? WHERE id = ?`,
		time.Now().Add(-2*lease).UnixMilli(), stored.ID); err != nil {
		t.Fatalf("expire test slot claim: %v", err)
	}

	claims, err := s.RecoveryClaims(ctx, lease, stored.ID)
	if err != nil || len(claims) != 1 || !claims[0].Live {
		t.Fatalf("RecoveryClaims with a live owner = (%+v, %v), want one live claim", claims, err)
	}
	s.MarkTaskComplete(identity, stored.ID)
	claims, err = s.RecoveryClaims(ctx, lease, stored.ID)
	if err != nil || len(claims) != 1 || claims[0].Live || !claims[0].Due.Before(time.Now()) {
		t.Fatalf("RecoveryClaims after owner completion = (%+v, %v), want one expired claim with no live owner", claims, err)
	}
}

func TestRecoveryClaimsIgnoreBindIntents(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if _, acquired, created, err := s.ClaimBindIntent(ctx, "repo-a", "owner", time.Hour, nil, nil); err != nil || !acquired || !created {
		t.Fatalf("ClaimBindIntent = (acquired %v, created %v, err %v), want new intent", acquired, created, err)
	}

	if claims, err := s.RecoveryClaims(ctx, time.Minute); err != nil || len(claims) != 0 {
		t.Fatalf("RecoveryClaims with only a bind intent = (%+v, %v), want no task claims", claims, err)
	}
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	if found, err := s.HasRecoverableClaims(ctx, owner); err != nil || found {
		t.Fatalf("HasRecoverableClaims with only a bind intent = (%v, %v), want false", found, err)
	}
}

func TestHasRecoverableClaimsMatchesTaskSlotAndStepOwners(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t-slot-owner", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	if found, err := s.HasRecoverableClaims(ctx, owner); err != nil || !found {
		t.Fatalf("HasRecoverableClaims(slot owner) = (%v, %v), want true", found, err)
	}
	if taskIDs, err := s.RecoverableTaskIDs(ctx, owner); err != nil || len(taskIDs) != 1 || taskIDs[0] != stored.ID {
		t.Fatalf("RecoverableTaskIDs(slot owner) = (%v, %v), want only %s", taskIDs, err, stored.ID)
	}
	other := owner
	other.StartTime++
	if found, err := s.HasRecoverableClaims(ctx, other); err != nil || found {
		t.Fatalf("HasRecoverableClaims(other process identity) = (%v, %v), want false", found, err)
	}
	if _, err := s.UpdateTask(ctx, stored.ID, stored.Revision, func(current *task.Task) error {
		current.Phase = task.WorktreeCreated
		return nil
	}); err != nil {
		t.Fatalf("move slot-owned task to worktree_created: %v", err)
	}
	if found, err := s.HasRecoverableClaims(ctx, owner); err != nil || !found {
		t.Fatalf("HasRecoverableClaims(resource-phase slot owner) = (%v, %v), want true", found, err)
	}

	stepStore := open(t, t.TempDir())
	if _, err := stepStore.RecordTaskIntent(ctx, gateTask(t, "t-step-owner", "repo-b", nil)); err != nil {
		t.Fatalf("RecordTaskIntent: %v", err)
	}
	stepTask, err := stepStore.Task(ctx, "t-step-owner")
	if err != nil {
		t.Fatalf("Task(step owner): %v", err)
	}
	stepTask, err = stepStore.UpdateTask(ctx, stepTask.ID, stepTask.Revision, func(current *task.Task) error {
		current.Phase = task.WorktreeCreated
		return nil
	})
	if err != nil {
		t.Fatalf("move step-owned task to worktree_created: %v", err)
	}
	if took, err := stepStore.ClaimStep(ctx, "t-step-owner", task.WorktreeCreated, "step-owner", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the step", took, err)
	}
	if _, err := stepStore.UpdateTask(ctx, stepTask.ID, stepTask.Revision, func(current *task.Task) error {
		current.Phase = task.AgentStarted
		return nil
	}); err != nil {
		t.Fatalf("advance step-owned task to agent_started: %v", err)
	}
	if found, err := stepStore.HasRecoverableClaims(ctx, owner); err != nil || !found {
		t.Fatalf("HasRecoverableClaims(step owner) = (%v, %v), want true", found, err)
	}
}

func TestHasRecoverableClaimsExcludesBlockedAndDisposedTasks(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t-not-recoverable", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	blocked, err := s.UpdateTask(ctx, stored.ID, stored.Revision, func(current *task.Task) error {
		current.Blocker = "inspect before retry"
		return nil
	})
	if err != nil {
		t.Fatalf("set task blocker: %v", err)
	}
	if found, err := s.HasRecoverableClaims(ctx, owner); err != nil || found {
		t.Fatalf("HasRecoverableClaims(blocked task) = (%v, %v), want false", found, err)
	}
	if _, err := s.UpdateTask(ctx, blocked.ID, blocked.Revision, func(current *task.Task) error {
		current.Blocker = ""
		current.Disposition = "closed"
		return nil
	}); err != nil {
		t.Fatalf("set task disposition: %v", err)
	}
	if found, err := s.HasRecoverableClaims(ctx, owner); err != nil || found {
		t.Fatalf("HasRecoverableClaims(disposed task) = (%v, %v), want false", found, err)
	}
}

func TestRecoverableTaskIDsExcludeCompletedPromptSubmission(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t-submitted", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	if _, err := s.UpdateTask(ctx, stored.ID, stored.Revision, func(current *task.Task) error {
		current.Phase = task.PromptSubmitted
		return nil
	}); err != nil {
		t.Fatalf("move task to prompt_submitted: %v", err)
	}
	taskIDs, err := s.RecoverableTaskIDs(ctx, owner)
	if err != nil || len(taskIDs) != 0 {
		t.Fatalf("RecoverableTaskIDs(prompt_submitted) = (%v, %v), want no recovery work", taskIDs, err)
	}
}

func TestRecoveryClaimsScheduleAResourceTaskBetweenSteps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	const lease = 200 * time.Millisecond
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t-between-steps", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	if _, err := s.UpdateTask(ctx, stored.ID, stored.Revision, func(current *task.Task) error {
		current.Phase = task.WorktreeCreated
		return nil
	}); err != nil {
		t.Fatalf("advance task to worktree_created: %v", err)
	}
	if _, err := rawState(t, dir).Exec(`UPDATE tasks SET slot_claimed_at = ? WHERE id = ?`,
		time.Now().Add(-2*lease).UnixMilli(), stored.ID); err != nil {
		t.Fatalf("expire the slot claim: %v", err)
	}
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	s.MarkTaskComplete(owner, stored.ID)

	claims, err := s.RecoveryClaims(ctx, lease)
	if err != nil || len(claims) != 1 || claims[0].Live || claims[0].Due.After(time.Now()) {
		t.Fatalf("RecoveryClaims between steps = (%+v, %v), want one claim due now with no live owner", claims, err)
	}
}

func TestRecoverableTaskIDsCanBeScopedToOneTicket(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	first := gateTask(t, "t-ticket-a", "repo-a", nil)
	second := gateTask(t, "t-ticket-b", "repo-a", nil)
	for _, st := range []task.Task{first, second} {
		if claimed, _, _, err := s.ClaimTask(ctx, st, 2); err != nil || !claimed {
			t.Fatalf("ClaimTask(%s) = (%v, %v), want a slot", st.ID, claimed, err)
		}
	}
	raw := rawState(t, dir)
	if _, err := raw.Exec(`UPDATE tasks SET data = json_set(data, '$.TicketID', ?) WHERE id = ?`, "ticket-a", first.ID); err != nil {
		t.Fatalf("link first task to ticket-a: %v", err)
	}
	if _, err := raw.Exec(`UPDATE tasks SET data = json_set(data, '$.TicketID', ?) WHERE id = ?`, "ticket-b", second.ID); err != nil {
		t.Fatalf("link second task to ticket-b: %v", err)
	}
	taskIDs, err := s.RecoverableTaskIDs(ctx, owner, "ticket-a")
	if err != nil || len(taskIDs) != 1 || taskIDs[0] != first.ID {
		t.Fatalf("RecoverableTaskIDs(ticket-a) = (%v, %v), want only %s", taskIDs, err, first.ID)
	}
}
