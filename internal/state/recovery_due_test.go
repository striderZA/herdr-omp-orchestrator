package state_test

import (
	"context"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// TestRecoveryClaimsClassifyOwnersAndLeases pins what recovery may read from each claim.
// A claim held by a live owner is live whatever its lease, so it is never taken over. A
// dead owner's claim is due once its lease ends. A legacy claim (no owner identity) is
// not live and is due once its lease ends, as the lease-only fallback requires. A task
// with no claim is not reported.
func TestRecoveryClaimsClassifyOwnersAndLeases(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	// The lease is long enough that claims stamped "now" stay young while the setup
	// below runs, and claims stamped two leases back are already due.
	const lease = 3 * time.Second
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	now := time.Now()

	// claim creates a task at worktree_created whose slot claim is stamped at `at`.
	claim := func(id string, at time.Time) string {
		t.Helper()
		claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, id, "repo-a", nil), 10)
		if err != nil || !claimed {
			t.Fatalf("ClaimTask %s = (%v, %v), want a slot", id, claimed, err)
		}
		if _, err := s.UpdateTask(ctx, stored.ID, stored.Revision, func(current *task.Task) error {
			current.Phase = task.WorktreeCreated
			return nil
		}); err != nil {
			t.Fatalf("advance %s to worktree_created: %v", id, err)
		}
		if _, err := rawState(t, dir).Exec(`UPDATE tasks SET slot_claimed_at = ? WHERE id = ?`,
			at.UnixMilli(), stored.ID); err != nil {
			t.Fatalf("stamp the slot claim of %s: %v", id, err)
		}
		return stored.ID
	}
	makeLegacy := func(id string) {
		t.Helper()
		if _, err := rawState(t, dir).Exec(`UPDATE tasks SET slot_owner_pid = 0, slot_owner_start = 0 WHERE id = ?`, id); err != nil {
			t.Fatalf("clear the owner identity of %s: %v", id, err)
		}
	}

	liveExpired := claim("t-live-expired", now.Add(-2*lease)) // owned by this process, which is alive
	deadDue := claim("t-dead-due", now.Add(-2*lease))
	s.MarkTaskComplete(owner, deadDue) // this process's claim on the task is fenced: dead to peers
	deadYoung := claim("t-dead-young", now)
	s.MarkTaskComplete(owner, deadYoung)
	legacyYoung := claim("t-legacy-young", now)
	makeLegacy(legacyYoung)
	legacyDue := claim("t-legacy-due", now.Add(-2*lease))
	makeLegacy(legacyDue)
	claimless := claim("t-claimless", now.Add(-2*lease))
	if _, err := rawState(t, dir).Exec(`UPDATE tasks SET slot_claimed_at = 0, slot_owner_pid = 0, slot_owner_start = 0 WHERE id = ?`,
		claimless); err != nil {
		t.Fatalf("release the claim of %s: %v", claimless, err)
	}

	claims, err := s.RecoveryClaims(ctx, lease)
	if err != nil {
		t.Fatalf("RecoveryClaims: %v", err)
	}
	byID := map[string]state.RecoveryClaim{}
	for _, c := range claims {
		byID[c.ID] = c
	}
	want := map[string]struct{ live, dueNow bool }{
		liveExpired: {live: true, dueNow: true},
		deadDue:     {live: false, dueNow: true},
		deadYoung:   {live: false, dueNow: false},
		legacyYoung: {live: false, dueNow: false},
		legacyDue:   {live: false, dueNow: true},
	}
	now = time.Now()
	for id, w := range want {
		c, ok := byID[id]
		if !ok {
			t.Errorf("RecoveryClaims omits %s, want it reported", id)
			continue
		}
		if c.Live != w.live {
			t.Errorf("RecoveryClaims reports %s live = %v, want %v", id, c.Live, w.live)
		}
		if dueNow := !c.Due.After(now); dueNow != w.dueNow {
			t.Errorf("RecoveryClaims reports %s due now = %v (due %v), want %v", id, dueNow, c.Due, w.dueNow)
		}
	}
	if _, ok := byID[claimless]; ok {
		t.Errorf("RecoveryClaims reports %s, which holds no claim", claimless)
	}
	if len(claims) != len(want) {
		t.Errorf("RecoveryClaims reports %d claims, want %d", len(claims), len(want))
	}
}
