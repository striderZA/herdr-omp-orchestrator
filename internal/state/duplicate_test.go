package state_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// This file covers the duplicate gate of the dispatch claim (issue #4): a
// request for work an undecided task of the same project already holds is
// answered with that task and records nothing. It is decided inside the claim's
// own write transaction, so two coordinator processes cannot both pass it.

// sameWork is one fixed request; only the identity differs between calls.
func sameWork(id, projectKey string, reserved []string) task.Task {
	return task.Task{
		ID:            id,
		ProjectKey:    projectKey,
		Title:         "Plan: parser",
		Brief:         "Investigate and report a plan.",
		Checks:        []string{"go test ./... ok"},
		BaseRevision:  "0123456",
		Branch:        "orch/" + id,
		ReservedPaths: reserved,
		Phase:         task.Intent,
	}
}

func duplicateOf(t *testing.T, err error) task.Task {
	t.Helper()
	if !errors.Is(err, state.ErrDuplicateOpen) {
		t.Fatalf("err = %v, want ErrDuplicateOpen", err)
	}
	var duplicate *state.DuplicateError
	if !errors.As(err, &duplicate) {
		t.Fatalf("err = %v carries no DuplicateError", err)
	}
	return duplicate.Existing
}

func TestStoreClaimTaskAnswersAnOpenDuplicateAndRecordsNothing(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if _, _, _, err := s.ClaimTask(ctx, sameWork("t1", "repo-a", nil), 3); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	slotsBefore, err := s.ProvisioningSlots(ctx, "repo-a")
	if err != nil {
		t.Fatalf("ProvisioningSlots: %v", err)
	}

	claimed, stored, token, err := s.ClaimTask(ctx, sameWork("t2", "repo-a", nil), 3)
	existing := duplicateOf(t, err)
	if existing.ID != "t1" {
		t.Errorf("the duplicate names %q, want the open task t1", existing.ID)
	}
	if claimed || token != 0 || stored.ID != "" {
		t.Errorf("a refused claim returned (%v, %q, %d): it must take and record nothing", claimed, stored.ID, token)
	}
	if _, err := s.Task(ctx, "t2"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a duplicate was recorded: %v", err)
	}
	if tasks, err := s.Tasks(ctx, "repo-a"); err != nil || len(tasks) != 1 {
		t.Errorf("tasks = %d (%v), want the one original", len(tasks), err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != slotsBefore {
		t.Errorf("ProvisioningSlots = (%d, %v), want it unchanged at %d", slots, err, slotsBefore)
	}
}

func TestStoreClaimTaskAcceptsDifferentWork(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if _, _, _, err := s.ClaimTask(ctx, sameWork("t1", "repo-a", nil), 3); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	for name, mutate := range map[string]func(*task.Task){
		"title":   func(x *task.Task) { x.Title = "Plan: lexer" },
		"brief":   func(x *task.Task) { x.Brief = "A different brief." },
		"checks":  func(x *task.Task) { x.Checks = []string{"go vet ./... ok"} },
		"base":    func(x *task.Task) { x.BaseRevision = "fedcba9" },
		"reserve": func(x *task.Task) { x.ReservedPaths = []string{"src/other.go"} },
	} {
		other := sameWork("t_"+name, "repo-a", nil)
		mutate(&other)
		if _, _, _, err := s.ClaimTask(ctx, other, 9); err != nil {
			t.Errorf("work that differs in its %s was refused: %v", name, err)
		}
	}
}

func TestStoreClaimTaskIgnoresAClosedTask(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if _, _, _, err := s.ClaimTask(ctx, sameWork("t1", "repo-a", nil), 3); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if _, err := s.UpdateTask(ctx, "t1", 1, func(next *task.Task) error {
		next.Outcome = task.OutcomeRejected
		next.Disposition = "not what I wanted"
		return nil
	}); err != nil {
		t.Fatalf("close t1: %v", err)
	}
	// A decided task is finished work: asking for it again is a new request, and a
	// rejected task in particular is exactly when a user dispatches it again.
	if _, _, _, err := s.ClaimTask(ctx, sameWork("t2", "repo-a", nil), 3); err != nil {
		t.Fatalf("the same work after its task was decided: %v", err)
	}
}

func TestStoreClaimTaskDuplicateGateStaysInsideTheProject(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if _, _, _, err := s.ClaimTask(ctx, sameWork("t1", "repo-a", nil), 3); err != nil {
		t.Fatalf("project A claim: %v", err)
	}
	// The same request in another project is not a repeat, and the answer to a
	// real repeat never names a task of another project.
	if _, _, _, err := s.ClaimTask(ctx, sameWork("t2", "repo-b", nil), 3); err != nil {
		t.Fatalf("the same work in project B was refused: %v", err)
	}
	_, _, _, err := s.ClaimTask(ctx, sameWork("t3", "repo-b", nil), 3)
	if existing := duplicateOf(t, err); existing.ID != "t2" || existing.ProjectKey != "repo-b" {
		t.Errorf("the duplicate in project B names %q of %q, want t2 of repo-b", existing.ID, existing.ProjectKey)
	}
}

func TestStoreClaimTaskDuplicateComesBeforeTheReservedPathConflict(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	paths := []string{"src/main.txt"}
	if _, _, _, err := s.ClaimTask(ctx, sameWork("t1", "repo-a", paths), 3); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// A repeat holds the very paths the first task holds. It is a repeat, not a
	// second worker that wants the same files, so it is answered as one.
	_, _, _, err := s.ClaimTask(ctx, sameWork("t2", "repo-a", paths), 3)
	if errors.Is(err, state.ErrReservedConflict) {
		t.Fatal("an identical repeat was reported as a reserved-path conflict")
	}
	duplicateOf(t, err)
	// Different work for the same paths is still a conflict.
	clash := sameWork("t3", "repo-a", paths)
	clash.Title = "Another task on the same files"
	if _, _, _, err := s.ClaimTask(ctx, clash, 3); !errors.Is(err, state.ErrReservedConflict) {
		t.Errorf("different work on held paths err = %v, want ErrReservedConflict", err)
	}
}

func TestStoreClaimTaskCountsAQueuedTaskAsOpen(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	// Budget of one: the first task takes the slot, the second is recorded queued.
	if _, _, _, err := s.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 1); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	claimed, _, _, err := s.ClaimTask(ctx, sameWork("t2", "repo-a", nil), 1)
	if err != nil || claimed {
		t.Fatalf("second claim = (%v, %v), want recorded without a slot", claimed, err)
	}
	// A full budget must not hide the duplicate: it is answered, not queued again.
	_, _, _, err = s.ClaimTask(ctx, sameWork("t3", "repo-a", nil), 1)
	if existing := duplicateOf(t, err); existing.ID != "t2" {
		t.Errorf("the duplicate names %q, want the queued task t2", existing.ID)
	}
}

func TestStoreConcurrentIdenticalClaimsRecordExactlyOneTask(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	stores := []*state.Store{open(t, dir), open(t, dir)}

	const requests = 8
	type result struct{ err error }
	results := make(chan result, requests)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, _, err := stores[i%2].ClaimTask(ctx, sameWork(taskIDFor(i%2, i/2), "repo-a", nil), 9)
			results <- result{err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	recorded, duplicates := 0, 0
	for got := range results {
		switch {
		case got.err == nil:
			recorded++
		case errors.Is(got.err, state.ErrDuplicateOpen):
			duplicates++
		default:
			t.Errorf("unexpected claim error: %v", got.err)
		}
	}
	if recorded != 1 || duplicates != requests-1 {
		t.Fatalf("recorded=%d duplicates=%d, want exactly one winner and %d duplicates", recorded, duplicates, requests-1)
	}
	if tasks, err := stores[0].Tasks(ctx, "repo-a"); err != nil || len(tasks) != 1 {
		t.Fatalf("stored tasks = %d (%v), want 1", len(tasks), err)
	}
}

func TestStoreClaimTaskNamesTheOldestOfSeveralOpenDuplicates(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	// Duplicates recorded before the gate existed (the bug this gate closes) are
	// still in the store. The answer must not depend on row order.
	for _, id := range []string{"t_old", "t_new"} {
		if err := s.CreateTask(ctx, sameWork(id, "repo-a", nil)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, _, _, err := s.ClaimTask(ctx, sameWork("t_third", "repo-a", nil), 9)
	if existing := duplicateOf(t, err); existing.ID != "t_old" {
		t.Errorf("the duplicate names %q, want the oldest open task t_old", existing.ID)
	}
}
