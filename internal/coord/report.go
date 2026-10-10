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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
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
	// ErrReportChanged refuses a decision when the task changed since the
	// primary displayed its persisted report for confirmation.
	ErrReportChanged = errors.New("coord: the reviewed task changed after its preview")
	// ErrCleanupChanged refuses cleanup when the task or inspected outcome no
	// longer matches the state the user confirmed.
	ErrCleanupChanged = errors.New("coord: cleanup changed after its preview")
	// ErrAlreadyDecided preserves the user's first disposition as a durable fact.
	ErrAlreadyDecided = errors.New("coord: the worker report already has a disposition")
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
	// ErrInputPending refuses any further write into a worker's pane while an earlier
	// attempt is unresolved: it may already have reached the worker, and typing the
	// same input twice is not recoverable. Only the user saying what they saw in the
	// pane settles it.
	ErrInputPending = errors.New("coord: an input attempt is waiting for the user's settlement")
	// ErrOwnership refuses an operation whose caller does not own the named task:
	// the primary must be the bound primary of that task's project.
	ErrOwnership       = errors.New("coord: the caller does not own this task")
	errCleanupForced   = errors.New("coord: Herdr claimed a forced checkout removal")
	errCleanupIdentity = errors.New("coord: Herdr returned a contradictory cleanup identity")
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

// The two channels this coordinator can write into a worker's pane, the decision the
// user records about an unresolved attempt, and the marker that records one attempt in
// the task's blocker.
const (
	channelAnswer = "answer"
	channelDialog = "dialog"

	decisionDelivered    = "delivered"
	decisionNotDelivered = "not_delivered"

	inputAttemptPrefix = "input attempt: "
	attemptNoKeys      = "-"
	// attemptNonceBytes sizes the random identity one attempt carries, so two attempts
	// with the same payload stay distinguishable.
	attemptNonceBytes = 8
)

// inputAttempt is the durable record of one attempt to write into a worker's pane.
//
// `agent prompt` and `agent send-keys` can answer ambiguously — a timeout after the
// input was written, or a response this build cannot interpret — and typing the same
// thing into a pane twice cannot be undone. So the uncertainty is recorded before the
// write and cleared only once its outcome is known: a confirmed send, a refusal that
// proves nothing was written, or the user saying what they saw in the pane.
//
// It identifies the attempt — its channel, a digest of what was sent, for a dialog the
// exact dialog it was approved for, and a nonce that makes this attempt distinguishable
// from every other one — and never carries what the user said. The nonce is what lets a
// settlement or a settlement-proof apply to *this* attempt: two attempts with the same
// payload are otherwise indistinguishable, so a decision about the first could clear the
// second.
type inputAttempt struct {
	channel  string
	identity string
	keysHash string
	nonce    string
	at       int64
}

// newAnswerAttempt is the attempt one ordinary answer makes.
func newAnswerAttempt(answer, nonce string, at time.Time) inputAttempt {
	return inputAttempt{
		channel:  channelAnswer,
		identity: hashInput(channelAnswer, answer),
		nonce:    nonce,
		at:       at.UnixMilli(),
	}
}

// newDialogAttempt is the attempt one approved key sequence makes, tied to the exact
// dialog it was approved for and identified by a digest of the keys.
func newDialogAttempt(dialogDigest string, keys []string, nonce string, at time.Time) inputAttempt {
	return inputAttempt{
		channel:  channelDialog,
		identity: strings.TrimSpace(dialogDigest),
		keysHash: hashInput(channelDialog, strings.Join(keys, "\x1f")),
		nonce:    nonce,
		at:       at.UnixMilli(),
	}
}

// attemptNonce is a fresh identity for one attempt. It is random rather than a counter
// because the coordinator owns no private state between calls, and it only has to be
// unique among the attempts of one task.
func (c *Coordinator) attemptNonce() (string, error) {
	raw := make([]byte, attemptNonceBytes)
	if err := c.random(raw); err != nil {
		return "", fmt.Errorf("generate an input attempt identity: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// String renders the attempt as the marker a task's blocker carries.
func (a inputAttempt) String() string {
	keys := a.keysHash
	if keys == "" {
		keys = attemptNoKeys
	}
	return fmt.Sprintf("%s%s %s %s %s %d", inputAttemptPrefix, a.channel, a.identity, keys, a.nonce, a.at)
}

// sameAs reports whether two records describe the same attempt *instance*, which is what
// a settlement, a settlement proof and a clear are all about: the same payload sent again
// is a different attempt, because the first one's outcome is what the user decided about.
func (a inputAttempt) sameAs(other inputAttempt) bool {
	return a.channel == other.channel && a.identity == other.identity &&
		a.keysHash == other.keysHash && a.nonce == other.nonce
}

// parseInputAttempt reads a recorded attempt. Anything that is not one — a dispatch
// blocker, a truncated or malformed marker, an attempt without an identity — is refused,
// so a task blocked for another reason is never treated as waiting on input.
func parseInputAttempt(marker string) (inputAttempt, bool) {
	marker = strings.TrimSpace(marker)
	if !strings.HasPrefix(marker, inputAttemptPrefix) {
		return inputAttempt{}, false
	}
	fields := strings.Fields(strings.TrimPrefix(marker, inputAttemptPrefix))
	if len(fields) != 5 || (fields[0] != channelAnswer && fields[0] != channelDialog) {
		return inputAttempt{}, false
	}
	keys := fields[2]
	if keys == attemptNoKeys {
		keys = ""
	}
	nonce := strings.TrimSpace(fields[3])
	// The nonce has to be exactly what this coordinator writes: sixteen hexadecimal
	// characters. A marker of the right shape but a different alphabet was not written
	// here, so it is refused rather than carried as a pending attempt.
	raw, err := hex.DecodeString(nonce)
	if err != nil || len(raw) != attemptNonceBytes {
		return inputAttempt{}, false
	}
	at, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil || at <= 0 {
		return inputAttempt{}, false
	}
	return inputAttempt{channel: fields[0], identity: fields[1], keysHash: keys, nonce: nonce, at: at}, true
}

// hashInput is the identity one attempt's payload gets: the payload itself is never
// stored, and two different payloads never share an identity.
func hashInput(channel, payload string) string {
	sum := sha256.Sum256([]byte(channel + "\x00" + payload))
	return hex.EncodeToString(sum[:8])
}

// formatMillis renders one attempt instant for a message a human reads.
func formatMillis(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }

// inputPendingError names the attempt a caller has to settle before anything else is
// written into that pane.
func inputPendingError(attempt inputAttempt) error {
	return fmt.Errorf("%w: a %s attempt of %s (%s) recorded at %s has not been settled; inspect the worker and confirm what it received with orch_settle_input before sending anything else",
		ErrInputPending, attempt.channel, attempt.identity, attempt.nonce,
		time.UnixMilli(attempt.at).UTC().Format(time.RFC3339))
}

// questionAttentionPrefix marks the attention one worker question records, and
// is what makes it distinguishable from a failure the dispatch step machine
// recorded: only this marker is cleared once the worker has its answer.
const questionAttentionPrefix = "worker question: "

// ---------------------------------------------------------------------------
// wire bodies
// ---------------------------------------------------------------------------

// ProgressParams is the body of a worker report whose kind is "progress".
type ProgressParams struct {
	Summary string `json:"summary"`
}

// QuestionParams is the body of a worker report whose kind is "question".
type QuestionParams struct {
	Text string `json:"text"`
}

// ReportParams is the body of a worker report whose kind is "result". It is its
// own type rather than task.Report because the wire spelling is lowercase and the
// extension, not the store, owns that shape.
type ReportParams struct {
	Kind     string   `json:"kind"`
	Summary  string   `json:"summary"`
	Branch   string   `json:"branch"`
	Evidence []string `json:"evidence"`
	// Checks are the checks the worker ran, each with the outcome it states. A
	// bare string decodes as a check that states none, which validateReport
	// refuses as an incomplete report rather than as a malformed body.
	Checks []task.Check `json:"checks"`
	Risk   string       `json:"risk"`
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

// SettleInputParams is the orch_settle_input body: the user's decision about an input
// attempt that was left uncertain.
type SettleInputParams struct {
	TaskID   string `json:"taskId"`
	Decision string `json:"decision"`
}

// ReviewParams is the orch_review body: the disposition the user confirmed
// after seeing this exact persisted report revision.
type ReviewParams struct {
	TaskID string `json:"taskId"`
	// Outcome is one of task.Outcomes(); Disposition is the free-text note.
	Outcome     string `json:"outcome"`
	Disposition string `json:"disposition"`
	// Successor names the task that replaces this one. Only a superseded decision
	// may name one, and it must name a task of the same project (issue #61).
	Successor        string `json:"successor"`
	ExpectedRevision int64  `json:"expectedRevision"`
}

// CleanupParams is the orch_cleanup body. Revision, WorkspaceID, and
// AlreadyRemoved bind the action to the read-only preview the user confirmed.
type CleanupParams struct {
	TaskID         string `json:"taskId"`
	Revision       int64  `json:"revision"`
	WorkspaceID    string `json:"workspaceId"`
	AlreadyRemoved bool   `json:"alreadyRemoved"`
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

// SettleInputResult is the answer to one settlement: which channel was resolved, what
// the user reported about it, and where the task stands now.
type SettleInputResult struct {
	TaskID   string     `json:"taskId"`
	Channel  string     `json:"channel"`
	Decision string     `json:"decision"`
	Stage    task.Stage `json:"stage"`
}

// ReportPreview is the exact persisted worker report the primary confirms.
// Revision binds that confirmation to the write, including any concurrent
// replacement of the report or change to the task's review state.
type ReportPreview struct {
	TaskID   string     `json:"taskId"`
	Revision int64      `json:"revision"`
	Stage    task.Stage `json:"stage"`
	Summary  string     `json:"summary"`
	Branch   string     `json:"branch"`
	Evidence []string   `json:"evidence"`
	// Checks is one line per recorded check, as task.Check.Label renders it: the
	// check's own text for a report from before outcomes were required, and
	// "name: outcome (detail)" for one that states them. It stays a list of strings
	// so the review confirmation, and an extension that predates outcomes, read
	// it as they always have.
	Checks []string `json:"checks"`
	Risk   string   `json:"risk"`
}

// ReviewResult is the answer to one recorded disposition.
type ReviewResult struct {
	TaskID      string     `json:"taskId"`
	Outcome     string     `json:"outcome"`
	Disposition string     `json:"disposition"`
	Successor   string     `json:"successor,omitempty"`
	Stage       task.Stage `json:"stage"`
}

// CleanupPreview is the read-only description of what cleanup would remove and
// whether it may: the primary shows it to the user before anything is approved.
type CleanupPreview struct {
	TaskID         string `json:"taskId"`
	Revision       int64  `json:"revision"`
	Branch         string `json:"branch"`
	Path           string `json:"path"`
	WorkspaceID    string `json:"workspaceId"`
	Reviewed       bool   `json:"reviewed"`
	Clean          bool   `json:"clean"`
	LiveWorker     bool   `json:"liveWorker"`
	Removable      bool   `json:"removable"`
	AlreadyRemoved bool   `json:"alreadyRemoved,omitempty"`
	ReopenRequired bool   `json:"reopenRequired,omitempty"`
	Reopening      bool   `json:"reopening,omitempty"` // Worktree open may have completed before its workspace was recorded.
	// Detail explains the first reason this checkout may not be removed, or what
	// exactly the removal will do when it may, in the words the primary shows the
	// user.
	Detail string `json:"detail"`
	// reopens records that Herdr no longer holds the checkout in any workspace, so
	// the removal has to open it by its recorded path first. It is unexported
	// because it only shapes Detail: the bridge recomputes it when it acts.
	reopens bool
}

// CleanupResult is the answer to one removed checkout. Branch is reported
// because cleanup preserves it, and Forced carries what Herdr reported for the
// removal the plugin asked for: it is always false, because a forced removal is
// refused rather than accepted, and reporting the real value keeps that visible.
type CleanupResult struct {
	TaskID      string     `json:"taskId"`
	Branch      string     `json:"branch"`
	Path        string     `json:"path"`
	WorkspaceID string     `json:"workspaceId"`
	Removed     bool       `json:"removed"`
	Forced      bool       `json:"forced"`
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
	return c.progress(ctx, st, text)
}

// progress records the note a preflighted worker pane's progress leaves. It is
// separate from Progress so the guard can be exercised against the snapshot a
// caller took before the primary decided.
//
// The note is the whole fact: no task field is written and the revision never
// moves, so there is nothing to compare and swap. The check that the task is
// still open and the insert are one store transaction instead (workerNote).
func (c *Coordinator) progress(ctx context.Context, st task.Task, text string) (ProgressResult, error) {
	inserted, err := c.workerNote(ctx, st, "progress", text, "worker progress")
	if err != nil {
		return ProgressResult{}, err
	}
	return ProgressResult{TaskID: st.ID, Stage: task.StageOf(st), Duplicate: !inserted}, nil
}

// Question records one durable question and puts the task in Needs input so the
// primary relays it to the user. It writes no task resource and never steals
// focus: a question is a fact about the task, not a terminal operation.
//
// The Needs-input marker and the note that tells the primary about it are one
// fact, so they are written in a single store transaction: a task can never be
// left waiting for an answer nobody was asked. Asking the same question again is
// the same fact — the marker already matches, so only the deduplicated note is
// (re)confirmed and the revision the primary is watching does not move.
func (c *Coordinator) Question(ctx context.Context, launchPaneID, text string) (QuestionResult, error) {
	st, err := c.workerTask(ctx, launchPaneID)
	if err != nil {
		return QuestionResult{}, err
	}
	question, err := boundedText(text, questionTextLimit, "the question")
	if err != nil {
		return QuestionResult{}, err
	}
	return c.question(ctx, st, question)
}

// question writes the question a preflighted worker pane asked. It is separate from
// Question so a snapshot taken before the primary's disposition can be replayed
// against the real write path.
//
// Both branches re-check the disposition where the write lands: the marker write
// does it in its mutation callback, and the deduplicated note does it inside the
// note's own transaction. A question asked before the task was closed is refused
// with ErrOwnership rather than moving a done task back to needs input.
func (c *Coordinator) question(ctx context.Context, st task.Task, question string) (QuestionResult, error) {
	attention := questionAttention(question)
	asked := st
	inserted := false
	if st.Attention != attention {
		updated, insertedNote, err := c.recordWithNote(ctx, st, func(next *task.Task) error {
			// Re-checked on every attempt: the disposition is final, and a
			// Review can land between workerTask and this write.
			if err := workerChannelOpen(*next); err != nil {
				return err
			}
			next.Attention = attention
			return nil
		}, c.noteFor(st, "question", question, "worker question"), "record the worker question")
		if err != nil {
			return QuestionResult{}, err
		}
		asked, inserted = updated, insertedNote
	} else {
		var err error
		inserted, err = c.workerNote(ctx, asked, "question", question, "worker question")
		if err != nil {
			return QuestionResult{}, err
		}
	}
	return QuestionResult{TaskID: asked.ID, Stage: task.StageOf(asked), Duplicate: !inserted}, nil
}

// FinalReport records a worker's complete result and moves the task to Ready for
// review. It never completes the task: only the primary's recorded disposition
// does.
//
// The report and the note that announces it are one durable fact: they are
// written in a single store transaction, so a task can never read as ready for
// review without the primary having been told. Re-submitting an identical report
// writes nothing to the task — it only re-confirms the note, which is
// deduplicated by its own identity.
func (c *Coordinator) FinalReport(ctx context.Context, launchPaneID string, report task.Report) (FinalReportResult, error) {
	st, err := c.workerTask(ctx, launchPaneID)
	if err != nil {
		return FinalReportResult{}, err
	}
	complete, err := validateReport(report, st)
	if err != nil {
		return FinalReportResult{}, err
	}
	return c.finalReport(ctx, st, complete)
}

// finalReport records the validated report a preflighted worker pane submitted. It
// is separate from FinalReport so a snapshot taken before the primary's disposition
// can be replayed against the real write path.
//
// Both branches re-check the disposition where the write lands: the report write
// does it in its mutation callback, and the deduplicated note does it inside the
// note's own transaction. A report submitted before the task was closed is refused
// with ErrOwnership, so a new result can never replace the one the user decided on.
func (c *Coordinator) finalReport(ctx context.Context, st task.Task, complete task.Report) (FinalReportResult, error) {
	// A worker path always has a bound project (workerTask re-reads it), which is
	// what makes the note addressable.
	note, err := resultNoteFor(st, complete)
	if err != nil {
		return FinalReportResult{}, err
	}
	recorded := st
	inserted := false
	if st.Report == nil || !sameReport(*st.Report, complete) || isQuestionAttention(st.Attention) || isLivenessAttention(st.Attention) {
		updated, insertedNote, err := c.recordWithNote(ctx, st, func(next *task.Task) error {
			// Re-checked on every attempt: the disposition is final, and a
			// Review can land between workerTask and this write.
			if err := workerChannelOpen(*next); err != nil {
				return err
			}
			// A report only ever describes the branch this task owns, whatever
			// landed in between.
			if !sameBranch(next.Branch, complete.Branch) {
				return fmt.Errorf("%w: the task now owns branch %q, not the reported %q", ErrReportInvalid, next.Branch, complete.Branch)
			}
			// The worker's own verified report is proof it is live, and a recorded
			// result outranks an absent or unknown state: either marker would
			// otherwise hide a task that is ready for review.
			if isQuestionAttention(next.Attention) || isLivenessAttention(next.Attention) {
				next.Attention = ""
			}
			next.Report = &complete
			return nil
		}, note, "record the final report")
		if err != nil {
			return FinalReportResult{}, err
		}
		recorded, inserted = updated, insertedNote
	} else {
		// The report is already durable, so only the deduplicated note is
		// (re)confirmed; a note that never landed is repaired here without
		// rewriting the task.
		var err error
		inserted, err = c.workerNoteEntry(ctx, st, note)
		if err != nil {
			return FinalReportResult{}, err
		}
	}
	return FinalReportResult{TaskID: recorded.ID, Stage: task.StageOf(recorded), Report: complete, Duplicate: !inserted}, nil
}

// validateReport refuses every report that is not a complete final result.
//
// Only Kind "result" is final — a partial report is what the "progress" kind is
// for — and a result has to carry what a reviewer needs: a summary, the branch
// this task actually owns, changed-file or commit evidence, the checks that ran,
// and a stated risk (including a stated absence of one).
func validateReport(report task.Report, st task.Task) (task.Report, error) {
	invalid := func(format string, args ...any) (task.Report, error) {
		return task.Report{}, fmt.Errorf("%w: %s", ErrReportInvalid, fmt.Sprintf(format, args...))
	}
	if kind := strings.TrimSpace(report.Kind); kind != "result" {
		return invalid("kind %q is not a final result; report partial progress with the \"progress\" kind instead", report.Kind)
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
	checks, err := validateChecks(report.Checks)
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
		"checks: "+strings.Join(task.CheckLabels(report.Checks), "; "),
		"risk: "+report.Risk,
	), " | "), inboxSummaryLimit)
}

// resultNoteFor keeps the primary-facing summary bounded but hashes the complete
// validated report. A later evidence item beyond the display cut is still a new
// durable result, not a retry of the earlier note.
func resultNoteFor(st task.Task, report task.Report) (task.Inbox, error) {
	encoded, err := json.Marshal(report)
	if err != nil {
		return task.Inbox{}, fmt.Errorf("encode the complete report identity for task %s: %w", st.ID, err)
	}
	sum := sha256.Sum256([]byte("result\x00" + st.ID + "\x00" + string(encoded)))
	return task.Inbox{
		ID:         "result:" + st.ID + ":" + hex.EncodeToString(sum[:]),
		ProjectKey: st.ProjectKey,
		TaskID:     st.ID,
		Kind:       "result",
		Summary:    reportNote(report),
		Delivery:   "worker result",
	}, nil
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

	// The attempt is recorded before anything is typed: from here on the answer may
	// have reached the worker, and nothing else may be sent to it until that is known.
	nonce, err := c.attemptNonce()
	if err != nil {
		return AnswerResult{}, err
	}
	attempt := newAnswerAttempt(answer, nonce, time.Now())
	// Nothing the worker's own state says settles an answer: it could have received the
	// answer, finished the turn and be idle again by now, so a resend would duplicate it.
	// Only the user, looking at the pane, settles this one — so no recorded attempt is
	// ever replaceable here.
	use, err := c.beginInputAttempt(ctx, st, attempt, nil)
	if err != nil {
		return AnswerResult{}, err
	}

	callCtx, cancel := c.callContext(ctx)
	_, promptErr := c.herdr.AgentPrompt(callCtx, pane, answer)
	cancel()
	answered := use
	switch {
	case promptErr == nil:
		settled, err := c.settleInputAttempt(ctx, use, attempt)
		if err != nil {
			return AnswerResult{}, fmt.Errorf("the answer reached pane %q, but the attempt record could not be settled: %w", pane, err)
		}
		answered = settled
	case errors.Is(promptErr, herdr.ErrAmbiguous):
		// The answer may or may not have been written: the attempt stays recorded and
		// the error is reported as it is.
		return AnswerResult{}, fmt.Errorf("answer the worker on pane %q: %w", pane, promptErr)
	default:
		// A definite refusal proves Herdr wrote nothing, so the answer can be sent again.
		settled, err := c.settleInputAttempt(ctx, use, attempt)
		if err != nil {
			return AnswerResult{}, fmt.Errorf("answer the worker on pane %q was refused, and the attempt record could not be settled: %w", pane, err)
		}
		answered = settled
		return AnswerResult{}, fmt.Errorf("answer the worker on pane %q: %w", pane, promptErr)
	}

	if isQuestionAttention(answered.Attention) {
		updated, err := c.record(ctx, answered, func(next *task.Task) error {
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
	nonce, err := c.attemptNonce()
	if err != nil {
		return ApproveDialogResult{}, err
	}
	attempt := newDialogAttempt(digest, approved, nonce, time.Now())
	if !isBlocked(view) {
		// The dialog an earlier attempt was approved for is gone: that is the one piece of
		// evidence which settles a key attempt on its own, and it is exactly the state a
		// worker that consumed the keys is left in. Nothing is approved here — the caller
		// is told there is no dialog to approve.
		if recorded, pending := parseInputAttempt(st.Blocker); pending && recorded.channel == channelDialog {
			if _, err := c.settleInputAttempt(ctx, st, recorded); err != nil {
				return ApproveDialogResult{}, err
			}
		}
		return ApproveDialogResult{}, fmt.Errorf("%w: the worker on pane %q is %s, not blocked on a dialog", ErrNotBlocked, pane, agentStatus(view))
	}

	// One read decides both things: whether the dialog on screen is still the one the user
	// approved, and whether an earlier attempt has visibly moved on. A read that shows
	// neither is refused, so those keys are never sent to a screen the user did not
	// approve.
	dialog, err := c.visibleDialog(ctx, pane)
	if err != nil {
		return ApproveDialogResult{}, err
	}
	seen := digestDialog(dialog)
	if seen != digest {
		return ApproveDialogResult{}, fmt.Errorf("%w: pane %q now shows a dialog hashing to %s, not the approved %s; nothing was sent", ErrDialogChanged, pane, seen, digest)
	}
	var settled *inputAttempt
	if recorded, pending := parseInputAttempt(st.Blocker); pending && recorded.channel == channelDialog {
		if recorded.identity == seen {
			// This very dialog may already hold those keys: only the user can say.
			return ApproveDialogResult{}, inputPendingError(recorded)
		}
		// The attempt was approved for a dialog that has since moved on, which is what the
		// keys would have done: it is settled by recording this new attempt.
		settled = &recorded
	}

	// Only now is the attempt recorded, tied to the exact dialog the user approved and to
	// the screen that was just verified: from here on those keys may be in that dialog, and
	// nothing else may be sent until that is known.
	use, err := c.beginInputAttempt(ctx, st, attempt, settled)
	if err != nil {
		return ApproveDialogResult{}, err
	}

	callCtx, cancel := c.callContext(ctx)
	sendErr := c.herdr.AgentSendKeys(callCtx, pane, approved...)
	cancel()
	answered := use
	switch {
	case sendErr == nil:
		settled, err := c.settleInputAttempt(ctx, use, attempt)
		if err != nil {
			return ApproveDialogResult{}, fmt.Errorf("the approved keys reached pane %q, but the attempt record could not be settled: %w", pane, err)
		}
		answered = settled
	case errors.Is(sendErr, herdr.ErrAmbiguous):
		// The keys may or may not be in the dialog: the attempt stays recorded.
		return ApproveDialogResult{}, fmt.Errorf("send the approved keys to pane %q: %w", pane, sendErr)
	default:
		settled, err := c.settleInputAttempt(ctx, use, attempt)
		if err != nil {
			return ApproveDialogResult{}, fmt.Errorf("sending the approved keys to pane %q was refused, and the attempt record could not be settled: %w", pane, err)
		}
		answered = settled
		return ApproveDialogResult{}, fmt.Errorf("send the approved keys to pane %q: %w", pane, sendErr)
	}

	if isQuestionAttention(answered.Attention) {
		updated, err := c.record(ctx, answered, func(next *task.Task) error {
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

// SettleInput records what the user saw in a worker's pane, which is the only thing
// that resolves an input attempt whose outcome is unknown.
//
// Nothing else can. An ambiguous write may or may not have reached the worker, and no
// status proves the negative: a worker can receive an answer, finish its turn and be
// idle again. `delivered` closes the question the input answered, because the worker
// has what it asked for; `not_delivered` leaves the question open so the primary can
// send again — deliberately, which is exactly what the user's confirmation makes it.
func (c *Coordinator) SettleInput(ctx context.Context, primaryLaunchPaneID, taskID, decision string) (SettleInputResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return SettleInputResult{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return SettleInputResult{}, err
	}
	choice, err := boundedText(decision, dispositionLimit, "the settlement decision")
	if err != nil {
		return SettleInputResult{}, err
	}
	if choice != decisionDelivered && choice != decisionNotDelivered {
		return SettleInputResult{}, fmt.Errorf("%w: %q is not a settlement; the user reports that the input either %s or %s",
			ErrInvalidRequest, decision, decisionDelivered, decisionNotDelivered)
	}
	recorded, pending := parseInputAttempt(st.Blocker)
	if !pending {
		return SettleInputResult{}, fmt.Errorf("%w: task %s records no input attempt to settle", ErrInvalidRequest, st.ID)
	}
	settled, err := c.record(ctx, st, func(next *task.Task) error {
		if err := settleableBlocker(*next, recorded); err != nil {
			return err
		}
		next.Blocker = ""
		if choice == decisionDelivered && isQuestionAttention(next.Attention) {
			next.Attention = ""
		}
		return nil
	}, "record the input settlement")
	if err != nil {
		return SettleInputResult{}, err
	}
	return SettleInputResult{TaskID: settled.ID, Channel: recorded.channel, Decision: choice, Stage: task.StageOf(settled)}, nil
}

// ReportPreview reads the durable final result owned by this primary. Inbox
// summaries and model-supplied report text are never evidence for a decision.
func (c *Coordinator) ReportPreview(ctx context.Context, primaryLaunchPaneID, taskID string) (ReportPreview, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return ReportPreview{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return ReportPreview{}, err
	}
	if !isReviewed(st) {
		return ReportPreview{}, fmt.Errorf("%w: task %s has no final result to preview", ErrNotReviewed, st.ID)
	}
	report := st.Report
	return ReportPreview{
		TaskID: st.ID, Revision: st.Revision, Stage: task.StageOf(st),
		Summary: report.Summary, Branch: report.Branch, Evidence: report.Evidence,
		Checks: task.CheckLabels(report.Checks), Risk: report.Risk,
	}, nil
}

// ReviewAtRevision records only the report revision the primary just showed to
// the user. An unrelated task update also invalidates the preview conservatively.
func (c *Coordinator) ReviewAtRevision(ctx context.Context, primaryLaunchPaneID, taskID string, expectedRevision int64, outcome, note, successor string) (ReviewResult, error) {
	if expectedRevision <= 0 {
		return ReviewResult{}, fmt.Errorf("%w: a positive report revision is required", ErrInvalidRequest)
	}
	return c.review(ctx, primaryLaunchPaneID, taskID, outcome, note, successor, expectedRevision)
}

// Review records the user's decision for a worker's reviewed result. This is
// the only thing that moves a task to Done.
func (c *Coordinator) Review(ctx context.Context, primaryLaunchPaneID, taskID, outcome, note, successor string) (ReviewResult, error) {
	return c.review(ctx, primaryLaunchPaneID, taskID, outcome, note, successor, 0)
}

// decisionOf is the single validation point of the disposition contract: a
// recognised outcome plus a bounded note. The note defaults to the outcome so a
// closed task always carries non-empty disposition text.
func decisionOf(outcome, note string) (string, string, error) {
	kind := strings.ToLower(strings.TrimSpace(outcome))
	if !task.ValidOutcome(kind) {
		return "", "", fmt.Errorf("%w: the outcome must be one of %s", ErrInvalidRequest, strings.Join(task.Outcomes(), ", "))
	}
	text := strings.TrimSpace(note)
	if text == "" {
		return kind, kind, nil
	}
	text, err := boundedText(text, dispositionLimit, "the disposition note")
	if err != nil {
		return "", "", err
	}
	return kind, text, nil
}

func sameDecision(st task.Task, kind, text, successor string) bool {
	return st.Disposition == text && st.Outcome == kind && st.Successor == successor
}

func alreadyDecided(st task.Task) error {
	return fmt.Errorf("%w: task %s already records %q (%s)", ErrAlreadyDecided, st.ID, st.Disposition, st.Outcome)
}

func (c *Coordinator) review(ctx context.Context, primaryLaunchPaneID, taskID, outcome, note, successor string, expectedRevision int64) (ReviewResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return ReviewResult{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return ReviewResult{}, err
	}
	kind, decision, err := decisionOf(outcome, note)
	if err != nil {
		return ReviewResult{}, err
	}
	successorID, err := normalizeSuccessor(successor)
	if err != nil {
		return ReviewResult{}, err
	}
	if !isReviewed(st) {
		return ReviewResult{}, fmt.Errorf("%w: task %s has no final result to decide on, so it cannot be closed", ErrNotReviewed, st.ID)
	}
	if st.Disposition != "" {
		if sameDecision(st, kind, decision, successorID) {
			return c.finishReview(ctx, project.Key, st)
		}
		return ReviewResult{}, alreadyDecided(st)
	}
	// A new decision's successor is checked once, here, before anything is written
	// (issue #61). A repeat of the recorded decision has already matched above.
	successorID, err = c.checkSuccessor(ctx, decisionSubject{projectKey: project.Key, id: st.ID}, kind, successorID)
	if err != nil {
		return ReviewResult{}, err
	}
	mutate := func(next *task.Task) error {
		if !isReviewed(*next) {
			return fmt.Errorf("%w: the task's final result was withdrawn while the decision was being recorded", ErrNotReviewed)
		}
		if next.Disposition != "" {
			return alreadyDecided(*next)
		}
		next.Outcome = kind
		next.Disposition = decision
		next.Successor = successorID
		return nil
	}
	var updated task.Task
	if expectedRevision > 0 {
		updated, err = c.store.UpdateTask(ctx, st.ID, expectedRevision, mutate)
	} else {
		updated, err = c.record(ctx, st, mutate, "record the user's disposition")
	}
	if err != nil {
		if errors.Is(err, state.ErrConflict) || errors.Is(err, ErrAlreadyDecided) {
			current, readErr := c.store.Task(ctx, st.ID)
			if readErr == nil && current.Disposition != "" {
				if sameDecision(current, kind, decision, successorID) {
					return c.finishReview(ctx, project.Key, current)
				}
				return ReviewResult{}, alreadyDecided(current)
			}
			if errors.Is(err, state.ErrConflict) && expectedRevision > 0 {
				return ReviewResult{}, fmt.Errorf("%w: task %s is no longer at revision %d", ErrReportChanged, st.ID, expectedRevision)
			}
		}
		if errors.Is(err, state.ErrSuccessorUnavailable) {
			return ReviewResult{}, fmt.Errorf("%w: %w", ErrSuccessorUnknown, err)
		}
		return ReviewResult{}, err
	}
	return c.finishReview(ctx, project.Key, updated)
}

// finishReview retries only the idempotent queue drain on an identical repeat
// decision; the stored disposition and task revision are never rewritten.
func (c *Coordinator) finishReview(ctx context.Context, projectKey string, updated task.Task) (ReviewResult, error) {
	result := ReviewResult{TaskID: updated.ID, Outcome: updated.Outcome, Disposition: updated.Disposition, Stage: task.StageOf(updated), Successor: updated.Successor}
	if _, err := c.DrainQueued(ctx, projectKey); err != nil {
		return result, fmt.Errorf("the disposition was recorded, but the queued tasks of %s could not be drained: %w", projectKey, err)
	}
	return result, nil
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
		Revision: st.Revision,
		Branch:   strings.TrimSpace(st.Branch),
		Path:     strings.TrimSpace(st.WorktreePath),
		Reviewed: isClosed(st),
	}
	pending := st.Cleanup != nil
	if pending {
		preview.Reopening = st.Cleanup.Opening
	}
	var target checkoutTarget
	if pending {
		if st.Cleanup.Refused {
			preview.Detail = fmt.Sprintf("Herdr returned a forced or contradictory removal result for %s; inspect checkout %s and branch %s manually", st.ID, st.Cleanup.Path, st.Cleanup.Branch)
			return preview, nil
		}
		var absent bool
		target, absent, err = c.inspectCleanup(ctx, st, *st.Cleanup)
		if err != nil {
			preview.Detail = err.Error()
			return preview, nil
		}
		if absent {
			preview.WorkspaceID = st.Cleanup.WorkspaceID
			preview.Clean = true
			preview.AlreadyRemoved = true
			preview.Removable = preview.Reviewed
			preview.Detail = fmt.Sprintf("checkout %s is already absent and branch %s is retained; confirm to settle the interrupted cleanup without removing it again", preview.Path, preview.Branch)
			return preview, nil
		}
		if st.Cleanup.RootPaneID != "" {
			if err := c.recordedCleanupWorkspaceIsIdle(ctx, *st.Cleanup); err != nil {
				preview.LiveWorker = errors.Is(err, ErrWorkerLive)
				preview.Detail = err.Error()
				return preview, nil
			}
		}
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

	provenanceOK := true
	if pending {
		preview.WorkspaceID = target.WorkspaceID
		preview.ReopenRequired = !target.Held
		preview.reopens = !target.Held
	} else {
		target, err = c.checkoutWorkspace(ctx, st)
		if err != nil {
			provenanceOK = false
			blockers = append(blockers, err.Error())
		} else {
			preview.WorkspaceID = target.WorkspaceID
			preview.ReopenRequired = !target.Held
			preview.reopens = !target.Held
		}
	}

	live, err := c.workerLive(ctx, st)
	preview.LiveWorker = live || err != nil
	switch {
	case err != nil:
		blockers = append(blockers, err.Error())
	case live:
		blockers = append(blockers, "the worker agent is still running in its pane; close it before removing its checkout")
	}

	preview.Removable = preview.Reviewed && preview.Clean && !preview.LiveWorker && preview.Path != "" && provenanceOK
	preview.Detail = strings.Join(blockers, "; ")
	if preview.Removable {
		held := fmt.Sprintf("workspace %s", preview.WorkspaceID)
		if preview.reopens {
			held = "no workspace holds it, so it is opened by its recorded path first"
		}
		preview.Detail = fmt.Sprintf("removes %s (branch %s, %s); the branch is kept and the removal is never forced", preview.Path, preview.Branch, held)
	}
	return preview, nil
}

// Cleanup removes a reviewed worker's checkout, and nothing else. It records
// intent before the non-forced removal, then settles only after verifying that
// the exact checkout is absent and its branch is still retained. An interrupted
// attempt is inspected before any retry.
func (c *Coordinator) Cleanup(ctx context.Context, primaryLaunchPaneID, taskID string) (CleanupResult, error) {
	return c.cleanup(ctx, primaryLaunchPaneID, taskID, 0, false, "")
}

// CleanupConfirmed requires the positive preview revision the user confirmed.
func (c *Coordinator) CleanupConfirmed(ctx context.Context, primaryLaunchPaneID string, request CleanupParams) (CleanupResult, error) {
	if request.Revision <= 0 {
		return CleanupResult{}, ErrCleanupChanged
	}
	return c.cleanup(ctx, primaryLaunchPaneID, request.TaskID, request.Revision, request.AlreadyRemoved, request.WorkspaceID)
}

func (c *Coordinator) cleanup(ctx context.Context, primaryLaunchPaneID, taskID string, expectedRevision int64, previewAlreadyRemoved bool, expectedWorkspaceID string) (CleanupResult, error) {
	project, err := c.primaryProject(ctx, primaryLaunchPaneID)
	if err != nil {
		return CleanupResult{}, err
	}
	st, err := c.ownedTask(ctx, project, taskID)
	if err != nil {
		return CleanupResult{}, err
	}
	if expectedRevision > 0 && st.Revision != expectedRevision {
		return CleanupResult{}, fmt.Errorf("%w: task %s is at revision %d, not the previewed revision %d", ErrCleanupChanged, st.ID, st.Revision, expectedRevision)
	}
	if previewAlreadyRemoved && st.Cleanup == nil {
		return CleanupResult{}, ErrCleanupChanged
	}
	if !isClosed(st) {
		return CleanupResult{}, fmt.Errorf("%w: task %s needs a final result and a disposition the user recorded before its checkout may be removed", ErrNotReviewed, st.ID)
	}

	pending := st.Cleanup != nil
	var attempt task.CleanupAttempt
	var target checkoutTarget
	if pending {
		attempt = *st.Cleanup
		if attempt.Refused {
			return CleanupResult{}, fmt.Errorf("%w: a previous removal result for task %s was forced or contradictory; inspect it manually", ErrProvenance, st.ID)
		}
		var absent bool
		target, absent, err = c.inspectCleanup(ctx, st, attempt)
		if err != nil {
			return CleanupResult{}, err
		}
		if absent {
			return c.settleCleanup(ctx, st, attempt, nil)
		}
		if expectedRevision > 0 && expectedWorkspaceID != target.WorkspaceID {
			return CleanupResult{}, fmt.Errorf("%w: task %s workspace changed from %q to %q after preview", ErrCleanupChanged, st.ID, expectedWorkspaceID, target.WorkspaceID)
		}
		if previewAlreadyRemoved {
			return CleanupResult{}, fmt.Errorf("%w: task %s checkout is listed again after the user confirmed settlement without removal", ErrCleanupChanged, st.ID)
		}
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
	if pending && attempt.RootPaneID != "" {
		if err := c.recordedCleanupWorkspaceIsIdle(ctx, attempt); err != nil {
			return CleanupResult{}, err
		}
	}

	if !pending {
		target, err = c.checkoutWorkspace(ctx, st)
		if err != nil {
			return CleanupResult{}, err
		}
		if expectedRevision > 0 && expectedWorkspaceID != target.WorkspaceID {
			return CleanupResult{}, fmt.Errorf("%w: task %s workspace changed from %q to %q after preview", ErrCleanupChanged, st.ID, expectedWorkspaceID, target.WorkspaceID)
		}
		st, attempt, err = c.recordCleanupAttempt(ctx, st, target, expectedRevision)
		if err != nil {
			return CleanupResult{}, err
		}
		var absent bool
		target, absent, err = c.inspectCleanup(ctx, st, attempt)
		if err != nil {
			return CleanupResult{}, err
		}
		if absent {
			return c.settleCleanup(ctx, st, attempt, nil)
		}
		if expectedRevision > 0 && expectedWorkspaceID != target.WorkspaceID {
			return CleanupResult{}, fmt.Errorf("%w: task %s workspace changed from %q to %q after preview", ErrCleanupChanged, st.ID, expectedWorkspaceID, target.WorkspaceID)
		}
	}

	current, err := c.store.Task(ctx, st.ID)
	if err != nil {
		return CleanupResult{}, err
	}
	if current.Revision != st.Revision || !sameCleanupAttempt(current.Cleanup, attempt) || current.Cleanup.Refused {
		return CleanupResult{}, ErrCleanupChanged
	}
	st, attempt = current, *current.Cleanup

	removed, st, attempt, removalAttempted, ambiguousRemoval, removeErr := c.removeCheckout(ctx, target, st, attempt)
	if errors.Is(removeErr, errCleanupForced) || errors.Is(removeErr, errCleanupIdentity) || errors.Is(removeErr, herdr.ErrIdentity) {
		if err := c.refuseCleanupOutcome(ctx, st, attempt); err != nil {
			removeErr = errors.Join(removeErr, fmt.Errorf("record the refused cleanup outcome: %w", err))
		}
		return CleanupResult{}, removeErr
	}
	if errors.Is(removeErr, ErrCleanupChanged) {
		return CleanupResult{}, removeErr
	}
	if removeErr != nil && (!removalAttempted || !ambiguousRemoval) {
		return CleanupResult{}, removeErr
	}
	if removeErr == nil && !removalAttempted {
		return CleanupResult{}, ErrCleanupChanged
	}
	_, absent, inspectErr := c.inspectCleanup(ctx, st, attempt)
	if inspectErr != nil {
		if removeErr != nil {
			return CleanupResult{}, fmt.Errorf("%w; cleanup result inspection also failed: %v", removeErr, inspectErr)
		}
		return CleanupResult{}, inspectErr
	}
	if absent {
		return c.settleCleanup(ctx, st, attempt, removed)
	}
	if removeErr != nil {
		return CleanupResult{}, removeErr
	}
	return CleanupResult{}, fmt.Errorf("%w: Herdr acknowledged removal of task %s, but its checkout is still listed; cleanup intent remains pending", ErrProvenance, st.ID)
}

// checkoutWorkspace validates the repository source and exact linked checkout
// against the task's recorded path and branch.
func (c *Coordinator) checkoutWorkspace(ctx context.Context, st task.Task) (checkoutTarget, error) {
	listed, err := c.worktreeList(ctx, st.ProjectKey)
	if err != nil {
		return checkoutTarget{}, fmt.Errorf("%w: the worktrees of %s could not be read: %w", ErrProvenance, st.ProjectKey, err)
	}
	return provenanceWorkspace(st, listed)
}

// checkoutTarget is where a task's recorded checkout lives: the workspace Herdr
// holds it in, and whether Herdr still holds it at all. A checkout no workspace
// holds has to be re-opened by its recorded path before it can be removed.
type checkoutTarget struct {
	WorkspaceID string
	Held        bool
}

// provenanceWorkspace requires the listed checkout to be linked, on the exact
// recorded branch, and held only by a workspace this task recorded. The workspace
// is a pointer to the path/branch identity; when no workspace holds it, its
// recorded worktree workspace is the fallback to be reopened before removal.
func provenanceWorkspace(st task.Task, listed *herdr.WorktreeList) (checkoutTarget, error) {
	checkout := strings.TrimSpace(st.WorktreePath)
	if checkout == "" || strings.TrimSpace(st.Branch) == "" {
		return checkoutTarget{}, fmt.Errorf("%w: task %s records no checkout to remove", ErrProvenance, st.ID)
	}
	if listed == nil {
		return checkoutTarget{}, fmt.Errorf("%w: Herdr listed no worktrees for the repository", ErrProvenance)
	}
	recorded := nonEmpty(st.WorktreeWorkspaceID, st.CurrentWorkspaceID)
	for _, entry := range listed.Worktrees {
		if !samePath(entry.Path, checkout) {
			continue
		}
		if entry.IsDetached || !entry.IsLinkedWorktree {
			return checkoutTarget{}, fmt.Errorf("%w: the checkout at %s is not a linked worktree", ErrProvenance, checkout)
		}
		if entry.Branch == nil || strings.TrimSpace(*entry.Branch) == "" || !sameBranch(*entry.Branch, st.Branch) {
			got := ""
			if entry.Branch != nil {
				got = *entry.Branch
			}
			return checkoutTarget{}, fmt.Errorf("%w: the checkout at %s reports branch %q, not the recorded %q", ErrProvenance, checkout, got, st.Branch)
		}
		if entry.OpenWorkspaceID != nil && strings.TrimSpace(*entry.OpenWorkspaceID) != "" {
			open := strings.TrimSpace(*entry.OpenWorkspaceID)
			if !containsString(recorded, open) {
				return checkoutTarget{}, fmt.Errorf("%w: Herdr holds the checkout at %s in workspace %q, which task %s never recorded (%s)",
					ErrProvenance, checkout, open, st.ID, quotedIDs(recorded))
			}
			return checkoutTarget{WorkspaceID: open, Held: true}, nil
		}
		if len(recorded) == 0 {
			return checkoutTarget{}, fmt.Errorf("%w: no workspace is recorded for the checkout at %s, so it cannot be removed safely", ErrProvenance, checkout)
		}
		return checkoutTarget{WorkspaceID: recorded[0]}, nil
	}
	return checkoutTarget{}, fmt.Errorf("%w: the worktree listing has no checkout at %s, so task %s cannot be removed from it", ErrProvenance, checkout, st.ID)
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

// removeCheckout removes the verified checkout, and never forces anything.
//
// A held workspace is tried first. Only a definite workspace_not_found or
// not_linked_worktree refusal permits reopening the recorded path. Before
// worktree open, the attempt records an Opening phase; after the exact repository,
// path, branch and root pane are reverified, the returned workspace is persisted
// before another removal call. Ambiguous remove results are returned to Cleanup,
// which inspects the worktree list instead of retrying them.
func (c *Coordinator) removeCheckout(
	ctx context.Context,
	target checkoutTarget,
	st task.Task,
	attempt task.CleanupAttempt,
) (*herdr.WorktreeRemoved, task.Task, task.CleanupAttempt, bool, bool, error) {
	removalAttempted := false
	if target.Held {
		removalAttempted = true
		removed, err := c.tryRemove(ctx, target.WorkspaceID, st)
		switch {
		case err == nil:
			return removed, st, attempt, true, false, nil
		case !isMissingTarget(err) && !isNotLinkedWorktree(err):
			return nil, st, attempt, true, errors.Is(err, herdr.ErrAmbiguous), err
		}
	}
	if !attempt.Opening {
		var err error
		st, attempt, err = c.recordCleanupOpening(ctx, st, attempt)
		if err != nil {
			return nil, st, attempt, removalAttempted, false, err
		}
	}
	opened, err := c.openCheckout(ctx, st)
	if err != nil {
		return nil, st, attempt, removalAttempted, false, err
	}
	if err := c.openedCheckoutIsIdle(ctx, opened); err != nil {
		return nil, st, attempt, removalAttempted, false, err
	}
	st, attempt, err = c.recordCleanupWorkspace(ctx, st, attempt, opened)
	if err != nil {
		return nil, st, attempt, removalAttempted, false, err
	}
	return nil, st, attempt, removalAttempted, false,
		fmt.Errorf("%w: checkout %s is now open in workspace %s; preview and confirm this workspace before removal",
			ErrCleanupChanged, st.WorktreePath, opened.WorkspaceID)
}

// tryRemove asks Herdr to remove one workspace's checkout, without force, and
// verifies the removal it acknowledged.
func (c *Coordinator) tryRemove(ctx context.Context, workspaceID string, st task.Task) (*herdr.WorktreeRemoved, error) {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	removed, err := c.herdr.WorktreeRemove(callCtx, workspaceID, false)
	if err != nil {
		return nil, fmt.Errorf("remove the checkout of task %s from workspace %q: %w", st.ID, workspaceID, err)
	}
	if removed != nil && removed.Forced {
		return nil, fmt.Errorf("%w: %w: Herdr reported a forced removal for task %s", ErrProvenance, errCleanupForced, st.ID)
	}
	return verifyRemoval(removed, st)
}

// openCheckout re-opens the task's recorded checkout so Herdr registers the
// workspace that holds it again, and verifies what Herdr reports for it before
// anything is removed.
//
// The checkout is named by its recorded path and the project's canonical root —
// never by a label — and Herdr's answer must describe that exact checkout as a
// linked worktree in a named workspace. Moving a pane out of a worktree's
// workspace closes that workspace, which is why Herdr then refuses to remove the
// checkout it still lists; opening it is the documented way back.
func (c *Coordinator) openCheckout(ctx context.Context, st task.Task) (*herdr.WorktreeOpened, error) {
	checkout := strings.TrimSpace(st.WorktreePath)
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	opened, err := c.herdr.WorktreeOpen(callCtx, st.ProjectKey, checkout)
	if err != nil {
		return nil, fmt.Errorf("%w: the checkout at %s could not be opened for removal: %w", ErrProvenance, checkout, err)
	}
	if !samePath(opened.WorktreePath, checkout) {
		return nil, fmt.Errorf("%w: Herdr opened the checkout at %s, not the recorded %s", ErrProvenance, opened.WorktreePath, checkout)
	}
	branch := strings.TrimSpace(opened.Branch)
	if branch == "" || !sameBranch(branch, st.Branch) {
		return nil, fmt.Errorf("%w: the opened checkout at %s tracks branch %q, not the recorded %q", ErrProvenance, checkout, branch, st.Branch)
	}
	if !opened.IsLinkedWorktree {
		return nil, fmt.Errorf("%w: Herdr did not report %s as a linked worktree checkout", ErrProvenance, checkout)
	}
	if strings.TrimSpace(opened.WorkspaceID) == "" {
		return nil, fmt.Errorf("%w: Herdr opened %s without naming a workspace to remove it from", ErrProvenance, checkout)
	}
	return opened, nil
}

// isNotLinkedWorktree reports Herdr's refusal to remove a workspace that is not
// the checkout's Herdr-managed worktree workspace: a definite refusal that wrote
// nothing, and the state a move leaves behind.
func isNotLinkedWorktree(err error) bool {
	var cliErr *herdr.CLIError
	return errors.As(err, &cliErr) && cliErr.IsCode(herdr.CodeNotLinkedWorktree)
}

// verifyRemoval checks the removal Herdr acknowledged: the checkout it removed
// must be the recorded one, and it must not have been a forced removal.
func verifyRemoval(removed *herdr.WorktreeRemoved, st task.Task) (*herdr.WorktreeRemoved, error) {
	if removed == nil {
		return nil, fmt.Errorf("%w: %w: Herdr reported no removed checkout for task %s", ErrProvenance, errCleanupIdentity, st.ID)
	}
	if removed.Forced {
		return nil, fmt.Errorf("%w: %w: Herdr removed the checkout of task %s by force, which this plugin never asks for; check %s", ErrProvenance, errCleanupForced, st.ID, st.WorktreePath)
	}
	if path := strings.TrimSpace(removed.Path); path != "" && !samePath(path, st.WorktreePath) {
		return nil, fmt.Errorf("%w: %w: Herdr removed the checkout at %s, not the recorded %s", ErrProvenance, errCleanupIdentity, path, st.WorktreePath)
	}
	return removed, nil
}

// inspectCleanup compares a pending intent with the authoritative worktree list.
// An absent checkout is settled only when its exact path is gone, its branch is
// still a local ref, and no other listed worktree holds that branch.
func (c *Coordinator) inspectCleanup(ctx context.Context, st task.Task, attempt task.CleanupAttempt) (checkoutTarget, bool, error) {
	path, branch := strings.TrimSpace(attempt.Path), strings.TrimSpace(attempt.Branch)
	if !isClosed(st) || path == "" || branch == "" ||
		!samePath(path, st.WorktreePath) || !sameBranch(branch, st.Branch) {
		return checkoutTarget{}, false, fmt.Errorf("%w: task %s no longer matches its recorded cleanup path and branch", ErrProvenance, st.ID)
	}
	if attempt.Refused {
		return checkoutTarget{}, false, fmt.Errorf("%w: Herdr returned a forced or contradictory removal result for task %s; inspect %s and branch %s manually", ErrProvenance, st.ID, path, branch)
	}
	listed, err := c.worktreeList(ctx, st.ProjectKey)
	if err != nil {
		return checkoutTarget{}, false, fmt.Errorf("%w: inspect cleanup of task %s: %w", ErrProvenance, st.ID, err)
	}
	target, absent, err := cleanupListingTarget(st, attempt, listed)
	if err != nil || !absent {
		return target, absent, err
	}
	if _, err := c.git(ctx, st.ProjectKey, "show-ref", "--verify", "--quiet", "--", "refs/heads/"+strings.TrimSpace(attempt.Branch)); err != nil {
		return checkoutTarget{}, false, fmt.Errorf("%w: branch %s was not confirmed retained after cleanup: %w", ErrProvenance, attempt.Branch, err)
	}
	return target, true, nil
}

func cleanupListingTarget(st task.Task, attempt task.CleanupAttempt, listed *herdr.WorktreeList) (checkoutTarget, bool, error) {
	path, branch := strings.TrimSpace(attempt.Path), strings.TrimSpace(attempt.Branch)
	var match *herdr.WorktreeInfo
	for i := range listed.Worktrees {
		entry := &listed.Worktrees[i]
		if samePath(entry.Path, path) {
			if match != nil {
				return checkoutTarget{}, false, fmt.Errorf("%w: worktree list names %s more than once", ErrProvenance, path)
			}
			match = entry
			continue
		}
		if entry.IsBare || entry.IsDetached {
			continue
		}
		if entry.Branch == nil || strings.TrimSpace(*entry.Branch) == "" {
			return checkoutTarget{}, false, fmt.Errorf("%w: worktree list does not identify the branch checked out at %s", ErrProvenance, entry.Path)
		}
		if sameBranch(*entry.Branch, branch) {
			return checkoutTarget{}, false, fmt.Errorf("%w: branch %s is checked out at %s instead of its recorded path %s", ErrProvenance, branch, entry.Path, path)
		}
	}
	if match != nil {
		if match.IsDetached || !match.IsLinkedWorktree || match.Branch == nil ||
			strings.TrimSpace(*match.Branch) == "" || !sameBranch(*match.Branch, branch) {
			got := ""
			if match.Branch != nil {
				got = *match.Branch
			}
			return checkoutTarget{}, false, fmt.Errorf("%w: checkout at %s is not a linked checkout on recorded branch %q (reported %q)", ErrProvenance, path, branch, got)
		}
		recorded := nonEmpty(st.WorktreeWorkspaceID, st.CurrentWorkspaceID)
		if match.OpenWorkspaceID != nil && strings.TrimSpace(*match.OpenWorkspaceID) != "" {
			open := strings.TrimSpace(*match.OpenWorkspaceID)
			if containsString(recorded, open) || strings.TrimSpace(attempt.WorkspaceID) == open {
				return checkoutTarget{WorkspaceID: open, Held: true}, false, nil
			}
			return checkoutTarget{}, false, fmt.Errorf("%w: Herdr holds checkout %s in unrecorded workspace %q", ErrProvenance, path, open)
		}
		if len(recorded) == 0 {
			return checkoutTarget{}, false, fmt.Errorf("%w: no workspace is recorded for checkout %s", ErrProvenance, path)
		}
		return checkoutTarget{WorkspaceID: recorded[0]}, false, nil
	}

	if _, err := os.Lstat(path); err == nil {
		return checkoutTarget{}, false, fmt.Errorf("%w: checkout path %s still exists but is absent from Herdr's worktree list", ErrProvenance, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return checkoutTarget{}, false, fmt.Errorf("%w: inspect checkout path %s: %w", ErrProvenance, path, err)
	}
	return checkoutTarget{WorkspaceID: attempt.WorkspaceID}, true, nil
}

func sameCleanupAttempt(record *task.CleanupAttempt, attempt task.CleanupAttempt) bool {
	return record != nil &&
		samePath(record.Path, attempt.Path) &&
		sameBranch(record.Branch, attempt.Branch) &&
		record.WorkspaceID == attempt.WorkspaceID &&
		record.RootPaneID == attempt.RootPaneID &&
		record.Opening == attempt.Opening &&
		record.Refused == attempt.Refused
}

func (c *Coordinator) recordCleanupAttempt(ctx context.Context, st task.Task, target checkoutTarget, expectedRevision int64) (task.Task, task.CleanupAttempt, error) {
	attempt := task.CleanupAttempt{
		Path:    strings.TrimSpace(st.WorktreePath),
		Branch:  strings.TrimSpace(st.Branch),
		Opening: !target.Held,
	}
	if target.Held {
		attempt.WorkspaceID = target.WorkspaceID
	}
	updated, err := c.record(ctx, st, func(next *task.Task) error {
		if expectedRevision > 0 && next.Revision != expectedRevision {
			return fmt.Errorf("%w: task %s changed before cleanup intent was recorded", ErrCleanupChanged, st.ID)
		}
		if !isClosed(*next) || !samePath(next.WorktreePath, attempt.Path) || !sameBranch(next.Branch, attempt.Branch) {
			return fmt.Errorf("%w: task %s changed before cleanup intent was recorded", ErrProvenance, st.ID)
		}
		if next.Cleanup != nil {
			return fmt.Errorf("%w: task %s already has an unsettled cleanup attempt", ErrProvenance, st.ID)
		}
		next.Cleanup = &attempt
		return nil
	}, "record cleanup intent")
	return updated, attempt, err
}

func (c *Coordinator) recordCleanupOpening(ctx context.Context, st task.Task, attempt task.CleanupAttempt) (task.Task, task.CleanupAttempt, error) {
	updated, err := c.record(ctx, st, func(next *task.Task) error {
		if !sameCleanupAttempt(next.Cleanup, attempt) || next.Cleanup.Refused || next.Cleanup.Opening {
			return fmt.Errorf("%w: task %s no longer has this cleanup opening phase", ErrCleanupChanged, st.ID)
		}
		nextAttempt := *next.Cleanup
		nextAttempt.Opening = true
		next.Cleanup = &nextAttempt
		return nil
	}, "record checkout reopen intent")
	if err != nil {
		return st, attempt, err
	}
	return updated, *updated.Cleanup, nil
}

func (c *Coordinator) recordCleanupWorkspace(ctx context.Context, st task.Task, attempt task.CleanupAttempt, opened *herdr.WorktreeOpened) (task.Task, task.CleanupAttempt, error) {
	if opened == nil || strings.TrimSpace(opened.WorkspaceID) == "" || strings.TrimSpace(opened.RootPaneID) == "" {
		return st, attempt, fmt.Errorf("%w: Herdr returned no workspace or root pane for the reopened checkout", ErrProvenance)
	}
	workspaceID, rootPaneID := strings.TrimSpace(opened.WorkspaceID), strings.TrimSpace(opened.RootPaneID)
	updated, err := c.record(ctx, st, func(next *task.Task) error {
		if !sameCleanupAttempt(next.Cleanup, attempt) || next.Cleanup.Refused || !next.Cleanup.Opening {
			return fmt.Errorf("%w: task %s no longer has this cleanup opening phase", ErrCleanupChanged, st.ID)
		}
		nextAttempt := *next.Cleanup
		nextAttempt.WorkspaceID = workspaceID
		nextAttempt.RootPaneID = rootPaneID
		nextAttempt.Opening = false
		next.Cleanup = &nextAttempt
		return nil
	}, "record reopened checkout workspace")
	if err != nil {
		return st, attempt, err
	}
	return updated, *updated.Cleanup, nil
}

func (c *Coordinator) openedCheckoutIsIdle(ctx context.Context, opened *herdr.WorktreeOpened) error {
	if opened == nil || strings.TrimSpace(opened.RootPaneID) == "" {
		return fmt.Errorf("%w: Herdr returned no root pane for the reopened checkout", ErrProvenance)
	}
	view, err := c.agentGet(ctx, opened.RootPaneID)
	if err != nil {
		var cliErr *herdr.CLIError
		if errors.As(err, &cliErr) && cliErr.Code == "agent_not_found" {
			return nil
		}
		if isMissingTarget(err) {
			return fmt.Errorf("%w: reopened workspace pane %q no longer exists: %v", ErrProvenance, opened.RootPaneID, err)
		}
		return fmt.Errorf("inspect reopened checkout pane %q before removal: %w", opened.RootPaneID, err)
	}
	if view.Agent.WorkspaceID != opened.WorkspaceID || !samePath(view.Agent.Cwd, opened.WorktreePath) {
		return fmt.Errorf("%w: a pane in the reopened workspace does not match checkout %s", ErrProvenance, opened.WorktreePath)
	}
	if !strings.EqualFold(strings.TrimSpace(view.Agent.Agent), c.opts.AgentKind) {
		return nil
	}
	return fmt.Errorf("%w: an OMP agent is live in reopened workspace %s; close it before removing the checkout", ErrWorkerLive, opened.WorkspaceID)
}

func (c *Coordinator) recordedCleanupWorkspaceIsIdle(ctx context.Context, attempt task.CleanupAttempt) error {
	if strings.TrimSpace(attempt.WorkspaceID) == "" || strings.TrimSpace(attempt.RootPaneID) == "" {
		return fmt.Errorf("%w: the cleanup attempt records no reopened workspace pane to verify", ErrProvenance)
	}
	return c.openedCheckoutIsIdle(ctx, &herdr.WorktreeOpened{
		WorkspaceID:  attempt.WorkspaceID,
		RootPaneID:   attempt.RootPaneID,
		WorktreePath: attempt.Path,
	})
}

func (c *Coordinator) settleCleanup(ctx context.Context, st task.Task, attempt task.CleanupAttempt, removed *herdr.WorktreeRemoved) (CleanupResult, error) {
	workspace := attempt.WorkspaceID
	if removed != nil && strings.TrimSpace(removed.WorkspaceID) != "" {
		workspace = strings.TrimSpace(removed.WorkspaceID)
	}
	summary := fmt.Sprintf("checkout %s of task %s removed; branch %s kept", attempt.Path, st.ID, attempt.Branch)
	updated, _, err := c.recordWithNote(ctx, st, func(next *task.Task) error {
		if !sameCleanupAttempt(next.Cleanup, attempt) || next.Cleanup.Refused {
			return fmt.Errorf("%w: task %s no longer has this cleanup intent", ErrProvenance, st.ID)
		}
		next.LaunchPaneID = ""
		next.PublicPaneID = ""
		next.TerminalID = ""
		next.WorktreeWorkspaceID = ""
		next.CurrentWorkspaceID = ""
		next.AgentName = ""
		next.OMPSessionID = ""
		next.Cleanup = nil
		return nil
	}, c.noteFor(st, "cleanup", summary, "worktree removed"), "record the removed checkout")
	if err != nil {
		return CleanupResult{}, fmt.Errorf("the checkout of task %s is removed, but the task could not be settled: %w", st.ID, err)
	}
	return CleanupResult{
		TaskID:      updated.ID,
		Branch:      attempt.Branch,
		Path:        attempt.Path,
		WorkspaceID: workspace,
		Removed:     true,
		Stage:       task.StageOf(updated),
	}, nil
}

func (c *Coordinator) refuseCleanupOutcome(ctx context.Context, st task.Task, attempt task.CleanupAttempt) error {
	_, err := c.record(ctx, st, func(next *task.Task) error {
		if !sameCleanupAttempt(next.Cleanup, attempt) {
			return fmt.Errorf("%w: task %s no longer has this cleanup intent", ErrProvenance, st.ID)
		}
		nextAttempt := *next.Cleanup
		nextAttempt.Refused = true
		next.Cleanup = &nextAttempt
		return nil
	}, "record refused cleanup outcome")
	return err
}

// ---------------------------------------------------------------------------
// ownership
// ---------------------------------------------------------------------------

// primaryProject reads the project a primary launch pane is bound to.
//
// It re-reads the store on every call rather than trusting anything the caller
// sent, and requires the record to still name that pane. Liveness is not
// re-derived here: the bridge runs VerifyCaller, a fresh `agent get` against
// this binding, before every operation it authorizes, so repeating it here would
// only restate that check.
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
//
// A task the primary has closed is not a task its worker may report on any more:
// the disposition is the end of that channel, so progress, questions and a second
// report are all refused. Anything else would let a closed task change under the
// decision the user made about it — a fresh report would replace the very result
// that was reviewed, and a question or a progress note would move a done task back
// into the working columns.
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
	}
	// The disposition is checked first, and again inside the write that consumes
	// this snapshot: it is final, and a primary's Review can land in between.
	if err := workerChannelOpen(st); err != nil {
		return task.Task{}, err
	}
	if st.Phase != task.AgentStarted && st.Phase != task.PromptSubmitted {
		return task.Task{}, fmt.Errorf("%w: task %s is in phase %q, so no worker has been briefed for it yet", ErrOwnership, st.ID, st.Phase)
	}
	if _, err := c.project(ctx, st.ProjectKey); err != nil {
		return task.Task{}, err
	}
	return st, nil
}

// workerChannelOpen refuses a worker write whose task the primary has closed.
//
// workerTask already refuses a closed task before a worker call builds anything,
// but that read is a snapshot: the primary's Review can land between it and the
// write, and the disposition is final. Every worker write therefore repeats this
// check where it can still be atomic with the write — the mutation callback of a
// task+note write, and the transaction of a note-only write — so an intervening
// decision is refused instead of overwritten.
func workerChannelOpen(st task.Task) error {
	if disposition := strings.TrimSpace(st.Disposition); disposition != "" {
		return fmt.Errorf("%w: task %s was closed with the disposition %q, so its worker may not report, ask or progress it any more",
			ErrOwnership, st.ID, disposition)
	}
	return nil
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
	announced, err := c.announcedWorker(ctx, st)
	if err != nil {
		return agentView{}, "", err
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
	case !announced.Proves(st.OMPSessionID, view.Ref):
		return agentView{}, "", fmt.Errorf("%w: pane %q reports %s session %q, which is not the session task %s recorded (%s)",
			ErrAgentChanged, pane, view.Ref.Kind, view.Ref.Value, st.ID, announced.Describe())
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
	announced, err := c.announcedWorker(ctx, st)
	if err != nil {
		return false, err
	}
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
		case announced.Proves(st.OMPSessionID, view.Ref):
			return true, nil
		default:
			return false, fmt.Errorf("%w: pane %q now runs %s session %q, which is not the session task %s recorded (%s)",
				ErrAgentChanged, pane, view.Ref.Kind, view.Ref.Value, st.ID, announced.Describe())
		}
	}
	return false, nil
}

// announcedWorker reads the session identity the worker's pane announced, and
// requires the coordinator to have verified it against `agent get`.
//
// A recorded session value and a reference Herdr reports now may be in different
// native forms (an id and a session file), and only the announcement knows both —
// so this is the fact every identity decision has to be made through. The
// announcement and its confirmation come from one store read
// (Store.VerifiedAnnouncement): reading them separately could judge a stale
// identity with a fresh confirmation after the pane announced a new session for the
// same file.
//
// An announcement nothing confirmed proves nothing: the request is refused until
// the coordinator's own comparison has made it a verified handshake.
func (c *Coordinator) announcedWorker(ctx context.Context, st task.Task) (task.Announcement, error) {
	pane := strings.TrimSpace(st.LaunchPaneID)
	if pane == "" {
		return task.Announcement{}, fmt.Errorf("%w: task %s records no launch pane, so its worker cannot be identified", ErrAgentChanged, st.ID)
	}
	// One read: the announcement and the confirmation it is judged by come from the
	// same row, so a refresh that announced a new identity for the same session
	// cannot be judged against the identity this call started with.
	announced, verified, err := c.store.VerifiedAnnouncement(ctx, pane)
	if err != nil {
		return task.Announcement{}, fmt.Errorf("read the verified handshake of pane %q: %w", pane, err)
	}
	if !verified {
		return task.Announcement{}, fmt.Errorf("%w: pane %q has no verified OMP session, so the recorded worker cannot be trusted", ErrAgentChanged, pane)
	}
	if strings.TrimSpace(st.OMPSessionID) == "" {
		return task.Announcement{}, fmt.Errorf("%w: task %s records no OMP session, so its worker cannot be identified", ErrAgentChanged, st.ID)
	}
	return announced, nil
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
	if panel, ok := extractDialogPanel(dialog); ok {
		return panel, nil
	}
	return dialog, nil
}

// extractDialogPanel returns the last closed, boxed dialog in the visible screen.
// OMP's live status/footer and earlier output change while a dialog is awaiting
// input; hashing them would invalidate the user's preview even when the active
// question and choices are unchanged. Unboxed dialogs retain the verbatim read.
func extractDialogPanel(screen string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(stripDialogFrame(line), "╭") || strings.HasPrefix(stripDialogFrame(line), "┌") {
			start = i
		}
	}
	if start < 0 {
		return "", false
	}
	end := -1
	for i := start + 1; i < len(lines); i++ {
		line := stripDialogFrame(lines[i])
		if strings.HasPrefix(line, "╰") || strings.HasPrefix(line, "└") {
			end = i
			break
		}
	}
	if end < 0 {
		return "", false
	}
	panel := make([]string, end-start+1)
	for i := start; i <= end; i++ {
		panel[i-start] = strings.TrimRight(stripDialogFrame(lines[i]), " \t\r")
	}
	return strings.Join(panel, "\n"), true
}

func stripDialogFrame(line string) string {
	line = strings.TrimSpace(line)
	for strings.HasPrefix(line, "│") || strings.HasPrefix(line, "³") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "│"), "³"))
	}
	for strings.HasSuffix(line, "│") || strings.HasSuffix(line, "³") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(line, "│"), "³"))
	}
	return line
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

// beginInputAttempt records one attempt before anything is written into a worker's
// pane, and refuses while a previous attempt is still unresolved.
//
// Persisting before the write is the point: a crash, a timeout or an unreadable answer
// after the write must leave the uncertainty durable, so nothing can type the same input
// twice. settled is the channel's own evidence that an unresolved attempt is harmless
// (a dialog that moved on); when it proves nothing, the caller is refused and told to
// ask the user.
func (c *Coordinator) beginInputAttempt(
	ctx context.Context,
	st task.Task,
	attempt inputAttempt,
	settled *inputAttempt,
) (task.Task, error) {
	if err := occupiedBlocker(st, settled); err != nil {
		return st, err
	}
	return c.record(ctx, st, func(next *task.Task) error {
		// Re-checked against the revision this write lands on: two callers that both saw
		// an empty blocker must not both write, or the second would erase an attempt whose
		// write may already have reached the pane.
		if err := occupiedBlocker(*next, settled); err != nil {
			return err
		}
		next.Blocker = attempt.String()
		return nil
	}, "record the input attempt before sending")
}

// occupiedBlocker reports why an input attempt may not be recorded on this task: a
// blocker this coordinator did not write is a human's to resolve, and a recorded attempt
// only ever gives way to the one attempt a caller proved settled.
func occupiedBlocker(st task.Task, settled *inputAttempt) error {
	blocker := strings.TrimSpace(st.Blocker)
	if blocker == "" {
		return nil
	}
	recorded, pending := parseInputAttempt(blocker)
	if !pending {
		return fmt.Errorf("%w: the task records a blocker that must be resolved before input is sent: %s", ErrBlocked, firstLine(blocker))
	}
	if settled == nil || !recorded.sameAs(*settled) {
		return inputPendingError(recorded)
	}
	return nil
}

// settleableBlocker reports why the attempt a user decided about may not be cleared:
// the latest record has to still hold that exact attempt, because a different one means
// something else is uncertain now and clearing it would silently permit a send.
func settleableBlocker(st task.Task, decided inputAttempt) error {
	current, pending := parseInputAttempt(st.Blocker)
	if !pending || !current.sameAs(decided) {
		return fmt.Errorf("%w: the attempt the user settled is no longer the recorded one; inspect the worker again", ErrInputPending)
	}
	return nil
}

// settleInputAttempt clears the recorded attempt once its outcome is known. It clears
// only that exact attempt: a different one is left for whoever recorded it.
func (c *Coordinator) settleInputAttempt(ctx context.Context, st task.Task, attempt inputAttempt) (task.Task, error) {
	return c.record(ctx, st, func(next *task.Task) error {
		if recorded, ok := parseInputAttempt(next.Blocker); ok && recorded.sameAs(attempt) {
			next.Blocker = ""
		}
		return nil
	}, "settle the input attempt")
}

// recordWithNote applies one update to a task and stores its deduplicated inbox
// note in a single store transaction, so a fact that announces itself can never
// land without the announcement.
//
// Like record, it builds on the revision the store holds at the moment of the
// write and retries only the compare-and-swap collision: a concurrent writer
// (the bridge adopting a session, or another coordinator call) must not make a
// legitimate report fail.
func (c *Coordinator) recordWithNote(
	ctx context.Context,
	st task.Task,
	mutate func(*task.Task) error,
	note task.Inbox,
	what string,
) (task.Task, bool, error) {
	var lastErr error
	for range recordAttempts {
		current, err := c.store.Task(ctx, st.ID)
		if err != nil {
			return st, false, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
		}
		updated, inserted, err := c.store.UpdateTaskWithNote(ctx, st.ID, current.Revision, mutate, note)
		if err == nil {
			// The fact and its note are durable together; deliver if the primary
			// can take it now, and keep it pending otherwise.
			c.flushAfterNote(ctx, updated.ProjectKey)
			return updated, inserted, nil
		}
		if !errors.Is(err, state.ErrConflict) {
			return st, false, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
		}
		lastErr = err
	}
	return st, false, fmt.Errorf("task %s: %s: %w", st.ID, what, lastErr)
}

// noteFor builds a durable inbox entry for task facts whose complete identity is
// their summary. The summary is what the primary reads, so repeating the same
// question or progress note is a no-op. Final reports use resultNoteFor instead:
// reportNote is intentionally bounded and cannot identify the complete report.
// Dispatch step notes share this recipe: one kind, one task and one complete
// summary are one durable entry, whichever path wrote it first.
func (c *Coordinator) noteFor(st task.Task, kind, summary, delivery string) task.Inbox {
	sum := sha256.Sum256([]byte(kind + "\x00" + st.ID + "\x00" + summary))
	return task.Inbox{
		ID:         kind + ":" + st.ID + ":" + hex.EncodeToString(sum[:6]),
		ProjectKey: st.ProjectKey,
		TaskID:     st.ID,
		Kind:       kind,
		Summary:    summary,
		Delivery:   delivery,
	}
}

// note records one deduplicated inbox entry for a task's project and reports
// whether it was new, for the facts that carry no transactional partner.
func (c *Coordinator) note(ctx context.Context, st task.Task, kind, summary, delivery string) (bool, error) {
	if strings.TrimSpace(st.ProjectKey) == "" {
		return false, nil
	}
	inserted, err := c.store.AddInbox(ctx, c.noteFor(st, kind, summary, delivery))
	if err != nil {
		return false, fmt.Errorf("record the %s note for task %s: %w", kind, st.ID, err)
	}
	// The note is durable; the primary is told now if it can be, and otherwise on
	// its next safe turn. A duplicate note is re-attempted too: it may be waiting
	// for exactly this.
	c.flushAfterNote(ctx, st.ProjectKey)
	return inserted, nil
}

// workerNote records one deduplicated note for a worker task, and only while the
// task is still open.
//
// A worker's note carries no task fact, so it has no revision to compare and swap
// — and it must not move the revision the primary is watching just to record
// progress. The check that the disposition is still absent and the insert share one
// store transaction instead (Store.AddInboxForTask), which is the atomic form of
// the guard: a primary's Review can commit between a caller's preflight and this
// write, and the disposition is final. A closed task is refused with ErrOwnership,
// exactly as workerTask refuses one.
//
// The note stays deduplicated: repeating it while the task is open reports false
// and writes nothing, which is what keeps a duplicate progress line or a
// re-confirmed question or report from notifying the primary twice.
func (c *Coordinator) workerNote(ctx context.Context, st task.Task, kind, summary, delivery string) (bool, error) {
	return c.workerNoteEntry(ctx, st, c.noteFor(st, kind, summary, delivery))
}

func (c *Coordinator) workerNoteEntry(ctx context.Context, st task.Task, note task.Inbox) (bool, error) {
	if strings.TrimSpace(st.ProjectKey) == "" {
		return false, nil
	}
	inserted, err := c.store.AddInboxForTask(ctx, note, workerChannelOpen)
	if err != nil {
		return false, fmt.Errorf("record the %s note for task %s: %w", note.Kind, st.ID, err)
	}
	// The note is durable; the primary is told now if it can be, and otherwise on
	// its next safe turn. A duplicate note is re-attempted too: it may be waiting
	// for exactly this.
	c.flushAfterNote(ctx, st.ProjectKey)
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

// validateChecks bounds the checks a result reports and requires each one to say
// how it ended. This is the single place that decides a check is complete: the
// extension's schema and the worker's instructions ask for the same thing, but a
// call that never went through them is refused here all the same.
//
// A worker reads a refusal to correct its next call, so each one names the check
// by its position and name and lists the outcomes it may state.
func validateChecks(checks []task.Check) ([]task.Check, error) {
	const what = "the checks and their outcomes"
	if len(checks) > reportChecksLimit {
		return nil, fmt.Errorf("%s names %d entries, at most %d are accepted", what, len(checks), reportChecksLimit)
	}
	outcomes := task.CheckOutcomes()
	vocabulary := strings.Join(outcomes[:len(outcomes)-1], ", ") + " or " + outcomes[len(outcomes)-1]
	out := make([]task.Check, 0, len(checks))
	for i, check := range checks {
		position := i + 1
		name, err := textField(check.Name, reportTextLimit, fmt.Sprintf("the name of check %d", position))
		if err != nil {
			return nil, err
		}
		outcome := strings.TrimSpace(check.Outcome)
		switch {
		case outcome == "":
			return nil, fmt.Errorf("check %d (%q) states no outcome; state %s", position, name, vocabulary)
		case !task.ValidCheckOutcome(outcome):
			return nil, fmt.Errorf("check %d (%q) has outcome %q, which is not one of %s",
				position, name, truncateRunes(outcome, 40), vocabulary)
		}
		clean := task.Check{Name: name, Outcome: outcome}
		// A detail is optional, so a blank one is no detail rather than an error.
		if strings.TrimSpace(check.Detail) != "" {
			detail, err := textField(check.Detail, reportTextLimit, fmt.Sprintf("the detail of check %d", position))
			if err != nil {
				return nil, err
			}
			clean.Detail = detail
		}
		out = append(out, clean)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty", what)
	}
	return out, nil
}

// boundedList bounds one required list of human-supplied strings. It refuses an
// empty list: a report with no evidence is not a report.
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
