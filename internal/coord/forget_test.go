package coord

import (
	"context"
	"database/sql"
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
	f.boundProject(t)
	beforeTasks := f.projectTasks()
	beforeProject := f.boundProject(t)
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
	if err != nil {
		t.Fatalf("PlanForget: %v", err)
	}
	if got := f.projectTasks(); !reflect.DeepEqual(got, beforeTasks) {
		t.Fatalf("tasks changed: before %#v, after %#v", beforeTasks, got)
	}
	if got := f.boundProject(t); !reflect.DeepEqual(got, beforeProject) {
		t.Fatalf("project changed: before %#v, after %#v", beforeProject, got)
	}
	if f.countCalls("worktree remove") != 0 {
		t.Fatal("planning removed a worktree")
	}
	if plan.ProjectKey != f.projectKey() {
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
	f.settleForgetTask(t, st.ID)
	plan, err := f.coord.PlanForget(context.Background(), f.projectKey())
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
	if !strings.Contains(strings.Join(result.Stays, "\n"), "branches") ||
		!strings.Contains(strings.Join(result.Stays, "\n"), "pane:") {
		t.Fatalf("Stays = %q, want branches and pane details", result.Stays)
	}
	if _, err := f.coord.PlanForget(context.Background(), f.projectKey()); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("PlanForget after forget = %v, want ErrProjectUnknown", err)
	}
	if f.countCalls("worktree remove") != 0 {
		t.Fatal("forget removed a Herdr worktree")
	}
	if _, err := os.Stat(f.root); err != nil {
		t.Fatalf("repository directory removed: %v", err)
	}
	if out, err := runGit(f.root, "show-ref", "--verify", "refs/heads/main"); err != nil {
		t.Fatalf("branch missing: %v: %s", err, out)
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
	if _, err := f.store.UpdateTask(context.Background(), id, st.Revision, func(next *task.Task) error {
		next.Phase = task.Intent
		next.WorktreePath = ""
		next.Branch = ""
		next.WorktreeWorkspaceID = ""
		next.LaunchPaneID = ""
		next.PublicPaneID = ""
		next.AgentName = ""
		next.OMPSessionID = ""
		next.Outcome = task.OutcomeAbandoned
		next.Disposition = "settled before forget"
		return nil
	}); err != nil {
		t.Fatalf("settle task %s: %v", id, err)
	}
	token := f.slotClaimToken(id)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		t.Fatalf("open store database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), `UPDATE tasks SET slot_claimed_at = 0, slot_owner_pid = 0, slot_owner_start = 0, step_claim_owner = '', step_claim_phase = '', step_claim_until = 0, step_claim_pid = 0, step_claim_start = 0 WHERE id = ?`, id); err != nil {
		t.Fatalf("clear completed task claims for %s (token %d): %v", id, token, err)
	}
}
