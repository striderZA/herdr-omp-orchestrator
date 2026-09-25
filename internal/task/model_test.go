package task

import "testing"

func TestStageSeparatesActivityFromCompletion(t *testing.T) {
	if StageOf(Task{Phase: Intent}) != Queued {
		t.Fatal("unprovisioned intent is queued, not running")
	}
	x := Task{Phase: PromptSubmitted, Activity: "done"}
	if StageOf(x) != Running {
		t.Fatalf("Herdr done is not task done: %s", StageOf(x))
	}
	x.Blocker = "need an answer"
	if StageOf(x) != NeedsInput {
		t.Fatal("blocker must be visible")
	}
	x.Blocker = ""
	x.Report = &Report{Kind: "partial", Summary: "more work remains"}
	if StageOf(x) != Running {
		t.Fatal("partial report is not final")
	}
	x.Report = &Report{Kind: "result", Summary: "finished"}
	if StageOf(x) != ReadyForReview {
		t.Fatal("report needs review")
	}
	x.Disposition = "user chose to keep branch"
	if StageOf(x) != Done {
		t.Fatal("recorded disposition closes task")
	}
}

func TestStagePriorityOrder(t *testing.T) {
	cases := []struct {
		name string
		job  Task
		want Stage
	}{
		{"empty task is queued", Task{}, Queued},
		{"worktree created is running", Task{Phase: WorktreeCreated}, Running},
		{"agent started is running", Task{Phase: AgentStarted}, Running},
		{"attention needs input", Task{Phase: PromptSubmitted, Attention: "dialog open"}, NeedsInput},
		{"blocker outranks final report", Task{Phase: PromptSubmitted, Report: &Report{Kind: "result"}, Blocker: "stuck"}, NeedsInput},
		{"disposition outranks final report", Task{Phase: PromptSubmitted, Report: &Report{Kind: "result"}, Disposition: "kept branch"}, Done},
		{"unknown report kind is not final", Task{Phase: PromptSubmitted, Report: &Report{Kind: "note"}}, Running},
		{"activity alone does not advance a task", Task{Phase: Intent, Activity: "done"}, Queued},
	}
	for _, tc := range cases {
		if got := StageOf(tc.job); got != tc.want {
			t.Errorf("%s: StageOf = %s, want %s", tc.name, got, tc.want)
		}
	}
}
