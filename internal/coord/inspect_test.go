package coord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/task"
)

// seedTask records one task of a project straight into the store, so a test can
// choose exactly the facts the listing derives a stage from.
func (f *coordFixture) seedTask(project, id string, mutate func(*task.Task)) {
	f.t.Helper()
	x := task.Task{ID: id, ProjectKey: project, Title: "title of " + id, Phase: task.PromptSubmitted}
	if mutate != nil {
		mutate(&x)
	}
	if err := f.store.CreateTask(context.Background(), x); err != nil {
		f.t.Fatalf("seed task %s: %v", id, err)
	}
}

func taskIDsOf(entries []TaskEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.TaskID)
	}
	return ids
}

func noteIDsOf(entries []InboxEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.NoteID)
	}
	return ids
}

func TestListTasksReportsRecordedFactsOpenWorkFirst(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	ticket := f.newTicket(key, "Add search")

	// Created oldest to newest. Ids ascend with creation, so the store's order is
	// t01..t07 and the listing's order is the only thing under test.
	f.seedTask(key, "t01", func(x *task.Task) { x.Outcome, x.Disposition = task.OutcomeIntegrated, "merged by hand" })
	f.seedTask(key, "t02", func(x *task.Task) { x.Blocker = "Which branch should I target?" })
	f.seedTask(key, "t03", func(x *task.Task) { x.Activity, x.TicketID = "working", ticket.ID })
	f.seedTask(key, "t04", func(x *task.Task) { x.Report = &task.Report{Kind: "result", Summary: "done"} })
	f.seedTask(key, "t05", func(x *task.Task) { x.Disposition = "decided before outcomes existed" })
	f.seedTask(key, "t06", func(x *task.Task) { x.Phase = task.Intent })
	f.seedTask(key, "t07", func(x *task.Task) { x.Attention = "the agent exited"; x.Activity = "done" })
	f.seedTask(foreignProject, "t99", nil)

	got, err := f.coord.ListTasksFor(ctx, key, TaskListParams{})
	if err != nil {
		t.Fatalf("ListTasksFor: %v", err)
	}
	// Open tasks, oldest first; then decided tasks, newest first. Another
	// project's task is never listed.
	if want := []string{"t02", "t03", "t04", "t06", "t07", "t05", "t01"}; !reflect.DeepEqual(taskIDsOf(got.Tasks), want) {
		t.Fatalf("order = %v, want %v", taskIDsOf(got.Tasks), want)
	}
	if got.Total != 7 || got.Truncated {
		t.Fatalf("total %d, truncated %v; want 7 and false", got.Total, got.Truncated)
	}

	byID := map[string]TaskEntry{}
	for _, entry := range got.Tasks {
		byID[entry.TaskID] = entry
	}
	for id, want := range map[string]TaskEntry{
		"t01": {TaskID: "t01", Title: "title of t01", Stage: task.Done, Outcome: task.OutcomeIntegrated, Disposition: "merged by hand"},
		"t02": {TaskID: "t02", Title: "title of t02", Stage: task.NeedsInput, Blocker: "Which branch should I target?"},
		"t03": {TaskID: "t03", Title: "title of t03", Stage: task.Running, Activity: "working", TicketID: ticket.ID},
		"t04": {TaskID: "t04", Title: "title of t04", Stage: task.ReadyForReview},
		"t05": {TaskID: "t05", Title: "title of t05", Stage: task.Done, Disposition: "decided before outcomes existed"},
		"t06": {TaskID: "t06", Title: "title of t06", Stage: task.Queued},
		// Herdr's "done" is carried as activity; it never moves a task off its stage.
		"t07": {TaskID: "t07", Title: "title of t07", Stage: task.NeedsInput, Attention: "the agent exited", Activity: "done"},
	} {
		if byID[id] != want {
			t.Errorf("%s = %+v, want %+v", id, byID[id], want)
		}
	}

	// The wire shape: an empty optional field is absent.
	encoded, err := json.Marshal(byID["t06"])
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(encoded), "blocker") || strings.Contains(string(encoded), "outcome") {
		t.Errorf("a queued task carries no blocker or outcome on the wire: %s", encoded)
	}

	// A stage filter keeps that stage only, and Total counts the matches.
	queued, err := f.coord.ListTasksFor(ctx, key, TaskListParams{Stage: string(task.Queued)})
	if err != nil {
		t.Fatalf("ListTasksFor queued: %v", err)
	}
	if only := taskIDsOf(queued.Tasks); !reflect.DeepEqual(only, []string{"t06"}) || queued.Total != 1 {
		t.Fatalf("queued filter = %v (total %d), want [t06]", only, queued.Total)
	}
	decided, err := f.coord.ListTasksFor(ctx, key, TaskListParams{Stage: string(task.Done)})
	if err != nil {
		t.Fatalf("ListTasksFor done: %v", err)
	}
	if only := taskIDsOf(decided.Tasks); !reflect.DeepEqual(only, []string{"t05", "t01"}) {
		t.Fatalf("done filter = %v, want newest first [t05 t01]", only)
	}
}

func TestListTasksForAProjectWithNoTasksIsAnEmptyList(t *testing.T) {
	f, key := ticketFixture(t)
	got, err := f.coord.ListTasksFor(context.Background(), key, TaskListParams{})
	if err != nil {
		t.Fatalf("ListTasksFor: %v", err)
	}
	encoded, _ := json.Marshal(got)
	if string(encoded) != `{"tasks":[],"total":0,"truncated":false}` {
		t.Fatalf("an empty project lists %s", encoded)
	}
}

func TestListTasksRefusesWhatItCannotHonor(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	for name, params := range map[string]TaskListParams{
		"unknown stage":  {Stage: "archived"},
		"stage by case":  {Stage: "Done"},
		"negative limit": {Limit: -1},
		"limit too big":  {Limit: InspectMaxLimit + 1},
	} {
		if _, err := f.coord.ListTasksFor(ctx, key, params); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: ListTasksFor = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := f.coord.ListTasksFor(ctx, "E:/never/bound", TaskListParams{}); !errors.Is(err, ErrProjectUnknown) {
		t.Errorf("an unbound project = %v, want ErrProjectUnknown", err)
	}
	// The largest allowed limit and a blank filter are the ordinary call.
	if _, err := f.coord.ListTasksFor(ctx, key, TaskListParams{Stage: "  ", Limit: InspectMaxLimit}); err != nil {
		t.Errorf("the largest valid call: %v", err)
	}
}

func TestListTasksCutsDecidedHistoryBeforeOpenWork(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	for i := 1; i <= 4; i++ {
		f.seedTask(key, fmt.Sprintf("d%02d", i), func(x *task.Task) { x.Outcome, x.Disposition = task.OutcomeKept, "kept" })
	}
	f.seedTask(key, "o01", func(x *task.Task) { x.Blocker = "needs an answer" })
	f.seedTask(key, "o02", nil)

	got, err := f.coord.ListTasksFor(ctx, key, TaskListParams{Limit: 4})
	if err != nil {
		t.Fatalf("ListTasksFor: %v", err)
	}
	// Both open tasks survive; the oldest decided history is what is cut.
	if want := []string{"o01", "o02", "d04", "d03"}; !reflect.DeepEqual(taskIDsOf(got.Tasks), want) {
		t.Fatalf("a cut list = %v, want %v", taskIDsOf(got.Tasks), want)
	}
	if got.Total != 6 || !got.Truncated {
		t.Fatalf("total %d, truncated %v; want 6 and true", got.Total, got.Truncated)
	}
}

func TestListTasksBoundsTextAndTotalSize(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)

	long := strings.Repeat("é", inspectTextLimit*3)
	f.seedTask(key, "t01", func(x *task.Task) { x.Blocker, x.Disposition = long, long })
	got, err := f.coord.ListTasksFor(ctx, key, TaskListParams{})
	if err != nil {
		t.Fatalf("ListTasksFor: %v", err)
	}
	if blocker := got.Tasks[0].Blocker; blocker != strings.Repeat("é", inspectTextLimit)+"…" {
		t.Errorf("a long blocker is cut to %d characters and marked, got %d runes", inspectTextLimit, len([]rune(blocker)))
	}

	// JSON escapes '<' to six bytes, so 200 tasks of it are far past the bound a
	// row count alone would give. The list must still fit the bridge's response.
	heavy := strings.Repeat("<", inspectTextLimit)
	for i := 2; i <= InspectMaxLimit; i++ {
		f.seedTask(key, fmt.Sprintf("t%03d", i), func(x *task.Task) { x.Blocker, x.Attention = heavy, heavy })
	}
	got, err = f.coord.ListTasksFor(ctx, key, TaskListParams{Limit: InspectMaxLimit})
	if err != nil {
		t.Fatalf("ListTasksFor: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) > 1<<20 {
		t.Fatalf("the answer is %d bytes, over the bridge's 1 MiB bound", len(encoded))
	}
	if !got.Truncated || got.Total != InspectMaxLimit || len(got.Tasks) >= InspectMaxLimit {
		t.Fatalf("an oversized list must say it was cut: %d of %d, truncated %v", len(got.Tasks), got.Total, got.Truncated)
	}
}

// queueInboxNote records one pending note of a project and returns its id.
func (f *coordFixture) queueInboxNote(project, taskID, kind, summary, label string) string {
	f.t.Helper()
	note := inboxNote(project, taskID, kind, summary, label)
	if _, err := f.store.AddInbox(context.Background(), note); err != nil {
		f.t.Fatalf("AddInbox(%s): %v", note.ID, err)
	}
	return note.ID
}

// settleInboxNote moves one pending note through the claim to a settled state.
func (f *coordFixture) settleInboxNote(id, to, detail string) {
	f.t.Helper()
	ctx := context.Background()
	claimed, err := f.store.ClaimInbox(ctx, []string{id})
	if err != nil || len(claimed) != 1 {
		f.t.Fatalf("ClaimInbox(%s) = %d notes, %v", id, len(claimed), err)
	}
	if to == task.DeliverySending {
		return
	}
	if settled, err := f.store.SettleInbox(ctx, []string{id}, to, detail); err != nil || settled != 1 {
		f.t.Fatalf("SettleInbox(%s, %s) = %d, %v", id, to, settled, err)
	}
}

func TestListInboxShowsDeliveryStateAndTheDetailOfAnUncertainNote(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)

	pending := f.queueInboxNote(key, "t1", "progress", "halfway there", "worker progress")
	sending := f.queueInboxNote(key, "t1", "question", "which branch?", "worker question")
	uncertain := f.queueInboxNote(key, "t2", "result", "finished", "worker result")
	deliveredOld := f.queueInboxNote(key, "t2", "progress", "started", "worker progress")
	deliveredNew := f.queueInboxNote(key, "", "attention", "primary lost", "bind failure")
	foreign := f.queueInboxNote(foreignProject, "t9", "progress", "someone else's", "worker progress")
	const detail = "the notification prompt to w1:p1 may already have been delivered; inspect `agent get`/`agent read` before retrying: timeout"
	f.settleInboxNote(sending, task.DeliverySending, "")
	f.settleInboxNote(uncertain, task.DeliveryUncertain, detail)
	f.settleInboxNote(deliveredOld, task.DeliveryDelivered, "prompted to w1:p1")
	f.settleInboxNote(deliveredNew, task.DeliveryDelivered, "prompted to w1:p1")

	got, err := f.coord.ListInboxFor(ctx, key, InboxListParams{})
	if err != nil {
		t.Fatalf("ListInboxFor: %v", err)
	}
	// Undelivered notes first, oldest first; delivered notes after, newest first.
	// Another project's note is never listed.
	if want := []string{pending, sending, uncertain, deliveredNew, deliveredOld}; !reflect.DeepEqual(noteIDsOf(got.Notes), want) {
		t.Fatalf("order = %v, want %v (foreign note %s must be absent)", noteIDsOf(got.Notes), want, foreign)
	}
	if got.Total != 5 || got.Truncated || got.Notice != inboxOriginLabel {
		t.Fatalf("total %d, truncated %v, notice %q", got.Total, got.Truncated, got.Notice)
	}

	byID := map[string]InboxEntry{}
	for _, entry := range got.Notes {
		byID[entry.NoteID] = entry
	}
	if entry := byID[uncertain]; entry.DeliveryState != task.DeliveryUncertain || entry.DeliveryDetail != detail ||
		entry.TaskID != "t2" || entry.Kind != "result" || entry.Summary != "finished" || entry.Label != "worker result" {
		t.Errorf("the uncertain note must carry its durable detail: %+v", entry)
	}
	if entry := byID[pending]; entry.DeliveryState != task.DeliveryPending || entry.DeliveryDetail != "" {
		t.Errorf("a never-claimed note is pending with no detail: %+v", entry)
	}
	if entry := byID[sending]; entry.DeliveryState != task.DeliverySending {
		t.Errorf("a claimed note is sending: %+v", entry)
	}
	if entry := byID[deliveredNew]; entry.DeliveryState != task.DeliveryDelivered || entry.TaskID != "" ||
		entry.DeliveryDetail != "prompted to w1:p1" {
		t.Errorf("a delivered project note: %+v", entry)
	}

	// The filter keeps one state, and only that state.
	only, err := f.coord.ListInboxFor(ctx, key, InboxListParams{State: task.DeliveryUncertain})
	if err != nil || !reflect.DeepEqual(noteIDsOf(only.Notes), []string{uncertain}) || only.Total != 1 {
		t.Fatalf("uncertain filter = %+v, %v", only, err)
	}

	// Reading is not delivering: no state moved, and nothing was sent to a pane.
	for id, want := range map[string]string{
		pending: task.DeliveryPending, sending: task.DeliverySending, uncertain: task.DeliveryUncertain,
		deliveredOld: task.DeliveryDelivered, deliveredNew: task.DeliveryDelivered,
	} {
		if state := f.noteState(t, id); state != want {
			t.Errorf("note %s is %s after the listing, want %s", id, state, want)
		}
	}
	if prompts := f.promptCalls(t); len(prompts) != 0 {
		t.Errorf("listing the inbox sent %d prompts", len(prompts))
	}
}

func TestListInboxRefusesWhatItCannotHonorAndCutsDeliveredHistoryFirst(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)
	for name, params := range map[string]InboxListParams{
		"unknown state":  {State: "lost"},
		"negative limit": {Limit: -3},
		"limit too big":  {Limit: InspectMaxLimit + 1},
	} {
		if _, err := f.coord.ListInboxFor(ctx, key, params); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: ListInboxFor = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := f.coord.ListInboxFor(ctx, "E:/never/bound", InboxListParams{}); !errors.Is(err, ErrProjectUnknown) {
		t.Errorf("an unbound project = %v, want ErrProjectUnknown", err)
	}
	empty, err := f.coord.ListInboxFor(ctx, key, InboxListParams{})
	if err != nil {
		t.Fatalf("ListInboxFor: %v", err)
	}
	if encoded, _ := json.Marshal(empty); !strings.Contains(string(encoded), `"notes":[]`) {
		t.Fatalf("an empty inbox lists %s, want an empty array", encoded)
	}

	var delivered []string
	for i := 1; i <= 3; i++ {
		id := f.queueInboxNote(key, "t1", "progress", fmt.Sprintf("old %d", i), "worker progress")
		f.settleInboxNote(id, task.DeliveryDelivered, "prompted to w1:p1")
		delivered = append(delivered, id)
	}
	stuck := f.queueInboxNote(key, "t1", "result", "never settled", "worker result")
	f.settleInboxNote(stuck, task.DeliveryUncertain, "unresolved")

	got, err := f.coord.ListInboxFor(ctx, key, InboxListParams{Limit: 2})
	if err != nil {
		t.Fatalf("ListInboxFor: %v", err)
	}
	// The uncertain note outranks every delivered one, so it survives the cut.
	if want := []string{stuck, delivered[2]}; !reflect.DeepEqual(noteIDsOf(got.Notes), want) {
		t.Fatalf("a cut inbox = %v, want %v", noteIDsOf(got.Notes), want)
	}
	if got.Total != 4 || !got.Truncated {
		t.Fatalf("total %d, truncated %v; want 4 and true", got.Total, got.Truncated)
	}
}

func TestListInboxBoundsTextAndTotalSize(t *testing.T) {
	ctx := context.Background()
	f, key := ticketFixture(t)

	long := strings.Repeat("x", inspectDetailLimit*2)
	id := f.queueInboxNote(key, "t1", "result", long, "worker result")
	f.settleInboxNote(id, task.DeliveryUncertain, long)
	got, err := f.coord.ListInboxFor(ctx, key, InboxListParams{})
	if err != nil {
		t.Fatalf("ListInboxFor: %v", err)
	}
	entry := got.Notes[0]
	if entry.Summary != strings.Repeat("x", inspectTextLimit)+"…" || entry.DeliveryDetail != strings.Repeat("x", inspectDetailLimit)+"…" {
		t.Errorf("a long summary and detail are cut and marked: %d and %d runes", len([]rune(entry.Summary)), len([]rune(entry.DeliveryDetail)))
	}

	heavy := strings.Repeat("<", inspectDetailLimit)
	for i := 0; i < InspectMaxLimit; i++ {
		note := f.queueInboxNote(key, "t1", "progress", fmt.Sprintf("%d %s", i, heavy), "worker progress")
		f.settleInboxNote(note, task.DeliveryUncertain, heavy)
	}
	got, err = f.coord.ListInboxFor(ctx, key, InboxListParams{Limit: InspectMaxLimit})
	if err != nil {
		t.Fatalf("ListInboxFor: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) > 1<<20 {
		t.Fatalf("the answer is %d bytes, over the bridge's 1 MiB bound", len(encoded))
	}
	if !got.Truncated || len(got.Notes) >= InspectMaxLimit || got.Total != InspectMaxLimit+1 {
		t.Fatalf("an oversized list must say it was cut: %d of %d, truncated %v", len(got.Notes), got.Total, got.Truncated)
	}
}
