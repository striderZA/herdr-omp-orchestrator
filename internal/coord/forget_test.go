package coord

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestPlanForgetIsReadOnly(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	project := f.boundProject(t)
	f.mustAddForgetTicket(t, "ticket-readonly")
	f.mustAddForgetNote(t, "note-readonly", task.DeliveryPending)
	beforeTasks := f.projectTasks()
	beforeProject := f.boundProject(t)
	beforeCalls := f.calls()
	beforeTicket, err := f.store.Ticket(context.Background(), "ticket-readonly")
	if err != nil {
		t.Fatal(err)
	}
	beforeNote, err := f.store.Inbox(context.Background(), "note-readonly")
	if err != nil {
		t.Fatal(err)
	}

	plan, err := f.coord.PlanForget(context.Background(), project.Key)
	if err != nil {
		t.Fatalf("PlanForget: %v", err)
	}
	if got := f.projectTasks(); !reflect.DeepEqual(got, beforeTasks) {
		t.Fatalf("tasks changed: before %#v, after %#v", beforeTasks, got)
	}
	if got := f.boundProject(t); !reflect.DeepEqual(got, beforeProject) {
		t.Fatalf("project changed: before %#v, after %#v", beforeProject, got)
	}
	if got := f.calls(); !reflect.DeepEqual(got, beforeCalls) {
		t.Fatalf("Herdr calls changed: before %#v, after %#v", beforeCalls, got)
	}
	afterTicket, err := f.store.Ticket(context.Background(), "ticket-readonly")
	if err != nil {
		t.Fatal(err)
	}
	afterNote, err := f.store.Inbox(context.Background(), "note-readonly")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterTicket, beforeTicket) {
		t.Fatalf("ticket changed: before %#v, after %#v", beforeTicket, afterTicket)
	}
	if !reflect.DeepEqual(afterNote, beforeNote) {
		t.Fatalf("inbox note changed: before %#v, after %#v", beforeNote, afterNote)
	}
	if f.countCalls("worktree remove") != 0 {
		t.Fatal("planning removed a worktree")
	}
	if plan.ProjectKey != project.Key {
		t.Fatalf("ProjectKey = %q", plan.ProjectKey)
	}
}

func TestPlanForgetNamesWhatBlocks(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.mustDispatch("undecided", "work")
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Blockers, "\n"), st.ID) {
		t.Fatalf("Blockers %q do not name %s", plan.Blockers, st.ID)
	}
	f.settleForgetTask(t, st.ID)
	plan, err = f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("Blockers = %q, want none", plan.Blockers)
	}
}

func TestForgetRefusesAnUndecidedTask(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.mustDispatch("undecided", "work")
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.coord.Forget(context.Background(), f.projectKey(), plan)
	if !errors.Is(err, ErrForgetRefused) || !strings.Contains(err.Error(), st.ID) {
		t.Fatalf("Forget err = %v, want ErrForgetRefused naming %s", err, st.ID)
	}
}

func TestForgetRefusalAppendsPrimaryReattachHint(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.mustDispatch("undecided", "work")
	f.setPrimaryAttention(t, "primary unavailable")
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.coord.Forget(context.Background(), f.projectKey(), plan)
	if !errors.Is(err, ErrForgetRefused) {
		t.Fatalf("Forget err = %v, want ErrForgetRefused", err)
	}
	parts := strings.Split(err.Error(), "; ")
	if len(parts) < 2 || !strings.Contains(parts[len(parts)-1], "reattach") || !strings.Contains(err.Error(), st.ID) {
		t.Fatalf("Forget err = %v, want task %s and final reattach hint", err, st.ID)
	}
}

func TestPlanForgetCountsAndFingerprint(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	projectKey := f.projectKey()
	decided := f.mustDispatch("decided", "work")
	f.settleForgetTask(t, decided.ID)
	f.mustDispatch("undecided", "work")
	f.mustAddForgetTicket(t, "ticket-counted")
	for _, item := range []struct{ id, state string }{
		{"note-pending", task.DeliveryPending},
		{"note-sending", task.DeliverySending},
		{"note-delivered", task.DeliveryDelivered},
		{"note-uncertain", task.DeliveryUncertain},
	} {
		f.mustAddForgetNote(t, item.id, item.state)
	}

	plan, err := f.coord.PlanForget(context.Background(), projectKey)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FolderName != filepath.Base(projectKey) {
		t.Errorf("FolderName = %q, want %q", plan.FolderName, filepath.Base(projectKey))
	}
	if plan.Tasks != 2 || plan.Decided != 1 {
		t.Errorf("tasks = %d, decided = %d, want 2 and 1", plan.Tasks, plan.Decided)
	}
	if plan.Tickets != 1 {
		t.Errorf("Tickets = %d, want 1", plan.Tickets)
	}
	if plan.NotesPending != 1 || plan.NotesDelivered != 1 || plan.NotesBlocking != 2 {
		t.Errorf("note counts = delivered %d, pending %d, blocking %d; want 1, 1, 2", plan.NotesDelivered, plan.NotesPending, plan.NotesBlocking)
	}
	if plan.Fingerprint == "" {
		t.Error("Fingerprint is empty")
	}
}

func TestForgetRefusesAStalePlan(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	f.projectKey()
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	f.mustDispatch("new task", "work")
	_, err = f.coord.Forget(context.Background(), f.projectKey(), plan)
	if !errors.Is(err, ErrForgetPlanChanged) {
		t.Fatalf("Forget err = %v, want ErrForgetPlanChanged", err)
	}
	if _, err := f.store.Project(context.Background(), f.projectKey()); err != nil {
		t.Fatalf("project did not survive: %v", err)
	}
}

func TestForgetRejectsAPlanForAnotherProject(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	projectA := f.boundProject(t)
	planA, err := f.coord.PlanForget(context.Background(), projectA.Key)
	if err != nil {
		t.Fatal(err)
	}
	projectB := filepath.Join(filepath.Dir(projectA.Key), "other-project")
	if err := f.store.CreateProject(context.Background(), task.Project{Key: projectB, LaunchPaneID: "w9:p9", PublicPaneID: "w9:p9"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.coord.Forget(context.Background(), projectB, planA)
	if !errors.Is(err, ErrForgetPlanChanged) || !strings.Contains(err.Error(), "key") || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("Forget with project A plan for B = %v, want ErrForgetPlanChanged key-differs error", err)
	}
	if _, err := f.store.Project(context.Background(), projectB); err != nil {
		t.Fatalf("project B did not survive: %v", err)
	}
}

func TestForgetNeedsTheConfirmedPlan(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	_, err := f.coord.Forget(context.Background(), f.projectKey(), ForgetPlan{})
	if !errors.Is(err, ErrForgetNotConfirmed) {
		t.Fatalf("Forget err = %v, want ErrForgetNotConfirmed", err)
	}
}

func TestForgetDeletesTheRecordsAndLeavesGitAndHerdrAlone(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	f.projectKey()
	st := f.mustDispatch("done", "work")
	branch, worktree := st.Branch, st.WorktreePath
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	if !containsForgetLine(plan.Stays, "branch: "+branch) || !containsForgetLine(plan.Stays, "worktree: "+worktree) {
		t.Fatalf("Stays = %q, want task branch %q and worktree %q", plan.Stays, branch, worktree)
	}
	f.settleForgetTask(t, st.ID)
	plan, err = f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.coord.Forget(context.Background(), f.projectKey(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted.Tasks != 1 {
		t.Fatalf("Deleted.Tasks = %d", result.Deleted.Tasks)
	}
	if _, err := f.coord.PlanForget(context.Background(), f.projectKey()); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("PlanForget after forget = %v, want ErrProjectUnknown", err)
	}
	if f.countCalls("worktree remove") != 0 {
		t.Fatal("forget removed a Herdr worktree")
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree path removed: %v", err)
	}
	if out, err := runGit(f.root, "show-ref", "--verify", "refs/heads/"+branch); err != nil {
		t.Fatalf("branch %q missing: %v: %s", branch, err, out)
	}
}

func TestPrimaryGoneHintIsAppendedToTheRefusal(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.mustDispatch("undecided", "work")
	f.setPrimaryAttention(t, "primary unavailable")
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) == 0 || !strings.Contains(plan.Blockers[len(plan.Blockers)-1], "reattach") {
		t.Fatalf("Blockers = %q, want final reattach hint (task %s)", plan.Blockers, st.ID)
	}
}

func TestEventForAForgottenPaneIsANoOp(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	project := f.boundProject(t)
	plan, err := f.coord.PlanForget(context.Background(), project.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.coord.Forget(context.Background(), project.Key, plan); err != nil {
		t.Fatal(err)
	}
	payload := eventFixture(t, "pane_agent_status_changed.json", "w2:p1", project.PublicPaneID, `"workspace_id": "w2"`, `"workspace_id": "w2"`)
	if err := f.coord.HandleEvent(context.Background(), EventAgentStatus, payload); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if _, err := f.store.Project(context.Background(), project.Key); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("project recreated: %v", err)
	}
}

func TestPlanForgetOfAnUnknownProjectIsProjectUnknown(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	_, err := f.coord.PlanForget(context.Background(), filepath.Join(f.root, "unbound"))
	if !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("PlanForget err = %v, want ErrProjectUnknown", err)
	}
}

func (f *coordFixture) settleForgetTask(t *testing.T, id string) {
	t.Helper()
	st := f.taskByID(t, id)
	updated, err := f.store.UpdateTask(context.Background(), id, st.Revision, func(next *task.Task) error {
		next.Phase = task.Intent
		next.WorktreePath = ""
		next.Branch = ""
		next.WorktreeWorkspaceID = ""
		next.LaunchPaneID = ""
		next.PublicPaneID = ""
		next.AgentName = ""
		next.OMPSessionID = ""
		return nil
	})
	if err != nil {
		t.Fatalf("settle phase for task %s: %v", id, err)
	}
	settled := f.recordDisposition(updated, "settled before forget")
	if err := f.store.ReleaseStep(context.Background(), id, f.coord.owner); err != nil {
		t.Fatalf("ReleaseStep(%s): %v", id, err)
	}
	if err := f.store.ReleaseProvisioningSlot(context.Background(), id, f.slotClaimToken(settled.ID)); err != nil {
		t.Fatalf("ReleaseProvisioningSlot(%s): %v", id, err)
	}
}

func (f *coordFixture) mustAddForgetTicket(t *testing.T, id string) {
	t.Helper()
	if err := f.store.CreateTicket(context.Background(), task.Ticket{ID: id, ProjectKey: f.projectKey(), Title: "ticket", Column: task.ColumnBacklog}); err != nil {
		t.Fatalf("CreateTicket(%s): %v", id, err)
	}
}

func (f *coordFixture) mustAddForgetNote(t *testing.T, id, deliveryState string) {
	t.Helper()
	added, err := f.store.AddInbox(context.Background(), task.Inbox{ID: id, ProjectKey: f.projectKey(), Kind: "test", Summary: "note"})
	if err != nil || !added {
		t.Fatalf("AddInbox(%s) = (%v, %v)", id, added, err)
	}
	if deliveryState == task.DeliveryPending {
		return
	}
	if _, err := f.store.ClaimInbox(context.Background(), []string{id}); err != nil {
		t.Fatalf("ClaimInbox(%s): %v", id, err)
	}
	if deliveryState == task.DeliverySending {
		return
	}
	if _, err := f.store.SettleInbox(context.Background(), []string{id}, deliveryState, "test settlement"); err != nil {
		t.Fatalf("SettleInbox(%s): %v", id, err)
	}
}

func containsForgetLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
