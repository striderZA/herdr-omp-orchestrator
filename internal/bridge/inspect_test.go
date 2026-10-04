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

var inspectMethods = []string{MethodTaskList, MethodInboxList}

// seedInboxNote records one note of a project and moves it to a delivery state
// the way a flush does: claimed, then settled.
func (f *bridgeFixture) seedInboxNote(project, taskID, kind, summary, deliveryState, detail string) task.Inbox {
	f.t.Helper()
	ctx := context.Background()
	note := task.Inbox{
		ID: kind + ":" + taskID + ":" + summary, ProjectKey: project, TaskID: taskID,
		Kind: kind, Summary: summary, Delivery: "worker " + kind,
	}
	if _, err := f.store.AddInbox(ctx, note); err != nil {
		f.t.Fatalf("AddInbox(%s): %v", note.ID, err)
	}
	if deliveryState == task.DeliveryPending {
		return note
	}
	if claimed, err := f.store.ClaimInbox(ctx, []string{note.ID}); err != nil || len(claimed) != 1 {
		f.t.Fatalf("ClaimInbox(%s) = %d notes, %v", note.ID, len(claimed), err)
	}
	if deliveryState != task.DeliverySending {
		if settled, err := f.store.SettleInbox(ctx, []string{note.ID}, deliveryState, detail); err != nil || settled != 1 {
			f.t.Fatalf("SettleInbox(%s) = %d, %v", note.ID, settled, err)
		}
	}
	return note
}

func TestBridgeInspectMethodsArePrimaryOnly(t *testing.T) {
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
	stub := Ops{TaskList: count, InboxList: count}

	// A confirmed, bound worker may read neither list.
	f.env["HERDR_PANE_ID"] = workerPane
	for _, method := range inspectMethods {
		wantError(t, f.callWith(method, workerSession, `{}`, stub), CodeForbidden)
	}

	// A primary whose handshake is announced but not yet confirmed is refused too.
	if err := f.store.RecordReady(ctx, primaryPane, announcedID(primarySession), 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	f.env["HERDR_PANE_ID"] = primaryPane
	for _, method := range inspectMethods {
		wantError(t, f.callWith(method, primarySession, `{}`, stub), CodeNotReady)
	}
	if calls != 0 {
		t.Fatalf("a refused caller reached the coordinator %d times", calls)
	}

	// A caller that is the primary's session but not its pane is refused as well:
	// the verified launch pane is part of the proof on every call.
	f.confirm(primaryPane, primarySession)
	f.env["HERDR_PANE_ID"] = workerPane
	for _, method := range inspectMethods {
		if resp := f.callWith(method, primarySession, `{}`, stub); resp.OK {
			t.Fatalf("%s answered a primary session from the worker's pane", method)
		}
	}
	if calls != 0 {
		t.Fatalf("a session on the wrong pane reached the coordinator %d times", calls)
	}

	// The confirmed primary reaches both, with the verified principal.
	f.env["HERDR_PANE_ID"] = primaryPane
	for _, method := range inspectMethods {
		if resp := f.callWith(method, primarySession, `{}`, stub); !resp.OK {
			t.Fatalf("%s failed for the confirmed primary: %+v", method, resp.Error)
		}
	}
	if calls != len(inspectMethods) {
		t.Fatalf("the operations ran %d times, want %d", calls, len(inspectMethods))
	}
	if seen.Role != "primary" || seen.ProjectKey != f.repoRoot || seen.LaunchPaneID != primaryPane {
		t.Fatalf("the coordinator must see the verified principal: %+v", seen)
	}
}

func TestBridgeInspectMethodsWithoutWiringAreAuthorizedThenUnavailable(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.asPrimary()
	for _, method := range inspectMethods {
		wantError(t, f.callWith(method, primarySession, `{}`, Ops{}), CodeNotImplemented)
		wantError(t, f.callWith(method, primarySession, `{}`, UnavailableOps(errors.New("no coordinator"))), CodeHerdrUnavailable)
	}
}

// A worker is refused by the real operations too, not only by a counting stub.
func TestBridgeInspectMethodsRefuseAWorkerWithTheRealCoordinator(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.seedInboxNote(f.repoRoot, "t1", "progress", "halfway", task.DeliveryPending, "")
	f.asWorker()
	ops := CoordinatorOps(f.coordinator(t))
	for _, method := range inspectMethods {
		wantError(t, f.callWith(method, workerSession, `{}`, ops), CodeForbidden)
	}
}

func TestBridgeTaskListReportsTheProjectsTasks(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)

	for _, x := range []task.Task{
		{ID: "orch_old", ProjectKey: f.repoRoot, Title: "closed work", Phase: task.PromptSubmitted, Outcome: task.OutcomeIntegrated, Disposition: "merged"},
		{ID: "orch_ask", ProjectKey: f.repoRoot, Title: "blocked work", Phase: task.PromptSubmitted, Blocker: "Which branch?", Activity: "blocked"},
		{ID: "orch_run", ProjectKey: f.repoRoot, Title: "running work", Phase: task.PromptSubmitted, Activity: "working"},
		{ID: "orch_foreign", ProjectKey: foreignTicketProject, Title: "someone else's", Phase: task.PromptSubmitted},
	} {
		if err := f.store.CreateTask(ctx, x); err != nil {
			t.Fatalf("seed %s: %v", x.ID, err)
		}
	}

	var listed coord.TaskListResult
	f.ticketCall(ops, MethodTaskList, `{}`).decodeResult(t, &listed)
	ids := make([]string, 0, len(listed.Tasks))
	for _, entry := range listed.Tasks {
		ids = append(ids, entry.TaskID)
	}
	if want := []string{"orch_ask", "orch_run", "orch_old"}; !reflect.DeepEqual(ids, want) || listed.Total != 3 || listed.Truncated {
		t.Fatalf("listed %v (total %d, truncated %v), want %v", ids, listed.Total, listed.Truncated, want)
	}
	ask := listed.Tasks[0]
	if ask.Stage != task.NeedsInput || ask.Blocker != "Which branch?" || ask.Activity != "blocked" || ask.Title != "blocked work" {
		t.Fatalf("the blocked task = %+v", ask)
	}
	if old := listed.Tasks[2]; old.Stage != task.Done || old.Outcome != task.OutcomeIntegrated || old.Disposition != "merged" {
		t.Fatalf("the decided task = %+v", old)
	}

	// A filter and a limit are the caller's only parameters.
	f.ticketCall(ops, MethodTaskList, `{"stage":"running","limit":1}`).decodeResult(t, &listed)
	if len(listed.Tasks) != 1 || listed.Tasks[0].TaskID != "orch_run" || listed.Total != 1 {
		t.Fatalf("a running filter = %+v", listed)
	}

	// Everything else in the body is ignored: the project, the task and the pane
	// come from the verified principal.
	f.ticketCall(ops, MethodTaskList, ticketBody(t, map[string]any{
		"projectKey": foreignTicketProject, "project": foreignTicketProject, "taskId": "orch_foreign", "paneId": workerPane,
	})).decodeResult(t, &listed)
	if listed.Total != 3 {
		t.Fatalf("a body naming another project changed the answer: %+v", listed)
	}
	for _, entry := range listed.Tasks {
		if entry.TaskID == "orch_foreign" {
			t.Fatalf("another project's task was listed: %+v", entry)
		}
	}

	// Reading changes nothing.
	if tasks, err := f.store.Tasks(ctx, f.repoRoot); err != nil || len(tasks) != 3 {
		t.Fatalf("listing wrote to the project's tasks: %+v, %v", tasks, err)
	}
}

func TestBridgeInboxListShowsDeliveryStateAndTheDetailOfAnUncertainNote(t *testing.T) {
	ctx := context.Background()
	f, ops := primaryTicketBridge(t)

	const detail = "the notification prompt to w1:p1 may already have been delivered; inspect `agent get`/`agent read` before retrying: timeout"
	f.seedInboxNote(f.repoRoot, "t1", "progress", "halfway", task.DeliveryPending, "")
	f.seedInboxNote(f.repoRoot, "t1", "question", "which branch?", task.DeliverySending, "")
	f.seedInboxNote(f.repoRoot, "t2", "result", "finished", task.DeliveryUncertain, detail)
	f.seedInboxNote(f.repoRoot, "t2", "progress", "started", task.DeliveryDelivered, "prompted to w1:p1")
	f.seedInboxNote(foreignTicketProject, "t9", "progress", "someone else's", task.DeliveryUncertain, "foreign detail")

	var listed coord.InboxListResult
	f.ticketCall(ops, MethodInboxList, `{}`).decodeResult(t, &listed)
	states := map[string]string{}
	for _, entry := range listed.Notes {
		states[entry.Summary] = entry.DeliveryState
	}
	want := map[string]string{
		"halfway": task.DeliveryPending, "which branch?": task.DeliverySending,
		"finished": task.DeliveryUncertain, "started": task.DeliveryDelivered,
	}
	if !reflect.DeepEqual(states, want) || listed.Total != 4 || listed.Truncated {
		t.Fatalf("states %v (total %d, truncated %v), want %v and no foreign note", states, listed.Total, listed.Truncated, want)
	}
	if !strings.Contains(listed.Notice, "not from the user") {
		t.Fatalf("the answer must say the notes are not the user's: %q", listed.Notice)
	}

	// The uncertain note's durable detail is retrievable, alone, by its state.
	f.ticketCall(ops, MethodInboxList, `{"state":"uncertain"}`).decodeResult(t, &listed)
	if len(listed.Notes) != 1 || listed.Notes[0].DeliveryDetail != detail || listed.Notes[0].TaskID != "t2" || listed.Notes[0].Kind != "result" {
		t.Fatalf("the uncertain filter = %+v", listed)
	}

	// Reading a note is not delivering it, and it never leaves uncertain.
	stored, err := f.store.Inbox(ctx, listed.Notes[0].NoteID)
	if err != nil || stored.State() != task.DeliveryUncertain || stored.DeliveryDetail != detail {
		t.Fatalf("the stored note after the listing = %+v, %v", stored, err)
	}
	if pending, err := f.store.PendingInbox(ctx, f.repoRoot); err != nil || len(pending) != 1 {
		t.Fatalf("listing must not claim a pending note: %+v, %v", pending, err)
	}

	// A body naming another project changes nothing.
	f.ticketCall(ops, MethodInboxList, ticketBody(t, map[string]any{"projectKey": foreignTicketProject, "taskId": "t9"})).decodeResult(t, &listed)
	if listed.Total != 4 {
		t.Fatalf("a body naming another project changed the answer: %+v", listed)
	}
}

func TestBridgeInspectRefusalsAreCoded(t *testing.T) {
	f, ops := primaryTicketBridge(t)
	for _, tc := range []struct{ method, body, code string }{
		{MethodTaskList, `{"stage":"archived"}`, "bad_request"},
		{MethodTaskList, `{"limit":0,"stage":"Done"}`, "bad_request"},
		{MethodTaskList, `{"limit":-1}`, "bad_request"},
		{MethodTaskList, `{"limit":201}`, "bad_request"},
		{MethodTaskList, `{"limit":"ten"}`, CodeBadParams},
		{MethodTaskList, `[1]`, CodeBadParams},
		{MethodInboxList, `{"state":"lost"}`, "bad_request"},
		{MethodInboxList, `{"limit":201}`, "bad_request"},
		{MethodInboxList, `{"limit":1.5}`, CodeBadParams},
		{MethodInboxList, `"all"`, CodeBadParams},
	} {
		wantError(t, f.ticketCall(ops, tc.method, tc.body), tc.code)
	}
	// An absent or null body is the ordinary unfiltered call.
	for _, method := range inspectMethods {
		for _, body := range []string{``, `null`, `{}`} {
			if resp := f.ticketCall(ops, method, body); !resp.OK {
				t.Errorf("%s with body %q: %+v", method, body, resp.Error)
			}
		}
	}
}

func TestBridgeInspectMethodsKeepProtocolOne(t *testing.T) {
	if Protocol != 1 {
		t.Fatalf("Protocol = %d: the inspection methods are additive and the version stays 1", Protocol)
	}
	f, ops := primaryTicketBridge(t)
	for _, method := range inspectMethods {
		payload := `{"version":2,"method":"` + method + `","ompSessionID":"` + primarySession + `","params":{}}`
		out, err := HandleWith(context.Background(), f.env, strings.NewReader(payload), ops)
		if err != nil {
			t.Fatalf("HandleWith(%s): %v", method, err)
		}
		wantError(t, decodeResponse(t, out), CodeRequestVersion)
		if resp := f.ticketCall(ops, method, `{}`); !resp.OK {
			t.Fatalf("version 1 still works for %s: %+v", method, resp.Error)
		}
	}
}
