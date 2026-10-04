package coord

// Issue #18: a lost primary is re-established only by an explicit, confirmed
// reattach that names the project and the replacement pane. These tests drive it
// against the stand-in Herdr: the replacement is an OMP the user started in a new
// pane, whose extension announces itself in the first turn it takes after the
// project records its session (AnnounceOnPrompt), so the handshake the tests see
// is the one awaitAgentReady really waits for.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// startReplacement stands in for the user starting OMP in a new pane of its own
// workspace, working in cwd. It returns the pane and the session Herdr reports.
func (f *coordFixture) startReplacement(t *testing.T, cwd string, mutate func(*fixtureAgent)) (pane, session string) {
	t.Helper()
	f.rewriteState(func(current *fixtureState) {
		workspace := current.newWorkspace(cwd)
		pane = workspace + ":p1"
		current.NextSession++
		session = fmt.Sprintf("omp-session-%d", current.NextSession)
		agent := &fixtureAgent{Name: "user-omp-" + workspace, Terminal: "term-user-" + workspace, Session: session, Status: "idle"}
		if mutate != nil {
			mutate(agent)
		}
		current.Agents[pane] = agent
		session = agent.Session
	})
	return pane, session
}

// losePrimary binds, dispatches a worker, queues a note for the primary and then
// loses the primary the way Herdr can lose it. It returns the worker, the project
// as the reconcile pass left it (with its recorded absence) and the note.
func (f *coordFixture) losePrimary(t *testing.T, closePane bool) (task.Task, task.Project, string) {
	t.Helper()
	worker := f.boundWorker(t)
	primary := f.boundProject(t).PublicPaneID
	noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID
	f.vanish(t, primary, closePane)
	f.mustReconcile(t)
	lost := f.boundProject(t)
	if lost.PrimaryAttention == "" {
		t.Fatal("the lost primary was not recorded")
	}
	return worker, lost, noteID
}

func TestReattachReconnectsWorkersAndInboxToAReplacementPrimary(t *testing.T) {
	for _, shape := range vanishShapes {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			f := newCoordFixture(t, fixturePlan{})
			worker, lost, noteID := f.losePrimary(t, shape.closePane)
			workerBefore, inboxBefore := f.taskByID(t, worker.ID), f.inboxRows(t)
			f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
			pane, session := f.startReplacement(t, lost.Key, nil)
			startsBefore, workspacesBefore := f.countCalls("agent start"), f.countCalls("workspace create")

			plan, err := f.coord.PlanReattach(ctx, lost.Key, pane)
			if err != nil {
				t.Fatalf("PlanReattach: %v", err)
			}
			switch {
			case plan.ProjectKey != lost.Key || plan.PaneID != pane || plan.Session.Value != session:
				t.Fatalf("plan = %+v, want the replacement %s/%s of %s", plan, pane, session, lost.Key)
			case plan.Replaces.LaunchPaneID != lost.LaunchPaneID || plan.Replaces.OMPSessionID != lost.OMPSessionID:
				t.Fatalf("plan replaces %+v, want the recorded primary %s/%s", plan.Replaces, lost.LaunchPaneID, lost.OMPSessionID)
			case plan.Observed == "" || plan.Recorded:
				t.Fatalf("plan = %+v, want the finding about the lost primary and no recorded replacement", plan)
			}
			for _, want := range []string{lost.Key, pane, session, lost.LaunchPaneID, lost.OMPSessionID} {
				if !strings.Contains(plan.Describe(), want) {
					t.Errorf("the plan the user confirms never names %q:\n%s", want, plan.Describe())
				}
			}
			// Planning is read only: nothing was recorded and nothing was sent.
			if got := f.boundProject(t); !reflect.DeepEqual(got, lost) {
				t.Fatalf("planning changed the project:\n got %+v\nwant %+v", got, lost)
			}
			if prompts := f.promptsTo(t, pane); len(prompts) != 0 {
				t.Fatalf("planning prompted the replacement %d times", len(prompts))
			}

			project, err := f.coord.Reattach(ctx, lost.Key, pane, plan)
			if err != nil {
				t.Fatalf("Reattach: %v", err)
			}
			stored := f.boundProject(t)
			if !reflect.DeepEqual(project, stored) {
				t.Fatalf("the returned project is not the stored one:\n got %+v\nwant %+v", project, stored)
			}
			workspace := workspaceOfPane(pane)
			switch {
			case stored.LaunchPaneID != pane || stored.PublicPaneID != pane:
				t.Fatalf("panes = %q/%q, want the replacement %q", stored.LaunchPaneID, stored.PublicPaneID, pane)
			case stored.OMPSessionID != session:
				t.Fatalf("session = %q, want the replacement's %q", stored.OMPSessionID, session)
			case stored.TerminalID != "term-user-"+workspace:
				t.Fatalf("terminal = %q, want the replacement's", stored.TerminalID)
			case stored.RepoWorkspaceID != workspace || stored.CurrentWorkspaceID != workspace:
				t.Fatalf("workspaces = %q/%q, want %q", stored.RepoWorkspaceID, stored.CurrentWorkspaceID, workspace)
			case stored.PrimaryAttention != "":
				t.Fatalf("a confirmed replacement still carries attention %q", stored.PrimaryAttention)
			}

			// The replaced session identity is recorded, not silently dropped.
			if len(stored.Replaced) != 1 {
				t.Fatalf("Replaced = %+v, want exactly the displaced primary", stored.Replaced)
			}
			replaced := stored.Replaced[0]
			switch {
			case replaced.LaunchPaneID != lost.LaunchPaneID || replaced.PublicPaneID != lost.PublicPaneID:
				t.Errorf("replaced panes = %q/%q, want %q/%q", replaced.LaunchPaneID, replaced.PublicPaneID, lost.LaunchPaneID, lost.PublicPaneID)
			case replaced.OMPSessionID != lost.OMPSessionID || replaced.TerminalID != lost.TerminalID:
				t.Errorf("replaced identity = %q/%q, want %q/%q", replaced.OMPSessionID, replaced.TerminalID, lost.OMPSessionID, lost.TerminalID)
			case replaced.WorkspaceID != lost.CurrentWorkspaceID || replaced.Reason == "" || replaced.At == 0:
				t.Errorf("replaced = %+v, want its workspace, the reason it was replaced and a time", replaced)
			}

			// Workers and the inbox are untouched, and nothing was started.
			if got := f.taskByID(t, worker.ID); !reflect.DeepEqual(got, workerBefore) {
				t.Fatalf("a reattach changed the worker:\n got %+v\nwant %+v", got, workerBefore)
			}
			if got := f.inboxRows(t); !reflect.DeepEqual(got, inboxBefore) {
				t.Fatalf("a reattach changed the inbox:\n got %+v\nwant %+v", got, inboxBefore)
			}
			if f.countCalls("agent start") != startsBefore || f.countCalls("workspace create") != workspacesBefore {
				t.Fatal("a reattach started an agent or created a workspace instead of adopting the replacement")
			}

			// The replacement was told once, and its extension's announcement is a
			// confirmed handshake for exactly the session Herdr reports.
			prompts := f.promptsTo(t, pane)
			if len(prompts) != 1 || !strings.Contains(prompts[0].Text, "now the primary of "+lost.Key) ||
				!strings.Contains(prompts[0].Text, lost.OMPSessionID) {
				t.Fatalf("prompts to the replacement = %+v, want one notice naming the project and the replaced session", prompts)
			}
			announced := fixtureAnnouncement(f.plan, session)
			if ready, err := f.store.Ready(ctx, pane, announced); err != nil || !ready {
				t.Fatalf("Ready(replacement) = (%v, %v), want a confirmed handshake", ready, err)
			}
			binding, err := f.store.ByOMPSession(ctx, announced)
			if err != nil || binding.Role != state.RolePrimary || binding.Project.Key != lost.Key || binding.Project.LaunchPaneID != pane {
				t.Fatalf("ByOMPSession(replacement) = (%+v, %v), want the project's primary on %s", binding, err, pane)
			}
			if strings.Contains(f.primaryMarker(t), "Primary not verified") {
				t.Fatalf("the board still marks a confirmed primary:\n%s", f.primaryMarker(t))
			}

			// The primary is verified again, so a pass over Herdr's state keeps it,
			// and once it is idle the retained note reaches the replacement.
			f.mustReconcile(t)
			if after := f.boundProject(t); !reflect.DeepEqual(after, stored) {
				t.Fatalf("a reconcile pass changed the reattached project:\n got %+v\nwant %+v", after, stored)
			}
			if got := f.noteState(t, noteID); got != task.DeliveryPending {
				t.Fatalf("note state = %q while the replacement works, want it retained", got)
			}
			f.setAgentStatus(t, pane, "idle")
			if err := f.coord.FlushInbox(ctx, lost.Key); err != nil {
				t.Fatalf("FlushInbox: %v", err)
			}
			if got := f.noteState(t, noteID); got != task.DeliveryDelivered {
				t.Fatalf("note state = %q, want the retained note delivered to the replacement", got)
			}
			if prompts := f.promptsTo(t, pane); len(prompts) != 2 || !strings.Contains(prompts[1].Text, worker.ID) {
				t.Fatalf("prompts to the replacement = %+v, want the notice and then the worker's note", prompts)
			}
		})
	}
}

func TestReattachRefusesAReplacementThatIsNotThisProjectsLiveOMP(t *testing.T) {
	other := t.TempDir()
	cases := []struct {
		name  string
		setup func(f *coordFixture, lost task.Project) string
		want  error
	}{
		{
			name: "a pane in a different repository",
			setup: func(f *coordFixture, lost task.Project) string {
				pane, _ := f.startReplacement(f.t, other, nil)
				return pane
			},
			want: ErrDifferentRepository,
		},
		{
			name: "a pane in a subdirectory of the repository",
			setup: func(f *coordFixture, lost task.Project) string {
				pane, _ := f.startReplacement(f.t, filepath.Join(lost.Key, "src"), nil)
				return pane
			},
			want: ErrDifferentRepository,
		},
		{
			name: "a pane that hosts a different agent kind",
			setup: func(f *coordFixture, lost task.Project) string {
				pane, _ := f.startReplacement(f.t, lost.Key, func(a *fixtureAgent) { a.Kind = "claude" })
				return pane
			},
			want: ErrAgentChanged,
		},
		{
			name: "a plain terminal that hosts no agent",
			setup: func(f *coordFixture, lost task.Project) string {
				var pane string
				f.rewriteState(func(current *fixtureState) { pane = current.newWorkspace(lost.Key) + ":p1" })
				return pane
			},
			want: ErrAgentChanged,
		},
		{
			name:  "a pane that does not exist",
			setup: func(f *coordFixture, lost task.Project) string { return "w99:p1" },
			want:  ErrInvalidRequest,
		},
		{
			name:  "no pane at all",
			setup: func(f *coordFixture, lost task.Project) string { return "  " },
			want:  ErrInvalidRequest,
		},
		{
			name: "an OMP that has not reported its session",
			setup: func(f *coordFixture, lost task.Project) string {
				pane, _ := f.startReplacement(f.t, lost.Key, nil)
				f.updatePlan(func(p *fixturePlan) { p.SuppressSessionFor = pane })
				return pane
			},
			want: ErrSessionUnavailable,
		},
		{
			name: "an OMP whose session is a worker's",
			setup: func(f *coordFixture, lost task.Project) string {
				pane, _ := f.startReplacement(f.t, lost.Key, func(a *fixtureAgent) { a.Session = f.firstWorkerSession() })
				return pane
			},
			want: ErrAlreadyBound,
		},
		{
			name: "a pane another project's primary already owns",
			setup: func(f *coordFixture, lost task.Project) string {
				pane, _ := f.startReplacement(f.t, lost.Key, nil)
				err := f.store.CreateProject(context.Background(), task.Project{
					Key: filepath.Join(other, "elsewhere"), LaunchPaneID: pane, PublicPaneID: pane, OMPSessionID: "elsewhere-session",
				})
				if err != nil {
					f.t.Fatalf("seed another project: %v", err)
				}
				return pane
			},
			want: ErrAlreadyBound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newCoordFixture(t, fixturePlan{})
			_, lost, _ := f.losePrimary(t, true)
			f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
			pane := tc.setup(f, lost)
			inboxBefore := f.inboxRows(t)
			before := f.boundProject(t)

			if _, err := f.coord.PlanReattach(ctx, lost.Key, pane); !errors.Is(err, tc.want) {
				t.Fatalf("PlanReattach err = %v, want %v", err, tc.want)
			}
			// Even a plan the user somehow confirmed cannot be applied to it.
			if _, err := f.coord.Reattach(ctx, lost.Key, pane, ReattachPlan{
				ProjectKey: lost.Key, PaneID: pane, Session: task.SessionRef{Kind: task.SessionKindID, Value: "x"},
			}); !errors.Is(err, tc.want) {
				t.Fatalf("Reattach err = %v, want %v", err, tc.want)
			}
			if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused reattach changed the project:\n got %+v\nwant %+v", after, before)
			}
			if got := f.inboxRows(t); !reflect.DeepEqual(got, inboxBefore) {
				t.Fatalf("a refused reattach changed the inbox:\n got %+v\nwant %+v", got, inboxBefore)
			}
			if prompts := f.promptsTo(t, pane); len(prompts) != 0 {
				t.Fatalf("a refused reattach prompted the pane %d times", len(prompts))
			}
		})
	}
}

// firstWorkerSession is the session of the one worker the fixture dispatched.
func (f *coordFixture) firstWorkerSession() string {
	f.t.Helper()
	tasks, err := f.store.Tasks(context.Background(), f.projectKey())
	if err != nil || len(tasks) == 0 || tasks[0].OMPSessionID == "" {
		f.t.Fatalf("no worker session to reuse: %v", err)
	}
	return tasks[0].OMPSessionID
}

func TestReattachRefusesWhileTheRecordedPrimaryIsLive(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	project := f.boundProject(t)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, _ := f.startReplacement(t, project.Key, nil)
	before := f.boundProject(t)

	_, err := f.coord.PlanReattach(ctx, project.Key, pane)
	if !errors.Is(err, ErrPrimaryLive) {
		t.Fatalf("PlanReattach err = %v, want ErrPrimaryLive", err)
	}
	if !strings.Contains(err.Error(), project.PublicPaneID) {
		t.Errorf("the refusal never names the live primary pane %s: %v", project.PublicPaneID, err)
	}
	if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused reattach changed the project:\n got %+v\nwant %+v", after, before)
	}

	// The refusal is exactly "answering": once the recorded primary is gone the
	// same replacement is accepted.
	f.vanish(t, project.PublicPaneID, false)
	if _, err := f.coord.PlanReattach(ctx, project.Key, pane); err != nil {
		t.Fatalf("PlanReattach after the primary was lost: %v", err)
	}
}

func TestReattachRefusesAnUnknownProject(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	pane, _ := f.startReplacement(t, f.root, nil)
	if _, err := f.coord.PlanReattach(context.Background(), filepath.Join(f.root, "never-bound"), pane); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("PlanReattach err = %v, want ErrProjectUnknown", err)
	}
	if _, err := f.coord.PlanReattach(context.Background(), "", pane); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("PlanReattach(no project) err = %v, want ErrInvalidRequest", err)
	}
}

func TestReattachNeedsTheConfirmedPlan(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, session := f.startReplacement(t, lost.Key, nil)
	plan, err := f.coord.PlanReattach(ctx, lost.Key, pane)
	if err != nil {
		t.Fatalf("PlanReattach: %v", err)
	}
	before := f.boundProject(t)

	mutations := []struct {
		name string
		plan ReattachPlan
		want error
	}{
		{"no plan was confirmed", ReattachPlan{}, ErrInvalidRequest},
		{"a plan for another session", func() ReattachPlan { p := plan; p.Session.Value = "someone-else"; return p }(), ErrEvidenceChanged},
		{"a plan for another replaced primary", func() ReattachPlan { p := plan; p.Replaces.OMPSessionID = "not-recorded"; return p }(), ErrEvidenceChanged},
		{"a plan for another workspace", func() ReattachPlan { p := plan; p.WorkspaceID = "w77"; return p }(), ErrEvidenceChanged},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if _, err := f.coord.Reattach(ctx, lost.Key, pane, m.plan); !errors.Is(err, m.want) {
				t.Fatalf("Reattach err = %v, want %v", err, m.want)
			}
			if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("an unconfirmed reattach changed the project:\n got %+v\nwant %+v", after, before)
			}
			if prompts := f.promptsTo(t, pane); len(prompts) != 0 {
				t.Fatalf("an unconfirmed reattach prompted the replacement")
			}
		})
	}

	// Evidence that changes after the user confirmed is not carried over: the
	// replacement restarted, so the confirmed plan names a session that is gone.
	f.rewriteState(func(current *fixtureState) { current.Agents[pane].Session = session + "-restarted" })
	if _, err := f.coord.Reattach(ctx, lost.Key, pane, plan); !errors.Is(err, ErrEvidenceChanged) {
		t.Fatalf("Reattach after a restart err = %v, want ErrEvidenceChanged", err)
	}
	if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a stale confirmation changed the project:\n got %+v\nwant %+v", after, before)
	}
}

func TestReattachWriteIsACompareAndSwapOfTheRecordedPrimary(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, _ := f.startReplacement(t, lost.Key, nil)
	facts, err := f.coord.verifyReattach(ctx, lost.Key, pane)
	if err != nil {
		t.Fatalf("verifyReattach: %v", err)
	}

	// Another writer re-keyed the primary between verification and the write.
	if err := f.store.UpdateProject(ctx, lost.Key, func(p *task.Project) error {
		p.TerminalID = "term-someone-else"
		return nil
	}); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	concurrent := f.boundProject(t)
	if _, err := f.coord.recordReattach(ctx, facts); !errors.Is(err, ErrEvidenceChanged) {
		t.Fatalf("recordReattach err = %v, want ErrEvidenceChanged", err)
	}
	if after := f.boundProject(t); !reflect.DeepEqual(after, concurrent) {
		t.Fatalf("a lost compare-and-swap wrote anyway:\n got %+v\nwant %+v", after, concurrent)
	}
}

func TestReattachRefusesABusyReplacementBeforeWritingAnything(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, _ := f.startReplacement(t, lost.Key, func(a *fixtureAgent) { a.Status = "working" })
	plan, err := f.coord.PlanReattach(ctx, lost.Key, pane)
	if err != nil {
		t.Fatalf("PlanReattach: %v", err)
	}
	before := f.boundProject(t)

	if _, err := f.coord.Reattach(ctx, lost.Key, pane, plan); !errors.Is(err, ErrReplacementBusy) {
		t.Fatalf("Reattach err = %v, want ErrReplacementBusy", err)
	}
	if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused reattach changed the project:\n got %+v\nwant %+v", after, before)
	}
	if prompts := f.promptsTo(t, pane); len(prompts) != 0 {
		t.Fatalf("a busy replacement was prompted")
	}
}

// pending is a reattach whose first attempt failed: the replacement is recorded
// and unconfirmed, and the worker and inbox are as they were before it.
type pending struct {
	f             *coordFixture
	lost          task.Project
	pane, session string
	err           error
	worker        task.Task
	workerBefore  task.Task
	inboxFrom     []task.Inbox
}

// pendingReattach runs a reattach whose first attempt fails.
func pendingReattach(t *testing.T, fail func(p *fixturePlan)) *pending {
	t.Helper()
	f := newCoordFixture(t, fixturePlan{})
	worker, lost, _ := f.losePrimary(t, true)
	ps := &pending{f: f, lost: lost, worker: worker, workerBefore: f.taskByID(t, worker.ID), inboxFrom: f.inboxRows(t)}
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true; fail(p) })
	ps.pane, ps.session = f.startReplacement(t, lost.Key, nil)
	plan, err := f.coord.PlanReattach(context.Background(), lost.Key, ps.pane)
	if err != nil {
		t.Fatalf("PlanReattach: %v", err)
	}
	_, ps.err = f.coord.Reattach(context.Background(), lost.Key, ps.pane, plan)
	if !errors.Is(ps.err, ErrReattachPending) {
		t.Fatalf("the first Reattach err = %v, want ErrReattachPending", ps.err)
	}
	return ps
}

// untouched asserts that a pending reattach left workers and the inbox alone.
func (ps *pending) untouched(t *testing.T) {
	t.Helper()
	if got := ps.f.taskByID(t, ps.worker.ID); !reflect.DeepEqual(got, ps.workerBefore) {
		t.Fatalf("a pending reattach changed the worker:\n got %+v\nwant %+v", got, ps.workerBefore)
	}
	if got := ps.f.inboxRows(t); !reflect.DeepEqual(got, ps.inboxFrom) {
		t.Fatalf("a pending reattach changed the inbox:\n got %+v\nwant %+v", got, ps.inboxFrom)
	}
}

// reattachAgain plans and confirms the same reattach again, the way the user
// does after a pending one.
func (f *coordFixture) reattachAgain(t *testing.T, key, pane string) (ReattachPlan, task.Project, error) {
	t.Helper()
	plan, err := f.coord.PlanReattach(context.Background(), key, pane)
	if err != nil {
		t.Fatalf("PlanReattach: %v", err)
	}
	project, err := f.coord.Reattach(context.Background(), key, pane, plan)
	return plan, project, err
}

// A refusal that proves nothing was written withdraws the notice attempt, so the
// same reattach may send the notice later; the replacement is recorded once.
func TestReattachRefusedNoticeIsWithdrawnAndTheResumeSendsIt(t *testing.T) {
	ctx := context.Background()
	ps := pendingReattach(t, func(p *fixturePlan) { p.PromptError = herdr.CodeAgentBlocked })
	f, lost, pane, session := ps.f, ps.lost, ps.pane, ps.session
	if !errors.Is(ps.err, herdr.ErrCLI) || !strings.Contains(ps.err.Error(), "bind") {
		t.Fatalf("err = %v, want the refusal as the cause and how to finish", ps.err)
	}
	stored := f.boundProject(t)
	switch {
	case stored.LaunchPaneID != pane || stored.OMPSessionID != session:
		t.Fatalf("the replacement was not recorded: %+v", stored)
	case !strings.Contains(stored.PrimaryAttention, "not confirmed") || !strings.Contains(stored.PrimaryAttention, pane):
		t.Fatalf("PrimaryAttention = %q, want the unconfirmed replacement marked", stored.PrimaryAttention)
	case stored.ReattachNotice != (task.NoticeAttempt{}):
		t.Fatalf("ReattachNotice = %+v, want the refused attempt withdrawn", stored.ReattachNotice)
	case len(stored.Replaced) != 1:
		t.Fatalf("Replaced = %+v, want the displaced primary once", stored.Replaced)
	}
	if !strings.Contains(f.primaryMarker(t), "Primary not verified") {
		t.Fatalf("the board does not mark the unconfirmed replacement:\n%s", f.primaryMarker(t))
	}
	if got := len(f.prompts(pane)); got != 0 {
		t.Fatalf("notices written = %d, want none", got)
	}
	ps.untouched(t)

	f.updatePlan(func(p *fixturePlan) { p.PromptError = "" })
	plan, project, err := f.reattachAgain(t, lost.Key, pane)
	if err != nil {
		t.Fatalf("resumed Reattach: %v", err)
	}
	if !plan.Recorded || plan.Notice != "" {
		t.Fatalf("resumed plan = %+v, want the recorded replacement with no notice on record", plan)
	}
	if project.PrimaryAttention != "" || project.ReattachNotice != (task.NoticeAttempt{}) || len(project.Replaced) != 1 {
		t.Fatalf("resumed project = %+v, want it confirmed with nothing pending and the displaced primary recorded once", project)
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d, want the one the resume sent", got)
	}
	if ready, err := f.store.Ready(ctx, pane, fixtureAnnouncement(f.plan, session)); err != nil || !ready {
		t.Fatalf("Ready(replacement) = (%v, %v), want a confirmed handshake", ready, err)
	}
	ps.untouched(t)
}

// An acknowledged notice is recorded, and a later reattach waits for the
// handshake instead of injecting a second one into the session.
func TestReattachNeverResendsADeliveredNoticeAcrossRetries(t *testing.T) {
	ps := pendingReattach(t, func(p *fixturePlan) { p.SuppressReady = true })
	f, lost, pane, session := ps.f, ps.lost, ps.pane, ps.session
	if !errors.Is(ps.err, ErrReadinessTimeout) {
		t.Fatalf("err = %v, want the readiness timeout as the cause", ps.err)
	}
	attempt := f.boundProject(t).ReattachNotice
	if attempt.PaneID != pane || attempt.Session != session || attempt.State != task.NoticeSent || attempt.At == 0 {
		t.Fatalf("ReattachNotice = %+v, want the acknowledged notice for %s/%s", attempt, pane, session)
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d, want one", got)
	}
	ps.untouched(t)

	// Still no announcement: the retry shows the notice on record, waits, stays
	// pending and sends nothing - even though the session is idle again.
	f.setAgentStatus(t, pane, "idle")
	plan, _, err := f.reattachAgain(t, lost.Key, pane)
	if !errors.Is(err, ErrReattachPending) || !strings.Contains(err.Error(), "none was sent again") {
		t.Fatalf("retry err = %v, want it pending and saying no notice was sent again", err)
	}
	if plan.Notice != task.NoticeSent || !strings.Contains(plan.Describe(), "does not send another") {
		t.Fatalf("retry plan = %+v, want the notice on record shown to the user:\n%s", plan, plan.Describe())
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d after a retry, want still one", got)
	}

	// The user sends the session a message; its extension announces, and the same
	// reattach confirms it without a notice.
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = false; p.SuppressReady = false })
	_, project, err := f.reattachAgain(t, lost.Key, pane)
	if err != nil {
		t.Fatalf("resumed Reattach: %v", err)
	}
	if project.PrimaryAttention != "" || project.ReattachNotice != (task.NoticeAttempt{}) || len(project.Replaced) != 1 {
		t.Fatalf("resumed project = %+v, want it confirmed with nothing pending", project)
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d, want no second notice at all", got)
	}
	ps.untouched(t)
}

// An ambiguous notice may already be in the conversation. It is recorded as
// uncertain and is never resent by a later reattach, exactly as an uncertain
// inbox note is inspected and never resent.
func TestReattachNeverResendsAnUncertainNoticeAcrossRetries(t *testing.T) {
	ps := pendingReattach(t, func(p *fixturePlan) { p.PromptAmbiguous = true; p.SuppressReady = true })
	f, lost, pane, session := ps.f, ps.lost, ps.pane, ps.session
	if !errors.Is(ps.err, ErrReadinessTimeout) {
		t.Fatalf("err = %v, want the readiness timeout as the cause", ps.err)
	}
	attempt := f.boundProject(t).ReattachNotice
	if attempt.PaneID != pane || attempt.Session != session || attempt.State != task.NoticeUncertain {
		t.Fatalf("ReattachNotice = %+v, want an uncertain attempt for %s/%s", attempt, pane, session)
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d, want one", got)
	}

	// Herdr now answers prompts cleanly and the session is idle, which must not
	// matter: the attempt is on record and nothing is resent until it is resolved.
	f.updatePlan(func(p *fixturePlan) { p.PromptAmbiguous = false })
	f.setAgentStatus(t, pane, "idle")
	plan, _, err := f.reattachAgain(t, lost.Key, pane)
	if !errors.Is(err, ErrReattachPending) || !strings.Contains(err.Error(), "none was sent again") {
		t.Fatalf("retry err = %v, want it pending and saying no notice was sent again", err)
	}
	if plan.Notice != task.NoticeUncertain || !strings.Contains(plan.Describe(), "uncertain") {
		t.Fatalf("retry plan = %+v, want the uncertain attempt shown to the user:\n%s", plan, plan.Describe())
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d after a retry, want still one: an uncertain notice is never resent", got)
	}

	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = false; p.SuppressReady = false })
	if _, project, err := f.reattachAgain(t, lost.Key, pane); err != nil || project.ReattachNotice != (task.NoticeAttempt{}) {
		t.Fatalf("resumed Reattach = (%+v, %v), want it confirmed and the attempt retired", project, err)
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d, want no second notice at all", got)
	}
	ps.untouched(t)
}

// The attempt is persisted before the prompt is sent, so a reattach that stopped
// between the two (or whose answer was never read) cannot send another notice.
func TestReattachNoticeIntentOutlivesAnInterruptedAttempt(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, session := f.startReplacement(t, lost.Key, nil)
	facts, err := f.coord.verifyReattach(ctx, lost.Key, pane)
	if err != nil {
		t.Fatalf("verifyReattach: %v", err)
	}
	recorded, err := f.coord.recordReattach(ctx, facts)
	if err != nil {
		t.Fatalf("recordReattach: %v", err)
	}
	// The process stopped after recording its intent and before `agent prompt`.
	intent := task.NoticeAttempt{PaneID: pane, Session: session, Kind: facts.plan.Session.Kind, State: task.NoticeUncertain, At: 1}
	if err := f.coord.recordNotice(ctx, recorded, intent); err != nil {
		t.Fatalf("recordNotice: %v", err)
	}

	plan, _, err := f.reattachAgain(t, lost.Key, pane)
	if !errors.Is(err, ErrReattachPending) {
		t.Fatalf("Reattach err = %v, want it pending: no announcement exists and no notice may be sent", err)
	}
	if plan.Notice != task.NoticeUncertain {
		t.Fatalf("plan.Notice = %q, want the recorded uncertain attempt", plan.Notice)
	}
	if got := len(f.prompts(pane)); got != 0 {
		t.Fatalf("notices written = %d, want none: the recorded intent forbids a new one", got)
	}
}

// The attempt belongs to the replacement it went to: a different replacement
// starts clean and is sent its own notice.
func TestReattachNoticeAttemptDoesNotFollowAnotherReplacement(t *testing.T) {
	ps := pendingReattach(t, func(p *fixturePlan) { p.SuppressReady = true })
	f, lost, first := ps.f, ps.lost, ps.pane
	if attempt := f.boundProject(t).ReattachNotice; attempt.State != task.NoticeSent || attempt.PaneID != first {
		t.Fatalf("ReattachNotice = %+v, want the notice sent to %s", attempt, first)
	}

	f.updatePlan(func(p *fixturePlan) { p.SuppressReady = false })
	second, secondSession := f.startReplacement(t, lost.Key, nil)
	plan, project, err := f.reattachAgain(t, lost.Key, second)
	if err != nil {
		t.Fatalf("Reattach to another replacement: %v", err)
	}
	if plan.Recorded || plan.Notice != "" {
		t.Fatalf("plan = %+v, want a new replacement with no notice on record", plan)
	}
	if project.LaunchPaneID != second || project.OMPSessionID != secondSession || project.ReattachNotice != (task.NoticeAttempt{}) {
		t.Fatalf("project = %+v, want the second replacement confirmed with nothing pending", project)
	}
	if len(project.Replaced) != 2 || project.Replaced[1].LaunchPaneID != first {
		t.Fatalf("Replaced = %+v, want the lost primary and then the unconfirmed first replacement", project.Replaced)
	}
	if got := len(f.prompts(first)); got != 1 {
		t.Fatalf("notices to the first replacement = %d, want the one it already got", got)
	}
	if got := len(f.prompts(second)); got != 1 {
		t.Fatalf("notices to the second replacement = %d, want its own", got)
	}
}

// A single call whose notice is ambiguous but whose effect shows up in the wait
// is confirmed, with the notice written once.
func TestReattachAmbiguousNoticeThatTookEffectIsConfirmedWithoutAResend(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true; p.PromptAmbiguous = true })
	pane, _ := f.startReplacement(t, lost.Key, nil)
	plan, err := f.coord.PlanReattach(ctx, lost.Key, pane)
	if err != nil {
		t.Fatalf("PlanReattach: %v", err)
	}

	project, err := f.coord.Reattach(ctx, lost.Key, pane, plan)
	if err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if project.PrimaryAttention != "" || project.ReattachNotice != (task.NoticeAttempt{}) {
		t.Fatalf("project = %+v, want the announced replacement confirmed and the attempt retired", project)
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d, want exactly one", got)
	}
}

func TestBindOnALostPrimaryStillRefusesAndNamesTheReattachAction(t *testing.T) {
	for _, shape := range vanishShapes[1:] {
		t.Run(shape.name, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			project := f.boundProject(t)
			f.vanish(t, project.PublicPaneID, shape.closePane)
			startsBefore, workspacesBefore := f.countCalls("agent start"), f.countCalls("workspace create")

			_, err := f.bind("", "")
			if !errors.Is(err, ErrPrimaryNotLive) {
				t.Fatalf("Bind err = %v, want ErrPrimaryNotLive", err)
			}
			if !strings.Contains(err.Error(), `"reattach" plugin action`) {
				t.Errorf("the refusal does not name the action that exists: %v", err)
			}
			after := f.boundProject(t)
			if after.LaunchPaneID != project.LaunchPaneID || after.OMPSessionID != project.OMPSessionID || len(after.Replaced) != 0 {
				t.Fatalf("a refused bind substituted a primary: %+v", after)
			}
			if f.countCalls("agent start") != startsBefore || f.countCalls("workspace create") != workspacesBefore {
				t.Fatal("a refused bind started an agent or created a workspace")
			}
		})
	}
}

func TestPaneProjectNamesTheRepositoryRootOfAPane(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	project := f.boundProject(t)

	pane, _ := f.startReplacement(t, project.Key, nil)
	if key, err := f.coord.PaneProject(ctx, pane); err != nil || !samePath(key, project.Key) {
		t.Fatalf("PaneProject = (%q, %v), want %q", key, err, project.Key)
	}
	sub, _ := f.startReplacement(t, filepath.Join(project.Key, "src"), nil)
	if key, err := f.coord.PaneProject(ctx, sub); err != nil || !samePath(key, project.Key) {
		t.Fatalf("PaneProject(subdirectory) = (%q, %v), want the repository root %q", key, err, project.Key)
	}
	elsewhere, _ := f.startReplacement(t, t.TempDir(), nil)
	if _, err := f.coord.PaneProject(ctx, elsewhere); !errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("PaneProject(outside a repository) err = %v, want ErrNotGitRepository", err)
	}
	if _, err := f.coord.PaneProject(ctx, "w99:p1"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("PaneProject(missing pane) err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.coord.PaneProject(ctx, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("PaneProject(no pane) err = %v, want ErrInvalidRequest", err)
	}
}

func TestAppendReplacedKeepsTheMostRecentOldestFirst(t *testing.T) {
	var kept []task.ReplacedPrimary
	for i := 0; i < task.MaxReplacedPrimaries+3; i++ {
		before := kept
		kept = appendReplaced(kept, task.ReplacedPrimary{OMPSessionID: fmt.Sprintf("s%d", i)})
		if len(before) > 0 && &before[0] == &kept[0] {
			t.Fatal("appendReplaced aliased the stored slice")
		}
	}
	if len(kept) != task.MaxReplacedPrimaries {
		t.Fatalf("kept %d entries, want the bound %d", len(kept), task.MaxReplacedPrimaries)
	}
	if kept[0].OMPSessionID != "s3" || kept[len(kept)-1].OMPSessionID != fmt.Sprintf("s%d", task.MaxReplacedPrimaries+2) {
		t.Fatalf("kept %v, want the most recent entries oldest first", kept)
	}
}

// Herdr can report a session as an id or as a session file, and a recorded
// identity can only be compared with a reference in the same form. A pane the
// project already records whose session is now reported in the other form cannot
// be told apart from a different session, so nothing is changed and no notice is
// sent: treating it as a new replacement would forget the notice already sent.
func TestReattachFailsClosedWhenTheSessionReferenceChangesForm(t *testing.T) {
	directions := []struct{ name, first, second string }{
		{"an id that is now reported as a path", task.SessionKindID, task.SessionKindPath},
		{"a path that is now reported as an id", task.SessionKindPath, task.SessionKindID},
	}
	for _, d := range directions {
		t.Run(d.name, func(t *testing.T) {
			ctx := context.Background()
			ps := pendingReattach(t, func(p *fixturePlan) { p.SuppressReady = true; p.SessionRefKind = d.first })
			f, lost, pane := ps.f, ps.lost, ps.pane
			before := f.boundProject(t)
			if before.ReattachNotice.State != task.NoticeSent || before.ReattachNotice.Kind != d.first {
				t.Fatalf("ReattachNotice = %+v, want the notice sent for a %s session", before.ReattachNotice, d.first)
			}
			if sessionKindOf(before.OMPSessionID) != d.first {
				t.Fatalf("recorded session %q is not held as a %s", before.OMPSessionID, d.first)
			}

			// The same session, now reported in the other form, and idle again.
			f.updatePlan(func(p *fixturePlan) { p.SessionRefKind = d.second })
			f.setAgentStatus(t, pane, "idle")
			if _, err := f.coord.PlanReattach(ctx, lost.Key, pane); !errors.Is(err, ErrAgentChanged) {
				t.Fatalf("PlanReattach err = %v, want ErrAgentChanged", err)
			}
			stale := ReattachPlan{ProjectKey: lost.Key, PaneID: pane, Session: task.SessionRef{Kind: d.second, Value: "anything"}}
			if _, err := f.coord.Reattach(ctx, lost.Key, pane, stale); !errors.Is(err, ErrAgentChanged) {
				t.Fatalf("Reattach err = %v, want ErrAgentChanged", err)
			}
			if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
				t.Fatalf("a refused reattach changed the project:\n got %+v\nwant %+v", after, before)
			}
			if got := len(f.prompts(pane)); got != 1 {
				t.Fatalf("notices written = %d, want still the one already sent", got)
			}
		})
	}
}

// A form change is not a blanket refusal: when a verified handshake proves the
// pane now runs a different session, it is a new replacement like any other and
// the displaced session is recorded.
func TestReattachAcceptsAFormChangeThatAVerifiedHandshakeProvesIsADifferentSession(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, first := f.startReplacement(t, lost.Key, nil)
	if _, _, err := f.reattachAgain(t, lost.Key, pane); err != nil {
		t.Fatalf("the first Reattach: %v", err)
	}
	if got := f.boundProject(t).OMPSessionID; got != first {
		t.Fatalf("recorded session = %q, want the first replacement's id %q", got, first)
	}

	// OMP restarts in the same pane: a new session, which Herdr now reports as a path.
	var second string
	f.rewriteState(func(current *fixtureState) {
		current.NextSession++
		second = fmt.Sprintf("omp-session-%d", current.NextSession)
		current.Agents[pane].Session = second
		current.Agents[pane].Status = "idle"
	})
	f.updatePlan(func(p *fixturePlan) { p.SessionRefKind = task.SessionKindPath })
	plan, project, err := f.reattachAgain(t, lost.Key, pane)
	if err != nil {
		t.Fatalf("Reattach after a restart in the same pane: %v", err)
	}
	if plan.Recorded {
		t.Fatalf("plan = %+v, want a new replacement, not the recorded one", plan)
	}
	if want := sessionFileFor(second); project.OMPSessionID != want || project.PrimaryAttention != "" {
		t.Fatalf("project = %+v, want the restarted session %q confirmed", project, want)
	}
	if len(project.Replaced) != 2 || project.Replaced[1].OMPSessionID != first || project.Replaced[1].LaunchPaneID != pane {
		t.Fatalf("Replaced = %+v, want the lost primary and then session %q", project.Replaced, first)
	}
}

// A notice attempt on record for the recorded pane must describe the session
// Herdr reports, in the same form. When it does not, the notice policy cannot be
// applied and nothing is changed.
func TestReattachRefusesANoticeAttemptThatDoesNotDescribeTheSession(t *testing.T) {
	ctx := context.Background()
	ps := pendingReattach(t, func(p *fixturePlan) { p.SuppressReady = true })
	f, lost, pane := ps.f, ps.lost, ps.pane
	if err := f.store.UpdateProject(ctx, lost.Key, func(p *task.Project) error {
		p.ReattachNotice.Kind = task.SessionKindPath
		return nil
	}); err != nil {
		t.Fatalf("tamper with the attempt: %v", err)
	}
	before := f.boundProject(t)

	if _, err := f.coord.PlanReattach(ctx, lost.Key, pane); !errors.Is(err, ErrEvidenceChanged) {
		t.Fatalf("PlanReattach err = %v, want ErrEvidenceChanged", err)
	}
	if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused reattach changed the project:\n got %+v\nwant %+v", after, before)
	}
	if got := len(f.prompts(pane)); got != 1 {
		t.Fatalf("notices written = %d, want still the one already sent", got)
	}
}

// Only "the extension has not announced yet" lets a reattach go on to the notice.
// A failure of the readiness store itself says nothing about the extension.
func TestHandshakePendingOnlyMeansTheExtensionHasNotAnnouncedYet(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("%w: launch pane", state.ErrNotFound), true},
		{fmt.Errorf("%w: another session", state.ErrMismatch), true},
		{fmt.Errorf("%w: recorded but not the announced session", ErrAgentChanged), false},
		{fmt.Errorf("%w: unusable reference", state.ErrInvalid), false},
		{errors.New("database is locked"), false},
		{context.DeadlineExceeded, false},
		{ErrSessionUnavailable, false},
	}
	for _, tc := range cases {
		if got := handshakePending(tc.err); got != tc.want {
			t.Errorf("handshakePending(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestReattachSendsNoNoticeWhenTheHandshakeCannotBeChecked(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, _ := f.startReplacement(t, lost.Key, nil)
	facts, err := f.coord.verifyReattach(ctx, lost.Key, pane)
	if err != nil {
		t.Fatalf("verifyReattach: %v", err)
	}
	recorded, err := f.coord.recordReattach(ctx, facts)
	if err != nil {
		t.Fatalf("recordReattach: %v", err)
	}

	// The readiness store refuses the reference as unusable: a failure of the check
	// itself, which says nothing about whether the extension announced.
	facts.view.Ref = task.SessionRef{Kind: "bogus", Value: "x"}
	_, err = f.coord.confirmReattach(ctx, recorded, facts, true)
	if !errors.Is(err, ErrReattachPending) || !errors.Is(err, state.ErrInvalid) || !strings.Contains(err.Error(), "no notice was sent") {
		t.Fatalf("confirmReattach err = %v, want it pending, caused by the store, saying no notice was sent", err)
	}
	if got := len(f.prompts(pane)); got != 0 {
		t.Fatalf("notices written = %d, want none", got)
	}
	if got := f.boundProject(t).ReattachNotice; got != (task.NoticeAttempt{}) {
		t.Fatalf("ReattachNotice = %+v, want no attempt: nothing was sent", got)
	}
}

// The other side of the form guard: when a verified handshake shows that the
// reference now reported in the other form is the very session the project
// records, the primary is live, not lost. The handshake carries both forms, so
// reattach refuses it as live instead of re-recording the pane as replacing itself.
func TestReattachRefusesAFormChangeThatAVerifiedHandshakeProvesIsTheSameSession(t *testing.T) {
	ctx := context.Background()
	f := newCoordFixture(t, fixturePlan{})
	_, lost, _ := f.losePrimary(t, true)
	f.updatePlan(func(p *fixturePlan) { p.AnnounceOnPrompt = true })
	pane, first := f.startReplacement(t, lost.Key, nil)
	if _, _, err := f.reattachAgain(t, lost.Key, pane); err != nil {
		t.Fatalf("the first Reattach: %v", err)
	}
	before := f.boundProject(t)
	if before.OMPSessionID != first || before.ReattachNotice != (task.NoticeAttempt{}) {
		t.Fatalf("project = %+v, want the verified replacement recorded by its id", before)
	}
	noticesBefore := len(f.prompts(pane))

	// Herdr now reports the same session as its session file.
	f.updatePlan(func(p *fixturePlan) { p.SessionRefKind = task.SessionKindPath })
	f.setAgentStatus(t, pane, "idle")
	if _, err := f.coord.PlanReattach(ctx, lost.Key, pane); !errors.Is(err, ErrPrimaryLive) {
		t.Fatalf("PlanReattach err = %v, want ErrPrimaryLive: the verified handshake proves it is the same session", err)
	}
	if after := f.boundProject(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused reattach changed the project:\n got %+v\nwant %+v", after, before)
	}
	if got := len(f.prompts(pane)); got != noticesBefore {
		t.Fatalf("notices written = %d, want still %d", got, noticesBefore)
	}
}
