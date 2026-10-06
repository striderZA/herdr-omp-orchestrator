package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"

	_ "modernc.org/sqlite"
)

// This file tests the durable half of Task 5: worker progress/questions, the
// completeness of a final report, and the primary's disposition. Every one of
// those is a fact in the store, never a terminal operation, and that is
// assertable: the fixture's Herdr client points at a binary that does not
// exist, so an operation that called Herdr would fail with "cannot run"
// instead of the outcome each test asserts. The operations that legitimately
// read Herdr (answer relay, dialog approval, cleanup) are exercised end to end
// through the bridge in internal/bridge, which owns a scripted Herdr.

const (
	reportPrimaryPane = "w1:p1"
	reportPrimarySess = "omp-primary-1"
	reportWorkerPane  = "w2:p1"
	reportWorkerSess  = "omp-worker-1"
	reportWorktreeWS  = "ws-worktree-1"
)

// reportFixture is one bound primary and one briefed worker over a real store.
type reportFixture struct {
	t        *testing.T
	store    *state.Store
	coord    *Coordinator
	stateDir string
	root     string
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	stateDir := t.TempDir()
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	client, err := herdr.NewClient(filepath.Join(t.TempDir(), "no-herdr-binary.exe"))
	if err != nil {
		t.Fatalf("herdr.NewClient: %v", err)
	}
	coordinator, err := New(store, client, Options{})
	if err != nil {
		t.Fatalf("coord.New: %v", err)
	}

	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	return &reportFixture{t: t, store: store, coord: coordinator, stateDir: stateDir, root: root}
}

// seedProject records one bound primary project.
func (f *reportFixture) seedProject(key, pane, session string) task.Project {
	f.t.Helper()
	project := task.Project{
		Key:                key,
		RepoWorkspaceID:    "ws-repo",
		CurrentWorkspaceID: "ws-repo",
		TerminalID:         "term-primary",
		LaunchPaneID:       pane,
		PublicPaneID:       pane,
		AgentName:          "orch_p_fixture",
		OMPSessionID:       session,
	}
	if err := f.store.CreateProject(context.Background(), project); err != nil {
		f.t.Fatalf("create project %s: %v", key, err)
	}
	return project
}

// seedWorker records one briefed worker task, as the dispatch step machine
// leaves it.
func (f *reportFixture) seedWorker(id, projectKey, pane, session, worktree string) task.Task {
	f.t.Helper()
	worker := task.Task{
		ID:                  id,
		ProjectKey:          projectKey,
		Title:               "Port the parser",
		Brief:               "Port it, with the recorded checks.",
		BaseRevision:        "0123456",
		Branch:              "orch/" + strings.TrimPrefix(id, "orch_"),
		Checks:              []string{"go test ./... ok"},
		WorktreePath:        worktree,
		WorktreeWorkspaceID: reportWorktreeWS,
		CurrentWorkspaceID:  reportWorktreeWS,
		TerminalID:          "term-" + id,
		LaunchPaneID:        pane,
		PublicPaneID:        pane,
		AgentName:           id,
		OMPSessionID:        session,
		Phase:               task.PromptSubmitted,
	}
	if err := f.store.CreateTask(context.Background(), worker); err != nil {
		f.t.Fatalf("create worker %s: %v", id, err)
	}
	return f.reload(id)
}

// seed records the project and the worker most tests use.
func (f *reportFixture) seed() (task.Project, task.Task) {
	f.t.Helper()
	project := f.seedProject(f.root, reportPrimaryPane, reportPrimarySess)
	worker := f.seedWorker("orch_aaaaaaaaaaaa", f.root, reportWorkerPane, reportWorkerSess,
		filepath.Join(f.t.TempDir(), "checkout"))
	return project, worker
}

func (f *reportFixture) reload(id string) task.Task {
	f.t.Helper()
	st, err := f.store.Task(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read task %s: %v", id, err)
	}
	return st
}

func (f *reportFixture) update(st task.Task, mutate func(*task.Task) error) task.Task {
	f.t.Helper()
	updated, err := f.store.UpdateTask(context.Background(), st.ID, st.Revision, mutate)
	if err != nil {
		f.t.Fatalf("update task %s: %v", st.ID, err)
	}
	return updated
}

// completeReport is a report that satisfies every accepted requirement.
func (f *reportFixture) completeReport(worker task.Task) task.Report {
	return task.Report{
		Kind:    "result",
		Summary: "Ported the parser and covered it with the recorded checks.",
		Branch:  worker.Branch,
		Evidence: []string{
			"commit 773efbc4c9a0d7f0f6f2b3f2d5f1c0b8f0a1d2e3 internal/coord/report.go",
			"internal/coord/report.go +214/-3",
		},
		Checks: []task.Check{
			{Name: "go test ./internal/coord", Outcome: task.CheckPassed},
			{Name: "gofmt -l internal/coord", Outcome: task.CheckPassed, Detail: "no output"},
		},
		Risk: "none: the new path is only reachable from the worker role",
	}
}

// inbox reads the durable inbox the way a later coordinator pass would. The
// store exposes no reader for it yet, so the test opens the same database
// read-only: the point is to prove a note was really persisted, not to exercise
// an accessor.
func (f *reportFixture) inbox() []task.Inbox {
	f.t.Helper()
	path := filepath.Join(f.stateDir, "orchestrator.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		f.t.Fatalf("open state read-only: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(), `SELECT id, project_key, task_id, kind, summary, delivery FROM inbox ORDER BY id`)
	if err != nil {
		f.t.Fatalf("read inbox: %v", err)
	}
	defer rows.Close()
	var out []task.Inbox
	for rows.Next() {
		var in task.Inbox
		if err := rows.Scan(&in.ID, &in.ProjectKey, &in.TaskID, &in.Kind, &in.Summary, &in.Delivery); err != nil {
			f.t.Fatalf("scan inbox: %v", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate inbox: %v", err)
	}
	return out
}

// notes lists the durable notes of one kind, which is how "one durable entry"
// is asserted without depending on the note's deduplication key.
func (f *reportFixture) notes(kind string) []task.Inbox {
	f.t.Helper()
	var out []task.Inbox
	for _, note := range f.inbox() {
		if note.Kind == kind {
			out = append(out, note)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// worker questions and progress
// ---------------------------------------------------------------------------

func TestQuestionIsDurableDeduplicatedAndNeedsInput(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	result, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?")
	if err != nil {
		t.Fatalf("Question: %v", err)
	}
	if result.TaskID != worker.ID {
		t.Errorf("Question answered for %q, want %q", result.TaskID, worker.ID)
	}

	after := f.reload(worker.ID)
	if stage := task.StageOf(after); stage != task.NeedsInput {
		t.Errorf("stage after a question = %q, want %q", stage, task.NeedsInput)
	}
	if !strings.Contains(after.Attention, "Which branch should I target?") {
		t.Errorf("attention %q does not carry the question", after.Attention)
	}
	if after.Report != nil {
		t.Errorf("a question created a report: %+v", after.Report)
	}

	notes := f.notes("question")
	if len(notes) != 1 {
		t.Fatalf("question notes = %d, want exactly 1 durable entry", len(notes))
	}
	if notes[0].TaskID != worker.ID || notes[0].ProjectKey != f.root {
		t.Errorf("question note addressed to %+v, want task %s of project %s", notes[0], worker.ID, f.root)
	}
	if !strings.Contains(notes[0].Summary, "Which branch should I target?") {
		t.Errorf("question note summary %q does not carry the question", notes[0].Summary)
	}

	// Asking the same question again is the same fact, so it is one entry.
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?"); err != nil {
		t.Fatalf("Question (repeat): %v", err)
	}
	if got := len(f.notes("question")); got != 1 {
		t.Errorf("question notes after a repeated question = %d, want 1", got)
	}

	// A different question is different work to do, so it is a second entry.
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "And which test command?"); err != nil {
		t.Fatalf("Question (new): %v", err)
	}
	if got := len(f.notes("question")); got != 2 {
		t.Errorf("question notes after a new question = %d, want 2", got)
	}
	if latest := f.reload(worker.ID); !strings.Contains(latest.Attention, "which test command") {
		t.Errorf("attention %q does not carry the latest question", latest.Attention)
	}
}

// TestQuestionIsDurableOnlyTogetherWithItsNote is the same transaction property the
// final report has: the Needs-input marker a question sets and the note that tells
// the primary about it are one fact. A question whose note cannot be written must
// leave the task exactly as it was — no marker, no revision bump, no note — instead
// of leaving it waiting for an answer nobody was asked.
func TestQuestionIsDurableOnlyTogetherWithItsNote(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	f.poisonInbox(t)
	before := f.reload(worker.ID)
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?"); err == nil {
		t.Fatal("Question succeeded although its note could not be written")
	}
	after := f.reload(worker.ID)
	if after.Attention != before.Attention {
		t.Errorf("a question that could not be announced still left the task waiting: attention %q -> %q", before.Attention, after.Attention)
	}
	if after.Revision != before.Revision {
		t.Errorf("a question that could not be announced wrote revision %d (was %d)", after.Revision, before.Revision)
	}
	if stage := task.StageOf(after); stage != task.Running {
		t.Errorf("stage after a question that could not be announced = %q, want %q", stage, task.Running)
	}
	if got := len(f.notes("question")); got != 0 {
		t.Errorf("a failed question left %d question notes", got)
	}

	// With the inbox writable again the same question lands, marker and note
	// together, and one note is exactly what the primary gets.
	f.healInbox(t)
	result, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?")
	if err != nil {
		t.Fatalf("Question after the inbox recovered: %v", err)
	}
	if result.Duplicate {
		t.Error("the question was reported as a duplicate although it never landed")
	}
	asked := f.reload(worker.ID)
	if !strings.Contains(asked.Attention, "Which branch should I target?") {
		t.Errorf("attention %q does not carry the recovered question", asked.Attention)
	}
	if stage := task.StageOf(asked); stage != task.NeedsInput {
		t.Errorf("stage after the recovered question = %q, want %q", stage, task.NeedsInput)
	}
	if got := len(f.notes("question")); got != 1 {
		t.Errorf("question notes after the recovered question = %d, want 1", got)
	}

	// Asking it again is the same fact: the note is deduplicated and the task is
	// not rewritten, so a repeated question cannot bump the revision the primary is
	// watching.
	revision := asked.Revision
	again, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?")
	if err != nil {
		t.Fatalf("Question (repeat): %v", err)
	}
	if !again.Duplicate {
		t.Error("a repeated question was not reported as a duplicate")
	}
	if got := f.reload(worker.ID).Revision; got != revision {
		t.Errorf("a repeated question wrote revision %d (was %d)", got, revision)
	}
	if got := len(f.notes("question")); got != 1 {
		t.Errorf("question notes after a repeated question = %d, want 1", got)
	}

	// A durable question whose note is already recorded survives an unwritable
	// inbox: the fact is unchanged, and the failure is reported rather than
	// silently swallowed.
	f.poisonInbox(t)
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?"); err == nil {
		t.Error("a repeated question reported success although the inbox could not be written")
	}
	unchanged := f.reload(worker.ID)
	if unchanged.Attention != asked.Attention || unchanged.Revision != revision {
		t.Errorf("a repeated question changed the task while its note failed: %+v", unchanged)
	}
}

func TestQuestionAndProgressNeverCompleteATask(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "Should I keep the old API?"); err != nil {
		t.Fatalf("Question: %v", err)
	}
	// A progress note is the partial channel: it is durable, and it neither
	// creates a report nor closes a question.
	first, err := f.coord.Progress(ctx, worker.LaunchPaneID, "Ported the lexer; the parser is next.")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if first.TaskID != worker.ID {
		t.Errorf("Progress answered for %q, want %q", first.TaskID, worker.ID)
	}
	if repeat, err := f.coord.Progress(ctx, worker.LaunchPaneID, "Ported the lexer; the parser is next."); err != nil {
		t.Fatalf("Progress (repeat): %v", err)
	} else if !repeat.Duplicate {
		t.Error("a repeated progress note was not reported as a duplicate")
	}

	after := f.reload(worker.ID)
	if after.Report != nil {
		t.Errorf("progress created a final report: %+v", after.Report)
	}
	if after.Disposition != "" {
		t.Errorf("progress recorded a disposition: %q", after.Disposition)
	}
	if stage := task.StageOf(after); stage != task.NeedsInput {
		t.Errorf("stage after progress = %q, want the pending question to keep it at %q", stage, task.NeedsInput)
	}
	if got := len(f.notes("progress")); got != 1 {
		t.Errorf("progress notes = %d, want 1 deduplicated entry", got)
	}

	// Herdr activity is not completion: the recorded projection of a Herdr
	// "done" agent must leave the task exactly where it was.
	f.update(after, func(next *task.Task) error {
		next.Activity = "done"
		return nil
	})
	promoted := f.reload(worker.ID)
	if promoted.Report != nil || promoted.Disposition != "" {
		t.Fatalf("Herdr's done activity completed the task: %+v", promoted)
	}
	if stage := task.StageOf(promoted); stage == task.ReadyForReview || stage == task.Done {
		t.Errorf("Herdr's done activity moved the task to %q", stage)
	}
}

// stateDB opens the fixture's state database for reading and writing, the way a
// migration or an operator tool would. It is what lets a test make one table
// refuse a write while the rest of the database stays usable.
func (f *reportFixture) stateDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(f.stateDir, "orchestrator.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// poisonInbox makes every insert into the durable inbox fail, the way a locked or
// read-only database does, so a test can prove that a fact never lands without the
// note that announces it.
func (f *reportFixture) poisonInbox(t *testing.T) {
	t.Helper()
	if _, err := f.stateDB(t).ExecContext(context.Background(),
		`CREATE TRIGGER refuse_inbox BEFORE INSERT ON inbox BEGIN SELECT RAISE(ABORT, 'inbox is not writable'); END`); err != nil {
		t.Fatalf("install the inbox trigger: %v", err)
	}
}

func (f *reportFixture) healInbox(t *testing.T) {
	t.Helper()
	if _, err := f.stateDB(t).ExecContext(context.Background(), `DROP TRIGGER refuse_inbox`); err != nil {
		t.Fatalf("remove the inbox trigger: %v", err)
	}
}

func TestReportIsDurableOnlyTogetherWithItsNote(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	// The durable inbox cannot be written at all: a report that would show the
	// task ready for review while nobody was told must not land either, and the
	// revision and attention it touches must be exactly what they were.
	f.poisonInbox(t)
	before := f.reload(worker.ID)
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err == nil {
		t.Fatal("FinalReport succeeded although its note could not be written")
	}
	after := f.reload(worker.ID)
	if after.Report != nil {
		t.Errorf("the report landed without the note that announces it: %+v", after.Report)
	}
	if after.Revision != before.Revision {
		t.Errorf("a report that could not be announced wrote revision %d (was %d)", after.Revision, before.Revision)
	}
	if after.Attention != before.Attention || after.Disposition != before.Disposition {
		t.Errorf("a failed report changed the task: attention %q, disposition %q", after.Attention, after.Disposition)
	}
	if stage := task.StageOf(after); stage != task.Running {
		t.Errorf("stage after a failed report = %q, want %q", stage, task.Running)
	}
	if got := len(f.notes("result")); got != 0 {
		t.Errorf("a failed report left %d result notes", got)
	}

	// The same report whose pending question is waiting to be cleared must roll
	// the cleared attention back too: half a transaction would silently drop the
	// question. The question is recorded while the inbox is writable, so the only
	// failing write is the report's own pair.
	f.healInbox(t)
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?"); err != nil {
		t.Fatalf("Question: %v", err)
	}
	asked := f.reload(worker.ID)
	f.poisonInbox(t)
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err == nil {
		t.Fatal("FinalReport succeeded although its note could not be written")
	}
	rolledBack := f.reload(worker.ID)
	if rolledBack.Attention != asked.Attention {
		t.Errorf("a failed report cleared the pending question: %q, want %q", rolledBack.Attention, asked.Attention)
	}
	if rolledBack.Report != nil || rolledBack.Revision != asked.Revision {
		t.Errorf("a failed report left a partial write: %+v", rolledBack)
	}

	// With the inbox writable again, that same report lands with its note, and
	// the question it answers is cleared in the same write.
	f.healInbox(t)
	result, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker))
	if err != nil {
		t.Fatalf("FinalReport after the inbox recovered: %v", err)
	}
	if result.Duplicate {
		t.Error("the report was reported as a duplicate although it never landed")
	}
	recorded := f.reload(worker.ID)
	if recorded.Report == nil || recorded.Attention != "" {
		t.Fatalf("the recovered report is not complete: %+v", recorded)
	}
	if stage := task.StageOf(recorded); stage != task.ReadyForReview {
		t.Errorf("stage after the recovered report = %q, want %q", stage, task.ReadyForReview)
	}
	if got := len(f.notes("result")); got != 1 {
		t.Errorf("result notes after the recovered report = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// the final report
// ---------------------------------------------------------------------------

func TestReportRejectsEveryIncompleteReport(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*task.Report)
	}{
		{"partial kind", func(r *task.Report) { r.Kind = "partial" }},
		{"missing kind", func(r *task.Report) { r.Kind = "" }},
		{"missing summary", func(r *task.Report) { r.Summary = "  " }},
		{"missing branch", func(r *task.Report) { r.Branch = "" }},
		{"another branch", func(r *task.Report) { r.Branch = "orch/ffffffffffff" }},
		{"no evidence", func(r *task.Report) { r.Evidence = nil }},
		{"blank evidence", func(r *task.Report) { r.Evidence = []string{"  ", ""} }},
		{"no checks", func(r *task.Report) { r.Checks = nil }},
		{"blank checks", func(r *task.Report) {
			r.Checks = []task.Check{{Outcome: task.CheckPassed}, {Name: " ", Outcome: task.CheckPassed}}
		}},
		{"a check with no outcome", func(r *task.Report) { r.Checks = []task.Check{{Name: "go test ./..."}} }},
		{"one check of several with no outcome", func(r *task.Report) {
			r.Checks = append(r.Checks, task.Check{Name: "go vet ./..."})
		}},
		{"a blank outcome", func(r *task.Report) { r.Checks[0].Outcome = "  " }},
		{"an unknown outcome", func(r *task.Report) { r.Checks[0].Outcome = "ok" }},
		{"an outcome in the wrong case", func(r *task.Report) { r.Checks[0].Outcome = "PASSED" }},
		{"an oversized check name", func(r *task.Report) { r.Checks[0].Name = strings.Repeat("x", reportTextLimit+1) }},
		{"an oversized check detail", func(r *task.Report) { r.Checks[0].Detail = strings.Repeat("x", reportTextLimit+1) }},
		{"too many checks", func(r *task.Report) {
			r.Checks = nil
			for i := 0; i <= reportChecksLimit; i++ {
				r.Checks = append(r.Checks, task.Check{Name: fmt.Sprintf("check %d", i), Outcome: task.CheckPassed})
			}
		}},
		{"no risk", func(r *task.Report) { r.Risk = " " }},
		{"oversized summary", func(r *task.Report) { r.Summary = strings.Repeat("x", reportSummaryLimit+1) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := f.completeReport(worker)
			tc.mutate(&report)
			before := f.reload(worker.ID)

			_, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, report)
			if !errors.Is(err, ErrReportInvalid) {
				t.Fatalf("FinalReport(%s) err = %v, want ErrReportInvalid", tc.name, err)
			}
			// The kind mismatches are the ones a worker can actually trigger by
			// sending the wrong note, and the bridge routes an unrouted kind here
			// too, so the message must keep naming what is wrong: "not a final
			// result" is what the live acceptance path asserts the worker is told.
			if tc.name == "partial kind" || tc.name == "missing kind" {
				if !strings.Contains(err.Error(), "not a final result") {
					t.Errorf("FinalReport(%s) message = %q, want the 'not a final result' guidance", tc.name, err.Error())
				}
			}
			after := f.reload(worker.ID)
			if after.Report != nil {
				t.Errorf("a refused report was recorded: %+v", after.Report)
			}
			if after.Revision != before.Revision {
				t.Errorf("a refused report wrote revision %d (was %d)", after.Revision, before.Revision)
			}
			if got := len(f.notes("result")); got != 0 {
				t.Errorf("a refused report left %d result notes", got)
			}
			if stage := task.StageOf(after); stage != task.Running {
				t.Errorf("stage after a refused report = %q, want %q", stage, task.Running)
			}
		})
	}
}

func TestReportMovesToReadyForReviewAndNotesOnce(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	result, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker))
	if err != nil {
		t.Fatalf("FinalReport: %v", err)
	}
	if result.TaskID != worker.ID || result.Duplicate {
		t.Errorf("FinalReport = %+v, want the first report for %s", result, worker.ID)
	}

	after := f.reload(worker.ID)
	if after.Report == nil || after.Report.Kind != "result" {
		t.Fatalf("recorded report = %+v, want a result", after.Report)
	}
	if got, want := after.Report.Summary, f.completeReport(worker).Summary; got != want {
		t.Errorf("recorded summary = %q, want %q", got, want)
	}
	if len(after.Report.Evidence) == 0 || len(after.Report.Checks) == 0 {
		t.Errorf("recorded report lost its evidence or checks: %+v", after.Report)
	}
	if stage := task.StageOf(after); stage != task.ReadyForReview {
		t.Errorf("stage after a final report = %q, want %q", stage, task.ReadyForReview)
	}
	if notes := f.notes("result"); len(notes) != 1 {
		t.Fatalf("result notes = %d, want exactly 1 durable entry", len(notes))
	} else if notes[0].TaskID != worker.ID || notes[0].ProjectKey != f.root {
		t.Errorf("result note addressed to %+v", notes[0])
	}

	// Re-submitting the identical report changes nothing: the task keeps its
	// revision and the primary keeps one note.
	revision := after.Revision
	again, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker))
	if err != nil {
		t.Fatalf("FinalReport (repeat): %v", err)
	}
	if !again.Duplicate {
		t.Error("a repeated report was not reported as a duplicate")
	}
	if got := f.reload(worker.ID).Revision; got != revision {
		t.Errorf("a repeated report wrote revision %d (was %d)", got, revision)
	}
	if got := len(f.notes("result")); got != 1 {
		t.Errorf("result notes after a repeated report = %d, want 1", got)
	}
}

// Every outcome a check can state is a recordable fact. A failed or skipped
// check does not block the report: the worker says what happened, and the
// primary and the user decide. The inbox note names each outcome, so a failure is
// visible without opening the report.
func TestReportRecordsEachCheckOutcomeAndTheNoteStatesThem(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	report := f.completeReport(worker)
	report.Checks = []task.Check{
		{Name: "go test ./internal/coord", Outcome: task.CheckPassed},
		{Name: "go vet ./...", Outcome: task.CheckFailed, Detail: "2 findings in report.go"},
		{Name: "golangci-lint run", Outcome: task.CheckSkipped, Detail: "not installed on this machine"},
	}
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, report); err != nil {
		t.Fatalf("FinalReport with a passed, a failed and a skipped check: %v", err)
	}

	after := f.reload(worker.ID)
	if after.Report == nil || !slices.Equal(after.Report.Checks, report.Checks) {
		t.Fatalf("recorded checks = %+v, want %+v", after.Report, report.Checks)
	}
	if stage := task.StageOf(after); stage != task.ReadyForReview {
		t.Errorf("stage after a report with a failed check = %q, want %q: the checks do not decide the stage", stage, task.ReadyForReview)
	}
	notes := f.notes("result")
	if len(notes) != 1 {
		t.Fatalf("result notes = %d, want 1", len(notes))
	}
	for _, want := range []string{
		"go test ./internal/coord: passed",
		"go vet ./...: failed (2 findings in report.go)",
		"golangci-lint run: skipped (not installed on this machine)",
	} {
		if !strings.Contains(notes[0].Summary, want) {
			t.Errorf("result note %q does not state %q", notes[0].Summary, want)
		}
	}
}

// The checks are recorded in the canonical form the rest of the system compares
// and shows: trimmed, with a blank detail meaning no detail.
func TestReportTrimsCheckFieldsBeforeRecordingThem(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	report := f.completeReport(worker)
	report.Checks = []task.Check{
		{Name: "  go test ./...  ", Outcome: " passed ", Detail: "   "},
		{Name: "go vet ./...", Outcome: "failed\n", Detail: "  2 findings  "},
	}
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, report); err != nil {
		t.Fatalf("FinalReport: %v", err)
	}
	want := []task.Check{
		{Name: "go test ./...", Outcome: task.CheckPassed},
		{Name: "go vet ./...", Outcome: task.CheckFailed, Detail: "2 findings"},
	}
	if got := f.reload(worker.ID).Report.Checks; !slices.Equal(got, want) {
		t.Errorf("recorded checks = %+v, want %+v", got, want)
	}
}

// The refusal has to tell the worker what to fix, because the worker is a model
// reading this message to correct its next call.
func TestReportRefusalNamesTheCheckThatStatesNoOutcome(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		checks []task.Check
		want   []string
	}{
		{
			"none stated",
			[]task.Check{{Name: "go test ./..."}},
			[]string{"check 1", "go test ./...", "outcome", "passed, failed or skipped"},
		},
		{
			"the second of two",
			[]task.Check{{Name: "go test ./...", Outcome: task.CheckPassed}, {Name: "go vet ./..."}},
			[]string{"check 2", "go vet ./...", "outcome"},
		},
		{
			"unknown",
			[]task.Check{{Name: "go test ./...", Outcome: "ok"}},
			[]string{"check 1", `"ok"`, "passed, failed or skipped"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := f.completeReport(worker)
			report.Checks = tc.checks
			_, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, report)
			if !errors.Is(err, ErrReportInvalid) {
				t.Fatalf("FinalReport err = %v, want ErrReportInvalid", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err.Error(), want)
				}
			}
		})
	}
}

// A report stored before checks carried outcomes holds bare strings. It must
// still preview as exactly those strings on the wire (so the review dialog shows
// what it always showed), its checks must still read as stating no outcome, and a
// later write of the same task must keep them as strings: nobody recorded how
// those checks ended.
func TestLegacyStoredReportPreviewsUnchangedAndSurvivesARewrite(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	stored := `{"Kind":"result","Summary":"Ported the parser.","Branch":"` + worker.Branch + `","Evidence":["commit 773efbc"],` +
		`"Checks":["go test ./internal/coord ok","gofmt -l internal/coord (no output)"],"Risk":"none"}`
	if _, err := f.stateDB(t).ExecContext(ctx,
		`UPDATE tasks SET data = json_set(data, '$.Report', json(?)) WHERE id = ?`, stored, worker.ID); err != nil {
		t.Fatalf("store a legacy report: %v", err)
	}

	preview, err := f.coord.ReportPreview(ctx, project.LaunchPaneID, worker.ID)
	if err != nil {
		t.Fatalf("preview of a legacy report: %v", err)
	}
	wire, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"checks":["go test ./internal/coord ok","gofmt -l internal/coord (no output)"]`; !strings.Contains(string(wire), want) {
		t.Errorf("legacy preview on the wire = %s, want it to carry %s", wire, want)
	}
	for i, check := range f.reload(worker.ID).Report.Checks {
		if check.HasOutcome() {
			t.Errorf("legacy check %d reads as having outcome %q", i, check.Outcome)
		}
	}

	// Decide the task: this rewrites the task row for a reason unrelated to the checks.
	if _, err := f.coord.ReviewAtRevision(ctx, project.LaunchPaneID, worker.ID, preview.Revision, "integrated", "accepted"); err != nil {
		t.Fatalf("review of a legacy report: %v", err)
	}
	var checks string
	if err := f.stateDB(t).QueryRowContext(ctx,
		`SELECT json_extract(data, '$.Report.Checks') FROM tasks WHERE id = ?`, worker.ID).Scan(&checks); err != nil {
		t.Fatalf("read the stored checks: %v", err)
	}
	if want := `["go test ./internal/coord ok","gofmt -l internal/coord (no output)"]`; checks != want {
		t.Errorf("stored checks after a rewrite = %s, want %s", checks, want)
	}
}

// A report with outcomes previews each check as one line, "name: outcome
// (detail)", in the list of strings the review confirmation has always printed.
// The wire form is pinned as bytes: an extension that predates outcomes reads it.
func TestPreviewCarriesEachCheckWithItsOutcomeAsOneLine(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	report := f.completeReport(worker)
	report.Checks = []task.Check{
		{Name: "go test ./...", Outcome: task.CheckPassed},
		{Name: "lint", Outcome: task.CheckSkipped, Detail: "not installed"},
	}
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, report); err != nil {
		t.Fatal(err)
	}
	preview, err := f.coord.ReportPreview(ctx, project.LaunchPaneID, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	const want = `"checks":["go test ./...: passed","lint: skipped (not installed)"]`
	if !strings.Contains(string(wire), want) {
		t.Errorf("preview on the wire = %s, want it to carry %s", wire, want)
	}
}

func TestReportClearsOnlyAPendingQuestion(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch should I target?"); err != nil {
		t.Fatalf("Question: %v", err)
	}
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatalf("FinalReport: %v", err)
	}
	answered := f.reload(worker.ID)
	if answered.Attention != "" {
		t.Errorf("attention %q survived the report that answered it", answered.Attention)
	}
	if stage := task.StageOf(answered); stage != task.ReadyForReview {
		t.Errorf("stage after the answered report = %q, want %q", stage, task.ReadyForReview)
	}

	// A failure the dispatch step machine recorded is not a question: the report
	// must not hide it.
	other := f.seedWorker("orch_bbbbbbbbbbbb", f.root, "w3:p1", "omp-worker-2", filepath.Join(t.TempDir(), "checkout"))
	failure := "task " + other.ID + " could not start the worker: the pane is gone"
	f.update(other, func(next *task.Task) error {
		next.Attention = failure
		return nil
	})
	if _, err := f.coord.FinalReport(ctx, other.LaunchPaneID, f.completeReport(f.reload(other.ID))); err != nil {
		t.Fatalf("FinalReport (after a failure): %v", err)
	}
	still := f.reload(other.ID)
	if still.Attention != failure {
		t.Errorf("attention after a report = %q, want the recorded failure %q", still.Attention, failure)
	}
	if stage := task.StageOf(still); stage != task.NeedsInput {
		t.Errorf("stage with a recorded failure = %q, want %q", stage, task.NeedsInput)
	}
}

func TestReportOnlyAPrimaryDispositionMovesATaskToDone(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	// A disposition before the report is reviewed is refused: there is nothing
	// to decide yet.
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "integrated", "accepted"); !errors.Is(err, ErrNotReviewed) {
		t.Fatalf("Review without a report err = %v, want ErrNotReviewed", err)
	}
	if after := f.reload(worker.ID); after.Disposition != "" || task.StageOf(after) == task.Done {
		t.Fatalf("a refused review closed the task: %+v", after)
	}

	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatalf("FinalReport: %v", err)
	}
	if stage := task.StageOf(f.reload(worker.ID)); stage != task.ReadyForReview {
		t.Fatalf("stage after the report = %q, want %q", stage, task.ReadyForReview)
	}

	// An empty outcome is not a decision.
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "   ", "accepted"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Review(empty outcome) err = %v, want ErrInvalidRequest", err)
	}
	if after := f.reload(worker.ID); after.Disposition != "" {
		t.Fatalf("an empty outcome was recorded: %q", after.Disposition)
	}

	disposition := "accepted: the user kept the branch and merged it by hand"
	result, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "integrated", disposition)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.TaskID != worker.ID || result.Stage != task.Done {
		t.Errorf("Review = %+v, want %s done", result, worker.ID)
	}
	done := f.reload(worker.ID)
	if done.Disposition != disposition {
		t.Errorf("recorded disposition = %q, want %q", done.Disposition, disposition)
	}
	if stage := task.StageOf(done); stage != task.Done {
		t.Errorf("stage after the disposition = %q, want %q", stage, task.Done)
	}
	if done.Report == nil || done.Report.Kind != "result" {
		t.Errorf("the disposition lost the reviewed report: %+v", done.Report)
	}
}

func TestReviewPreviewShowsDurableReportAndRefusesStaleDecision(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()
	if _, err := f.coord.ReportPreview(ctx, project.LaunchPaneID, worker.ID); !errors.Is(err, ErrNotReviewed) {
		t.Fatalf("preview without final report = %v, want ErrNotReviewed", err)
	}
	report := f.completeReport(worker)
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, report); err != nil {
		t.Fatal(err)
	}
	preview, err := f.coord.ReportPreview(ctx, project.LaunchPaneID, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.TaskID != worker.ID || preview.Revision != f.reload(worker.ID).Revision ||
		preview.Summary != report.Summary || preview.Branch != report.Branch ||
		preview.Risk != report.Risk || !slices.Equal(preview.Evidence, report.Evidence) ||
		!slices.Equal(preview.Checks, task.CheckLabels(report.Checks)) {
		t.Fatalf("preview did not return the persisted report: %+v", preview)
	}
	changed := report
	changed.Summary = "A corrected result after the preview."
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.coord.ReviewAtRevision(ctx, project.LaunchPaneID, worker.ID, preview.Revision, "integrated", "accepted"); !errors.Is(err, ErrReportChanged) {
		t.Fatalf("decision after report replacement = %v, want ErrReportChanged", err)
	}
	if actual := f.reload(worker.ID); actual.Disposition != "" {
		t.Fatalf("stale decision changed the task: %+v", actual)
	}
	current, err := f.coord.ReportPreview(ctx, project.LaunchPaneID, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Summary != changed.Summary || current.Revision == preview.Revision {
		t.Fatalf("replacement was not visible in new preview: %+v", current)
	}
	if _, err := f.coord.ReviewAtRevision(ctx, project.LaunchPaneID, worker.ID, current.Revision, "integrated", "accepted"); err != nil {
		t.Fatalf("decision on the previewed revision: %v", err)
	}
}

func TestReviewRefusesToRewriteARecordedDecision(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatal(err)
	}
	preview, err := f.coord.ReportPreview(ctx, project.LaunchPaneID, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.coord.ReviewAtRevision(ctx, project.LaunchPaneID, worker.ID, preview.Revision, "integrated", "accepted"); err != nil {
		t.Fatalf("first decision: %v", err)
	}
	closed := f.reload(worker.ID)
	if retry, err := f.coord.ReviewAtRevision(ctx, project.LaunchPaneID, worker.ID, preview.Revision, "integrated", "accepted"); err != nil || retry.Stage != task.Done {
		t.Fatalf("retry after a recorded decision = (%+v, %v), want unchanged done task", retry, err)
	}
	if retry, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "integrated", "accepted"); err != nil || retry.Stage != task.Done {
		t.Fatalf("legacy retry of the same decision = (%+v, %v)", retry, err)
	}
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "rejected", "rejected"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("legacy repeat review = %v, want ErrAlreadyDecided", err)
	}
	latest, err := f.coord.ReportPreview(ctx, project.LaunchPaneID, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.coord.ReviewAtRevision(ctx, project.LaunchPaneID, worker.ID, latest.Revision, "rejected", "rejected"); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("freshly previewed repeat review = %v, want ErrAlreadyDecided", err)
	}
	if after := f.reload(worker.ID); after.Disposition != "accepted" || after.Revision != closed.Revision {
		t.Fatalf("repeat review rewrote the durable decision: %+v", after)
	}
}

// ---------------------------------------------------------------------------
// the disposition of a prerequisite starts the work waiting on it
// ---------------------------------------------------------------------------

// TestReviewDrainsTheTasksItsDispositionUnblocks pins the wiring the plan asks for:
// recording the user's disposition is also what lets the tasks waiting on that
// prerequisite start, so a reviewed decision does not leave dependent work parked
// until something else happens to run.
func TestReviewDrainsTheTasksItsDispositionUnblocks(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()

	first := f.mustDispatch("first", "First brief.")
	dependent, err := f.dispatch("dependent", "Dependent brief.", []string{"go test ./... ok"}, []string{first.ID}, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch the dependent task: %v", err)
	}
	if dependent.Phase != task.Intent || task.StageOf(dependent) != task.Queued {
		t.Fatalf("the dependent task was not gated on its prerequisite: %+v", dependent)
	}

	// The prerequisite is reviewed: the user's decision frees its slot and unblocks the
	// task that was waiting on it.
	f.satisfyReview(first)
	project, err := f.store.Project(ctx, f.projectKey())
	if err != nil {
		t.Fatalf("read the bound project: %v", err)
	}
	result, err := f.coord.Review(ctx, project.LaunchPaneID, first.ID, "integrated", "accepted")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.TaskID != first.ID || result.Disposition != "accepted" || result.Stage != task.Done {
		t.Errorf("Review = %+v, want the recorded disposition on a done task", result)
	}

	drained, err := f.store.Task(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("read the dependent task: %v", err)
	}
	if drained.Phase != task.PromptSubmitted {
		t.Errorf("the dependent task is at phase %q after its prerequisite was reviewed, want %q (task %+v)",
			drained.Phase, task.PromptSubmitted, drained)
	}
	if got := f.prompts(drained.PublicPaneID); len(got) != 1 {
		t.Errorf("the drained task's prompts = %v, want its brief submitted exactly once", got)
	}
}

// A non-accepting outcome closes the prerequisite (Done, slot freed) but must not
// start the task waiting on it, even with no note recorded.
func TestRejectedReviewClosesButDoesNotReleaseDependents(t *testing.T) {
	for _, outcome := range []string{task.OutcomeRejected, task.OutcomeSuperseded, task.OutcomeAbandoned} {
		t.Run(outcome, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			ctx := context.Background()
			first := f.mustDispatch("first", "First brief.")
			dependent, err := f.dispatch("dependent", "Dependent brief.", []string{"go test ./... ok"}, []string{first.ID}, nil, f.base)
			if err != nil {
				t.Fatalf("dispatch the dependent task: %v", err)
			}
			f.satisfyReview(first)
			project, err := f.store.Project(ctx, f.projectKey())
			if err != nil {
				t.Fatalf("read the bound project: %v", err)
			}
			result, err := f.coord.Review(ctx, project.LaunchPaneID, first.ID, outcome, "")
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if result.Outcome != outcome || result.Stage != task.Done {
				t.Errorf("Review = %+v, want a done task with outcome %q", result, outcome)
			}
			after, err := f.store.Task(ctx, dependent.ID)
			if err != nil {
				t.Fatalf("read the dependent task: %v", err)
			}
			if after.Phase != task.Intent || task.StageOf(after) != task.Queued {
				t.Errorf("the dependent of a %s task = %q/%q, want it still queued", outcome, after.Phase, task.StageOf(after))
			}
			if f.countCalls("worktree create") != 1 {
				t.Errorf("a worker started for a dependent of a %s task: %v", outcome, f.calls())
			}
			tasks, err := f.store.Tasks(ctx, f.projectKey())
			if err != nil {
				t.Fatalf("read tasks: %v", err)
			}
			if reason := unsatisfiedDependency(tasks, after); !strings.Contains(reason, "closed as "+outcome) {
				t.Errorf("blocking reason = %q, want it to name the %s outcome", reason, outcome)
			}
		})
	}
}

// TestDrainNeverProvisionsATaskAnotherCallerClaimed pins the property that keeps two
// concurrent drain callers (a review's disposition and another dispatch, say) from
// provisioning the same task twice: the durable claim is exclusive per task, and a drain
// that did not win it leaves that task alone rather than racing the same worktree, the
// same pane and the same brief.
func TestDrainNeverProvisionsATaskAnotherCallerClaimed(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()

	prereq := f.mustDispatch("prereq", "Prereq brief.")
	dependent, err := f.dispatch("dependent", "Dependent brief.", []string{"go test ./... ok"}, []string{prereq.ID}, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch the dependent task: %v", err)
	}
	// The disposition closes the prerequisite, so the dependent task is eligible to
	// be drained: what holds it here is the claim below, not its gate.
	f.recordDisposition(f.satisfyReview(prereq), "accepted")

	// Another caller claims it first, and the claim is exclusive: the same task cannot be
	// claimed twice, so only one caller can ever provision it.
	claimed, _, err := f.store.ClaimProvisioningSlot(ctx, dependent.ID, 3)
	if err != nil || !claimed {
		t.Fatalf("the first claim = (%v, %v), want the task claimed", claimed, err)
	}
	again, _, err := f.store.ClaimProvisioningSlot(ctx, dependent.ID, 3)
	if err != nil {
		t.Fatalf("the second claim: %v", err)
	}
	if again {
		t.Fatal("two callers hold one task's provisioning claim")
	}

	// So a drain skips it instead of provisioning it a second time.
	if _, err := f.coord.DrainQueued(ctx, f.projectKey()); err != nil {
		t.Fatalf("DrainQueued: %v", err)
	}
	after, err := f.store.Task(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("read the dependent task: %v", err)
	}
	if after.Phase != task.Intent {
		t.Errorf("the drain provisioned a task another caller had claimed: %+v", after)
	}
	if got := f.prompts(after.PublicPaneID); len(got) != 0 {
		t.Errorf("the drain submitted a brief for a claimed task: %v", got)
	}
}

// ---------------------------------------------------------------------------
// ownership rechecks
// ---------------------------------------------------------------------------

func TestAnswerRelayRequiresABoundPrimaryAndItsOwnTask(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	otherProject := f.seedProject(filepath.Join(t.TempDir(), "other-repo"), "w8:p1", "omp-primary-2")
	foreign := f.seedWorker("orch_cccccccccccc", otherProject.Key, "w9:p1", "omp-worker-3", filepath.Join(t.TempDir(), "checkout"))
	ctx := context.Background()

	if _, err := f.coord.Answer(ctx, "w7:p7", worker.ID, "keep the old API"); !errors.Is(err, ErrOwnership) {
		t.Errorf("Answer from an unbound pane err = %v, want ErrOwnership", err)
	}
	if _, err := f.coord.Answer(ctx, project.LaunchPaneID, "", "keep the old API"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("Answer without a task err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.coord.Answer(ctx, project.LaunchPaneID, worker.ID, "   "); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("Answer with empty text err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.coord.Answer(ctx, project.LaunchPaneID, foreign.ID, "keep the old API"); !errors.Is(err, ErrOwnership) {
		t.Errorf("Answer for another project's task err = %v, want ErrOwnership", err)
	}
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, foreign.ID, "integrated", "accepted"); !errors.Is(err, ErrOwnership) {
		t.Errorf("Review of another project's task err = %v, want ErrOwnership", err)
	}
	if _, err := f.coord.Cleanup(ctx, project.LaunchPaneID, foreign.ID); !errors.Is(err, ErrOwnership) {
		t.Errorf("Cleanup of another project's task err = %v, want ErrOwnership", err)
	}
	if _, err := f.coord.DialogPreview(ctx, project.LaunchPaneID, foreign.ID); !errors.Is(err, ErrOwnership) {
		t.Errorf("DialogPreview of another project's task err = %v, want ErrOwnership", err)
	}
	if _, err := f.coord.CleanupPreview(ctx, project.LaunchPaneID, foreign.ID); !errors.Is(err, ErrOwnership) {
		t.Errorf("CleanupPreview of another project's task err = %v, want ErrOwnership", err)
	}
	// None of the refusals may have touched the foreign task.
	if after := f.reload(foreign.ID); after.Disposition != "" || after.Attention != "" {
		t.Errorf("a refused primary action wrote to the foreign task: %+v", after)
	}
}

func TestQuestionReportAndProgressRequireTheRecordedWorkerPane(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	if _, err := f.coord.Progress(ctx, "w7:p7", "halfway"); !errors.Is(err, ErrOwnership) {
		t.Errorf("Progress from an unrecorded pane err = %v, want ErrOwnership", err)
	}
	if _, err := f.coord.Question(ctx, "w7:p7", "which branch?"); !errors.Is(err, ErrOwnership) {
		t.Errorf("Question from an unrecorded pane err = %v, want ErrOwnership", err)
	}
	// Ownership is decided before the report is judged: an unrecorded pane must
	// never be told that its report is at fault.
	if _, err := f.coord.FinalReport(ctx, "w7:p7", f.completeReport(worker)); !errors.Is(err, ErrOwnership) {
		t.Errorf("FinalReport from an unrecorded pane err = %v, want ErrOwnership", err)
	}
	// The primary's own pane is not a worker pane.
	if _, err := f.coord.Question(ctx, project.LaunchPaneID, "which branch?"); !errors.Is(err, ErrOwnership) {
		t.Errorf("Question from the primary pane err = %v, want ErrOwnership", err)
	}
	// A worker whose project is not bound has nowhere to report: its pane is
	// recorded, but the project ownership recheck refuses it.
	unbound := f.seedWorker("orch_dddddddddddd", filepath.Join(t.TempDir(), "gone-repo"), "w6:p1", "omp-worker-4", filepath.Join(t.TempDir(), "checkout"))
	if _, err := f.coord.Progress(ctx, unbound.LaunchPaneID, "halfway"); !errors.Is(err, ErrProjectUnknown) {
		t.Errorf("Progress for an unbound project err = %v, want ErrProjectUnknown", err)
	}
	if _, err := f.coord.Question(ctx, unbound.LaunchPaneID, "which branch?"); !errors.Is(err, ErrProjectUnknown) {
		t.Errorf("Question for an unbound project err = %v, want ErrProjectUnknown", err)
	}
}

// ---------------------------------------------------------------------------
// recorded input attempts
// ---------------------------------------------------------------------------

// TestInputAttemptMarkerNamesTheChannelAndNeverTheText pins the durable record of an
// attempt to write into a worker's pane: it has to identify the channel and exactly
// what was sent (so a repeat of the same attempt is recognisable, and a different one
// is not), without storing what the user said or which keys were approved.
func TestInputAttemptMarkerNamesTheChannelAndNeverTheText(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_123)
	const nonce = "0123456789abcdef"

	answer := newAnswerAttempt("the API stays as it is", nonce, now)
	marker := answer.String()
	if !strings.Contains(marker, inputAttemptPrefix) || !strings.Contains(marker, channelAnswer) {
		t.Errorf("marker %q does not name an answer attempt", marker)
	}
	if strings.Contains(marker, "the API") || strings.Contains(marker, "stays") {
		t.Errorf("marker %q carries the answer itself", marker)
	}
	if !strings.Contains(marker, formatMillis(now)) {
		t.Errorf("marker %q does not carry when the attempt was made", marker)
	}
	parsed, ok := parseInputAttempt(marker)
	if !ok {
		t.Fatalf("marker %q does not parse back", marker)
	}
	if !parsed.sameAs(answer) {
		t.Errorf("parsed attempt = %+v, want the recorded one %+v", parsed, answer)
	}
	// A different answer is a different attempt, even at the same instant.
	if other := newAnswerAttempt("the API changes", nonce, now); parsed.sameAs(other) {
		t.Error("two different answers share one attempt identity")
	}
	// So is the *same* answer sent again: the first one's outcome is what a settlement
	// decides about, so the second is a new attempt and not the one that was settled.
	if repeat := newAnswerAttempt("the API stays as it is", "fedcba9876543210", now); parsed.sameAs(repeat) {
		t.Error("the same answer sent again shares the first attempt's identity")
	}

	keys := newDialogAttempt("0123456789abcdef", []string{"1", "ENTER"}, nonce, now)
	keyMarker := keys.String()
	if !strings.Contains(keyMarker, channelDialog) || !strings.Contains(keyMarker, "0123456789abcdef") {
		t.Errorf("marker %q does not tie the keys to the dialog that was approved", keyMarker)
	}
	if strings.Contains(keyMarker, "ENTER") {
		t.Errorf("marker %q carries the approved keys", keyMarker)
	}
	parsedKeys, ok := parseInputAttempt(keyMarker)
	if !ok {
		t.Fatalf("marker %q does not parse back", keyMarker)
	}
	if !parsedKeys.sameAs(keys) {
		t.Errorf("parsed attempt = %+v, want %+v", parsedKeys, keys)
	}
	// The same keys for another dialog, or other keys for the same dialog, are not
	// the same attempt.
	if parsedKeys.sameAs(newDialogAttempt("fedcba9876543210", []string{"1", "ENTER"}, nonce, now)) {
		t.Error("two dialogs share one attempt identity")
	}
	if parsedKeys.sameAs(newDialogAttempt("0123456789abcdef", []string{"2"}, nonce, now)) {
		t.Error("two approvals share one attempt identity")
	}
	if parsedKeys.sameAs(newDialogAttempt("0123456789abcdef", []string{"1", "ENTER"}, "fedcba9876543210", now)) {
		t.Error("the same approval made again shares the first attempt's identity")
	}

	// Anything that is not an attempt — a dispatch blocker, a truncated or malformed
	// marker — is never read as a pending attempt, so a task blocked for another
	// reason is not mistaken for one waiting on input.
	for _, notAnAttempt := range []string{
		"",
		"task orch_x could not start the worker: the pane is gone",
		inputAttemptPrefix,
		inputAttemptPrefix + "answer",
		inputAttemptPrefix + "answer deadbeef",
		inputAttemptPrefix + "bogus deadbeef - 1700000000123",
		inputAttemptPrefix + "answer deadbeef - not-a-time",
		// A marker of the right shape but the wrong alphabet was not written here.
		inputAttemptPrefix + "answer deadbeef - zzzzzzzzzzzzzzzz 1700000000123",
		inputAttemptPrefix + "answer deadbeef - 0123456789abcde 1700000000123",
	} {
		if _, ok := parseInputAttempt(notAnAttempt); ok {
			t.Errorf("%q is read as a pending input attempt", notAnAttempt)
		}
	}
}

// TestBlockerRulesNeverEraseAnAttemptTheyDidNotSettle pins the two rules that make
// recording and clearing an attempt safe under concurrent calls: a recorded attempt only
// gives way to the exact attempt a caller proved settled, and only the exact attempt the
// user decided about is ever cleared.
func TestBlockerRulesNeverEraseAnAttemptTheyDidNotSettle(t *testing.T) {
	at := time.UnixMilli(1_700_000_000_123)
	mine := newAnswerAttempt("keep the old API", "0123456789abcdef", at)
	theirs := newAnswerAttempt("change the API", "fedcba9876543210", at)
	// The same words sent again are a different attempt: a decision about the first must
	// not clear the second, whose outcome nobody has looked at yet.
	repeatOfMine := newAnswerAttempt("keep the old API", "0011223344556677", at)
	foreign := "task orch_x could not start the worker: the pane is gone"

	cases := []struct {
		name    string
		blocker string
		settled *inputAttempt
		want    error
	}{
		{"an empty record accepts a new attempt", "", nil, nil},
		{"an attempt nobody settled is never replaced", mine.String(), nil, ErrInputPending},
		{"another attempt is never replaced", mine.String(), &theirs, ErrInputPending},
		{"the same payload sent again is another attempt", mine.String(), &repeatOfMine, ErrInputPending},
		{"the attempt that was proved settled is replaced", mine.String(), &mine, nil},
		{"a blocker this coordinator did not write is never sent over", foreign, &mine, ErrBlocked},
	}
	for _, tc := range cases {
		if got := occupiedBlocker(task.Task{Blocker: tc.blocker}, tc.settled); !errors.Is(got, tc.want) {
			t.Errorf("%s: occupiedBlocker = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The same discipline for clearing: only the attempt the user decided about.
	if err := settleableBlocker(task.Task{Blocker: mine.String()}, mine); err != nil {
		t.Errorf("settling the recorded attempt = %v, want nil", err)
	}
	for name, tc := range map[string]struct {
		blocker string
		decided inputAttempt
	}{
		"nothing recorded":    {"", mine},
		"a different attempt": {theirs.String(), mine},
		"a foreign blocker":   {foreign, mine},
	} {
		if err := settleableBlocker(task.Task{Blocker: tc.blocker}, tc.decided); !errors.Is(err, ErrInputPending) {
			t.Errorf("%s: settleableBlocker = %v, want ErrInputPending", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// dialog digests
// ---------------------------------------------------------------------------

func TestDialogDigestIgnoresTerminalRenderingNoise(t *testing.T) {
	crlf := "Choose an option:\r\n  1. Allow once\r\n  2. Deny\r\n\r\n"
	extraBlank := "Choose an option:\n  1. Allow once\n  2. Deny\n\n\n"
	plain := "Choose an option:\n  1. Allow once\n  2. Deny\n"
	changed := "Choose an option:\n  1. Allow once\n  2. Deny\n  3. Always allow\n"

	if digestDialog(crlf) != digestDialog(plain) {
		t.Error("the same dialog rendered with CRLF hashed differently")
	}
	if digestDialog(extraBlank) != digestDialog(plain) {
		t.Error("trailing blank lines changed the dialog digest")
	}
	if digestDialog(changed) == digestDialog(plain) {
		t.Error("a changed dialog hashed the same")
	}
	if digestDialog("") == "" {
		t.Error("an empty dialog hashed to the empty string")
	}
}

func TestDialogApprovalRequiresAnObservedDigest(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	if _, err := f.coord.ApproveDialog(ctx, project.LaunchPaneID, worker.ID, "", []string{"1"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("ApproveDialog without a digest err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.coord.ApproveDialog(ctx, project.LaunchPaneID, worker.ID, "deadbeef", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("ApproveDialog without keys err = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.coord.ApproveDialog(ctx, project.LaunchPaneID, worker.ID, "deadbeef", []string{" "}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("ApproveDialog with a blank key err = %v, want ErrInvalidRequest", err)
	}
}

// ---------------------------------------------------------------------------
// cleanup gates that need no terminal
// ---------------------------------------------------------------------------

func TestCleanupRefusesAnythingButAReviewedDoneTask(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	// Not reported yet.
	if _, err := f.coord.Cleanup(ctx, project.LaunchPaneID, worker.ID); !errors.Is(err, ErrNotReviewed) {
		t.Fatalf("Cleanup before a report err = %v, want ErrNotReviewed", err)
	}
	// Reported but never disposed: the user has not decided, so nothing may be
	// removed.
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatalf("FinalReport: %v", err)
	}
	if _, err := f.coord.Cleanup(ctx, project.LaunchPaneID, worker.ID); !errors.Is(err, ErrNotReviewed) {
		t.Fatalf("Cleanup after a report without a disposition err = %v, want ErrNotReviewed", err)
	}
	if after := f.reload(worker.ID); after.WorktreeWorkspaceID == "" || after.LaunchPaneID == "" {
		t.Fatalf("a refused cleanup cleared the recorded resources: %+v", after)
	}
	// Reviewed Done, but the task records no checkout to remove.
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "integrated", "accepted"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	f.update(f.reload(worker.ID), func(next *task.Task) error {
		next.WorktreePath = ""
		return nil
	})
	if _, err := f.coord.Cleanup(ctx, project.LaunchPaneID, worker.ID); !errors.Is(err, ErrProvenance) {
		t.Fatalf("Cleanup without a recorded checkout err = %v, want ErrProvenance", err)
	}
}

func TestCleanupCheckoutStatusComesFromGit(t *testing.T) {
	f := newReportFixture(t)
	root := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "orchestrator@example.invalid"},
		{"config", "user.name", "Orchestrator Test"},
		{"config", "core.autocrlf", "false"},
	} {
		if out, err := runGit(root, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	writeRepoFile(t, root, "README.md", "first\n")
	commitAll(t, root, "first commit")
	checkout := filepath.Join(t.TempDir(), "checkout")
	if out, err := runGit(root, "worktree", "add", "-b", "orch/aaaaaaaaaaaa", checkout, "HEAD"); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	ctx := context.Background()

	status, err := f.coord.checkoutStatus(ctx, checkout)
	if err != nil {
		t.Fatalf("checkoutStatus(clean): %v", err)
	}
	if strings.TrimSpace(status) != "" {
		t.Errorf("a clean checkout reported %q", status)
	}

	// Untracked work is work the checkout would lose, so it must read dirty.
	if err := os.WriteFile(filepath.Join(checkout, "notes.txt"), []byte("kept\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	status, err = f.coord.checkoutStatus(ctx, checkout)
	if err != nil {
		t.Fatalf("checkoutStatus(dirty): %v", err)
	}
	if !strings.Contains(status, "notes.txt") {
		t.Errorf("a dirty checkout reported %q, want the untracked file named", status)
	}

	// A checkout that does not exist cannot be inspected, so it is refused
	// rather than treated as clean.
	if _, err := f.coord.checkoutStatus(ctx, filepath.Join(checkout, "gone")); err == nil {
		t.Error("a missing checkout was reported as inspectable")
	}
}

func TestCleanupProvenanceFollowsTheRecordedCheckout(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "checkout")
	branch := "orch/aaaaaaaaaaaa"
	worker := task.Task{
		ID:                  "orch_aaaaaaaaaaaa",
		Branch:              branch,
		WorktreePath:        checkout,
		WorktreeWorkspaceID: "w7",
		CurrentWorkspaceID:  "w9",
	}

	t.Run("a moved worker keeps its checkout", func(t *testing.T) {
		// The pane moved to another workspace, so the worktree listing names the
		// destination; the source workspace may already be closed.
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: filepath.Dir(checkout), Branch: new("main")},
			{Path: checkout, Branch: new(branch), OpenWorkspaceID: new("w9"), IsLinkedWorktree: true},
		}}
		target, err := provenanceWorkspace(worker, listed)
		if err != nil {
			t.Fatalf("provenanceWorkspace: %v", err)
		}
		if target.WorkspaceID != "w9" || !target.Held {
			t.Errorf("target = %+v, want the recorded destination w9 still holding the checkout", target)
		}
	})

	t.Run("the recorded checkout path must be listed", func(t *testing.T) {
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: filepath.Join(filepath.Dir(checkout), "someone-else"), Branch: new(branch), OpenWorkspaceID: new("w9")},
		}}
		if _, err := provenanceWorkspace(worker, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})
	t.Run("an unclassified worktree prevents an absent-checkout conclusion", func(t *testing.T) {
		other := filepath.Join(filepath.Dir(checkout), "unknown")
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: other, IsLinkedWorktree: true},
		}}
		attempt := task.CleanupAttempt{Path: checkout, Branch: branch, WorkspaceID: "w7"}
		if _, _, err := cleanupListingTarget(worker, attempt, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("cleanupListingTarget err = %v, want ErrProvenance", err)
		}
	})
	t.Run("a detached stale branch report does not block absent-checkout settlement", func(t *testing.T) {
		other := filepath.Join(filepath.Dir(checkout), "detached")
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: other, Branch: new(branch), IsDetached: true},
		}}
		attempt := task.CleanupAttempt{Path: checkout, Branch: branch, WorkspaceID: "w7"}
		_, absent, err := cleanupListingTarget(worker, attempt, listed)
		if err != nil || !absent {
			t.Fatalf("cleanupListingTarget = absent %t, err %v; want stale detached branch ignored", absent, err)
		}
	})
	t.Run("an attached entry on the task branch prevents absent-checkout settlement", func(t *testing.T) {
		other := filepath.Join(filepath.Dir(checkout), "attached")
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: other, Branch: new(branch), IsLinkedWorktree: true},
		}}
		attempt := task.CleanupAttempt{Path: checkout, Branch: branch, WorkspaceID: "w7"}
		if _, _, err := cleanupListingTarget(worker, attempt, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("cleanupListingTarget err = %v, want ErrProvenance for an attached task branch", err)
		}
	})
	t.Run("a detached entry without a branch cannot conceal a checked-out branch", func(t *testing.T) {
		other := filepath.Join(filepath.Dir(checkout), "detached-without-branch")
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: other, IsDetached: true},
		}}
		attempt := task.CleanupAttempt{Path: checkout, Branch: branch, WorkspaceID: "w7"}
		_, absent, err := cleanupListingTarget(worker, attempt, listed)
		if err != nil || !absent {
			t.Fatalf("cleanupListingTarget = absent %t, err %v; want detached entry ignored and exact checkout absent", absent, err)
		}
	})

	t.Run("the recorded branch must match", func(t *testing.T) {
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: checkout, Branch: new("orch/bbbbbbbbbbbb"), OpenWorkspaceID: new("w9"), IsLinkedWorktree: true},
		}}
		if _, err := provenanceWorkspace(worker, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})

	t.Run("an unknown workspace is refused", func(t *testing.T) {
		// A workspace this task never recorded is not its checkout: a human
		// decides what happened to it.
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: checkout, Branch: new(branch), OpenWorkspaceID: new("w4"), IsLinkedWorktree: true},
		}}
		if _, err := provenanceWorkspace(worker, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})

	t.Run("a checkout no workspace holds is reported as unheld", func(t *testing.T) {
		// Herdr names no workspace for the checkout: the workspace that held it is
		// gone, so the removal has to open the recorded checkout first.
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: checkout, Branch: new(branch), IsLinkedWorktree: true},
		}}
		target, err := provenanceWorkspace(worker, listed)
		if err != nil {
			t.Fatalf("provenanceWorkspace: %v", err)
		}
		if target.WorkspaceID != "w7" || target.Held {
			t.Errorf("target = %+v, want the recorded worktree workspace w7 and not held", target)
		}
	})

	t.Run("an unheld checkout with no recorded workspace is refused", func(t *testing.T) {
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: checkout, Branch: new(branch), IsLinkedWorktree: true},
		}}
		unrecorded := worker
		unrecorded.WorktreeWorkspaceID = ""
		unrecorded.CurrentWorkspaceID = ""
		// Nothing identifies a workspace to remove, so a human decides.
		if _, err := provenanceWorkspace(unrecorded, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})

	t.Run("a task that records no checkout is refused", func(t *testing.T) {
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{{Path: checkout, Branch: new(branch)}}}
		if _, err := provenanceWorkspace(task.Task{Branch: branch}, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})
}

// TestCleanupRecoversOnlyFromRefusalsThatProveTheCheckoutMoved pins the set of
// Herdr refusals cleanup recovers from by opening the recorded checkout: a
// workspace the move closed, and a workspace that is not the checkout's
// Herdr-managed worktree workspace. Anything else is reported as it is, because
// nothing about it proves the checkout's registration is merely stale.
func TestCleanupRecoversOnlyFromRefusalsThatProveTheCheckoutMoved(t *testing.T) {
	// The two real refusals the production probe produced.
	closed := &herdr.CLIError{Op: "worktree remove", Code: "workspace_not_found", Message: "workspace wR no longer exists"}
	notLinked := &herdr.CLIError{Op: "worktree remove", Code: herdr.CodeNotLinkedWorktree, Message: "workspace is not a Herdr-managed worktree checkout"}
	if !isMissingTarget(closed) {
		t.Error("a closed workspace is not classified as a missing target")
	}
	if !isNotLinkedWorktree(notLinked) {
		t.Errorf("code %q is not classified as not_linked_worktree", herdr.CodeNotLinkedWorktree)
	}

	// A refusal that proves nothing about the registration is not recoverable.
	for _, err := range []error{
		&herdr.CLIError{Op: "worktree remove", Code: "worktree_dirty", Message: "the checkout has uncommitted changes"},
		&herdr.CLIError{Op: "worktree remove", Code: "permission_denied", Message: "the checkout could not be deleted"},
		errors.New("herdr worktree remove: ambiguous outcome"),
	} {
		if isNotLinkedWorktree(err) || isMissingTarget(err) {
			t.Errorf("%v is treated as a stale registration", err)
		}
	}
}

// ---------------------------------------------------------------------------
// a closed task accepts no more worker writes
// ---------------------------------------------------------------------------

// TestProgressQuestionAndReportAreRefusedAfterTheDisposition pins that the
// primary's disposition is final for the worker's channel: once the user has
// closed a task, the worker that produced it may not add a progress note, ask a
// question, or replace the report the decision was made about.
func TestProgressQuestionAndReportAreRefusedAfterTheDisposition(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatalf("FinalReport: %v", err)
	}
	reviewed := f.reload(worker.ID)
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "integrated", "accepted: merged by hand"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	closed := f.reload(worker.ID)
	if stage := task.StageOf(closed); stage != task.Done || closed.Disposition == "" {
		t.Fatalf("task after the disposition = %q/%q, want a closed task", stage, closed.Disposition)
	}
	notes := len(f.inbox())

	// The partial channel is refused: a closed task must not start reading as work
	// in flight again.
	if _, err := f.coord.Progress(ctx, worker.LaunchPaneID, "still working on it"); err == nil {
		t.Error("Progress succeeded for a task the disposition closed")
	}
	// A question is refused too: it would move the closed task back to needs input.
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, "One more decision?"); err == nil {
		t.Error("Question succeeded for a task the disposition closed")
	}
	// And a new report may not replace the report the user decided on.
	changed := f.completeReport(reviewed)
	changed.Summary = "A second, different result."
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, changed); err == nil {
		t.Error("FinalReport succeeded for a task the disposition closed")
	}

	after := f.reload(worker.ID)
	if after.Disposition != closed.Disposition {
		t.Errorf("the disposition changed: %q, want %q", after.Disposition, closed.Disposition)
	}
	if stage := task.StageOf(after); stage != task.Done {
		t.Errorf("stage after the refused writes = %q, want %q", stage, task.Done)
	}
	if after.Attention != "" {
		t.Errorf("a refused write recorded attention on a closed task: %q", after.Attention)
	}
	if after.Report == nil || after.Report.Summary == changed.Summary {
		t.Errorf("a report written after the disposition replaced the decided one: %+v", after.Report)
	}
	if after.Revision != closed.Revision {
		t.Errorf("a refused write changed the revision: %d -> %d", closed.Revision, after.Revision)
	}
	if got := len(f.inbox()); got != notes {
		t.Errorf("a refused write left %d new inbox notes", got-notes)
	}
}

// TestStaleWorkerPreflightCannotCommitAfterADisposition pins the race the fast
// workerTask precheck cannot close on its own.
//
// A worker call reads the task in workerTask, and the primary's Review can commit
// before the write that follows. Every write therefore re-checks the disposition
// where it lands, from the snapshot the preflight took: a task+note write does it in
// its mutation callback, a note-only write in the note's own store transaction that
// never moves the revision. The stale snapshot below is exactly what a call paused
// between its preflight and its write holds, and every path must refuse it with
// ErrOwnership (the bridge's forbidden) instead of writing over the decision.
func TestStaleWorkerPreflightCannotCommitAfterADisposition(t *testing.T) {
	f := newReportFixture(t)
	project, worker := f.seed()
	ctx := context.Background()

	// The snapshot a worker's progress would have taken while the task was open.
	staleProgress := f.reload(worker.ID)

	// A question already on the task: the snapshot a repeated question would have
	// taken, whose marker already matches, so its write is the note alone.
	const asked = "Which branch should I keep?"
	if _, err := f.coord.Question(ctx, worker.LaunchPaneID, asked); err != nil {
		t.Fatalf("Question: %v", err)
	}
	staleDuplicateQuestion := f.reload(worker.ID)

	// The worker's result, then the primary's decision about it.
	if _, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatalf("FinalReport: %v", err)
	}
	staleDuplicateReport := f.reload(worker.ID)
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "integrated", "accepted: merged by hand"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	closed := f.reload(worker.ID)
	if !isClosed(closed) {
		t.Fatalf("task after the disposition = %+v, want a closed task", closed)
	}
	notes := len(f.inbox())

	// A question from the stale snapshot: the disposition is re-checked before the
	// attention marker it would have set.
	if _, err := f.coord.question(ctx, staleProgress, "Did the decision land?"); !errors.Is(err, ErrOwnership) {
		t.Errorf("question from a stale snapshot err = %v, want ErrOwnership", err)
	}
	// The same question again, which would only (re)confirm its deduplicated note.
	if _, err := f.coord.question(ctx, staleDuplicateQuestion, asked); !errors.Is(err, ErrOwnership) {
		t.Errorf("duplicate question from a stale snapshot err = %v, want ErrOwnership", err)
	}
	// A report the snapshot never recorded: the task+note write.
	changed := f.completeReport(staleProgress)
	changed.Summary = "A result the user never decided on."
	if _, err := f.coord.finalReport(ctx, staleProgress, changed); !errors.Is(err, ErrOwnership) {
		t.Errorf("final report from a stale snapshot err = %v, want ErrOwnership", err)
	}
	// The identical report again, which would only (re)confirm its note.
	if _, err := f.coord.finalReport(ctx, staleDuplicateReport, f.completeReport(worker)); !errors.Is(err, ErrOwnership) {
		t.Errorf("duplicate final report from a stale snapshot err = %v, want ErrOwnership", err)
	}
	// Progress writes no task field and no revision, so its guard can live only in
	// the note's own transaction.
	if _, err := f.coord.progress(ctx, staleProgress, "still working on it"); !errors.Is(err, ErrOwnership) {
		t.Errorf("progress from a stale snapshot err = %v, want ErrOwnership", err)
	}

	after := f.reload(worker.ID)
	if after.Disposition != closed.Disposition {
		t.Errorf("the disposition changed: %q, want %q", after.Disposition, closed.Disposition)
	}
	if after.Revision != closed.Revision {
		t.Errorf("a refused stale write changed the revision: %d -> %d", closed.Revision, after.Revision)
	}
	if after.Attention != closed.Attention {
		t.Errorf("a refused stale write changed the attention: %q, want %q", after.Attention, closed.Attention)
	}
	if after.Report == nil || closed.Report == nil || after.Report.Summary != closed.Report.Summary {
		t.Errorf("a refused stale write replaced the decided report: %+v", after.Report)
	}
	if got := len(f.inbox()); got != notes {
		t.Errorf("a refused stale write left %d new inbox notes", got-notes)
	}
}

// TestDuplicateWorkerWritesStillLandWhileTheTaskIsOpen is the other half of the
// guard: the disposition re-check must not be a blanket refusal. A task that is still
// open keeps its deduplicated semantics — a repeated question and a repeated report
// only re-confirm their note and never move the revision, and progress deduplicates
// the same way.
func TestDuplicateWorkerWritesStillLandWhileTheTaskIsOpen(t *testing.T) {
	f := newReportFixture(t)
	_, worker := f.seed()
	ctx := context.Background()

	// Progress twice: one durable entry, and the task is not touched at all.
	if result, err := f.coord.Progress(ctx, worker.LaunchPaneID, "halfway"); err != nil {
		t.Fatalf("Progress: %v", err)
	} else if result.Duplicate {
		t.Errorf("the first progress note was reported as a duplicate: %+v", result)
	}
	opened := f.reload(worker.ID)
	if result, err := f.coord.Progress(ctx, worker.LaunchPaneID, "halfway"); err != nil {
		t.Fatalf("Progress (repeat): %v", err)
	} else if !result.Duplicate {
		t.Errorf("a repeated progress note was not a duplicate: %+v", result)
	}
	if after := f.reload(worker.ID); after.Revision != opened.Revision {
		t.Errorf("recording progress moved the revision: %d -> %d", opened.Revision, after.Revision)
	}
	if got := len(f.notes("progress")); got != 1 {
		t.Errorf("progress notes = %d, want 1", got)
	}

	if result, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch?"); err != nil {
		t.Fatalf("Question: %v", err)
	} else if result.Duplicate {
		t.Errorf("the first question was reported as a duplicate: %+v", result)
	}
	asked := f.reload(worker.ID)
	if result, err := f.coord.Question(ctx, worker.LaunchPaneID, "Which branch?"); err != nil {
		t.Fatalf("Question (repeat): %v", err)
	} else if !result.Duplicate {
		t.Errorf("a repeated question was not a duplicate: %+v", result)
	}
	if after := f.reload(worker.ID); after.Revision != asked.Revision {
		t.Errorf("a repeated question moved the revision: %d -> %d", asked.Revision, after.Revision)
	}

	if result, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatalf("FinalReport: %v", err)
	} else if result.Duplicate {
		t.Errorf("the first report was reported as a duplicate: %+v", result)
	}
	reported := f.reload(worker.ID)
	if result, err := f.coord.FinalReport(ctx, worker.LaunchPaneID, f.completeReport(worker)); err != nil {
		t.Fatalf("FinalReport (repeat): %v", err)
	} else if !result.Duplicate {
		t.Errorf("a repeated report was not a duplicate: %+v", result)
	}
	if after := f.reload(worker.ID); after.Revision != reported.Revision {
		t.Errorf("a repeated report moved the revision: %d -> %d", reported.Revision, after.Revision)
	}
	if got := len(f.notes("result")); got != 1 {
		t.Errorf("result notes = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// the drain's failure surface
// ---------------------------------------------------------------------------

// seedQueued records one admitted, unprovisioned task: the shape a dispatch leaves
// when the budget is full or a dependency is not closed yet.
func (f *reportFixture) seedQueued(id, projectKey, attention string) task.Task {
	f.t.Helper()
	queued := task.Task{
		ID:           id,
		ProjectKey:   projectKey,
		Title:        "Queued work",
		Brief:        "Do the queued work.",
		BaseRevision: "0123456",
		Branch:       "orch/" + strings.TrimPrefix(id, "orch_"),
		Checks:       []string{"go test ./... ok"},
		Attention:    attention,
		Phase:        task.Intent,
	}
	if err := f.store.CreateTask(context.Background(), queued); err != nil {
		f.t.Fatalf("create queued task %s: %v", id, err)
	}
	return f.reload(id)
}

// TestDrainQueuedSurfacesStorageFailures pins the drain's error surface. A drain
// is the surface that admits queued work at the moment a gate opens, so a storage
// failure anywhere in its walk has to reach the caller: a swallowed failure looks
// exactly like "nothing was eligible", and the queue would sit still with nobody
// knowing why. A spent budget is the exception — that is the normal reason a
// queued task waits, not a failure.
func TestDrainQueuedSurfacesStorageFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("a project task read that fails", func(t *testing.T) {
		f := newReportFixture(t)
		project, _ := f.seed()
		f.seedQueued("orch_queued000001", project.Key, "")
		raw := f.stateDB(t)
		if _, err := raw.ExecContext(ctx, `ALTER TABLE tasks RENAME TO tasks_hidden`); err != nil {
			t.Fatalf("hide the tasks table: %v", err)
		}
		if _, err := f.coord.DrainQueued(ctx, project.Key); err == nil {
			t.Error("DrainQueued swallowed a failed project task read")
		}
		if _, err := raw.ExecContext(ctx, `ALTER TABLE tasks_hidden RENAME TO tasks`); err != nil {
			t.Fatalf("restore the tasks table: %v", err)
		}
	})

	t.Run("a slot claim that cannot be written", func(t *testing.T) {
		f := newReportFixture(t)
		project, _ := f.seed()
		f.seedQueued("orch_queued000002", project.Key, "")
		if _, err := f.stateDB(t).ExecContext(ctx, `CREATE TRIGGER refuse_slot_claim
			BEFORE UPDATE OF slot_claimed_at ON tasks
			WHEN NEW.slot_claimed_at > 0
			BEGIN SELECT RAISE(ABORT, 'slot claim is not writable'); END`); err != nil {
			t.Fatalf("install the slot-claim trigger: %v", err)
		}
		_, err := f.coord.DrainQueued(ctx, project.Key)
		if err == nil {
			t.Fatal("DrainQueued swallowed the storage failure of the slot claim")
		}
		if errors.Is(err, state.ErrBudgetExhausted) {
			t.Errorf("a storage failure was reported as a spent budget: %v", err)
		}
	})

	t.Run("a spent budget is not a failure", func(t *testing.T) {
		f := newReportFixture(t)
		project, _ := f.seed()
		panes := []string{"w3:p1", "w4:p1"}
		for i, id := range []string{"orch_bbbbbbbbbbbb", "orch_cccccccccccc"} {
			f.seedWorker(id, project.Key, panes[i], "omp-worker-"+id, filepath.Join(f.root, "checkout-"+id))
		}
		queued := f.seedQueued("orch_queued000003", project.Key, "")
		drained, err := f.coord.DrainQueued(ctx, project.Key)
		if err != nil {
			t.Fatalf("DrainQueued with a spent budget = %v, want the normal no-progress answer", err)
		}
		if len(drained) != 0 {
			t.Fatalf("DrainQueued = %+v, want nothing provisioned past the budget", drained)
		}
		if still := f.reload(queued.ID); still.Phase != task.Intent || still.Attention != "" {
			t.Errorf("a budget-refused task changed: %+v", still)
		}
	})

	t.Run("an attention clear that cannot be written", func(t *testing.T) {
		f := newReportFixture(t)
		project, _ := f.seed()
		queued := f.seedQueued("orch_queued000004", project.Key,
			"task orch_queued000004 could not create the worktree: the checkout was refused")
		if _, err := f.stateDB(t).ExecContext(ctx, `CREATE TRIGGER refuse_attention_clear
			BEFORE UPDATE ON tasks
			WHEN json_extract(OLD.data, '$.Attention') != '' AND json_extract(NEW.data, '$.Attention') = ''
			BEGIN SELECT RAISE(ABORT, 'attention clear is not writable'); END`); err != nil {
			t.Fatalf("install the attention trigger: %v", err)
		}
		if _, err := f.coord.DrainQueued(ctx, project.Key); err == nil {
			t.Error("DrainQueued swallowed a failed attention write")
		}
		if still := f.reload(queued.ID); still.Attention == "" {
			t.Errorf("the retryable attention vanished although its write failed: %+v", still)
		}
	})
}
