// Package coord binds a primary OMP session to one canonical Git repository and
// dispatches isolated worker tasks into Herdr-managed worktrees.
//
// # Persisted step machine
//
// Every dispatch persists its intent before the first side effect and records
// one named phase after each acknowledged side effect, so a retry after a crash
// or an ambiguous timeout resumes from the recorded phase instead of repeating
// work:
//
//	intent            task recorded; nothing done in Herdr yet
//	worktree_created  `worktree create` answered; checkout/workspace/pane recorded
//	agent_started     the pained OMP session is recorded and its `ready` handshake confirmed
//	prompt_submitted  `agent prompt` answered; the brief is now the agent's turn
//
// A retry never creates a second checkout, never starts a second OMP in a pane,
// and never re-sends a brief. When a Herdr call is ambiguous the coordinator
// inspects authoritativeHerde state (`worktree list`, `agent get`, the store's
// readiness handshake) and reconciles; an outcome it cannot identify stays
// actionable on the task instead of being guessed away.
//
// # Readiness
//
// A pane is recorded as both its launch-time and public identity before
// `agent start`, and the brief is only submitted after the extension announced
// `ready` for exactly the OMP session `agent get` reports for that pane.
package coord

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// Sentinel errors. Callers classify failures with errors.Is and read the
// message for the recorded identities.
var (
	// ErrInvalidRequest rejects a dispatch request before anything is written.
	ErrInvalidRequest = errors.New("coord: invalid request")
	// ErrNotGitRepository reports a directory that is not inside a Git work tree.
	ErrNotGitRepository = errors.New("coord: not a Git repository")
	// ErrProjectUnknown reports a dispatch for a project that was never bound.
	ErrProjectUnknown = errors.New("coord: unknown project")
	// ErrDifferentRepository reports a pane or workspace that belongs to another
	// repository than the one being bound.
	ErrDifferentRepository = errors.New("coord: different repository")
	// ErrBaseRevision reports a base revision that is not a committed hexadecimal
	// commit id of the project's repository.
	ErrBaseRevision = errors.New("coord: base revision is not a committed commit")
	// ErrReservedConflict reports reserved paths that overlap an active worker.
	ErrReservedConflict = errors.New("coord: reserved paths overlap an active worker")
	// ErrPrimaryNotLive reports a recorded primary whose OMP session is no longer
	// the live agent in its recorded pane.
	ErrPrimaryNotLive = errors.New("coord: the recorded primary is not live")
	// ErrAgentChanged reports a pane whose OMP session or provenance no longer
	// matches the recorded worker.
	ErrAgentChanged = errors.New("coord: the pane's OMP session changed")
	// ErrSessionUnavailable reports that Herdr published no native session
	// reference for a pane within the readiness window, so the announced
	// handshake cannot be matched yet. The integration reports session identity
	// asynchronously, so this is a retryable condition.
	ErrSessionUnavailable = errors.New("coord: Herdr reported no session reference for the pane")
	// ErrSessionRefUnsupported reports a reference Herdr published in a form this
	// build cannot read. Retrying cannot fix it: the shape is what it is, so a
	// human decides.
	ErrSessionRefUnsupported = errors.New("coord: Herdr reported a session reference this build cannot read")
	// ErrReadinessTimeout reports that the extension did not announce readiness
	// for the recorded session in time; no brief was submitted.
	ErrReadinessTimeout = errors.New("coord: the OMP extension did not announce readiness")
	// ErrBlocked reports a task that needs a human decision before it can be
	// resumed; resuming it would risk repeating an ambiguous side effect.
	ErrBlocked = errors.New("coord: task needs input")
	// ErrPersistence reports that a task's facts could not be written: the
	// coordinator could not leave the durable failure it must always leave.
	ErrPersistence = errors.New("coord: the task's facts could not be recorded")
	// ErrAmbiguousStep reports a provisioning step whose outcome could not be
	// identified from Herdr's state. A human inspects it; it is never repeated
	// blindly.
	ErrAmbiguousStep = errors.New("coord: the step's outcome could not be identified")
	// ErrHerdrUnavailable reports that the Herdr CLI could not be used at all.
	ErrHerdrUnavailable = errors.New("coord: Herdr is unavailable")
)

// Defaults for one coordinator.
const (
	// DefaultConcurrency is the first release's worker limit.
	DefaultConcurrency = 3
	// defaultCallTimeout bounds one Herdr CLI call.
	defaultCallTimeout = 20 * time.Second
	// defaultReadinessTimeout bounds the wait for the extension's `ready`
	// handshake; it must stay well inside the extension's own bridge timeout.
	defaultReadinessTimeout = 20 * time.Second
	// defaultPollInterval is how often the readiness wait re-reads Herdr.
	defaultPollInterval = 200 * time.Millisecond
	// defaultAgentKind is the Herdr agent kind the coordinator requires.
	defaultAgentKind = "omp"
	// defaultStepLease bounds how long one provisioning step may hold its durable
	// step claim. It has to outlast the longest step (an agent start plus the
	// readiness wait) so a live caller is never overtaken, and stay short enough
	// that a crashed one is recoverable by the next resume.
	defaultStepLease = 2 * time.Minute
	// primaryAgentPrefix and workerAgentPrefix keep the two roles' names disjoint:
	// a worker's name is its task id, a primary's is derived from the project.
	primaryAgentPrefix = "orch_p_"
	agentNameLimit     = 32
)

// Task identities the coordinator generates. Herdr labels are never used as
// identities.
const (
	taskIDPrefix   = "orch_"
	taskBranchBase = "orch/"
	taskIDBytes    = 6 // 12 hexadecimal characters
	idAttempts     = 5
	// recordAttempts bounds how often one step re-reads and re-applies its write
	// when another writer lands first: the bridge adopting an announced session
	// for a pane, or a concurrent coordinator call for the same task.
	recordAttempts = 4
)

// gitOutputLimit bounds one git invocation's diagnostics.
const gitOutputLimit = 64 << 10

// Options tunes one coordinator. A zero value is replaced by the documented
// default.
type Options struct {
	// Concurrency is the number of workers that may hold provisioning or running
	// resources at once.
	Concurrency int
	// CallTimeout bounds one Herdr CLI call.
	CallTimeout time.Duration
	// ReadinessTimeout bounds the wait for the extension's `ready` handshake.
	ReadinessTimeout time.Duration
	// PollInterval is how often the readiness wait re-reads Herdr.
	PollInterval time.Duration
	// Git is the git executable. It defaults to "git".
	Git string
	// AgentKind is the Herdr agent kind the coordinator requires and starts. It
	// defaults to "omp". The name is derived per agent (PrimaryAgentName /
	// WorkerAgentName): Herdr keeps live agent names unique, so one name for
	// every agent would collide with the primary that is already running.
	AgentKind string
	// StepLease bounds how long one provisioning step holds its durable step
	// claim. It defaults to two minutes.
	StepLease time.Duration
}

// Coordinator is the only writer of dispatch facts. It owns no private state:
// every decision is re-derived from the store and from authoritative Herdr
// responses.
type Coordinator struct {
	store  *state.Store
	herdr  *herdr.Client
	opts   Options
	random func([]byte) error
	// owner identifies this coordinator in the durable step claim, so the claim it
	// takes can be released by it and by nobody else.
	owner string
}

// New returns a coordinator over an open store and a Herdr client.
func New(store *state.Store, client *herdr.Client, opts Options) (*Coordinator, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: no store", ErrInvalidRequest)
	}
	if client == nil {
		return nil, fmt.Errorf("%w: no Herdr client", ErrInvalidRequest)
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	if opts.ReadinessTimeout <= 0 {
		opts.ReadinessTimeout = defaultReadinessTimeout
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if strings.TrimSpace(opts.Git) == "" {
		opts.Git = "git"
	}
	if strings.TrimSpace(opts.AgentKind) == "" {
		opts.AgentKind = defaultAgentKind
	}
	if opts.StepLease <= 0 {
		opts.StepLease = defaultStepLease
	}
	owner := make([]byte, 8)
	if _, err := rand.Read(owner); err != nil {
		return nil, fmt.Errorf("generate a coordinator identity: %w", err)
	}
	return &Coordinator{
		store:  store,
		herdr:  client,
		opts:   opts,
		random: fillRandom,
		owner:  hex.EncodeToString(owner),
	}, nil
}

// Bind records the primary OMP session for one canonical repository.
//
// repo is any path inside the repository; it is canonicalized with
// `git rev-parse --show-toplevel`, filepath.Abs/EvalSymlinks and the platform's
// case rules, and the canonical root becomes the project key the bridge
// compares against a pane's working directory.
//
// workspaceID and rootPaneID describe where the action was invoked from. A pane
// that already represents the canonical root is used as the primary's pane;
// otherwise a workspace is created for the root. The pane is recorded as both
// its launch-time and public identity before `agent start`, so the extension's
// handshake can be matched against `agent get`. Binding never steals focus and
// never substitutes a new primary for a recorded one that is gone.
func (c *Coordinator) Bind(ctx context.Context, repo, workspaceID, rootPaneID string) (task.Project, error) {
	root, err := c.canonicalRoot(ctx, repo)
	if err != nil {
		return task.Project{}, err
	}
	switch existing, err := c.store.Project(ctx, root); {
	case err == nil:
		return c.reopenPrimary(ctx, existing)
	case !errors.Is(err, state.ErrNotFound):
		return task.Project{}, err
	}

	workspace, paneID := "", ""
	if named := strings.TrimSpace(rootPaneID); named != "" {
		pane, err := c.paneGet(ctx, named)
		switch {
		case err == nil && !samePath(pane.Cwd, root):
			return task.Project{}, fmt.Errorf("%w: pane %q works in %s, not the requested repository %s",
				ErrDifferentRepository, named, pane.Cwd, root)
		case err == nil && paneHostsAgent(pane):
			// The pane already runs an agent, for example the user's own OMP
			// session. A dedicated primary is started in its own workspace on the
			// same checkout: this never starts a second OMP in a pane that has
			// one, and never adopts a session whose readiness cannot be observed
			// before the next human turn.
		case err == nil:
			workspace, paneID = pane.WorkspaceID, pane.PaneID
		case isMissingTarget(err):
			// The named pane is gone: fall through and create a workspace for the
			// root rather than binding a pane that no longer exists.
		case err != nil:
			return task.Project{}, err
		}
		if strings.TrimSpace(workspaceID) != "" && workspace != "" && workspace != workspaceID {
			return task.Project{}, fmt.Errorf("%w: pane %q is in workspace %q, not the requested %q",
				ErrDifferentRepository, paneID, workspace, workspaceID)
		}
	}
	if paneID == "" {
		created, err := c.workspaceCreate(ctx, root)
		if err != nil {
			return task.Project{}, err
		}
		workspace, paneID = created.WorkspaceID, created.RootPaneID
	}

	project := task.Project{
		Key:                root,
		RepoWorkspaceID:    workspace,
		CurrentWorkspaceID: workspace,
		LaunchPaneID:       paneID,
		PublicPaneID:       paneID,
		AgentName:          PrimaryAgentName(root),
	}
	if err := c.store.CreateProject(ctx, project); err != nil {
		return task.Project{}, fmt.Errorf("record the primary for %s: %w", root, err)
	}
	name, err := c.ensureAgent(ctx, paneID, PrimaryAgentName(root))
	if err != nil {
		return project, c.settleProject(ctx, root, isTerminal(err), err)
	}
	if name != "" {
		project.AgentName = name
		if err := c.store.UpdateProject(ctx, root, func(next *task.Project) error {
			next.AgentName = name
			return nil
		}); err != nil {
			return project, c.settleProject(ctx, root, true, err)
		}
	}
	view, err := c.awaitAgentReady(ctx, paneID, paneID, root, nil, func(view agentView) error {
		return c.store.UpdateProject(ctx, root, func(next *task.Project) error {
			if next.LaunchPaneID != paneID {
				return fmt.Errorf("%w: the primary binding changed to pane %q while the agent was starting",
					ErrPrimaryNotLive, next.LaunchPaneID)
			}
			// The first identity Herdr reports is the binding's session identity;
			// a later confirmation never overwrites it.
			if strings.TrimSpace(next.OMPSessionID) == "" {
				next.OMPSessionID = view.Ref.Value
			}
			next.TerminalID = view.Agent.TerminalID
			next.CurrentWorkspaceID = view.Agent.WorkspaceID
			return nil
		})
	})
	if err != nil {
		return project, c.settleProject(ctx, root, isTerminal(err), err)
	}
	if strings.TrimSpace(project.OMPSessionID) == "" {
		project.OMPSessionID = view.Ref.Value
	}
	project.TerminalID = view.Agent.TerminalID
	project.CurrentWorkspaceID = view.Agent.WorkspaceID
	return project, nil
}

// reopenPrimary re-verifies a recorded primary binding. It never starts a new
// primary in place of one that is missing: that would silently substitute a
// different session for the one the user was talking to. It does complete a bind
// that was interrupted before its agent started, in the pane that bind recorded.
//
// A recorded primary that is definitely not live is also recorded on the project,
// where the board reads it, and a primary this bind verified live retires that
// record.
func (c *Coordinator) reopenPrimary(ctx context.Context, existing task.Project) (task.Project, error) {
	paneID := existing.LaunchPaneID
	if strings.TrimSpace(paneID) == "" {
		paneID = existing.PublicPaneID
	}
	if strings.TrimSpace(paneID) == "" {
		return existing, c.refuseReopen(ctx, existing, "the project records no primary pane",
			fmt.Errorf("%w: project %s records no pane", ErrPrimaryNotLive, existing.Key))
	}
	pane, err := c.paneGet(ctx, paneID)
	switch {
	case isMissingTarget(err):
		return existing, c.refuseReopen(ctx, existing,
			fmt.Sprintf("the recorded primary pane %s no longer exists", paneID),
			fmt.Errorf("%w: the recorded primary pane %s no longer exists; relaunch the primary explicitly", ErrPrimaryNotLive, paneID))
	case err != nil:
		return existing, err
	case !samePath(pane.Cwd, existing.Key):
		return existing, c.refuseReopen(ctx, existing,
			fmt.Sprintf("the recorded primary pane %s works in %s, not the bound repository %s", paneID, pane.Cwd, existing.Key),
			fmt.Errorf("%w: the recorded primary pane %s works in %s, not the bound repository %s",
				ErrPrimaryNotLive, paneID, pane.Cwd, existing.Key))
	case paneHostsAgent(pane) && !strings.EqualFold(pane.Agent, c.opts.AgentKind):
		return existing, c.refuseReopen(ctx, existing,
			fmt.Sprintf("the recorded primary pane %s now runs %q", paneID, pane.Agent),
			fmt.Errorf("%w: the recorded primary pane %s now runs %q", ErrPrimaryNotLive, paneID, pane.Agent))
	}
	if pane.PaneID != existing.PublicPaneID || pane.WorkspaceID != existing.CurrentWorkspaceID {
		// Herdr resolved a moved pane to its current public identity. The
		// launch-time identity and the worktree provenance stay untouched.
		err := c.store.UpdateProject(ctx, existing.Key, func(next *task.Project) error {
			next.PublicPaneID = pane.PaneID
			next.CurrentWorkspaceID = pane.WorkspaceID
			return nil
		})
		if err != nil {
			return existing, err
		}
		existing.PublicPaneID = pane.PaneID
		existing.CurrentWorkspaceID = pane.WorkspaceID
	}
	name, err := c.ensureAgent(ctx, paneID, PrimaryAgentName(existing.Key))
	if err != nil {
		return existing, c.settleProject(ctx, existing.Key, isTerminal(err), err)
	}
	if name != "" && name != existing.AgentName {
		if err := c.store.UpdateProject(ctx, existing.Key, func(next *task.Project) error {
			next.AgentName = name
			return nil
		}); err != nil {
			return existing, c.settleProject(ctx, existing.Key, true, err)
		}
		existing.AgentName = name
	}
	workspaces := nonEmpty(existing.RepoWorkspaceID, existing.CurrentWorkspaceID)
	view, err := c.awaitAgentReady(ctx, paneID, paneID, existing.Key, workspaces, func(view agentView) error {
		return c.store.UpdateProject(ctx, existing.Key, func(next *task.Project) error {
			if next.LaunchPaneID != paneID {
				return fmt.Errorf("%w: the primary binding changed to pane %q", ErrPrimaryNotLive, next.LaunchPaneID)
			}
			if strings.TrimSpace(next.OMPSessionID) == "" {
				next.OMPSessionID = view.Ref.Value
			}
			next.TerminalID = view.Agent.TerminalID
			next.CurrentWorkspaceID = view.Agent.WorkspaceID
			return nil
		})
	})
	if err != nil {
		return existing, c.settleProject(ctx, existing.Key, isTerminal(err), err)
	}
	if strings.TrimSpace(existing.OMPSessionID) == "" {
		existing.OMPSessionID = view.Ref.Value
	}
	existing.TerminalID = view.Agent.TerminalID
	existing.CurrentWorkspaceID = view.Agent.WorkspaceID
	// The agent answered its readiness handshake in the recorded pane: the primary
	// is verified live, so a recorded absence of it ends here.
	if err := c.clearPrimaryAttention(ctx, existing); err != nil {
		return existing, err
	}
	existing.PrimaryAttention = ""
	return existing, nil
}

// settleProject records a failed primary bind as a deduplicated inbox note for
// the project, so the failure is durable and names the recorded identities. The
// project facts keep the pane the binding already recorded, so a retry reopens
// the same primary instead of starting another one.
func (c *Coordinator) settleProject(ctx context.Context, key string, terminal bool, cause error) error {
	kind := "attention"
	if terminal {
		kind = "blocked"
	}
	if err := c.notify(ctx, key, "", kind, cause.Error(), "bind failed"); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// PrimaryAgentName is the stable Herdr agent name of one repository's primary.
//
// It is derived from the canonical project key, so re-binding the same repository
// adopts the same agent instead of starting a second one, and two projects never
// share a name. The result is a legal Herdr name (see herdr.ValidAgentName).
func PrimaryAgentName(projectKey string) string {
	sum := sha256.Sum256([]byte(normalizedKey(projectKey)))
	return primaryAgentPrefix + hex.EncodeToString(sum[:4])
}

// WorkerAgentName is the Herdr agent name of one worker. The task id is already
// unique and stable, so using it as the name cannot collide with another worker,
// and Herdr's live agent name identifies the task it belongs to.
func WorkerAgentName(taskID string) string {
	name := strings.ToLower(strings.TrimSpace(taskID))
	if name == "" || !herdr.ValidAgentName(name) {
		return ""
	}
	return name
}

// normalizedKey is the project key in the one spelling the name derivation uses,
// so a differently cased path names the same primary.
func normalizedKey(projectKey string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(strings.TrimSpace(projectKey))))
}

// SessionHint is what a bound primary reports about its OMP session: the
// identities the session announced, whether the runtime has materialized the
// session to durable storage, and — only when resuming can actually work — the
// Herdr restore command.
//
// The rule this type exists to enforce: a session the runtime has not written to
// disk cannot be resumed. OMP materializes a session lazily (on its first
// assistant message), so `getSessionFile()` may name a path that leads nowhere and
// `omp --resume=<id>` would fail. A binding with such a session is still validly
// bound; it just has no runnable resume hint yet, and saying so beats handing the
// user a command that cannot work.
type SessionHint struct {
	ID                string `json:"id,omitempty"`
	File              string `json:"file,omitempty"`
	OnDisk            bool   `json:"onDisk"`
	Resume            string `json:"resume,omitempty"`
	ResumeUnavailable string `json:"resumeUnavailable,omitempty"`
}

// SessionHintOf renders a session hint, omitting the resume command while the
// session is not on disk.
func SessionHintOf(announced task.Announcement) SessionHint {
	hint := SessionHint{ID: announced.ID, File: announced.File, OnDisk: announced.OnDisk}
	switch {
	case !announced.OnDisk:
		hint.ResumeUnavailable = "this session is not materialized on disk yet, so it cannot be resumed; it becomes resumable after its first assistant message"
	case announced.ID == "":
		hint.ResumeUnavailable = "this session announced no id, so no resume command can be built for it"
	default:
		hint.Resume = "omp --resume=" + announced.ID
	}
	return hint
}

// SessionHint reports what the launch pane announced, for a caller that wants to
// describe the bound session to a human. It never claims durability: the on-disk
// flag comes from the runtime.
func (c *Coordinator) SessionHint(ctx context.Context, launchPaneID string) (SessionHint, error) {
	announced, found, err := c.store.AnnouncedSession(ctx, launchPaneID)
	if err != nil {
		return SessionHint{}, err
	}
	if !found {
		return SessionHint{}, fmt.Errorf("%w: launch pane %q announced no session", ErrSessionUnavailable, launchPaneID)
	}
	return SessionHintOf(announced), nil
}

// ---------------------------------------------------------------------------
// canonical repository identity
// ---------------------------------------------------------------------------

// canonicalRoot returns the canonical absolute path of the Git repository
// containing dir.
func (c *Coordinator) canonicalRoot(ctx context.Context, dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("%w: no directory was given", ErrNotGitRepository)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrNotGitRepository, dir, err)
	}
	out, err := c.git(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%w: %s is not inside a Git work tree (%s)", ErrNotGitRepository, dir, firstLine(out))
	}
	root := filepath.FromSlash(strings.TrimSpace(out))
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: %s resolved to the relative path %s", ErrNotGitRepository, dir, root)
	}
	return root, nil
}

// committedSHA resolves revision to the full commit id it names, refusing
// anything that is not a hexadecimal commit id: a branch, tag or symbolic
// revision is not a reproducible base for an isolated worker.
func (c *Coordinator) committedSHA(ctx context.Context, root, revision string) (string, error) {
	rev := strings.TrimSpace(revision)
	if !isHexRevision(rev) {
		return "", fmt.Errorf("%w: %q is not a hexadecimal commit id", ErrBaseRevision, revision)
	}
	out, err := c.git(ctx, root, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a commit of %s", ErrBaseRevision, revision, root)
	}
	sha := strings.ToLower(strings.TrimSpace(out))
	if !isHexRevision(sha) {
		return "", fmt.Errorf("%w: %q resolved to %q", ErrBaseRevision, revision, sha)
	}
	return sha, nil
}

// isHexRevision reports whether rev could name a commit id: hexadecimal, at
// least the abbreviated length Herdr's OMP tool accepts, and not longer than a
// full object id.
func isHexRevision(rev string) bool {
	if len(rev) < 7 || len(rev) > 64 {
		return false
	}
	for _, r := range rev {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// git runs one git invocation. argv goes straight to the executable; there is
// deliberately no shell anywhere in this package.
func (c *Coordinator) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.CallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.opts.Git, append([]string{"-C", dir}, args...)...)
	buf := &boundedOutput{limit: gitOutputLimit}
	cmd.Stdout = buf
	cmd.Stderr = buf
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return buf.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), ctxErr)
	}
	if err != nil {
		return buf.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return buf.String(), nil
}

// boundedOutput keeps at most limit bytes and reports full writes, so a noisy
// command is bounded without failing the process.
type boundedOutput struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf.Write(p[:room])
	}
	return len(p), nil
}

func (b *boundedOutput) String() string { return b.buf.String() }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// samePath compares two paths the way the platform does.
func samePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if !containsString(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Herdr calls with the coordinator's bounds
// ---------------------------------------------------------------------------

func (c *Coordinator) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.opts.CallTimeout)
}

// workspaceCreate creates a workspace for cwd without stealing focus.
func (c *Coordinator) workspaceCreate(ctx context.Context, cwd string) (*herdr.WorkspaceCreated, error) {
	ctx, cancel := c.callContext(ctx)
	defer cancel()
	created, err := c.herdr.WorkspaceCreate(ctx, cwd, "")
	if err != nil {
		return nil, fmt.Errorf("create a workspace for %q: %w", cwd, err)
	}
	return created, nil
}

// paneGet reads one pane. A launch-time id that Herdr still keeps as an alias
// resolves to the current public id, so callers use the response.
func (c *Coordinator) paneGet(ctx context.Context, paneID string) (*herdr.PaneInfo, error) {
	ctx, cancel := c.callContext(ctx)
	defer cancel()
	return c.herdr.PaneGet(ctx, paneID)
}

// paneHostsAgent reports whether a pane already runs an agent. Herdr labels a
// plain terminal as a shell; anything else is treated as occupied, so a pane
// that might be an agent is never started twice.
func paneHostsAgent(pane *herdr.PaneInfo) bool {
	agent := strings.ToLower(strings.TrimSpace(pane.Agent))
	switch agent {
	case "", "shell":
		return false
	default:
		return true
	}
}

// isMissingTarget reports a Herdr error that proves the target does not exist:
// such a failure is safe to retry or to replace, unlike an ambiguous one.
func isMissingTarget(err error) bool {
	var cliErr *herdr.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	switch cliErr.Code {
	case "agent_not_found", "pane_not_found", "not_found", "workspace_not_found", "worktree_not_found":
		return true
	default:
		return false
	}
}

// isTerminal reports whether a failure needs a human decision instead of an
// idempotent retry.
func isTerminal(err error) bool {
	switch {
	case errors.Is(err, herdr.ErrAmbiguous),
		errors.Is(err, herdr.ErrIdentity),
		errors.Is(err, herdr.ErrMalformedResponse),
		errors.Is(err, herdr.ErrOutputTooLarge),
		errors.Is(err, ErrAgentChanged),
		errors.Is(err, ErrSessionRefUnsupported),
		errors.Is(err, ErrAmbiguousStep),
		errors.Is(err, ErrHerdrUnavailable):
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// agent session identity
// ---------------------------------------------------------------------------

// agentView is the coordinator's read of one live agent: the typed projection
// plus the native session reference Herdr reports for its pane. A reference Herdr
// reports in a form this build cannot read is carried as RefErr and treated as a
// refusal, never as an empty session.
type agentView struct {
	Agent  herdr.AgentInfo
	Ref    task.SessionRef
	HasRef bool
	RefErr error
}

// agentGetResult decodes the raw `agent get` result, including the unmodeled
// agent_session field the typed client deliberately drops.
type agentGetResult struct {
	Type  string `json:"type"`
	Agent struct {
		herdr.AgentInfo
		AgentSession *herdr.SessionRef `json:"agent_session"`
	} `json:"agent"`
}

// agentGet reads the agent behind a live name or pane id and its native session
// reference. The raw body is decoded because the session reference is not part of
// the typed projection; everything else is validated as strictly as the typed
// client does, and the reference is decoded by the Herdr boundary as a typed pair.
func (c *Coordinator) agentGet(ctx context.Context, target string) (agentView, error) {
	ctx, cancel := c.callContext(ctx)
	defer cancel()
	raw, err := c.herdr.Call(ctx, "agent", "get", target)
	if err != nil {
		return agentView{}, err
	}
	var res agentGetResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return agentView{}, fmt.Errorf("%w: agent get %q: %v", herdr.ErrMalformedResponse, target, err)
	}
	if res.Type != "agent_info" {
		return agentView{}, fmt.Errorf("%w: agent get: expected result type %q, got %q",
			herdr.ErrMalformedResponse, "agent_info", res.Type)
	}
	for _, field := range []struct{ name, value string }{
		{"agent.pane_id", res.Agent.PaneID},
		{"agent.terminal_id", res.Agent.TerminalID},
		{"agent.workspace_id", res.Agent.WorkspaceID},
		{"agent.tab_id", res.Agent.TabID},
	} {
		if strings.TrimSpace(field.value) == "" {
			return agentView{}, &herdr.IdentityError{Op: "agent get", Field: field.name,
				Detail: "required identity is empty"}
		}
	}
	view := agentView{Agent: res.Agent.AgentInfo}
	if res.Agent.AgentSession != nil {
		kind, value, err := res.Agent.AgentSession.Identity()
		switch {
		case err != nil:
			// A reference this build cannot read is a refusal: the coordinator
			// reports it and never records a session it cannot verify.
			view.RefErr = err
		default:
			view.Ref = task.SessionRef{Kind: kind, Value: value}
			view.HasRef = true
		}
	}
	return view, nil
}

// ensureAgent makes sure exactly one agent of the required kind runs in pane, and
// returns the name Herdr reports for it.
//
// It adopts a live agent instead of starting a second one — including a
// re-bind, where the recorded agent is already running under the name this
// derivation produces — and records the name Herdr reports rather than assuming
// the one that was asked for. A name Herdr has already given to another live
// agent stays a definite failure: the collision is real and a human has to
// resolve it, so it is never retried blindly.
func (c *Coordinator) ensureAgent(ctx context.Context, paneID, name string) (string, error) {
	view, err := c.agentGet(ctx, paneID)
	switch {
	case err == nil:
		if !strings.EqualFold(view.Agent.Agent, c.opts.AgentKind) {
			return "", fmt.Errorf("%w: pane %q already runs %q, not %q",
				ErrAgentChanged, paneID, view.Agent.Agent, c.opts.AgentKind)
		}
		return liveAgentName(view), nil
	case isMissingTarget(err):
		// No agent in this pane: starting one is the next step.
	default:
		return "", fmt.Errorf("the pane's agent could not be read before starting one: %w", err)
	}

	if !herdr.ValidAgentName(name) {
		return "", fmt.Errorf("%w: %q is not a legal Herdr agent name", ErrInvalidRequest, name)
	}
	callCtx, cancel := c.callContext(ctx)
	started, startErr := c.herdr.AgentStart(callCtx, name, paneID)
	cancel()
	if startErr == nil {
		// The client already validated that the started agent sits on this pane.
		return liveAgentName(agentView{Agent: started.Agent}), nil
	}
	if !errors.Is(startErr, herdr.ErrAmbiguous) {
		return "", fmt.Errorf("start %q on pane %q: %w", name, paneID, startErr)
	}
	// The start may have taken effect. Adopt a live agent instead of repeating.
	if view, readErr := c.agentGet(ctx, paneID); readErr == nil && strings.EqualFold(view.Agent.Agent, c.opts.AgentKind) {
		return liveAgentName(view), nil
	}
	return "", fmt.Errorf("start %q on pane %q: %w", name, paneID, startErr)
}

// liveAgentName is the name Herdr reports for a live agent, falling back to the
// recorded role name when the response carries none.
func liveAgentName(view agentView) string {
	if name := strings.TrimSpace(view.Agent.Name); name != "" {
		return name
	}
	return ""
}

// awaitAgentReady waits until Herdr reports the current pane's native session
// reference and the extension has announced a session under readyPaneID that
// the store can confirm against that reference.
//
// Herdr reports session identity asynchronously after the agent starts, so the
// reference may be absent for a while: that is a normal state and is polled with
// backoff inside the readiness timeout. A reference Herdr reports in a form this
// build cannot read is a refusal, not an absence. The confirmed reference must be
// the one Herdr reports *in the confirmed kind's form*: an id compared as an id, a
// session file compared as a path. A session the extension announced that is not
// the reference Herdr reports is ErrAgentChanged — a human decides, and no brief is
// sent. write records a first-seen reference on the binding (it never overwrites).
func (c *Coordinator) awaitAgentReady(
	ctx context.Context,
	paneID, readyPaneID, checkout string,
	workspaces []string,
	write func(agentView) error,
) (agentView, error) {
	deadline := time.Now().Add(c.opts.ReadinessTimeout)
	interval := c.opts.PollInterval
	var last agentView
	var lastErr error
	sawReference := false
	for {
		view, err := c.agentGet(ctx, paneID)
		switch {
		case err == nil:
			last, lastErr = view, nil
			if !strings.EqualFold(view.Agent.Agent, c.opts.AgentKind) {
				return view, fmt.Errorf("%w: pane %q runs %q, not %q",
					ErrAgentChanged, paneID, view.Agent.Agent, c.opts.AgentKind)
			}
			if strings.TrimSpace(checkout) != "" && !samePath(view.Agent.Cwd, checkout) {
				return view, fmt.Errorf("%w: pane %q works in %s, not the recorded checkout %s",
					ErrAgentChanged, paneID, view.Agent.Cwd, checkout)
			}
			if len(workspaces) > 0 && !containsString(workspaces, view.Agent.WorkspaceID) {
				return view, fmt.Errorf("%w: pane %q is in workspace %q, not the recorded %s",
					ErrAgentChanged, paneID, view.Agent.WorkspaceID, strings.Join(workspaces, " or "))
			}
			if view.RefErr != nil {
				// Failing closed with the reason, rather than polling forever for a
				// reference this build will never understand.
				return view, fmt.Errorf("%w: pane %q: %v", ErrSessionRefUnsupported, paneID, view.RefErr)
			}
			if view.HasRef {
				if !sawReference {
					sawReference = true
					if write != nil {
						if err := write(view); err != nil {
							return view, err
						}
					}
				}
				stateErr := c.confirmReference(ctx, readyPaneID, view.Ref)
				switch {
				case stateErr == nil:
					return view, nil
				case errors.Is(stateErr, state.ErrNotFound):
					// The extension has not announced this session yet: keep waiting.
				default:
					return view, fmt.Errorf("%w: pane %q: %v", ErrAgentChanged, paneID, stateErr)
				}
			}
		case isMissingTarget(err):
			last, lastErr = view, err
		default:
			return view, err
		}

		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return last, fmt.Errorf("%w: pane %s: %v", ErrReadinessTimeout, paneID, lastErr)
			}
			if !sawReference {
				return last, fmt.Errorf("%w: pane %q reported no native session reference within %s, so the extension's handshake cannot be matched yet; the integration publishes it asynchronously, so a later resume can still succeed",
					ErrSessionUnavailable, paneID, c.opts.ReadinessTimeout)
			}
			return last, fmt.Errorf("%w: pane %q did not announce the session Herdr reports in time; no brief was sent",
				ErrReadinessTimeout, paneID)
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(interval):
		}
		// Back off so a slow integration does not spin, and reset whenever a
		// reference appeared (the next step needs a fresh observation).
		if sawReference {
			interval = c.opts.PollInterval
		} else if interval < 8*c.opts.PollInterval {
			interval *= 2
		}
	}
}

// confirmReference records the reference Herdr reported as verified, and reports
// whether the session the extension announced is that reference. The store owns
// the comparison: it knows which form was announced.
func (c *Coordinator) confirmReference(ctx context.Context, paneID string, ref task.SessionRef) error {
	if err := c.store.ConfirmReady(ctx, paneID, ref); err != nil {
		return err
	}
	announcement, ok := task.AnnouncementOfRef(ref)
	if !ok {
		return fmt.Errorf("%w: pane %q reported an unusable session reference", ErrSessionUnavailable, paneID)
	}
	ready, err := c.store.Ready(ctx, paneID, announcement)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%w: pane %q recorded the reference but it is not the announced session", ErrAgentChanged, paneID)
	}
	return nil
}
