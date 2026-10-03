package coord

// Issue #16: a worker whose brief was submitted and that Herdr can no longer see
// (its pane is gone, or its activity is "unknown") is a task that needs a human.
// The attention is a durable fact raised by the reconcile pass and the status
// hook, projected by task.StageOf, and retired only by a verified live
// observation - never by anything that belongs to a worker's question, a blocker
// or the dispatch step machine.

import (
	"context"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/board"
	"herdr-omp-orchestrator/internal/task"
)

// vanishShapes are the two ways Herdr stops showing a worker. Both leave the
// task's recorded pane without its agent; only the second removes the pane from
// Herdr's snapshot altogether, which is what reconcile meets once a worker's
// workspace is closed. The fixture lists a root pane for every workspace that has
// no agent, so deleting the agent alone only ever reaches the vacant-pane path.
var vanishShapes = []struct {
	name      string
	closePane bool
}{
	{"the agent exited and its terminal remains", false},
	{"the pane is absent from the snapshot", true},
}

// vanishedWorker is what vanish removed, so a test can bring it back.
type vanishedWorker struct {
	agent     fixtureAgent
	workspace string
}

// vanish removes the agent from a pane. With closePane it removes the pane's
// workspace too, and then proves from Herdr's own snapshot that nothing in it
// names the pane or works in its checkout - a test of the absent path that did
// not check this could silently run the vacant one.
func (f *coordFixture) vanish(t *testing.T, pane string, closePane bool) vanishedWorker {
	t.Helper()
	var saved vanishedWorker
	f.rewriteState(func(current *fixtureState) {
		agent := current.Agents[pane]
		if agent == nil {
			t.Fatalf("no agent in %q", pane)
		}
		workspace := workspaceOfPane(pane)
		saved = vanishedWorker{agent: *agent, workspace: current.Workspaces[workspace]}
		delete(current.Agents, pane)
		if closePane {
			delete(current.Workspaces, workspace)
		}
	})
	snapshot, err := f.coord.snapshotView(context.Background())
	if err != nil {
		t.Fatalf("snapshotView: %v", err)
	}
	listed := false
	for _, listedPane := range snapshot.Panes {
		if listedPane.PaneID == pane || snapshotWorksIn(listedPane, saved.workspace) {
			listed = true
		}
	}
	if listed == closePane {
		t.Fatalf("closePane=%t but the snapshot lists the pane or its checkout = %t", closePane, listed)
	}
	return saved
}

// restore brings back the worker vanish took, as an unchanged process in its
// unchanged workspace.
func (f *coordFixture) restore(pane string, gone vanishedWorker) {
	f.t.Helper()
	f.rewriteState(func(current *fixtureState) {
		current.Workspaces[workspaceOfPane(pane)] = gone.workspace
		restored := gone.agent
		current.Agents[pane] = &restored
	})
}

func (f *coordFixture) mustReconcile(t *testing.T) {
	t.Helper()
	if err := f.coord.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// boardStageOf places one task on the board the way the board does, from the
// durable store, and returns the column it landed in.
func (f *coordFixture) boardStageOf(t *testing.T, id string) task.Stage {
	t.Helper()
	cards, err := board.ProjectCards(context.Background(), f.store, f.projectKey())
	if err != nil {
		t.Fatalf("board.ProjectCards: %v", err)
	}
	b := board.NewBoard(cards)
	for _, stage := range board.Columns {
		for _, card := range b.Column(stage) {
			if card.ID() == id {
				return stage
			}
		}
	}
	t.Fatalf("task %s is on no board column", id)
	return ""
}

func TestReconcileRecordsAttentionForAVanishedSubmittedWorker(t *testing.T) {
	for _, shape := range vanishShapes {
		t.Run(shape.name, func(t *testing.T) { recordsAttentionForAVanishedWorker(t, shape.closePane) })
	}
}

func recordsAttentionForAVanishedWorker(t *testing.T, closePane bool) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	startsBefore := f.countCalls("agent start")
	promptsBefore := len(f.promptsTo(t, worker.PublicPaneID))
	f.vanish(t, worker.PublicPaneID, closePane)

	f.mustReconcile(t)

	after := f.taskByID(t, worker.ID)
	if !strings.HasPrefix(after.Attention, absentAttentionPrefix) {
		t.Fatalf("attention = %q, want the absent-worker marker %q", after.Attention, absentAttentionPrefix)
	}
	if stage := task.StageOf(after); stage != task.NeedsInput {
		t.Fatalf("stage = %q, want %q", stage, task.NeedsInput)
	}
	if got := f.boardStageOf(t, worker.ID); got != task.NeedsInput {
		t.Fatalf("the board places the vanished worker in %q, want %q", got, task.NeedsInput)
	}
	if after.Phase != worker.Phase || after.Blocker != "" || after.Report != nil || after.Disposition != "" {
		t.Fatalf("a vanished worker changed more than its attention: %+v", after)
	}
	if f.countCalls("agent start") != startsBefore {
		t.Fatal("reconcile started an agent for a vanished worker")
	}
	if got := len(f.promptsTo(t, worker.PublicPaneID)); got != promptsBefore {
		t.Fatalf("reconcile prompted the vanished worker's pane: %d prompts, want %d", got, promptsBefore)
	}
	if notes := f.notesOfKind(t, noteKindExit); len(notes) != 1 {
		t.Fatalf("exit notes = %d, want 1", len(notes))
	}

	// A second pass over the same Herdr state writes nothing: not the task, not
	// the note.
	f.mustReconcile(t)
	again := f.taskByID(t, worker.ID)
	if again.Revision != after.Revision || again.Attention != after.Attention {
		t.Fatalf("a repeated pass rewrote the task: revision %d -> %d", after.Revision, again.Revision)
	}
	if notes := f.notesOfKind(t, noteKindExit); len(notes) != 1 {
		t.Fatalf("exit notes after a repeated pass = %d, want 1", len(notes))
	}
}

func TestReconcileDoesNotOverwriteAQuestionOrBlockerWhenAWorkerVanishes(t *testing.T) {
	cases := []struct {
		name  string
		raise func(*task.Task)
	}{
		{"worker question", func(next *task.Task) { next.Attention = questionAttention("Which branch should I target?") }},
		{"blocker", func(next *task.Task) { next.Blocker = "waiting on a decision from the primary" }},
		{"dispatch failure", func(next *task.Task) { next.Attention = "task t could not start the worker: the pane is gone" }},
	}
	for _, shape := range vanishShapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				f := newCoordFixture(t, fixturePlan{})
				worker := f.boundWorker(t)
				asked := f.block(worker, tc.raise)
				f.vanish(t, asked.PublicPaneID, shape.closePane)

				f.mustReconcile(t)

				after := f.taskByID(t, worker.ID)
				if after.Attention != asked.Attention || after.Blocker != asked.Blocker {
					t.Fatalf("a vanished worker replaced what was recorded: attention %q blocker %q, was attention %q blocker %q",
						after.Attention, after.Blocker, asked.Attention, asked.Blocker)
				}
				if after.Revision != asked.Revision {
					t.Fatalf("the task was rewritten (revision %d -> %d); only the note may be written", asked.Revision, after.Revision)
				}
				if notes := f.notesOfKind(t, noteKindExit); len(notes) != 1 {
					t.Fatalf("exit notes = %d, want the one note that tells the primary", len(notes))
				}
			})
		}
	}
}

func TestReconcileClearsOnlyTheAttentionAVanishedWorkerRaised(t *testing.T) {
	for _, shape := range vanishShapes {
		t.Run(shape.name, func(t *testing.T) { clearsOnlyTheAttentionAVanishedWorkerRaised(t, shape.closePane) })
	}
}

func clearsOnlyTheAttentionAVanishedWorkerRaised(t *testing.T, closePane bool) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	pane := worker.PublicPaneID
	agent := f.vanish(t, pane, closePane)
	f.mustReconcile(t)
	if got := f.taskByID(t, worker.ID); !isLivenessAttention(got.Attention) {
		t.Fatalf("attention = %q, want a liveness attention before the worker returns", got.Attention)
	}
	startsBefore := f.countCalls("agent start")

	// The same process is back: a verified probe shows the worker live again.
	f.restore(pane, agent)
	f.mustReconcile(t)

	back := f.taskByID(t, worker.ID)
	if back.Attention != "" {
		t.Fatalf("attention = %q after the worker returned, want it cleared", back.Attention)
	}
	if stage := task.StageOf(back); stage != task.Running {
		t.Fatalf("stage = %q after the worker returned, want %q", stage, task.Running)
	}
	if f.countCalls("agent start") != startsBefore {
		t.Fatal("reconcile started an agent")
	}
	if notes := f.notesOfKind(t, noteKindExit); len(notes) != 1 {
		t.Fatalf("exit notes = %d, want the original one", len(notes))
	}

	// A worker question recorded meanwhile belongs to the worker: seeing the
	// worker live must not clear it.
	asked := f.block(back, func(next *task.Task) { next.Attention = questionAttention("Which branch?") })
	f.mustReconcile(t)
	if kept := f.taskByID(t, worker.ID); kept.Attention != asked.Attention {
		t.Fatalf("attention = %q, want the worker question %q kept", kept.Attention, asked.Attention)
	}
}

func TestReconcileLeavesReviewedAndProvisioningTasksAlone(t *testing.T) {
	t.Run("a reported task", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.satisfyReview(f.boundWorker(t))
		f.vanish(t, worker.PublicPaneID, true)

		f.mustReconcile(t)

		after := f.taskByID(t, worker.ID)
		if after.Attention != "" || after.Revision != worker.Revision {
			t.Fatalf("a reported task was touched: attention %q, revision %d -> %d", after.Attention, worker.Revision, after.Revision)
		}
		if stage := task.StageOf(after); stage != task.ReadyForReview {
			t.Fatalf("stage = %q, want %q", stage, task.ReadyForReview)
		}
		if notes := f.notesOfKind(t, noteKindExit); len(notes) != 0 {
			t.Fatalf("a reported task left %d exit notes", len(notes))
		}
	})

	// A worker that is up but whose brief was never submitted belongs to the
	// dispatch step machine: its absence is an exit note, not a liveness verdict.
	t.Run("a worker that never received its brief", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		started := f.block(worker, func(next *task.Task) { next.Phase = task.AgentStarted })

		if err := f.coord.reconcileExit(context.Background(), f.boundProject(t), started); err != nil {
			t.Fatalf("reconcileExit: %v", err)
		}

		after := f.taskByID(t, worker.ID)
		if after.Attention != "" || after.Revision != started.Revision {
			t.Fatalf("a task that never submitted its brief was touched: attention %q, revision %d -> %d",
				after.Attention, started.Revision, after.Revision)
		}
		if notes := f.notesOfKind(t, noteKindExit); len(notes) != 1 {
			t.Fatalf("exit notes = %d, want the note", len(notes))
		}
	})

	t.Run("a worker with no confirmed session", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		bare := f.block(worker, func(next *task.Task) {
			next.Phase = task.WorktreeCreated
			next.OMPSessionID = ""
		})

		if err := f.coord.reconcileExit(context.Background(), f.boundProject(t), bare); err != nil {
			t.Fatalf("reconcileExit: %v", err)
		}

		after := f.taskByID(t, worker.ID)
		if after.Attention != "" || after.Revision != bare.Revision {
			t.Fatalf("a task still provisioning was touched: attention %q", after.Attention)
		}
		if notes := f.notesOfKind(t, noteKindExit); len(notes) != 0 {
			t.Fatalf("a task with no worker left %d exit notes", len(notes))
		}
	})
}

func TestReconcileTreatsAnUnknownActivityAsAttentionUntilItIsLive(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.boundWorker(t)
	pane := worker.PublicPaneID

	f.setAgentStatus(t, pane, "unknown")
	f.mustReconcile(t)
	unknown := f.taskByID(t, worker.ID)
	if unknown.Activity != "unknown" || !strings.HasPrefix(unknown.Attention, unknownAttentionPrefix) {
		t.Fatalf("activity %q attention %q, want unknown with its marker", unknown.Activity, unknown.Attention)
	}
	if got := f.boardStageOf(t, worker.ID); got != task.NeedsInput {
		t.Fatalf("the board places the unknown worker in %q, want %q", got, task.NeedsInput)
	}
	notes := f.notesOfKind(t, noteKindActivity)
	if len(notes) != 1 {
		t.Fatalf("activity notes = %d, want 1", len(notes))
	}

	f.mustReconcile(t)
	if again := f.taskByID(t, worker.ID); again.Revision != unknown.Revision {
		t.Fatalf("a repeated pass rewrote the task: revision %d -> %d", unknown.Revision, again.Revision)
	}
	if got := len(f.notesOfKind(t, noteKindActivity)); got != 1 {
		t.Fatalf("activity notes after a repeated pass = %d, want 1", got)
	}

	// The status hook seeing the same observation adds nothing.
	f.mustHandle(t, EventAgentStatus, statusEventPayload(t, pane, "unknown"))
	if got := len(f.notesOfKind(t, noteKindActivity)); got != 1 {
		t.Fatalf("activity notes after the matching event = %d, want 1", got)
	}

	f.setAgentStatus(t, pane, "idle")
	f.mustReconcile(t)
	if live := f.taskByID(t, worker.ID); live.Attention != "" || task.StageOf(live) != task.Running {
		t.Fatalf("after a live probe: attention %q stage %q, want it cleared and running", live.Attention, task.StageOf(live))
	}
}

func TestRaiseLivenessRefusesTasksThatAreNotSubmittedWorkers(t *testing.T) {
	submitted := task.Task{ID: "t", Phase: task.PromptSubmitted}
	cases := []struct {
		name   string
		mutate func(*task.Task)
	}{
		{"closed", func(x *task.Task) { x.Disposition = "integrated" }},
		{"reported", func(x *task.Task) { x.Report = &task.Report{Kind: "result"} }},
		{"blocked", func(x *task.Task) { x.Blocker = "pending" }},
		{"question", func(x *task.Task) { x.Attention = questionAttention("why?") }},
		{"dispatch failure", func(x *task.Task) { x.Attention = "could not create the worktree" }},
		{"intent", func(x *task.Task) { x.Phase = task.Intent }},
		{"worktree created", func(x *task.Task) { x.Phase = task.WorktreeCreated }},
		{"agent started", func(x *task.Task) { x.Phase = task.AgentStarted }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := submitted
			tc.mutate(&x)
			before := x
			if raiseLiveness(&x, absentAttention) {
				t.Fatal("raiseLiveness changed a task that is not a submitted worker")
			}
			if x.Attention != before.Attention || x.Blocker != before.Blocker {
				t.Fatalf("task changed despite refusal: %+v", x)
			}
		})
	}

	// A liveness attention of the other kind is the plugin's own, so the finding
	// that is true now replaces it; the same finding is a no-op.
	x := submitted
	if !raiseLiveness(&x, unknownAttention) || x.Attention != unknownAttention {
		t.Fatalf("raise unknown: %q", x.Attention)
	}
	if !raiseLiveness(&x, absentAttention) || x.Attention != absentAttention {
		t.Fatalf("raise absent over unknown: %q", x.Attention)
	}
	if raiseLiveness(&x, absentAttention) {
		t.Fatal("raising the same finding twice reported a change")
	}
	if !clearLiveness(&x) || x.Attention != "" {
		t.Fatalf("clear liveness: %q", x.Attention)
	}
	for _, other := range []string{questionAttention("q"), "task t could not start the worker", ""} {
		y := task.Task{Attention: other}
		if clearLiveness(&y) || y.Attention != other {
			t.Fatalf("clearLiveness touched %q", other)
		}
	}
}
