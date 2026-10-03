package state_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func seedTicket(id, project string) task.Ticket {
	return task.Ticket{ID: id, ProjectKey: project, Title: "write the docs", Body: "details", Column: task.ColumnBacklog}
}

// ticketTask is a task of project repo-a linked to a ticket. Each n gets its
// own launch pane, because one pane belongs to one task.
func ticketTask(id, ticketID string, n int) task.Task {
	x := seedTask(id)
	x.TicketID = ticketID
	x.LaunchPaneID = fmt.Sprintf("w1:p%d", 100+n)
	x.PublicPaneID = x.LaunchPaneID
	x.ReservedPaths = nil // seedTask reserves internal/, which two tasks of one project cannot share
	return x
}

func mustCreateTicket(t *testing.T, s *state.Store, ticket task.Ticket) {
	t.Helper()
	if err := s.CreateTicket(context.Background(), ticket); err != nil {
		t.Fatalf("CreateTicket(%q): %v", ticket.ID, err)
	}
}

func TestStoreCreateTicketRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	want := seedTicket("tkt_a", "repo-a")
	mustCreateTicket(t, s, want)

	got, err := s.Ticket(ctx, "tkt_a")
	if err != nil {
		t.Fatalf("Ticket: %v", err)
	}
	if got.Revision != 1 || got.CreatedAt == 0 || got.CreatedAt != got.UpdatedAt {
		t.Fatalf("a new ticket is revision 1 with equal, non-zero timestamps: %+v", got)
	}
	want.Revision, want.CreatedAt, want.UpdatedAt = got.Revision, got.CreatedAt, got.UpdatedAt
	if got != want {
		t.Fatalf("Ticket = %+v, want %+v", got, want)
	}
	if _, err := s.Ticket(ctx, "tkt_missing"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("an unknown id = %v, want ErrNotFound", err)
	}
	if err := s.CreateTicket(ctx, seedTicket("tkt_a", "repo-a")); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("a duplicate id = %v, want ErrConflict", err)
	}
}

func TestStoreRejectsInvalidTickets(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	for name, mutate := range map[string]func(*task.Ticket){
		"empty id":          func(x *task.Ticket) { x.ID = "" },
		"empty project key": func(x *task.Ticket) { x.ProjectKey = "" },
		"blank title":       func(x *task.Ticket) { x.Title = "  \t" },
		"empty column":      func(x *task.Ticket) { x.Column = "" },
		"unknown column":    func(x *task.Ticket) { x.Column = "in_progress" },
		"already decided":   func(x *task.Ticket) { x.Outcome, x.Disposition = task.OutcomeKept, task.OutcomeKept },
		"disposition only":  func(x *task.Ticket) { x.Disposition = "done" },
	} {
		ticket := seedTicket("tkt_a", "repo-a")
		mutate(&ticket)
		if err := s.CreateTicket(ctx, ticket); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("%s: CreateTicket = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.Ticket(ctx, "tkt_a"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("a rejected ticket must not be stored: %v", err)
	}
}

func TestStoreTicketsListsOneProjectInStableOrder(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_z", "repo-a"))
	mustCreateTicket(t, s, seedTicket("tkt_other", "repo-b"))
	time.Sleep(3 * time.Millisecond) // a later CreatedAt, so age decides before the id does
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))

	got, err := s.Tickets(ctx, "repo-a")
	if err != nil {
		t.Fatalf("Tickets: %v", err)
	}
	if len(got) != 2 || got[0].ID != "tkt_z" || got[1].ID != "tkt_a" {
		t.Fatalf("Tickets(repo-a) = %+v, want only repo-a's, oldest first (tkt_z, tkt_a)", got)
	}
	if others, err := s.Tickets(ctx, "repo-b"); err != nil || len(others) != 1 || others[0].ID != "tkt_other" {
		t.Fatalf("Tickets(repo-b) = %+v, %v", others, err)
	}
	if none, err := s.Tickets(ctx, "repo-none"); err != nil || len(none) != 0 {
		t.Fatalf("Tickets of a project with none = %+v, %v", none, err)
	}
	if _, err := s.Tickets(ctx, "  "); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("a blank project key = %v, want ErrInvalid", err)
	}
}

func TestStoreUpdateTicketRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	moved, err := s.UpdateTicket(ctx, "tkt_a", 1, func(x *task.Ticket) error { x.Column = task.ColumnTodo; return nil })
	if err != nil || moved.Revision != 2 || moved.Column != task.ColumnTodo || moved.UpdatedAt < moved.CreatedAt {
		t.Fatalf("UpdateTicket = %+v, %v; want revision 2 in todo", moved, err)
	}
	called := false
	if _, err := s.UpdateTicket(ctx, "tkt_a", 1, func(x *task.Ticket) error { called = true; return nil }); !errors.Is(err, state.ErrConflict) || called {
		t.Fatalf("a stale revision = %v (mutate called: %v), want ErrConflict before mutating", err, called)
	}
	if _, err := s.UpdateTicket(ctx, "tkt_missing", 1, func(*task.Ticket) error { return nil }); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("an unknown id = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateTicket(ctx, "tkt_a", 2, nil); !errors.Is(err, state.ErrInvalid) {
		t.Fatalf("a nil mutation = %v, want ErrInvalid", err)
	}
}

func TestStoreUpdateTicketRejectsIdentityChange(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	for name, mutate := range map[string]func(*task.Ticket){
		"id":          func(x *task.Ticket) { x.ID = "tkt_b" },
		"project key": func(x *task.Ticket) { x.ProjectKey = "repo-b" },
	} {
		if _, err := s.UpdateTicket(ctx, "tkt_a", 1, func(x *task.Ticket) error { mutate(x); return nil }); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("changing the %s = %v, want ErrInvalid", name, err)
		}
	}
	if got, _ := s.Ticket(ctx, "tkt_a"); got.Revision != 1 || got.ID != "tkt_a" || got.ProjectKey != "repo-a" {
		t.Fatalf("a refused update must leave the ticket alone: %+v", got)
	}
}

// DecideTicket is the only way to record a decision, because it is the only
// writer that proves no linked task is open in the same transaction.
func TestStoreUpdateTicketCannotRecordADecisionOrAnInvalidColumn(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	for name, mutate := range map[string]func(*task.Ticket){
		"disposition": func(x *task.Ticket) { x.Disposition = "done" },
		"outcome":     func(x *task.Ticket) { x.Outcome = task.OutcomeKept },
		"column":      func(x *task.Ticket) { x.Column = "archived" },
	} {
		if _, err := s.UpdateTicket(ctx, "tkt_a", 1, func(x *task.Ticket) error { mutate(x); return nil }); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("changing the %s = %v, want ErrInvalid", name, err)
		}
	}
	if got, _ := s.Ticket(ctx, "tkt_a"); got.Revision != 1 || got.Decided() || got.Column != task.ColumnBacklog {
		t.Fatalf("a refused update must leave the ticket alone: %+v", got)
	}
}

func TestStoreConcurrentTicketUpdateHasOneWinner(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first := open(t, dir)
	second := open(t, dir)
	mustCreateTicket(t, first, seedTicket("tkt_a", "repo-a"))

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*state.Store{first, second} {
		wg.Add(1)
		go func(s *state.Store) {
			defer wg.Done()
			<-start
			_, err := s.UpdateTicket(ctx, "tkt_a", 1, func(x *task.Ticket) error { x.Column = task.ColumnTodo; return nil })
			errs <- err
		}(s)
	}
	close(start)
	wg.Wait()
	close(errs)

	var winners, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, state.ErrConflict):
			conflicts++
		default:
			t.Errorf("unexpected update error: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("concurrent ticket update = %d winners, %d conflicts; want 1, 1", winners, conflicts)
	}
	if final, err := first.Ticket(ctx, "tkt_a"); err != nil || final.Revision != 2 {
		t.Fatalf("final ticket = %+v, %v; want revision 2", final, err)
	}
}

func TestOpenMigratesAStateWithoutATicketsTable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	raw := rawState(t, dir)
	if _, err := raw.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY,
		project_key TEXT NOT NULL,
		launch_pane_id TEXT NULL,
		public_pane_id TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL,
		data JSON NOT NULL
	)`); err != nil {
		t.Fatalf("create previous task schema: %v", err)
	}
	const legacy = `{"ID":"t1","ProjectKey":"repo-a","Phase":"intent","Revision":1}`
	if _, err := raw.Exec(`INSERT INTO tasks (id, project_key, launch_pane_id, public_pane_id, revision, data)
		VALUES ('t1', 'repo-a', NULL, '', 1, ?)`, legacy); err != nil {
		t.Fatalf("insert previous task row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy handle: %v", err)
	}

	s := open(t, dir)
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	got, err := s.Task(ctx, "t1")
	if err != nil || got.ID != "t1" || got.TicketID != "" || got.Revision != 1 {
		t.Fatalf("the legacy task must survive unlinked and unchanged: %+v, %v", got, err)
	}
	var stored []byte
	check := rawState(t, dir)
	if err := check.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = 't1'`).Scan(&stored); err != nil || !bytes.Equal(stored, []byte(legacy)) {
		t.Fatalf("the legacy row's stored JSON changed: %s, %v", stored, err)
	}
}

func TestClaimTaskAndCreateTaskRefuseAnUnavailableTicket(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_other", "repo-b"))
	mustCreateTicket(t, s, seedTicket("tkt_done", "repo-a"))
	if _, err := s.DecideTicket(ctx, "tkt_done", 1, task.OutcomeAbandoned, "not needed"); err != nil {
		t.Fatalf("DecideTicket: %v", err)
	}
	for _, ticketID := range []string{"tkt_other", "tkt_missing", "tkt_done"} {
		x := ticketTask("t1", ticketID, 1)
		if err := s.CreateTask(ctx, x); !errors.Is(err, state.ErrTicketUnavailable) {
			t.Errorf("CreateTask(ticket %s) = %v, want ErrTicketUnavailable", ticketID, err)
		}
		if _, _, _, err := s.ClaimTask(ctx, x, 3); !errors.Is(err, state.ErrTicketUnavailable) {
			t.Errorf("ClaimTask(ticket %s) = %v, want ErrTicketUnavailable", ticketID, err)
		}
		if _, err := s.Task(ctx, "t1"); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("a refused task for ticket %s must not be stored: %v", ticketID, err)
		}
	}
}

func TestClaimTaskAcceptsABacklogTicket(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a")) // still in the backlog
	if err := s.CreateTask(ctx, ticketTask("t1", "tkt_a", 1)); err != nil {
		t.Fatalf("CreateTask for a backlog ticket: %v", err)
	}
	// Identical work would be answered as a repeat of t1 (issue #4); this test is
	// about the ticket, so the second task is a different piece of work.
	second := ticketTask("t2", "tkt_a", 2)
	second.Title = "a second piece of work for the ticket"
	if _, linked, _, err := s.ClaimTask(ctx, second, 3); err != nil || linked.TicketID != "tkt_a" {
		t.Fatalf("ClaimTask for a backlog ticket = %+v, %v", linked, err)
	}
	for _, id := range []string{"t1", "t2"} {
		if got, err := s.Task(ctx, id); err != nil || got.TicketID != "tkt_a" {
			t.Fatalf("task %s must be stored linked: %+v, %v", id, got, err)
		}
	}
}

func TestDecideTicketRefusesWhileALinkedTaskIsOpen(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	mustCreateTicket(t, s, seedTicket("tkt_b", "repo-a"))
	if _, _, _, err := s.ClaimTask(ctx, ticketTask("t1", "tkt_a", 1), 3); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	// A task linked to another ticket, and an unlinked one, never block tkt_a.
	if _, _, _, err := s.ClaimTask(ctx, ticketTask("t2", "tkt_b", 2), 3); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeIntegrated, "merged"); !errors.Is(err, state.ErrTicketHasOpenTasks) {
		t.Fatalf("DecideTicket with an open linked task = %v, want ErrTicketHasOpenTasks", err)
	}
	if got, _ := s.Ticket(ctx, "tkt_a"); got.Revision != 1 || got.Decided() {
		t.Fatalf("a refused decision must leave the ticket unchanged: %+v", got)
	}

	if _, err := s.UpdateTask(ctx, "t1", 1, func(x *task.Task) error {
		x.Outcome, x.Disposition = task.OutcomeIntegrated, task.OutcomeIntegrated
		return nil
	}); err != nil {
		t.Fatalf("close the linked task: %v", err)
	}
	decided, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeIntegrated, "merged")
	if err != nil || !decided.Decided() || decided.Outcome != task.OutcomeIntegrated || decided.Disposition != "merged" || decided.Revision != 2 {
		t.Fatalf("DecideTicket after the task closed = %+v, %v", decided, err)
	}
	if decided.Column != task.ColumnBacklog {
		t.Fatalf("a decision does not look at or change the column: %+v", decided)
	}
}

func TestDecideTicketWithNoLinkedTasksIsAllowed(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	// An unlinked task of the same project must not count as linked work.
	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeAbandoned, "not needed"); err != nil || !got.Decided() {
		t.Fatalf("DecideTicket with zero linked tasks = %+v, %v", got, err)
	}
}

func TestDecideTicketValidatesAndKeepsTheFirstDecision(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	if _, err := s.DecideTicket(ctx, "tkt_missing", 1, task.OutcomeKept, "x"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("an unknown ticket = %v, want ErrNotFound", err)
	}
	if _, err := s.DecideTicket(ctx, "tkt_a", 7, task.OutcomeKept, "x"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("a stale revision = %v, want ErrConflict", err)
	}
	for name, args := range map[string][2]string{"blank outcome": {" ", "x"}, "blank disposition": {task.OutcomeKept, "  "}} {
		if _, err := s.DecideTicket(ctx, "tkt_a", 1, args[0], args[1]); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeKept, "first"); err != nil {
		t.Fatalf("first decision: %v", err)
	}
	// Even at the current revision a second decision must not overwrite the first.
	if _, err := s.DecideTicket(ctx, "tkt_a", 2, task.OutcomeRejected, "second"); !errors.Is(err, state.ErrConflict) {
		t.Errorf("a second decision = %v, want ErrConflict", err)
	}
	if got, _ := s.Ticket(ctx, "tkt_a"); got.Outcome != task.OutcomeKept || got.Disposition != "first" || got.Revision != 2 {
		t.Fatalf("the first decision must stand: %+v", got)
	}
}

func TestClaimTaskAfterADecisionIsRefused(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	if _, err := s.DecideTicket(ctx, "tkt_a", 1, task.OutcomeSuperseded, "replaced"); err != nil {
		t.Fatalf("DecideTicket: %v", err)
	}
	if _, _, _, err := s.ClaimTask(ctx, ticketTask("t1", "tkt_a", 1), 3); !errors.Is(err, state.ErrTicketUnavailable) {
		t.Fatalf("ClaimTask after a decision = %v, want ErrTicketUnavailable", err)
	}
	if _, err := s.Task(ctx, "t1"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("no task row may exist after the refusal: %v", err)
	}
}

// A decision and a dispatch for the same ticket are both immediate write
// transactions, so SQLite runs them one after the other in either order, across
// handles. Whichever lands first decides the outcome; both can never succeed.
func TestTicketDecisionAndTaskClaimNeverBothSucceed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	decider := open(t, dir)
	claimer := open(t, dir)
	var claimWins, decideWins int

	for round := 0; round < 50; round++ {
		ticketID := fmt.Sprintf("tkt_%02d", round)
		taskID := fmt.Sprintf("task_%02d", round)
		mustCreateTicket(t, decider, seedTicket(ticketID, "repo-a"))
		linked := ticketTask(taskID, ticketID, round)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var decideErr, claimErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, decideErr = decider.DecideTicket(ctx, ticketID, 1, task.OutcomeAbandoned, "not needed")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, _, claimErr = claimer.ClaimTask(ctx, linked, 1000)
		}()
		close(start)
		wg.Wait()

		_, taskErr := claimer.Task(ctx, taskID)
		switch {
		case decideErr == nil && claimErr == nil:
			t.Fatalf("round %d: the decision and the claim both succeeded", round)
		case decideErr != nil && claimErr != nil:
			t.Fatalf("round %d: neither succeeded: decide=%v claim=%v", round, decideErr, claimErr)
		case claimErr == nil:
			claimWins++
			if !errors.Is(decideErr, state.ErrTicketHasOpenTasks) || taskErr != nil {
				t.Fatalf("round %d: the claim won, so the decision must be ErrTicketHasOpenTasks (got %v) and the task must exist (got %v)", round, decideErr, taskErr)
			}
		default:
			decideWins++
			if !errors.Is(claimErr, state.ErrTicketUnavailable) || !errors.Is(taskErr, state.ErrNotFound) {
				t.Fatalf("round %d: the decision won, so the claim must be ErrTicketUnavailable (got %v) and no task row may exist (got %v)", round, claimErr, taskErr)
			}
		}
	}
	t.Logf("50 rounds: %d claims won, %d decisions won", claimWins, decideWins)
}

func TestUpdateTaskRefusesToChangeTicketID(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	mustCreateTicket(t, s, seedTicket("tkt_a", "repo-a"))
	mustCreateTicket(t, s, seedTicket("tkt_b", "repo-a"))
	if err := s.CreateTask(ctx, ticketTask("t1", "tkt_a", 1)); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	unlinked := seedTask("t2")
	unlinked.LaunchPaneID, unlinked.PublicPaneID = "w1:p200", "w1:p200"
	if err := s.CreateTask(ctx, unlinked); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	for name, c := range map[string]struct {
		id       string
		ticketID string
	}{
		"another ticket":             {"t1", "tkt_b"},
		"cleared":                    {"t1", ""},
		"linked after the fact":      {"t2", "tkt_a"},
		"linked to a missing ticket": {"t2", "tkt_missing"},
	} {
		if _, err := s.UpdateTask(ctx, c.id, 1, func(x *task.Task) error { x.TicketID = c.ticketID; return nil }); !errors.Is(err, state.ErrInvalid) {
			t.Errorf("%s: UpdateTask = %v, want ErrInvalid", name, err)
		}
	}
	// An unrelated change still works and keeps the link.
	next, err := s.UpdateTask(ctx, "t1", 1, func(x *task.Task) error { x.Activity = "working"; return nil })
	if err != nil || next.TicketID != "tkt_a" || next.Activity != "working" {
		t.Fatalf("an unrelated update = %+v, %v", next, err)
	}
}
