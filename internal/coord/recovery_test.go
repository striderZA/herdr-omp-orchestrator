package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// declinedPhrase is the part of the attention a declined provisioning claim records
// that every such note carries. Tests match the phrase and the task the note names.
const declinedPhrase = "recovery left the recorded step unresumed"

// testRecoveryPace makes a worker that cannot advance a claim stop in milliseconds
// rather than after the production minute.
var testRecoveryPace = recoveryPace{first: 5 * time.Millisecond, max: 20 * time.Millisecond, limit: 3}

// recoveryWorker is the coordinator a detached worker builds: a fresh process over
// the same store, with the short lease the stranded claims carry.
func recoveryWorker(t *testing.T, f *coordFixture) *Coordinator {
	t.Helper()
	worker, err := New(f.store, f.client, Options{StepLease: 750 * time.Millisecond})
	if err != nil {
		t.Fatalf("New (recovery worker): %v", err)
	}
	worker.pace = testRecoveryPace
	return worker
}

// strandDispatch leaves one provisioning step the way a crash does: the dispatch's
// agent start is refused, so the task stays at worktree_created holding its claims.
// ClaimStep records this process's identity, so the step claim is taken on a short
// lease and then fenced with MarkTaskComplete, the in-memory fence a finished route
// leaves. The fence makes the claims dead to this process; no process is killed.
func strandDispatch(t *testing.T, f *coordFixture, title string) task.Task {
	t.Helper()
	ctx := context.Background()
	stranded, err := f.dispatch(title, "brief "+title, []string{"go test ./... ok"}, nil, nil, f.base)
	if err == nil {
		t.Fatalf("dispatch %s succeeded, want the refused agent start to stop the step", title)
	}
	before := f.taskByID(t, stranded.ID)
	if before.Phase != task.WorktreeCreated || before.WorktreePath == "" {
		t.Fatalf("task %s = phase %q worktree %q, want a recorded worktree step",
			title, before.Phase, before.WorktreePath)
	}
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	if took, err := f.store.ClaimStep(ctx, stranded.ID, task.WorktreeCreated, "stopped-owner", 750*time.Millisecond); err != nil || !took {
		t.Fatalf("the stopped owner's step claim = (%v, %v), want it taken", took, err)
	}
	f.store.MarkTaskComplete(owner, stranded.ID)
	return before
}

// stripPanes removes a task's recorded panes from Herdr's state, so no snapshot
// holds them and no pass can prove the recorded step.
func stripPanes(t *testing.T, f *coordFixture, st task.Task) {
	t.Helper()
	f.rewriteState(func(current *fixtureState) {
		for _, pane := range []string{st.LaunchPaneID, st.PublicPaneID} {
			if pane == "" {
				continue
			}
			delete(current.Agents, pane)
			delete(current.Workspaces, workspaceOfPane(pane))
		}
	})
}

// runRecoveryFor runs one worker over taskIDs for at most d. A worker still running
// then is cancelled and waited for, so a test reads the state the worker left.
// returned reports whether the worker stopped on its own; err is its result if so.
func runRecoveryFor(d time.Duration, worker *Coordinator, taskIDs ...string) (bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.RecoverUntilIdle(ctx, taskIDs...) }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case err := <-done:
		return true, err
	case <-timer.C:
		cancel()
		<-done
		return false, nil
	}
}

// declinedNotes returns the attention notes that record a declined claim of taskID.
func declinedNotes(t *testing.T, f *coordFixture, taskID string) []task.Inbox {
	t.Helper()
	var out []task.Inbox
	for _, note := range f.notesOfKind(t, noteKindAttention) {
		if note.TaskID == taskID && strings.Contains(note.Summary, declinedPhrase) {
			out = append(out, note)
		}
	}
	return out
}

// attentionIDs returns the IDs of every attention note that names taskID.
func attentionIDs(t *testing.T, f *coordFixture, taskID string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, note := range f.notesOfKind(t, noteKindAttention) {
		if note.TaskID == taskID {
			ids[note.ID] = true
		}
	}
	return ids
}

// newAttentionIDs reports the attention note IDs in after that before does not have.
func newAttentionIDs(before, after map[string]bool) []string {
	var fresh []string
	for id := range after {
		if !before[id] {
			fresh = append(fresh, id)
		}
	}
	return fresh
}

// TestRecoveryStopsWhenNoPassCanAdvanceAClaim pins a worker that keeps passing for a
// claim no pass can prove. The task's panes are gone, so the worker must record why it
// left the step unresumed, start nothing, and stop after exactly its stall budget.
func TestRecoveryStopsWhenNoPassCanAdvanceAClaim(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	primary := f.boundProject(t).PublicPaneID
	f.setAgentStatus(t, primary, "working")
	stranded := strandDispatch(t, f, "stranded")
	stripPanes(t, f, stranded)
	starts := f.countCalls("agent start")
	if got := len(f.promptsTo(t, primary)); got != 0 {
		t.Fatalf("the foreground refusal prompted the working primary %d times", got)
	}
	f.setAgentStatus(t, primary, "idle")

	returned, err := runRecoveryFor(10*time.Second, recoveryWorker(t, f), stranded.ID)
	if !returned {
		t.Fatal("RecoverUntilIdle kept running: a claim no pass can advance kept the worker looping")
	}
	if !errors.Is(err, ErrRecoveryStalled) {
		t.Fatalf("RecoverUntilIdle = %v, want ErrRecoveryStalled", err)
	}
	var stall *recoveryStall
	if !errors.As(err, &stall) || stall.passes != testRecoveryPace.limit {
		t.Fatalf("the stop = %v, want it after exactly %d stalled passes", err, testRecoveryPace.limit)
	}
	if got := f.countCalls("agent start"); got != starts {
		t.Fatalf("agent start calls = %d, want %d: recovery started a step no pane proved", got, starts)
	}
	notes := declinedNotes(t, f, stranded.ID)
	if len(notes) != 1 {
		t.Fatalf("declined-claim notes for task %s = %d, want 1", stranded.ID, len(notes))
	}
	if state := f.noteState(t, notes[0].ID); state != task.DeliveryPending {
		t.Fatalf("background recovery note state = %q, want %q", state, task.DeliveryPending)
	}
	if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
		t.Fatalf("background recovery flushed inbox notes to the primary %d times, want 0", len(prompts))
	}
}

// TestStartupRecoveryStopsWhenNoPassCanAdvanceAClaim is the same decline through the
// empty scope a startup route arms: the same attention is recorded and the worker
// stops.
func TestStartupRecoveryStopsWhenNoPassCanAdvanceAClaim(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	stranded := strandDispatch(t, f, "stranded")
	stripPanes(t, f, stranded)

	returned, err := runRecoveryFor(10*time.Second, recoveryWorker(t, f))
	if !returned {
		t.Fatal("RecoverUntilIdle (startup scope) kept running on a claim no pass can advance")
	}
	if !errors.Is(err, ErrRecoveryStalled) {
		t.Fatalf("RecoverUntilIdle (startup scope) = %v, want ErrRecoveryStalled", err)
	}
	if len(declinedNotes(t, f, stranded.ID)) == 0 {
		t.Fatalf("the startup-scope worker stopped without recording why task %s is unresumed", stranded.ID)
	}
}

// TestRecoveryResumesEveryClaimInScopeWhileOneIsRefused pins a scope that stopped at
// its first refused claim: a resumable claim listed after it was never continued.
func TestRecoveryResumesEveryClaimInScopeWhileOneIsRefused(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	refused := strandDispatch(t, f, "refused")
	resumable := strandDispatch(t, f, "resumable")
	// Herdr no longer lists the refused task's checkout, so its pass is refused before
	// any step runs. The resumable task's checkout is still listed.
	f.rewriteState(func(current *fixtureState) {
		kept := current.Worktrees[:0]
		for _, wt := range current.Worktrees {
			if !samePath(wt.Path, refused.WorktreePath) {
				kept = append(kept, wt)
			}
		}
		current.Worktrees = kept
	})
	f.updatePlan(func(plan *fixturePlan) { plan.AgentStartError = "" })
	starts := f.countCalls("agent start")

	returned, err := runRecoveryFor(10*time.Second, recoveryWorker(t, f), refused.ID, resumable.ID)
	if got := f.taskByID(t, resumable.ID); got.Phase == task.WorktreeCreated {
		t.Fatal("resumable task still at worktree_created: the refused claim before it starved the scope")
	}
	if got := f.countCalls("agent start"); got != starts+1 {
		t.Fatalf("agent start calls = %d, want %d: only the resumable task may start an agent", got, starts+1)
	}
	if !returned || !errors.Is(err, ErrRecoveryStalled) {
		t.Fatalf("RecoverUntilIdle = (returned %v, %v), want ErrRecoveryStalled while the refused claim stays", returned, err)
	}
}

// TestRecoveryDoesNotWaitOnAClaimItsOwnPassLeft pins that a worker never waits on a claim its
// own pass left under its own identity. strandDispatch leaves the claim with a dead owner: its
// fence makes the claim due at its lease end and not live. The worker then re-acquires the claim,
// which retires that fence, and its refused start leaves the slot under the worker's identity.
// A live owner is never taken over, so waiting on that slot would never end. The worker must
// fence what its own pass left, retry it once per declined pass, and then stop with the stall.
func TestRecoveryDoesNotWaitOnAClaimItsOwnPassLeft(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	stranded := strandDispatch(t, f, "own pass")
	worker := recoveryWorker(t, f)

	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := worker.RecoverUntilIdle(runCtx, stranded.ID)
	if runCtx.Err() != nil {
		t.Fatalf("RecoverUntilIdle waited on a claim its own pass left: %v", err)
	}
	if !errors.Is(err, ErrRecoveryStalled) {
		t.Fatalf("RecoverUntilIdle = %v, want ErrRecoveryStalled once the refused claim is exhausted", err)
	}
	// The dispatch made one start, and the worker made one more for each declined pass.
	if got := f.countCalls("agent start"); got != 1+testRecoveryPace.limit {
		t.Fatalf("agent start calls = %d, want %d: one start per declined pass after the dispatch", got, 1+testRecoveryPace.limit)
	}
	after := f.taskByID(t, stranded.ID)
	if after.Blocker != "" || after.Attention == "" {
		t.Fatalf("task blocker %q attention %q, want the refused start recorded as attention and no blocker", after.Blocker, after.Attention)
	}
	claims, err := f.store.RecoveryClaims(ctx, 750*time.Millisecond, stranded.ID)
	if err != nil || len(claims) != 1 || claims[0].Live {
		t.Fatalf("after the stall the claim = (%+v, %v), want one recorded claim with no live owner", claims, err)
	}
}
