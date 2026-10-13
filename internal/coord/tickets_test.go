package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/herdr"
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

// acceptedPlanner records a ticket's planner whose report the user accepted: a
// closed plan-role task, which is what lets linked implementation work start.
func (f *coordFixture) acceptedPlanner(project, ticketID string) task.Task {
	f.t.Helper()
	x := task.Task{
		ID: "orch_plan_" + ticketID, ProjectKey: project, TicketID: ticketID, Role: task.TaskRolePlan,
		Title: "Plan: " + ticketID, Phase: task.PromptSubmitted,
		Outcome: task.OutcomeIntegrated, Disposition: "accepted the plan",
	}
	if err := f.store.CreateTask(context.Background(), x); err != nil {
		f.t.Fatalf("seed the accepted planner of %s: %v", ticketID, err)
	}
	return x
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
	safeBody := strings.Repeat("b", 10_000)
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "big", Body: safeBody}); err != nil {
		t.Fatalf("a body that fits the bounded planner brief: %v", err)
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

func TestCreateTicketUsesUTF16CharacterLimits(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: strings.Repeat("界", 160)}); err != nil {
		t.Fatalf("160-character CJK title rejected: %v", err)
	}
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: strings.Repeat("界", 161)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("161-character CJK title error = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "body", Body: strings.Repeat("界", 4000)}); err != nil {
		t.Fatalf("multibyte body within both limits rejected: %v", err)
	}
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "body", Body: strings.Repeat("界", 16001)}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("16001-character CJK body error = %v, want ErrInvalidRequest", err)
	}
}

func TestCreateTicketRejectsPlannerBriefThatExceedsWindowsArgumentBudget(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	body := strings.Repeat(`"`, 12_000)
	if len(body) > maxBriefLength {
		t.Fatalf("test body has %d bytes, beyond the ticket body limit", len(body))
	}
	_, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "quoted context", Body: body})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("CreateTicketFor with an argv-expanding body = %v, want ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "shorten the ticket body") {
		t.Fatalf("the refusal must say what to shorten: %v", err)
	}
	if tickets, err := f.store.Tickets(ctx, key); err != nil || len(tickets) != 0 {
		t.Fatalf("an oversized planner context must not be persisted: %+v, %v", tickets, err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("rejecting an oversized planner context must not reach Herdr: %+v", calls)
	}
	legacy := task.Ticket{
		ID: "tkt_legacyoversize", ProjectKey: key, Title: "legacy quoted context",
		Body: body, Column: task.ColumnBacklog,
	}
	if err := f.store.CreateTicket(ctx, legacy); err != nil {
		t.Fatalf("seed an oversized pre-existing ticket: %v", err)
	}
	storedLegacy, err := f.store.Ticket(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("read the oversized pre-existing ticket: %v", err)
	}
	if _, err := f.coord.MoveTicketFor(ctx, key, storedLegacy.ID, storedLegacy.Digest()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("MoveTicketFor with an argv-expanding legacy ticket = %v, want ErrInvalidRequest", err)
	}
	after, err := f.store.Ticket(ctx, legacy.ID)
	if err != nil || after.Column != task.ColumnBacklog || after.Revision != 1 {
		t.Fatalf("a refused move must leave the legacy ticket in backlog: %+v, %v", after, err)
	}
	if tasks, err := f.store.Tasks(ctx, key); err != nil || len(tasks) != 0 {
		t.Fatalf("a refused legacy move must not record a planner: %+v, %v", tasks, err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("refusing the legacy move must not reach Herdr: %+v", calls)
	}
}

func TestListTicketsForShowsOnlyThisProjectsTicketsWithDerivedStage(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	first := f.newTicket(key, "first")
	time.Sleep(3 * time.Millisecond)
	second := f.newTicket(key, "second")
	f.foreignTicket("tkt_foreign")
	planner := f.acceptedPlanner(key, first.ID)
	time.Sleep(3 * time.Millisecond)
	f.linkedTask(key, first.ID, "orch_one")
	f.linkedTask(key, first.ID, "orch_two")

	list, err := f.coord.ListTicketsFor(ctx, key, TicketListParams{})
	if err != nil {
		t.Fatalf("ListTicketsFor: %v", err)
	}
	if len(list.Tickets) != 2 || list.Tickets[0].TicketID != first.ID || list.Tickets[1].TicketID != second.ID {
		t.Fatalf("want this project's two tickets, oldest first: %+v", list.Tickets)
	}
	if got := list.Tickets[0]; got.Stage != task.TicketInProgress || !reflect.DeepEqual(got.TaskIDs, []string{planner.ID, "orch_one", "orch_two"}) {
		t.Fatalf("a ticket with an accepted plan and open linked tasks is in progress and lists them oldest first: %+v", got)
	}
	if got := list.Tickets[1]; got.Stage != task.TicketBacklog || len(got.TaskIDs) != 0 {
		t.Fatalf("an untouched ticket is backlog with no tasks: %+v", got)
	}

	f.closeTask("orch_one")
	f.closeTask("orch_two")
	list, err = f.coord.ListTicketsFor(ctx, key, TicketListParams{})
	if err != nil || list.Tickets[0].Stage != task.TicketReadyForReview {
		t.Fatalf("every linked task closed reads ready for review: %+v, %v", list.Tickets, err)
	}
	if _, err := f.coord.ListTicketsFor(ctx, "E:/never/bound", TicketListParams{}); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("an unbound project key = %v, want ErrProjectUnknown", err)
	}
}

func TestListTicketsForCannotOverflowBridgeResponse(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	body := strings.Repeat("<", 1500)
	for i := range 120 {
		if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "large ticket", Body: body}); err != nil {
			t.Fatalf("CreateTicketFor large ticket %d: %v", i, err)
		}
	}
	got, err := f.coord.ListTicketsFor(ctx, key, TicketListParams{Limit: InspectMaxLimit})
	if err != nil {
		t.Fatalf("ListTicketsFor: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode ticket list: %v", err)
	}
	if len(encoded) > 1<<20 {
		t.Fatalf("ticket list is %d bytes, over bridge's 1 MiB response bound", len(encoded))
	}
	var metadata struct {
		Total     int  `json:"total"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		t.Fatalf("decode ticket list metadata: %v", err)
	}
	if metadata.Total != 120 || !metadata.Truncated {
		t.Fatalf("bounded list metadata = total %d, truncated %v; want total 120 and truncated", metadata.Total, metadata.Truncated)
	}
}

func TestListTicketsForUsesDefaultRowLimit(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	for range InspectDefaultLimit + 1 {
		f.newTicket(key, "ticket")
	}
	got, err := f.coord.ListTicketsFor(ctx, key, TicketListParams{})
	if err != nil {
		t.Fatalf("ListTicketsFor: %v", err)
	}
	if len(got.Tickets) > InspectDefaultLimit {
		t.Fatalf("default list returned %d tickets, over the %d row limit", len(got.Tickets), InspectDefaultLimit)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode ticket list metadata: %v", err)
	}
	var metadata struct {
		Total     int  `json:"total"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		t.Fatalf("decode ticket list metadata: %v", err)
	}
	if metadata.Total != InspectDefaultLimit+1 || !metadata.Truncated {
		t.Fatalf("default list metadata = total %d, truncated %v; want total %d and truncated",
			metadata.Total, metadata.Truncated, InspectDefaultLimit+1)
	}
}

func TestListTicketsHonorsRequestedRowLimit(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	for range 3 {
		f.newTicket(key, "ticket")
	}
	got, err := f.coord.ListTicketsFor(ctx, key, TicketListParams{Limit: 2})
	if err != nil {
		t.Fatalf("ListTicketsFor: %v", err)
	}
	if len(got.Tickets) != 2 || got.Total != 3 || !got.Truncated {
		t.Fatalf("explicit limit result = %d tickets, total %d, truncated %v; want 2, 3, true",
			len(got.Tickets), got.Total, got.Truncated)
	}
}

func TestListTicketsRefusesInvalidLimits(t *testing.T) {
	f, key := ticketFixture(t)
	for name, limit := range map[string]int{
		"negative":  -1,
		"too large": InspectMaxLimit + 1,
	} {
		if _, err := f.coord.ListTicketsFor(context.Background(), key, TicketListParams{Limit: limit}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s limit: ListTicketsFor = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestListTicketsTruncatesBodyAndLinkedTaskIDs(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	body := strings.Repeat("é", inspectTextLimit+1)
	created, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "ticket", Body: body})
	if err != nil {
		t.Fatalf("CreateTicketFor: %v", err)
	}
	f.acceptedPlanner(key, created.TicketID)
	for i := range InspectMaxLimit + 1 {
		f.linkedTask(key, created.TicketID, fmt.Sprintf("orch_%03d", i))
	}
	got, err := f.coord.ListTicketsFor(ctx, key, TicketListParams{})
	if err != nil {
		t.Fatalf("ListTicketsFor: %v", err)
	}
	entry := got.Tickets[0]
	if entry.Body != strings.Repeat("é", inspectTextLimit)+"…" {
		t.Errorf("listed body has %d runes; want %d and an ellipsis", len([]rune(entry.Body)), inspectTextLimit+1)
	}
	if len(entry.TaskIDs) != InspectMaxLimit {
		t.Errorf("listed task ids = %d, want the bounded %d", len(entry.TaskIDs), InspectMaxLimit)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode ticket list: %v", err)
	}
	var metadata struct {
		Tickets []struct {
			BodyTruncated    bool `json:"bodyTruncated"`
			TaskIDsTruncated bool `json:"taskIdsTruncated"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		t.Fatalf("decode ticket list truncation metadata: %v", err)
	}
	if len(metadata.Tickets) != 1 || !metadata.Tickets[0].BodyTruncated || !metadata.Tickets[0].TaskIDsTruncated {
		t.Fatalf("ticket truncation metadata = %+v, want both fields marked", metadata.Tickets)
	}
}

func TestMoveTicketRecordsTodoAndStartsOnePlanner(t *testing.T) {
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
	if moved.Column != task.ColumnTodo || moved.Stage != task.TicketPlanning || moved.Revision != 2 || moved.Digest == preview.Digest {
		t.Fatalf("a move records todo and starts planning at revision 2: %+v", moved)
	}
	tasks, err := f.store.Tasks(ctx, key)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("a move must record one planner task: %+v, %v", tasks, err)
	}
	planner := tasks[0]
	if planner.Role != task.TaskRolePlan || planner.TicketID != ticket.ID || planner.Title != "Plan: move me" {
		t.Fatalf("planner facts = %+v, want a plan task linked to the moved ticket", planner)
	}
	if planner.Phase != task.PromptSubmitted || len(planner.ReservedPaths) != 0 ||
		!strings.Contains(planner.Brief, ticket.Title) || !strings.Contains(planner.Brief, ticket.Body) ||
		!strings.Contains(strings.ToLower(planner.Brief), "do not edit") {
		t.Fatalf("planner must be briefed without reservations and told to plan only: %+v", planner)
	}
	if !reflect.DeepEqual(moved.TaskIDs, []string{planner.ID}) {
		t.Fatalf("moved ticket task ids = %v, want its planner %s", moved.TaskIDs, planner.ID)
	}
	if starts := f.countCalls("agent start"); starts != 1 {
		t.Fatalf("planner agent starts = %d, want one: %v", starts, f.calls())
	}
}

// The move and its planner intent commit together, and starting the planner is a
// later step. When that step fails the ticket has still moved, so the answer is
// the moved ticket with ErrPlannerNotStarted naming the planner - never a failed
// move, which a retry of the confirmed digest could only meet as ticket_changed.
// The planner stays a retryable intent for a later drain.
func TestMoveTicketReportsAMoveWhosePlannerDidNotStartAsMoved(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "planner cannot start")
	f.updatePlan(func(p *fixturePlan) { p.WorktreeCreateError = "not_git_worktree" })
	moved, err := f.coord.MoveTicketFor(ctx, key, ticket.ID, ticket.Digest())
	if !errors.Is(err, ErrPlannerNotStarted) || !errors.Is(err, herdr.ErrCLI) {
		t.Fatalf("a move whose planner could not start = %v, want ErrPlannerNotStarted wrapping the Herdr failure", err)
	}
	tasks, readErr := f.store.Tasks(ctx, key)
	if readErr != nil || len(tasks) != 1 || tasks[0].Role != task.TaskRolePlan || tasks[0].TicketID != ticket.ID {
		t.Fatalf("the move must keep its one planner task: %+v, %v", tasks, readErr)
	}
	planner := tasks[0]
	var notStarted *PlannerNotStartedError
	if !errors.As(err, &notStarted) || notStarted.TicketID != ticket.ID || notStarted.TaskID != planner.ID {
		t.Fatalf("the error must name the moved ticket and its planner %s: %#v", planner.ID, err)
	}
	if moved.TicketID != ticket.ID || moved.Column != task.ColumnTodo || moved.Stage != task.TicketPlanning ||
		moved.Revision != 2 || !reflect.DeepEqual(moved.TaskIDs, []string{planner.ID}) {
		t.Fatalf("the answer must be the moved ticket with its planner: %+v", moved)
	}
	if stored, _ := f.store.Ticket(ctx, ticket.ID); stored.Column != task.ColumnTodo || stored.Revision != 2 {
		t.Fatalf("the move must stay recorded: %+v", stored)
	}
	if planner.Phase != task.Intent || planner.Blocker != "" {
		t.Fatalf("the planner must stay a retryable intent for a later drain: %+v", planner)
	}
}

func TestMoveTicketAndPlannerIntentRollBackTogether(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "atomic move")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER refuse_planner_intent
		BEFORE INSERT ON tasks
		WHEN json_extract(NEW.data, '$.Role') = 'plan'
		BEGIN SELECT RAISE(ABORT, 'planner intent is not writable'); END`); err != nil {
		t.Fatalf("install planner-intent trigger: %v", err)
	}
	_, err = f.coord.MoveTicketFor(ctx, key, ticket.ID, ticket.Digest())
	if err == nil {
		t.Fatal("the move succeeded although its planner intent could not be recorded")
	}
	if after, readErr := f.store.Ticket(ctx, ticket.ID); readErr != nil ||
		after.Column != task.ColumnBacklog || after.Revision != 1 {
		t.Fatalf("a refused planner intent must leave the ticket in backlog: %+v, %v", after, readErr)
	}
	if tasks, readErr := f.store.Tasks(ctx, key); readErr != nil || len(tasks) != 0 {
		t.Fatalf("a refused move must leave no planner task: %+v, %v", tasks, readErr)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("a failed atomic move must reach no Herdr operation: %+v", calls)
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
	if _, err := f.store.DecideTicket(ctx, decided.ID, decided.Revision, task.OutcomeAbandoned, "no", ""); err != nil {
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
	if final, err := f.store.Ticket(ctx, ticket.ID); err != nil || final.Revision != 2 || final.Column != task.ColumnTodo {
		t.Fatalf("final ticket = %+v, %v; want todo at revision 2", final, err)
	}
	if tasks, err := f.store.Tasks(ctx, key); err != nil || len(tasks) != 1 || tasks[0].Role != task.TaskRolePlan {
		t.Fatalf("one winning move must leave exactly one planner intent: %+v, %v", tasks, err)
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
		f.acceptedPlanner(key, ticket.ID)
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

func TestDispatchLinksATaskToATicketAfterItsPlanIsAccepted(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "linked work")
	moved, err := f.coord.MoveTicketFor(ctx, key, ticket.ID, ticket.Digest())
	if err != nil {
		t.Fatalf("move to todo: %v", err)
	}
	if len(moved.TaskIDs) != 1 {
		t.Fatalf("the moved ticket must name its planner: %+v", moved)
	}
	planner, err := f.store.Task(ctx, moved.TaskIDs[0])
	if err != nil {
		t.Fatalf("read planner: %v", err)
	}
	beforeCalls := len(f.calls())
	if _, err := f.ticketDispatch(key, ticket.ID, "worker one"); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "plan") {
		t.Fatalf("implementation dispatch before plan approval = %v, want a plan refusal", err)
	}
	if tasks, readErr := f.store.Tasks(ctx, key); readErr != nil || len(tasks) != 1 {
		t.Fatalf("a refused implementation dispatch must create nothing: %+v, %v", tasks, readErr)
	}
	if calls := f.calls(); len(calls) != beforeCalls {
		t.Fatalf("a refused implementation dispatch must reach no Herdr operation: %+v", calls[beforeCalls:])
	}
	f.closeTask(planner.ID)
	result, err := f.ticketDispatch(key, ticket.ID, "worker one")
	if err != nil {
		t.Fatalf("DispatchFor with an accepted plan: %v", err)
	}
	if result.TicketID != ticket.ID {
		t.Fatalf("DispatchResult.TicketID = %q, want %q", result.TicketID, ticket.ID)
	}
	stored, err := f.store.Task(ctx, result.TaskID)
	if err != nil || stored.TicketID != ticket.ID {
		t.Fatalf("the implementation task must carry the ticket id: %+v, %v", stored, err)
	}
	list, err := f.coord.ListTicketsFor(ctx, key, TicketListParams{})
	if err != nil || len(list.Tickets) != 1 {
		t.Fatalf("ListTicketsFor = %+v, %v", list, err)
	}
	if got := list.Tickets[0]; got.Stage != task.TicketInProgress ||
		!reflect.DeepEqual(got.TaskIDs, []string{planner.ID, result.TaskID}) {
		t.Fatalf("ticket must show its plan and implementation task in progress: %+v", got)
	}
}

// A ticket's linked work starts only after its planner's report was accepted
// (spec §5): a backlog ticket has no planner yet, and a todo ticket recorded
// before planners existed never had one, so both are refused before anything is
// recorded or reaches Herdr.
func TestDispatchRefusesATicketWithoutAnAcceptedPlan(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	backlog := f.newTicket(key, "still in the backlog")
	legacy := f.newTicket(key, "todo before planners")
	if _, err := f.store.UpdateTicket(ctx, legacy.ID, legacy.Revision, func(next *task.Ticket) error {
		next.Column = task.ColumnTodo
		return nil
	}); err != nil {
		t.Fatalf("seed a todo ticket without a planner: %v", err)
	}
	for name, ticketID := range map[string]string{"a backlog ticket": backlog.ID, "a todo ticket without a planner": legacy.ID} {
		_, err := f.ticketDispatch(key, ticketID, "early worker for "+name)
		if !errors.Is(err, ErrTicketPlanNotAccepted) || !strings.Contains(err.Error(), ticketID) {
			t.Errorf("%s: DispatchFor = %v, want ErrTicketPlanNotAccepted naming %s", name, err, ticketID)
		}
	}
	if tasks, err := f.store.Tasks(ctx, key); err != nil || len(tasks) != 0 {
		t.Fatalf("a refused dispatch must create no task: %+v, %v", tasks, err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Fatalf("a refused dispatch must not reach Herdr: %+v", calls)
	}
}

func TestDispatchRefusesAnUnusableTicketAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	foreign := f.foreignTicket("tkt_foreign")
	decided := f.newTicket(key, "already decided")
	if _, err := f.store.DecideTicket(ctx, decided.ID, decided.Revision, task.OutcomeAbandoned, "no", ""); err != nil {
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
	if _, err := f.store.DecideTicket(ctx, ticket.ID, ticket.Revision, task.OutcomeAbandoned, "dropped", ""); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, _, err := f.coord.recordTaskIntent(ctx, key, request, f.base); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("recordTaskIntent for a ticket decided after the pre-check = %v, want ErrAlreadyDecided", err)
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

func TestQueuedTicketPlannerDrainsWithoutChecks(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	workers := make([]task.Task, 0, 3)
	for _, title := range []string{"slot owner a", "slot owner b", "slot owner c"} {
		workers = append(workers, f.mustDispatch(title, "working"))
	}
	ticket := f.newTicket(key, "queued planner")
	moved, err := f.coord.MoveTicketFor(ctx, key, ticket.ID, ticket.Digest())
	if err != nil {
		t.Fatalf("MoveTicketFor with a full concurrency budget: %v", err)
	}
	if moved.Stage != task.TicketPlanning {
		t.Fatalf("queued ticket stage = %q, want planning", moved.Stage)
	}
	tasks, err := f.store.Tasks(ctx, key)
	if err != nil {
		t.Fatalf("Tasks after move: %v", err)
	}
	var planner task.Task
	for _, st := range tasks {
		if st.Role == task.TaskRolePlan {
			planner = st
		}
	}
	if planner.ID == "" || planner.Phase != task.Intent || len(planner.Checks) != 0 || len(tasks) != 4 {
		t.Fatalf("full capacity must leave one queued planner intent without worker checks: %+v", tasks)
	}
	if starts := f.countCalls("agent start"); starts != 3 {
		t.Fatalf("planner must wait for capacity; agent starts = %d, want 3", starts)
	}

	f.closeTask(workers[0].ID)
	drained, err := f.coord.DrainQueued(ctx, key)
	if err != nil {
		t.Fatalf("DrainQueued after a slot opens: %v", err)
	}
	if len(drained) != 1 || drained[0].ID != planner.ID || drained[0].Phase != task.PromptSubmitted {
		t.Fatalf("drained tasks = %+v, want planner %s to be submitted", drained, planner.ID)
	}
	if starts := f.countCalls("agent start"); starts != 4 {
		t.Fatalf("planner starts after a slot opens = %d, want 4", starts)
	}
}

// The planner brief is budgeted in bytes, so multibyte text reaches the budget at
// about a third of the characters ASCII does, and creation must count the same way.
func TestCreateTicketBudgetsThePlannerBriefInBytes(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	if _, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "cjk body", Body: strings.Repeat("界", 4_000)}); err != nil {
		t.Fatalf("a 4000-character CJK body fits the planner budget, about 12 KB of text: %v", err)
	}
	_, err := f.coord.CreateTicketFor(ctx, key, TicketCreateParams{Title: "cjk body", Body: strings.Repeat("界", 5_000)})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "shorten the ticket body") {
		t.Fatalf("a 5000-character CJK body = %v, want ErrInvalidRequest that says what to shorten", err)
	}
}
