package coord

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/task"
)

const foreignProject = "E:/elsewhere/other-repo"

// ticketFixture is a bound project plus its key.
func ticketFixture(t *testing.T) (*coordFixture, string) {
	t.Helper()
	f := newCoordFixture(t, fixturePlan{})
	return f, f.projectKey()
}

func (f *coordFixture) newTicket(project, title string) task.Ticket {
	f.t.Helper()
	view, err := f.coord.CreateTicketFor(context.Background(), project, TicketCreateParams{Title: title, Body: "body of " + title})
	if err != nil {
		f.t.Fatalf("CreateTicketFor(%q): %v", title, err)
	}
	ticket, err := f.store.Ticket(context.Background(), view.TicketID)
	if err != nil {
		f.t.Fatalf("read the created ticket: %v", err)
	}
	return ticket
}

// foreignTicket seeds a ticket that belongs to another project, straight into
// the store (the coordinator can only write for a bound project).
func (f *coordFixture) foreignTicket(id string) task.Ticket {
	f.t.Helper()
	ticket := task.Ticket{ID: id, ProjectKey: foreignProject, Title: "someone else's", Column: task.ColumnBacklog}
	if err := f.store.CreateTicket(context.Background(), ticket); err != nil {
		f.t.Fatalf("seed the foreign ticket: %v", err)
	}
	stored, err := f.store.Ticket(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read the foreign ticket: %v", err)
	}
	return stored
}

// linkedTask records an open task for a ticket, the way dispatch does.
func (f *coordFixture) linkedTask(project, ticketID, id string) {
	f.t.Helper()
	x := task.Task{ID: id, ProjectKey: project, TicketID: ticketID, Title: "work", Phase: task.PromptSubmitted}
	if err := f.store.CreateTask(context.Background(), x); err != nil {
		f.t.Fatalf("seed a linked task: %v", err)
	}
}

func (f *coordFixture) closeTask(id string) {
	f.t.Helper()
	current, err := f.store.Task(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read task %s: %v", id, err)
	}
	if _, err := f.store.UpdateTask(context.Background(), id, current.Revision, func(x *task.Task) error {
		x.Outcome, x.Disposition = task.OutcomeIntegrated, task.OutcomeIntegrated
		return nil
	}); err != nil {
		f.t.Fatalf("close task %s: %v", id, err)
	}
}

func TestCreateTicketRecordsABacklogTicket(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	view, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "  Add search  ", Body: "find things"})
	if err != nil {
		t.Fatalf("CreateTicketFor: %v", err)
	}
	if view.Column != task.ColumnBacklog || view.Stage != task.TicketBacklog || view.Revision != 1 {
		t.Fatalf("a new ticket is backlog at revision 1: %+v", view)
	}
	if !strings.HasPrefix(view.TicketID, "tkt_") || len(view.TicketID) != len("tkt_")+12 {
		t.Fatalf("ticket id %q must be tkt_ plus 12 hex characters", view.TicketID)
	}
	if view.Title != "Add search" || view.Body != "find things" {
		t.Fatalf("the title is trimmed and the body kept: %+v", view)
	}
	stored, err := f.store.Ticket(ctx, view.TicketID)
	if err != nil || stored.ProjectKey != key || view.Digest != stored.Digest() {
		t.Fatalf("stored ticket %+v, %v; the view's digest must be the stored ticket's", stored, err)
	}
	if view.TaskIDs == nil || len(view.TaskIDs) != 0 {
		t.Fatalf("TaskIDs must be an empty list, not null: %#v", view.TaskIDs)
	}

	// The bounds are the dispatch bounds, inclusive; a title alone is enough.
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: strings.Repeat("t", maxTitleLength)}); err != nil {
		t.Fatalf("a title of exactly %d characters and no body: %v", maxTitleLength, err)
	}
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "big", Body: strings.Repeat("b", maxBriefLength)}); err != nil {
		t.Fatalf("a body of exactly %d characters: %v", maxBriefLength, err)
	}
	for name, params := range map[string]TicketCreateParams{
		"blank title":   {Title: "  \t"},
		"long title":    {Title: strings.Repeat("t", maxTitleLength+1)},
		"long body":     {Title: "ok", Body: strings.Repeat("b", maxBriefLength+1)},
		"title missing": {Body: "only a body"},
	} {
		if _, err := f.coord.CreateTicketFor(ctx, key, params); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: CreateTicketFor = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := f.coord.CreateTicketFor(ctx, "E:/never/bound", TicketCreateParams{Title: "x"}); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("an unbound project key = %v, want ErrProjectUnknown", err)
	}
	if all, _ := f.store.Tickets(ctx, key); len(all) != 3 {
		t.Fatalf("only the three valid creations may be stored, got %d", len(all))
	}
}

func TestListTicketsForShowsOnlyThisProjectsTicketsWithDerivedStage(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	first := f.newTicket(key, "first")
	time.Sleep(3 * time.Millisecond)
	second := f.newTicket(key, "second")
	f.foreignTicket("tkt_foreign")
	f.linkedTask(key, first.ID, "orch_one")
	f.linkedTask(key, first.ID, "orch_two")

	list, err := f.coord.ListTicketsFor(ctx, key)
	if err != nil {
		t.Fatalf("ListTicketsFor: %v", err)
	}
	if len(list.Tickets) != 2 || list.Tickets[0].TicketID != first.ID || list.Tickets[1].TicketID != second.ID {
		t.Fatalf("want this project's two tickets, oldest first: %+v", list.Tickets)
	}
	if got := list.Tickets[0]; got.Stage != task.TicketInProgress || !reflect.DeepEqual(got.TaskIDs, []string{"orch_one", "orch_two"}) {
		t.Fatalf("a ticket with open linked tasks is in progress and lists them oldest first: %+v", got)
	}
	if got := list.Tickets[1]; got.Stage != task.TicketBacklog || len(got.TaskIDs) != 0 {
		t.Fatalf("an untouched ticket is backlog with no tasks: %+v", got)
	}

	f.closeTask("orch_one")
	f.closeTask("orch_two")
	list, err = f.coord.ListTicketsFor(ctx, key)
	if err != nil || list.Tickets[0].Stage != task.TicketReadyForReview {
		t.Fatalf("every linked task closed reads ready for review: %+v, %v", list.Tickets, err)
	}
	if _, err := f.coord.ListTicketsFor(ctx, "E:/never/bound"); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("an unbound project key = %v, want ErrProjectUnknown", err)
	}
}

func TestMoveTicketRecordsTodoAndStartsNothing(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "move me")
	preview, err := f.coord.TicketPreviewFor(ctx, key, ticket.ID)
	if err != nil || preview.Digest != ticket.Digest() || preview.Column != task.ColumnBacklog {
		t.Fatalf("TicketPreviewFor = %+v, %v", preview, err)
	}
	moved, err := f.coord.MoveTicketFor(ctx, key, ticket.ID, preview.Digest)
	if err != nil {
		t.Fatalf("MoveTicketFor: %v", err)
	}
	if moved.Column != task.ColumnTodo || moved.Stage != task.TicketTodo || moved.Revision != 2 || moved.Digest == preview.Digest {
		t.Fatalf("a move records todo at revision 2 with a new digest: %+v", moved)
	}
	if tasks, err := f.store.Tasks(ctx, key); err != nil || len(tasks) != 0 {
		t.Fatalf("a move must not start any task: %+v, %v", tasks, err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("a move must not call Herdr at all: %+v", calls)
	}
}

func TestMoveTicketRefusals(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)

	stale := f.newTicket(key, "stale")
	staleDigest := stale.Digest()
	if _, err := f.coord.MoveTicketFor(ctx, key, stale.ID, staleDigest); err != nil {
		t.Fatalf("the first move: %v", err)
	}
	// A digest taken before that move no longer matches.
	if _, err := f.coord.MoveTicketFor(ctx, key, stale.ID, staleDigest); !errors.Is(err, ErrTicketChanged) {
		t.Errorf("a stale digest = %v, want ErrTicketChanged", err)
	}
	// A fresh digest for a ticket already in todo is a different refusal.
	current, _ := f.store.Ticket(ctx, stale.ID)
	if _, err := f.coord.MoveTicketFor(ctx, key, stale.ID, current.Digest()); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("moving a ticket already in todo = %v, want ErrInvalidRequest", err)
	}

	decided := f.newTicket(key, "decided")
	if _, err := f.store.DecideTicket(ctx, decided.ID, decided.Revision, task.OutcomeAbandoned, "no"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	fresh, _ := f.store.Ticket(ctx, decided.ID)
	if _, err := f.coord.MoveTicketFor(ctx, key, decided.ID, fresh.Digest()); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("moving a decided ticket = %v, want ErrAlreadyDecided", err)
	}
	if _, err := f.coord.MoveTicketFor(ctx, key, decided.ID, "not-the-digest"); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("a decided ticket is refused as decided before its digest is compared, got %v", err)
	}

	blank := f.newTicket(key, "blank digest")
	for _, digest := range []string{"", "   "} {
		if _, err := f.coord.MoveTicketFor(ctx, key, blank.ID, digest); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("digest %q = %v, want ErrInvalidRequest", digest, err)
		}
	}
	if after, _ := f.store.Ticket(ctx, blank.ID); after.Column != task.ColumnBacklog || after.Revision != 1 {
		t.Fatalf("a refused move must write nothing: %+v", after)
	}
	if _, err := f.coord.MoveTicketFor(ctx, key, "  ", "x"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a blank ticket id = %v, want ErrInvalidRequest", err)
	}
}

func TestMoveTicketConcurrentHasOneWinner(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "race")
	other := f.sibling()
	digest := ticket.Digest()

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, c := range []*Coordinator{f.coord, other} {
		wg.Add(1)
		go func(c *Coordinator) {
			defer wg.Done()
			<-start
			_, err := c.MoveTicketFor(ctx, key, ticket.ID, digest)
			errs <- err
		}(c)
	}
	close(start)
	wg.Wait()
	close(errs)

	var winners, changed int
	for err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrTicketChanged):
			changed++
		default:
			t.Errorf("unexpected move error: %v", err)
		}
	}
	if winners != 1 || changed != 1 {
		t.Fatalf("concurrent move = %d winners, %d ErrTicketChanged; want 1, 1", winners, changed)
	}
	if final, err := f.store.Ticket(ctx, ticket.ID); err != nil || final.Revision != 2 || final.Column != task.ColumnTodo {
		t.Fatalf("final ticket = %+v, %v; want todo at revision 2", final, err)
	}
}

func TestTicketOpsRefuseAnotherProjectsTicket(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	foreign := f.foreignTicket("tkt_foreign")

	for _, id := range []string{foreign.ID, "tkt_nonexistent"} {
		if _, err := f.coord.TicketPreviewFor(ctx, key, id); !errors.Is(err, ErrTicketUnknown) {
			t.Errorf("TicketPreviewFor(%s) = %v, want ErrTicketUnknown", id, err)
		}
		if _, err := f.coord.MoveTicketFor(ctx, key, id, foreign.Digest()); !errors.Is(err, ErrTicketUnknown) {
			t.Errorf("MoveTicketFor(%s) = %v, want ErrTicketUnknown", id, err)
		}
		if _, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{
			TicketID: id, Digest: foreign.Digest(), Outcome: task.OutcomeAbandoned,
		}); !errors.Is(err, ErrTicketUnknown) {
			t.Errorf("ReviewTicketFor(%s) = %v, want ErrTicketUnknown", id, err)
		}
	}
	// The refusal and the not-found are the same answer, so an id cannot be probed.
	_, foreignErr := f.coord.TicketPreviewFor(ctx, key, foreign.ID)
	_, missingErr := f.coord.TicketPreviewFor(ctx, key, "tkt_nonexistent")
	if foreignErr == nil || missingErr == nil || strings.ReplaceAll(foreignErr.Error(), foreign.ID, "ID") != strings.ReplaceAll(missingErr.Error(), "tkt_nonexistent", "ID") {
		t.Errorf("a foreign id and a missing id must read the same: %v vs %v", foreignErr, missingErr)
	}
	after, err := f.store.Ticket(ctx, foreign.ID)
	if err != nil || !reflect.DeepEqual(after, foreign) {
		t.Fatalf("the other project's ticket must be untouched: %+v, %v", after, err)
	}
	if _, err := f.coord.TicketPreviewFor(ctx, "E:/never/bound", foreign.ID); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("an unbound project key = %v, want ErrProjectUnknown", err)
	}
}

func TestReviewTicket(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)

	t.Run("abandoned closes a ticket with no tasks", func(t *testing.T) {
		ticket := f.newTicket(key, "no work")
		view, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: task.OutcomeAbandoned})
		if err != nil {
			t.Fatalf("ReviewTicketFor: %v", err)
		}
		if view.Stage != task.TicketDone || view.Outcome != task.OutcomeAbandoned || view.Disposition != task.OutcomeAbandoned {
			t.Fatalf("a closed ticket is done and the note defaults to the outcome: %+v", view)
		}
	})

	t.Run("an open linked task refuses the decision", func(t *testing.T) {
		ticket := f.newTicket(key, "busy")
		f.linkedTask(key, ticket.ID, "orch_busy")
		params := TicketReviewParams{TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: task.OutcomeIntegrated, Disposition: "all merged"}
		if _, err := f.coord.ReviewTicketFor(ctx, key, params); !errors.Is(err, ErrTicketOpen) {
			t.Fatalf("an open linked task = %v, want ErrTicketOpen", err)
		}
		if after, _ := f.store.Ticket(ctx, ticket.ID); after.Revision != ticket.Revision || after.Decided() {
			t.Fatalf("a refused review must leave the ticket unchanged: %+v", after)
		}
		f.closeTask("orch_busy")
		view, err := f.coord.ReviewTicketFor(ctx, key, params)
		if err != nil || view.Stage != task.TicketDone || view.Disposition != "all merged" {
			t.Fatalf("after the task closed = %+v, %v; want done", view, err)
		}
	})

	t.Run("repeats and conflicts", func(t *testing.T) {
		ticket := f.newTicket(key, "decide once")
		params := TicketReviewParams{TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: task.OutcomeKept, Disposition: "keep it"}
		first, err := f.coord.ReviewTicketFor(ctx, key, params)
		if err != nil {
			t.Fatalf("first review: %v", err)
		}
		again, err := f.coord.ReviewTicketFor(ctx, key, params)
		if err != nil || !reflect.DeepEqual(again, first) {
			t.Fatalf("the identical repeat must return the same view: %+v, %v (first %+v)", again, err, first)
		}
		if after, _ := f.store.Ticket(ctx, ticket.ID); after.Revision != 2 {
			t.Fatalf("a repeat must not rewrite the decision: revision %d", after.Revision)
		}
		different := params
		different.Outcome = task.OutcomeRejected
		if _, err := f.coord.ReviewTicketFor(ctx, key, different); !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("a different outcome = %v, want ErrAlreadyDecided", err)
		}
		differentNote := params
		differentNote.Disposition = "some other note"
		if _, err := f.coord.ReviewTicketFor(ctx, key, differentNote); !errors.Is(err, ErrAlreadyDecided) {
			t.Fatalf("a different note = %v, want ErrAlreadyDecided", err)
		}
	})

	t.Run("validation order", func(t *testing.T) {
		ticket := f.newTicket(key, "validate")
		if _, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: "merged"}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf(`outcome "merged" = %v, want ErrInvalidRequest`, err)
		}
		if _, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{TicketID: ticket.ID, Digest: "", Outcome: task.OutcomeKept}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("a blank digest = %v, want ErrInvalidRequest", err)
		}
		if _, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{TicketID: ticket.ID, Digest: "stale", Outcome: task.OutcomeKept}); !errors.Is(err, ErrTicketChanged) {
			t.Errorf("a stale digest = %v, want ErrTicketChanged", err)
		}
		long := strings.Repeat("n", dispositionLimit+1)
		if _, err := f.coord.ReviewTicketFor(ctx, key, TicketReviewParams{TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: task.OutcomeKept, Disposition: long}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("a note over %d characters = %v, want ErrInvalidRequest", dispositionLimit, err)
		}
		if after, _ := f.store.Ticket(ctx, ticket.ID); after.Revision != 1 || after.Decided() {
			t.Fatalf("every refusal must write nothing: %+v", after)
		}
	})
}

func TestReviewTicketConcurrentDecisions(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	run := func(t *testing.T, outcomes [2]string) (results [2]error) {
		ticket := f.newTicket(key, "concurrent "+outcomes[0]+outcomes[1])
		other := f.sibling()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i, c := range []*Coordinator{f.coord, other} {
			wg.Add(1)
			go func(i int, c *Coordinator) {
				defer wg.Done()
				<-start
				_, results[i] = c.ReviewTicketFor(ctx, key, TicketReviewParams{TicketID: ticket.ID, Digest: ticket.Digest(), Outcome: outcomes[i]})
			}(i, c)
		}
		close(start)
		wg.Wait()
		return results
	}

	t.Run("the same decision succeeds for both", func(t *testing.T) {
		results := run(t, [2]string{task.OutcomeKept, task.OutcomeKept})
		if results[0] != nil || results[1] != nil {
			t.Fatalf("two identical concurrent decisions must both succeed: %v, %v", results[0], results[1])
		}
	})
	t.Run("different decisions have one winner", func(t *testing.T) {
		results := run(t, [2]string{task.OutcomeKept, task.OutcomeRejected})
		var winners, decided int
		for _, err := range results {
			switch {
			case err == nil:
				winners++
			case errors.Is(err, ErrAlreadyDecided):
				decided++
			default:
				t.Errorf("unexpected review error: %v", err)
			}
		}
		if winners != 1 || decided != 1 {
			t.Fatalf("different concurrent decisions = %d winners, %d ErrAlreadyDecided; want 1, 1", winners, decided)
		}
	})
}

// ticketDispatch dispatches one worker for a ticket through the same entry point
// the bridge uses.
func (f *coordFixture) ticketDispatch(key, ticketID, title string) (DispatchResult, error) {
	f.t.Helper()
	return f.coord.DispatchFor(context.Background(), key, DispatchParams{
		Title:        title,
		Brief:        "Brief for " + title,
		Checks:       []string{"go test ./... ok"},
		BaseRevision: f.base,
		TicketID:     ticketID,
	})
}

func TestDispatchLinksATaskToATicket(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "linked work")
	if _, err := f.coord.MoveTicketFor(ctx, key, ticket.ID, ticket.Digest()); err != nil {
		t.Fatalf("move to todo: %v", err)
	}
	result, err := f.ticketDispatch(key, ticket.ID, "worker one")
	if err != nil {
		t.Fatalf("DispatchFor with a todo ticket: %v", err)
	}
	if result.TicketID != ticket.ID {
		t.Fatalf("DispatchResult.TicketID = %q, want %q", result.TicketID, ticket.ID)
	}
	stored, err := f.store.Task(ctx, result.TaskID)
	if err != nil || stored.TicketID != ticket.ID {
		t.Fatalf("the stored task must carry the ticket id: %+v, %v", stored, err)
	}
	list, err := f.coord.ListTicketsFor(ctx, key)
	if err != nil || len(list.Tickets) != 1 {
		t.Fatalf("ListTicketsFor = %+v, %v", list, err)
	}
	if got := list.Tickets[0]; got.Stage != task.TicketInProgress || !reflect.DeepEqual(got.TaskIDs, []string{result.TaskID}) {
		t.Fatalf("the ticket must be in progress with its task listed: %+v", got)
	}
}

func TestDispatchAcceptsABacklogTicketAsTheSpecAllows(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "still in the backlog")
	result, err := f.ticketDispatch(key, ticket.ID, "early worker")
	if err != nil {
		t.Fatalf("DispatchFor with a backlog ticket: %v", err)
	}
	if result.TicketID != ticket.ID {
		t.Fatalf("DispatchResult.TicketID = %q, want %q", result.TicketID, ticket.ID)
	}
	list, err := f.coord.ListTicketsFor(ctx, key)
	if err != nil || len(list.Tickets) != 1 || list.Tickets[0].Stage != task.TicketInProgress || list.Tickets[0].Column != task.ColumnBacklog {
		t.Fatalf("a backlog ticket with an open task reads in_progress and stays in the backlog column: %+v, %v", list, err)
	}
}

func TestDispatchRefusesAnUnusableTicketAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	foreign := f.foreignTicket("tkt_foreign")
	decided := f.newTicket(key, "already decided")
	if _, err := f.store.DecideTicket(ctx, decided.ID, decided.Revision, task.OutcomeAbandoned, "no"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	for name, c := range map[string]struct {
		ticketID string
		want     error
		// says is text the refusal must carry; saysNot is text it must not (a
		// refused ticket is not about a worker report).
		says, saysNot string
	}{
		"another project's ticket": {ticketID: foreign.ID, want: ErrTicketUnknown},
		"a missing id":             {ticketID: "tkt_missing", want: ErrTicketUnknown},
		"a decided ticket":         {ticketID: decided.ID, want: ErrAlreadyDecided, says: decided.ID, saysNot: "worker report"},
	} {
		_, err := f.ticketDispatch(key, c.ticketID, "refused")
		switch {
		case !errors.Is(err, c.want):
			t.Errorf("%s: DispatchFor = %v, want %v", name, err, c.want)
		case c.says != "" && !strings.Contains(err.Error(), c.says):
			t.Errorf("%s: the refusal %q must name %q", name, err, c.says)
		case c.saysNot != "" && strings.Contains(err.Error(), c.saysNot):
			t.Errorf("%s: the refusal %q must not mention %q", name, err, c.saysNot)
		}
		if tasks, err := f.store.Tasks(ctx, key); err != nil || len(tasks) != 0 {
			t.Errorf("%s: a refused dispatch must create no task: %+v, %v", name, tasks, err)
		}
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("a refused dispatch must not reach Herdr: %+v", calls)
	}
	if after, _ := f.store.Ticket(ctx, foreign.ID); !reflect.DeepEqual(after, foreign) {
		t.Fatalf("the other project's ticket must be untouched: %+v", after)
	}
}

// The pre-check is a read; the store's gate is the authority. A ticket decided
// after the pre-check passed must still be refused, with the pre-check's answer.
func TestDispatchRefusesATicketDecidedAfterThePreCheck(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "decided mid-flight")
	request, err := newDispatchRequest("late", "Late brief.", []string{"check"}, nil, nil)
	if err != nil {
		t.Fatalf("newDispatchRequest: %v", err)
	}
	request.ticketID = ticket.ID
	if _, err := f.store.DecideTicket(ctx, ticket.ID, ticket.Revision, task.OutcomeAbandoned, "dropped"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, _, _, err := f.coord.createTask(ctx, key, request, f.base); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("createTask for a ticket decided after the pre-check = %v, want ErrAlreadyDecided", err)
	}
	if tasks, err := f.store.Tasks(ctx, key); err != nil || len(tasks) != 0 {
		t.Fatalf("no task may exist after the refusal: %+v, %v", tasks, err)
	}
}

func TestDispatchWithoutATicketIsUnchanged(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	result, err := f.ticketDispatch(key, "", "plain worker")
	if err != nil {
		t.Fatalf("DispatchFor without a ticket: %v", err)
	}
	stored, err := f.store.Task(ctx, result.TaskID)
	if err != nil || stored.TicketID != "" || result.TicketID != "" {
		t.Fatalf("a task dispatched without a ticket stays unlinked: stored %q, result %q, %v", stored.TicketID, result.TicketID, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "ticketId") {
		t.Fatalf("an unlinked result must not carry a ticketId field: %s, %v", encoded, err)
	}
}

func TestDispatchTicketWireNames(t *testing.T) {
	var params DispatchParams
	if err := json.Unmarshal([]byte(`{"title":"t","ticketId":"tkt_x"}`), &params); err != nil || params.TicketID != "tkt_x" {
		t.Fatalf("DispatchParams must read ticketId: %+v, %v", params, err)
	}
	encoded, err := json.Marshal(DispatchResult{TaskID: "orch_a", TicketID: "tkt_x"})
	if err != nil || !strings.Contains(string(encoded), `"ticketId":"tkt_x"`) {
		t.Fatalf("DispatchResult must write ticketId: %s, %v", encoded, err)
	}
}

// A ticket id that was supplied but is only whitespace is a link the caller asked
// for and the coordinator cannot honor, so it is refused. Only an absent id (the
// empty string) means "no ticket".
func TestDispatchRefusesABlankTicketIdAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	for name, blank := range map[string]string{"spaces": "   ", "tab": "\t", "newline and spaces": " \n  "} {
		if _, err := f.ticketDispatch(key, blank, "blank "+name); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: DispatchFor = %v, want ErrInvalidRequest", name, err)
		}
	}
	if tasks, err := f.store.Tasks(ctx, key); err != nil || len(tasks) != 0 {
		t.Fatalf("a refused dispatch must create no task: %+v, %v", tasks, err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("a refused dispatch must not reach Herdr: %+v", calls)
	}
}
