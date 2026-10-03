package state_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func kindsAndValues(h []state.Transition) [][3]string {
	var out [][3]string
	for _, x := range h {
		out = append(out, [3]string{x.Kind, x.From, x.To})
	}
	return out
}

func TestHistoryRecordsOrderedTransitions(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	steps := []func(*task.Task){
		func(x *task.Task) { x.Phase = task.PromptSubmitted },
		func(x *task.Task) { x.Title = "no tracked change" },
		func(x *task.Task) { x.Attention = "need a call" },
		func(x *task.Task) { x.Attention = "" },
		func(x *task.Task) { x.Report = &task.Report{Kind: "partial", Summary: "half"} },
		func(x *task.Task) { x.Report = &task.Report{Kind: "partial", Summary: "most"} },
		func(x *task.Task) { x.Report = &task.Report{Kind: "result", Summary: "done"} },
		func(x *task.Task) { x.Disposition = "merged"; x.Outcome = task.OutcomeIntegrated },
	}
	rev := int64(1)
	for _, step := range steps {
		if _, err := s.UpdateTask(ctx, "t1", rev, func(x *task.Task) error { step(x); return nil }); err != nil {
			t.Fatalf("UpdateTask at %d: %v", rev, err)
		}
		rev++
	}

	h, err := s.History(ctx, "t1")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	want := [][3]string{
		{"created", "", "queued"},
		{"stage", "queued", "running"}, {"phase", "intent", "prompt_submitted"},
		{"stage", "running", "needs_input"}, {"attention", "", "need a call"},
		{"stage", "needs_input", "running"}, {"attention", "need a call", ""},
		{"report", "", "partial"}, {"report", "partial", "partial (revised: summary)"},
		{"stage", "running", "ready_for_review"}, {"report", "partial", "result"},
		{"stage", "ready_for_review", "done"}, {"outcome", "", task.OutcomeIntegrated}, {"disposition", "", "merged"},
	}
	if got := kindsAndValues(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v\nwant %v", got, want)
	}
	for i := 1; i < len(h); i++ {
		if h[i].Seq <= h[i-1].Seq || h[i].Revision < h[i-1].Revision {
			t.Errorf("entry %d is out of order: %+v after %+v", i, h[i], h[i-1])
		}
	}
}

func TestHistoryRolledBackUpdateLeavesNoRows(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	_, err := s.UpdateTask(ctx, "t1", 1, func(x *task.Task) error {
		x.Phase = task.AgentStarted
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("failing mutation succeeded")
	}
	if _, err := s.UpdateTask(ctx, "t1", 7, func(x *task.Task) error { x.Phase = task.AgentStarted; return nil }); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale update err = %v", err)
	}
	h, _ := s.History(ctx, "t1")
	if len(h) != 1 || h[0].Kind != "created" {
		t.Errorf("history after failed updates = %v, want only creation", kindsAndValues(h))
	}
}

func TestHistoryIsAppendOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	raw := rawState(t, dir)
	if _, err := raw.Exec(`UPDATE task_history SET to_value = 'x'`); err == nil {
		t.Error("UPDATE of a history row succeeded")
	}
	if _, err := raw.Exec(`DELETE FROM task_history`); err == nil {
		t.Error("DELETE of a history row succeeded")
	}
}

func TestHistoryRecordsPublicPaneMoveAndClaim(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	claimed, _, _, err := s.ClaimTask(ctx, seedTask("t1"), 1)
	if err != nil || !claimed {
		t.Fatalf("ClaimTask = (%v, %v)", claimed, err)
	}
	if err := s.MovePublicPane(ctx, launchPane, launchPane, "w2:p3"); err != nil {
		t.Fatalf("MovePublicPane: %v", err)
	}
	h, err := s.History(ctx, "t1")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	want := [][3]string{{"created", "", "queued"}, {"public_pane", launchPane, "w2:p3"}}
	if got := kindsAndValues(h); !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v, want %v", got, want)
	}
}

func TestHistoryRefusedNoteAndStalePaneMoveLeaveNoRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	if err := s.CreateTask(ctx, seedTask("t1")); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	raw := rawState(t, dir)
	if _, err := raw.Exec(`CREATE TRIGGER refuse_inbox BEFORE INSERT ON inbox BEGIN SELECT RAISE(ABORT, 'no'); END`); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	note := task.Inbox{ID: "i1", ProjectKey: "repo-a", TaskID: "t1", Kind: "blocker", Summary: "x", Delivery: "pending"}
	if _, _, err := s.UpdateTaskWithNote(ctx, "t1", 1, func(x *task.Task) error { x.Attention = "help"; return nil }, note); err == nil {
		t.Fatal("update with a refused note succeeded")
	}
	if err := s.MovePublicPane(ctx, launchPane, "w9:p9", "w2:p3"); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("stale pane move err = %v, want ErrConflict", err)
	}
	h, _ := s.History(ctx, "t1")
	if len(h) != 1 || h[0].Kind != "created" {
		t.Errorf("history = %v, want only creation", kindsAndValues(h))
	}
}
