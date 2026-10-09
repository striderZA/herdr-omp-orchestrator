package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/task"
)

var ticketMethods = []string{
	MethodTicketCreate, MethodTicketList, MethodTicketMovePreview, MethodTicketMove, MethodTicketReview,
}

const foreignTicketProject = "E:/elsewhere/other-repo"

// primaryTicketBridge is a bound, confirmed primary with a real coordinator
// behind the bridge. Ticket operations never touch Herdr, so no stand-in CLI is
// scripted.
func primaryTicketBridge(t *testing.T) (*bridgeFixture, Ops) {
	t.Helper()
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.asPrimary()
	return f, CoordinatorOps(f.coordinator(t))
}

func (f *bridgeFixture) ticketCall(ops Ops, method, params string) bridgeResponse {
	f.t.Helper()
	return f.callWith(method, primarySession, params, ops)
}

func (f *bridgeFixture) ticketView(ops Ops, method, params string) coord.TicketView {
	f.t.Helper()
	resp := f.ticketCall(ops, method, params)
	var view coord.TicketView
	resp.decodeResult(f.t, &view)
	return view
}

func ticketBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	return string(encoded)
}

func (f *bridgeFixture) foreignTicket(id string) task.Ticket {
	f.t.Helper()
	ticket := task.Ticket{ID: id, ProjectKey: foreignTicketProject, Title: "someone else's", Column: task.ColumnBacklog}
	if err := f.store.CreateTicket(context.Background(), ticket); err != nil {
		f.t.Fatalf("seed the foreign ticket: %v", err)
	}
	stored, err := f.store.Ticket(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read the foreign ticket: %v", err)
	}
	return stored
}

func TestBridgeTicketMethodsArePrimaryOnly(t *testing.T) {
	ctx := context.Background()
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)

	calls := 0
	var seen Principal
	count := func(_ context.Context, p Principal, _ json.RawMessage) (any, error) {
		calls++
		seen = p
		return map[string]any{"accepted": true}, nil
	}
	stub := Ops{TicketCreate: count, TicketList: count, TicketMovePreview: count, TicketMove: count, TicketReview: count}
	bodies := map[string]string{
		MethodTicketCreate:      `{"title":"x"}`,
		MethodTicketList:        `{}`,
		MethodTicketMovePreview: `{"ticketId":"tkt_x"}`,
		MethodTicketMove:        `{"ticketId":"tkt_x","digest":"d"}`,
		MethodTicketReview:      `{"ticketId":"tkt_x","digest":"d","outcome":"abandoned"}`,
	}

	// A confirmed, bound worker may not touch a ticket.
	f.env["HERDR_PANE_ID"] = workerPane
	for _, method := range ticketMethods {
		wantError(t, f.callWith(method, workerSession, bodies[method], stub), CodeForbidden)
	}

	// A primary whose handshake is announced but not yet confirmed is refused too.
	if err := f.store.RecordReady(ctx, primaryPane, announcedID(primarySession), 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	f.env["HERDR_PANE_ID"] = primaryPane
	for _, method := range ticketMethods {
		wantError(t, f.callWith(method, primarySession, bodies[method], stub), CodeNotReady)
	}
	if calls != 0 {
		t.Fatalf("a refused caller reached the coordinator %d times", calls)
	}

	// The confirmed primary reaches every operation, with the verified principal.
	f.confirm(primaryPane, primarySession)
	for _, method := range ticketMethods {
		if resp := f.callWith(method, primarySession, bodies[method], stub); !resp.OK {
			t.Fatalf("%s failed for the confirmed primary: %+v", method, resp.Error)
		}
	}
	if calls != len(ticketMethods) {
		t.Fatalf("the operations ran %d times, want %d", calls, len(ticketMethods))
	}
	if seen.Role != "primary" || seen.ProjectKey != f.repoRoot || seen.LaunchPaneID != primaryPane {
		t.Fatalf("the coordinator must see the verified principal: %+v", seen)
	}
}

func TestBridgeTicketMethodsWithoutWiringAreAuthorizedThenUnavailable(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.asPrimary()
	for _, method := range ticketMethods {
		// Authorized, but no operation is wired: not_implemented, never a bypass.
		wantError(t, f.callWith(method, primarySession, `{}`, Ops{}), CodeNotImplemented)
		// A build whose coordinator did not open answers with the cause.
		wantError(t, f.callWith(method, primarySession, `{}`, UnavailableOps(errors.New("no coordinator"))), CodeHerdrUnavailable)
	}
}

func TestBridgeTicketPreflightAndReviewRoundTrip(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)

	created := f.ticketView(ops, MethodTicketCreate, `{"title":"Add search","body":"find things"}`)
	if created.Column != task.ColumnBacklog || created.Stage != task.TicketBacklog || !strings.HasPrefix(created.TicketID, "tkt_") {
		t.Fatalf("created = %+v, want a backlog ticket", created)
	}
	second := f.ticketView(ops, MethodTicketCreate, `{"title":"Add logging"}`)

	var listed coord.TicketListResult
	f.ticketCall(ops, MethodTicketList, `{"limit":1}`).decodeResult(t, &listed)
	if len(listed.Tickets) != 1 || listed.Total != 2 || !listed.Truncated ||
		(listed.Tickets[0].TicketID != created.TicketID && listed.Tickets[0].TicketID != second.TicketID) ||
		listed.Tickets[0].Stage != task.TicketBacklog {
		t.Fatalf("list = %+v, want one bounded backlog ticket from total two", listed)
	}
	wantError(t, f.ticketCall(ops, MethodTicketList, `{"limit":-1}`), "bad_request")

	preview := f.ticketView(ops, MethodTicketMovePreview, ticketBody(t, map[string]any{"ticketId": created.TicketID}))
	if preview.Digest == "" || preview.Digest != created.Digest {
		t.Fatalf("preview digest %q must be the ticket's confirmation token %q", preview.Digest, created.Digest)
	}
	wantError(t, f.ticketCall(ops, MethodTicketMove, ticketBody(t, map[string]any{
		"ticketId": created.TicketID, "digest": preview.Digest,
	})), "bad_base_revision")
	if after, err := f.store.Ticket(ctx, created.TicketID); err != nil ||
		after.Column != task.ColumnBacklog || after.Revision != 1 {
		t.Fatalf("a move without a committed project HEAD must leave the ticket unchanged: %+v, %v", after, err)
	}
	if tasks, err := f.store.Tasks(ctx, f.repoRoot); err != nil || len(tasks) != 0 {
		t.Fatalf("a refused bridge move must leave no planner task: %+v, %v", tasks, err)
	}

	review := f.ticketView(ops, MethodTicketMovePreview, ticketBody(t, map[string]any{"ticketId": created.TicketID}))
	closed := f.ticketView(ops, MethodTicketReview, ticketBody(t, map[string]any{
		"ticketId": created.TicketID, "digest": review.Digest, "outcome": "abandoned", "disposition": "not needed",
	}))
	if closed.Stage != task.TicketDone || closed.Outcome != "abandoned" || closed.Disposition != "not needed" {
		t.Fatalf("closed = %+v, want done/abandoned", closed)
	}
}

// A move the coordinator recorded answers ok even when its planner could not
// start afterwards (here the stand-in Herdr refuses to create the worktree): the
// answer is the moved ticket plus why its planner has not started, so the primary
// neither reports a failed move nor retries a confirmed one, which the changed
// digest would refuse as ticket_changed. The field is additive; the protocol
// stays 1.
func TestBridgeTicketMoveAnswersAMoveWhosePlannerDidNotStart(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)
	bridgeCommittedRepo(t, f.repoRoot)
	created := f.ticketView(ops, MethodTicketCreate, `{"title":"Plan search","body":"find things"}`)
	resp := f.ticketCall(ops, MethodTicketMove, ticketBody(t, map[string]any{"ticketId": created.TicketID, "digest": created.Digest}))
	var answer struct {
		coord.TicketView
		PlannerNotStarted string `json:"plannerNotStarted"`
	}
	resp.decodeResult(t, &answer)
	tasks, err := f.store.Tasks(ctx, f.repoRoot)
	if err != nil || len(tasks) != 1 || tasks[0].Role != task.TaskRolePlan {
		t.Fatalf("the move must keep its one planner task: %+v, %v", tasks, err)
	}
	if answer.TicketID != created.TicketID || answer.Column != task.ColumnTodo ||
		!reflect.DeepEqual(answer.TaskIDs, []string{tasks[0].ID}) || !strings.Contains(answer.PlannerNotStarted, tasks[0].ID) {
		t.Fatalf("the answer must be the moved ticket and why its planner %s did not start: %+v", tasks[0].ID, answer)
	}
}

func TestBridgeDispatchRefusesAnUnacceptedTicketPlan(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)
	created := f.ticketView(ops, MethodTicketCreate, `{"title":"Add search","body":"find things"}`)
	planner := task.Task{
		ID: "orch_plan", ProjectKey: f.repoRoot, Title: "Plan: Add search",
		TicketID: created.TicketID, Role: task.TaskRolePlan, Phase: task.PromptSubmitted,
	}
	if err := f.store.CreateTask(ctx, planner); err != nil {
		t.Fatalf("seed an open planner task: %v", err)
	}
	response := f.ticketCall(ops, MethodDispatch, ticketBody(t, map[string]any{
		"title": "implement search", "brief": "Add the search feature.",
		"checks": []string{"go test ./..."}, "baseRevision": "0123456", "ticketId": created.TicketID,
	}))
	wantError(t, response, "ticket_plan_not_accepted")
	if tasks, err := f.store.Tasks(ctx, f.repoRoot); err != nil || len(tasks) != 1 || tasks[0].ID != planner.ID {
		t.Fatalf("dispatch before plan approval must create no implementation task: %+v, %v", tasks, err)
	}
}

func TestBridgeTicketRefusalsAreCoded(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)
	foreign := f.foreignTicket("tkt_foreign")

	// A foreign id and a missing id are the same code on every method that names one.
	for _, id := range []string{foreign.ID, "tkt_missing"} {
		wantError(t, f.ticketCall(ops, MethodTicketMovePreview, ticketBody(t, map[string]any{"ticketId": id})), "ticket_unknown")
		wantError(t, f.ticketCall(ops, MethodTicketMove, ticketBody(t, map[string]any{"ticketId": id, "digest": foreign.Digest()})), "ticket_unknown")
		wantError(t, f.ticketCall(ops, MethodTicketReview, ticketBody(t, map[string]any{
			"ticketId": id, "digest": foreign.Digest(), "outcome": "abandoned",
		})), "ticket_unknown")
	}
	if after, _ := f.store.Ticket(ctx, foreign.ID); after != foreign {
		t.Fatalf("the other project's ticket must be untouched: %+v", after)
	}

	// A digest from before another ticket edit is refused as changed.
	moving := f.ticketView(ops, MethodTicketCreate, `{"title":"moves once"}`)
	if _, err := f.store.UpdateTicket(ctx, moving.TicketID, moving.Revision, func(next *task.Ticket) error {
		next.Title = "moves once edited"
		return nil
	}); err != nil {
		t.Fatalf("edit the ticket: %v", err)
	}
	move := ticketBody(t, map[string]any{"ticketId": moving.TicketID, "digest": moving.Digest})
	wantError(t, f.ticketCall(ops, MethodTicketMove, move), "ticket_changed")

	// A decision with an open linked task is refused, and writes nothing. Linked
	// work needs the ticket's plan accepted first.
	busy := f.ticketView(ops, MethodTicketCreate, `{"title":"has work"}`)
	accepted := task.Task{
		ID: "orch_plan_busy", ProjectKey: f.repoRoot, TicketID: busy.TicketID, Role: task.TaskRolePlan,
		Title: "Plan: has work", Phase: task.PromptSubmitted, Outcome: task.OutcomeIntegrated, Disposition: "accepted the plan",
	}
	if err := f.store.CreateTask(ctx, accepted); err != nil {
		t.Fatalf("seed the accepted planner: %v", err)
	}
	linked := task.Task{ID: "orch_linked", ProjectKey: f.repoRoot, TicketID: busy.TicketID, Title: "work", Phase: task.PromptSubmitted}
	if err := f.store.CreateTask(ctx, linked); err != nil {
		t.Fatalf("seed a linked task: %v", err)
	}
	wantError(t, f.ticketCall(ops, MethodTicketReview, ticketBody(t, map[string]any{
		"ticketId": busy.TicketID, "digest": busy.Digest, "outcome": "integrated",
	})), "ticket_open")
	if after, _ := f.store.Ticket(ctx, busy.TicketID); after.Decided() {
		t.Fatalf("a refused review must write nothing: %+v", after)
	}

	// A blank digest is a malformed request.
	wantError(t, f.ticketCall(ops, MethodTicketMove, ticketBody(t, map[string]any{"ticketId": busy.TicketID, "digest": ""})), "bad_request")

	// Dispatch naming a ticket the caller cannot use: unknown, decided, or
	// without an accepted plan.
	dispatch := func(ticketID string) bridgeResponse {
		return f.ticketCall(ops, MethodDispatch, ticketBody(t, map[string]any{
			"title": "t", "brief": "b", "checks": []string{"go test ./..."}, "baseRevision": "0123456", "ticketId": ticketID,
		}))
	}
	wantError(t, dispatch(foreign.ID), "ticket_unknown")
	wantError(t, dispatch("tkt_missing"), "ticket_unknown")
	done := f.ticketView(ops, MethodTicketCreate, `{"title":"closed"}`)
	f.ticketView(ops, MethodTicketReview, ticketBody(t, map[string]any{"ticketId": done.TicketID, "digest": done.Digest, "outcome": "abandoned"}))
	wantError(t, dispatch(done.TicketID), "already_decided")
	wantError(t, dispatch(moving.TicketID), "ticket_plan_not_accepted")
	if tasks, err := f.store.Tasks(ctx, f.repoRoot); err != nil || len(tasks) != 2 {
		t.Fatalf("a refused dispatch must create no task beyond the seeded planner and linked task: %+v, %v", tasks, err)
	}
}

// The project, like the task and the pane, comes from persisted facts. A body
// that names one changes nothing.
func TestBridgeTicketCreateIgnoresIdentityFieldsInTheBody(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)
	foreign := f.foreignTicket("tkt_foreign")

	created := f.ticketView(ops, MethodTicketCreate, ticketBody(t, map[string]any{
		"title": "mine", "ticketId": foreign.ID, "projectKey": foreignTicketProject, "column": "todo", "revision": 99,
	}))
	if created.TicketID == foreign.ID || created.Column != task.ColumnBacklog || created.Revision != 1 {
		t.Fatalf("created = %+v; the body's id, column and revision must be ignored", created)
	}
	stored, err := f.store.Ticket(ctx, created.TicketID)
	if err != nil || stored.ProjectKey != f.repoRoot {
		t.Fatalf("the ticket belongs to the verified principal's project: %+v, %v", stored, err)
	}
	if after, _ := f.store.Ticket(ctx, foreign.ID); after != foreign {
		t.Fatalf("the other project's ticket must be untouched: %+v", after)
	}
}

func TestBridgeProtocolVersionIsPreserved(t *testing.T) {
	if Protocol != 1 {
		t.Fatalf("Protocol = %d: the ticket methods are additive and the version stays 1", Protocol)
	}
	f, ops := primaryTicketBridge(t)
	for _, method := range ticketMethods {
		payload := `{"version":2,"method":"` + method + `","ompSessionID":"` + primarySession + `","params":{}}`
		out, err := HandleWith(context.Background(), f.env, strings.NewReader(payload), ops)
		if err != nil {
			t.Fatalf("HandleWith(%s): %v", method, err)
		}
		wantError(t, decodeResponse(t, out), CodeRequestVersion)
	}
	if resp := f.ticketCall(ops, MethodTicketList, `{}`); !resp.OK {
		t.Fatalf("version 1 still works: %+v", resp.Error)
	}
}

// A lost response must be safe to retry. After the decision is recorded the
// extension asks the user again and sends the same decision with the ticket's
// current digest; the coordinator answers with the recorded view. A different
// decision is refused. The extension's preview carries no outcome or note, so it
// cannot tell these apart: the answer has to come from here.
func TestBridgeTicketReviewRetryAfterALostResponse(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)
	created := f.ticketView(ops, MethodTicketCreate, `{"title":"decide once"}`)
	decide := func(digest, outcome string) string {
		return ticketBody(t, map[string]any{
			"ticketId": created.TicketID, "digest": digest, "outcome": outcome, "disposition": "not needed",
		})
	}

	first := f.ticketView(ops, MethodTicketReview, decide(created.Digest, "abandoned"))
	if first.Stage != task.TicketDone || first.Outcome != "abandoned" {
		t.Fatalf("first review = %+v, want done/abandoned", first)
	}

	// The retry previews the now decided ticket (a new digest) and repeats the decision.
	preview := f.ticketView(ops, MethodTicketMovePreview, ticketBody(t, map[string]any{"ticketId": created.TicketID}))
	if preview.Stage != task.TicketDone || preview.Digest == created.Digest {
		t.Fatalf("a decided ticket previews as done with a new digest: %+v", preview)
	}
	retry := f.ticketView(ops, MethodTicketReview, decide(preview.Digest, "abandoned"))
	if !reflect.DeepEqual(retry, first) {
		t.Fatalf("an identical retry must return the recorded view: %+v, want %+v", retry, first)
	}
	// Even the digest from before the decision gets the recorded answer: nothing is written.
	if stale := f.ticketView(ops, MethodTicketReview, decide(created.Digest, "abandoned")); !reflect.DeepEqual(stale, first) {
		t.Fatalf("a retry with the pre-decision digest must return the recorded view: %+v", stale)
	}

	wantError(t, f.ticketCall(ops, MethodTicketReview, decide(preview.Digest, "kept")), "already_decided")
	if after, err := f.store.Ticket(ctx, created.TicketID); err != nil || after.Revision != 2 || after.Outcome != "abandoned" {
		t.Fatalf("the first decision must stand untouched: %+v, %v", after, err)
	}
}

// A dispatch that supplies a blank ticket id is refused as a bad request; it must
// not quietly become an unlinked task.
func TestBridgeDispatchRefusesABlankTicketId(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)
	for _, blank := range []string{" ", "   ", "\t"} {
		wantError(t, f.ticketCall(ops, MethodDispatch, ticketBody(t, map[string]any{
			"title": "t", "brief": "b", "checks": []string{"go test ./..."}, "baseRevision": "0123456", "ticketId": blank,
		})), "bad_request")
	}
	if tasks, err := f.store.Tasks(ctx, f.repoRoot); err != nil || len(tasks) != 0 {
		t.Fatalf("a refused dispatch must create no task: %+v, %v", tasks, err)
	}
}
