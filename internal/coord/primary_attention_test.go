package coord

// Issue #17: a lost primary does not destroy workers; the board marks its absence
// and the inbox is retained. The coordinator persists the observation as
// task.Project.PrimaryAttention - from the startup reconcile and from the paths
// that already probe the primary - and clears it once the primary is verified
// live. These tests drive each writer against the stand-in Herdr and then read the
// fact the way the board does: from the store, with no Herdr call.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/muesli/termenv"

	"herdr-omp-orchestrator/internal/board"
	"herdr-omp-orchestrator/internal/task"
)

// primaryMarker is what the board draws for the project's recorded primary
// attention, read from the durable store exactly as the board model reads it.
func (f *coordFixture) primaryMarker(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	cards, err := board.ProjectCards(ctx, f.store, f.projectKey())
	if err != nil {
		t.Fatalf("board.ProjectCards: %v", err)
	}
	attention, err := board.ProjectPrimaryAttention(ctx, f.store, f.projectKey())
	if err != nil {
		t.Fatalf("board.ProjectPrimaryAttention: %v", err)
	}
	b := board.NewBoard(cards).WithPrimaryAttention(attention)
	return board.Render(b, board.Detail{}, 100, board.NewTheme(termenv.Ascii))
}

// setPrimaryAttention records a stale fact the way an earlier observation would
// have left it.
func (f *coordFixture) setPrimaryAttention(t *testing.T, reason string) {
	t.Helper()
	if err := f.store.UpdateProject(context.Background(), f.projectKey(), func(p *task.Project) error {
		p.PrimaryAttention = reason
		return nil
	}); err != nil {
		t.Fatalf("record the primary attention: %v", err)
	}
}

func TestReconcileRecordsAnAbsentPrimaryAndLeavesWorkersAndInboxAlone(t *testing.T) {
	shapes := []struct {
		name      string
		closePane bool
		want      string
	}{
		{"the primary's agent exited and its terminal remains", false, "holds no agent"},
		{"the primary's pane is absent from the snapshot", true, "absent from Herdr's snapshot"},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			worker := f.boundWorker(t)
			primary := f.boundProject(t).PublicPaneID
			f.mustReconcile(t)
			if got := f.boundProject(t).PrimaryAttention; got != "" {
				t.Fatalf("a verified primary carries attention %q", got)
			}
			if strings.Contains(f.primaryMarker(t), "Primary not verified") {
				t.Fatalf("the board marks a verified primary:\n%s", f.primaryMarker(t))
			}
			noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID
			workerBefore, inboxBefore := f.taskByID(t, worker.ID), f.inboxRows(t)
			startsBefore := f.countCalls("agent start")

			f.vanish(t, primary, shape.closePane)
			f.mustReconcile(t)

			project := f.boundProject(t)
			if !strings.Contains(project.PrimaryAttention, shape.want) || !strings.Contains(project.PrimaryAttention, primary) {
				t.Fatalf("PrimaryAttention = %q, want it to say %q about pane %s", project.PrimaryAttention, shape.want, primary)
			}
			if marker := f.primaryMarker(t); !strings.Contains(marker, "Primary not verified: ") || !strings.Contains(marker, shape.want) {
				t.Fatalf("the board does not mark the absent primary:\n%s", marker)
			}

			// Workers, stages and inbox notes are unaffected.
			if got := f.taskByID(t, worker.ID); !reflect.DeepEqual(got, workerBefore) {
				t.Fatalf("a lost primary changed the worker:\n got %+v\nwant %+v", got, workerBefore)
			}
			if stage := task.StageOf(f.taskByID(t, worker.ID)); stage != task.Running {
				t.Fatalf("worker stage = %q, want %q", stage, task.Running)
			}
			if got := f.inboxRows(t); !reflect.DeepEqual(got, inboxBefore) {
				t.Fatalf("a lost primary changed the inbox:\n got %+v\nwant %+v", got, inboxBefore)
			}
			if state := f.noteState(t, noteID); state != task.DeliveryPending {
				t.Fatalf("note state = %q, want %q: a lost primary's inbox is retained", state, task.DeliveryPending)
			}
			if f.countCalls("agent start") != startsBefore {
				t.Fatal("recording the absence started an agent")
			}
			if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
				t.Fatalf("an absent primary was prompted %d times", len(prompts))
			}

			// A repeated pass over the same Herdr state records the same fact.
			f.mustReconcile(t)
			if again := f.boundProject(t); !reflect.DeepEqual(again, project) {
				t.Fatalf("a repeated pass rewrote the project:\n got %+v\nwant %+v", again, project)
			}
			if got := f.inboxRows(t); !reflect.DeepEqual(got, inboxBefore) {
				t.Fatalf("a repeated pass changed the inbox:\n got %+v\nwant %+v", got, inboxBefore)
			}
		})
	}
}

func TestReconcileClearsThePrimaryAttentionOnceThePrimaryIsVerifiedAgain(t *testing.T) {
	for _, shape := range vanishShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			f.boundWorker(t)
			primary := f.boundProject(t).PublicPaneID
			gone := f.vanish(t, primary, shape.closePane)
			f.mustReconcile(t)
			if f.boundProject(t).PrimaryAttention == "" {
				t.Fatal("the absent primary was not recorded")
			}

			f.restore(primary, gone)
			f.mustReconcile(t)

			project := f.boundProject(t)
			if project.PrimaryAttention != "" {
				t.Fatalf("PrimaryAttention = %q after the primary was verified again, want it cleared", project.PrimaryAttention)
			}
			if marker := f.primaryMarker(t); strings.Contains(marker, "Primary not verified") {
				t.Fatalf("the board still marks a verified primary:\n%s", marker)
			}
			if project.OMPSessionID == "" || project.LaunchPaneID != primary {
				t.Fatalf("clearing the fact changed the binding: %+v", project)
			}
		})
	}
}

func TestReconcileRecordsAPaneThatIsNotThePrimaryAndClearsItWhenItIsAgain(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	f.boundWorker(t)
	project := f.boundProject(t)
	primary := project.PublicPaneID
	var session string
	f.rewriteState(func(current *fixtureState) {
		session = current.Agents[primary].Session
		current.Agents[primary].Session = "someone-elses-session"
	})

	err := f.coord.Reconcile(context.Background())
	if !errors.Is(err, ErrPrimaryNotLive) {
		t.Fatalf("Reconcile err = %v, want ErrPrimaryNotLive", err)
	}
	recorded := f.boundProject(t)
	if !strings.Contains(recorded.PrimaryAttention, "is not the primary") || !strings.Contains(recorded.PrimaryAttention, primary) {
		t.Fatalf("PrimaryAttention = %q, want the refusal that names pane %s", recorded.PrimaryAttention, primary)
	}
	if recorded.LaunchPaneID != project.LaunchPaneID || recorded.OMPSessionID != project.OMPSessionID {
		t.Fatalf("a refuted primary was re-keyed: %+v", recorded)
	}
	if !strings.Contains(f.primaryMarker(t), "Primary not verified: ") {
		t.Fatalf("the board does not mark a primary that another session took over:\n%s", f.primaryMarker(t))
	}

	f.rewriteState(func(current *fixtureState) { current.Agents[primary].Session = session })
	f.mustReconcile(t)
	if got := f.boundProject(t).PrimaryAttention; got != "" {
		t.Fatalf("PrimaryAttention = %q after the session returned, want it cleared", got)
	}
}

func TestFlushInboxKeepsThePrimaryAttentionTrue(t *testing.T) {
	t.Run("an unverified primary is recorded and never rewritten by another observer", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		primary := f.boundProject(t).PublicPaneID
		noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID
		f.vanish(t, primary, false)

		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("FlushInbox with a missing primary: %v", err)
		}
		recorded := f.boundProject(t).PrimaryAttention
		if !strings.HasPrefix(recorded, "the recorded primary cannot be verified: ") || !strings.Contains(recorded, primary) {
			t.Fatalf("PrimaryAttention = %q, want the flush's finding about pane %s", recorded, primary)
		}
		if state := f.noteState(t, noteID); state != task.DeliveryPending {
			t.Fatalf("note state = %q, want %q", state, task.DeliveryPending)
		}

		// An incidental observer keeps the reason that is already there.
		f.setPrimaryAttention(t, "an earlier finding")
		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("FlushInbox: %v", err)
		}
		if got := f.boundProject(t).PrimaryAttention; got != "an earlier finding" {
			t.Fatalf("PrimaryAttention = %q, want the earlier reason kept", got)
		}
	})

	t.Run("a missing checkout replaces an older absence reason", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		project := f.boundProject(t)
		noteID := f.queueNote(t, worker, "progress", "halfway", "worker progress").ID
		f.setPrimaryAttention(t, "the recorded primary cannot be verified: pane absent from Herdr's snapshot")
		f.updatePlan(func(plan *fixturePlan) { plan.BlankCwdFor = project.PublicPaneID })

		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("FlushInbox with an unreported checkout: %v", err)
		}
		if reason := f.boundProject(t).PrimaryAttention; !strings.Contains(reason, "checkout") || strings.Contains(reason, "absent") {
			t.Fatalf("PrimaryAttention = %q, want the newer checkout finding instead of the stale absence", reason)
		}
		if state := f.noteState(t, noteID); state != task.DeliveryPending {
			t.Fatalf("note state = %q, want %q", state, task.DeliveryPending)
		}
		if prompts := f.promptsTo(t, project.PublicPaneID); len(prompts) != 0 {
			t.Fatalf("an unverified primary received %d prompts, want 0", len(prompts))
		}
	})

	t.Run("a primary idle event clears it once the primary is back", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		worker := f.boundWorker(t)
		primary := f.boundProject(t).PublicPaneID
		f.queueNote(t, worker, "progress", "halfway", "worker progress")
		gone := f.vanish(t, primary, false)
		f.mustHandle(t, EventAgentStatus, statusEventPayload(t, primary, "idle"))
		if f.boundProject(t).PrimaryAttention == "" {
			t.Fatal("the idle event for an absent primary recorded nothing")
		}

		f.restore(primary, gone)
		f.setAgentStatus(t, primary, "idle")
		f.mustHandle(t, EventAgentStatus, statusEventPayload(t, primary, "idle"))
		if got := f.boundProject(t).PrimaryAttention; got != "" {
			t.Fatalf("PrimaryAttention = %q after a verified idle primary, want it cleared", got)
		}
		if prompts := f.promptsTo(t, primary); len(prompts) != 1 {
			t.Fatalf("prompts = %d, want the queued note delivered once", len(prompts))
		}
	})

	t.Run("a verified primary retires it even with nothing queued, without prompting", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		f.boundWorker(t)
		primary := f.boundProject(t).PublicPaneID
		f.setPrimaryAttention(t, "the recorded primary cannot be verified: left over")

		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("FlushInbox: %v", err)
		}
		if got := f.boundProject(t).PrimaryAttention; got != "" {
			t.Fatalf("PrimaryAttention = %q, want it cleared by a verified primary", got)
		}
		if prompts := f.promptsTo(t, primary); len(prompts) != 0 {
			t.Fatalf("clearing the fact prompted the primary %d times", len(prompts))
		}
	})

	t.Run("nothing queued and nothing recorded asks Herdr nothing", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		f.boundWorker(t)
		f.projectKey()
		before := len(f.calls())

		if err := f.coord.FlushInbox(context.Background(), f.projectKey()); err != nil {
			t.Fatalf("FlushInbox: %v", err)
		}
		if after := len(f.calls()); after != before {
			t.Fatalf("an idle flush made %d Herdr calls, want none", after-before)
		}
	})
}

func TestPrimaryMoveRetiresButNeverRecordsThePrimaryAttention(t *testing.T) {
	t.Run("a proven move retires it", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		f.boundWorker(t)
		before := f.boundProject(t)
		f.setPrimaryAttention(t, "the recorded primary cannot be verified: left over")

		move := f.moveResult(t, before.PublicPaneID)
		f.mustHandle(t, EventPaneMoved, eventFromMoveResult(t, move))

		after := f.boundProject(t)
		if after.PublicPaneID == before.PublicPaneID {
			t.Fatal("the move was not applied")
		}
		if after.PrimaryAttention != "" {
			t.Fatalf("PrimaryAttention = %q after a proven move, want it cleared", after.PrimaryAttention)
		}
	})

	// A move whose proof fails says the moved pane is not the primary; it does not
	// say the primary is gone, so it keeps its note and refusal and leaves the
	// recorded fact to the passes that actually search for the primary.
	t.Run("a move that does not match the primary records nothing", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		f.boundWorker(t)
		project := f.boundProject(t)
		like := task.Task{
			PublicPaneID: project.PublicPaneID, CurrentWorkspaceID: project.CurrentWorkspaceID,
			TerminalID: project.TerminalID, WorktreePath: project.Key, OMPSessionID: project.OMPSessionID,
		}
		payload := movePayload(t, like, "w9:p1", "w9")
		payload["pane"].(map[string]any)["terminal_id"] = "term-someone-else"
		notesBefore := len(f.notesOfKind(t, noteKindAttention))

		err := f.coord.HandleEvent(context.Background(), EventPaneMoved, encodeEvent(t, payload))
		if !errors.Is(err, ErrPrimaryNotLive) {
			t.Fatalf("err = %v, want ErrPrimaryNotLive", err)
		}
		after := f.boundProject(t)
		if !reflect.DeepEqual(after, project) {
			t.Fatalf("a refused move changed the project:\n got %+v\nwant %+v", after, project)
		}
		if notes := f.notesOfKind(t, noteKindAttention); len(notes) != notesBefore+1 {
			t.Fatalf("attention notes = %d, want the refusal's note (%d before)", len(notes), notesBefore)
		}
	})
}

func TestBindReopenRecordsTheAbsentPrimaryAndRetiresItWhenVerified(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	f.boundWorker(t)
	primary := f.boundProject(t).LaunchPaneID
	gone := f.vanish(t, primary, true)

	if _, err := f.bind("", ""); !errors.Is(err, ErrPrimaryNotLive) {
		t.Fatalf("Bind with the primary pane gone: err = %v, want ErrPrimaryNotLive", err)
	}
	recorded := f.boundProject(t).PrimaryAttention
	if !strings.Contains(recorded, "no longer exists") || !strings.Contains(recorded, primary) {
		t.Fatalf("PrimaryAttention = %q, want the gone pane %s named", recorded, primary)
	}

	f.restore(primary, gone)
	project, err := f.bind("", "")
	if err != nil {
		t.Fatalf("Bind with the primary back: %v", err)
	}
	if project.PrimaryAttention != "" || f.boundProject(t).PrimaryAttention != "" {
		t.Fatalf("PrimaryAttention = %q (stored %q) after a verified bind, want it cleared",
			project.PrimaryAttention, f.boundProject(t).PrimaryAttention)
	}
}

func TestPrimaryAttentionIsBoundedAndNeverEmpty(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	project := f.boundProject(t)

	if err := f.coord.recordPrimaryAttention(context.Background(), project, "  ", true); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("recording a blank reason: err = %v, want ErrInvalidRequest", err)
	}
	if err := f.coord.recordPrimaryAttention(context.Background(), project, strings.Repeat("long ", 400), true); err != nil {
		t.Fatalf("record a long reason: %v", err)
	}
	if got := f.boundProject(t).PrimaryAttention; len([]rune(got)) > primaryAttentionLimit+1 {
		t.Fatalf("stored reason is %d runes, want at most %d", len([]rune(got)), primaryAttentionLimit+1)
	}

	// A stale observation made against a binding that has since moved on is
	// dropped, whether it records or clears. A move keeps the launch pane and
	// changes the public one, so both identities guard the write.
	for name, change := range map[string]func(*task.Project){
		"a newer binding": func(p *task.Project) { p.LaunchPaneID = "w404:p404" },
		"a newer move":    func(p *task.Project) { p.PublicPaneID = "w404:p404" },
	} {
		stale := project
		change(&stale)
		f.setPrimaryAttention(t, "")
		if err := f.coord.recordPrimaryAttention(context.Background(), stale, "stale finding", true); err != nil {
			t.Fatalf("%s: record a stale finding: %v", name, err)
		}
		if got := f.boundProject(t).PrimaryAttention; got != "" {
			t.Fatalf("%s: a stale observation recorded %q", name, got)
		}
		f.setPrimaryAttention(t, "current finding")
		if err := f.coord.clearPrimaryAttention(context.Background(), stale); err != nil {
			t.Fatalf("%s: clear from a stale observation: %v", name, err)
		}
		if got := f.boundProject(t).PrimaryAttention; got != "current finding" {
			t.Fatalf("%s: a stale verification cleared %q", name, got)
		}
	}
}

// With nothing queued the end-of-pass flush returns before it probes, so a
// recorded absence can only have come from the reconcile pass itself.
func TestReconcileAloneRecordsAnAbsentPrimaryWhenNothingIsQueued(t *testing.T) {
	for _, shape := range vanishShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			f.boundWorker(t)
			primary := f.boundProject(t).PublicPaneID
			f.mustReconcile(t)
			inboxBefore := f.inboxRows(t)

			f.vanish(t, primary, shape.closePane)
			f.mustReconcile(t)

			if got := f.boundProject(t).PrimaryAttention; !strings.Contains(got, primary) {
				t.Fatalf("PrimaryAttention = %q, want the reconcile pass to record the absence of pane %s", got, primary)
			}
			if got := f.inboxRows(t); !reflect.DeepEqual(got, inboxBefore) {
				t.Fatalf("recording the absence changed the inbox:\n got %+v\nwant %+v", got, inboxBefore)
			}
		})
	}
}

// applyPrimaryPane is the reconcile pass's own writer. Its project is a snapshot
// taken at the start of the pass, and a hook in another process may have changed
// the stored row since, so it must decide inside the store's transaction.
func TestApplyPrimaryPaneDecidesAgainstTheStoredRow(t *testing.T) {
	livePrimary := func(t *testing.T, f *coordFixture, snapshot task.Project) paneProbe {
		t.Helper()
		probe, err := f.coord.probePrimary(context.Background(), snapshot)
		if err != nil || !probe.Ours {
			t.Fatalf("probePrimary = (%+v, %v), want a verified primary", probe, err)
		}
		return probe
	}

	t.Run("an unchanged identity still clears an absence a hook recorded after the snapshot", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		f.boundWorker(t)
		snapshot := f.boundProject(t)
		probe := livePrimary(t, f, snapshot)
		f.setPrimaryAttention(t, "the recorded primary cannot be verified: recorded by another process")

		if err := f.coord.applyPrimaryPane(context.Background(), snapshot, probe); err != nil {
			t.Fatalf("applyPrimaryPane: %v", err)
		}
		if got := f.boundProject(t).PrimaryAttention; got != "" {
			t.Fatalf("PrimaryAttention = %q after a verified live pass, want it cleared", got)
		}
	})

	t.Run("a pass that predates a move neither rewrites the old pane nor clears the fact", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		f.boundWorker(t)
		snapshot := f.boundProject(t)
		probe := livePrimary(t, f, snapshot)
		probe.Pane.WorkspaceID = "w9" // the verified pane differs from the snapshot, so this pass would write
		const moved, reason = "w404:p1", "the recorded primary cannot be verified: left by the move"
		if err := f.store.UpdateProject(context.Background(), f.projectKey(), func(p *task.Project) error {
			p.PublicPaneID = moved
			p.PrimaryAttention = reason
			return nil
		}); err != nil {
			t.Fatalf("record the move: %v", err)
		}
		recorded := f.boundProject(t)

		if err := f.coord.applyPrimaryPane(context.Background(), snapshot, probe); err != nil {
			t.Fatalf("applyPrimaryPane: %v", err)
		}
		if after := f.boundProject(t); !reflect.DeepEqual(after, recorded) {
			t.Fatalf("a stale reconcile overwrote a newer move:\n got %+v\nwant %+v", after, recorded)
		}
	})

	t.Run("a changed identity is recorded and clears the fact in the same write", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		f.boundWorker(t)
		snapshot := f.boundProject(t)
		probe := livePrimary(t, f, snapshot)
		probe.Pane.WorkspaceID = "w9"
		f.setPrimaryAttention(t, "the recorded primary cannot be verified: stale")
		snapshot.PrimaryAttention = "the recorded primary cannot be verified: stale"

		if err := f.coord.applyPrimaryPane(context.Background(), snapshot, probe); err != nil {
			t.Fatalf("applyPrimaryPane: %v", err)
		}
		after := f.boundProject(t)
		if after.CurrentWorkspaceID != "w9" || after.PrimaryAttention != "" {
			t.Fatalf("project = %+v, want workspace w9 recorded and the fact cleared", after)
		}
	})
}
