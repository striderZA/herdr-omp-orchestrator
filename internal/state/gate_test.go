package state_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// This file covers the durable dispatch gate: the claim of a project's
// provisioning slots and of reserved paths, decided inside one write transaction
// so two coordinator processes cannot both pass the same check.

func gateTask(t *testing.T, id, projectKey string, reserved []string) task.Task {
	t.Helper()
	return task.Task{
		ID:            id,
		ProjectKey:    projectKey,
		Title:         "gate " + id,
		Brief:         "brief",
		Checks:        []string{"go test ./... ok"},
		BaseRevision:  "0123456",
		Branch:        "orch/" + id,
		ReservedPaths: reserved,
		Phase:         task.Intent,
	}
}

func TestStoreClaimTaskRespectsTheBudgetAndReservations(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	// Two slots: the first two claims succeed, the third is recorded without one.
	for _, id := range []string{"t1", "t2"} {
		claimed, stored, err := s.ClaimTask(ctx, gateTask(t, id, "repo-a", nil), 2)
		if err != nil {
			t.Fatalf("ClaimTask(%s): %v", id, err)
		}
		if !claimed {
			t.Fatalf("ClaimTask(%s) claimed = false, want a slot", id)
		}
		if stored.Revision != 1 || stored.Phase != task.Intent {
			t.Errorf("stored task = %+v, want a fresh intent record", stored)
		}
	}
	claimed, stored, err := s.ClaimTask(ctx, gateTask(t, "t3", "repo-a", nil), 2)
	if err != nil {
		t.Fatalf("ClaimTask(t3): %v", err)
	}
	if claimed {
		t.Error("the third claim took a slot although the budget is 2")
	}
	if stored.ID != "t3" {
		t.Errorf("a refused slot did not record the task: %+v", stored)
	}
	slots, err := s.ProvisioningSlots(ctx, "repo-a")
	if err != nil || slots != 2 {
		t.Errorf("ProvisioningSlots = (%d, %v), want (2, nil)", slots, err)
	}

	// A reservation another task holds refuses the whole request, and records it.
	if _, _, err := s.ClaimTask(ctx, gateTask(t, "t4", "repo-a", []string{"notes.md"}), 2); err != nil {
		t.Fatalf("ClaimTask(t4): %v", err)
	}
	_, _, err = s.ClaimTask(ctx, gateTask(t, "t5", "repo-a", []string{"NOTES.md"}), 2)
	if !errors.Is(err, state.ErrReservedConflict) {
		t.Fatalf("ClaimTask(t5) err = %v, want ErrReservedConflict", err)
	}
	if !contains(err.Error(), "t4") || !contains(err.Error(), "notes.md") {
		t.Errorf("refusal %q does not name the owner and the path", err)
	}
	if _, err := s.Task(ctx, "t5"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a refused claim recorded a task: %v", err)
	}
	// The reservation rule covers a directory containing the requested file.
	if _, _, err := s.ClaimTask(ctx, gateTask(t, "t6", "repo-a", []string{"notes.md/inner.txt"}), 2); !errors.Is(err, state.ErrReservedConflict) {
		t.Errorf("ClaimTask(t6) err = %v, want ErrReservedConflict for a path inside the reservation", err)
	}
	// A closed task no longer holds its reservation.
	if _, err := s.UpdateTask(ctx, "t4", 1, func(next *task.Task) error {
		next.Disposition = "integrated"
		return nil
	}); err != nil {
		t.Fatalf("close t4: %v", err)
	}
	if _, _, err := s.ClaimTask(ctx, gateTask(t, "t7", "repo-a", []string{"notes.md"}), 2); err != nil {
		t.Errorf("a closed task still holds its reservation: %v", err)
	}
}

func TestStoreProvisioningSlotClaimIsIdempotentAndReleasable(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	st := gateTask(t, "t1", "repo-a", nil)
	claimed, stored, err := s.ClaimTask(ctx, st, 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	// A second task may not take the one slot, and neither may a re-claim.
	if _, _, err := s.ClaimTask(ctx, gateTask(t, "t2", "repo-a", nil), 1); err != nil {
		t.Fatalf("ClaimTask(t2): %v", err)
	}
	took, err := s.ClaimProvisioningSlot(ctx, stored.ID, 1)
	if err != nil {
		t.Errorf("re-claiming a held slot = %v, want no error", err)
	}
	if took {
		t.Error("re-claiming a held slot reported that this call took it: a second caller would provision the same task")
	}
	if _, err := s.ClaimProvisioningSlot(ctx, "t2", 1); !errors.Is(err, state.ErrBudgetExhausted) {
		t.Errorf("ClaimProvisioningSlot(t2) err = %v, want ErrBudgetExhausted", err)
	}

	// Releasing the claim frees the slot for the queued task.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID); err != nil {
		t.Fatalf("ReleaseProvisioningSlot: %v", err)
	}
	if took, err := s.ClaimProvisioningSlot(ctx, "t2", 1); err != nil || !took {
		t.Fatalf("ClaimProvisioningSlot(t2) after the release = (%v, %v), want the free claim", took, err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots = (%d, %v), want (1, nil)", slots, err)
	}

	// A task that reached a resource-owning phase owns its slot by its phase, so
	// its claim must not be released: that would hand out a slot whose worktree
	// and pane still exist.
	if _, err := s.UpdateTask(ctx, "t2", 1, func(next *task.Task) error {
		next.Phase = task.WorktreeCreated
		return nil
	}); err != nil {
		t.Fatalf("advance t2: %v", err)
	}
	if err := s.ReleaseProvisioningSlot(ctx, "t2"); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("ReleaseProvisioningSlot(advanced task) err = %v, want ErrInvalid", err)
	}
	if _, err := s.UpdateTask(ctx, stored.ID, 1, func(next *task.Task) error {
		next.Phase = task.WorktreeCreated
		return nil
	}); err != nil {
		t.Fatalf("advance t1: %v", err)
	}
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("ReleaseProvisioningSlot(advanced task) err = %v, want ErrInvalid", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 2 {
		t.Errorf("ProvisioningSlots = (%d, %v), want both phases counted", slots, err)
	}
}

func TestStoreConcurrentClaimsHaveExactlyTheBudgetWinners(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := open(t, dir)
	second := open(t, dir)

	// Two connections, a budget of two and four requests: the gate must hand out
	// exactly two slots, whichever process asks first.
	type result struct {
		claimed bool
		err     error
	}
	var wg sync.WaitGroup
	results := make(chan result, 4)
	start := make(chan struct{})
	for i, s := range []*state.Store{first, second} {
		for j := range 2 {
			wg.Add(1)
			go func(i, j int, s *state.Store) {
				defer wg.Done()
				<-start
				claimed, _, err := s.ClaimTask(ctx, gateTask(t, taskIDFor(i, j), "repo-a", nil), 2)
				results <- result{claimed, err}
			}(i, j, s)
		}
	}
	close(start)
	wg.Wait()
	close(results)

	claimed := 0
	for got := range results {
		switch {
		case got.err != nil:
			t.Errorf("concurrent claim failed: %v", got.err)
		case got.claimed:
			claimed++
		}
	}
	if claimed != 2 {
		t.Fatalf("claimed slots = %d, want exactly the budget of 2", claimed)
	}
	if slots, err := first.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 2 {
		t.Fatalf("ProvisioningSlots = (%d, %v), want (2, nil)", slots, err)
	}
	if tasks, err := first.Tasks(ctx, "repo-a"); err != nil {
		t.Fatalf("Tasks: %v", err)
	} else if len(tasks) != 4 {
		t.Errorf("recorded tasks = %d, want all four requests recorded", len(tasks))
	}
}

func taskIDFor(i, j int) string {
	return "orch_" + string(rune('a'+i)) + string(rune('0'+j))
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && strings.Contains(haystack, needle)
}

func TestStoreClaimStepIsExclusivePerPhase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := open(t, dir)
	second := open(t, dir)

	if _, _, err := first.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 3); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// Two connections claiming the same step of the same task: exactly one wins.
	took, err := first.ClaimStep(ctx, "t1", task.Intent, "owner-a", time.Minute)
	if err != nil || !took {
		t.Fatalf("first ClaimStep = (%v, %v), want the claim", took, err)
	}
	if took, err := second.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Fatalf("second ClaimStep = (%v, %v), want it refused", took, err)
	}
	// The phase a task has moved past cannot be claimed again, even by another
	// owner: a caller with a stale read must not re-run a finished step.
	if _, err := first.UpdateTask(ctx, "t1", 1, func(next *task.Task) error {
		next.Phase = task.WorktreeCreated
		return nil
	}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if took, err := second.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Errorf("ClaimStep on a phase the task left = (%v, %v), want it refused", took, err)
	}
	// The next phase is claimable, and the old owner's claim does not block it.
	if took, err := second.ClaimStep(ctx, "t1", task.WorktreeCreated, "owner-b", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep for the new phase = (%v, %v), want the claim", took, err)
	}
	// A claim whose lease expired can be taken over: that is crash recovery.
	if err := second.ReleaseStep(ctx, "t1", "owner-b"); err != nil {
		t.Fatalf("ReleaseStep: %v", err)
	}
	if took, err := second.ClaimStep(ctx, "t1", task.WorktreeCreated, "owner-dead", 20*time.Millisecond); err != nil || !took {
		t.Fatalf("ClaimStep(dead) = (%v, %v), want the claim", took, err)
	}
	time.Sleep(40 * time.Millisecond)
	if took, err := first.ClaimStep(ctx, "t1", task.WorktreeCreated, "owner-live", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep after the lease expired = (%v, %v), want the claim", took, err)
	}
	// Releasing someone else's claim is refused, never silent.
	if err := first.ReleaseStep(ctx, "t1", "owner-dead"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("ReleaseStep(other owner) err = %v, want ErrConflict", err)
	}
	if err := first.ReleaseStep(ctx, "t1", "owner-live"); err != nil {
		t.Errorf("ReleaseStep(own claim) err = %v", err)
	}
}
