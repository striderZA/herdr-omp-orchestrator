package coord

import (
	"context"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// The tests in this file drive the empty-scope worker a startup route arms, through
// RecoverUntilIdle. A stranded step is the trigger: it makes a pass run, and each test
// checks what that pass was allowed to touch.

// failAgentStarts makes the next agent starts fail; healAgentStarts lets them succeed.
func failAgentStarts(f *coordFixture) {
	f.updatePlan(func(plan *fixturePlan) { plan.AgentStartError = "agent_start_failed" })
}

func healAgentStarts(f *coordFixture) {
	f.updatePlan(func(plan *fixturePlan) { plan.AgentStartError = "" })
}

// TestBackgroundRecoveryLeavesUnrelatedCleanupToStartup pins a background pass that
// settled an interrupted cleanup of an unrelated task. Only startup settles those.
func TestBackgroundRecoveryLeavesUnrelatedCleanupToStartup(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	closed := f.recordDisposition(f.satisfyReview(worker), "integrated")
	closed = f.block(closed, func(next *task.Task) {
		next.Cleanup = &task.CleanupAttempt{
			Path: closed.WorktreePath, Branch: closed.Branch, WorkspaceID: closed.WorktreeWorkspaceID,
		}
	})
	if out, err := runGit(f.root, "worktree", "remove", "--", closed.WorktreePath); err != nil {
		t.Fatalf("remove the checkout before task settlement: %v: %s", err, out)
	}
	session := f.state()
	remaining := session.Worktrees[:0]
	for _, wt := range session.Worktrees {
		if !samePath(wt.Path, closed.WorktreePath) {
			remaining = append(remaining, wt)
		}
	}
	session.Worktrees = remaining
	session.save(f.dir)

	failAgentStarts(f)
	strandDispatch(t, f, "stranded")
	healAgentStarts(f)

	if returned, err := runRecoveryFor(10*time.Second, recoveryWorker(t, f)); !returned || err != nil {
		t.Fatalf("background recovery = (returned %v, %v), want the stranded step resumed", returned, err)
	}
	if after := f.taskByID(t, closed.ID); after.Cleanup == nil {
		t.Fatal("a background pass settled an unrelated interrupted cleanup; only startup may")
	}
	if err := f.coord.Reconcile(ctx); err != nil {
		t.Fatalf("startup Reconcile: %v", err)
	}
	if after := f.taskByID(t, closed.ID); after.Cleanup != nil {
		t.Fatalf("startup left the interrupted cleanup unsettled: %+v", after.Cleanup)
	}
}

// TestBackgroundRecoveryDoesNotJudgeALiveClaim pins a background pass that judged a
// task a live process still provisions. Its pane contradicts the record, but the live
// route owns that judgement.
func TestBackgroundRecoveryDoesNotJudgeALiveClaim(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	live, err := f.coord.Dispatch(ctx, f.projectKey(), "live", "brief live",
		[]string{"go test ./... ok"}, nil, nil, f.base)
	if err == nil {
		t.Fatal("dispatch succeeded, want the refused agent start to leave the step in flight")
	}
	before := f.taskByID(t, live.ID)
	if before.Phase != task.WorktreeCreated || before.LaunchPaneID == "" {
		t.Fatalf("live task = phase %q pane %q, want a recorded worktree step in a pane",
			before.Phase, before.LaunchPaneID)
	}
	f.rewriteState(func(current *fixtureState) {
		current.Agents[before.LaunchPaneID] = &fixtureAgent{
			Name:     "started-by-the-live-route",
			Terminal: "term-live-route",
			Session:  "someone-elses-session",
			Status:   "idle",
		}
	})
	noted := attentionIDs(t, f, live.ID)

	strandDispatch(t, f, "stranded")
	healAgentStarts(f)
	// The live claim keeps the worker polling after the stranded step is resolved, so
	// the run is bounded and the state it left is what is inspected.
	runRecoveryFor(3*time.Second, recoveryWorker(t, f))

	if fresh := newAttentionIDs(noted, attentionIDs(t, f, live.ID)); len(fresh) != 0 {
		t.Fatalf("background recovery wrote %d attention note(s) on a task a live process still owns", len(fresh))
	}
	if after := f.taskByID(t, live.ID); after.Attention != before.Attention || after.Blocker != before.Blocker {
		t.Fatalf("background recovery changed the live task: attention %q -> %q, blocker %q -> %q",
			before.Attention, after.Attention, before.Blocker, after.Blocker)
	}
}

// TestBackgroundRecoveryDoesNotJudgeAnOpenWorkerWithoutADueClaim pins a background
// pass that judged an open worker whose dispatching route is gone. Its pane contradicts
// the record. Judging that pane is startup's job; background recovery holds no claim
// on the worker, so it must not decide anything about it.
func TestBackgroundRecoveryDoesNotJudgeAnOpenWorkerWithoutADueClaim(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	if worker.Blocker != "" || worker.Disposition != "" ||
		(worker.Phase != task.PromptSubmitted && worker.Phase != task.AgentStarted) {
		t.Fatalf("precondition: the worker must be open and provisioned, got phase %q blocker %q disposition %q",
			worker.Phase, worker.Blocker, worker.Disposition)
	}
	owner, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	f.store.MarkTaskComplete(owner, worker.ID) // its dispatching route is gone
	oldPane := worker.PublicPaneID
	f.rewriteState(func(current *fixtureState) {
		agent := current.Agents[oldPane]
		if agent == nil {
			t.Fatalf("no agent in %q", oldPane)
		}
		delete(current.Agents, oldPane)
		delete(current.Workspaces, workspaceOfPane(oldPane))
		agent.Session = "someone-elses-session"
		agent.Terminal = "term-someone-else"
		current.Workspaces["other"] = worker.WorktreePath
		current.Agents["other:p1"] = agent
	})
	noted := attentionIDs(t, f, worker.ID)

	failAgentStarts(f)
	strandDispatch(t, f, "stranded")
	healAgentStarts(f)
	runRecoveryFor(10*time.Second, recoveryWorker(t, f))

	if fresh := newAttentionIDs(noted, attentionIDs(t, f, worker.ID)); len(fresh) != 0 {
		t.Fatalf("background recovery wrote %d attention note(s) on an open worker with no due claim", len(fresh))
	}
	if after := f.taskByID(t, worker.ID); after.Attention != worker.Attention || after.Blocker != worker.Blocker {
		t.Fatalf("background recovery changed the open worker: attention %q -> %q, blocker %q -> %q",
			worker.Attention, after.Attention, worker.Blocker, after.Blocker)
	}
}

// TestBackgroundRecoveryLeavesAnInFlightInboxDeliveryToStartup pins a background pass
// that marked a live delivery uncertain. Only startup recovers in-flight deliveries.
func TestBackgroundRecoveryLeavesAnInFlightInboxDeliveryToStartup(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	stranded := strandDispatch(t, f, "stranded")
	note := f.coord.noteFor(stranded, "progress", "halfway", "worker progress")
	if _, err := f.store.AddInbox(ctx, note); err != nil {
		t.Fatalf("AddInbox: %v", err)
	}
	if claimed, err := f.store.ClaimInbox(ctx, []string{note.ID}); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimInbox = %d, err %v", len(claimed), err)
	}
	healAgentStarts(f)

	if returned, err := runRecoveryFor(10*time.Second, recoveryWorker(t, f)); !returned || err != nil {
		t.Fatalf("background recovery = (returned %v, %v), want the stranded step resumed", returned, err)
	}
	if state := f.noteState(t, note.ID); state != task.DeliverySending {
		t.Fatalf("background recovery left the live delivery in %q, want %q", state, task.DeliverySending)
	}
	if err := f.coord.Reconcile(ctx); err != nil {
		t.Fatalf("startup Reconcile: %v", err)
	}
	if state := f.noteState(t, note.ID); state != task.DeliveryUncertain {
		t.Fatalf("startup left the interrupted delivery in %q, want %q", state, task.DeliveryUncertain)
	}
}

// TestBackgroundRecoveryLeavesQueuedWorkWaitingForRunningCapacity pins the capacity rule
// a background pass must respect. The steps it resumes become running workers, and a
// running worker keeps its slot until a review disposition frees it, so the queued task
// stays queued and no checkout is created for it.
func TestBackgroundRecoveryLeavesQueuedWorkWaitingForRunningCapacity(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	var stranded []task.Task
	for _, title := range []string{"stranded-a", "stranded-b", "stranded-c"} {
		stranded = append(stranded, strandDispatch(t, f, title))
	}
	queued, err := f.dispatch("queued", "brief queued", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch queued: %v", err)
	}
	if queued.Phase != task.Intent {
		t.Fatalf("queued phase = %q, want %q: the three stranded steps hold the cap", queued.Phase, task.Intent)
	}
	worktrees := f.countCalls("worktree create")
	healAgentStarts(f)

	if returned, err := runRecoveryFor(15*time.Second, recoveryWorker(t, f)); !returned || err != nil {
		t.Fatalf("background recovery = (returned %v, %v), want the stranded steps resumed and the worker idle", returned, err)
	}
	for _, st := range stranded {
		if got := f.taskByID(t, st.ID); got.Phase != task.PromptSubmitted {
			t.Fatalf("stranded task %s phase = %q, want %q: recovery did not resume it", st.ID, got.Phase, task.PromptSubmitted)
		}
	}
	if got := f.taskByID(t, queued.ID); got.Phase != task.Intent {
		t.Fatalf("background recovery admitted queued task %s to phase %q over capacity held by running workers",
			queued.ID, got.Phase)
	}
	if got := f.countCalls("worktree create"); got != worktrees {
		t.Fatalf("worktree create calls = %d, want %d: a checkout was made for queued work over full capacity", got, worktrees)
	}
}
