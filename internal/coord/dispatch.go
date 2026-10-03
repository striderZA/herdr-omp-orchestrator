package coord

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// DispatchParams is the orch_dispatch request body the OMP extension sends.
type DispatchParams struct {
	Title         string   `json:"title"`
	Brief         string   `json:"brief"`
	Checks        []string `json:"checks"`
	Dependencies  []string `json:"dependencies"`
	ReservedPaths []string `json:"reservedPaths"`
	BaseRevision  string   `json:"baseRevision"`
	// TicketID optionally names the ticket this work is for. It is refused unless
	// the ticket exists in the caller's project and is not decided.
	TicketID string `json:"ticketId"`
}

// DispatchResult is the durable outcome of one dispatch, keyed by task id,
// branch and recorded pane identities rather than a Herdr label.
type DispatchResult struct {
	TaskID              string `json:"taskId"`
	ProjectKey          string `json:"projectKey"`
	Title               string `json:"title"`
	Branch              string `json:"branch"`
	BaseRevision        string `json:"baseRevision"`
	Phase               string `json:"phase"`
	Stage               string `json:"stage"`
	WorktreePath        string `json:"worktreePath,omitempty"`
	WorktreeWorkspaceID string `json:"worktreeWorkspaceId,omitempty"`
	LaunchPaneID        string `json:"launchPaneId,omitempty"`
	PublicPaneID        string `json:"publicPaneId,omitempty"`
	OMPSessionID        string `json:"ompSessionId,omitempty"`
	AgentName           string `json:"agentName,omitempty"`
	Attention           string `json:"attention,omitempty"`
	Blocker             string `json:"blocker,omitempty"`
	TicketID            string `json:"ticketId,omitempty"`
	// Duplicate is true when this request repeated work an open task of the
	// project already holds: no task was created and nothing was touched, and
	// every other field describes that existing task as it stands (issue #4).
	Duplicate bool `json:"duplicate,omitempty"`
}

// DispatchFor runs one dispatch for an already authorized primary. The project
// key comes from the verified caller, never from the request body. A request
// that repeats work an open task of the project already holds creates nothing:
// the answer describes that task as it stands and carries Duplicate (issue #4).
func (c *Coordinator) DispatchFor(ctx context.Context, projectKey string, params DispatchParams) (DispatchResult, error) {
	st, duplicate, err := c.dispatch(ctx, projectKey, params.TicketID, params.Title, params.Brief, params.Checks,
		params.Dependencies, params.ReservedPaths, params.BaseRevision)
	result := resultOf(st)
	result.Duplicate = duplicate
	return result, err
}

// resultOf projects the durable facts of a task onto the dispatch answer.
func resultOf(st task.Task) DispatchResult {
	return DispatchResult{
		TaskID:              st.ID,
		ProjectKey:          st.ProjectKey,
		Title:               st.Title,
		Branch:              st.Branch,
		BaseRevision:        st.BaseRevision,
		Phase:               string(st.Phase),
		Stage:               string(task.StageOf(st)),
		WorktreePath:        st.WorktreePath,
		WorktreeWorkspaceID: st.WorktreeWorkspaceID,
		LaunchPaneID:        st.LaunchPaneID,
		PublicPaneID:        st.PublicPaneID,
		OMPSessionID:        st.OMPSessionID,
		AgentName:           st.AgentName,
		Attention:           st.Attention,
		Blocker:             st.Blocker,
		TicketID:            st.TicketID,
	}
}

// Dispatch records a new task and, when the project's gates allow it, takes the
// task through worktree creation, agent start, the readiness handshake and the
// brief submission.
//
// A request that is legal but gated — a dependency whose disposition is not
// recorded yet (a final report alone does not open it), or a full concurrency
// budget — returns the recorded task in its queued phase
// with no side effects, and ResumeDispatch provisions it later. A request that
// cannot be honored at all (an unknown dependency, a base revision that is not
// a committed SHA, reserved paths another active worker owns) is refused before
// anything is written.
//
// A provisioning failure leaves an actionable task that names the recorded
// resource ids: attention when a retry is provably safe, a blocker when the
// outcome is unknown and a human must decide.
func (c *Coordinator) Dispatch(
	ctx context.Context,
	projectKey, title, brief string,
	checks, dependencies, reservedPaths []string,
	baseRevision string,
) (task.Task, error) {
	st, _, err := c.dispatch(ctx, projectKey, "", title, brief, checks, dependencies, reservedPaths, baseRevision)
	return st, err
}

// dispatch is Dispatch with an optional ticket. An absent ticket is the empty
// string; an id that was supplied but is only whitespace is a link the caller
// asked for and nothing can honor, so it is ErrInvalidRequest rather than a
// silently unlinked task. A named ticket is checked right
// after the request is validated and before anything is read from git or
// written, so an ordinary refusal costs nothing: an id that is not in the
// caller's project (or does not exist) is ErrTicketUnknown, a decided ticket is
// ErrAlreadyDecided. Nothing else about the ticket is checked (its column and
// stage are irrelevant). That read can go stale, so the store's own gate, inside
// the transaction that creates the task, is the authority (see createTask).
//
// The bool reports a repeat (issue #4): the request asks for work an undecided
// task of this project already holds, so nothing is created or touched (not even
// the queue drain) and that task is returned as it stands. The check before the
// drain is a read-only convenience; the store's gate inside the claim
// transaction decides it again, which is what settles two callers racing with
// the same work.
func (c *Coordinator) dispatch(
	ctx context.Context,
	projectKey, ticketID, title, brief string,
	checks, dependencies, reservedPaths []string,
	baseRevision string,
) (task.Task, bool, error) {
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return task.Task{}, false, err
	}
	request, err := newDispatchRequest(title, brief, checks, dependencies, reservedPaths)
	if err != nil {
		return task.Task{}, false, err
	}
	if ticketID != "" {
		linked := strings.TrimSpace(ticketID)
		if linked == "" {
			return task.Task{}, false, fmt.Errorf("%w: the ticket id is blank", ErrInvalidRequest)
		}
		if err := c.checkTicketForDispatch(ctx, project.Key, linked); err != nil {
			return task.Task{}, false, err
		}
		request.ticketID = linked
	}
	base, err := c.committedSHA(ctx, project.Key, baseRevision)
	if err != nil {
		return task.Task{}, false, err
	}
	existing, err := c.projectTasks(ctx, project.Key)
	if err != nil {
		return task.Task{}, false, err
	}
	if err := checkDependencies(existing, request.dependencies); err != nil {
		return task.Task{}, false, err
	}
	if holder, ok := task.OpenHolderOf(existing, request.work(project.Key, base)); ok {
		return holder, true, nil
	}

	// Queued work goes first: a free slot belongs to the tasks that have been
	// waiting, not to the request that happens to arrive after them. The drain
	// claims its slots durably, so a request that arrives when the queue takes the
	// last slot is queued rather than provisioned past the budget. A drain that
	// could not run is reported: it admitted nothing, and quietly taking
	// resources for this request instead would hide a stalled queue.
	if _, err := c.drainQueued(ctx, project.Key, ""); err != nil {
		return task.Task{}, false, err
	}

	// The budget and the reserved paths are decided by the durable gate, in one
	// transaction with the task's own creation: a second coordinator process
	// cannot pass the same checks and provision past the limit or take paths this
	// request holds. The same transaction answers a repeat that lost a race.
	st, claimed, token, err := c.createTask(ctx, project.Key, request, base)
	if err != nil {
		var repeat *state.DuplicateError
		if errors.As(err, &repeat) {
			return repeat.Existing, true, nil
		}
		return task.Task{}, false, err
	}
	if !claimed {
		// The durable gate refused the slot: the task is recorded queued and
		// records no side effect.
		return st, false, nil
	}
	// ClaimTask has committed the intent. Re-read dependencies at that point: a
	// primary disposition may have landed after the earlier existence check but
	// before the task was created, and a stale snapshot would leave this task
	// queued after the Review drain had already run.
	current, err := c.projectTasks(ctx, project.Key)
	if err != nil {
		if _, releaseErr := c.releaseSlot(ctx, st, token); releaseErr != nil {
			return st, false, fmt.Errorf("recheck dependencies for task %s: %w; release its slot: %w",
				st.ID, err, releaseErr)
		}
		return st, false, fmt.Errorf("recheck dependencies for task %s: %w", st.ID, err)
	}
	if ok, _ := c.dispatchable(current, st); !ok {
		// A dependency is still open: give the slot this request took (its own token)
		// back so another queued task can use it.
		if _, err := c.releaseSlot(ctx, st, token); err != nil {
			return st, false, fmt.Errorf("release provisioning slot for dependency-gated task %s: %w", st.ID, err)
		}
		return st, false, nil
	}
	provisioned, err := c.provision(ctx, st, slotClaim{token: token})
	return provisioned, false, err
}

// ResumeDispatch advances a recorded task from its persisted phase. It is
// idempotent: a task that was already dispatched, reviewed or closed is
// returned untouched, and no step whose side effect is recorded is repeated.
func (c *Coordinator) ResumeDispatch(ctx context.Context, taskID string) (task.Task, error) {
	st, err := c.store.Task(ctx, taskID)
	if err != nil {
		return task.Task{}, err
	}
	switch {
	case st.Disposition != "":
		return st, nil
	case st.Phase == task.PromptSubmitted:
		return st, nil
	case st.Blocker != "":
		return st, fmt.Errorf("%w: task %s: %s", ErrBlocked, st.ID, st.Blocker)
	}

	existing, err := c.projectTasks(ctx, st.ProjectKey)
	if err != nil {
		return st, err
	}
	if ok, _ := c.dispatchable(existing, st); !ok {
		return st, nil
	}
	// A resume provisions, so it takes the same durable slot a dispatch takes. The
	// order is provision's: the step claim comes first, then the capacity, then the
	// retirement of the reason this retry is acting on, and only then any external
	// call. Nothing here touches the task before that, so a resume that finds the step
	// in flight changes nothing at all — not the retryable reason another attempt
	// recorded, and not the claim that attempt still holds.
	//
	// A resume deliberately continues when the claim was already held: that claim is
	// the record of an attempt this caller was asked to retry, and the recorded phase
	// is what makes the retry idempotent (an existing checkout and an existing agent
	// are adopted, a submitted brief is never sent again). The claim it did not take is
	// renewed inside provision, once the durable step claim is this caller's — the only
	// ordering in which a renewal cannot outlive a step this caller never got.
	provisioned, err := c.provision(ctx, st, slotClaim{renew: true})
	if err != nil {
		return provisioned, err
	}
	// The task was provisioned, so a drain that could not run is reported without
	// pretending the step itself failed: a resume is idempotent from the recorded
	// phase, and the queued work it could not admit must not stay silent.
	if _, err := c.drainQueued(ctx, st.ProjectKey, st.ID); err != nil {
		return provisioned, fmt.Errorf("task %s was dispatched, but %w", st.ID, err)
	}
	return provisioned, nil
}

// ---------------------------------------------------------------------------
// request validation
// ---------------------------------------------------------------------------

// dispatchRequest is one validated dispatch request. Paths and identifiers are
// normalized once, so every later decision compares the same values.
type dispatchRequest struct {
	title         string
	brief         string
	checks        []string
	dependencies  []string
	reservedPaths []string
	ticketID      string
}

// work is the task a request describes, before it has an identity: exactly the
// fields that make up "the same work" (task.Task.Fingerprint). The repeat
// check and the claim both build the task from here, so they cannot compare
// different things.
func (r dispatchRequest) work(projectKey, base string) task.Task {
	return task.Task{
		ProjectKey:    projectKey,
		Title:         r.title,
		Brief:         r.brief,
		Checks:        r.checks,
		Dependencies:  r.dependencies,
		ReservedPaths: r.reservedPaths,
		BaseRevision:  base,
		Phase:         task.Intent,
		TicketID:      r.ticketID,
	}
}

// Bounds mirror the extension's own tool schema, so a request that never went
// through the OMP tool is refused the same way.
const (
	maxTitleLength   = 160
	maxBriefLength   = 16000
	maxCheckLength   = 512
	maxPathLength    = 512
	maxDependencies  = 32
	maxReservedPaths = 32
)

func newDispatchRequest(title, brief string, checks, dependencies, reservedPaths []string) (dispatchRequest, error) {
	request := dispatchRequest{
		title: strings.TrimSpace(title),
		brief: strings.TrimSpace(brief),
	}
	switch {
	case request.title == "":
		return dispatchRequest{}, fmt.Errorf("%w: a title is required", ErrInvalidRequest)
	case len(request.title) > maxTitleLength:
		return dispatchRequest{}, fmt.Errorf("%w: the title exceeds %d characters", ErrInvalidRequest, maxTitleLength)
	case request.brief == "":
		return dispatchRequest{}, fmt.Errorf("%w: a brief is required", ErrInvalidRequest)
	case len(request.brief) > maxBriefLength:
		return dispatchRequest{}, fmt.Errorf("%w: the brief exceeds %d characters", ErrInvalidRequest, maxBriefLength)
	case len(checks) == 0:
		return dispatchRequest{}, fmt.Errorf("%w: at least one acceptance check is required", ErrInvalidRequest)
	}
	for _, check := range checks {
		trimmed := strings.TrimSpace(check)
		switch {
		case trimmed == "":
			return dispatchRequest{}, fmt.Errorf("%w: an acceptance check is empty", ErrInvalidRequest)
		case len(trimmed) > maxCheckLength:
			return dispatchRequest{}, fmt.Errorf("%w: an acceptance check exceeds %d characters", ErrInvalidRequest, maxCheckLength)
		}
		request.checks = append(request.checks, trimmed)
	}
	if len(dependencies) > maxDependencies {
		return dispatchRequest{}, fmt.Errorf("%w: more than %d dependencies", ErrInvalidRequest, maxDependencies)
	}
	for _, dependency := range dependencies {
		trimmed := strings.TrimSpace(dependency)
		if trimmed == "" {
			return dispatchRequest{}, fmt.Errorf("%w: a dependency id is empty", ErrInvalidRequest)
		}
		if !containsString(request.dependencies, trimmed) {
			request.dependencies = append(request.dependencies, trimmed)
		}
	}
	if len(reservedPaths) > maxReservedPaths {
		return dispatchRequest{}, fmt.Errorf("%w: more than %d reserved paths", ErrInvalidRequest, maxReservedPaths)
	}
	for _, reserved := range reservedPaths {
		normalized, err := normalizeReservedPath(reserved)
		if err != nil {
			return dispatchRequest{}, err
		}
		if !containsString(request.reservedPaths, normalized) {
			request.reservedPaths = append(request.reservedPaths, normalized)
		}
	}
	return request, nil
}

// normalizeReservedPath turns a repository-relative reservation into one
// comparable form: forward slashes, no trailing separator, and — on Windows,
// where the filesystem is case-insensitive — case-folded.
func normalizeReservedPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a reserved path is empty", ErrInvalidRequest)
	}
	if len(trimmed) > maxPathLength {
		return "", fmt.Errorf("%w: the reserved path %q exceeds %d characters", ErrInvalidRequest, trimmed, maxPathLength)
	}
	slashed := strings.ReplaceAll(trimmed, `\`, "/")
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("%w: the reserved path %q must be repository-relative", ErrInvalidRequest, trimmed)
	}
	cleaned := filepath.ToSlash(filepath.Clean(slashed))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: the reserved path %q must stay inside the repository", ErrInvalidRequest, trimmed)
	}
	if isCaseInsensitivePath() {
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned, nil
}

// ---------------------------------------------------------------------------
// gates: dependencies, reservations, concurrency
// ---------------------------------------------------------------------------

// dispatchable reports whether a task's dependencies allow it to provision now.
//
// The concurrency budget is deliberately not decided here: it is decided by the
// durable gate (Store.ClaimTask / Store.ClaimProvisioningSlot), in the same
// transaction that records the task or claims its slot, so two coordinator
// processes cannot both pass this check and provision past the limit. A
// satisfied dependency never becomes unsatisfied (a recorded decision is final),
// so a stale read can only queue a task that could have run, never the other way
// around. A dependency closed with a non-accepting outcome never becomes
// satisfied: its dependents stay queued for good unless the primary re-plans.
func (c *Coordinator) dispatchable(existing []task.Task, st task.Task) (bool, string) {
	if reason := unsatisfiedDependency(existing, st); reason != "" {
		return false, reason
	}
	return true, ""
}

// unsatisfiedDependency names the first dependency that has not released its
// dependents. Closure and acceptance are distinct: any recorded outcome closes a
// task, but only an accepting one (integrated, kept, or a legacy disposition)
// satisfies a dependency.
//
// The gate is the recorded decision and nothing else: a worker's final report only
// says its worker stopped and makes the dependency ready for review, which is not a
// decision about the work. A dependent task therefore stays queued until the primary
// records an accepting outcome (Review) — the moment the reviewer has actually seen
// the work it depends on.
func unsatisfiedDependency(existing []task.Task, st task.Task) string {
	for _, dependency := range st.Dependencies {
		dep, ok := findTask(existing, dependency)
		if !ok {
			return fmt.Sprintf("dependency %s is not recorded in this project", dependency)
		}
		if stage := task.StageOf(dep); stage != task.Done {
			return fmt.Sprintf("dependency %s is %s, not closed by a disposition yet", dependency, stage)
		}
		if !dep.Accepted() {
			return fmt.Sprintf("dependency %s was closed as %s, which does not release its dependents; the primary must re-plan and re-dispatch this task without that dependency", dependency, dep.Outcome)
		}
	}
	return ""
}

// checkDependencies refuses a request whose dependencies do not exist in the
// same project, before anything is written.
func checkDependencies(existing []task.Task, dependencies []string) error {
	for _, dependency := range dependencies {
		if _, ok := findTask(existing, dependency); !ok {
			return fmt.Errorf("%w: dependency %s is not a task of this project", ErrInvalidRequest, dependency)
		}
	}
	return nil
}

// isCaseInsensitivePath reports whether this platform compares paths without
// case, which is how a reservation is normalized before it is stored.
func isCaseInsensitivePath() bool {
	return filepath.Separator == '\\'
}

// findTask reads one task out of a project listing.
func findTask(existing []task.Task, id string) (task.Task, bool) {
	for _, st := range existing {
		if st.ID == id {
			return st, true
		}
	}
	return task.Task{}, false
}

// ---------------------------------------------------------------------------
// task creation
// ---------------------------------------------------------------------------

// createTask records one task in its intent phase through the durable dispatch
// gate, which decides the concurrency budget and the reserved paths in the same
// transaction. It reports whether the task also claimed a provisioning slot;
// only the identity collision and the id-less insert are retried, because both
// touch nothing.
func (c *Coordinator) createTask(ctx context.Context, projectKey string, request dispatchRequest, base string) (task.Task, bool, int64, error) {
	var lastErr error
	for range idAttempts {
		id, branch, err := c.newIdentity()
		if err != nil {
			return task.Task{}, false, 0, err
		}
		st := request.work(projectKey, base)
		st.ID, st.Branch = id, branch
		claimed, created, token, err := c.store.ClaimTask(ctx, st, c.opts.Concurrency)
		if err == nil {
			return created, claimed, token, nil
		}
		lastErr = err
		if errors.Is(err, state.ErrReservedConflict) {
			// The reservation is held by another task: the request is refused and
			// nothing was recorded. Report it as the coordinator's own refusal so
			// callers classify it the same way as before.
			return task.Task{}, false, 0, fmt.Errorf("%w: %s", ErrReservedConflict, err)
		}
		if errors.Is(err, state.ErrTicketUnavailable) {
			// The ticket was decided (or went away) after the pre-check passed. The
			// gate recorded nothing; report the refusal the pre-check would have given.
			if refusal := c.checkTicketForDispatch(ctx, projectKey, request.ticketID); refusal != nil {
				return task.Task{}, false, 0, refusal
			}
		}
		if !errors.Is(err, state.ErrConflict) {
			return task.Task{}, false, 0, fmt.Errorf("record task intent for %q: %w", projectKey, err)
		}
	}
	return task.Task{}, false, 0, fmt.Errorf("record task intent for %q: %w", projectKey, lastErr)
}

// releaseSlot gives back a claimed slot after a step failed before recording any
// resource, and reports whether the capacity actually went back.
//
// The token is the claim this caller acquired: only that claim may be given back,
// so a retry that took the task over keeps the capacity it is about to use. A
// storage failure is returned — the task still consumes capacity, so hiding the
// error would leave queued work stalled without an actionable cause — while a
// store refusal (the claim is no longer this caller's, or a step owns it now) is
// reported as "nothing was freed". That is the retry working, not this caller's
// failure, and it is what keeps a dispatch that lost the race from reporting a task
// as failed while its worker is starting.
func (c *Coordinator) releaseSlot(ctx context.Context, st task.Task, token int64) (bool, error) {
	err := c.store.ReleaseProvisioningSlot(ctx, st.ID, token)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, state.ErrConflict):
		return false, nil
	default:
		return false, err
	}
}

// DrainQueued provisions the project's queued tasks that a disposition (or a
// freed slot) now allows, oldest first, and returns what it provisioned.
//
// It is the surface that makes a queued task progress at the moment its gate
// opens, instead of waiting for a future dispatch: a task becomes eligible when
// the primary records a disposition for the worker that held a slot (Review), so
// whoever records that fact calls this. It is idempotent, safe to call
// concurrently (every candidate is re-gated and claims its slot through the
// durable gate), and it never repeats a recorded step.
//
// Capacity semantics: a slot is held from the moment a task claims it until the
// primary records a disposition for it. A worker's final report neither frees a
// slot — its pane and checkout still exist — nor opens a dependency: a dependent
// task waits for the disposition that closes its prerequisite.
//
// A storage failure is returned instead of being swallowed: a drain that could
// not read the queue or claim a slot has admitted nothing, and a caller that
// cannot tell that apart from "nothing was eligible" would leave the queue parked
// with no reason recorded. A spent budget is not a failure — it is the normal
// reason a queued task waits.
func (c *Coordinator) DrainQueued(ctx context.Context, projectKey string) ([]task.Task, error) {
	if strings.TrimSpace(projectKey) == "" {
		return nil, fmt.Errorf("%w: project key is required", ErrInvalidRequest)
	}
	return c.drainQueued(ctx, projectKey, "")
}

// drainQueued provisions the project's queued tasks whose gates now allow it,
// oldest first, bounded by the concurrency budget. It returns what it provisioned
// and the storage failure that stopped it, if any.
//
// It is the automatic drain behind a freed slot, a disposition or a restart: a
// task that was queued because the budget was full or a dependency was not closed
// is re-read, re-gated and claimed through the same durable gate a dispatch uses,
// so draining in two processes cannot provision past the budget and cannot take a
// path another task holds. The queue is ordered, so a task the gate still refuses
// stops the walk: a later task must not overtake it. excluded is the task the
// caller has just handled itself.
//
// A candidate's claim is handed to provision together with the step it takes next,
// and provision follows the ordering every caller follows: the step claim first,
// then the capacity, then the retirement of the retryable reason, then the step.
// A candidate that turns out not to own the step gives that claim back (only its
// own token), so the walk moves on with the capacity free; a candidate whose own
// step fails records its attention or blocker and the walk continues — that is a
// fact about one task, not a broken drain. What the drain does surface is its own
// bookkeeping: a write it had to make about a candidate's retry (retiring a
// retryable reason) that could not be stored.
func (c *Coordinator) drainQueued(ctx context.Context, projectKey, excluded string) ([]task.Task, error) {
	if strings.TrimSpace(projectKey) == "" {
		return nil, fmt.Errorf("%w: project key is required", ErrInvalidRequest)
	}
	queued, err := c.projectTasks(ctx, projectKey)
	if err != nil {
		return nil, fmt.Errorf("drain the queued tasks of %s: %w", projectKey, err)
	}
	var drained []task.Task
	for _, cand := range queued {
		if cand.ID == excluded || cand.Phase != task.Intent || cand.Blocker != "" ||
			cand.Disposition != "" || len(cand.Checks) == 0 {
			continue
		}
		if ok, _ := c.dispatchable(queued, cand); !ok {
			return drained, nil
		}
		took, token, err := c.store.ClaimProvisioningSlot(ctx, cand.ID, c.opts.Concurrency)
		switch {
		case errors.Is(err, state.ErrBudgetExhausted):
			// No slot is left: every later candidate would be refused for the same
			// reason, and the ones before it were not eligible.
			return drained, nil
		case errors.Is(err, state.ErrConflict):
			// The claim moved under this walk: another caller took it (the same fact
			// as took == false), so this candidate is theirs and the walk goes on.
			continue
		case err != nil:
			// A claim that could not be stored is not capacity pressure: the drain
			// has not admitted anything and the caller has to know.
			return drained, fmt.Errorf("claim the provisioning slot of task %s while draining %s: %w", cand.ID, projectKey, err)
		}
		if !took {
			// Somebody already holds this task's claim: it is being provisioned right
			// now, or it is waiting for a deliberate resume. A drain must never
			// provision a task another caller owns — that would race the same branch,
			// the same pane and the same brief — and it must not renew that caller's
			// claim either: the renewal belongs to the caller that decided to use it.
			continue
		}
		provisioned, err := c.provision(ctx, cand, slotClaim{token: token})
		if err != nil {
			// The claim this candidate took is given back by provision when it turns
			// out not to own the step (only that token, never another caller's), so the
			// walk moves on with the capacity free again.
			var bookkeeping *retryBookkeepingError
			if errors.As(err, &bookkeeping) {
				// Not this task's fact: a write the drain itself had to make could not be
				// stored, and swallowing that would stall the queue silently.
				return drained, fmt.Errorf("drain the queued tasks of %s: %w", projectKey, bookkeeping.err)
			}
			// The task recorded its own attention or blocker; the next candidate can
			// still use the slot if this one gave it back.
			continue
		}
		drained = append(drained, provisioned)
	}
	return drained, nil
}

// newIdentity returns a fresh task id and its branch. Herdr labels are never
// used as identities.
func (c *Coordinator) newIdentity() (string, string, error) {
	random := make([]byte, taskIDBytes)
	if err := c.random(random); err != nil {
		return "", "", fmt.Errorf("generate a task identity: %w", err)
	}
	suffix := hex.EncodeToString(random)
	return taskIDPrefix + suffix, taskBranchBase + suffix, nil
}

func fillRandom(buf []byte) error {
	_, err := rand.Read(buf)
	return err
}

// ---------------------------------------------------------------------------
// the persisted step machine
// ---------------------------------------------------------------------------

// slotClaim is the capacity claim one provisioning caller holds, and whether it
// still has to be renewed.
//
// A caller that acquires the claim, or claims it fresh, owns the capacity from that
// moment. A caller that continues on a claim another attempt left behind — the
// deliberate resume — does not own it yet: it renews the claim once the step is its
// own, so the attempt that failed can no longer give the slot back from under the
// step. Renewing before the step were taken would be unsound in the other
// direction: a resume that then loses the step (ErrStepInFlight) would leave the
// capacity held by nobody, because the failed attempt's release can no longer match
// a token that was renewed past it and no drain may provision a task whose claim
// somebody holds.
type slotClaim struct {
	// token is the value this caller may present when it gives the capacity back.
	token int64
	// renew marks a claim this caller continued on without taking it: it becomes
	// this caller's only once the durable step claim is.
	renew bool
}

// assumeSlotClaim takes the capacity a task's step needs, with the step claim
// already this caller's.
//
// It is the only place a slot claim is taken over, and the order it is called in is
// what makes the take-over sound:
//
//   - a caller that already brought a claim (a dispatch or a drain that just claimed
//     the task) keeps it — the capacity was decided in the durable gate's own
//     transaction;
//   - a resume that continued on an earlier attempt's claim renews it, which
//     Store.RenewProvisioningSlot only allows the live step owner to do. If the
//     earlier attempt gave the claim back in the meantime there is nothing to renew,
//     so this caller claims the capacity like any other caller — and reports false
//     when the budget is spent or somebody else took it, so the caller leaves the
//     task queued instead of provisioning it without a slot;
//   - a caller that brought neither (the brief resume inside a running task) takes
//     nothing: the task's phase already owns its capacity.
func (c *Coordinator) assumeSlotClaim(ctx context.Context, st task.Task, claim slotClaim) (slotClaim, bool, error) {
	switch {
	case claim.token != 0, !claim.renew:
		return claim, true, nil
	}
	token, err := c.store.RenewProvisioningSlot(ctx, st.ID, c.owner)
	switch {
	case err == nil:
		return slotClaim{token: token}, true, nil
	case !errors.Is(err, state.ErrConflict):
		return slotClaim{}, false, err
	}
	took, fresh, claimErr := c.store.ClaimProvisioningSlot(ctx, st.ID, c.opts.Concurrency)
	switch {
	case errors.Is(claimErr, state.ErrBudgetExhausted), claimErr == nil && !took:
		return slotClaim{}, false, nil
	case claimErr != nil:
		return slotClaim{}, false, claimErr
	}
	return slotClaim{token: fresh}, true, nil
}

// abandonSlotClaim gives back the capacity a caller brought when it turns out not to
// own the task's step, and returns cause.
//
// Only the token this caller acquired is released: a claim a concurrent caller renewed
// in the meantime is refused by the store (it is not this caller's any more) instead
// of being cleared, so the retry that owns it keeps its capacity. A caller that
// brought no token (a resume) has nothing to give back.
func (c *Coordinator) abandonSlotClaim(ctx context.Context, st task.Task, claim slotClaim, cause error) error {
	if claim.token == 0 {
		return cause
	}
	if _, err := c.releaseSlot(ctx, st, claim.token); err != nil {
		return fmt.Errorf("%w; release provisioning slot for task %s: %w", cause, st.ID, err)
	}
	return cause
}

// retryBookkeepingError reports that the write a caller had to make about its own
// retry — retiring the retryable reason it was acting on — could not be stored.
// Nothing external ran, so it is not one task's provisioning failure: the task is
// exactly as it was, and a drain surfaces this instead of walking on as though the
// task had recorded its own reason.
type retryBookkeepingError struct{ err error }

func (e *retryBookkeepingError) Error() string { return e.err.Error() }

func (e *retryBookkeepingError) Unwrap() error { return e.err }

// provision advances a task as far as its recorded phase allows, one step at a
// time. Each step is idempotent on its own (it inspects the recorded facts and
// authoritative Herdr state before acting) and each step is taken exclusively
// through the durable step claim, so two callers resuming the same task — a
// startup pass and a user's adjudication, or two startup passes — cannot both
// issue the same side effect. A caller that does not get the claim stops and
// reports ErrStepInFlight; the recorded phase is what the other caller continues
// from, and a claim left by a crashed caller can be taken over once its lease
// expires, so crash recovery is not weakened.
//
// The order of what this caller takes is deliberate, and it is what keeps a lost
// race from leaving traces:
//
//  1. the durable step claim — nothing else is touched until the step is this
//     caller's, so a caller that finds the step in flight changes nothing at all;
//  2. the capacity (see assumeSlotClaim) — again nothing external has run;
//  3. the retirement of the retryable reason this attempt is acting on;
//  4. the step's own external calls.
//
// A path that ends before (4) keeps neither claim: the step goes back, and so does
// the capacity this caller took (a claim it brought) — a task waiting for a reason
// that still holds is better than one holding capacity nobody is using.
//
// claim carries the slot claim this caller holds (or has to take over), and it is
// threaded, never re-read: a step that fails before recording any resource may only
// give that exact claim back, so a retry that took the task over keeps the capacity
// it is provisioning with.
func (c *Coordinator) provision(ctx context.Context, st task.Task, claim slotClaim) (task.Task, error) {
	for {
		if st.Phase == task.PromptSubmitted {
			return st, nil
		}
		lease := c.opts.StepLease
		if st.Phase == task.AgentStarted {
			// A brief prompt can remain in flight until CallTimeout. Give the
			// pre-send record and the whole external call time before a user
			// adjudication is allowed to take over this claim.
			minimum := 2 * c.opts.CallTimeout
			if lease < minimum {
				lease = minimum
			}
		}
		took, err := c.store.ClaimStep(ctx, st.ID, st.Phase, c.owner, lease)
		if err != nil {
			return st, err
		}
		if !took {
			// Either another caller is running this exact step, or it finished it
			// and moved the task on. Re-read the task to tell the two apart: a
			// phase that advanced is ours to continue from, an unchanged phase is
			// somebody else's step.
			current, readErr := c.store.Task(ctx, st.ID)
			if readErr != nil {
				return st, readErr
			}
			if current.Phase == st.Phase {
				// Another attempt owns this step: this caller never took the task over,
				// so it gives back the claim it brought and leaves the retryable reason
				// this task carries untouched (it never ran the step that reason is
				// about).
				return current, c.abandonSlotClaim(ctx, st, claim, fmt.Errorf(
					"%w: task %s is already running its %s step elsewhere",
					ErrStepInFlight, st.ID, phaseStep(st.Phase)))
			}
			st = current
			continue
		}
		// The step is this caller's. Only now does it take what the step needs: the
		// capacity first, then the retirement of the retryable reason, both before any
		// external call.
		//
		// The retryable reason belongs to the caller that is retrying the task — a
		// dispatch, a drain, or a resume that took the capacity over. A brief
		// adjudication resumes a running task on the capacity its phase already owns
		// and deliberately leaves the task's attention to its owner.
		retrying := claim.token != 0 || claim.renew
		assumed, ok, err := c.assumeSlotClaim(ctx, st, claim)
		if err != nil {
			c.releaseStep(ctx, st)
			return st, err
		}
		if !ok {
			// No capacity to run this step: give the step back so a later retry does
			// not have to wait out this lease, and leave the task queued.
			c.releaseStep(ctx, st)
			return st, nil
		}
		claim = assumed
		if retrying && st.Attention != "" {
			// Retrying a step retires the retryable reason it failed with: an
			// attention that outlived its retry would leave a running task looking
			// like it still needs a human. A blocker is never touched.
			retried, err := c.record(ctx, st, func(next *task.Task) error {
				next.Attention = ""
				return nil
			}, "clear the retryable attention")
			if err != nil {
				// Nothing external ran, so no claim is kept: the step goes back, and so
				// does the capacity this caller took. The task keeps the reason it
				// carried, because the retry that would have retired it did not happen.
				c.releaseStep(ctx, st)
				if _, releaseErr := c.releaseSlot(ctx, st, claim.token); releaseErr != nil {
					return st, fmt.Errorf("%w; release provisioning slot for task %s: %w", err, st.ID, releaseErr)
				}
				return st, &retryBookkeepingError{err: fmt.Errorf("task %s: clear the retryable attention: %w", st.ID, err)}
			}
			st = retried
		}
		next, err := c.provisionPhase(ctx, st, claim.token)
		if err != nil {
			return next, err
		}
		st = next
	}
}

// provisionPhase runs the one step the task's recorded phase asks for.
func (c *Coordinator) provisionPhase(ctx context.Context, st task.Task, token int64) (task.Task, error) {
	switch st.Phase {
	case task.Intent:
		return c.createWorktree(ctx, st, token)
	case task.WorktreeCreated:
		return c.startWorker(ctx, st)
	case task.AgentStarted:
		return c.submitBrief(ctx, st)
	default:
		return st, fmt.Errorf("%w: task %s is at unknown phase %q", ErrInvalidRequest, st.ID, st.Phase)
	}
}

// phaseStep names the work one phase asks for, for a message a human reads.
func phaseStep(phase task.Phase) string {
	switch phase {
	case task.Intent:
		return "create the worktree"
	case task.WorktreeCreated:
		return "start the worker"
	case task.AgentStarted:
		return "submit the brief"
	default:
		return string(phase)
	}
}

// releaseStep gives up this coordinator's step claim after a step that failed
// without advancing the phase, so a retry does not have to wait out the lease.
func (c *Coordinator) releaseStep(ctx context.Context, st task.Task) {
	_ = c.store.ReleaseStep(ctx, st.ID, c.owner)
}

// definiteFailure reports whether a failed step provably wrote nothing, which is
// what lets its step claim go back immediately. Every undecided outcome — a
// timeout, Herdr's own ambiguity, an uninterpretable response, an identity this
// build cannot read — keeps the claim instead, so the next attempt waits for the
// lease or for a human, and never races a side effect that may have happened.
func definiteFailure(cause error) bool {
	switch {
	case errors.Is(cause, herdr.ErrTimeout),
		errors.Is(cause, herdr.ErrAmbiguous),
		errors.Is(cause, herdr.ErrMalformedResponse),
		errors.Is(cause, herdr.ErrIdentity),
		errors.Is(cause, herdr.ErrOutputTooLarge),
		errors.Is(cause, ErrAmbiguousStep),
		errors.Is(cause, ErrEvidenceChanged),
		errors.Is(cause, ErrSessionRefUnsupported):
		return false
	default:
		return true
	}
}

// createWorktree creates the isolated checkout for a task and records the exact
// returned identities.
//
// Before asking Herdr to create anything it lists the repository's worktrees:
// a checkout for this task's unique branch can only be this task's, so a crash
// between the CLI answering and the phase being recorded cannot produce a
// second checkout.
//
// token is the slot claim the caller acquired for this task; it is what a definite
// failure gives back, and only that claim.
func (c *Coordinator) createWorktree(ctx context.Context, st task.Task, token int64) (task.Task, error) {
	if listed, err := c.findWorktree(ctx, st.ProjectKey, st.Branch); err != nil {
		c.releaseStep(ctx, st)
		return c.failTask(ctx, st, "read the repository's worktrees", isTerminal(err),
			fmt.Errorf("the repository's worktrees could not be read before creating %q: %w", st.Branch, err))
	} else if listed != nil {
		return c.adoptWorktree(ctx, st, listed)
	}

	callCtx, cancel := c.callContext(ctx)
	created, err := c.herdr.WorktreeCreate(callCtx, st.ProjectKey, st.Branch, st.BaseRevision)
	cancel()
	if err == nil {
		branch := strings.TrimSpace(created.Branch)
		if branch == "" {
			branch = st.Branch
		}
		if !sameBranch(branch, st.Branch) {
			return c.failTask(ctx, st, "create the worktree", true,
				fmt.Errorf("%w: asked for branch %q but `worktree create` answered with %q",
					ErrAgentChanged, st.Branch, created.Branch))
		}
		next, recordErr := c.record(ctx, st, func(record *task.Task) error {
			record.Branch = branch
			record.WorktreePath = created.WorktreePath
			record.WorktreeWorkspaceID = created.WorkspaceID
			record.CurrentWorkspaceID = created.WorkspaceID
			record.LaunchPaneID = created.RootPaneID
			record.PublicPaneID = created.RootPaneID
			record.Phase = task.WorktreeCreated
			return nil
		}, "record the created worktree")
		if recordErr != nil {
			// The checkout exists but is not recorded: a human must decide,
			// because a retry from this phase cannot tell it apart later.
			return c.failTask(ctx, st, "record the created worktree", true, recordErr)
		}
		return next, nil
	}

	if !errors.Is(err, herdr.ErrAmbiguous) {
		// A definite failure created nothing, so the claimed slot goes back and
		// another queued task can use it. The slot is released only after the failure
		// is stored: releasing it first would hand the task's own slot to another
		// caller while this one still had to write, and the step claim is released by
		// the same store transaction that records the failure.
		failed, failureErr := c.failTask(ctx, st, "create the worktree", false,
			fmt.Errorf("worktree create for root %s branch %q base %s failed: %w",
				st.ProjectKey, st.Branch, st.BaseRevision, err))
		// failTask returns the recorded failure as its report, so the slot goes back
		// unless recording itself failed (a PersistenceError): a failure nobody could
		// store must not free the slot of the task it belongs to.
		var unrecorded *PersistenceError
		if !errors.As(failureErr, &unrecorded) {
			freed, releaseErr := c.releaseSlot(ctx, failed, token)
			if releaseErr != nil {
				return failed, fmt.Errorf("%w; release provisioning slot for task %s: %w",
					failureErr, failed.ID, releaseErr)
			}
			if freed {
				// The capacity this attempt held is free now, so the work that was
				// waiting for it starts here: nothing else consults the queue until
				// something happens to run, and for a project whose queue is waiting on
				// this slot that may be a long time. The failed task is excluded — its
				// cause is not fixed by the drain, and an immediate retry would only
				// fail again. A release that freed nothing (a retry already owns the
				// claim) drains nothing either: the capacity is not free.
				if _, drainErr := c.drainQueued(ctx, failed.ProjectKey, failed.ID); drainErr != nil {
					return failed, fmt.Errorf("%w; %w", failureErr, drainErr)
				}
			}
		}
		return failed, failureErr
	}
	// The create may have taken effect. Inspect the repository before deciding.
	listed, listErr := c.findWorktree(ctx, st.ProjectKey, st.Branch)
	if listErr != nil {
		return c.failTask(ctx, st, "create the worktree", true,
			fmt.Errorf("%w: worktree create for root %s branch %q base %s is ambiguous and the worktree listing failed: %v",
				ErrAmbiguousStep, st.ProjectKey, st.Branch, st.BaseRevision, listErr))
	}
	if listed == nil {
		return c.failTask(ctx, st, "create the worktree", true,
			fmt.Errorf("%w: worktree create for root %s branch %q base %s is ambiguous and `worktree list` does not list that branch",
				ErrAmbiguousStep, st.ProjectKey, st.Branch, st.BaseRevision))
	}
	next, adoptErr := c.adoptWorktree(ctx, st, listed)
	if adoptErr != nil {
		return c.failTask(ctx, st, "adopt the existing worktree", true, adoptErr)
	}
	return next, nil
}

// adoptWorktree records a checkout that already exists for the task's branch,
// including its workspace and root pane when Herdr reports them.
func (c *Coordinator) adoptWorktree(ctx context.Context, st task.Task, listed *herdr.WorktreeInfo) (task.Task, error) {
	if listed.Branch != nil && !sameBranch(*listed.Branch, st.Branch) {
		return st, fmt.Errorf("%w: task %s asked for branch %q but the checkout at %s tracks %q",
			ErrAgentChanged, st.ID, st.Branch, listed.Path, *listed.Branch)
	}
	workspace := ""
	if listed.OpenWorkspaceID != nil {
		workspace = strings.TrimSpace(*listed.OpenWorkspaceID)
	}
	next, err := c.record(ctx, st, func(record *task.Task) error {
		record.WorktreePath = listed.Path
		if workspace != "" {
			record.WorktreeWorkspaceID = workspace
			record.CurrentWorkspaceID = workspace
		}
		record.Phase = task.WorktreeCreated
		return nil
	}, "record the worktree")
	if err != nil {
		return st, err
	}
	return next, nil
}

// sameBranch compares two branch names: Git branch names are case-sensitive,
// but the checkout path Herdr reports may normalize separators only.
func sameBranch(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }

// findWorktree looks for the task's branch in the repository's worktree
// listing. It never guesses: a branch this task generated is matched exactly.
func (c *Coordinator) findWorktree(ctx context.Context, root, branch string) (*herdr.WorktreeInfo, error) {
	ctx, cancel := c.callContext(ctx)
	defer cancel()
	list, err := c.herdr.WorktreeList(ctx, root)
	if err != nil {
		return nil, err
	}
	if !samePath(list.Source.RepoRoot, root) {
		return nil, fmt.Errorf("%w: worktree list for %s reported repository root %q",
			ErrDifferentRepository, root, list.Source.RepoRoot)
	}
	for i := range list.Worktrees {
		listed := list.Worktrees[i]
		if listed.Branch == nil || !sameBranch(*listed.Branch, branch) {
			continue
		}
		return &listed, nil
	}
	return nil, nil
}

// startWorker makes sure the worker's pane runs exactly one OMP agent and then
// waits for the extension to announce the session Herdr reports for it.
func (c *Coordinator) startWorker(ctx context.Context, st task.Task) (task.Task, error) {
	paneID := st.PublicPaneID
	if strings.TrimSpace(paneID) == "" {
		resolved, err := c.paneForTask(ctx, st)
		if err != nil {
			return c.failTask(ctx, st, "resolve the worker pane", true, err)
		}
		next, err := c.record(ctx, st, func(record *task.Task) error {
			record.LaunchPaneID = resolved
			record.PublicPaneID = resolved
			return nil
		}, "record the worker pane")
		if err != nil {
			return c.failTask(ctx, st, "record the worker pane", true, err)
		}
		st = next
		paneID = resolved
	}

	name, err := c.ensureAgent(ctx, paneID, WorkerAgentName(st.ID))
	if err != nil {
		return c.failTask(ctx, st, "start the worker agent", isTerminal(err), err)
	}
	if name == "" {
		// Herdr reported no name (older response shape): keep the derived one.
		name = WorkerAgentName(st.ID)
	}
	st, err = c.record(ctx, st, func(record *task.Task) error {
		record.AgentName = name
		return nil
	}, "record the worker agent name")
	if err != nil {
		return c.failTask(ctx, st, "record the worker agent name", true, err)
	}

	st, err = c.recordAgentStart(ctx, st, paneID)
	if err != nil {
		return st, err
	}
	return st, nil
}

// recordAgentStart waits for Herdr's session reference and the extension's
// handshake. The session is persisted through the store before the handshake
// can succeed, so the extension resolves its role against a recorded binding.
func (c *Coordinator) recordAgentStart(ctx context.Context, st task.Task, paneID string) (task.Task, error) {
	workspaces := nonEmpty(st.WorktreeWorkspaceID, st.CurrentWorkspaceID)
	use := st
	write := func(view agentView) error {
		apply := func(record *task.Task) error {
			// The first identity Herdr reports is the binding's session identity;
			// whichever form (id or session file) the bridge adopted first is kept.
			if strings.TrimSpace(record.OMPSessionID) == "" {
				record.OMPSessionID = view.Ref.Value
			}
			record.TerminalID = view.Agent.TerminalID
			record.CurrentWorkspaceID = view.Agent.WorkspaceID
			record.Phase = task.AgentStarted
			return nil
		}
		next, err := c.record(ctx, use, apply, "record the started agent")
		if err == nil {
			use = next
			return nil
		}
		// Another coordinator call — the bridge adopting the announced session
		// for this pane — may have recorded it first. The same session on the
		// same binding is the same fact, so re-apply on the stored revision
		// instead of reporting a conflict a human cannot act on.
		current, readErr := c.store.Task(ctx, use.ID)
		if readErr != nil {
			return err
		}
		next, err = c.record(ctx, current, apply, "record the started agent")
		if err != nil {
			return err
		}
		use = next
		return nil
	}
	if _, err := c.awaitAgentReady(ctx, paneID, st.LaunchPaneID, st.WorktreePath, workspaces, write); err != nil {
		return c.failTask(ctx, use, "confirm the worker's OMP session", isTerminal(err), err)
	}
	return use, nil
}

// paneForTask resolves the worker's pane from Herdr's authoritative snapshot
// when the recorded identity is missing, which happens when a create answered
// ambiguously or the process stopped between two phases. It matches on the
// recorded workspace and the worktree checkout path, never on a label.
func (c *Coordinator) paneForTask(ctx context.Context, st task.Task) (string, error) {
	ctx, cancel := c.callContext(ctx)
	defer cancel()
	snapshot, err := c.herdr.Snapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("no pane is recorded for worktree %s and the snapshot failed: %w", st.WorktreePath, err)
	}
	var candidates []string
	for _, pane := range snapshot.Panes {
		if strings.TrimSpace(st.WorktreePath) == "" || !samePath(pane.Cwd, st.WorktreePath) {
			continue
		}
		if st.WorktreeWorkspaceID != "" && pane.WorkspaceID != st.WorktreeWorkspaceID {
			continue
		}
		candidates = append(candidates, pane.PaneID)
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("%w: no pane of workspace %q works in the recorded checkout %s",
			ErrAgentChanged, st.WorktreeWorkspaceID, st.WorktreePath)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

// submitBrief persists an inspection marker before submitting the brief. If
// Herdr accepts the prompt but the phase write fails, the marker is the durable
// reason a later resume must inspect and adjudicate rather than submit again.
func (c *Coordinator) submitBrief(ctx context.Context, st task.Task) (task.Task, error) {
	paneID := st.PublicPaneID
	if strings.TrimSpace(paneID) == "" {
		paneID = st.LaunchPaneID
	}
	if strings.TrimSpace(paneID) == "" {
		return c.failTask(ctx, st, "submit the brief", true,
			fmt.Errorf("no pane is recorded for the worker, so the brief cannot be addressed"))
	}
	marker, err := c.newBriefSubmissionMarker(st.Brief)
	if err != nil {
		return c.refuseBriefBeforeSend(ctx, st, "prepare a durable brief attempt before sending it", err)
	}
	pending, err := c.record(ctx, st, func(next *task.Task) error {
		if next.Phase != task.AgentStarted {
			return fmt.Errorf("%w: task %s moved to %s before its brief was submitted",
				state.ErrConflict, next.ID, next.Phase)
		}
		if next.Blocker != "" {
			return fmt.Errorf("%w: task %s already has a blocker", ErrBlocked, next.ID)
		}
		next.Blocker = marker
		return nil
	}, "record the brief attempt before submission")
	if err != nil {
		return c.refuseBriefBeforeSend(ctx, st, "record the durable brief attempt before sending it", err)
	}
	// The startup check is a snapshot: the pane can change before the brief is
	// sent. Reconfirm readiness, then prove it is still the session this task
	// recorded. This also completes an interrupted AgentStarted handshake without
	// relying on an unconfirmed same-form guess.
	workspaces := nonEmpty(pending.WorktreeWorkspaceID, pending.CurrentWorkspaceID)
	if _, err := c.awaitAgentReady(ctx, paneID, pending.LaunchPaneID, pending.WorktreePath, workspaces, nil); err != nil {
		return c.failTask(ctx, pending, "confirm the worker session before submitting the brief", isTerminal(err), err)
	}
	if _, _, err := c.workerOccupant(ctx, pending); err != nil {
		return c.failTask(ctx, pending, "verify the worker session before submitting the brief", isTerminal(err), err)
	}

	callCtx, cancel := c.callContext(ctx)
	_, err = c.herdr.AgentPrompt(callCtx, paneID, st.Brief)
	cancel()
	if err == nil {
		submitted, recordErr := c.record(ctx, pending, func(next *task.Task) error {
			if next.Phase != task.AgentStarted || next.Blocker != marker {
				return fmt.Errorf("%w: task %s no longer carries the brief attempt being recorded",
					state.ErrConflict, next.ID)
			}
			next.Phase = task.PromptSubmitted
			next.Blocker = ""
			return nil
		}, "record the submitted brief")
		if recordErr != nil {
			releaseErr := c.store.ReleaseStep(ctx, pending.ID, c.owner)
			message := fmt.Sprintf("Herdr accepted the brief on pane %s, but task %s could not record prompt_submitted; inspect the durable attempt and use orch_adjudicate_dispatch before retrying",
				paneID, pending.ID)
			if releaseErr != nil {
				return pending, fmt.Errorf("%s; releasing the completed prompt claim also failed: %v: %w",
					message, releaseErr, recordErr)
			}
			return pending, fmt.Errorf("%s: %w", message, recordErr)
		}
		return submitted, nil
	}
	if errors.Is(err, herdr.ErrAmbiguous) {
		return c.failTask(ctx, pending, "submit the brief", true,
			fmt.Errorf("%w: the brief on pane %s may already have been delivered; inspect `agent read %s` before deciding: %w",
				ErrAmbiguousStep, paneID, paneID, err))
	}
	if isNonDelivery(err) {
		return c.failTask(ctx, pending, "submit the brief", false,
			fmt.Errorf("agent prompt on pane %s was refused before any input was written: %w", paneID, err))
	}
	return c.failTask(ctx, pending, "submit the brief", true,
		fmt.Errorf("%w: agent prompt on pane %s failed without proof that nothing was delivered: %w",
			ErrAmbiguousStep, paneID, err))
}

func (c *Coordinator) refuseBriefBeforeSend(ctx context.Context, st task.Task, operation string, cause error) (task.Task, error) {
	releaseErr := c.store.ReleaseStep(ctx, st.ID, c.owner)
	if releaseErr != nil {
		return st, fmt.Errorf("%s (no Herdr prompt was sent); releasing the step claim also failed: %v: %w",
			operation, releaseErr, cause)
	}
	return st, fmt.Errorf("%s (no Herdr prompt was sent): %w", operation, cause)
}

// isNonDelivery reports a Herdr refusal that provably wrote no input: a blocked
// agent and a missing target are documented to be rejected before any bytes.
func isNonDelivery(err error) bool {
	var cliErr *herdr.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	switch cliErr.Code {
	case herdr.CodeAgentBlocked, "agent_not_found", "pane_not_found":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// resolving an unresolved brief submission
// ---------------------------------------------------------------------------

// Errors specific to the brief adjudication. They live here rather than with the
// other sentinels because this surface owns them.
var (
	// ErrNotAdjudicable reports a task that carries no unresolved brief submission
	// to decide, so nothing may be recorded for it.
	ErrNotAdjudicable = errors.New("coord: the task has no unresolved brief submission")
	// ErrEvidenceChanged reports that what a user inspected is not what the task
	// records now, so the decision cannot be applied to it.
	ErrEvidenceChanged = errors.New("coord: the inspected evidence no longer matches")
	// ErrStepInFlight reports that another caller holds the durable claim for the
	// task's step, so this caller issued no side effect at all.
	ErrStepInFlight = errors.New("coord: another caller is running this task's step")
)

// BriefDecision is a user's adjudication of an unresolved brief submission: the
// one state a submission leaves which nothing else can resolve.
type BriefDecision string

const (
	// BriefDelivered records that the brief did reach the worker. The task moves to
	// prompt_submitted without another submission.
	BriefDelivered BriefDecision = "delivered"
	// BriefNotDelivered records that the user confirmed the brief never reached the
	// worker. The blocker is cleared and the recorded phase resumes once.
	BriefNotDelivered BriefDecision = "not_delivered"
)

const briefSubmissionPendingPrefix = "brief submission pending: "

// BriefEvidence is what a user was shown and answered about: the worker's pane and
// the digest of the terminal text the primary displayed for that pane. The
// coordinator re-reads the same pane and requires the same digest, so a decision
// can never be applied to a terminal that has moved on.
type BriefEvidence struct {
	PaneID string `json:"paneId"`
	Digest string `json:"digest"`
}

// BriefInspectionParams is the brief_inspection request body: the task to inspect.
type BriefInspectionParams struct {
	TaskID string `json:"taskId"`
}

// AdjudicateBriefParams is the adjudicate_brief request body: the task, the
// revision the user inspected, the outcome they confirmed and the evidence handle
// the inspection returned. All four are required: the decision is made about that
// exact revision and that exact text.
type AdjudicateBriefParams struct {
	TaskID           string `json:"taskId"`
	ExpectedRevision int64  `json:"expectedRevision"`
	Decision         string `json:"decision"`
	PaneID           string `json:"paneId"`
	Digest           string `json:"digest"`
}

// BriefInspectionResult is the evidence a primary shows a user before deciding
// about an unresolved brief submission. It is an additive wire result: the task,
// the pane it read, the revision the decision must be tied to, the digest of the
// text shown, the text itself, and the two outcomes the coordinator accepts.
type BriefInspectionResult struct {
	TaskID     string   `json:"taskId"`
	ProjectKey string   `json:"projectKey"`
	Title      string   `json:"title"`
	Phase      string   `json:"phase"`
	Blocker    string   `json:"blocker"`
	PaneID     string   `json:"paneId"`
	Revision   int64    `json:"revision"`
	Digest     string   `json:"digest"`
	Terminal   string   `json:"terminal"`
	Decisions  []string `json:"decisions"`
}

// BriefAdjudication is the additive result of a recorded adjudication: which
// outcome the user chose and the task as it now stands.
type BriefAdjudication struct {
	Decision string         `json:"decision"`
	Task     DispatchResult `json:"task"`
}

// BriefInspectionFor is BriefInspection for an already authorized primary, and it
// requires the task to belong to that primary's project: a primary adjudicates
// the workers it dispatched, never another project's.
func (c *Coordinator) BriefInspectionFor(ctx context.Context, projectKey, taskID string) (BriefInspectionResult, error) {
	st, err := c.adjudicableTask(ctx, projectKey, taskID)
	if err != nil {
		return BriefInspectionResult{}, err
	}
	evidence, text, err := c.inspectAmbiguousBrief(ctx, st)
	if err != nil {
		return BriefInspectionResult{}, err
	}
	return BriefInspectionResult{
		TaskID:     st.ID,
		ProjectKey: st.ProjectKey,
		Title:      st.Title,
		Phase:      string(st.Phase),
		Blocker:    st.Blocker,
		PaneID:     evidence.PaneID,
		Revision:   st.Revision,
		Digest:     evidence.Digest,
		Terminal:   text,
		Decisions:  []string{string(BriefDelivered), string(BriefNotDelivered)},
	}, nil
}

// AdjudicateBriefFor is AdjudicateBrief for an already authorized primary: the
// task must belong to that primary's project.
func (c *Coordinator) AdjudicateBriefFor(
	ctx context.Context,
	projectKey, taskID string,
	expectedRevision int64,
	decision BriefDecision,
	observed BriefEvidence,
) (BriefAdjudication, error) {
	st, err := c.adjudicableTask(ctx, projectKey, taskID)
	if err != nil {
		return BriefAdjudication{}, err
	}
	decided, err := c.adjudicate(ctx, st, expectedRevision, decision, observed)
	if err != nil {
		return BriefAdjudication{}, err
	}
	return BriefAdjudication{Decision: string(decision), Task: resultOf(decided)}, nil
}

// adjudicableTask reads one task and requires it to belong to the given project
// and to carry the unresolved brief blocker.
func (c *Coordinator) adjudicableTask(ctx context.Context, projectKey, taskID string) (task.Task, error) {
	st, err := c.store.Task(ctx, strings.TrimSpace(taskID))
	if err != nil {
		return task.Task{}, err
	}
	if key := strings.TrimSpace(projectKey); key != "" && st.ProjectKey != key {
		return task.Task{}, fmt.Errorf("%w: task %s belongs to %q, not to this primary's project %q",
			ErrNotAdjudicable, st.ID, st.ProjectKey, key)
	}
	if !IsAmbiguousBriefTask(st) {
		return task.Task{}, fmt.Errorf("%w: task %s is at phase %s with no unresolved brief submission",
			ErrNotAdjudicable, st.ID, st.Phase)
	}
	return st, nil
}

// IsAmbiguousBriefTask reports whether a task carries the exact blocker that
// requires inspection before its brief can be retried. It accepts only this task's
// well-formed pre-send marker or its own ambiguity failure, never another blocker.
func IsAmbiguousBriefTask(st task.Task) bool {
	if st.Phase != task.AgentStarted || strings.TrimSpace(st.Blocker) == "" {
		return false
	}
	if isBriefSubmissionPending(st.Blocker) {
		return true
	}
	prefix := fmt.Sprintf("task %s could not submit the brief: ", st.ID)
	return strings.HasPrefix(st.Blocker, prefix) && strings.Contains(st.Blocker, ErrAmbiguousStep.Error())
}

func (c *Coordinator) newBriefSubmissionMarker(brief string) (string, error) {
	nonce, err := c.attemptNonce()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte("brief\x00" + brief))
	return briefSubmissionPendingPrefix + hex.EncodeToString(digest[:8]) + " " + nonce, nil
}

func isBriefSubmissionPending(marker string) bool {
	marker = strings.TrimSpace(marker)
	if !strings.HasPrefix(marker, briefSubmissionPendingPrefix) {
		return false
	}
	fields := strings.Fields(strings.TrimPrefix(marker, briefSubmissionPendingPrefix))
	if len(fields) != 2 {
		return false
	}
	for _, field := range fields {
		raw, err := hex.DecodeString(field)
		if err != nil || len(raw) != 8 {
			return false
		}
	}
	return true
}

// BriefInspection reads the evidence a user has to see before deciding: the
// text of the task's current worker pane, exactly as the primary will show it,
// together with the handle the decision must be made with. It is read-only, and it
// refuses anything that is not an unresolved brief submission.
func (c *Coordinator) BriefInspection(ctx context.Context, taskID string) (BriefEvidence, string, error) {
	st, err := c.store.Task(ctx, taskID)
	if err != nil {
		return BriefEvidence{}, "", err
	}
	if !IsAmbiguousBriefTask(st) {
		return BriefEvidence{}, "", fmt.Errorf("%w: task %s is at phase %s with no unresolved brief submission",
			ErrNotAdjudicable, st.ID, st.Phase)
	}
	return c.inspectAmbiguousBrief(ctx, st)
}

// inspectAmbiguousBrief reads the evidence for a task already known to carry the
// unresolved brief blocker.
func (c *Coordinator) inspectAmbiguousBrief(ctx context.Context, st task.Task) (BriefEvidence, string, error) {
	paneID := strings.TrimSpace(st.PublicPaneID)
	if paneID == "" {
		paneID = strings.TrimSpace(st.LaunchPaneID)
	}
	if paneID == "" {
		return BriefEvidence{}, "", fmt.Errorf("%w: task %s records no pane to inspect", ErrEvidenceChanged, st.ID)
	}
	text, err := c.readAgentText(ctx, paneID)
	if err != nil {
		return BriefEvidence{}, "", fmt.Errorf("%w: the worker's terminal could not be read: %v", ErrEvidenceChanged, err)
	}
	return BriefEvidence{PaneID: paneID, Digest: digestDialog(text)}, text, nil
}

// AdjudicateBrief records a user's decision about an unresolved brief submission.
//
// It is the only way out of that state, so it is narrow on purpose:
//
//   - the task must still be at AgentStarted with the specific ambiguous
//     submission blocker, at exactly the revision the user inspected (a compare
//     and swap, so a task that changed under the decision is refused);
//   - the recorded pane must still run the worker this task recorded (its name and
//     its native session identity), so a pane now held by somebody else refuses;
//   - the caller must present the pane and the digest it showed the user, and the
//     coordinator re-reads that pane and requires the same digest — a terminal that
//     moved on refuses;
//   - `BriefDelivered` records the submission (Phase = prompt_submitted) and sends
//     nothing; `BriefNotDelivered` clears only the ambiguity blocker and resumes the
//     recorded phase once.
//
// It never touches the task's Attention (another feature owns that) and it is never
// called automatically or from a hook: only a user's answer reaches it.
func (c *Coordinator) AdjudicateBrief(
	ctx context.Context,
	taskID string,
	expectedRevision int64,
	decision BriefDecision,
	observed BriefEvidence,
) (task.Task, error) {
	st, err := c.store.Task(ctx, taskID)
	if err != nil {
		return task.Task{}, err
	}
	if !IsAmbiguousBriefTask(st) {
		return task.Task{}, fmt.Errorf("%w: task %s is at phase %s with no unresolved brief submission",
			ErrNotAdjudicable, st.ID, st.Phase)
	}
	return c.adjudicate(ctx, st, expectedRevision, decision, observed)
}

// adjudicate applies one decision to a task already known to carry the unresolved
// brief blocker.
// It is the only way out of that state, so it is narrow on purpose:
func (c *Coordinator) adjudicate(
	ctx context.Context,
	st task.Task,
	expectedRevision int64,
	decision BriefDecision,
	observed BriefEvidence,
) (task.Task, error) {
	switch decision {
	case BriefDelivered, BriefNotDelivered:
	default:
		return task.Task{}, fmt.Errorf("%w: %q is not a brief decision", ErrInvalidRequest, decision)
	}

	// Provenance: the pane the user looked at must be this task's worker, and the
	// worker must still be the process the task recorded.
	paneID := strings.TrimSpace(st.PublicPaneID)
	if paneID == "" {
		paneID = strings.TrimSpace(st.LaunchPaneID)
	}
	if paneID == "" || observed.PaneID != paneID {
		return st, fmt.Errorf("%w: task %s records pane %q, not the inspected %q",
			ErrEvidenceChanged, st.ID, paneID, observed.PaneID)
	}
	if err := c.confirmWorkerIdentity(ctx, st, paneID); err != nil {
		return st, err
	}
	text, err := c.readAgentText(ctx, paneID)
	if err != nil {
		return st, fmt.Errorf("%w: the worker's terminal could not be re-read: %v", ErrEvidenceChanged, err)
	}
	if digest := digestDialog(text); observed.Digest == "" || digest != observed.Digest {
		return st, fmt.Errorf("%w: pane %s shows something other than what was inspected (digest %s, inspected %s)",
			ErrEvidenceChanged, paneID, digest, observed.Digest)
	}

	// Apply on the inspected revision, re-checking the state inside the compare and
	// swap so a task that changed under the decision is refused rather than adjusted.
	// The decision, the claim retirement and the note that records it are one fact,
	// so they are one transaction: a note that cannot be stored must leave the task
	// exactly as adjudicable as it was, not decided with a silent hole where the
	// audit trail and the resume should be.
	claimedPending := isBriefSubmissionPending(st.Blocker)
	if claimedPending {
		took, err := c.store.ClaimStep(ctx, st.ID, st.Phase, c.owner, c.opts.StepLease)
		if err != nil {
			return st, err
		}
		if !took {
			return st, fmt.Errorf("%w: task %s still has an active brief-submission claim; wait for it to finish before deciding",
				ErrStepInFlight, st.ID)
		}
	}
	summary := fmt.Sprintf("the user decided the brief for task %s was %s after inspecting pane %s",
		st.ID, decision, paneID)
	decided, err := c.store.RetireStepWithNote(ctx, st.ID, expectedRevision, func(next *task.Task) error {
		if !IsAmbiguousBriefTask(*next) {
			return fmt.Errorf("%w: task %s changed while it was being adjudicated", ErrNotAdjudicable, next.ID)
		}
		next.Blocker = ""
		if decision == BriefDelivered {
			next.Phase = task.PromptSubmitted
		}
		return nil
	}, inboxNote(st.ProjectKey, st.ID, "adjudication", summary, "recorded by the user"))
	if err != nil {
		if claimedPending {
			if releaseErr := c.store.ReleaseStep(ctx, st.ID, c.owner); releaseErr != nil {
				return st, fmt.Errorf("record the brief decision: %w; release the step claim: %v", err, releaseErr)
			}
		}
		return st, err
	}
	if decision == BriefDelivered {
		return decided, nil
	}
	// Not delivered: resume the recorded phase exactly once. The decision above
	// already retired the step's claim in the same transaction that cleared the
	// blocker, so no concurrent caller can take the step in between; the phase owns
	// the task's provisioning slot, so nothing has to be claimed again — and no
	// claim token is carried either: this path never reaches the worktree step that
	// could give a claim back (a task at agent_started resumes with a brief), and a
	// release would be refused by the phase fence in any case.
	provisioned, err := c.provision(ctx, decided, slotClaim{})
	if err != nil {
		return provisioned, err
	}
	return provisioned, nil
}

// confirmWorkerIdentity requires the pane to still run the worker this task
// recorded: the recorded agent name (when there is one) and the recorded native
// session identity, compared in either form (an id or a session file path).
func (c *Coordinator) confirmWorkerIdentity(ctx context.Context, st task.Task, paneID string) error {
	view, err := c.agentGet(ctx, paneID)
	if err != nil {
		return fmt.Errorf("%w: the worker on pane %s could not be read: %v", ErrEvidenceChanged, paneID, err)
	}
	if view.RefErr != nil {
		return fmt.Errorf("%w: pane %s reports a session this build cannot read: %v", ErrEvidenceChanged, paneID, view.RefErr)
	}
	if name := strings.TrimSpace(st.AgentName); name != "" && !strings.EqualFold(strings.TrimSpace(view.Agent.Name), name) {
		return fmt.Errorf("%w: pane %s now runs %q, not the recorded worker %q",
			ErrEvidenceChanged, paneID, view.Agent.Name, name)
	}
	if recorded := strings.TrimSpace(st.OMPSessionID); recorded != "" {
		if !view.HasRef || !sameSessionIdentity(recorded, view.Ref.Value) {
			return fmt.Errorf("%w: pane %s now reports session %q, not the recorded %q",
				ErrEvidenceChanged, paneID, view.Ref.Value, recorded)
		}
	}
	return nil
}

// sameSessionIdentity compares a recorded session identity with one Herdr reports:
// the same token, or the same file when either side names a path. Both values come
// from Herdr, so an exact match is the normal case and the path form only tolerates
// separator and case differences a move or a restore can introduce.
func sameSessionIdentity(recorded, reported string) bool {
	recorded, reported = strings.TrimSpace(recorded), strings.TrimSpace(reported)
	if recorded == "" || reported == "" {
		return false
	}
	if strings.EqualFold(recorded, reported) {
		return true
	}
	if strings.ContainsAny(recorded, `/\`) || strings.ContainsAny(reported, `/\`) {
		return samePath(recorded, reported)
	}
	return false
}

// readAgentText reads the worker's visible terminal, the same way the primary's
// approval surface does: a passive read that works for a blocked full-screen agent.
func (c *Coordinator) readAgentText(ctx context.Context, paneID string) (string, error) {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	return c.herdr.AgentRead(callCtx, paneID, herdr.AgentReadOptions{Source: "visible"})
}

// ---------------------------------------------------------------------------
// durable outcomes
// ---------------------------------------------------------------------------

// record applies one update to a task's facts on the revision the store holds at
// the moment of the write.
//
// A step must build on the current revision, never on one captured before an
// intervening write: another coordinator call and the bridge (which adopts an
// announced session for a pane) both write the same task, so a write that carried
// a stale revision would fail even though nothing is wrong. The retry re-reads and
// re-applies, which is safe because every mutation here is a plain field
// assignment or a fill-if-empty.
func (c *Coordinator) record(ctx context.Context, st task.Task, mutate func(*task.Task) error, what string) (task.Task, error) {
	var lastErr error
	for range recordAttempts {
		current, err := c.store.Task(ctx, st.ID)
		if err != nil {
			return st, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
		}
		updated, err := c.store.UpdateTask(ctx, st.ID, current.Revision, mutate)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, state.ErrConflict) {
			return st, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
		}
		lastErr = err
	}
	return st, fmt.Errorf("task %s: %s: %w", st.ID, what, lastErr)
}

// recordStepFailure records a failed step on the task it was read from, releasing
// this owner's matching step claim only when the task still stands on the phase the
// step ran. It retries a conflicting write, because another writer that only bumped
// the revision (a report, a question) must not lose the failure — but every attempt
// refuses a task that advanced past the failed phase, so a concurrent resume that
// took the step can never have a stale failure recorded onto the phase it reached.
func (c *Coordinator) recordStepFailure(
	ctx context.Context,
	st task.Task,
	failure *StepError,
	release bool,
	mutate func(*task.Task) error,
	what string,
) (task.Task, error) {
	var lastErr error
	for range recordAttempts {
		current, err := c.store.Task(ctx, st.ID)
		if err != nil {
			return st, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
		}
		if current.Phase != st.Phase {
			return st, fmt.Errorf("task %s: %s: %w", st.ID, what, fmt.Errorf(
				"%w: task %s advanced from %s to %s while its failed step was being recorded",
				state.ErrConflict, st.ID, st.Phase, current.Phase))
		}
		// The phase is checked again inside the mutation, because the assertion above
		// is only against the snapshot this attempt read.
		guard := func(next *task.Task) error {
			if next.Phase != st.Phase {
				return fmt.Errorf("%w: task %s advanced from %s to %s while its failed step was being recorded",
					state.ErrConflict, next.ID, st.Phase, next.Phase)
			}
			return mutate(next)
		}
		updated, _, err := c.store.RecordStepFailure(ctx, st.ID, current.Revision, guard, c.owner, st.Phase, release)
		if err == nil {
			return updated, nil
		}
		if errors.Is(err, state.ErrStepNotHeld) {
			// The step is another caller's now, so retrying this write can only put a
			// failure onto state that caller owns.
			return st, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
		}
		if !errors.Is(err, state.ErrConflict) {
			return st, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
		}
		lastErr = err
	}
	return st, fmt.Errorf("task %s: %s: %w", st.ID, what, lastErr)
}

// StepError reports a dispatch step that failed, with the facts a human needs in
// order to act: the task, the step and phase it stopped at, the resources it
// already recorded, and the cause. The coordinator returns it for every
// provisioning failure, so a caller never has to read this package or the state
// database to find out what happened, and it always says that the task is
// resumable rather than something to retry blindly.
type StepError struct {
	TaskID string
	Step   string
	Phase  task.Phase
	Facts  task.Task
	Cause  error
	// Stalled marks a failure the caller must not retry by dispatching again.
	Stalled bool
	// Ambiguous marks a step whose outcome could not be identified. Its advice is
	// to inspect the recorded pane and the task before any retry.
	Ambiguous bool
}

// Error renders one line naming the step, the cause and the recorded resources.
func (e *StepError) Error() string {
	var recorded []string
	for _, fact := range []struct{ name, value string }{
		{"branch", e.Facts.Branch},
		{"worktree", e.Facts.WorktreePath},
		{"workspace", e.Facts.WorktreeWorkspaceID},
		{"pane", e.Facts.PublicPaneID},
		{"agent", e.Facts.AgentName},
		{"session", e.Facts.OMPSessionID},
	} {
		if strings.TrimSpace(fact.value) != "" {
			recorded = append(recorded, fact.name+" "+fact.value)
		}
	}
	detail := "phase " + string(e.Phase)
	if len(recorded) > 0 {
		detail += ", " + strings.Join(recorded, ", ")
	}
	advice := e.advice()
	return fmt.Sprintf("task %s could not %s: %v (task %s; %s)", e.TaskID, e.Step, e.Cause, detail, advice)
}

// advice is what the caller should do next.
//
// An ambiguous step is never resumed blindly: its outcome is unknown, so a human
// inspects the recorded pane and the task first. That matters most for an
// ambiguous brief, where resuming would resubmit a brief that may already have
// been delivered — the guidance must not contradict the blocker it accompanies.
// A definite failure keeps the resumable advice, because resuming it re-runs the
// recorded phase and repeats nothing.
func (e *StepError) advice() string {
	if e.Ambiguous {
		target := "the recorded pane"
		if strings.TrimSpace(e.Facts.PublicPaneID) != "" {
			target = "pane " + e.Facts.PublicPaneID
		}
		return fmt.Sprintf("the outcome is unknown: inspect %s and the task before any retry, and do not resubmit this step's input until delivery is determined", target)
	}
	if e.Stalled {
		return "the task is resumable: fix the cause and resume it from the recorded phase instead of dispatching the same work again"
	}
	return "the task is resumable: this step is retried from the recorded phase, and nothing was repeated"
}

// Unwrap keeps the cause reachable, so a caller still classifies with errors.Is
// and never matches this wrapper by accident.
func (e *StepError) Unwrap() error { return e.Cause }

// stepError wraps a provisioning failure with the task's recorded facts.
func stepError(st task.Task, step string, terminal bool, cause error) *StepError {
	return &StepError{
		TaskID:  st.ID,
		Step:    step,
		Phase:   st.Phase,
		Facts:   st,
		Cause:   cause,
		Stalled: terminal,
		// Ambiguity is decided by the cause: the call sites mark a step whose
		// outcome could not be identified with ErrAmbiguousStep, and Herdr's own
		// ambiguity classification means the same thing here.
		Ambiguous: errors.Is(cause, ErrAmbiguousStep) || errors.Is(cause, herdr.ErrAmbiguous),
	}
}

// PersistenceError reports that a task's failure could not be recorded.
//
// It keeps the whole action context: the step error (which step failed, the phase
// reached, the recorded worktree/workspace/pane/agent/session facts, the cause and
// the resume guidance) alongside the write failure. A caller that cannot find a
// durable reason still knows what happened and what to do, and the error still
// classifies as ErrPersistence.
type PersistenceError struct {
	// Step is the actionable failure that could not be stored.
	Step *StepError
	// Recording is the write failure that prevented it.
	Recording error
}

func (e *PersistenceError) Error() string {
	return fmt.Sprintf("%s; that failure could not be recorded (%v), so the task may show no reason",
		e.Step.Error(), e.Recording)
}

// Unwrap exposes the step error (and, through it, the underlying cause) and the
// write failure, so errors.Is and errors.As reach either of them.
func (e *PersistenceError) Unwrap() []error { return []error{e.Step, e.Recording} }

// Is classifies this as a persistence failure without hiding the step error's own
// classification.
func (e *PersistenceError) Is(target error) bool { return target == ErrPersistence }

// failTask records why provisioning stopped and returns the actionable error.
// Attention marks a failure that a retry repeats safely; a blocker marks an
// outcome a human must decide, and ResumeDispatch refuses to touch it.
//
// The write is retried on the freshly read revision: a step that failed must
// still leave durable attention naming the task, the phase, the recorded
// resources and the cause. If even that fails, the persistence failure is part of
// the returned error instead of being dropped.
func (c *Coordinator) failTask(ctx context.Context, st task.Task, step string, terminal bool, cause error) (task.Task, error) {
	// A definite failure provably wrote nothing, so its step claim goes back at
	// once and a resume may run now. A timeout, Herdr's ambiguity, and a response
	// this build cannot interpret all leave the outcome undecided: their claim is
	// kept until its lease expires, so a retry waits for live inspection instead of
	// racing a side effect that may already have happened.
	failure := stepError(st, step, terminal, cause)
	// The failure and the release of this owner's claim for that exact step are one
	// transaction: releasing first would let a concurrent resume take the step and
	// advance the task, after which this call would write a stale failure onto the
	// phase it reached. A definite failure releases the claim (a retry does not have
	// to wait out the lease); an undecided one keeps it until the lease expires.
	updated, err := c.recordStepFailure(ctx, st, failure, definiteFailure(cause), func(next *task.Task) error {
		if isBriefSubmissionPending(st.Blocker) && next.Blocker == st.Blocker {
			next.Blocker = ""
		}
		if terminal {
			next.Blocker = failure.Error()
			return nil
		}
		next.Attention = failure.Error()
		return nil
	}, "record the dispatch failure")
	if err != nil {
		return st, &PersistenceError{Step: failure, Recording: err}
	}
	return updated, failure
}

// inboxNote builds the durable note for one task fact. The id is derived from the
// fact itself, so recording the same fact twice stores it once.
func inboxNote(projectKey, taskID, kind, summary, delivery string) task.Inbox {
	sum := sha256.Sum256([]byte(kind + "\x00" + taskID + "\x00" + summary))
	return task.Inbox{
		ID:         kind + ":" + taskID + ":" + hex.EncodeToString(sum[:6]),
		ProjectKey: projectKey,
		TaskID:     taskID,
		Kind:       kind,
		Summary:    summary,
		Delivery:   delivery,
	}
}

// notify records one deduplicated inbox note for the project, so a failure that
// has no primary session to report to is still durable and visible.
func (c *Coordinator) notify(ctx context.Context, projectKey, taskID, kind, summary, delivery string) error {
	if strings.TrimSpace(projectKey) == "" {
		return nil
	}
	note := inboxNote(projectKey, taskID, kind, summary, delivery)
	if _, err := c.store.AddInbox(ctx, note); err != nil {
		return fmt.Errorf("record the %s note for %q: %w", kind, projectKey, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// read projections
// ---------------------------------------------------------------------------

// project reads one bound project.
func (c *Coordinator) project(ctx context.Context, key string) (task.Project, error) {
	project, err := c.store.Project(ctx, strings.TrimSpace(key))
	switch {
	case errors.Is(err, state.ErrNotFound):
		return task.Project{}, fmt.Errorf("%w: %q is not a bound project; bind the repository first", ErrProjectUnknown, key)
	case err != nil:
		return task.Project{}, err
	case strings.TrimSpace(project.LaunchPaneID) == "":
		return task.Project{}, fmt.Errorf("%w: project %q records no primary pane", ErrProjectUnknown, key)
	}
	return project, nil
}

// projectTasks reads every task of one project through the store.
//
// The coordinator needs the project-wide view to enforce the concurrency budget,
// to wait for dependencies and to keep two workers from reserving the same
// paths; the store owns the schema, so the listing is a store read like any
// other (and it is read-only: internal/state is the only writer of facts).
func (c *Coordinator) projectTasks(ctx context.Context, projectKey string) ([]task.Task, error) {
	tasks, err := c.store.Tasks(ctx, projectKey)
	if err != nil {
		return nil, fmt.Errorf("read the tasks of %q: %w", projectKey, err)
	}
	return tasks, nil
}
