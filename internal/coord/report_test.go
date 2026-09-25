package coord

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		Checks: []string{"go test ./internal/coord ok", "gofmt -l internal/coord (no output)"},
		Risk:   "none: the new path is only reachable from the worker role",
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
		{"blank checks", func(r *task.Report) { r.Checks = []string{" "} }},
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
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "accepted"); !errors.Is(err, ErrNotReviewed) {
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

	// An empty disposition is not a decision.
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "   "); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Review(empty disposition) err = %v, want ErrInvalidRequest", err)
	}
	if after := f.reload(worker.ID); after.Disposition != "" {
		t.Fatalf("an empty disposition was recorded: %q", after.Disposition)
	}

	disposition := "accepted: the user kept the branch and merged it by hand"
	result, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, disposition)
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
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, foreign.ID, "accepted"); !errors.Is(err, ErrOwnership) {
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
	if _, err := f.coord.Review(ctx, project.LaunchPaneID, worker.ID, "accepted"); err != nil {
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
			{Path: checkout, Branch: new(branch), OpenWorkspaceID: new("w9")},
		}}
		workspace, err := provenanceWorkspace(worker, listed)
		if err != nil {
			t.Fatalf("provenanceWorkspace: %v", err)
		}
		if workspace != "w9" {
			t.Errorf("workspace = %q, want the recorded destination w9", workspace)
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

	t.Run("the recorded branch must match", func(t *testing.T) {
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: checkout, Branch: new("orch/bbbbbbbbbbbb"), OpenWorkspaceID: new("w9")},
		}}
		if _, err := provenanceWorkspace(worker, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})

	t.Run("an unknown workspace is refused", func(t *testing.T) {
		// A workspace this task never recorded is not its checkout: a human
		// decides what happened to it.
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: checkout, Branch: new(branch), OpenWorkspaceID: new("w4")},
		}}
		if _, err := provenanceWorkspace(worker, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})

	t.Run("a checkout no workspace holds falls back to the recorded one", func(t *testing.T) {
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{
			{Path: checkout, Branch: new(branch)},
		}}
		workspace, err := provenanceWorkspace(worker, listed)
		if err != nil {
			t.Fatalf("provenanceWorkspace: %v", err)
		}
		if workspace != "w7" {
			t.Errorf("workspace = %q, want the recorded worktree workspace w7", workspace)
		}
	})

	t.Run("a task that records no checkout is refused", func(t *testing.T) {
		listed := &herdr.WorktreeList{Worktrees: []herdr.WorktreeInfo{{Path: checkout, Branch: new(branch)}}}
		if _, err := provenanceWorkspace(task.Task{Branch: branch}, listed); !errors.Is(err, ErrProvenance) {
			t.Fatalf("provenanceWorkspace err = %v, want ErrProvenance", err)
		}
	})
}
