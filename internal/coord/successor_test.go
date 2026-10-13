package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/task"
)

// reviewedWorker dispatches one worker and gives it a final result, so the
// primary can decide on it.
func reviewedWorker(t *testing.T, f *coordFixture, title string) task.Task {
	t.Helper()
	return f.satisfyReview(f.mustDispatch(title, "Brief for "+title+"."))
}

func primaryPane(t *testing.T, f *coordFixture) string {
	t.Helper()
	project, err := f.store.Project(context.Background(), f.projectKey())
	if err != nil {
		t.Fatalf("read the bound project: %v", err)
	}
	return project.LaunchPaneID
}

func TestSupersededTaskDecisionNeedsAValidSuccessor(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	worker := reviewedWorker(t, f, "superseded")
	foreign := task.Task{ID: "orch_foreign", ProjectKey: foreignProject, Title: "someone else's", Phase: task.PromptSubmitted}
	if err := f.store.CreateTask(ctx, foreign); err != nil {
		t.Fatalf("seed another project's task: %v", err)
	}
	before := f.taskByID(t, worker.ID)

	for _, c := range []struct {
		name, successor string
		want            error
	}{
		{"no successor", "", ErrSuccessorRequired},
		{"another project's task", foreign.ID, ErrSuccessorUnknown},
		{"a task that does not exist", "orch_missing", ErrSuccessorUnknown},
		{"the task itself", worker.ID, ErrSuccessorSelf},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.coord.Review(ctx, pane, worker.ID, task.OutcomeSuperseded, "replaced", c.successor)
			if !errors.Is(err, c.want) {
				t.Fatalf("superseded naming %q = %v, want %v", c.successor, err, c.want)
			}
			if after := f.taskByID(t, worker.ID); after.Disposition != "" || after.Outcome != "" || after.Revision != before.Revision {
				t.Fatalf("a refused superseded decision recorded something:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

func TestOtherOutcomesRefuseASuccessorAndRecordNothing(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	worker := reviewedWorker(t, f, "decided")
	successor := f.mustDispatch("successor", "The replacement brief.")
	before := f.taskByID(t, worker.ID)

	for _, outcome := range []string{task.OutcomeIntegrated, task.OutcomeKept, task.OutcomeRejected, task.OutcomeAbandoned} {
		if _, err := f.coord.Review(ctx, pane, worker.ID, outcome, "note", successor.ID); !errors.Is(err, ErrSuccessorNotAllowed) {
			t.Fatalf("%s naming a successor = %v, want ErrSuccessorNotAllowed", outcome, err)
		}
	}
	if after := f.taskByID(t, worker.ID); after.Disposition != "" || after.Revision != before.Revision {
		t.Fatalf("a refused decision recorded something: %+v", after)
	}
}

// A superseded decision names where the work went and releases nothing: a
// dependent of the superseded task stays queued, and no worker starts for it.
func TestSupersededDecisionRecordsTheSuccessorAndReleasesNoDependent(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	worker := reviewedWorker(t, f, "superseded")
	successor := f.mustDispatch("successor", "The replacement brief.")
	dependent, err := f.dispatch("dependent", "Waits on the superseded work.", []string{"go test ./... ok"}, []string{worker.ID}, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch the dependent: %v", err)
	}
	starts := f.countCalls("worktree create")
	successorBefore := f.taskByID(t, successor.ID)

	result, err := f.coord.Review(ctx, pane, worker.ID, task.OutcomeSuperseded, "replaced by the successor", successor.ID)
	if err != nil {
		t.Fatalf("Review superseded: %v", err)
	}
	if result.Outcome != task.OutcomeSuperseded || result.Successor != successor.ID || result.Stage != task.Done {
		t.Fatalf("Review = %+v, want a done task superseded by %s", result, successor.ID)
	}
	if after := f.taskByID(t, dependent.ID); task.StageOf(after) != task.Queued || after.Phase != task.Intent {
		t.Fatalf("recording superseded released the dependent: stage %q phase %q", task.StageOf(after), after.Phase)
	}
	if got := f.countCalls("worktree create"); got != starts {
		t.Fatalf("a worker started for the dependent of a superseded task (%d -> %d)", starts, got)
	}
	if after := f.taskByID(t, successor.ID); !reflect.DeepEqual(after, successorBefore) {
		t.Fatalf("the successor changed while the decision named it:\nbefore %+v\nafter  %+v", successorBefore, after)
	}
}

// The first decision stands. A repeat is idempotent only when its successor
// matches the recorded one (ADR 0001 decision 6, extended by issue #61).
func TestRepeatedSupersededDecisionIsIdempotentOnlyWithTheSameSuccessor(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	worker := reviewedWorker(t, f, "superseded")
	successor := f.mustDispatch("successor", "The replacement brief.")
	other := f.mustDispatch("other successor", "Another replacement brief.")

	first, err := f.coord.Review(ctx, pane, worker.ID, task.OutcomeSuperseded, "replaced", successor.ID)
	if err != nil {
		t.Fatalf("first decision: %v", err)
	}
	revision := f.taskByID(t, worker.ID).Revision
	again, err := f.coord.Review(ctx, pane, worker.ID, task.OutcomeSuperseded, "replaced", successor.ID)
	if err != nil || again != first {
		t.Fatalf("the identical repeat = (%+v, %v), want the first result %+v", again, err, first)
	}
	for _, successorID := range []string{"", other.ID} {
		if _, err := f.coord.Review(ctx, pane, worker.ID, task.OutcomeSuperseded, "replaced", successorID); !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("a repeat naming %q = %v, want ErrAlreadyDecided", successorID, err)
		}
	}
	if after := f.taskByID(t, worker.ID); after.Successor != successor.ID || after.Revision != revision {
		t.Fatalf("a repeat rewrote the first decision: %+v", after)
	}
}

// A superseded task closed before successors existed names none. It reads and
// repeats exactly as it did (issue #61: legacy rows keep their behaviour).
func TestLegacySupersededTaskKeepsItsBehaviour(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	worker := reviewedWorker(t, f, "legacy")
	f.seedLegacyTask(worker.ID, func(next *task.Task) {
		next.Outcome = task.OutcomeSuperseded
		next.Disposition = "replaced by hand"
	})
	dependent, err := f.dispatch("dependent", "Waits on the legacy superseded work.", []string{"go test ./... ok"}, []string{worker.ID}, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch the dependent: %v", err)
	}

	result, err := f.coord.Review(ctx, pane, worker.ID, task.OutcomeSuperseded, "replaced by hand", "")
	if err != nil || result.Stage != task.Done || result.Successor != "" {
		t.Fatalf("the legacy repeat = (%+v, %v), want the unchanged done task", result, err)
	}
	if _, err := f.coord.Review(ctx, pane, worker.ID, task.OutcomeSuperseded, "replaced by hand", dependent.ID); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a legacy task gaining a successor = %v, want ErrAlreadyDecided", err)
	}
	if after := f.taskByID(t, dependent.ID); task.StageOf(after) != task.Queued {
		t.Fatalf("the dependent of a legacy superseded task = %q, want queued as before", task.StageOf(after))
	}
}

func TestSupersededTicketDecisionNeedsAValidSuccessorTicket(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "superseded")
	f.newTicket(key, "replacement")
	foreign := f.foreignTicket("tkt_foreign")

	for _, c := range []struct {
		name, outcome, successor string
		want                     error
	}{
		{"no successor", task.OutcomeSuperseded, "", ErrSuccessorRequired},
		{"another project's ticket", task.OutcomeSuperseded, foreign.ID, ErrTicketUnknown},
		{"a ticket that does not exist", task.OutcomeSuperseded, "tkt_missing", ErrTicketUnknown},
		{"the ticket itself", task.OutcomeSuperseded, ticket.ID, ErrSuccessorSelf},
		{"rejected naming a successor", task.OutcomeRejected, foreign.ID, ErrSuccessorNotAllowed},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{
				TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: c.outcome, Disposition: "replaced", Successor: c.successor,
			})
			if !errors.Is(err, c.want) {
				t.Fatalf("%s naming %q = %v, want %v", c.outcome, c.successor, err, c.want)
			}
			if after, _ := f.store.Ticket(ctx, ticket.ID); after.Decided() || after.Revision != ticket.Revision {
				t.Fatalf("a refused ticket decision recorded something: %+v", after)
			}
		})
	}
}

// A superseded ticket waits for its linked tasks like any decision, and then it
// changes no linked task and creates no dependency.
func TestSupersededTicketDecisionWaitsForLinkedTasksAndChangesNone(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "busy")
	replacement := f.newTicket(key, "replacement")
	f.acceptedPlanner(key, ticket.ID)
	f.linkedTask(key, ticket.ID, "orch_busy")
	params := TicketReviewParams{
		TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: task.OutcomeSuperseded, Disposition: "replaced", Successor: replacement.ID,
	}
	if _, err := f.coord.ReviewTicketFor(ctx, key, params); !errors.Is(err, ErrTicketOpen) {
		t.Fatalf("a superseded ticket with an open linked task = %v, want ErrTicketOpen", err)
	}
	if after, _ := f.store.Ticket(ctx, ticket.ID); after.Decided() || after.Revision != ticket.Revision {
		t.Fatalf("a refused superseded ticket changed: %+v", after)
	}

	f.closeTask("orch_busy")
	linkedBefore := f.taskByID(t, "orch_busy")
	view, err := f.coord.ReviewTicketFor(ctx, key, params)
	if err != nil {
		t.Fatalf("ReviewTicketFor after the linked task closed: %v", err)
	}
	if view.Outcome != task.OutcomeSuperseded || view.Successor != replacement.ID || view.Stage != task.TicketDone {
		t.Fatalf("the superseded ticket view = %+v, want done, superseded by %s", view, replacement.ID)
	}
	if after := f.taskByID(t, "orch_busy"); !reflect.DeepEqual(after, linkedBefore) {
		t.Fatalf("a superseded ticket changed its linked task:\nbefore %+v\nafter  %+v", linkedBefore, after)
	}
}

func TestRepeatedSupersededTicketDecisionMatchesTheSuccessor(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "decide once")
	replacement := f.newTicket(key, "replacement")
	other := f.newTicket(key, "other replacement")
	params := TicketReviewParams{
		TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: task.OutcomeSuperseded, Disposition: "replaced", Successor: replacement.ID,
	}
	first, err := f.coord.ReviewTicketFor(ctx, key, params)
	if err != nil {
		t.Fatalf("first decision: %v", err)
	}
	again, err := f.coord.ReviewTicketFor(ctx, key, params)
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("the identical repeat = (%+v, %v), want %+v", again, err, first)
	}
	for _, successor := range []string{"", other.ID} {
		different := params
		different.Successor = successor
		if _, err := f.coord.ReviewTicketFor(ctx, key, different); !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("a repeat naming %q = %v, want ErrAlreadyDecided", successor, err)
		}
	}
}

// A ticket superseded before successors existed reads and repeats as before.
func TestLegacySupersededTicketKeepsItsBehaviour(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "legacy")
	f.seedLegacyTicket(ticket.ID, func(x *task.Ticket) {
		x.Outcome, x.Disposition = task.OutcomeSuperseded, "replaced by hand"
	})
	view, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{
		TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: task.OutcomeSuperseded, Disposition: "replaced by hand",
	})
	if err != nil || view.Stage != task.TicketDone || view.Successor != "" {
		t.Fatalf("the legacy repeat = (%+v, %v), want the unchanged done ticket", view, err)
	}
}

// seedLegacyTask rewrites a task row the way a build before issue #61 left it.
// The store's successor gate guards every write the current build makes, so a
// superseded row that names no successor can only come from an earlier build;
// the test writes that row straight to the state database.
func (f *coordFixture) seedLegacyTask(id string, mutate func(*task.Task)) {
	f.t.Helper()
	ctx := context.Background()
	db := f.rawStateDB()
	defer func() { _ = db.Close() }()
	var data string
	if err := db.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, id).Scan(&data); err != nil {
		f.t.Fatalf("read task %s for a legacy row: %v", id, err)
	}
	var st task.Task
	if err := json.Unmarshal([]byte(data), &st); err != nil {
		f.t.Fatalf("decode task %s: %v", id, err)
	}
	mutate(&st)
	out, err := json.Marshal(st)
	if err != nil {
		f.t.Fatalf("encode task %s: %v", id, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET data = ? WHERE id = ?`, string(out), id); err != nil {
		f.t.Fatalf("write legacy task %s: %v", id, err)
	}
}

// seedLegacyTicket is seedLegacyTask for a ticket: a decision an earlier build
// recorded before successors existed.
func (f *coordFixture) seedLegacyTicket(id string, mutate func(*task.Ticket)) {
	f.t.Helper()
	ctx := context.Background()
	db := f.rawStateDB()
	defer func() { _ = db.Close() }()
	var data string
	if err := db.QueryRowContext(ctx, `SELECT data FROM tickets WHERE id = ?`, id).Scan(&data); err != nil {
		f.t.Fatalf("read ticket %s for a legacy row: %v", id, err)
	}
	var tk task.Ticket
	if err := json.Unmarshal([]byte(data), &tk); err != nil {
		f.t.Fatalf("decode ticket %s: %v", id, err)
	}
	mutate(&tk)
	out, err := json.Marshal(tk)
	if err != nil {
		f.t.Fatalf("encode ticket %s: %v", id, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tickets SET data = ? WHERE id = ?`, string(out), id); err != nil {
		f.t.Fatalf("write legacy ticket %s: %v", id, err)
	}
}

// rawStateDB opens the fixture's state database for a test that must write a
// row the store itself refuses. The caller closes it.
func (f *coordFixture) rawStateDB() *sql.DB {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		f.t.Fatalf("open state database: %v", err)
	}
	return db
}

// A dependent of a superseded task names the successor and the re-point path, and
// the task list shows the successor the decision recorded (issue #61).
func TestSupersededDependencyNamesTheRepointPathAndTheListShowsTheSuccessor(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	pane := primaryPane(t, f)
	superseded, successor := supersededWithSuccessor(t, f, pane)
	dependent := queuedDependent(t, f, "dependent", superseded.ID)

	tasks, err := f.store.Tasks(ctx, f.projectKey())
	if err != nil {
		t.Fatalf("read the tasks: %v", err)
	}
	reason := unsatisfiedDependency(tasks, f.taskByID(t, dependent.ID))
	for _, want := range []string{"closed as superseded", "re-point", successor.ID} {
		if !strings.Contains(reason, want) {
			t.Errorf("the queue reason %q does not contain %q", reason, want)
		}
	}

	list, err := f.coord.ListTasksFor(ctx, f.projectKey(), TaskListParams{})
	if err != nil {
		t.Fatalf("ListTasksFor: %v", err)
	}
	var entries []TaskEntry
	for _, entry := range list.Tasks {
		if entry.TaskID == superseded.ID {
			entries = append(entries, entry)
		}
	}
	if len(entries) != 1 || entries[0].Successor != successor.ID {
		t.Fatalf("the task list shows %+v for the superseded task, want successor %s", entries, successor.ID)
	}
}
func TestNormalizeSuccessorUsesUTF16Units(t *testing.T) {
	if got, err := normalizeSuccessor(strings.Repeat("😀", 32)); err != nil || got == "" {
		t.Fatalf("64 UTF-16-unit successor rejected: %q, %v", got, err)
	}
	if _, err := normalizeSuccessor(strings.Repeat("😀", 33)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("66 UTF-16-unit successor error = %v, want ErrInvalidRequest", err)
	}
}
