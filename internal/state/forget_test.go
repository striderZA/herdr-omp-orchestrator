package state_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestForgetFactsReturnsNotFoundForAnUnknownProject(t *testing.T) {
	s := open(t, t.TempDir())
	_, err := s.ForgetFacts(context.Background(), "repo-missing")
	if !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("ForgetFacts(unknown) = %v, want ErrNotFound", err)
	}
}

func TestForgetProjectDeletesOnlyThisProject(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	createForgetProject(t, s, "repo-a", "w1:p1")
	createForgetProject(t, s, "repo-b", "w2:p1")
	createDecidedForgetTask(t, s, "task-a", "repo-a", "w1:p2")
	createDecidedForgetTask(t, s, "task-b", "repo-b", "w2:p2")
	mustCreateTicket(t, s, seedTicket("ticket-a", "repo-a"))
	mustCreateTicket(t, s, seedTicket("ticket-b", "repo-b"))
	addSettledForgetNote(t, s, "note-a", "repo-a")
	addSettledForgetNote(t, s, "note-b", "repo-b")
	for _, pane := range []string{"w1:p1", "w1:p2", "w2:p1", "w2:p2"} {
		recordForgetReady(t, s, pane)
	}

	projectB, err := s.Project(ctx, "repo-b")
	if err != nil {
		t.Fatalf("Project(repo-b): %v", err)
	}
	taskB, err := s.Task(ctx, "task-b")
	if err != nil {
		t.Fatalf("Task(task-b): %v", err)
	}
	ticketB, err := s.Ticket(ctx, "ticket-b")
	if err != nil {
		t.Fatalf("Ticket(ticket-b): %v", err)
	}
	noteB, err := s.Inbox(ctx, "note-b")
	if err != nil {
		t.Fatalf("Inbox(note-b): %v", err)
	}

	fingerprint := forgetFingerprint(t, s, "repo-a")
	counts, err := s.ForgetProject(ctx, "repo-a", fingerprint)
	if err != nil {
		t.Fatalf("ForgetProject(repo-a): %v", err)
	}
	if want := (state.ForgetCounts{Tasks: 1, Tickets: 1, Notes: 1, ReadySessions: 2}); counts != want {
		t.Errorf("ForgetProject counts = %+v, want %+v", counts, want)
	}
	for _, tc := range []struct {
		name string
		read func() error
	}{
		{"project", func() error { _, err := s.Project(ctx, "repo-a"); return err }},
		{"task", func() error { _, err := s.Task(ctx, "task-a"); return err }},
		{"ticket", func() error { _, err := s.Ticket(ctx, "ticket-a"); return err }},
		{"note", func() error { _, err := s.Inbox(ctx, "note-a"); return err }},
	} {
		if err := tc.read(); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("forgotten %s read = %v, want ErrNotFound", tc.name, err)
		}
	}
	if got, err := s.Project(ctx, "repo-b"); err != nil || !reflect.DeepEqual(got, projectB) {
		t.Errorf("project B after forgetting A = (%+v, %v), want unchanged %+v", got, err, projectB)
	}
	if got, err := s.Task(ctx, "task-b"); err != nil || !reflect.DeepEqual(got, taskB) {
		t.Errorf("task B after forgetting A = (%+v, %v), want unchanged %+v", got, err, taskB)
	}
	if got, err := s.Ticket(ctx, "ticket-b"); err != nil || !reflect.DeepEqual(got, ticketB) {
		t.Errorf("ticket B after forgetting A = (%+v, %v), want unchanged %+v", got, err, ticketB)
	}
	if got, err := s.Inbox(ctx, "note-b"); err != nil || !reflect.DeepEqual(got, noteB) {
		t.Errorf("note B after forgetting A = (%+v, %v), want unchanged %+v", got, err, noteB)
	}
	if got := readyPaneExists(t, rawState(t, dir), "w1:p1", "w1:p2", "w2:p1", "w2:p2"); !reflect.DeepEqual(got, []bool{false, false, true, true}) {
		t.Errorf("ready-session panes after forgetting A = %v, want only B's rows", got)
	}
}

func TestForgetProjectRefusesAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	setups := []struct {
		name  string
		setup func(*testing.T, *state.Store)
	}{
		{
			name: "undecided task",
			setup: func(t *testing.T, s *state.Store) {
				if err := s.CreateTask(ctx, gateTask(t, "task-undecided", "repo-a", nil)); err != nil {
					t.Fatalf("CreateTask: %v", err)
				}
			},
		},
		{
			name: "claimed task",
			setup: func(t *testing.T, s *state.Store) {
				claimed, stored, _, err := s.ClaimTask(ctx, gateTask(t, "task-claimed", "repo-a", nil), 3)
				if err != nil || !claimed {
					t.Fatalf("ClaimTask = (%v, %+v, %v), want claimed", claimed, stored, err)
				}
				if _, err := s.UpdateTask(ctx, stored.ID, stored.Revision, func(x *task.Task) error {
					x.Outcome, x.Disposition = task.OutcomeAbandoned, "already settled"
					return nil
				}); err != nil {
					t.Fatalf("UpdateTask(decide claimed task): %v", err)
				}
			},
		},
		{
			name: "step-claimed task",
			setup: func(t *testing.T, s *state.Store) {
				work := gateTask(t, "task-step-claimed", "repo-a", nil)
				work.Phase = task.WorktreeCreated
				if err := s.CreateTask(ctx, work); err != nil {
					t.Fatalf("CreateTask: %v", err)
				}
				stored, err := s.UpdateTask(ctx, work.ID, 1, func(x *task.Task) error {
					x.Outcome, x.Disposition = task.OutcomeAbandoned, "already settled"
					return nil
				})
				if err != nil {
					t.Fatalf("UpdateTask(decide task): %v", err)
				}
				claimed, err := s.ClaimStep(ctx, stored.ID, task.WorktreeCreated, "owner-1", time.Minute)
				if err != nil || !claimed {
					t.Fatalf("ClaimStep = (%v, %v), want true", claimed, err)
				}
			},
		},
		{
			name: "sending note",
			setup: func(t *testing.T, s *state.Store) {
				addForgetNote(t, s, "note-sending", "repo-a")
				if _, err := s.ClaimInbox(ctx, []string{"note-sending"}); err != nil {
					t.Fatalf("ClaimInbox: %v", err)
				}
			},
		},
		{
			name: "uncertain note",
			setup: func(t *testing.T, s *state.Store) {
				addForgetNote(t, s, "note-uncertain", "repo-a")
				if _, err := s.ClaimInbox(ctx, []string{"note-uncertain"}); err != nil {
					t.Fatalf("ClaimInbox: %v", err)
				}
				if _, err := s.SettleInbox(ctx, []string{"note-uncertain"}, task.DeliveryUncertain, "inspect before retry"); err != nil {
					t.Fatalf("SettleInbox(uncertain): %v", err)
				}
			},
		},
		{
			name: "open bind intent",
			setup: func(t *testing.T, s *state.Store) {
				if _, acquired, created, err := s.ClaimBindIntent(ctx, "repo-a", "owner", time.Minute, nil, nil); err != nil || !acquired || !created {
					t.Fatalf("ClaimBindIntent = (acquired %v, created %v, err %v), want new intent", acquired, created, err)
				}
			},
		},
		{
			name: "uncertain reattach notice",
			setup: func(t *testing.T, s *state.Store) {
				if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
					p.ReattachNotice = task.NoticeAttempt{PaneID: "w1:p1", State: task.NoticeUncertain}
					return nil
				}); err != nil {
					t.Fatalf("UpdateProject(uncertain notice): %v", err)
				}
			},
		},
	}

	for _, tc := range setups {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir)
			createForgetProject(t, s, "repo-a", "w1:p1")
			tc.setup(t, s)
			db := rawState(t, dir)
			before := forgetTableCounts(t, db)
			fingerprint := forgetFingerprint(t, s, "repo-a")
			if _, err := s.ForgetProject(ctx, "repo-a", fingerprint); !errors.Is(err, state.ErrForgetBlocked) {
				t.Fatalf("ForgetProject = %v, want ErrForgetBlocked", err)
			}
			if after := forgetTableCounts(t, db); !reflect.DeepEqual(after, before) {
				t.Errorf("a refusal changed row counts: before %v, after %v", before, after)
			}
		})
	}
}

func TestForgetProjectRefusesAStaleFingerprint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	createForgetProject(t, s, "repo-a", "w1:p1")
	createDecidedForgetTask(t, s, "task-a", "repo-a", "w1:p2")
	fingerprint := forgetFingerprint(t, s, "repo-a")
	current, err := s.Task(ctx, "task-a")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if _, err := s.UpdateTask(ctx, current.ID, current.Revision, func(x *task.Task) error {
		x.Title = "a newer revision"
		return nil
	}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	db := rawState(t, dir)
	before := forgetTableCounts(t, db)
	if _, err := s.ForgetProject(ctx, "repo-a", fingerprint); !errors.Is(err, state.ErrConflict) {
		t.Fatalf("ForgetProject(stale fingerprint) = %v, want ErrConflict", err)
	}
	if after := forgetTableCounts(t, db); !reflect.DeepEqual(after, before) {
		t.Errorf("stale fingerprint refusal changed row counts: before %v, after %v", before, after)
	}
}
func TestForgetProjectRefusesEachChangedFingerprintComponent(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		mutate func(*testing.T, *state.Store)
	}{
		{
			name: "ready-session panes",
			mutate: func(t *testing.T, s *state.Store) {
				recordForgetReady(t, s, "w1:p2")
			},
		},
		{
			name: "notes",
			mutate: func(t *testing.T, s *state.Store) {
				addForgetNote(t, s, "note-new", "repo-a")
			},
		},
		{
			name: "ticket revision",
			mutate: func(t *testing.T, s *state.Store) {
				if _, err := s.DecideTicket(ctx, "ticket-a", 1, task.OutcomeIntegrated, "resolved", ""); err != nil {
					t.Fatalf("DecideTicket: %v", err)
				}
			},
		},
		{
			name: "claimed task IDs",
			mutate: func(t *testing.T, s *state.Store) {
				before, err := s.Task(ctx, "task-a")
				if err != nil {
					t.Fatalf("Task before claim: %v", err)
				}
				claimed, _, err := s.ClaimProvisioningSlot(ctx, "task-a", 3)
				if err != nil || !claimed {
					t.Fatalf("ClaimProvisioningSlot = (%v, %v), want true", claimed, err)
				}
				after, err := s.Task(ctx, "task-a")
				if err != nil {
					t.Fatalf("Task after claim: %v", err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Errorf("task facts changed when claiming a slot: before %+v, after %+v", before, after)
				}
			},
		},
		{
			name: "project",
			mutate: func(t *testing.T, s *state.Store) {
				if err := s.UpdateProject(ctx, "repo-a", func(p *task.Project) error {
					p.OMPSessionID = "updated-session"
					return nil
				}); err != nil {
					t.Fatalf("UpdateProject: %v", err)
				}
			},
		},
		{
			name: "bind intent",
			mutate: func(t *testing.T, s *state.Store) {
				if _, acquired, created, err := s.ClaimBindIntent(ctx, "repo-a", "owner", time.Minute, nil, nil); err != nil || !acquired || !created {
					t.Fatalf("ClaimBindIntent = (acquired %v, created %v, err %v), want new intent", acquired, created, err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, t.TempDir())
			createForgetProject(t, s, "repo-a", "w1:p1")
			createDecidedForgetTask(t, s, "task-a", "repo-a", "w1:p2")
			mustCreateTicket(t, s, seedTicket("ticket-a", "repo-a"))
			fingerprint := forgetFingerprint(t, s, "repo-a")
			tc.mutate(t, s)
			if _, err := s.ForgetProject(ctx, "repo-a", fingerprint); !errors.Is(err, state.ErrConflict) {
				t.Fatalf("ForgetProject(stale %s) = %v, want ErrConflict", tc.name, err)
			}
		})
	}
}

func TestForgetProjectRollsBackWhenTicketDeleteFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	createForgetProject(t, s, "repo-a", "w1:p1")
	recordForgetReady(t, s, "w1:p1")
	addForgetNote(t, s, "note-a", "repo-a")
	mustCreateTicket(t, s, seedTicket("ticket-a", "repo-a"))
	db := rawState(t, dir)
	if _, err := db.Exec(`CREATE TRIGGER fail_forget_ticket BEFORE DELETE ON tickets
		BEGIN SELECT RAISE(ABORT, 'injected ticket delete failure'); END`); err != nil {
		t.Fatalf("create ticket-delete trigger: %v", err)
	}
	before := forgetTableSnapshot(t, db)
	fingerprint := forgetFingerprint(t, s, "repo-a")
	if _, err := s.ForgetProject(ctx, "repo-a", fingerprint); err == nil ||
		errors.Is(err, state.ErrConflict) || errors.Is(err, state.ErrForgetBlocked) {
		t.Fatalf("ForgetProject with ticket-delete failure = %v, want another error", err)
	}
	if after := forgetTableSnapshot(t, db); !reflect.DeepEqual(after, before) {
		t.Errorf("failed forget changed table contents: before %v, after %v", before, after)
	}
}

func TestForgetProjectWithAPendingNoteDeletesIt(t *testing.T) {
	ctx := context.Background()
	s := open(t, t.TempDir())
	createForgetProject(t, s, "repo-a", "w1:p1")
	addForgetNote(t, s, "note-pending", "repo-a")
	counts, err := s.ForgetProject(ctx, "repo-a", forgetFingerprint(t, s, "repo-a"))
	if err != nil {
		t.Fatalf("ForgetProject with pending note: %v", err)
	}
	if counts.Notes != 1 {
		t.Errorf("ForgetProject Notes = %d, want 1", counts.Notes)
	}
	if _, err := s.Inbox(ctx, "note-pending"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("pending note after forget = %v, want ErrNotFound", err)
	}
}

func TestForgetFreesThePaneAndDropsItsSessions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	createForgetProject(t, s, "repo-a", "w1:p1")
	createDecidedForgetTask(t, s, "task-a", "repo-a", "w1:p2")
	recordForgetReady(t, s, "w1:p1")
	recordForgetReady(t, s, "w1:p2")
	counts, err := s.ForgetProject(ctx, "repo-a", forgetFingerprint(t, s, "repo-a"))
	if err != nil {
		t.Fatalf("ForgetProject: %v", err)
	}
	if counts.ReadySessions != 2 {
		t.Errorf("ForgetProject ReadySessions = %d, want 2", counts.ReadySessions)
	}
	db := rawState(t, dir)
	if got := readyPaneExists(t, db, "w1:p1", "w1:p2"); !reflect.DeepEqual(got, []bool{false, false}) {
		t.Errorf("forgotten ready-session rows remain: %v", got)
	}
	if err := s.CreateProject(ctx, task.Project{Key: "repo-new", LaunchPaneID: "w1:p1", PublicPaneID: "w1:p1"}); err != nil {
		t.Fatalf("CreateProject on the freed pane: %v", err)
	}
}

func createForgetProject(t *testing.T, s *state.Store, key, pane string) {
	t.Helper()
	if err := s.CreateProject(context.Background(), task.Project{Key: key, LaunchPaneID: pane, PublicPaneID: pane}); err != nil {
		t.Fatalf("CreateProject(%q): %v", key, err)
	}
}

func createDecidedForgetTask(t *testing.T, s *state.Store, id, key, pane string) task.Task {
	t.Helper()
	x := gateTask(t, id, key, nil)
	x.LaunchPaneID, x.PublicPaneID = pane, pane
	if err := s.CreateTask(context.Background(), x); err != nil {
		t.Fatalf("CreateTask(%q): %v", id, err)
	}
	stored, err := s.UpdateTask(context.Background(), id, 1, func(current *task.Task) error {
		current.Outcome, current.Disposition = task.OutcomeAbandoned, "settled before forget"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateTask(decide %q): %v", id, err)
	}
	return stored
}

func addForgetNote(t *testing.T, s *state.Store, id, key string) {
	t.Helper()
	added, err := s.AddInbox(context.Background(), task.Inbox{ID: id, ProjectKey: key, Kind: "worker_result", Summary: "summary", Delivery: "result"})
	if err != nil || !added {
		t.Fatalf("AddInbox(%q) = (%v, %v), want new note", id, added, err)
	}
}

func addSettledForgetNote(t *testing.T, s *state.Store, id, key string) {
	t.Helper()
	addForgetNote(t, s, id, key)
	if _, err := s.ClaimInbox(context.Background(), []string{id}); err != nil {
		t.Fatalf("ClaimInbox(%q): %v", id, err)
	}
	if _, err := s.SettleInbox(context.Background(), []string{id}, task.DeliveryDelivered, "delivered"); err != nil {
		t.Fatalf("SettleInbox(%q): %v", id, err)
	}
}

func recordForgetReady(t *testing.T, s *state.Store, pane string) {
	t.Helper()
	if err := s.RecordReady(context.Background(), pane, task.Announcement{ID: "omp-" + pane}, 1); err != nil {
		t.Fatalf("RecordReady(%q): %v", pane, err)
	}
}

func forgetFingerprint(t *testing.T, s *state.Store, key string) string {
	t.Helper()
	facts, err := s.ForgetFacts(context.Background(), key)
	if err != nil {
		t.Fatalf("ForgetFacts(%q): %v", key, err)
	}
	fingerprint, err := task.ForgetFingerprint(facts)
	if err != nil {
		t.Fatalf("ForgetFingerprint(%q): %v", key, err)
	}
	return fingerprint
}

func forgetTableCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for _, table := range []string{"projects", "tasks", "inbox", "tickets", "ready_sessions", "bind_intents", "task_history"} {
		var count int
		if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

func readyPaneExists(t *testing.T, db *sql.DB, panes ...string) []bool {
	t.Helper()
	got := make([]bool, len(panes))
	for i, pane := range panes {
		var count int
		if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM ready_sessions WHERE launch_pane_id = ?`, pane).Scan(&count); err != nil {
			t.Fatalf("count ready session for %q: %v", pane, err)
		}
		got[i] = count != 0
	}
	return got
}
func forgetTableSnapshot(t *testing.T, db *sql.DB) map[string][][]string {
	t.Helper()
	snapshot := make(map[string][][]string)
	for _, table := range []string{"projects", "tasks", "inbox", "tickets", "ready_sessions", "bind_intents", "task_history"} {
		rows, err := db.QueryContext(context.Background(), "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatalf("read %s rows: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatalf("read %s columns: %v", table, err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				_ = rows.Close()
				t.Fatalf("scan %s row: %v", table, err)
			}
			record := make([]string, len(values))
			for i, value := range values {
				record[i] = fmt.Sprintf("%T:%#v", value, value)
			}
			snapshot[table] = append(snapshot[table], record)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("iterate %s rows: %v", table, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close %s rows: %v", table, err)
		}
	}
	return snapshot
}
