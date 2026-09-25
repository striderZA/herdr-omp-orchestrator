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
}

// DispatchFor runs one dispatch for an already authorized primary. The project
// key comes from the verified caller, never from the request body.
func (c *Coordinator) DispatchFor(ctx context.Context, projectKey string, params DispatchParams) (DispatchResult, error) {
	st, err := c.Dispatch(ctx, projectKey, params.Title, params.Brief, params.Checks,
		params.Dependencies, params.ReservedPaths, params.BaseRevision)
	return resultOf(st), err
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
	}
}

// Dispatch records a new task and, when the project's gates allow it, takes the
// task through worktree creation, agent start, the readiness handshake and the
// brief submission.
//
// A request that is legal but gated — a dependency that is not reviewed yet, or
// a full concurrency budget — returns the recorded task in its queued phase
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
	project, err := c.project(ctx, projectKey)
	if err != nil {
		return task.Task{}, err
	}
	request, err := newDispatchRequest(title, brief, checks, dependencies, reservedPaths)
	if err != nil {
		return task.Task{}, err
	}
	base, err := c.committedSHA(ctx, project.Key, baseRevision)
	if err != nil {
		return task.Task{}, err
	}
	existing, err := c.projectTasks(ctx, project.Key)
	if err != nil {
		return task.Task{}, err
	}
	if err := checkDependencies(existing, request.dependencies); err != nil {
		return task.Task{}, err
	}
	if err := checkReservations(existing, request.reservedPaths); err != nil {
		return task.Task{}, err
	}

	st, err := c.createTask(ctx, project.Key, request, base)
	if err != nil {
		return task.Task{}, err
	}
	if ok, _ := c.dispatchable(existing, st); !ok {
		return st, nil
	}
	return c.provision(ctx, st)
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
	if st.Attention != "" {
		cleared, err := c.record(ctx, st, func(next *task.Task) error {
			next.Attention = ""
			return nil
		}, "clear the retryable attention")
		if err != nil {
			return st, err
		}
		st = cleared
	}
	return c.provision(ctx, st)
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

// dispatchable reports whether a task may take provisioning resources now.
func (c *Coordinator) dispatchable(existing []task.Task, st task.Task) (bool, string) {
	if reason := unsatisfiedDependency(existing, st); reason != "" {
		return false, reason
	}
	if occupied := occupiedCount(existing, st.ID); occupied >= c.opts.Concurrency {
		return false, fmt.Sprintf("the concurrency budget of %d is full", c.opts.Concurrency)
	}
	return true, ""
}

// occupiedCount counts the tasks that hold provisioning or running resources,
// ignoring the task itself. A task keeps its slot until the primary records a
// disposition for it, because its pane and checkout still exist.
func occupiedCount(existing []task.Task, excludeID string) int {
	count := 0
	for _, other := range existing {
		if other.ID == excludeID || !holdsResources(other) {
			continue
		}
		count++
	}
	return count
}

// holdsResources reports whether a task currently owns a worker pane and
// checkout.
func holdsResources(st task.Task) bool {
	if st.Disposition != "" {
		return false
	}
	switch st.Phase {
	case task.WorktreeCreated, task.AgentStarted, task.PromptSubmitted:
		return true
	default:
		return false
	}
}

// unsatisfiedDependency names the first dependency that is not reviewed yet.
func unsatisfiedDependency(existing []task.Task, st task.Task) string {
	for _, dependency := range st.Dependencies {
		dep, ok := findTask(existing, dependency)
		if !ok {
			return fmt.Sprintf("dependency %s is not recorded in this project", dependency)
		}
		switch task.StageOf(dep) {
		case task.ReadyForReview, task.Done:
		default:
			return fmt.Sprintf("dependency %s is %s, not reviewed yet", dependency, task.StageOf(dep))
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

// checkReservations refuses a request whose reserved paths overlap a task that
// already holds them. Two workers never own the same file or directory.
func checkReservations(existing []task.Task, reserved []string) error {
	for _, other := range existing {
		if other.Disposition != "" || len(other.ReservedPaths) == 0 {
			continue
		}
		for _, mine := range reserved {
			for _, theirs := range other.ReservedPaths {
				if overlap, ok := pathsOverlap(mine, theirs); ok {
					return fmt.Errorf("%w: task %s reserved %q (requested %q)",
						ErrReservedConflict, other.ID, overlap, mine)
				}
			}
		}
	}
	return nil
}

// pathsOverlap reports whether one reservation covers the other: the same path,
// or one is a directory containing the other.
func pathsOverlap(a, b string) (string, bool) {
	a, b = filepath.ToSlash(a), filepath.ToSlash(b)
	if isCaseInsensitivePath() {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	switch {
	case a == b:
		return a, true
	case strings.HasPrefix(b, a+"/"):
		return a, true
	case strings.HasPrefix(a, b+"/"):
		return b, true
	default:
		return "", false
	}
}

func isCaseInsensitivePath() bool {
	return filepath.Separator == '\\'
}

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

// createTask records one task in its intent phase, retrying only the identity
// collision, which is the one failure that touches nothing.
func (c *Coordinator) createTask(ctx context.Context, projectKey string, request dispatchRequest, base string) (task.Task, error) {
	var lastErr error
	for range idAttempts {
		id, branch, err := c.newIdentity()
		if err != nil {
			return task.Task{}, err
		}
		st := task.Task{
			ID:            id,
			ProjectKey:    projectKey,
			Title:         request.title,
			Brief:         request.brief,
			Checks:        request.checks,
			Dependencies:  request.dependencies,
			ReservedPaths: request.reservedPaths,
			BaseRevision:  base,
			Branch:        branch,
			Phase:         task.Intent,
		}
		lastErr = c.store.CreateTask(ctx, st)
		if lastErr == nil {
			// Re-read so every later compare-and-swap uses the stored revision.
			created, readErr := c.store.Task(ctx, st.ID)
			if readErr != nil {
				return task.Task{}, fmt.Errorf("read the recorded task %s: %w", st.ID, readErr)
			}
			return created, nil
		}
		if !errors.Is(lastErr, state.ErrConflict) {
			return task.Task{}, fmt.Errorf("record task intent for %q: %w", projectKey, lastErr)
		}
	}
	return task.Task{}, fmt.Errorf("record task intent for %q: %w", projectKey, lastErr)
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

// provision advances a task as far as its recorded phase allows. Each step is
// idempotent on its own: it inspects the recorded facts and authoritative Herdr
// state before acting.
func (c *Coordinator) provision(ctx context.Context, st task.Task) (task.Task, error) {
	if st.Phase == task.PromptSubmitted {
		return st, nil
	}
	if st.Phase == task.Intent {
		next, err := c.createWorktree(ctx, st)
		if err != nil {
			return next, err
		}
		st = next
	}
	if st.Phase == task.WorktreeCreated {
		next, err := c.startWorker(ctx, st)
		if err != nil {
			return next, err
		}
		st = next
	}
	if st.Phase == task.AgentStarted {
		return c.submitBrief(ctx, st)
	}
	return st, fmt.Errorf("%w: task %s is at unknown phase %q", ErrInvalidRequest, st.ID, st.Phase)
}

// createWorktree creates the isolated checkout for a task and records the exact
// returned identities.
//
// Before asking Herdr to create anything it lists the repository's worktrees:
// a checkout for this task's unique branch can only be this task's, so a crash
// between the CLI answering and the phase being recorded cannot produce a
// second checkout.
func (c *Coordinator) createWorktree(ctx context.Context, st task.Task) (task.Task, error) {
	if listed, err := c.findWorktree(ctx, st.ProjectKey, st.Branch); err != nil {
		return c.failTask(ctx, st, isTerminal(err),
			fmt.Errorf("task %s: the repository's worktrees could not be read before creating %q: %w", st.ID, st.Branch, err))
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
			return c.failTask(ctx, st, true,
				fmt.Errorf("%w: task %s asked for branch %q but `worktree create` answered with %q",
					ErrAgentChanged, st.ID, st.Branch, created.Branch))
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
			return c.failTask(ctx, st, true, recordErr)
		}
		return next, nil
	}

	if !errors.Is(err, herdr.ErrAmbiguous) {
		return c.failTask(ctx, st, false,
			fmt.Errorf("task %s: worktree create for root %s branch %q base %s failed: %w",
				st.ID, st.ProjectKey, st.Branch, st.BaseRevision, err))
	}
	// The create may have taken effect. Inspect the repository before deciding.
	listed, listErr := c.findWorktree(ctx, st.ProjectKey, st.Branch)
	if listErr != nil {
		return c.failTask(ctx, st, true,
			fmt.Errorf("task %s: worktree create for root %s branch %q base %s is ambiguous and the worktree listing failed: %v: %w",
				st.ID, st.ProjectKey, st.Branch, st.BaseRevision, listErr, err))
	}
	if listed == nil {
		return c.failTask(ctx, st, true,
			fmt.Errorf("task %s: worktree create for root %s branch %q base %s is ambiguous and `worktree list` does not list that branch: %w",
				st.ID, st.ProjectKey, st.Branch, st.BaseRevision, err))
	}
	next, adoptErr := c.adoptWorktree(ctx, st, listed)
	if adoptErr != nil {
		return c.failTask(ctx, st, true, adoptErr)
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
			return c.failTask(ctx, st, true, fmt.Errorf("task %s: %w", st.ID, err))
		}
		next, err := c.record(ctx, st, func(record *task.Task) error {
			record.LaunchPaneID = resolved
			record.PublicPaneID = resolved
			return nil
		}, "record the worker pane")
		if err != nil {
			return c.failTask(ctx, st, true, err)
		}
		st = next
		paneID = resolved
	}

	name, err := c.ensureAgent(ctx, paneID, WorkerAgentName(st.ID))
	if err != nil {
		return c.failTask(ctx, st, isTerminal(err),
			fmt.Errorf("task %s: pane %s: %w", st.ID, paneID, err))
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
		return c.failTask(ctx, st, true, err)
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
	if _, err := c.awaitAgentReady(ctx, paneID, st.WorktreePath, workspaces, write); err != nil {
		return c.failTask(ctx, use, isTerminal(err),
			fmt.Errorf("task %s: pane %s: %w", st.ID, paneID, err))
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

// submitBrief submits the brief once, as a single argv element. An ambiguous
// answer becomes a blocker: the brief may already be the worker's turn, so it
// is never sent again.
func (c *Coordinator) submitBrief(ctx context.Context, st task.Task) (task.Task, error) {
	paneID := st.PublicPaneID
	if strings.TrimSpace(paneID) == "" {
		paneID = st.LaunchPaneID
	}
	if strings.TrimSpace(paneID) == "" {
		return c.failTask(ctx, st, true,
			fmt.Errorf("task %s: no pane is recorded for the worker, so the brief cannot be addressed", st.ID))
	}
	callCtx, cancel := c.callContext(ctx)
	_, err := c.herdr.AgentPrompt(callCtx, paneID, st.Brief)
	cancel()
	if err == nil {
		return c.record(ctx, st, func(next *task.Task) error {
			next.Phase = task.PromptSubmitted
			return nil
		}, "record the submitted brief")
	}
	if errors.Is(err, herdr.ErrAmbiguous) {
		return c.failTask(ctx, st, true,
			fmt.Errorf("task %s: agent prompt on pane %s is ambiguous, so the brief may already have been delivered; inspect `agent read %s` before deciding: %w",
				st.ID, paneID, paneID, err))
	}
	if isNonDelivery(err) {
		return c.failTask(ctx, st, false,
			fmt.Errorf("task %s: agent prompt on pane %s was refused before any input was written: %w", st.ID, paneID, err))
	}
	return c.failTask(ctx, st, true,
		fmt.Errorf("task %s: agent prompt on pane %s failed without proof that nothing was delivered: %w", st.ID, paneID, err))
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
// durable outcomes
// ---------------------------------------------------------------------------

// record applies one compare-and-swap update and reports a failure to persist
// an acknowledged side effect.
func (c *Coordinator) record(ctx context.Context, st task.Task, mutate func(*task.Task) error, what string) (task.Task, error) {
	updated, err := c.store.UpdateTask(ctx, st.ID, st.Revision, mutate)
	if err != nil {
		return st, fmt.Errorf("task %s: %s: %w", st.ID, what, err)
	}
	return updated, nil
}

// failTask records why provisioning stopped. Attention marks a failure that a
// retry repeats safely; a blocker marks an outcome a human must decide, and
// ResumeDispatch refuses to touch it.
func (c *Coordinator) failTask(ctx context.Context, st task.Task, terminal bool, cause error) (task.Task, error) {
	message := cause.Error()
	updated, err := c.record(ctx, st, func(next *task.Task) error {
		if terminal {
			next.Blocker = message
			return nil
		}
		next.Attention = message
		return nil
	}, "record the dispatch failure")
	if err != nil {
		return st, errors.Join(cause, err)
	}
	return updated, cause
}

// notify records one deduplicated inbox note for the project, so a failure that
// has no primary session to report to is still durable and visible.
func (c *Coordinator) notify(ctx context.Context, projectKey, taskID, kind, summary, delivery string) error {
	if strings.TrimSpace(projectKey) == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + taskID + "\x00" + summary))
	note := task.Inbox{
		ID:         kind + ":" + taskID + ":" + hex.EncodeToString(sum[:6]),
		ProjectKey: projectKey,
		TaskID:     taskID,
		Kind:       kind,
		Summary:    summary,
		Delivery:   delivery,
	}
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
