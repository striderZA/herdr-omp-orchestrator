package state_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

const (
	processIdentityHelperEnv  = "ORCH_PROCESS_IDENTITY_HELPER"
	processIdentityHelperFile = "ORCH_PROCESS_IDENTITY_FILE"
)

func TestProcessIdentityHelper(t *testing.T) {
	if os.Getenv(processIdentityHelperEnv) != "1" {
		return
	}
	identity, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	if err := os.WriteFile(os.Getenv(processIdentityHelperFile),
		[]byte(fmt.Sprintf("%d %d", identity.PID, identity.StartTime)), 0o600); err != nil {
		t.Fatalf("write process identity: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestProcessIdentityDistinguishesLiveExitedAndReusedPIDs(t *testing.T) {
	current, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	if current.PID == 0 || current.StartTime == 0 {
		t.Fatalf("current process identity = %+v, want a PID and creation time", current)
	}
	if alive, err := state.ProcessAlive(current); err != nil || !alive {
		t.Fatalf("ProcessAlive(current) = (%v, %v), want alive", alive, err)
	}
	otherStart := current
	otherStart.StartTime++
	if alive, err := state.ProcessAlive(otherStart); err != nil || alive {
		t.Fatalf("ProcessAlive(reused PID) = (%v, %v), want a different process", alive, err)
	}

	identityFile := filepath.Join(t.TempDir(), "child-identity")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessIdentityHelper$")
	cmd.Env = append(
		os.Environ(),
		processIdentityHelperEnv+"=1",
		processIdentityHelperFile+"="+identityFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start identity helper: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	var child state.ProcessIdentity
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(identityFile)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) != 2 {
				t.Fatalf("child identity text = %q, want PID and start time", data)
			}
			pid, err := strconv.ParseUint(fields[0], 10, 32)
			if err != nil {
				t.Fatalf("parse child PID: %v", err)
			}
			start, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				t.Fatalf("parse child start time: %v", err)
			}
			child = state.ProcessIdentity{PID: uint32(pid), StartTime: int64(start)}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child.PID == 0 {
		t.Fatal("identity helper did not report its process identity")
	}
	if alive, err := state.ProcessAlive(child); err != nil || !alive {
		t.Fatalf("ProcessAlive(child) = (%v, %v), want alive", alive, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill identity helper: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		// A forced stop is expected; the process handle must still be reaped.
		if !strings.Contains(strings.ToLower(err.Error()), "exit status") {
			t.Fatalf("wait for identity helper: %v", err)
		}
	}
	if alive, err := state.ProcessAlive(child); err != nil || alive {
		t.Fatalf("ProcessAlive(exited child) = (%v, %v), want not alive", alive, err)
	}
}

func TestStoreClaimStepDoesNotEnterAResourceTaskOwnedByAnotherLiveProcessBetweenSteps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	identityFile := filepath.Join(dir, "live-owner-identity")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessIdentityHelper$")
	cmd.Env = append(
		os.Environ(),
		processIdentityHelperEnv+"=1",
		processIdentityHelperFile+"="+identityFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start live task owner: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	var owner state.ProcessIdentity
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(identityFile); err == nil {
			fields := strings.Fields(string(data))
			if len(fields) != 2 {
				t.Fatalf("owner identity text = %q, want PID and start time", data)
			}
			pid, err := strconv.ParseUint(fields[0], 10, 32)
			if err != nil {
				t.Fatalf("parse owner PID: %v", err)
			}
			start, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatalf("parse owner start time: %v", err)
			}
			owner = state.ProcessIdentity{PID: uint32(pid), StartTime: start}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if owner.PID == 0 {
		t.Fatal("live owner did not report its process identity")
	}

	s := open(t, dir)
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t-between-steps-live", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	if _, err := s.UpdateTask(ctx, stored.ID, stored.Revision, func(current *task.Task) error {
		current.Phase = task.WorktreeCreated
		return nil
	}); err != nil {
		t.Fatalf("advance task to worktree_created: %v", err)
	}
	if _, err := rawState(t, dir).Exec(`UPDATE tasks
		SET slot_owner_pid = ?, slot_owner_start = ?, slot_claimed_at = ?
		WHERE id = ?`, int64(owner.PID), owner.StartTime, time.Now().Add(-time.Minute).UnixMilli(), stored.ID); err != nil {
		t.Fatalf("record the live resource owner: %v", err)
	}
	if took, err := s.ClaimStep(ctx, stored.ID, task.WorktreeCreated, "other-operation", time.Second); err != nil || took {
		t.Fatalf("ClaimStep while another live process owns the slot = (%v, %v), want it refused", took, err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("stop live task owner: %v", err)
	}
	if err := cmd.Wait(); err != nil && !strings.Contains(strings.ToLower(err.Error()), "exit status") {
		t.Fatalf("wait for stopped task owner: %v", err)
	}
	if took, err := s.ClaimStep(ctx, stored.ID, task.WorktreeCreated, "other-operation", time.Second); err != nil || !took {
		t.Fatalf("ClaimStep after the owner exited and lease elapsed = (%v, %v), want takeover", took, err)
	}
}

func TestStoreNewStepClaimClearsPriorRouteCompletion(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "t-reused-owner", "repo-a", nil), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v), want a slot", claimed, err)
	}
	identity, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	if took, err := s.ClaimStep(ctx, stored.ID, task.Intent, "route-one", 20*time.Millisecond); err != nil || !took {
		t.Fatalf("first ClaimStep = (%v, %v), want the claim", took, err)
	}
	s.MarkTaskComplete(identity, stored.ID)
	time.Sleep(40 * time.Millisecond)
	if took, err := s.ClaimStep(ctx, stored.ID, task.Intent, "route-two", 20*time.Millisecond); err != nil || !took {
		t.Fatalf("second route ClaimStep = (%v, %v), want takeover of the completed route", took, err)
	}
	time.Sleep(40 * time.Millisecond)
	if took, err := s.ClaimStep(ctx, stored.ID, task.Intent, "still-live-route", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after the second lease expired = (%v, %v), want the live process to retain its claim", took, err)
	}
}
