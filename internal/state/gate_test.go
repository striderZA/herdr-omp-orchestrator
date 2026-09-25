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

// gateNote is the durable note a decision records, empty enough to be valid.
func gateNote(summary string) task.Inbox {
	return task.Inbox{
		ID:         "adjudication:t1:" + summary,
		ProjectKey: "repo-a",
		TaskID:     "t1",
		Kind:       "adjudication",
		Summary:    summary,
		Delivery:   "recorded by the user",
	}
}

// TestStoreDecisionIsOneTransaction pins what one decision means: the task change,
// the retirement of the step claim and the note that records it are stored together
// or not at all. A decision that cannot be recorded must leave the task exactly as
// decidable as it was, with the step still claimed.
func TestStoreDecisionIsOneTransaction(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	other := open(t, dir)
	raw := rawState(t, dir)
	trigger := func(query string) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, query); err != nil {
			t.Fatalf("exec %q: %v", query, err)
		}
	}

	st := gateTask(t, "t1", "repo-a", nil)
	if err := s.CreateTask(ctx, st); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	current, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if took, err := s.ClaimStep(ctx, "t1", task.Intent, "owner-a", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the claim", took, err)
	}
	advance := func(next *task.Task) error {
		next.Blocker = ""
		next.Phase = task.PromptSubmitted
		return nil
	}

	// A failed mutation leaves the task and the claim alone: a decision that cannot
	// be stored must not retire the claim of a step that may still be running.
	if _, err := s.RetireStepWithNote(ctx, "t1", current.Revision, func(*task.Task) error {
		return errors.New("refused")
	}, gateNote("refused")); err == nil {
		t.Fatal("RetireStepWithNote with a failing mutation = nil, want an error")
	}
	if took, err := other.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after a failed mutation = (%v, %v), want the claim still held", took, err)
	}

	// A stale revision is refused the same way.
	if _, err := s.RetireStepWithNote(ctx, "t1", current.Revision+9, advance, gateNote("stale")); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale revision err = %v, want ErrConflict", err)
	}
	if took, err := other.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after a stale decision = (%v, %v), want the claim still held", took, err)
	}

	// A note that cannot be stored rolls the whole decision back: the task keeps its
	// phase and its claim, and stays decidable, instead of being decided with a
	// silent hole where its audit trail should be.
	trigger(`CREATE TRIGGER refuse_inbox BEFORE INSERT ON inbox BEGIN SELECT RAISE(ABORT, 'inbox is not writable'); END`)
	if _, err := s.RetireStepWithNote(ctx, "t1", current.Revision, advance, gateNote("unwritable")); err == nil {
		t.Fatal("RetireStepWithNote with an unwritable note = nil, want an error")
	}
	after, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Phase != task.Intent || after.Revision != current.Revision {
		t.Errorf("task after a refused note = %+v, want it unchanged", after)
	}
	if took, err := other.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after a refused note = (%v, %v), want the claim still held", took, err)
	}
	if notes, err := s.PendingInbox(ctx, "repo-a"); err != nil || len(notes) != 0 {
		t.Fatalf("inbox after a refused note = (%d, %v), want nothing stored", len(notes), err)
	}
	trigger(`DROP TRIGGER refuse_inbox`)

	// With the note writable again, one call stores the task change, the note and the
	// freed claim together: the next caller can take the step the decision resolved.
	decided, err := s.RetireStepWithNote(ctx, "t1", current.Revision, advance, gateNote("decided"))
	if err != nil {
		t.Fatalf("RetireStepWithNote: %v", err)
	}
	if decided.Phase != task.PromptSubmitted || decided.Blocker != "" || decided.Revision != current.Revision+1 {
		t.Fatalf("decided task = %+v, want the mutation stored once", decided)
	}
	if _, err := s.RetireStepWithNote(ctx, "t1", current.Revision, advance, gateNote("decided")); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("replayed decision err = %v, want ErrConflict", err)
	}
	notes, err := s.PendingInbox(ctx, "repo-a")
	if err != nil {
		t.Fatalf("PendingInbox: %v", err)
	}
	if len(notes) != 1 || notes[0].Summary != "decided" {
		t.Errorf("inbox = %+v, want exactly the one decision note", notes)
	}
	if took, err := other.ClaimStep(ctx, "t1", task.PromptSubmitted, "owner-b", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep for the next step = (%v, %v), want it free", took, err)
	}
}

// TestStoreRecordStepFailureReleasesOnlyItsOwnClaim pins the failure half: the
// failure is stored with the release of this owner's claim for that exact phase, in
// one transaction, and nothing is released when the write does not happen.
func TestStoreRecordStepFailureReleasesOnlyItsOwnClaim(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	other := open(t, dir)

	st := gateTask(t, "t1", "repo-a", nil)
	if err := s.CreateTask(ctx, st); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	current, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if took, err := s.ClaimStep(ctx, "t1", task.Intent, "owner-a", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the claim", took, err)
	}
	note := func(next *task.Task) error {
		next.Attention = "the create failed"
		return nil
	}

	// Another owner's release attempt does not touch this claim, even in the same
	// transaction that records a failure.
	if _, released, err := s.RecordStepFailure(ctx, "t1", current.Revision, note, "owner-b", task.Intent, true); err != nil || released {
		t.Fatalf("RecordStepFailure(other owner) = (%v, %v), want no release", released, err)
	}
	if took, err := other.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after a foreign release = (%v, %v), want the claim still held", took, err)
	}
	// Nor does the right owner on the wrong phase: the claim belongs to the phase it
	// was taken for.
	recorded, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if _, released, err := s.RecordStepFailure(ctx, "t1", recorded.Revision, note, "owner-a", task.WorktreeCreated, true); err != nil || released {
		t.Fatalf("RecordStepFailure(wrong phase) = (%v, %v), want no release", released, err)
	}

	// A write that does not land releases nothing: a stale revision is refused and
	// the claim stays exactly where it was.
	recorded, err = s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	refuse := func(next *task.Task) error {
		next.Attention = "stale"
		return errors.New("the task moved on")
	}
	if _, released, err := s.RecordStepFailure(ctx, "t1", recorded.Revision, refuse, "owner-a", task.Intent, true); err == nil || released {
		t.Fatalf("RecordStepFailure(refused mutation) = (%v, %v), want an error and no release", released, err)
	}
	if _, released, err := s.RecordStepFailure(ctx, "t1", recorded.Revision+9, note, "owner-a", task.Intent, true); !errors.Is(err, state.ErrConflict) || released {
		t.Fatalf("RecordStepFailure(stale revision) = (%v, %v), want ErrConflict and no release", released, err)
	}
	failed2, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if failed2.Revision != recorded.Revision || failed2.Attention != "the create failed" {
		t.Fatalf("task after refused failures = %+v, want nothing further stored", failed2)
	}

	// The matching owner and phase release it, and the failure is stored with it.
	failed, released, err := s.RecordStepFailure(ctx, "t1", recorded.Revision, note, "owner-a", task.Intent, true)
	if err != nil || !released {
		t.Fatalf("RecordStepFailure = (%v, %v), want the release", released, err)
	}
	if failed.Attention != "the create failed" || failed.Revision != recorded.Revision+1 {
		t.Fatalf("failed task = %+v, want the failure stored", failed)
	}
	if took, err := other.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep after the release = (%v, %v), want it free", took, err)
	}
}

// TestStoreRecordStepFailureFailsClosedWhenTheReleaseCannotBeWritten pins that the
// claim release is part of the failure's transaction, not a follow-up write. If the
// release cannot be stored, the failure must not be stored either: a task with a
// recorded failure and a claim nobody can release is a task stuck behind a lease,
// and a caller that was told the write failed must find nothing changed.
func TestStoreRecordStepFailureFailsClosedWhenTheReleaseCannotBeWritten(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	other := open(t, dir)
	raw := rawState(t, dir)

	st := gateTask(t, "t1", "repo-a", nil)
	if err := s.CreateTask(ctx, st); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	current, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if took, err := s.ClaimStep(ctx, "t1", task.Intent, "owner-a", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the claim", took, err)
	}
	if _, err := raw.ExecContext(ctx,
		`CREATE TRIGGER refuse_release BEFORE UPDATE OF step_claim_owner ON tasks BEGIN SELECT RAISE(ABORT, 'the claim is not writable'); END`); err != nil {
		t.Fatalf("create the trigger: %v", err)
	}

	if _, released, err := s.RecordStepFailure(ctx, "t1", current.Revision, func(next *task.Task) error {
		next.Attention = "the create failed"
		return nil
	}, "owner-a", task.Intent, true); err == nil || released {
		t.Fatalf("RecordStepFailure with an unwritable release = (%v, %v), want an error and no release", released, err)
	}
	after, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Attention != "" || after.Revision != current.Revision {
		t.Errorf("task after a refused release = %+v, want the failure rolled back with it", after)
	}
	if took, err := other.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after a refused release = (%v, %v), want the claim still held", took, err)
	}

	// The same call on the undecided path writes no release, so the trigger cannot
	// matter: the failure is recorded and the claim stays with its owner.
	if _, err := raw.ExecContext(ctx, `DROP TRIGGER refuse_release`); err != nil {
		t.Fatalf("drop the trigger: %v", err)
	}
	failed, released, err := s.RecordStepFailure(ctx, "t1", current.Revision, func(next *task.Task) error {
		next.Blocker = "the outcome is unknown"
		return nil
	}, "owner-a", task.Intent, false)
	if err != nil || released {
		t.Fatalf("RecordStepFailure(release=false) = (%v, %v), want the failure stored without a release", released, err)
	}
	if failed.Blocker == "" {
		t.Errorf("failed task = %+v, want the blocker stored", failed)
	}
	if took, err := other.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after an undecided failure = (%v, %v), want the claim kept", took, err)
	}
}
