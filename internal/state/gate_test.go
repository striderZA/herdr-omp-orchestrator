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

// A queued task's slot claim that no caller is using is one a drain may continue:
// it is older than the step lease and no step claim is live. A fresh claim, a
// claim under a live step and a released claim all belong to somebody else, or to
// nobody, so none of them is abandoned.
func TestStoreSlotClaimAbandonedOnlyAfterTheLeaseWithNoLiveStep(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	const lease = 50 * time.Millisecond

	claimed, stored, token, err := s.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 3)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	if abandoned, err := s.SlotClaimAbandoned(ctx, stored.ID, lease); err != nil || abandoned {
		t.Fatalf("a fresh claim = (%v, %v), want its caller's", abandoned, err)
	}
	time.Sleep(2 * lease)
	if abandoned, err := s.SlotClaimAbandoned(ctx, stored.ID, lease); err != nil || !abandoned {
		t.Fatalf("a claim older than the lease with no step = (%v, %v), want abandoned", abandoned, err)
	}
	// A live step claim is a caller provisioning the task right now.
	if took, err := s.ClaimStep(ctx, stored.ID, task.Intent, "owner-a", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the step", took, err)
	}
	if abandoned, err := s.SlotClaimAbandoned(ctx, stored.ID, lease); err != nil || abandoned {
		t.Fatalf("a claim under a live step = (%v, %v), want its owner's", abandoned, err)
	}
	if err := s.ReleaseStep(ctx, stored.ID, "owner-a"); err != nil {
		t.Fatalf("ReleaseStep: %v", err)
	}
	if abandoned, err := s.SlotClaimAbandoned(ctx, stored.ID, lease); err != nil || !abandoned {
		t.Fatalf("a claim whose step was given back = (%v, %v), want abandoned again", abandoned, err)
	}
	// A released claim is no claim at all.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, token); err != nil {
		t.Fatalf("ReleaseProvisioningSlot: %v", err)
	}
	if abandoned, err := s.SlotClaimAbandoned(ctx, stored.ID, lease); err != nil || abandoned {
		t.Fatalf("a released claim = (%v, %v), want nothing to continue", abandoned, err)
	}
	if _, err := s.SlotClaimAbandoned(ctx, "missing", lease); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("an unknown task = %v, want ErrNotFound", err)
	}
}

func TestStoreClaimTaskRespectsTheBudgetAndReservations(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	// Two slots: the first two claims succeed, the third is recorded without one.
	for _, id := range []string{"t1", "t2"} {
		claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, id, "repo-a", nil), 2)
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
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t3", "repo-a", nil), 2)
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
	if _, _, _, err := s.ClaimTask(ctx, gateTask(t, "t4", "repo-a", []string{"notes.md"}), 2); err != nil {
		t.Fatalf("ClaimTask(t4): %v", err)
	}
	_, _, _, err = s.ClaimTask(ctx, gateTask(t, "t5", "repo-a", []string{"NOTES.md"}), 2)
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
	if _, _, _, err := s.ClaimTask(ctx, gateTask(t, "t6", "repo-a", []string{"notes.md/inner.txt"}), 2); !errors.Is(err, state.ErrReservedConflict) {
		t.Errorf("ClaimTask(t6) err = %v, want ErrReservedConflict for a path inside the reservation", err)
	}
	// A closed task no longer holds its reservation.
	if _, err := s.UpdateTask(ctx, "t4", 1, func(next *task.Task) error {
		next.Disposition = "integrated"
		return nil
	}); err != nil {
		t.Fatalf("close t4: %v", err)
	}
	if _, _, _, err := s.ClaimTask(ctx, gateTask(t, "t7", "repo-a", []string{"notes.md"}), 2); err != nil {
		t.Errorf("a closed task still holds its reservation: %v", err)
	}
}

func TestStoreProvisioningSlotClaimIsIdempotentAndReleasable(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	st := gateTask(t, "t1", "repo-a", nil)
	claimed, stored, token, err := s.ClaimTask(ctx, st, 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	// A second task may not take the one slot, and neither may a re-claim.
	_, _, queuedToken, err := s.ClaimTask(ctx, gateTask(t, "t2", "repo-a", nil), 1)
	if err != nil {
		t.Fatalf("ClaimTask(t2): %v", err)
	}
	if queuedToken != 0 {
		t.Errorf("a task refused a slot reported the claim token %d, want none", queuedToken)
	}
	took, reused, err := s.ClaimProvisioningSlot(ctx, stored.ID, 1)
	if err != nil {
		t.Errorf("re-claiming a held slot = %v, want no error", err)
	}
	if took {
		t.Error("re-claiming a held slot reported that this call took it: a second caller would provision the same task")
	}
	if reused != 0 {
		t.Errorf("re-claiming a held slot reported the token %d, want none: it took nothing", reused)
	}
	if _, _, err := s.ClaimProvisioningSlot(ctx, "t2", 1); !errors.Is(err, state.ErrBudgetExhausted) {
		t.Errorf("ClaimProvisioningSlot(t2) err = %v, want ErrBudgetExhausted", err)
	}

	// Releasing the claim frees the slot for the queued task. The token is the one
	// this call acquired; the queued task's claim never existed to release.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, token); err != nil {
		t.Fatalf("ReleaseProvisioningSlot: %v", err)
	}
	took, claimedToken, err := s.ClaimProvisioningSlot(ctx, "t2", 1)
	if err != nil || !took {
		t.Fatalf("ClaimProvisioningSlot(t2) after the release = (%v, %v), want the free claim", took, err)
	}
	if claimedToken == 0 || claimedToken == token {
		t.Errorf("the free claim's token = %d, want a fresh token (the released one was %d)", claimedToken, token)
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
	if err := s.ReleaseProvisioningSlot(ctx, "t2", claimedToken); !errors.Is(err, state.ErrInvalid) {
		t.Errorf("ReleaseProvisioningSlot(advanced task) err = %v, want ErrInvalid", err)
	}
	if _, err := s.UpdateTask(ctx, stored.ID, 1, func(next *task.Task) error {
		next.Phase = task.WorktreeCreated
		return nil
	}); err != nil {
		t.Fatalf("advance t1: %v", err)
	}
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, token); !errors.Is(err, state.ErrInvalid) {
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
				claimed, _, _, err := s.ClaimTask(ctx, gateTask(t, taskIDFor(i, j), "repo-a", nil), 2)
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

	if _, _, _, err := first.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 3); err != nil {
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

	// Another owner's late failure is refused outright: it does not hold this step, so
	// its outcome describes state this task does not have.
	if _, released, err := s.RecordStepFailure(ctx, "t1", current.Revision, note, "owner-b", task.Intent, true); !errors.Is(err, state.ErrStepNotHeld) || released {
		t.Fatalf("RecordStepFailure(other owner) = (%v, %v), want ErrStepNotHeld", released, err)
	}
	// The right owner may resolve the step it claimed even when the task has moved on
	// to the next phase: starting the worker records agent_started while the step that
	// started it is still the one running. The release, though, is phase-gated, so this
	// does not free the claim that belongs to the phase the task now stands on.
	recorded, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	written, released, err := s.RecordStepFailure(ctx, "t1", recorded.Revision, note, "owner-a", task.WorktreeCreated, true)
	if err != nil {
		t.Fatalf("RecordStepFailure(own step, next phase) = (%v, %v), want the failure stored", released, err)
	}
	if released {
		t.Errorf("released = true, want a claim for another phase left alone")
	}
	if written.Attention == "" || written.Revision != recorded.Revision+1 {
		t.Errorf("task = %+v, want the failure stored once", written)
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
	if _, released, err := s.RecordStepFailure(ctx, "t1", written.Revision, refuse, "owner-a", task.Intent, true); err == nil || released {
		t.Fatalf("RecordStepFailure(refused mutation) = (%v, %v), want an error and no release", released, err)
	}
	if _, released, err := s.RecordStepFailure(ctx, "t1", written.Revision+9, note, "owner-a", task.Intent, true); !errors.Is(err, state.ErrConflict) || released {
		t.Fatalf("RecordStepFailure(stale revision) = (%v, %v), want ErrConflict and no release", released, err)
	}
	failed2, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if failed2.Revision != written.Revision {
		t.Fatalf("task after refused failures = %+v, want nothing further stored", failed2)
	}

	// The matching owner and phase release it, and the failure is stored with it.
	failed, released, err := s.RecordStepFailure(ctx, "t1", written.Revision, note, "owner-a", task.Intent, true)
	if err != nil || !released {
		t.Fatalf("RecordStepFailure = (%v, %v), want the release", released, err)
	}
	if failed.Revision != written.Revision+1 {
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

// TestStoreRecordStepFailureKeepsAnUndecidedOutcomeWithoutALiveOwner pins the other
// half of the fence: a step nobody holds is still the phase this caller ran, so an
// undecided outcome keeps its blocker — refusing it would leave the task resumable
// with nobody allowed to retry it safely. Only another owner's step is refused, and
// the release stays lease-gated so an expired claim is never freed from under one.
func TestStoreRecordStepFailureKeepsAnUndecidedOutcomeWithoutALiveOwner(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	st := gateTask(t, "t1", "repo-a", nil)
	if err := s.CreateTask(ctx, st); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	current, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	blocker := func(next *task.Task) error {
		next.Blocker = "the outcome is unknown"
		return nil
	}
	attention := func(next *task.Task) error {
		next.Attention = "the create failed"
		return nil
	}

	// Nobody holds the step at all: the undecided outcome is still recorded.
	if _, released, err := s.RecordStepFailure(ctx, "t1", current.Revision, blocker, "owner-a", task.Intent, false); err != nil || released {
		t.Fatalf("RecordStepFailure(no claim) = (%v, %v), want the failure recorded", released, err)
	}
	blocked, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if blocked.Blocker != "the outcome is unknown" {
		t.Fatalf("blocker = %q, want the undecided outcome recorded", blocked.Blocker)
	}

	// The caller's own claim expired while its step ran: its outcome is recorded, and
	// the expired claim is left alone (the takeover that follows needs no release).
	if took, err := s.ClaimStep(ctx, "t1", task.Intent, "owner-a", 20*time.Millisecond); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the claim", took, err)
	}
	time.Sleep(40 * time.Millisecond)
	decided, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	recorded, released, err := s.RecordStepFailure(ctx, "t1", decided.Revision, attention, "owner-a", task.Intent, true)
	if err != nil {
		t.Fatalf("RecordStepFailure(expired own claim) = (%v, %v), want the failure recorded", released, err)
	}
	if released {
		t.Errorf("released = true, want an expired claim left to be taken over")
	}
	if recorded.Attention != "the create failed" {
		t.Errorf("attention = %q, want the definite failure recorded", recorded.Attention)
	}
	// The step can still be taken over at once, so a definite failure never leaves the
	// task behind a lease.
	if took, err := s.ClaimStep(ctx, "t1", task.Intent, "owner-b", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep after the expired failure = (%v, %v), want the takeover", took, err)
	}
	// And the expired owner can no longer release what is now somebody else's.
	if err := s.ReleaseStep(ctx, "t1", "owner-a"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("ReleaseStep(expired owner) err = %v, want ErrConflict", err)
	}
	// The live owner's step is not this caller's to resolve.
	live, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if _, _, err := s.RecordStepFailure(ctx, "t1", live.Revision, blocker, "owner-a", task.Intent, false); !errors.Is(err, state.ErrStepNotHeld) {
		t.Fatalf("RecordStepFailure(another live owner) err = %v, want ErrStepNotHeld", err)
	}
	after, err := s.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Blocker != blocked.Blocker || after.Revision != live.Revision {
		t.Errorf("task after a refused failure = %+v, want the live owner's state", after)
	}
}

// TestStoreSlotReleaseLeavesAnActiveRetryItsCapacity pins the step half of the
// release fence: while a retry owns the task's step, the failed attempt's release
// must not clear the capacity the retry is provisioning with.
func TestStoreSlotReleaseLeavesAnActiveRetryItsCapacity(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	claimed, stored, token, err := s.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want the one slot", claimed, err)
	}
	// The retry takes the task's step for the phase the task still records. The
	// task stays at that phase: the retry is provisioning it right now.
	took, err := s.ClaimStep(ctx, stored.ID, task.Intent, "retry-owner", time.Minute)
	if err != nil || !took {
		t.Fatalf("ClaimStep of the retry = (%v, %v), want the retry's claim", took, err)
	}

	// The release presents the token the failed attempt acquired, so this is the
	// legitimate give-back — and it is still refused, because a live step owns the
	// task.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, token); !errors.Is(err, state.ErrConflict) {
		t.Errorf("ReleaseProvisioningSlot during a live retry step = %v, want ErrConflict", err)
	}

	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots after the old release = (%d, %v), want 1: the retry's active capacity claim was cleared", slots, err)
	}
	// The retry still owns the claim, so no second caller may take it.
	if took, _, err := s.ClaimProvisioningSlot(ctx, stored.ID, 1); err != nil || took {
		t.Errorf("a second caller took the retry's claim = (%v, %v), want the retry's claim left alone", took, err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots after the second claim attempt = (%d, %v), want 1", slots, err)
	}
}

// TestStoreRenewRefusesWithoutOwningTheStep pins the ordering the renewal depends
// on: only the caller that owns the task's live step claim may renew its slot claim.
//
// Renewing before the step were taken is what makes a resume unsound: a resume that
// then finds the step in flight (ErrStepInFlight) has already renewed the token past
// the attempt that failed, so that attempt's legitimate release is refused and the
// slot stays charged with nobody provisioning it. The first task shows the fence, the
// second shows the consequence for a failed attempt that no renewal got in the way
// of.
func TestStoreRenewRefusesWithoutOwningTheStep(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	claimed, stored, token, err := s.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 3)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	// The older attempt owns the task's step right now, and its worktree create has
	// not concluded.
	if took, err := s.ClaimStep(ctx, stored.ID, task.Intent, "attempt-a", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep(attempt-a) = (%v, %v), want the live step claim", took, err)
	}

	// A resume may not renew a claim under a step it does not own.
	if renewed, err := s.RenewProvisioningSlot(ctx, stored.ID, "resume-b"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("RenewProvisioningSlot(resume-b) = (%d, %v), want ErrConflict", renewed, err)
	}
	// The step's owner may: the renewal is the act of the caller running the step.
	renewed, err := s.RenewProvisioningSlot(ctx, stored.ID, "attempt-a")
	if err != nil || renewed == token {
		t.Fatalf("RenewProvisioningSlot(attempt-a) = (%d, %v), want the step owner's fresh token", renewed, err)
	}

	// The other task's failed attempt never had its claim renewed, so the release it
	// makes still matches and the capacity comes back.
	claimedOther, other, otherToken, err := s.ClaimTask(ctx, gateTask(t, "t2", "repo-a", nil), 3)
	if err != nil || !claimedOther {
		t.Fatalf("ClaimTask(t2) = (%v, %v), want a slot", claimedOther, err)
	}
	if took, err := s.ClaimStep(ctx, other.ID, task.Intent, "attempt-b", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep(attempt-b) = (%v, %v), want the live step claim", took, err)
	}
	if _, _, err := s.RecordStepFailure(ctx, other.ID, other.Revision, func(next *task.Task) error {
		next.Attention = "task t2 could not create the worktree: the checkout was refused"
		return nil
	}, "attempt-b", task.Intent, true); err != nil {
		t.Fatalf("RecordStepFailure(t2): %v", err)
	}
	if err := s.ReleaseProvisioningSlot(ctx, other.ID, otherToken); err != nil {
		t.Errorf("ReleaseProvisioningSlot(attempt-b's token) = %v, want the capacity back", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots = (%d, %v), want only t1's renewed claim", slots, err)
	}
}

// TestStoreSlotReleaseRefusesAClaimAnotherCallerTookOver pins the token half of the
// release fence, in the window that has nothing else to show for it: a retry takes
// the task's step over and renews the claim it continued on (took == false, so no
// count changes). Between that renewal and the retry's own write there is nothing
// else about the task to show the reuse, so only the renewed token records that the
// capacity is the retry's — and the failed attempt's late release, which presents
// the claim it acquired, must be refused with the capacity left charged.
func TestStoreSlotReleaseRefusesAClaimAnotherCallerTookOver(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	claimed, stored, token, err := s.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want the one slot", claimed, err)
	}
	// The definite failure the failed attempt records: the reason is durable and its
	// own step claim goes back, while the slot claim stays for the retry.
	failed, _, err := s.RecordStepFailure(ctx, stored.ID, stored.Revision, func(next *task.Task) error {
		next.Attention = "task t1 could not create the worktree: the checkout was refused"
		return nil
	}, "failed-owner", task.Intent, true)
	if err != nil {
		t.Fatalf("RecordStepFailure: %v", err)
	}
	if failed.Attention == "" {
		t.Fatal("the definite failure was not recorded")
	}

	// The retry reuses the held claim, takes the step (the only ordering the store
	// allows a renewal in), and renews: this is the durable sign that the capacity is
	// now the retry's.
	took, reused, err := s.ClaimProvisioningSlot(ctx, stored.ID, 1)
	if err != nil {
		t.Fatalf("ClaimProvisioningSlot of the retry: %v", err)
	}
	if took {
		t.Fatal("the retry took the claim although the failed attempt still holds it")
	}
	if reused != 0 {
		t.Errorf("a reused claim reported the token %d, want none until the retry renews it", reused)
	}
	if took, err := s.ClaimStep(ctx, failed.ID, task.Intent, "retry-owner", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep(retry-owner) = (%v, %v), want the retry's step claim", took, err)
	}
	renewed, err := s.RenewProvisioningSlot(ctx, stored.ID, "retry-owner")
	if err != nil {
		t.Fatalf("RenewProvisioningSlot: %v", err)
	}
	if renewed == token || renewed == 0 {
		t.Errorf("renewed token = %d, want a new non-zero token (the failed attempt holds %d)", renewed, token)
	}

	// The failed attempt's release runs here, with the claim it acquired.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, token); !errors.Is(err, state.ErrConflict) {
		t.Errorf("ReleaseProvisioningSlot(stale token) = %v, want ErrConflict", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 1 {
		t.Fatalf("ProvisioningSlots after the stale release = (%d, %v), want 1: the retry lost its capacity", slots, err)
	}

	// The retry's own give-back (its token) still works, so the fence is a fence and
	// not a leak: it records its definite failure, which releases its step claim, and
	// then gives the claim back.
	if _, _, err := s.RecordStepFailure(ctx, failed.ID, failed.Revision, func(next *task.Task) error {
		next.Attention = "task t1 could not create the worktree: still refused"
		return nil
	}, "retry-owner", task.Intent, true); err != nil {
		t.Fatalf("RecordStepFailure(retry-owner): %v", err)
	}
	if err := s.ReleaseProvisioningSlot(ctx, failed.ID, renewed); err != nil {
		t.Fatalf("ReleaseProvisioningSlot(renewed token): %v", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 0 {
		t.Errorf("ProvisioningSlots after the retry released = (%d, %v), want 0", slots, err)
	}
}

// TestStoreSlotReleaseRefusesAClaimItDidNotTake pins the identity half of the fence:
// a caller may only give back the claim it took. A claim another caller took fresh,
// and a token this caller never acquired, are both refused.
func TestStoreSlotReleaseRefusesAClaimItDidNotTake(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())

	claimed, stored, token, err := s.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want the one slot", claimed, err)
	}
	// A zero token is "I acquired nothing": there is nothing to give back.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, 0); !errors.Is(err, state.ErrConflict) {
		t.Errorf("ReleaseProvisioningSlot(0) = %v, want ErrConflict", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 1 {
		t.Fatalf("ProvisioningSlots after the empty release = (%d, %v), want 1", slots, err)
	}

	// Another caller takes the claim this one gave back: the released token is a
	// claim nobody holds any more, so releasing it again is refused, and the fresh
	// claim stays counted until the caller that took it gives it back.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, token); err != nil {
		t.Fatalf("the owner's own release: %v", err)
	}
	fresh, freshToken, err := s.ClaimProvisioningSlot(ctx, stored.ID, 1)
	if err != nil || !fresh {
		t.Fatalf("ClaimProvisioningSlot = (%v, %v), want the fresh claim", fresh, err)
	}
	if freshToken == token {
		t.Fatalf("the fresh claim reused the released token %d", token)
	}
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, token); !errors.Is(err, state.ErrConflict) {
		t.Errorf("ReleaseProvisioningSlot(released token) = %v, want ErrConflict", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots after the refused release = (%d, %v), want the fresh claim counted", slots, err)
	}
	// The claim that took it can give it back.
	if err := s.ReleaseProvisioningSlot(ctx, stored.ID, freshToken); err != nil {
		t.Fatalf("the fresh owner's release: %v", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 0 {
		t.Errorf("ProvisioningSlots after the fresh owner released = (%d, %v), want 0", slots, err)
	}
}

// TestStoreSlotClaimDistinguishesASpentBudgetFromAStorageFailure pins the two
// answers a caller of the gate has to tell apart: a full budget is a normal
// outcome that queues the work, and a claim that could not be stored is a failure
// the caller must see instead of reading it as capacity pressure.
func TestStoreSlotClaimDistinguishesASpentBudgetFromAStorageFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)

	claimed, _, token, err := s.ClaimTask(ctx, gateTask(t, "t1", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask(t1) = (%v, %v), want the one slot", claimed, err)
	}
	// t2 is recorded without a slot: the budget is spent.
	if _, _, _, err := s.ClaimTask(ctx, gateTask(t, "t2", "repo-a", nil), 1); err != nil {
		t.Fatalf("ClaimTask(t2): %v", err)
	}
	if _, _, err := s.ClaimProvisioningSlot(ctx, "t2", 1); !errors.Is(err, state.ErrBudgetExhausted) {
		t.Errorf("ClaimProvisioningSlot with a spent budget = %v, want ErrBudgetExhausted", err)
	}

	// A claim that cannot be written is not a spent budget.
	raw := rawState(t, dir)
	if _, err := raw.ExecContext(ctx, `CREATE TRIGGER refuse_slot_claim
		BEFORE UPDATE OF slot_claimed_at ON tasks
		WHEN NEW.slot_claimed_at > 0
		BEGIN SELECT RAISE(ABORT, 'slot claim is not writable'); END`); err != nil {
		t.Fatalf("install the slot-claim trigger: %v", err)
	}
	if err := s.ReleaseProvisioningSlot(ctx, "t1", token); err != nil {
		t.Fatalf("ReleaseProvisioningSlot(t1): %v", err)
	}
	_, _, err = s.ClaimProvisioningSlot(ctx, "t1", 1)
	if err == nil {
		t.Fatal("ClaimProvisioningSlot succeeded although the claim could not be written")
	}
	if errors.Is(err, state.ErrBudgetExhausted) {
		t.Errorf("a storage failure was reported as a spent budget: %v", err)
	}
	if slots, err := s.ProvisioningSlots(ctx, "repo-a"); err != nil || slots != 0 {
		t.Errorf("ProvisioningSlots after the failed claim = (%d, %v), want nothing claimed", slots, err)
	}
}
