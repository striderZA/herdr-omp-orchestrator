package coord

// Task 5: what a worker reports, what the primary decides, and the one
// destructive action this plugin has.
//
// # Direction of authority
//
// A worker reports facts. It never completes its own task: a final report only
// makes a task *ready for review*, and only a disposition the primary recorded
// from the user moves it to done. Herdr's own `done`/`idle` activity is
// terminal state, never task state, so nothing here reads it as completion.
//
// # Sending into a pane
//
// Everything that writes to a terminal (an ordinary answer, an approved dialog)
// re-derives the target from the store and then proves it against Herdr on the
// spot: the recorded *current public* pane must still resolve to itself, must
// still run the required agent kind, must report the recorded session reference
// in the form Herdr reports it, and must still work in the recorded checkout.
// A moved worker is found by that exact provenance rather than by a label, and
// anything that does not match is refused instead of sent.
//
// # Durable, deduplicated, bounded
//
// Questions, progress and results are durable notes in the store, deduplicated
// by content so a retry writes nothing new. Every human-supplied field is
// bounded before it is recorded, and no request body is authority: the project,
// the task and the worker's pane all come from persisted facts the caller was
// already authorized against.
//
// # Cleanup
//
// Cleanup is explicit, confirmed by the user in the primary's UI, and refuses
// unless the task is reviewed done, its worker is provably gone, and its
// checkout is clean. It removes the checkout, never `--force`, and never the
// branch.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// Stable classifications for the refusals this file adds, so the bridge and the
// extension answer with a specific code instead of a generic failure. Every one
// of them is a refusal that wrote nothing.
var (
	// ErrReportInvalid refuses a report that is not a complete final result.
	ErrReportInvalid = errors.New("coord: the worker report is not a complete result")
	// ErrNotReviewed refuses an action that needs a reviewed done task: a final
	// result the primary can decide on, and the disposition that decided it.
	ErrNotReviewed = errors.New("coord: the task has no reviewed result to decide on")
	// ErrWorkerLive refuses cleanup while the task's worker agent is running.
	ErrWorkerLive = errors.New("coord: the worker agent is still live")
	// ErrCheckoutDirty refuses cleanup of a checkout that still holds work.
	ErrCheckoutDirty = errors.New("coord: the worker checkout is not clean")
	// ErrProvenance refuses an action whose recorded checkout provenance cannot
	// be confirmed against the repository's own worktree listing.
	ErrProvenance = errors.New("coord: the checkout provenance could not be confirmed")
	// ErrDialogChanged refuses an approval whose dialog is no longer the one the
	// user saw, or can no longer be read at all.
	ErrDialogChanged = errors.New("coord: the worker dialog is not the one that was approved")
	// ErrNotBlocked refuses a dialog approval for a worker that is not showing a
	// dialog.
	ErrNotBlocked = errors.New("coord: the worker is not blocked on a dialog")
	// ErrOwnership refuses an operation whose caller does not own the named task:
	// the primary must be the bound primary of that task's project.
	ErrOwnership = errors.New("coord: the caller does not own this task")
)

// Bounds on what one caller may record. They mirror the extension's own tool
// schemas, so a request that never went through the OMP tool is refused the same
// way instead of being trusted.
const (
	progressSummaryLimit = 2000
	questionTextLimit    = 4000
	reportSummaryLimit   = 4000
	reportTextLimit      = 512
	reportEvidenceLimit  = 64
	reportChecksLimit    = 32
	reportRiskLimit      = 4000
	answerTextLimit      = 8000
	dispositionLimit     = 200
	approvalKeyLimit     = 16
	approvalKeyLength    = 64
	dialogPreviewLimit   = 8000
	inboxSummaryLimit    = 4000
	porcelainLimit       = 2000
)

// questionAttentionPrefix marks the attention one worker question records, and
// is what makes it distinguishable from a failure the dispatch step machine
// recorded: only this marker is cleared once the worker has its answer.
const questionAttentionPrefix = "worker question: "

// ---------------------------------------------------------------------------
// wire bodies
// ---------------------------------------------------------------------------

// ProgressParams is the orch_progress body.
type ProgressParams struct {
	Summary string `json:"summary"`
}

// QuestionParams is the orch_question body.
type QuestionParams struct {
	Text string `json:"text"`
}

// ReportParams is the orch_report body. It is its own type rather than
// task.Report because the wire spelling is lowercase and the extension, not the
// store, owns that shape.
type ReportParams struct {
	Kind     string   `json:"kind"`
	Summary  string   `json:"summary"`
	Branch   string   `json:"branch"`
	Evidence []string `json:"evidence"`
	Checks   []string `json:"checks"`
	Risk     string   `json:"risk"`
}

// Report projects the wire body onto the durable report.
func (p ReportParams) Report() task.Report {
	return task.Report{
		Kind:     p.Kind,
		Summary:  p.Summary,
		Branch:   p.Branch,
		Evidence: p.Evidence,
		Checks:   p.Checks,
		Risk:     p.Risk,
	}
}

// AnswerParams is the orch_answer body: the worker task a primary answers, and
// the answer to relay to it.
type AnswerParams struct {
	TaskID string `json:"taskId"`
	Text   string `json:"text"`
}

// ApproveDialogParams is the orch_approve_dialog body: the digest of the dialog
// the user approved and the exact keys that approval covers.
type ApproveDialogParams struct {
	TaskID         string   `json:"taskId"`
	ObservedDigest string   `json:"observedDigest"`
	Keys           []string `json:"keys"`
}

// ReviewParams is the orch_review body: the disposition the user recorded for a
// reviewed result.
type ReviewParams struct {
	TaskID      string `json:"taskId"`
	Disposition string `json:"disposition"`
}

// CleanupParams is the orch_cleanup body.
type CleanupParams struct {
	TaskID string `json:"taskId"`
}

// ---------------------------------------------------------------------------
// results
// ---------------------------------------------------------------------------

// ProgressResult is the answer to one progress note. Duplicate reports that the
// same note was already durable, so nothing was written.
type ProgressResult struct {
	TaskID    string     `json:"taskId"`
	Stage     task.Stage `json:"stage"`
	Duplicate bool       `json:"duplicate"`
}

// QuestionResult is the answer to one worker question. The task is Needs input
// until the primary relays an answer.
type QuestionResult struct {
	TaskID    string     `json:"taskId"`
	Stage     task.Stage `json:"stage"`
	Duplicate bool       `json:"duplicate"`
}

// FinalReportResult is the answer to one final report. The task is Read for
// review; it is not done.
type FinalReportResult struct {
	TaskID    string      `json:"taskId"`
	Stage     task.Stage  `json:"stage"`
	Report    task.Report `json:"report"`
	Duplicate bool        `json:"duplicate"`
}

// AnswerResult is the answer to one relayed answer: the pane it was sent to.
type AnswerResult struct {
	TaskID       string     `json:"taskId"`
	PublicPaneID string     `json:"publicPaneId"`
	Stage        task.Stage `json:"stage"`
	Delivered    bool       `json:"delivered"`
}

// DialogPreview is the read-only description of the dialog a blocked worker is
// showing. Digest identifies exactly this dialog, and is what an approval has to
// quote back.
type DialogPreview struct {
	TaskID       string `json:"taskId"`
	PublicPaneID string `json:"publicPaneId"`
	AgentName    string `json:"agentName"`
	AgentStatus  string `json:"agentStatus"`
	Blocked      bool   `json:"blocked"`
	Dialog       string `json:"dialog"`
	Digest       string `json:"digest"`
}

// ApproveDialogResult is the answer to one approved dialog: the keys that were
// sent, to which pane, for which dialog.
type ApproveDialogResult struct {
	TaskID       string     `json:"taskId"`
	PublicPaneID string     `json:"publicPaneId"`
	Digest       string     `json:"digest"`
	Keys         []string   `json:"keys"`
	Stage        task.Stage `json:"stage"`
}

// ReviewResult is the answer to one recorded disposition.
type ReviewResult struct {
	TaskID      string     `json:"taskId"`
	Disposition string     `json:"disposition"`
	Stage       task.Stage `json:"stage"`
}

// CleanupPreview is the read-only description of what cleanup would remove and
// whether it may: the primary shows it to the user before anything is approved.
type CleanupPreview struct {
	TaskID      string `json:"taskId"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	WorkspaceID string `json:"workspaceId"`
	Reviewed    bool   `json:"reviewed"`
	Clean       bool   `json:"clean"`
	LiveWorker  bool   `json:"liveWorker"`
	Removable   bool   `json:"removable"`
	// Detail explains the first reason this checkout may not be removed, in the
	// words the primary shows the user.
	Detail string `json:"detail"`
}

// CleanupResult is the answer to one removed checkout. Branch is reported
// because cleanup preserves it.
type CleanupResult struct {
	TaskID      string     `json:"taskId"`
	Branch      string     `json:"branch"`
	Path        string     `json:"path"`
	WorkspaceID string     `json:"workspaceId"`
	Removed     bool       `json:"removed"`
	Stage       task.Stage `json:"stage"`
}

// ---------------------------------------------------------------------------
// worker reports
// ---------------------------------------------------------------------------

// Progress records one durable, deduplicated progress note for a worker task.
//
// Progress is the partial channel: it records no task fact and can never produce
// a report, so a worker that is still working cannot read as finished.
func (c *Coordinator) Progress(ctx context.Context, launchPaneID, summary string) (ProgressResult, error) {
	st, err := c.workerTask(ctx, launchPaneID)
	if err != nil {
		return ProgressResult{}, err
	}
	text, err := boundedText(summary, progressSummaryLimit, "the progress summary")
	if err != nil {
		return ProgressResult{}, err
	}
	inserted, err := c.note(ctx, st, "progress", text, "worker progress")
	if err != nil {
		return ProgressResult{}, err
	}
	return ProgressResult{TaskID: st.ID, Stage: task.StageOf(st), Duplicate: !inserted}, nil
}

// Question records one durable question and puts the task in Needs input so the
// primary relays it to the user. It writes no task resource and never steals
// focus: a question is a fact about the task, not a terminal operation.
func (c *Coordinator) Question(ctx context.Context, launchPaneID, text string) (QuestionResult, error) {
	st, err := c.workerTask(ctx, launchPaneID)
	if err != nil {
		return QuestionResult{}, err
	}
	question, err := boundedText(text, questionTextLimit, "the question")
	if err != nil {
		return QuestionResult{}, err
	}

	asked := st
	if attention := questionAttention(question); st.Attention != attention {
		updated, err := c.record(ctx, st, func(next *task.Task) error {
			next.Attention = attention
			return nil
		}, "record the worker question")
		if err != nil {
			return QuestionResult{}, err
		}
		asked = updated
	}
	inserted, err := c.note(ctx, asked, "question", question, "worker question")
	if err != nil {
		return QuestionResult{}, err
	}
	return QuestionResult{TaskID: asked.ID, Stage: task.StageOf(asked), Duplicate: !inserted}, nil
}

// FinalReport records a worker's complete result and moves the task to Ready for
// review. It never completes the task: only the primary's recorded disposition
// does.
//
// The write is one compare-and-swap task update followed by one deduplicated
// inbox note, so a retry after an ambiguous answer either re-records the same
// report (writing nothing) or leaves the durability of the report to the task
// row itself.
func (c *Coordinator) FinalReport(ctx context.Context, launchPaneID string, report task.Report) (FinalReportResult, error) {
	st, err := c.workerTask(ctx, launchPaneID)
	if err != nil {
		return FinalReportResult{}, err
	}
	complete, err := validateReport(report, st)
	if err != nil {
		return FinalReportResult{}, err
	}

	recorded := st
	if st.Report == nil || !sameReport(*st.Report, complete) || isQuestionAttention(st.Attention) {
		updated, err := c.record(ctx, st, func(next *task.Task) error {
			// Re-checked on every attempt: a report only ever describes the
			// branch this task owns, whatever landed in between.
			if !sameBranch(next.Branch, complete.Branch) {
				return fmt.Errorf("%w: the task now owns branch %q, not the reported %q", ErrReportInvalid, next.Branch, complete.Branch)
			}
			if isQuestionAttention(next.Attention) {
				next.Attention = ""
			}
			next.Report = &complete
			return nil
		}, "record the final report")
		if err != nil {
			return FinalReportResult{}, err
		}
		recorded = updated
	}

	inserted, err := c.note(ctx, recorded, "result", reportNote(complete), "worker result")
	if err != nil {
		return FinalReportResult{}, err
	}
	return FinalReportResult{TaskID: recorded.ID, Stage: task.StageOf(recorded), Report: complete, Duplicate: !inserted}, nil
}

// validateReport refuses every report that is not a complete final result.
//
// Only Kind "result" is final — a partial report is what orch_progress is for —
// and a result has to carry what a reviewer needs: a summary, the branch this
// task actually owns, changed-file or commit evidence, the checks that ran, and a
// stated risk (including a stated absence of one).
func validateReport(report task.Report, st task.Task) (task.Report, error) {
	invalid := func(format string, args ...any) (task.Report, error) {
		return task.Report{}, fmt.Errorf("%w: %s", ErrReportInvalid, fmt.Sprintf(format, args...))
	}
	if kind := strings.TrimSpace(report.Kind); kind != "result" {
		return invalid("kind %q is not a final result; report partial progress with orch_progress instead", report.Kind)
	}
	summary, err := textField(report.Summary, reportSummaryLimit, "the report summary")
	if err != nil {
		return invalid("%v", err)
	}
	branch := strings.TrimSpace(report.Branch)
	if branch == "" {
		return invalid("the report names no branch")
	}
	if !sameBranch(branch, st.Branch) {
		return invalid("the report names branch %q, but task %s owns %q", branch, st.ID, st.Branch)
	}
	evidence, err := boundedList(report.Evidence, reportEvidenceLimit, reportTextLimit, "the changed-file/commit evidence")
	if err != nil {
		return invalid("%v", err)
	}
	checks, err := boundedList(report.Checks, reportChecksLimit, reportTextLimit, "the checks and their outcomes")
	if err != nil {
		return invalid("%v", err)
	}
	risk, err := textField(report.Risk, reportRiskLimit, "the stated risk")
	if err != nil {
		return invalid("%v", err)
	}
	return task.Report{
		Kind:     "result",
		Summary:  summary,
		Branch:   branch,
		Evidence: evidence,
		Checks:   checks,
		Risk:     risk,
	}, nil
}

// reportNote is the durable inbox line a reviewed result leaves for the primary:
// what was reported and the evidence and checks it stands on.
func reportNote(report task.Report) string {
	return truncateRunes(strings.Join(nonEmpty(
		report.Summary,
		"evidence: "+strings.Join(report.Evidence, "; "),
		"checks: "+strings.Join(report.Checks, "; "),
		"risk: "+report.Risk,
	), " | "), inboxSummaryLimit)
}

// questionAttention is the marker a pending worker question leaves on the task.
func questionAttention(question string) string {
	return questionAttentionPrefix + question
}

// isQuestionAttention reports whether an attention was recorded by a worker
// question, which is the only attention a report or an answer may clear.
func isQuestionAttention(attention string) bool {
	return strings.HasPrefix(strings.TrimSpace(attention), questionAttentionPrefix)
}

// ---------------------------------------------------------------------------
// primary decisions
// ---------------------------------------------------------------------------

// Answer relays an ordinary answer from the primary to the worker that asked.
//
// The target is the recorded *current public* pane, re-proven against Herdr as
// this task's live worker; a pane whose occupant or session changed is refused,
// so an answer can never be typed into another process. A blocked worker is
// refused here too: what it needs is its dialog's exact keys, not a prompt.
func (c *Coordinator) Answer(ctx context.Context, primaryLaunchPaneID, taskID, text string) (AnswerResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return AnswerResult{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return AnswerResult{}, err
	}
	answer, err := boundedText(text, answerTextLimit, "the answer")
	if err != nil {
		return AnswerResult{}, err
	}

	view, pane, err := c.workerOccupant(ctx, st)
	if err != nil {
		return AnswerResult{}, err
	}
	if isBlocked(view) {
		return AnswerResult{}, fmt.Errorf("%w: the worker on pane %q is blocked on a dialog, so an ordinary answer cannot reach it; approve its exact keys with orch_approve_dialog", ErrBlocked, pane)
	}
	callCtx, cancel := c.callContext(ctx)
	_, promptErr := c.herdr.AgentPrompt(callCtx, pane, answer)
	cancel()
	if promptErr != nil {
		return AnswerResult{}, fmt.Errorf("answer the worker on pane %q: %w", pane, promptErr)
	}

	answered := st
	if isQuestionAttention(st.Attention) {
		updated, err := c.record(ctx, st, func(next *task.Task) error {
			if isQuestionAttention(next.Attention) {
				next.Attention = ""
			}
			return nil
		}, "clear the answered question")
		if err != nil {
			return AnswerResult{}, fmt.Errorf("the answer was delivered to pane %q, but the pending question could not be cleared: %w", pane, err)
		}
		answered = updated
	}
	return AnswerResult{TaskID: answered.ID, PublicPaneID: pane, Stage: task.StageOf(answered), Delivered: true}, nil
}

// DialogPreview reads what a blocked worker is showing, without sending
// anything.
//
// The preview is passive and read-only: the primary user sees exactly the
// on-screen dialog and its digest, and an approval quotes that digest back.
func (c *Coordinator) DialogPreview(ctx context.Context, primaryLaunchPaneID, taskID string) (DialogPreview, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return DialogPreview{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return DialogPreview{}, err
	}
	view, pane, err := c.workerOccupant(ctx, st)
	if err != nil {
		return DialogPreview{}, err
	}
	dialog, err := c.visibleDialog(ctx, pane)
	if err != nil {
		return DialogPreview{}, err
	}
	return DialogPreview{
		TaskID:       st.ID,
		PublicPaneID: pane,
		AgentName:    strings.TrimSpace(view.Agent.Name),
		AgentStatus:  strings.TrimSpace(view.Agent.AgentStatus),
		Blocked:      isBlocked(view),
		Dialog:       dialog,
		Digest:       digestDialog(dialog),
	}, nil
}

// ApproveDialog sends exactly the keys the user approved for exactly the dialog
// the user saw.
//
// It re-reads the visible screen and refuses unless the pane, the session and
// the dialogue digest are all still the ones that were verified and approved, so
// a changed, replaced or unreadable dialog is never answered. Nothing is ever
// approved in bulk: the keys are the approval.
func (c *Coordinator) ApproveDialog(ctx context.Context, primaryLaunchPaneID, taskID, observedDigest string, keys []string) (ApproveDialogResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return ApproveDialogResult{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return ApproveDialogResult{}, err
	}
	digest := strings.TrimSpace(observedDigest)
	if digest == "" {
		return ApproveDialogResult{}, fmt.Errorf("%w: no approved dialog digest was supplied; preview the dialog first", ErrInvalidRequest)
	}
	approved, err := approvedKeys(keys)
	if err != nil {
		return ApproveDialogResult{}, err
	}

	view, pane, err := c.workerOccupant(ctx, st)
	if err != nil {
		return ApproveDialogResult{}, err
	}
	if !isBlocked(view) {
		return ApproveDialogResult{}, fmt.Errorf("%w: the worker on pane %q is %s, not blocked on a dialog", ErrNotBlocked, pane, agentStatus(view))
	}
	dialog, err := c.visibleDialog(ctx, pane)
	if err != nil {
		return ApproveDialogResult{}, err
	}
	if got := digestDialog(dialog); got != digest {
		return ApproveDialogResult{}, fmt.Errorf("%w: pane %q now shows a dialog hashing to %s, not the approved %s; nothing was sent", ErrDialogChanged, pane, got, digest)
	}

	callCtx, cancel := c.callContext(ctx)
	sendErr := c.herdr.AgentSendKeys(callCtx, pane, approved...)
	cancel()
	if sendErr != nil {
		return ApproveDialogResult{}, fmt.Errorf("send the approved keys to pane %q: %w", pane, sendErr)
	}

	answered := st
	if isQuestionAttention(st.Attention) {
		updated, err := c.record(ctx, st, func(next *task.Task) error {
			if isQuestionAttention(next.Attention) {
				next.Attention = ""
			}
			return nil
		}, "clear the approved question")
		if err != nil {
			return ApproveDialogResult{}, fmt.Errorf("the approved keys were sent to pane %q, but the pending question could not be cleared: %w", pane, err)
		}
		answered = updated
	}
	return ApproveDialogResult{TaskID: answered.ID, PublicPaneID: pane, Digest: digest, Keys: approved, Stage: task.StageOf(answered)}, nil
}

// Review records the user's disposition for a worker's reviewed result. This is
// the only thing that moves a task to Done.
func (c *Coordinator) Review(ctx context.Context, primaryLaunchPaneID, taskID, disposition string) (ReviewResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return ReviewResult{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return ReviewResult{}, err
	}
	decision, err := boundedText(disposition, dispositionLimit, "the disposition")
	if err != nil {
		return ReviewResult{}, err
	}
	if !isReviewed(st) {
		return ReviewResult{}, fmt.Errorf("%w: task %s has no final result to decide on, so it cannot be closed", ErrNotReviewed, st.ID)
	}
	updated, err := c.record(ctx, st, func(next *task.Task) error {
		if !isReviewed(*next) {
			return fmt.Errorf("%w: the task's final result was withdrawn while the decision was being recorded", ErrNotReviewed)
		}
		next.Disposition = decision
		return nil
	}, "record the user's disposition")
	if err != nil {
		return ReviewResult{}, err
	}
	return ReviewResult{TaskID: updated.ID, Disposition: updated.Disposition, Stage: task.StageOf(updated)}, nil
}

// isReviewed reports whether a task carries a final result a primary can decide
// on. It is deliberately exact: everything except a result needs more work
// before a disposition means anything.
func isReviewed(st task.Task) bool {
	return st.Report != nil && st.Report.Kind == "result"
}

// isClosed reports whether a task is both reviewed and decided, which is the
// only state cleanup may touch.
func isClosed(st task.Task) bool {
	return isReviewed(st) && strings.TrimSpace(st.Disposition) != ""
}

// ---------------------------------------------------------------------------
// cleanup
// ---------------------------------------------------------------------------

// CleanupPreview reports what cleanup would remove and whether it may, without
// changing anything. It is what the primary shows the user before asking for the
// confirmation that cleanup then revalidates.
func (c *Coordinator) CleanupPreview(ctx context.Context, primaryLaunchPaneID, taskID string) (CleanupPreview, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return CleanupPreview{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return CleanupPreview{}, err
	}

	preview := CleanupPreview{
		TaskID:   st.ID,
		Branch:   strings.TrimSpace(st.Branch),
		Path:     strings.TrimSpace(st.WorktreePath),
		Reviewed: isClosed(st),
	}
	blockers := []string{}
	if !preview.Reviewed {
		blockers = append(blockers, "the task is not a reviewed done task: only a final result with a disposition the user recorded can be cleaned")
	}
	if preview.Branch == "" || preview.Path == "" {
		blockers = append(blockers, "the task records no checkout to remove")
	}

	if preview.Path != "" {
		status, err := c.checkoutStatus(ctx, preview.Path)
		switch {
		case err != nil:
			blockers = append(blockers, err.Error())
		case strings.TrimSpace(status) != "":
			blockers = append(blockers, "the checkout has uncommitted or untracked changes: "+firstLine(status))
		default:
			preview.Clean = true
		}
	}

	workspace, err := c.checkoutWorkspace(ctx, st)
	if err != nil {
		blockers = append(blockers, err.Error())
	} else {
		preview.WorkspaceID = workspace
	}

	live, err := c.workerLive(ctx, st)
	preview.LiveWorker = live || err != nil
	switch {
	case err != nil:
		blockers = append(blockers, err.Error())
	case live:
		blockers = append(blockers, "the worker agent is still running in its pane; close it before removing its checkout")
	}

	preview.Removable = preview.Reviewed && preview.Clean && !preview.LiveWorker && preview.WorkspaceID != ""
	preview.Detail = strings.Join(blockers, "; ")
	if preview.Removable {
		preview.Detail = fmt.Sprintf("removes %s (branch %s, workspace %s); the branch is kept and the removal is never forced", preview.Path, preview.Branch, preview.WorkspaceID)
	}
	return preview, nil
}

// Cleanup removes a reviewed worker's checkout, and nothing else.
//
// It revalidates the whole decision on the spot — reviewed and disposed, no live
// worker, a clean checkout, and a worktree listing that still names the recorded
// path and branch — then removes the checkout without `--force`. The branch
// survives, the source workspace id alone is never trusted (a move can close it),
// and any refusal or failure leaves the checkout exactly where it was.
func (c *Coordinator) Cleanup(ctx context.Context, primaryLaunchPaneID, taskID string) (CleanupResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return CleanupResult{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return CleanupResult{}, err
	}
	if !isClosed(st) {
		return CleanupResult{}, fmt.Errorf("%w: task %s needs a final result and a disposition the user recorded before its checkout may be removed", ErrNotReviewed, st.ID)
	}

	status, err := c.checkoutStatus(ctx, st.WorktreePath)
	if err != nil {
		return CleanupResult{}, err
	}
	if strings.TrimSpace(status) != "" {
		return CleanupResult{}, fmt.Errorf("%w: %s still holds work (%s); commit, move or remove it before cleaning up",
			ErrCheckoutDirty, st.WorktreePath, firstLine(status))
	}

	live, err := c.workerLive(ctx, st)
	if err != nil {
		return CleanupResult{}, err
	}
	if live {
		return CleanupResult{}, fmt.Errorf("%w: the worker agent recorded for task %s is still running; close it before removing its checkout", ErrWorkerLive, st.ID)
	}

	workspace, err := c.checkoutWorkspace(ctx, st)
	if err != nil {
		return CleanupResult{}, err
	}
	removed, err := c.removeCheckout(ctx, workspace, st)
	if err != nil {
		return CleanupResult{}, err
	}

	// The checkout is gone, so the task's live identities are cleared and its
	// branch and path stay as the provenance of what was removed.
	updated, err := c.record(ctx, st, func(next *task.Task) error {
		next.LaunchPaneID = ""
		next.PublicPaneID = ""
		next.TerminalID = ""
		next.WorktreeWorkspaceID = ""
		next.CurrentWorkspaceID = ""
		return nil
	}, "record the removed checkout")
	if err != nil {
		return CleanupResult{}, fmt.Errorf("the checkout of task %s was removed, but the task could not be updated: %w", st.ID, err)
	}
	if _, err := c.note(ctx, updated, "cleanup",
		fmt.Sprintf("checkout %s of task %s removed; branch %s kept", removed.Path, st.ID, st.Branch),
		"worktree removed"); err != nil {
		return CleanupResult{}, err
	}
	return CleanupResult{
		TaskID:      updated.ID,
		Branch:      st.Branch,
		Path:        removed.Path,
		WorkspaceID: removed.WorkspaceID,
		Removed:     true,
		Stage:       task.StageOf(updated),
	}, nil
}

// checkoutWorkspace confirms the recorded checkout against the repository's own
// worktree listing and returns the workspace whose removal Herdr would honor.
func (c *Coordinator) checkoutWorkspace(ctx context.Context, st task.Task) (string, error) {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	listed, err := c.herdr.WorktreeList(callCtx, st.ProjectKey)
	if err != nil {
		return "", fmt.Errorf("%w: the worktrees of %s could not be read: %w", ErrProvenance, st.ProjectKey, err)
	}
	return provenanceWorkspace(st, listed)
}

// provenanceWorkspace picks the workspace that holds the checkout this task
// recorded.
//
// The recorded path and branch are the identity; the workspace id is only a
// pointer to it. A pane that was moved into another workspace can close the
// source workspace, so the workspace Herdr lists as the holder is accepted when
// it is one this task recorded, and the recorded worktree workspace is the
// fallback when Herdr reports none. A checkout held by a workspace this task
// never recorded is somebody else's, and is refused.
func provenanceWorkspace(st task.Task, listed *herdr.WorktreeList) (string, error) {
	checkout := strings.TrimSpace(st.WorktreePath)
	if checkout == "" || strings.TrimSpace(st.Branch) == "" {
		return "", fmt.Errorf("%w: task %s records no checkout to remove", ErrProvenance, st.ID)
	}
	if listed == nil {
		return "", fmt.Errorf("%w: Herdr listed no worktrees for the repository", ErrProvenance)
	}
	accepted := nonEmpty(st.WorktreeWorkspaceID, st.CurrentWorkspaceID)
	for _, entry := range listed.Worktrees {
		if !samePath(entry.Path, checkout) {
			continue
		}
		if entry.Branch != nil && !sameBranch(*entry.Branch, st.Branch) {
			return "", fmt.Errorf("%w: the checkout at %s tracks branch %q, not the recorded %q", ErrProvenance, checkout, *entry.Branch, st.Branch)
		}
		if entry.OpenWorkspaceID != nil && strings.TrimSpace(*entry.OpenWorkspaceID) != "" {
			open := strings.TrimSpace(*entry.OpenWorkspaceID)
			if !containsString(accepted, open) {
				return "", fmt.Errorf("%w: Herdr holds the checkout at %s in workspace %q, which task %s never recorded (%s)",
					ErrProvenance, checkout, open, st.ID, quotedIDs(accepted))
			}
			return open, nil
		}
		if len(accepted) == 0 {
			return "", fmt.Errorf("%w: no workspace is recorded for the checkout at %s, so it cannot be removed safely", ErrProvenance, checkout)
		}
		return accepted[0], nil
	}
	return "", fmt.Errorf("%w: the worktree listing has no checkout at %s, so task %s cannot be removed from it", ErrProvenance, checkout, st.ID)
}

// checkoutStatus reads the checkout's porcelain status, which is empty exactly
// when it holds no work that removing it would lose. Untracked files count:
// they are work too.
func (c *Coordinator) checkoutStatus(ctx context.Context, path string) (string, error) {
	checkout := strings.TrimSpace(path)
	if checkout == "" {
		return "", fmt.Errorf("%w: the task records no checkout path", ErrProvenance)
	}
	status, err := c.git(ctx, checkout, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("%w: the checkout at %s could not be inspected: %w", ErrProvenance, checkout, err)
	}
	if len(status) > porcelainLimit {
		return status[:porcelainLimit], nil
	}
	return status, nil
}

// removeCheckout removes the checkout of the verified workspace. It never forces
// and never touches the branch, and it refuses to act on a workspace whose
// checkout is not the recorded one.
//
// A workspace a move closed is expected state, not a reason to refuse: the
// candidates are the listing's workspace followed by the ones this task
// recorded, and only a definite "that workspace is gone" moves on to the next.
func (c *Coordinator) removeCheckout(ctx context.Context, workspace string, st task.Task) (*herdr.WorktreeRemoved, error) {
	var firstErr error
	for _, candidate := range nonEmpty(workspace, st.WorktreeWorkspaceID, st.CurrentWorkspaceID) {
		callCtx, cancel := c.callContext(ctx)
		removed, err := c.herdr.WorktreeRemove(callCtx, candidate, false)
		cancel()
		switch {
		case err == nil:
			return verifyRemoval(removed, st)
		case isMissingTarget(err):
			if firstErr == nil {
				firstErr = err
			}
		default:
			return nil, fmt.Errorf("remove the checkout of task %s from workspace %q: %w", st.ID, candidate, err)
		}
	}
	return nil, fmt.Errorf("%w: no recorded workspace still holds the checkout of task %s (%v)", ErrProvenance, st.ID, firstErr)
}

// verifyRemoval checks the removal Herdr acknowledged: the checkout it removed
// must be the recorded one, and it must not have been a forced removal.
func verifyRemoval(removed *herdr.WorktreeRemoved, st task.Task) (*herdr.WorktreeRemoved, error) {
	if removed == nil {
		return nil, fmt.Errorf("%w: Herdr reported no removed checkout for task %s", ErrProvenance, st.ID)
	}
	if removed.Forced {
		return nil, fmt.Errorf("%w: Herdr removed the checkout of task %s by force, which this plugin never asks for; check %s", ErrProvenance, st.ID, st.WorktreePath)
	}
	if path := strings.TrimSpace(removed.Path); path != "" && !samePath(path, st.WorktreePath) {
		return nil, fmt.Errorf("%w: Herdr removed the checkout at %s, not the recorded %s", ErrProvenance, path, st.WorktreePath)
	}
	return removed, nil
}

// ---------------------------------------------------------------------------
// ownership
// ---------------------------------------------------------------------------

// primaryProject reads the project a primary launch pane is bound to.
//
// It re-reads the store on every call rather than trusting anything the caller
// sent, and requires the record to still name that pane. Liveness is not
// re-derived here: the bridge authorizes a mutating call only for the OMP session
// it confirmed against `agent get` for this very pane, so the caller is the live
// primary by construction, and a Herdr round trip would only restate it.
func (c *Coordinator) primaryProject(ctx context.Context, launchPaneID string) (task.Project, error) {
	pane := strings.TrimSpace(launchPaneID)
	if pane == "" {
		return task.Project{}, fmt.Errorf("%w: no primary launch pane was given", ErrOwnership)
	}
	project, err := c.store.ProjectByLaunchPane(ctx, pane)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return task.Project{}, fmt.Errorf("%w: launch pane %q is not a bound primary", ErrOwnership, pane)
	case err != nil:
		return task.Project{}, fmt.Errorf("read the primary bound to pane %q: %w", pane, err)
	case strings.TrimSpace(project.LaunchPaneID) != pane:
		return task.Project{}, fmt.Errorf("%w: pane %q is not the recorded launch pane of project %s", ErrOwnership, pane, project.Key)
	}
	return project, nil
}

// workerTask reads the worker task a launch pane is bound to, and requires that
// pane and its project to still be the recorded ones.
func (c *Coordinator) workerTask(ctx context.Context, launchPaneID string) (task.Task, error) {
	pane := strings.TrimSpace(launchPaneID)
	if pane == "" {
		return task.Task{}, fmt.Errorf("%w: no worker launch pane was given", ErrOwnership)
	}
	st, err := c.store.ByLaunchPane(ctx, pane)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return task.Task{}, fmt.Errorf("%w: launch pane %q is not a recorded worker", ErrOwnership, pane)
	case err != nil:
		return task.Task{}, fmt.Errorf("read the worker bound to pane %q: %w", pane, err)
	case strings.TrimSpace(st.LaunchPaneID) != pane:
		return task.Task{}, fmt.Errorf("%w: pane %q is not the recorded launch pane of task %s", ErrOwnership, pane, st.ID)
	case st.Phase != task.AgentStarted && st.Phase != task.PromptSubmitted:
		return task.Task{}, fmt.Errorf("%w: task %s is in phase %q, so no worker has been briefed for it yet", ErrOwnership, st.ID, st.Phase)
	}
	if _, err := c.project(ctx, st.ProjectKey); err != nil {
		return task.Task{}, err
	}
	return st, nil
}

// ownedTask reads one task and requires it to belong to the caller's project.
// The task id comes from the request, and this is the check that makes it
// harmless: a primary can only act on the tasks of its own project.
func (c *Coordinator) ownedTask(ctx context.Context, project task.Project, taskID string) (task.Task, error) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return task.Task{}, fmt.Errorf("%w: no task id was given", ErrInvalidRequest)
	}
	st, err := c.store.Task(ctx, id)
	switch {
	case errors.Is(err, state.ErrNotFound):
		return task.Task{}, fmt.Errorf("%w: task %s does not exist", ErrInvalidRequest, id)
	case err != nil:
		return task.Task{}, fmt.Errorf("read task %s: %w", id, err)
	}
	if !samePath(st.ProjectKey, project.Key) {
		return task.Task{}, fmt.Errorf("%w: task %s belongs to %s, not to this primary's project %s", ErrOwnership, id, st.ProjectKey, project.Key)
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// the worker's live pane
// ---------------------------------------------------------------------------

// workerOccupant re-verifies that a task's recorded worker is still the occupant
// of its recorded current public pane, and returns that pane.
//
// Exact identity, never a label: the pane must still resolve to the recorded
// public id, must still run the required agent kind, must report the recorded
// session reference in the form Herdr reports it, and must still work in the
// recorded checkout. Anything else is ErrAgentChanged, which a human resolves;
// nothing is ever sent to a pane that may hold a different process.
func (c *Coordinator) workerOccupant(ctx context.Context, st task.Task) (agentView, string, error) {
	pane := strings.TrimSpace(st.PublicPaneID)
	if pane == "" {
		return agentView{}, "", fmt.Errorf("%w: task %s records no current public pane", ErrProvenance, st.ID)
	}
	if strings.TrimSpace(st.OMPSessionID) == "" {
		return agentView{}, "", fmt.Errorf("%w: task %s records no OMP session, so its worker cannot be identified", ErrAgentChanged, st.ID)
	}
	view, err := c.agentGet(ctx, pane)
	switch {
	case isMissingTarget(err):
		return agentView{}, "", fmt.Errorf("%w: the worker's public pane %q no longer exists", ErrAgentChanged, pane)
	case err != nil:
		return agentView{}, "", fmt.Errorf("read the worker's public pane %q: %w", pane, err)
	}
	if view.Agent.PaneID != pane {
		return agentView{}, "", fmt.Errorf("%w: pane %q now resolves to %q, so the recorded public pane is stale", ErrAgentChanged, pane, view.Agent.PaneID)
	}
	if !strings.EqualFold(strings.TrimSpace(view.Agent.Agent), c.opts.AgentKind) {
		return agentView{}, "", fmt.Errorf("%w: pane %q now runs %q, not %q", ErrAgentChanged, pane, view.Agent.Agent, c.opts.AgentKind)
	}
	switch {
	case view.RefErr != nil:
		return agentView{}, "", fmt.Errorf("%w: %w", ErrAgentChanged, view.RefErr)
	case !view.HasRef:
		return agentView{}, "", fmt.Errorf("%w: Herdr reports no session reference for pane %q, so the recorded worker cannot be verified", ErrAgentChanged, pane)
	case !sessionMatchesRef(st.OMPSessionID, view.Ref):
		return agentView{}, "", fmt.Errorf("%w: pane %q reports %s session %q, not the recorded worker session %q",
			ErrAgentChanged, pane, view.Ref.Kind, view.Ref.Value, st.OMPSessionID)
	}
	if cwd := strings.TrimSpace(view.Agent.Cwd); cwd != "" && strings.TrimSpace(st.WorktreePath) != "" && !samePath(cwd, st.WorktreePath) {
		return agentView{}, "", fmt.Errorf("%w: pane %q works in %s, not the task's checkout %s", ErrAgentChanged, pane, cwd, st.WorktreePath)
	}
	return view, pane, nil
}

// workerLive reports whether the task's recorded worker agent is still running in
// one of its recorded panes, and refuses when that cannot be proven either way.
//
// A pane that is gone or holds no agent is not a live worker. An agent in the
// worker's own pane that Herdr has not published a session reference for is
// treated as live: cleanup may not proceed on a maybe. An agent with a different
// session in that pane is a changed occupant, which a human decides.
func (c *Coordinator) workerLive(ctx context.Context, st task.Task) (bool, error) {
	for _, pane := range nonEmpty(st.PublicPaneID, st.LaunchPaneID) {
		view, err := c.agentGet(ctx, pane)
		switch {
		case isMissingTarget(err):
			continue
		case err != nil:
			return false, fmt.Errorf("read pane %q before removing its checkout: %w", pane, err)
		}
		if !strings.EqualFold(strings.TrimSpace(view.Agent.Agent), c.opts.AgentKind) {
			// A plain terminal is not the worker.
			continue
		}
		switch {
		case !view.HasRef:
			return true, nil
		case sessionMatchesRef(st.OMPSessionID, view.Ref):
			return true, nil
		default:
			return false, fmt.Errorf("%w: pane %q now runs %s session %q, not the recorded worker %s",
				ErrAgentChanged, pane, view.Ref.Kind, view.Ref.Value, st.OMPSessionID)
		}
	}
	return false, nil
}

// sessionMatchesRef compares the session a task recorded against the reference
// Herdr reports for its pane, in the form that reference uses.
//
// The recorded identity may be either native form — an id where the build
// exposes one, a session file where Herdr reported a path — and Herdr may report
// either, so the comparison is by kind rather than by a value parsed out of
// something else.
func sessionMatchesRef(recorded string, ref task.SessionRef) bool {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return false
	}
	announced, ok := task.AnnouncementOfRef(ref)
	if !ok {
		return false
	}
	if announced.ID != "" {
		return task.SameSessionReference(task.SessionKindID, recorded, announced.ID)
	}
	return task.SameSessionReference(task.SessionKindPath, recorded, announced.File)
}

// isBlocked reports whether a live agent is showing a full-screen dialog and
// waiting for keys.
func isBlocked(view agentView) bool {
	return strings.EqualFold(strings.TrimSpace(view.Agent.AgentStatus), "blocked")
}

// agentStatus names the status Herdr reports, for a message a human reads.
func agentStatus(view agentView) string {
	if status := strings.TrimSpace(view.Agent.AgentStatus); status != "" {
		return "in status " + status
	}
	return "not blocked"
}

// visibleDialog reads what the worker's terminal shows right now.
//
// The passive source is deliberate: `recent-unwrapped --lines N` can answer
// `agent_not_idle` while a full-screen OMP dialog is up, so the digest a user
// approved would never be reproducible on the way back in. `--source visible`
// reads the on-screen dialog whether or not the agent is blocked.
func (c *Coordinator) visibleDialog(ctx context.Context, paneID string) (string, error) {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	dialog, err := c.herdr.AgentRead(callCtx, paneID, herdr.AgentReadOptions{Source: "visible"})
	if err != nil {
		return "", fmt.Errorf("%w: the visible dialog on pane %q could not be read: %w", ErrDialogChanged, paneID, err)
	}
	if strings.TrimSpace(dialog) == "" {
		return "", fmt.Errorf("%w: pane %q shows no dialog to approve", ErrDialogChanged, paneID)
	}
	if len(dialog) > dialogPreviewLimit {
		return "", fmt.Errorf("%w: the visible dialog on pane %q is %d bytes, larger than the %d byte bound a user can be shown and approve",
			ErrDialogChanged, paneID, len(dialog), dialogPreviewLimit)
	}
	return dialog, nil
}

// digestDialog is the stable identity of one rendered dialog.
//
// Terminal reads carry rendering noise that says nothing about the choice on
// screen: CRLF, trailing spaces and blank padding lines. Normalizing those away
// keeps the digest comparable between the preview the user approved and the
// re-read that sends the keys, while any change to the dialog's own content
// changes the digest and refuses the stale approval.
func digestDialog(dialog string) string {
	lines := strings.Split(strings.ReplaceAll(dialog, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(strings.ReplaceAll(line, "\r", ""), " \t")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

// approvedKeys bounds one approval. The keys are the approval: an empty or
// oversized list is not something a user confirmed.
func approvedKeys(keys []string) ([]string, error) {
	switch {
	case len(keys) == 0:
		return nil, fmt.Errorf("%w: no keys were approved", ErrInvalidRequest)
	case len(keys) > approvalKeyLimit:
		return nil, fmt.Errorf("%w: %d keys were approved, at most %d are accepted", ErrInvalidRequest, len(keys), approvalKeyLimit)
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		text, err := textField(key, approvalKeyLength, "the approved key")
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
		}
		out = append(out, text)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// durable notes
// ---------------------------------------------------------------------------

// note records one deduplicated inbox entry for a task's project and reports
// whether it was new.
//
// The identity is the note's whole content, so asking the same question or
// reporting the same progress twice writes nothing new while a different one
// always notifies the primary. It deliberately shares the recipe the dispatch
// step machine uses for its own notes: one kind, one task and one text are one
// durable entry, whichever path wrote it first.
func (c *Coordinator) note(ctx context.Context, st task.Task, kind, summary, delivery string) (bool, error) {
	if strings.TrimSpace(st.ProjectKey) == "" {
		return false, nil
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + st.ID + "\x00" + summary))
	inserted, err := c.store.AddInbox(ctx, task.Inbox{
		ID:         kind + ":" + st.ID + ":" + hex.EncodeToString(sum[:6]),
		ProjectKey: st.ProjectKey,
		TaskID:     st.ID,
		Kind:       kind,
		Summary:    summary,
		Delivery:   delivery,
	})
	if err != nil {
		return false, fmt.Errorf("record the %s note for task %s: %w", kind, st.ID, err)
	}
	return inserted, nil
}

// ---------------------------------------------------------------------------
// bounded fields
// ---------------------------------------------------------------------------

// textField trims and bounds one human-supplied field, reporting what was wrong
// without deciding which sentinel the caller uses: the same field is a refusal
// reason for a report and for an answer, and callers classify it differently.
func textField(value string, limit int, what string) (string, error) {
	text := strings.TrimSpace(value)
	switch {
	case text == "":
		return "", fmt.Errorf("%s is empty", what)
	case !utf8.ValidString(text):
		return "", fmt.Errorf("%s is not valid UTF-8", what)
	case utf8.RuneCountInString(text) > limit:
		return "", fmt.Errorf("%s exceeds %d characters", what, limit)
	}
	return text, nil
}

// boundedText bounds a required field of a request body.
func boundedText(value string, limit int, what string) (string, error) {
	text, err := textField(value, limit, what)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	return text, nil
}

// boundedList bounds one required list of human-supplied strings. It refuses an
// empty list: a report with no evidence and no checks is not a report.
func boundedList(values []string, maxItems, maxLength int, what string) ([]string, error) {
	if len(values) > maxItems {
		return nil, fmt.Errorf("%s names %d entries, at most %d are accepted", what, len(values), maxItems)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		item, err := textField(value, maxLength, what)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty", what)
	}
	return out, nil
}

// quotedIDs renders workspace ids for a message that has to name everything
// that would have been accepted.
func quotedIDs(ids []string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, fmt.Sprintf("%q", id))
	}
	return strings.Join(quoted, " or ")
}

// truncateRunes cuts text to a rune bound, so a bounded note never ends inside a
// character.
func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

// sameReport reports whether two recorded reports are the same fact, which is
// what makes a repeated final report write nothing.
func sameReport(a, b task.Report) bool {
	return a.Kind == b.Kind &&
		a.Summary == b.Summary &&
		a.Branch == b.Branch &&
		a.Risk == b.Risk &&
		slices.Equal(a.Evidence, b.Evidence) &&
		slices.Equal(a.Checks, b.Checks)
}
