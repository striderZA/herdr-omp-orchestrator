package coord

// Issue #4: a repeated new dispatch must not create a second task for the same
// work. The scenario that makes it real is the extension's bridge_timeout (60 s)
// killing the bridge child while Dispatch is still provisioning (the executable
// itself allows 2 minutes): the task row was written at the intent phase before
// any provisioning, so the response is lost but the task exists, and a caller
// that repeats orch_dispatch sends the same parameters again.
//
// The tests stand in for the kill with a cancelled caller context. That is not
// identical (a killed process runs no cleanup, a cancelled context may record a
// failure), but the repeat never reads the first task's phase or blocker, so the
// difference cannot change the outcome.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/task"
)

const (
	repeatTitle = "Plan: parser"
	repeatBrief = "Investigate the parser and report a plan."
)

var repeatChecks = []string{"go test ./... ok"}

func (f *coordFixture) dispatchRepeat(reserved []string) (task.Task, error) {
	f.t.Helper()
	return f.coord.Dispatch(context.Background(), f.projectKey(), repeatTitle, repeatBrief, repeatChecks, nil, reserved, f.base)
}

func (f *coordFixture) projectTasks() []task.Task {
	f.t.Helper()
	tasks, err := f.store.Tasks(context.Background(), f.projectKey())
	if err != nil {
		f.t.Fatalf("read tasks: %v", err)
	}
	return tasks
}

// loseTheResponse starts a dispatch whose brief prompt hangs, waits until it is
// inside that call, then cancels the caller: the task row exists and the answer
// never arrives.
func loseTheResponse(t *testing.T, f *coordFixture, reserved []string) {
	t.Helper()
	projectKey := f.projectKey()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.coord.Dispatch(ctx, projectKey, repeatTitle, repeatBrief, repeatChecks, nil, reserved, f.base)
		done <- err
	}()
	waitForFixtureFile(t, filepath.Join(f.dir, "prompt_started"), 10*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the cancelled dispatch never returned")
	}
}

func TestRepeatAfterALostResponseReturnsTheExistingTaskAndTouchesNothing(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{PromptDelayMillis: 10_000})
	loseTheResponse(t, f, nil)
	first := f.projectTasks()
	if len(first) != 1 {
		t.Fatalf("after the lost response there are %d tasks, want 1 (persisted before provisioning)", len(first))
	}
	f.updatePlan(func(p *fixturePlan) { p.PromptDelayMillis = 0 })
	callsBefore := len(f.calls())

	// The caller never saw a task id, so it repeats the call unchanged.
	second, err := f.dispatchRepeat(nil)
	if err != nil {
		t.Fatalf("the repeated dispatch failed: %v", err)
	}
	if second.ID != first[0].ID {
		t.Fatalf("the repeat returned %s, want the existing task %s", second.ID, first[0].ID)
	}
	if got := len(f.projectTasks()); got != 1 {
		t.Fatalf("task count = %d, want 1", got)
	}
	if got := len(f.calls()); got != callsBefore {
		t.Errorf("the repeat made %d Herdr calls, want none", got-callsBefore)
	}
	if got := f.countCalls("worktree create"); got != 1 {
		t.Errorf("worktree create ran %d times, want once", got)
	}
	// The stalled task is reported as it stands, not repaired by the repeat.
	if second.Blocker == "" {
		t.Errorf("the existing task's blocker was dropped: %+v", second)
	}
}

func TestDispatchForMarksOnlyARepeatAsDuplicate(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	params := DispatchParams{Title: repeatTitle, Brief: repeatBrief, Checks: repeatChecks, BaseRevision: f.base}
	first, err := f.coord.DispatchFor(context.Background(), f.projectKey(), params)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	callsBefore := len(f.calls())
	second, err := f.coord.DispatchFor(context.Background(), f.projectKey(), params)
	if err != nil {
		t.Fatalf("repeated dispatch: %v", err)
	}

	if first.Duplicate {
		t.Error("a new task was marked as a duplicate")
	}
	if !second.Duplicate || second.TaskID != first.TaskID {
		t.Errorf("repeat = {duplicate:%v task:%s}, want a duplicate of %s", second.Duplicate, second.TaskID, first.TaskID)
	}
	if second.Phase != string(task.PromptSubmitted) || second.Stage != first.Stage {
		t.Errorf("the repeat reports phase %q stage %q, want the task as it stands (%q, %q)",
			second.Phase, second.Stage, task.PromptSubmitted, first.Stage)
	}
	if got := len(f.calls()); got != callsBefore {
		t.Errorf("the repeat made %d Herdr calls, want none", got-callsBefore)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if strings.Contains(string(firstJSON), "duplicate") {
		t.Errorf("a new task's answer carries a duplicate field: %s", firstJSON)
	}
	if !strings.Contains(string(secondJSON), `"duplicate":true`) {
		t.Errorf("a repeat's answer lacks \"duplicate\":true: %s", secondJSON)
	}
}

func TestRepeatWithReservedPathsReturnsTheOpenTaskNotAConflict(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	paths := []string{"src/main.txt"}
	first, err := f.dispatchRepeat(paths)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// Identical work holding the very same paths is a repeat, not a second worker
	// that wants the same files.
	again, err := f.dispatchRepeat(paths)
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeat = (%s, %v), want the open task %s", again.ID, err, first.ID)
	}
	// Different work on held paths is still the path guard's business.
	if _, err := f.coord.Dispatch(context.Background(), f.projectKey(), "another task", repeatBrief, repeatChecks, nil, paths, f.base); !errors.Is(err, ErrReservedConflict) {
		t.Fatalf("different work on held paths err = %v, want ErrReservedConflict", err)
	}
}

func TestSameWorkAfterTheTaskIsDecidedIsANewTask(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	first, err := f.dispatchRepeat(nil)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// A rejected task is exactly when a user dispatches the same work again.
	closed := f.satisfyReview(first)
	f.block(closed, func(next *task.Task) {
		next.Outcome = task.OutcomeRejected
		next.Disposition = "not what I wanted"
	})
	second, err := f.dispatchRepeat(nil)
	if err != nil {
		t.Fatalf("dispatch after the first task was decided: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("a decided task answered a new request for the same work")
	}
	if got := len(f.projectTasks()); got != 2 {
		t.Fatalf("task count = %d, want 2", got)
	}
}

func TestRepeatOfAQueuedTaskReturnsIt(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	for _, title := range []string{"one", "two", "three"} {
		f.mustDispatch(title, "brief "+title)
	}
	queued, err := f.dispatch("four", "brief four", repeatChecks, nil, nil, f.base)
	if err != nil {
		t.Fatalf("fourth dispatch: %v", err)
	}
	if task.StageOf(queued) != task.Queued {
		t.Fatalf("fourth task stage = %q, want queued", task.StageOf(queued))
	}
	callsBefore := len(f.calls())
	again, err := f.dispatch("four", "brief four", repeatChecks, nil, nil, f.base)
	if err != nil || again.ID != queued.ID {
		t.Fatalf("repeat = (%s, %v), want the queued task %s", again.ID, err, queued.ID)
	}
	if got := len(f.projectTasks()); got != 4 {
		t.Fatalf("task count = %d, want 4", got)
	}
	if got := len(f.calls()); got != callsBefore {
		t.Errorf("the repeat made %d Herdr calls, want none (not even a queue drain)", got-callsBefore)
	}
}

func TestConcurrentIdenticalDispatchesYieldOneTask(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	coordinators := []*Coordinator{f.coord, f.sibling()}
	projectKey := f.projectKey()

	const callers = 4
	type answer struct {
		id  string
		err error
	}
	answers := make(chan answer, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(c *Coordinator) {
			defer wg.Done()
			<-start
			st, err := c.Dispatch(context.Background(), projectKey, repeatTitle, repeatBrief, repeatChecks, nil, nil, f.base)
			answers <- answer{st.ID, err}
		}(coordinators[i%2])
	}
	close(start)
	wg.Wait()
	close(answers)

	ids := map[string]int{}
	for got := range answers {
		if got.err != nil {
			t.Errorf("a concurrent identical dispatch failed: %v", got.err)
		}
		ids[got.id]++
	}
	if len(ids) != 1 {
		t.Fatalf("callers were answered with %d different tasks %v, want one", len(ids), ids)
	}
	if got := len(f.projectTasks()); got != 1 {
		t.Fatalf("task count = %d, want 1", got)
	}
	if got := f.countCalls("worktree create"); got != 1 {
		t.Errorf("worktree create ran %d times, want once", got)
	}
}

func TestConcurrentDispatchReturnsDuplicateBeforeDrainingQueuedWork(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	var running []task.Task
	for _, title := range []string{"one", "two", "three"} {
		running = append(running, f.mustDispatch(title, "brief "+title))
	}
	queued := f.mustDispatch("four", "brief four")
	if task.StageOf(queued) != task.Queued {
		t.Fatalf("fourth task stage = %q, want queued", task.StageOf(queued))
	}
	f.block(running[0], func(next *task.Task) { next.Disposition = "integrated" })
	f.updatePlan(func(p *fixturePlan) { p.PromptDelayMillis = 1500 })

	projectKey := f.projectKey()
	params := DispatchParams{
		Title: "racing task", Brief: "same work", Checks: repeatChecks, BaseRevision: f.base,
	}
	type answer struct {
		result DispatchResult
		err    error
	}
	firstDone := make(chan answer, 1)
	go func() {
		result, err := f.coord.DispatchFor(context.Background(), projectKey, params)
		firstDone <- answer{result: result, err: err}
	}()

	waitForFixtureFile(t, filepath.Join(f.dir, "prompt_started"), 10*time.Second)
	callsBeforeRepeat := len(f.calls())
	repeat, repeatErr := f.coord.DispatchFor(context.Background(), projectKey, params)
	if repeatErr != nil {
		t.Errorf("concurrent repeat: %v", repeatErr)
	}
	if !repeat.Duplicate {
		t.Errorf("concurrent repeat = %+v, want the first caller's task with duplicate:true", repeat)
	}
	if got := len(f.calls()); got != callsBeforeRepeat {
		t.Errorf("concurrent repeat made %d Herdr calls while queued work was draining, want none", got-callsBeforeRepeat)
	}

	select {
	case first := <-firstDone:
		if first.err != nil {
			t.Fatalf("first dispatch: %v", first.err)
		}
		if first.result.Duplicate {
			t.Errorf("first dispatch was marked duplicate: %+v", first.result)
		}
		if first.result.TaskID != repeat.TaskID {
			t.Errorf("first task id = %s, repeat task id = %s", first.result.TaskID, repeat.TaskID)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first dispatch did not finish after the queued prompt delay")
	}
}

func TestRepeatNeverMatchesATaskOfAnotherProject(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	other := task.Task{
		ID: "orch_foreign000001", ProjectKey: "E:/another/repo",
		Title: repeatTitle, Brief: repeatBrief, Checks: repeatChecks,
		BaseRevision: f.base, Branch: "orch/foreign000001", Phase: task.Intent,
	}
	if err := f.store.CreateTask(context.Background(), other); err != nil {
		t.Fatalf("seed the other project's task: %v", err)
	}
	first, err := f.dispatchRepeat(nil)
	if err != nil || first.ID == other.ID {
		t.Fatalf("dispatch = (%s, %v), want a new task of this project, not %s", first.ID, err, other.ID)
	}
	again, err := f.dispatchRepeat(nil)
	if err != nil || again.ID != first.ID {
		t.Fatalf("repeat = (%s, %v), want this project's own task %s", again.ID, err, first.ID)
	}
}

func TestDifferentTicketIsDifferentWork(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	projectKey := f.projectKey()
	var tickets []string
	for _, title := range []string{"first ticket", "second ticket"} {
		view, err := f.coord.CreateTicketFor(context.Background(), projectKey, TicketCreateParams{Title: title, Body: "body"})
		if err != nil {
			t.Fatalf("create ticket %q: %v", title, err)
		}
		tickets = append(tickets, view.TicketID)
	}
	dispatch := func(ticketID string) DispatchResult {
		t.Helper()
		got, err := f.coord.DispatchFor(context.Background(), projectKey,
			DispatchParams{Title: repeatTitle, Brief: repeatBrief, Checks: repeatChecks, BaseRevision: f.base, TicketID: ticketID})
		if err != nil {
			t.Fatalf("dispatch for ticket %q: %v", ticketID, err)
		}
		return got
	}
	a, b := dispatch(tickets[0]), dispatch(tickets[1])
	if a.TaskID == b.TaskID || a.Duplicate || b.Duplicate {
		t.Fatalf("the same text for two tickets was merged: %+v / %+v", a, b)
	}
	if again := dispatch(tickets[0]); !again.Duplicate || again.TaskID != a.TaskID {
		t.Fatalf("a repeat for the first ticket = %+v, want a duplicate of %s", again, a.TaskID)
	}
	if unlinked := dispatch(""); unlinked.Duplicate {
		t.Fatalf("an unlinked request matched a ticket's task: %+v", unlinked)
	}
}

func TestRepeatDoesNotDrainTheQueueEvenWhenASlotIsFree(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	var running []task.Task
	for _, title := range []string{"one", "two", "three"} {
		running = append(running, f.mustDispatch(title, "brief "+title))
	}
	queued, err := f.dispatch("four", "brief four", repeatChecks, nil, nil, f.base)
	if err != nil || task.StageOf(queued) != task.Queued {
		t.Fatalf("fourth dispatch = (%q, %v), want a queued task", task.StageOf(queued), err)
	}
	// A decision frees a slot, and only a drain would hand it to the queued task.
	f.block(running[1], func(next *task.Task) { next.Disposition = "integrated" })
	callsBefore := len(f.calls())

	again, err := f.dispatch("four", "brief four", repeatChecks, nil, nil, f.base)
	if err != nil || again.ID != queued.ID {
		t.Fatalf("repeat = (%s, %v), want the queued task %s", again.ID, err, queued.ID)
	}
	if again.Phase != task.Intent {
		t.Errorf("the repeat moved the queued task to %q: a repeat must not drain the queue", again.Phase)
	}
	if got := len(f.calls()); got != callsBefore {
		t.Errorf("the repeat made %d Herdr calls, want none", got-callsBefore)
	}
	// The slot really was free: an explicit drain admits the task the repeat left.
	drained, err := f.coord.DrainQueued(context.Background(), f.projectKey())
	if err != nil || len(drained) != 1 || drained[0].ID != queued.ID {
		t.Fatalf("DrainQueued = (%+v, %v), want the queued task %s", drained, err, queued.ID)
	}
}
