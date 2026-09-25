package coord

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// This file tests the coordinator against two kinds of real evidence:
//
//   - A real temporary Git repository with two commits. Worktree creation is
//     performed by the fixture with `git worktree add`, exactly as Herdr does,
//     so "the base revision is a committed SHA" and "a dirty primary checkout is
//     not implicitly copied" are asserted on a real checkout.
//   - A real child process: TestMain turns this test binary into a stand-in
//     Herdr CLI chosen by COORD_HERDR_FIXTURE, so argv construction, phase
//     persistence and retry behavior are exercised through exec, never a mock.
//     The fixture also stands in for the OMP extension: it records the `ready`
//     handshake only for a session the store already binds to that pane, which
//     is precisely the condition the real bridge.authorize enforces.

// ---------------------------------------------------------------------------
// fixture child process
// ---------------------------------------------------------------------------

const (
	fixtureEnv       = "COORD_HERDR_FIXTURE"
	fixtureEnvDir    = "COORD_HERDR_FIXTURE_DIR"
	fixtureEnvStore  = "COORD_HERDR_FIXTURE_STATE_DIR"
	fixturePlanName  = "plan.json"
	fixtureStateName = "state.json"
	fixtureCallsName = "calls.jsonl"
)

// fixturePlan is what a test tells the stand-in CLI to do. Every knob exists to
// make one failure or retry path reproducible.
type fixturePlan struct {
	RepoRoot string `json:"repo_root"`
	// BindPane* describe the pane the bind action was invoked from. An empty
	// BindPaneID makes `pane get` answer pane_not_found, so bind creates a
	// workspace for the canonical root.
	BindPaneID        string `json:"bind_pane_id"`
	BindPaneCwd       string `json:"bind_pane_cwd"`
	BindPaneWorkspace string `json:"bind_pane_workspace"`
	BindPaneAgent     string `json:"bind_pane_agent"`
	// SuppressReady makes the extension stand-in never record the handshake.
	SuppressReady bool `json:"suppress_ready"`
	// SuppressSession makes `agent get` report no native session reference.
	SuppressSession bool `json:"suppress_session"`
	// RestartBeforeReady makes Herdr report a different native session once the
	// store already binds the pane, i.e. the agent was restarted.
	RestartBeforeReady bool `json:"restart_before_ready"`
	// AdoptBeforeRecord makes the stand-in extension adopt the announced session
	// through the bridge (as a real fresh session does) before the coordinator
	// could record it from `agent get`.
	AdoptBeforeRecord bool `json:"adopt_before_record"`
	// SessionRefKind selects the shape Herdr reports for the pane's native
	// session: "" or "id" (the bare id), "path" (the OMP session file the real
	// 0.9.1-preview build reports), or any other kind verbatim.
	SessionRefKind string `json:"session_ref_kind"`
	// SessionRefValue overrides the reported reference value verbatim.
	SessionRefValue string `json:"session_ref_value"`
	// SessionRefAfterGets makes Herdr report no reference at all for the first N
	// reads of a pane, the way the integration reports session identity
	// asynchronously after the agent starts.
	SessionRefAfterGets int `json:"session_ref_after_gets"`
	// SessionFileEmpty makes the announced session have no file yet, as an
	// unmaterialized OMP session does.
	SessionFileEmpty bool `json:"session_file_empty"`
	// SessionOnDisk makes the announced session claim it is materialized.
	SessionOnDisk bool `json:"session_on_disk"`
	// AnnounceSessionMismatch makes the extension announce a different session
	// than the one Herdr reports for the pane.
	AnnounceSessionMismatch bool `json:"announce_session_mismatch"`
	// AdoptDuringStart makes the extension (through the bridge) adopt its
	// announced session while `agent start` is still answering, which is what
	// bumps the task's revision between the coordinator's read and its write.
	AdoptDuringStart bool `json:"adopt_during_start"`
	// PromptAmbiguous makes `agent prompt` report Herdr's own timeout code after
	// recording the submission, the way a real CLI timeout does.
	PromptAmbiguous bool `json:"prompt_ambiguous"`
	// PromptError makes `agent prompt` refuse with that Herdr code before any
	// input is written, the way a blocked or vanished agent is refused.
	PromptError string `json:"prompt_error"`
	// WorktreeCreateError makes `worktree create` fail with that Herdr code
	// before any checkout exists.
	WorktreeCreateError string `json:"worktree_create_error"`
	// WorktreeCreateTimeout makes `worktree create` create the checkout and
	// then answer with Herdr's own timeout code, the way a real CLI timeout
	// arrives after the side effect.
	WorktreeCreateTimeout bool `json:"worktree_create_timeout"`
	// AgentStartError makes `agent start` fail with that Herdr code.
	AgentStartError string `json:"agent_start_error"`
	// SkipGitWorktree keeps the checkout off disk (an already-broken create).
	SkipGitWorktree bool `json:"skip_git_worktree"`
}

// fixtureAgent is one live agent the stand-in CLI remembers. Terminal is the
// terminal the agent's process runs on: it follows the terminal across a pane
// move and is what a binding proves a moved pane with.
type fixtureAgent struct {
	Name     string `json:"name"`
	Terminal string `json:"terminal"`
	Session  string `json:"session"`
	Status   string `json:"status"`
	Prompted int    `json:"prompted"`
	Gets     int    `json:"gets"`
}

// fixtureWorktree is one checkout the stand-in CLI created.
type fixtureWorktree struct {
	Path        string `json:"path"`
	Branch      string `json:"branch"`
	Base        string `json:"base"`
	WorkspaceID string `json:"workspace_id"`
	PaneID      string `json:"pane_id"`
}

// fixtureState is the stand-in CLI's durable view of the Herdr session.
type fixtureState struct {
	NextWorkspace int                      `json:"next_workspace"`
	NextSession   int                      `json:"next_session"`
	Workspaces    map[string]string        `json:"workspaces"`
	Agents        map[string]*fixtureAgent `json:"agents"`
	Worktrees     []fixtureWorktree        `json:"worktrees"`
	Prompts       map[string][]string      `json:"prompts"`
}

// fixtureCall is one invocation the coordinator made, recorded verbatim.
type fixtureCall struct {
	Argv []string `json:"argv"`
}

func TestMain(m *testing.M) {
	if os.Getenv(fixtureEnv) == "1" {
		os.Exit(runFixture())
	}
	os.Exit(m.Run())
}

func runFixture() int {
	dir := os.Getenv(fixtureEnvDir)
	if dir == "" {
		return 20
	}
	plan := fixturePlan{}
	if err := readJSONFile(filepath.Join(dir, fixturePlanName), &plan); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: plan: %v\n", err)
		return 21
	}
	state := fixtureState{}
	if err := readJSONFile(filepath.Join(dir, fixtureStateName), &state); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: state: %v\n", err)
		return 22
	}
	state.ensure()
	argv := append([]string(nil), os.Args[1:]...)
	if err := appendCall(dir, argv); err != nil {
		return 23
	}
	if len(argv) < 2 {
		return fixtureUnexpected(argv, "argv is too short")
	}
	command := argv[0] + " " + argv[1]
	switch command {
	case "workspace create":
		ws := state.newWorkspace(fixtureFlag(argv, "--cwd"))
		state.save(dir)
		return fixtureResult(workspaceCreatedResult(ws, fixtureFlag(argv, "--cwd")))
	case "worktree create":
		return fixtureWorktreeCreate(dir, plan, &state, argv)
	case "worktree list":
		state.save(dir)
		return fixtureResult(worktreeListResult(plan.RepoRoot, state))
	case "api snapshot":
		state.save(dir)
		return fixtureResult(sessionSnapshotResult(plan, state))
	case "pane get":
		return fixturePaneGet(plan, state, argv)
	case "pane move":
		return fixturePaneMove(dir, plan, &state, argv)
	case "agent get":
		return fixtureAgentGet(dir, plan, &state, argv)
	case "agent start":
		return fixtureAgentStart(dir, plan, &state, argv)
	case "agent prompt":
		return fixtureAgentPrompt(dir, plan, &state, argv)
	default:
		return fixtureUnexpected(argv, "unsupported command")
	}
}

func fixtureWorktreeCreate(dir string, plan fixturePlan, state *fixtureState, argv []string) int {
	if plan.WorktreeCreateError != "" {
		return fixtureError(plan.WorktreeCreateError, "the checkout was refused")
	}
	root := fixtureFlag(argv, "--cwd")
	branch := fixtureFlag(argv, "--branch")
	base := fixtureFlag(argv, "--base")
	for _, wt := range state.Worktrees {
		if wt.Branch == branch {
			return fixtureError("worktree_exists", "the branch already has a checkout")
		}
	}
	path := filepath.Join(dir, "worktrees", strings.ReplaceAll(branch, "/", "-"))
	if !plan.SkipGitWorktree {
		if out, err := runGit(root, "worktree", "add", "-b", branch, path, base); err != nil {
			fmt.Fprintf(os.Stderr, "fixture: git worktree add: %v: %s\n", err, out)
			return 24
		}
	}
	ws := state.newWorkspace(path)
	wt := fixtureWorktree{Path: path, Branch: branch, Base: base, WorkspaceID: ws, PaneID: ws + ":p1"}
	state.Worktrees = append(state.Worktrees, wt)
	state.save(dir)
	if plan.WorktreeCreateTimeout {
		// The checkout exists; the answer never arrived.
		return fixtureError("timeout", "the command timed out after the checkout was created")
	}
	return fixtureResult(worktreeCreatedResult(wt, root))
}

func fixturePaneGet(plan fixturePlan, state fixtureState, argv []string) int {
	pane := argv[2]
	if ws := workspaceOfPane(pane); state.Workspaces[ws] != "" {
		agent := ""
		if state.Agents[pane] != nil {
			agent = "omp"
		}
		return fixtureResult(map[string]any{
			"type": "pane_info",
			"pane": paneInfo(state, pane, ws, state.Workspaces[ws], agent),
		})
	}
	if plan.BindPaneID == "" || pane != plan.BindPaneID {
		return fixtureError("pane_not_found", "pane "+pane+" not found")
	}
	return fixtureResult(map[string]any{
		"type": "pane_info",
		"pane": paneInfo(state, plan.BindPaneID, plan.BindPaneWorkspace, plan.BindPaneCwd, plan.BindPaneAgent),
	})
}

// fixturePaneMove moves one pane to a new workspace, the way `herdr pane move
// --new-workspace` does. Moving the last pane out of a workspace closes that
// workspace, and the result reports exactly that. The terminal - and therefore
// the agent process - keeps its terminal id across the move.
func fixturePaneMove(dir string, plan fixturePlan, state *fixtureState, argv []string) int {
	pane := argv[2]
	workspace := workspaceOfPane(pane)
	if state.Workspaces[workspace] == "" {
		return fixtureError("pane_not_found", "pane "+pane+" not found")
	}
	if !fixtureHasFlag(argv, "--new-workspace") {
		return fixtureUnexpected(argv, "only a --new-workspace move is supported")
	}
	cwd := state.Workspaces[workspace]
	next := state.newWorkspace(cwd)
	moved := next + ":p1"
	movedPane := paneInfo(*state, moved, next, cwd, "")
	if agent := state.Agents[pane]; agent != nil {
		delete(state.Agents, pane)
		state.Agents[moved] = agent
		movedPane = paneInfo(*state, moved, next, cwd, "omp")
		movedPane["foreground_cwd"] = cwd
		// A real PaneInfo carries the pane's native session reference, so the
		// move result does too.
		if ref := fixtureReference(plan, fixtureAnnouncement(plan, agent.Session)); ref != nil {
			movedPane["agent_session"] = sessionInfo(*ref)
		}
	}
	// The source workspace held this pane alone, so the move closed it.
	delete(state.Workspaces, workspace)
	state.save(dir)
	return fixtureResult(map[string]any{
		"type": "pane_move",
		"move_result": map[string]any{
			"changed":               true,
			"previous_pane_id":      pane,
			"previous_workspace_id": workspace,
			"previous_tab_id":       workspace + ":t1",
			"closed_workspace_id":   workspace,
			"closed_tab_id":         workspace + ":t1",
			"pane":                  movedPane,
		},
	})
}

func fixtureAgentGet(dir string, plan fixturePlan, state *fixtureState, argv []string) int {
	pane := argv[2]
	agent := state.Agents[pane]
	if agent == nil {
		return fixtureError("agent_not_found", "agent target "+pane+" not found")
	}
	agent.Gets++
	announced := fixtureAnnouncement(plan, agent.Session)
	if plan.AdoptBeforeRecord {
		adoptFixtureSession(pane, announced.ID)
	}
	reported := fixtureReference(plan, announced)
	if plan.SessionRefAfterGets > 0 && agent.Gets <= plan.SessionRefAfterGets {
		// The integration has not reported session identity for this pane yet.
		reported = nil
	}
	if plan.RestartBeforeReady && fixtureBound(stateDirOf(), pane, announced) {
		// The extension announced this session, and Herdr now reports a different
		// one for the same pane: the process was restarted.
		recordFixtureReady(pane, announced)
		restarted := announced
		restarted.ID += "-restarted"
		reported = &task.SessionRef{Kind: task.SessionKindID, Value: restarted.ID}
	} else if !plan.SuppressReady && fixtureBound(stateDirOf(), pane, announced) {
		// The OMP extension can only announce readiness for a session the bridge
		// already resolves to its own launch pane.
		recordFixtureReady(pane, announced)
	}
	state.save(dir)
	return fixtureResult(map[string]any{
		"type":  "agent_info",
		"agent": agentInfo(*state, pane, fixtureCwd(plan, *state, pane), agent, reported),
	})
}

// sessionFileFor is the session file OMP allocates for one session: the runtime
// names it after the session id, under the agent's sessions directory.
func sessionFileFor(session string) string {
	return `C:\work\omp\agent\sessions\-work-orch\2026-09-25T11-59-03-837Z_` + session + `.jsonl`
}

// fixtureAnnouncement is what this fixture's extension stand-in announces: both
// identities OMP exposes, plus whether the session is materialized.
func fixtureAnnouncement(plan fixturePlan, session string) task.Announcement {
	if session == "" {
		return task.Announcement{}
	}
	file := sessionFileFor(session)
	if plan.SessionFileEmpty {
		file = ""
	}
	return task.Announcement{ID: session, File: file, OnDisk: plan.SessionOnDisk}
}

// fixtureReference is Herdr's reported reference for a pane: the kind the plan
// asks for (the real build reports "PATH"), carrying the matching identity.
func fixtureReference(plan fixturePlan, announced task.Announcement) *task.SessionRef {
	if plan.SuppressSession || announced.ID == "" {
		return nil
	}
	kind := plan.SessionRefKind
	if kind == "" {
		kind = task.SessionKindID
	}
	value := announced.ID
	if strings.EqualFold(kind, task.SessionKindPath) {
		value = announced.File
	}
	if plan.SessionRefValue != "" {
		value = plan.SessionRefValue
	}
	return &task.SessionRef{Kind: kind, Value: value}
}

// fixtureCwd is the working directory Herdr reports for an agent's pane: the
// checkout the workspace was created for, or the primary checkout bind was
// invoked from.
func fixtureCwd(plan fixturePlan, state fixtureState, pane string) string {
	if cwd := state.Workspaces[workspaceOfPane(pane)]; cwd != "" {
		return cwd
	}
	if plan.BindPaneCwd != "" {
		return plan.BindPaneCwd
	}
	return plan.RepoRoot
}

func fixtureAgentStart(dir string, plan fixturePlan, state *fixtureState, argv []string) int {
	pane := fixtureFlag(argv, "--pane")
	name := argv[2]
	if state.Agents[pane] != nil {
		return fixtureError("agent_already_running", "the pane already runs an agent")
	}
	// Herdr keeps live agent names unique across the whole session, which is what
	// made one shared name collide with the running primary.
	for otherPane, agent := range state.Agents {
		if agent != nil && agent.Name == name {
			return fixtureError("agent_name_taken", "agent name "+name+" is already used by "+otherPane)
		}
	}
	if plan.AgentStartError != "" {
		return fixtureError(plan.AgentStartError, "the agent did not start")
	}
	state.NextSession++
	agent := &fixtureAgent{
		Name:     argv[2],
		Terminal: "term-" + strings.ReplaceAll(pane, ":", "-"),
		Session:  fmt.Sprintf("omp-session-%d", state.NextSession),
		Status:   "idle",
	}
	state.Agents[pane] = agent
	state.save(dir)
	if plan.AdoptDuringStart {
		// The extension is already running in this pane: it resolves and adopts
		// its announced session while the start call is still answering.
		adoptFixtureSession(pane, agent.Session)
	}
	return fixtureResult(map[string]any{
		"type":  "agent_started",
		"agent": agentInfo(*state, pane, fixtureCwd(plan, *state, pane), agent, fixtureReference(plan, fixtureAnnouncement(plan, agent.Session))),
		"argv":  []string{"omp"},
	})
}

func fixtureAgentPrompt(dir string, plan fixturePlan, state *fixtureState, argv []string) int {
	pane := argv[2]
	agent := state.Agents[pane]
	if agent == nil {
		return fixtureError("agent_not_found", "agent target "+pane+" not found")
	}
	if plan.PromptError != "" {
		// Refused before anything was written: Herdr rejects a prompt to a
		// blocked or vanished agent without sending input.
		return fixtureError(plan.PromptError, "the prompt was refused before any input was written")
	}
	// The submission is recorded before the answer: a timeout here means the
	// brief may already have been delivered.
	state.Prompts[pane] = append(state.Prompts[pane], argv[3])
	agent.Prompted++
	agent.Status = "working"
	state.save(dir)
	if plan.PromptAmbiguous {
		return fixtureError("timeout", "the prompt may already have been submitted")
	}
	return fixtureResult(map[string]any{
		"type":  "agent_prompted",
		"agent": agentInfo(*state, pane, fixtureCwd(plan, *state, pane), agent, fixtureReference(plan, fixtureAnnouncement(plan, agent.Session))),
	})
}

// fixtureBound reports whether the store already binds this pane to the
// announcement the extension would make, the way bridge.authorize decides it.
func fixtureBound(storeDir, pane string, announced task.Announcement) bool {
	if storeDir == "" || announced.ID == "" {
		return false
	}
	store, err := state.Open(storeDir)
	if err != nil {
		return false
	}
	defer func() { _ = store.Close() }()
	binding, err := store.ByOMPSession(context.Background(), announced)
	if err != nil {
		return false
	}
	if binding.Role == state.RolePrimary {
		return binding.Project.LaunchPaneID == pane
	}
	return binding.Task.LaunchPaneID == pane
}

// adoptFixtureSession stands in for the bridge adopting an announced session for
// the task already keyed to this pane: the extension resolves before the
// coordinator could have recorded the session it reads from `agent get`.
func adoptFixtureSession(pane, session string) {
	storeDir := stateDirOf()
	if storeDir == "" || session == "" {
		return
	}
	store, err := state.Open(storeDir)
	if err != nil {
		return
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	taskOnPane, err := store.ByLaunchPane(ctx, pane)
	if err != nil || strings.TrimSpace(taskOnPane.OMPSessionID) != "" {
		return
	}
	// The bridge adopts the announced id when the runtime exposes one, which is
	// what OMP always does.
	if _, err := store.UpdateTask(ctx, taskOnPane.ID, taskOnPane.Revision, func(next *task.Task) error {
		next.OMPSessionID = session
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: adopt: %v\n", err)
	}
}

// recordFixtureReady stands in for the extension's `ready` call: it announces the
// whole identity surface, still unverified.
func recordFixtureReady(pane string, announced task.Announcement) {
	storeDir := stateDirOf()
	if storeDir == "" {
		return
	}
	store, err := state.Open(storeDir)
	if err != nil {
		return
	}
	defer func() { _ = store.Close() }()
	if err := store.RecordReady(context.Background(), pane, announced, 1); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: RecordReady: %v\n", err)
	}
}

func stateDirOf() string { return os.Getenv(fixtureEnvStore) }

func (s *fixtureState) ensure() {
	if s.Workspaces == nil {
		s.Workspaces = map[string]string{}
	}
	if s.Agents == nil {
		s.Agents = map[string]*fixtureAgent{}
	}
	if s.Prompts == nil {
		s.Prompts = map[string][]string{}
	}
}

func (s *fixtureState) newWorkspace(cwd string) string {
	s.NextWorkspace++
	id := fmt.Sprintf("w%d", s.NextWorkspace)
	s.Workspaces[id] = cwd
	return id
}

func (s *fixtureState) save(dir string) {
	if err := writeJSONFile(filepath.Join(dir, fixtureStateName), s); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: save state: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// fixture responses
// ---------------------------------------------------------------------------

func workspaceCreatedResult(ws, cwd string) map[string]any {
	pane := paneInfo(fixtureState{}, ws+":p1", ws, cwd, "")
	return map[string]any{
		"type":      "workspace_created",
		"workspace": workspaceInfo(ws),
		"tab":       tabInfo(ws),
		"root_pane": pane,
	}
}

func worktreeCreatedResult(wt fixtureWorktree, root string) map[string]any {
	pane := paneInfo(fixtureState{}, wt.PaneID, wt.WorkspaceID, wt.Path, "")
	ws := workspaceInfo(wt.WorkspaceID)
	ws["worktree"] = map[string]any{
		"checkout_path":      wt.Path,
		"repo_key":           strings.ToLower(filepath.ToSlash(root)) + "/.git",
		"repo_name":          filepath.Base(root),
		"repo_root":          root,
		"is_linked_worktree": true,
	}
	return map[string]any{
		"type":      "worktree_created",
		"workspace": ws,
		"tab":       tabInfo(wt.WorkspaceID),
		"root_pane": pane,
		"worktree": map[string]any{
			"path":               wt.Path,
			"label":              filepath.Base(wt.Path),
			"branch":             wt.Branch,
			"open_workspace_id":  wt.WorkspaceID,
			"is_bare":            false,
			"is_detached":        false,
			"is_prunable":        false,
			"is_linked_worktree": true,
		},
	}
}

func worktreeListResult(root string, state fixtureState) map[string]any {
	worktrees := []map[string]any{{
		"path":               root,
		"label":              filepath.Base(root),
		"branch":             "main",
		"open_workspace_id":  nil,
		"is_bare":            false,
		"is_detached":        false,
		"is_prunable":        false,
		"is_linked_worktree": false,
	}}
	for _, wt := range state.Worktrees {
		open := wt.WorkspaceID
		worktrees = append(worktrees, map[string]any{
			"path":               wt.Path,
			"label":              filepath.Base(wt.Path),
			"branch":             wt.Branch,
			"open_workspace_id":  open,
			"is_bare":            false,
			"is_detached":        false,
			"is_prunable":        false,
			"is_linked_worktree": true,
		})
	}
	return map[string]any{
		"type": "worktree_list",
		"source": map[string]any{
			"repo_key":             strings.ToLower(filepath.ToSlash(root)) + "/.git",
			"repo_name":            filepath.Base(root),
			"repo_root":            root,
			"source_checkout_path": root,
			"source_workspace_id":  "",
		},
		"worktrees": worktrees,
	}
}

// sessionSnapshotResult renders Herdr's authoritative live state: the panes of
// every workspace, including the agents running in them.
//
// A pane is emitted at the id its agent actually occupies, so a pane the session
// moved to a different workspace appears where it is now; a workspace with no
// agent contributes its root pane. The pane objects carry the same fields
// `pane get` answers with, including the native session reference and the
// terminal's foreground directory (`herdr api schema --json`: PaneInfo requires
// pane_id, terminal_id, workspace_id, tab_id and allows agent_session and
// foreground_cwd).
func sessionSnapshotResult(plan fixturePlan, state fixtureState) map[string]any {
	panes := []map[string]any{}
	agents := []map[string]any{}
	occupied := map[string]bool{}
	for _, pane := range sortedPanes(state.Agents) {
		occupied[workspaceOfPane(pane)] = true
	}
	for _, ws := range sortedWorkspaces(state.Workspaces) {
		if occupied[ws] {
			continue
		}
		panes = append(panes, paneInfo(state, ws+":p1", ws, state.Workspaces[ws], ""))
	}
	for _, pane := range sortedPanes(state.Agents) {
		agent := state.Agents[pane]
		if agent == nil {
			continue
		}
		ws := workspaceOfPane(pane)
		cwd := state.Workspaces[ws]
		reported := fixtureReference(plan, fixtureAnnouncement(plan, agent.Session))
		info := paneInfo(state, pane, ws, cwd, "omp")
		info["foreground_cwd"] = cwd
		if reported != nil {
			info["agent_session"] = sessionInfo(*reported)
		}
		panes = append(panes, info)
		agents = append(agents, agentInfo(state, pane, cwd, agent, reported))
	}
	return map[string]any{
		"type": "session_snapshot",
		"snapshot": map[string]any{
			"version":              "0.9.1-preview",
			"protocol":             22,
			"focused_pane_id":      "",
			"focused_tab_id":       "",
			"focused_workspace_id": "",
			"agents":               agents,
			"panes":                panes,
		},
	}
}

// sessionInfo renders Herdr's AgentSessionInfo: a typed reference, captured from
// a real `herdr agent get` (kind "PATH" carrying the session file on the
// installed preview build).
func sessionInfo(ref task.SessionRef) map[string]any {
	return map[string]any{
		"source": "herdr:omp",
		"agent":  "omp",
		"kind":   ref.Kind,
		"value":  ref.Value,
	}
}

func sortedWorkspaces(workspaces map[string]string) []string {
	ids := make([]string, 0, len(workspaces))
	for id := range workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedPanes(agents map[string]*fixtureAgent) []string {
	panes := make([]string, 0, len(agents))
	for pane := range agents {
		panes = append(panes, pane)
	}
	sort.Strings(panes)
	return panes
}

// fixtureTerminal is the terminal Herdr reports for a pane. The terminal id
// follows the terminal across a move, so an agent remembers its own.
func fixtureTerminal(state fixtureState, pane string) string {
	if agent := state.Agents[pane]; agent != nil && strings.TrimSpace(agent.Terminal) != "" {
		return agent.Terminal
	}
	return "term-" + strings.ReplaceAll(pane, ":", "-")
}

// fixtureHasFlag reports whether one flag is present in an argv.
func fixtureHasFlag(argv []string, name string) bool {
	for _, arg := range argv {
		if arg == name {
			return true
		}
	}
	return false
}

func workspaceInfo(ws string) map[string]any {
	return map[string]any{
		"workspace_id":  ws,
		"label":         ws,
		"active_tab_id": ws + ":t1",
		"agent_status":  "unknown",
		"number":        1,
		"pane_count":    1,
		"tab_count":     1,
	}
}

func tabInfo(ws string) map[string]any {
	return map[string]any{
		"tab_id":       ws + ":t1",
		"workspace_id": ws,
		"label":        "1",
		"number":       1,
		"pane_count":   1,
	}
}

func paneInfo(state fixtureState, pane, ws, cwd, agent string) map[string]any {
	return map[string]any{
		"pane_id":       pane,
		"terminal_id":   fixtureTerminal(state, pane),
		"workspace_id":  ws,
		"tab_id":        ws + ":t1",
		"label":         "",
		"title":         "pwsh",
		"agent":         agent,
		"display_agent": "",
		"agent_status":  "unknown",
		"cwd":           cwd,
		"focused":       false,
		"revision":      1,
	}
}

// agentInfo renders Herdr's AgentInfo, including the native session reference
// (AgentSessionInfo) that `herdr api schema --json` documents. The reference is
// optional: the integration reports it asynchronously.
func agentInfo(state fixtureState, pane, cwd string, agent *fixtureAgent, reported *task.SessionRef) map[string]any {
	ws := workspaceOfPane(pane)
	info := map[string]any{
		"name":                    agent.Name,
		"agent":                   "omp",
		"display_agent":           "omp",
		"agent_status":            agent.Status,
		"pane_id":                 pane,
		"terminal_id":             fixtureTerminal(state, pane),
		"workspace_id":            ws,
		"tab_id":                  ws + ":t1",
		"cwd":                     cwd,
		"focused":                 false,
		"revision":                2,
		"state_change_seq":        1,
		"interactive_ready":       true,
		"launch_pending":          false,
		"terminal_title":          "omp",
		"terminal_title_stripped": "omp",
	}
	if reported != nil {
		info["agent_session"] = sessionInfo(*reported)
	}
	return info
}

// ---------------------------------------------------------------------------
// fixture plumbing
// ---------------------------------------------------------------------------

func fixtureFlag(argv []string, name string) string {
	for i, arg := range argv {
		if arg == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func fixtureResult(result any) int {
	body, err := json.Marshal(map[string]any{"id": "cli:fixture", "result": result})
	if err != nil {
		return 25
	}
	if _, err := os.Stdout.Write(append(body, '\n')); err != nil {
		return 26
	}
	return 0
}

func fixtureError(code, message string) int {
	body, err := json.Marshal(map[string]any{
		"id":    "cli:fixture",
		"error": map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return 27
	}
	if _, err := os.Stderr.Write(append(body, '\n')); err != nil {
		return 28
	}
	return 1
}

func fixtureUnexpected(argv []string, why string) int {
	fmt.Fprintf(os.Stderr, "fixture: %s: argv %q\n", why, argv)
	return 29
}

func appendCall(dir string, argv []string) error {
	path := filepath.Join(dir, fixtureCallsName)
	line, err := json.Marshal(fixtureCall{Argv: argv})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.Write(append(line, '\n'))
	return err
}

func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func writeJSONFile(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func workspaceOfPane(pane string) string {
	ws, _, _ := strings.Cut(pane, ":")
	return ws
}

// ---------------------------------------------------------------------------
// test fixture
// ---------------------------------------------------------------------------

type coordFixture struct {
	t       *testing.T
	root    string
	base    string
	dir     string
	plan    fixturePlan
	store   *state.Store
	coord   *Coordinator
	client  *herdr.Client
	project string
}

// newCoordFixture builds a real repository with two commits, a stand-in Herdr
// CLI and a coordinator wired to a real store.
func newCoordFixture(t *testing.T, plan fixturePlan) *coordFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
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
	writeRepoFile(t, root, "src/main.txt", "second\n")
	base := commitAll(t, root, "second commit")

	f := &coordFixture{t: t, root: root, base: base, dir: t.TempDir()}
	plan.RepoRoot = root
	f.plan = plan
	f.writePlan()

	stateDir := t.TempDir()
	t.Setenv(fixtureEnv, "1")
	t.Setenv(fixtureEnvDir, f.dir)
	t.Setenv(fixtureEnvStore, stateDir)

	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store

	client, err := herdr.NewClient(exe)
	if err != nil {
		t.Fatalf("herdr.NewClient: %v", err)
	}
	f.client = client

	coord, err := New(store, client, Options{
		Concurrency:      3,
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("coord.New: %v", err)
	}
	f.coord = coord
	return f
}

func (f *coordFixture) writePlan() {
	f.t.Helper()
	if err := writeJSONFile(filepath.Join(f.dir, fixturePlanName), f.plan); err != nil {
		f.t.Fatalf("write plan: %v", err)
	}
}

func (f *coordFixture) updatePlan(mutate func(*fixturePlan)) {
	f.t.Helper()
	mutate(&f.plan)
	f.writePlan()
}

func (f *coordFixture) bind(workspaceID, rootPaneID string) (task.Project, error) {
	f.t.Helper()
	return f.coord.Bind(context.Background(), f.root, workspaceID, rootPaneID)
}

// projectKey binds the fixture's repository once, the way the user's bind
// action does before a primary can dispatch. The bind runs with a neutral plan,
// because the test's failure knobs describe the worker's pane; its Herdr calls
// are dropped from the call log so each dispatch test counts only its own work.
func (f *coordFixture) projectKey() string {
	f.t.Helper()
	if f.project != "" {
		return f.project
	}
	saved := f.plan
	f.plan = fixturePlan{RepoRoot: saved.RepoRoot}
	f.writePlan()
	project, err := f.bind("", "")
	f.plan = saved
	f.writePlan()
	if err != nil {
		f.t.Fatalf("bind the fixture project: %v", err)
	}
	f.project = project.Key
	if err := os.Remove(filepath.Join(f.dir, fixtureCallsName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("reset the call log: %v", err)
	}
	return f.project
}

func (f *coordFixture) dispatch(title, brief string, checks, dependencies, reserved []string, base string) (task.Task, error) {
	f.t.Helper()
	return f.coord.Dispatch(context.Background(), f.projectKey(), title, brief, checks, dependencies, reserved, base)
}

func (f *coordFixture) resume(taskID string) (task.Task, error) {
	f.t.Helper()
	return f.coord.ResumeDispatch(context.Background(), taskID)
}

func (f *coordFixture) mustDispatch(title, brief string) task.Task {
	f.t.Helper()
	st, err := f.dispatch(title, brief, []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		f.t.Fatalf("dispatch %q: %v", title, err)
	}
	return st
}

func (f *coordFixture) mustBind() task.Project {
	f.t.Helper()
	p, err := f.bind("", "")
	if err != nil {
		f.t.Fatalf("bind: %v", err)
	}
	return p
}

func (f *coordFixture) calls() []fixtureCall {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, fixtureCallsName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		f.t.Fatalf("read calls: %v", err)
	}
	var out []fixtureCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var call fixtureCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			f.t.Fatalf("decode call %q: %v", line, err)
		}
		out = append(out, call)
	}
	return out
}

func (f *coordFixture) countCalls(command string) int {
	f.t.Helper()
	count := 0
	for _, call := range f.calls() {
		if len(call.Argv) >= 2 && call.Argv[0]+" "+call.Argv[1] == command {
			count++
		}
	}
	return count
}

func (f *coordFixture) state() fixtureState {
	f.t.Helper()
	var st fixtureState
	if err := readJSONFile(filepath.Join(f.dir, fixtureStateName), &st); err != nil {
		f.t.Fatalf("read state: %v", err)
	}
	return st
}

func (f *coordFixture) worktreeFor(t *testing.T, branch string) fixtureWorktree {
	t.Helper()
	for _, wt := range f.state().Worktrees {
		if wt.Branch == branch {
			return wt
		}
	}
	t.Fatalf("no worktree recorded for branch %q", branch)
	return fixtureWorktree{}
}

func (f *coordFixture) prompts(pane string) []string {
	f.t.Helper()
	return f.state().Prompts[pane]
}

// satisfyReview marks a task reviewed the way a worker's final report does, so a
// dependency is no longer gating.
func (f *coordFixture) satisfyReview(st task.Task) task.Task {
	f.t.Helper()
	updated, err := f.store.UpdateTask(context.Background(), st.ID, st.Revision, func(next *task.Task) error {
		next.Report = &task.Report{
			Kind:     "result",
			Summary:  "done",
			Branch:   next.Branch,
			Evidence: []string{"commit " + f.base},
			Checks:   []string{"go test ./... ok"},
			Risk:     "none",
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("mark %s reviewed: %v", st.ID, err)
	}
	return updated
}

func (f *coordFixture) block(st task.Task, mutate func(*task.Task)) task.Task {
	f.t.Helper()
	updated, err := f.store.UpdateTask(context.Background(), st.ID, st.Revision, func(next *task.Task) error {
		mutate(next)
		return nil
	})
	if err != nil {
		f.t.Fatalf("update %s: %v", st.ID, err)
	}
	return updated
}

// decodeHex turns a hexadecimal identity back into the bytes a pinned random
// source must produce.
func decodeHex(t *testing.T, value string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode %q: %v", value, err)
	}
	return raw
}

func writeRepoFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func commitAll(t *testing.T, root, message string) string {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", message}} {
		if out, err := runGit(root, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := runGit(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v: %s", err, out)
	}
	return strings.TrimSpace(out)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatalf("git -C %s %v: %v: %s", dir, args, err, out)
	}
	return strings.TrimSpace(out)
}

// ---------------------------------------------------------------------------
// Bind
// ---------------------------------------------------------------------------

func TestBindRecordsTheCanonicalPrimaryAndWaitsForTheExtension(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{
		BindPaneID:        "w1:p2",
		BindPaneCwd:       "",
		BindPaneWorkspace: "w1",
	})
	// A differently-cased, dot-terminated path must canonicalize to the same
	// repository root: the project key is the checkout identity the bridge
	// compares against a pane's cwd.
	eleven := filepath.Join(f.root, ".")
	f.plan.BindPaneCwd = eleven
	f.plan.BindPaneAgent = ""
	f.writePlan()
	project, err := f.coord.Bind(context.Background(), eleven, "w1", "w1:p2")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if !samePath(project.Key, f.root) {
		t.Errorf("project key = %q, want the canonical root %q", project.Key, f.root)
	}
	if project.LaunchPaneID != "w1:p2" || project.PublicPaneID != "w1:p2" {
		t.Errorf("pane identity = %q/%q, want w1:p2 for both (loaded before the agent started)",
			project.LaunchPaneID, project.PublicPaneID)
	}
	if project.RepoWorkspaceID != "w1" || project.CurrentWorkspaceID != "w1" {
		t.Errorf("workspace = %q/%q, want the existing w1", project.RepoWorkspaceID, project.CurrentWorkspaceID)
	}
	if project.OMPSessionID != "omp-session-1" {
		t.Errorf("OMPSessionID = %q, want omp-session-1 from `agent get`", project.OMPSessionID)
	}
	if !strings.HasPrefix(project.AgentName, "orch_p_") {
		t.Errorf("AgentName = %q, want the derived primary name", project.AgentName)
	}
	if f.countCalls("workspace create") != 0 {
		t.Error("bind created a workspace although the named workspace already represents the root")
	}
	if f.countCalls("agent start") != 1 {
		t.Errorf("agent start calls = %d, want 1", f.countCalls("agent start"))
	}
	ready, err := f.store.Ready(context.Background(), "w1:p2", task.Announcement{ID: "omp-session-1"})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !ready {
		t.Error("bind returned without confirming the extension's ready handshake")
	}
}

func TestBindCreatesAWorkspaceWhenNoneRepresentsTheRoot(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{BindPaneID: "w9:p9", BindPaneCwd: t.TempDir(), BindPaneWorkspace: "w9"})
	project := f.mustBind()
	if project.RepoWorkspaceID == "" || project.LaunchPaneID == "" {
		t.Fatalf("project = %+v, want a created workspace and root pane", project)
	}
	if project.RepoWorkspaceID == "w9" {
		t.Error("bind reused a workspace whose root pane works in a different directory")
	}
	if f.countCalls("workspace create") != 1 {
		t.Errorf("workspace create calls = %d, want 1", f.countCalls("workspace create"))
	}
	created := f.calls()
	argv := created[0].Argv
	if len(argv) != 5 || argv[0] != "workspace" || argv[1] != "create" || argv[2] != "--cwd" || argv[4] != "--no-focus" {
		t.Errorf("workspace create argv = %q, want workspace create --cwd <root> --no-focus", argv)
	}
	if !samePath(argv[3], f.root) {
		t.Errorf("workspace create --cwd %q, want the canonical root", argv[3])
	}
}

func TestBindStartsADedicatedPrimaryWhenThePaneAlreadyRunsAnAgent(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{
		BindPaneID:        "w9:p2",
		BindPaneCwd:       "",
		BindPaneWorkspace: "w9",
		BindPaneAgent:     "omp",
	})
	f.plan.BindPaneCwd = f.root
	f.writePlan()
	project, err := f.bind("w9", "w9:p2")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if project.LaunchPaneID == "w9:p2" || project.RepoWorkspaceID == "w9" {
		t.Fatalf("project = %+v, want a dedicated workspace, not the occupied pane's", project)
	}
	if f.countCalls("workspace create") != 1 {
		t.Errorf("workspace create calls = %d, want 1", f.countCalls("workspace create"))
	}
	for _, call := range f.calls() {
		if len(call.Argv) >= 5 && call.Argv[0] == "agent" && call.Argv[1] == "start" {
			if call.Argv[4] == "w9:p2" {
				t.Fatalf("agent start reused the occupied pane: %q", call.Argv)
			}
		}
	}
	if f.countCalls("agent start") != 1 {
		t.Errorf("agent start calls = %d, want 1", f.countCalls("agent start"))
	}
}

func TestBindRetriesAnInterruptedPrimaryInTheSamePane(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	if _, err := f.bind("", ""); err == nil {
		t.Fatal("bind succeeded although the primary agent could not start")
	}
	project, err := f.store.Project(context.Background(), f.root)
	if err != nil {
		t.Fatalf("the interrupted bind recorded no project: %v", err)
	}
	if project.LaunchPaneID == "" {
		t.Fatal("the interrupted bind recorded no pane")
	}
	if note, err := f.store.Project(context.Background(), f.root); err != nil || note.OMPSessionID != "" {
		t.Errorf("an interrupted bind recorded a session: %v/%q", err, note.OMPSessionID)
	}

	f.updatePlan(func(p *fixturePlan) { p.AgentStartError = "" })
	retried, err := f.bind("", "")
	if err != nil {
		t.Fatalf("bind after the failure was fixed: %v", err)
	}
	if retried.LaunchPaneID != project.LaunchPaneID {
		t.Errorf("retry bound pane %q, want the recorded %q", retried.LaunchPaneID, project.LaunchPaneID)
	}
	if retried.OMPSessionID == "" {
		t.Error("retry did not record the primary's OMP session")
	}
	if f.countCalls("workspace create") != 1 {
		t.Errorf("workspace create calls = %d, want 1 (the retry reuses the recorded workspace)", f.countCalls("workspace create"))
	}
	if f.countCalls("agent start") != 2 {
		t.Errorf("agent start calls = %d, want 2 (the failed attempt and the retry)", f.countCalls("agent start"))
	}
}

func TestBindRejectsADifferentRepositoryUnderTheSamePane(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{
		BindPaneID:        "w1:p2",
		BindPaneCwd:       t.TempDir(),
		BindPaneWorkspace: "w1",
	})
	if _, err := f.bind("w1", "w1:p2"); !errors.Is(err, ErrDifferentRepository) {
		t.Fatalf("Bind err = %v, want ErrDifferentRepository", err)
	}
	if f.countCalls("agent start") != 0 || f.countCalls("workspace create") != 0 {
		t.Errorf("a rejected bind mutated Herdr: calls = %v", f.calls())
	}
	if _, err := f.store.Project(context.Background(), f.root); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a rejected bind recorded a project: err = %v", err)
	}
}

func TestBindRejectsANonRepository(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	if _, err := f.coord.Bind(context.Background(), t.TempDir(), "", ""); !errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("Bind err = %v, want ErrNotGitRepository", err)
	}
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func TestDispatchDoesNotCopyADirtyPrimaryCheckout(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	// An uncommitted change in the primary checkout must never reach a worker.
	writeRepoFile(t, f.root, "src/main.txt", "second\nuncommitted primary edit\n")

	st := f.mustDispatch("isolate a change", "Change the parser.")
	if st.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q (second=%v)", st.Phase, task.PromptSubmitted, st.Report)
	}
	wt := f.worktreeFor(t, st.Branch)
	if st.WorktreePath != wt.Path || st.WorktreeWorkspaceID != wt.WorkspaceID {
		t.Errorf("recorded worktree = %q/%q, want the created %q/%q", st.WorktreePath, st.WorktreeWorkspaceID, wt.Path, wt.WorkspaceID)
	}
	if status := gitOutput(t, wt.Path, "status", "--porcelain"); status != "" {
		t.Errorf("worker checkout is dirty: %q", status)
	}
	if head := gitOutput(t, wt.Path, "rev-parse", "HEAD"); head != f.base {
		t.Errorf("worker HEAD = %s, want the committed base revision %s", head, f.base)
	}
	if dirty := gitOutput(t, f.root, "status", "--porcelain"); !strings.Contains(dirty, "src/main.txt") {
		t.Errorf("the primary's own checkout was disturbed: %q", dirty)
	}
}

func TestDispatchRequiresACommittedSHAAsBaseRevision(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	for _, base := range []string{"HEAD", "main", "not-a-sha-at-all"} {
		if _, err := f.dispatch("bad base", "brief", []string{"check"}, nil, nil, base); !errors.Is(err, ErrBaseRevision) {
			t.Errorf("dispatch(base=%q) err = %v, want ErrBaseRevision", base, err)
		}
	}
	if f.countCalls("worktree create") != 0 {
		t.Errorf("a rejected base revision created a checkout: %v", f.calls())
	}
	// A committed short SHA resolves to the full commit and is recorded as such.
	st, err := f.dispatch("short base", "brief", []string{"check"}, nil, nil, f.base[:12])
	if err != nil {
		t.Fatalf("dispatch(short base): %v", err)
	}
	if st.BaseRevision != f.base {
		t.Errorf("BaseRevision = %q, want the full commit %q", st.BaseRevision, f.base)
	}
	unknown := strings.Repeat("a", 40)
	if _, err := f.dispatch("unknown base", "brief", []string{"check"}, nil, nil, unknown); !errors.Is(err, ErrBaseRevision) {
		t.Errorf("dispatch(unknown sha) err = %v, want ErrBaseRevision", err)
	}
}

func TestConcurrencyCapsAtThreeAndQueuesTheFourth(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	workers := make([]task.Task, 0, 3)
	for i, title := range []string{"one", "two", "three"} {
		st, err := f.dispatch(title, "brief "+title, []string{"go test ./... ok"}, nil, []string{fmt.Sprintf("src/%s.txt", title)}, f.base)
		if err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
		if st.Phase != task.PromptSubmitted {
			t.Fatalf("dispatch %s phase = %q, want %q", title, st.Phase, task.PromptSubmitted)
		}
		if got := task.StageOf(st); got != task.Running {
			t.Errorf("task %d stage = %q, want running", i, got)
		}
		workers = append(workers, st)
	}
	if f.countCalls("worktree create") != 3 || f.countCalls("agent start") != 3 || f.countCalls("agent prompt") != 3 {
		t.Fatalf("three dispatches did not start three workers: %v", f.calls())
	}

	queued, err := f.dispatch("four", "brief four", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch four: %v", err)
	}
	if queued.Phase != task.Intent {
		t.Errorf("fourth task phase = %q, want %q", queued.Phase, task.Intent)
	}
	if got := task.StageOf(queued); got != task.Queued {
		t.Errorf("fourth task stage = %q, want queued", got)
	}
	if queued.WorktreePath != "" || queued.LaunchPaneID != "" {
		t.Errorf("a queued task holds resources: %+v", queued)
	}
	if f.countCalls("worktree create") != 3 {
		t.Errorf("a queued task created a checkout: %d worktree create calls", f.countCalls("worktree create"))
	}
	// A queued task stays queued while the cap is full...
	if resumed, err := f.resume(queued.ID); err != nil || resumed.Phase != task.Intent {
		t.Errorf("resume(queued) = (%q, %v), want an untouched queued task", resumed.Phase, err)
	}
	// ...and is provisioned once a reviewed worker frees its slot.
	f.block(workers[0], func(next *task.Task) {
		next.Disposition = "integrated"
	})
	resumed, err := f.resume(queued.ID)
	if err != nil {
		t.Fatalf("resume after a freed slot: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Errorf("resume after a freed slot phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
}

func TestDispatchWaitsForDependenciesUntilTheyAreReviewed(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	first := f.mustDispatch("first", "First brief.")

	second, err := f.dispatch("second", "Second brief.", []string{"go test ./... ok"}, []string{first.ID}, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch dependent task: %v", err)
	}
	if second.Phase != task.Intent || task.StageOf(second) != task.Queued {
		t.Fatalf("dependent task = %q/%q, want a queued intent", second.Phase, task.StageOf(second))
	}
	if f.countCalls("worktree create") != 1 {
		t.Fatalf("dependent task started a worker before its dependency was reviewed: %v", f.calls())
	}
	if resumed, err := f.resume(second.ID); err != nil || resumed.Phase != task.Intent {
		t.Fatalf("resume before review = (%q, %v), want still queued", resumed.Phase, err)
	}

	first = f.satisfyReview(first)
	if got := task.StageOf(first); got != task.ReadyForReview {
		t.Fatalf("dependency stage = %q, want ready_for_review", got)
	}
	resumed, err := f.resume(second.ID)
	if err != nil {
		t.Fatalf("resume after review: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Errorf("resume after review phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
	if f.countCalls("worktree create") != 2 {
		t.Errorf("worktree create calls = %d, want 2", f.countCalls("worktree create"))
	}
}

func TestDispatchRejectsUnknownDependencies(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	if _, err := f.dispatch("dep", "brief", []string{"check"}, []string{"orch_missing"}, nil, f.base); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("dispatch(unknown dependency) err = %v, want ErrInvalidRequest", err)
	}
}

func TestDispatchRefusesOverlappingReservedPaths(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	owner, err := f.dispatch("owner", "Owns the parser.", []string{"check"}, nil, []string{"src/main.txt", "src/lib"}, f.base)
	if err != nil {
		t.Fatalf("dispatch owner: %v", err)
	}
	if len(owner.ReservedPaths) != 2 {
		t.Fatalf("owner reserved paths = %v, want both recorded", owner.ReservedPaths)
	}

	for _, reserved := range [][]string{{"src/main.txt"}, {"src"}, {"SRC\\MAIN.TXT"}, {"src/lib/parser.go"}} {
		_, err := f.dispatch("thief", "brief", []string{"check"}, nil, reserved, f.base)
		if !errors.Is(err, ErrReservedConflict) {
			t.Fatalf("dispatch(reserved=%v) err = %v, want ErrReservedConflict", reserved, err)
		}
		if !strings.Contains(err.Error(), owner.ID) {
			t.Errorf("overlap error %q does not name the owning task %q", err, owner.ID)
		}
	}
	if f.countCalls("worktree create") != 1 {
		t.Errorf("a refused dispatch created a checkout: %d worktree create calls", f.countCalls("worktree create"))
	}
	if f.countCalls("agent prompt") != 1 {
		t.Errorf("a refused dispatch prompted an agent: %d prompts", f.countCalls("agent prompt"))
	}
	// Non-overlapping ownership is fine.
	if _, err := f.dispatch("sibling", "brief", []string{"check"}, nil, []string{"src/other.txt"}, f.base); err != nil {
		t.Fatalf("dispatch(non-overlapping) err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Retry safety
// ---------------------------------------------------------------------------

func TestRetryAtEachPersistedPhaseRepeatsNoSideEffect(t *testing.T) {
	t.Run("intent", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		gate := f.mustDispatch("gate", "gate brief")
		queued, err := f.dispatch("dependent", "brief dependent", []string{"check"}, []string{gate.ID}, nil, f.base)
		if err != nil {
			t.Fatalf("dispatch dependent: %v", err)
		}
		if queued.Phase != task.Intent {
			t.Fatalf("phase = %q, want %q", queued.Phase, task.Intent)
		}
		if f.countCalls("worktree create") != 1 {
			t.Fatalf("a gated task was provisioned: %d worktree create calls", f.countCalls("worktree create"))
		}
		gate = f.satisfyReview(gate)
		resumed, err := f.resume(queued.ID)
		if err != nil {
			t.Fatalf("resume from intent: %v", err)
		}
		if resumed.Phase != task.PromptSubmitted {
			t.Fatalf("resumed phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
		}
		if f.countCalls("worktree create") != 2 {
			t.Errorf("worktree create calls = %d, want 2 (one per provisioned task)", f.countCalls("worktree create"))
		}
		if resumed.WorktreePath != f.worktreeFor(t, resumed.Branch).Path {
			t.Errorf("resumed worktree = %q, want the checkout for its own branch", resumed.WorktreePath)
		}
	})

	t.Run("worktree_created", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
		st, err := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
		if !errors.Is(err, herdr.ErrCLI) {
			t.Fatalf("dispatch err = %v, want a Herdr CLI failure", err)
		}
		if st.Phase != task.WorktreeCreated {
			t.Fatalf("phase = %q, want %q", st.Phase, task.WorktreeCreated)
		}
		if got := task.StageOf(st); got != task.NeedsInput {
			t.Fatalf("stage = %q, want needs_input", got)
		}
		if st.Attention == "" || !strings.Contains(st.Attention, st.LaunchPaneID) {
			t.Errorf("attention = %q, want the recorded pane %q named", st.Attention, st.LaunchPaneID)
		}
		if st.Blocker != "" {
			t.Errorf("a definite, retryable start failure was recorded as terminal: %q", st.Blocker)
		}
		firstWorktree := st.WorktreePath
		f.updatePlan(func(p *fixturePlan) { p.AgentStartError = "" })

		resumed, err := f.resume(st.ID)
		if err != nil {
			t.Fatalf("resume from worktree_created: %v", err)
		}
		if resumed.Phase != task.PromptSubmitted {
			t.Fatalf("resumed phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
		}
		if f.countCalls("worktree create") != 1 {
			t.Errorf("retry recreated a worktree: %d worktree create calls", f.countCalls("worktree create"))
		}
		if resumed.WorktreePath != firstWorktree {
			t.Errorf("resumed worktree = %q, want the recorded %q", resumed.WorktreePath, firstWorktree)
		}
		if f.countCalls("agent start") != 2 {
			t.Errorf("agent start calls = %d, want 2 (the failed attempt and the retry)", f.countCalls("agent start"))
		}
		// The real repository holds exactly one checkout for this branch.
		list := gitOutput(t, f.root, "worktree", "list", "--porcelain")
		if strings.Count(list, "worktree ") != 2 {
			t.Errorf("repository worktrees = %q, want the primary plus one worker", list)
		}
	})

	t.Run("agent_started", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{SuppressReady: true})
		st, err := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
		if !errors.Is(err, ErrReadinessTimeout) {
			t.Fatalf("dispatch err = %v, want ErrReadinessTimeout", err)
		}
		if st.Phase != task.AgentStarted {
			t.Fatalf("phase = %q, want %q", st.Phase, task.AgentStarted)
		}
		if st.OMPSessionID == "" {
			t.Fatal("the recorded session was not persisted with the agent_started phase")
		}
		if got := task.StageOf(st); got != task.NeedsInput {
			t.Errorf("stage = %q, want needs_input", got)
		}
		if len(f.prompts(st.PublicPaneID)) != 0 {
			t.Error("a brief was sent before the extension announced readiness")
		}
		f.updatePlan(func(p *fixturePlan) { p.SuppressReady = false })
		resumed, err := f.resume(st.ID)
		if err != nil {
			t.Fatalf("resume from agent_started: %v", err)
		}
		if resumed.Phase != task.PromptSubmitted {
			t.Fatalf("resumed phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
		}
		if f.countCalls("agent start") != 1 {
			t.Errorf("retry started a second OMP: %d agent start calls", f.countCalls("agent start"))
		}
		if f.countCalls("worktree create") != 1 {
			t.Errorf("retry recreated the worktree: %d worktree create calls", f.countCalls("worktree create"))
		}
		if got := f.prompts(st.PublicPaneID); len(got) != 1 {
			t.Errorf("prompts = %v, want exactly one brief", got)
		}
	})

	t.Run("prompt_submitted", func(t *testing.T) {
		f := newCoordFixture(t, fixturePlan{})
		st := f.mustDispatch("one", "brief one")
		before := len(f.calls())
		resumed, err := f.resume(st.ID)
		if err != nil {
			t.Fatalf("resume from prompt_submitted: %v", err)
		}
		if resumed.Phase != task.PromptSubmitted {
			t.Fatalf("resumed phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
		}
		if after := len(f.calls()); after != before {
			t.Errorf("resuming a dispatched task called Herdr %d more times: %v", after-before, f.calls()[before:])
		}
		if got := f.prompts(st.PublicPaneID); len(got) != 1 {
			t.Errorf("prompts = %v, want exactly one brief", got)
		}
	})
}

func TestDispatchLeavesAnActionableTaskWhenTheWorktreeCreateFails(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{WorktreeCreateError: "not_git_worktree"})
	st, err := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
	if !errors.Is(err, herdr.ErrCLI) {
		t.Fatalf("dispatch err = %v, want a Herdr CLI failure", err)
	}
	if task.StageOf(st) != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", task.StageOf(st))
	}
	if st.Attention == "" {
		t.Fatal("a failed create left no actionable attention")
	}
	for _, want := range []string{st.ID, st.Branch, f.root} {
		if !strings.Contains(st.Attention, want) {
			t.Errorf("attention %q does not name %q", st.Attention, want)
		}
	}
	if st.Phase != task.Intent {
		t.Errorf("phase = %q, want %q", st.Phase, task.Intent)
	}
	if st.WorktreePath != "" || st.LaunchPaneID != "" {
		t.Errorf("a failed create recorded resources: %+v", st)
	}
	// Retrying a definite failure is allowed and must not double-create.
	f.updatePlan(func(p *fixturePlan) { p.WorktreeCreateError = "" })
	resumed, err := f.resume(st.ID)
	if err != nil {
		t.Fatalf("resume after a fixed create: %v", err)
	}
	if resumed.ID != st.ID || resumed.Phase != task.PromptSubmitted {
		t.Errorf("resumed = %q/%q, want the same task dispatched", resumed.ID, resumed.Phase)
	}
	if f.countCalls("worktree create") != 2 {
		t.Errorf("worktree create calls = %d, want 2 (failed attempt plus retry)", f.countCalls("worktree create"))
	}
}

func TestRetryAdoptsACheckoutWhosePhaseWasNeverRecorded(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	// Pin the generated identity so the test can leave behind exactly the
	// checkout a crashed attempt would have created.
	identity := strings.Repeat("ab", taskIDBytes)
	f.coord.random = func(buf []byte) error {
		copy(buf, decodeHex(t, identity))
		return nil
	}
	branch := taskBranchBase + identity
	f.projectKey()

	path := filepath.Join(f.dir, "worktrees", "orch-"+identity)
	if out, err := runGit(f.root, "worktree", "add", "-b", branch, path, f.base); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	state := f.state()
	state.ensure()
	state.NextWorkspace++
	ws := fmt.Sprintf("w%d", state.NextWorkspace)
	state.Workspaces[ws] = path
	state.Worktrees = append(state.Worktrees, fixtureWorktree{
		Path: path, Branch: branch, Base: f.base, WorkspaceID: ws, PaneID: ws + ":p1",
	})
	if err := writeJSONFile(filepath.Join(f.dir, fixtureStateName), state); err != nil {
		t.Fatalf("write fixture state: %v", err)
	}

	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch with an unrecorded checkout: %v", err)
	}
	if st.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q (attention=%q)", st.Phase, task.PromptSubmitted, st.Attention)
	}
	if f.countCalls("worktree create") != 0 {
		t.Errorf("an existing checkout was created again: %d worktree create calls", f.countCalls("worktree create"))
	}
	if !samePath(st.WorktreePath, path) {
		t.Errorf("recorded worktree = %s, want the existing checkout %s", st.WorktreePath, path)
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts = %v, want one brief", got)
	}
}

func TestRetryAmbiguousPromptIsNeverResent(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{PromptAmbiguous: true})
	st, err := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
	if !errors.Is(err, herdr.ErrAmbiguous) {
		t.Fatalf("dispatch err = %v, want an ambiguous outcome", err)
	}
	if task.StageOf(st) != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", task.StageOf(st))
	}
	if st.Blocker == "" {
		t.Fatal("an ambiguous submission left no actionable blocker")
	}
	if st.Phase == task.PromptSubmitted {
		t.Error("an ambiguous submission was recorded as delivered")
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 1 {
		t.Fatalf("prompts = %v, want the one ambiguous submission recorded by the CLI", got)
	}
	if _, err := f.resume(st.ID); !errors.Is(err, ErrBlocked) {
		t.Fatalf("resume(ambiguous) err = %v, want ErrBlocked", err)
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts after resume = %v, want no resubmission", got)
	}
	if f.countCalls("agent prompt") != 1 {
		t.Errorf("agent prompt calls = %d, want 1", f.countCalls("agent prompt"))
	}
}

func TestRetryRestartedWorkerSessionIsNotPrompted(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{RestartBeforeReady: true})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("dispatch err = %v, want ErrAgentChanged", err)
	}
	if task.StageOf(st) != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", task.StageOf(st))
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 0 {
		t.Errorf("prompts = %v, want none for a restarted session", got)
	}
	if st.OMPSessionID == "" || strings.HasSuffix(st.OMPSessionID, "-restarted") {
		t.Errorf("recorded session = %q, want the session the first read reported", st.OMPSessionID)
	}
	if live := f.state().Agents[st.LaunchPaneID]; live == nil || live.Session != st.OMPSessionID {
		t.Errorf("recorded session = %q, want the pane's own session %v", st.OMPSessionID, live)
	}
}

func TestDispatchRecordsTheBriefAsOneArgvElement(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	brief := "Say \"hi\"\nand then run `git status`;\nrm -rf nothing"
	if _, err := f.dispatch("quoted", brief, []string{"go test ./... ok"}, nil, nil, f.base); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	for _, call := range f.calls() {
		if len(call.Argv) >= 2 && call.Argv[0] == "agent" && call.Argv[1] == "prompt" {
			if len(call.Argv) != 4 {
				t.Fatalf("agent prompt argv = %q, want exactly target and one brief element", call.Argv)
			}
			if call.Argv[3] != brief {
				t.Errorf("brief argv = %q, want the brief verbatim", call.Argv[3])
			}
			return
		}
	}
	t.Fatal("no agent prompt call was made")
}

// ---------------------------------------------------------------------------
// the native session reference Herdr reports
// ---------------------------------------------------------------------------

func TestDispatchRecordsHerdrsPathFormSessionReference(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{SessionRefKind: "PATH"})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch with a path-form reference: %v", err)
	}
	if st.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q (attention=%q blocker=%q)", st.Phase, task.PromptSubmitted, st.Attention, st.Blocker)
	}
	// The recorded identity is the value Herdr reported, and the handshake
	// matched it in the path form the extension announced.
	live := f.state().Agents[st.LaunchPaneID]
	if live == nil {
		t.Fatalf("no live agent on the recorded pane %q", st.LaunchPaneID)
	}
	if !samePath(st.OMPSessionID, sessionFileFor(live.Session)) {
		t.Errorf("recorded identity = %q, want the session file %q Herdr reported",
			st.OMPSessionID, sessionFileFor(live.Session))
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts = %v, want one brief", got)
	}
}

func TestDispatchRefusesAnUnrecognizedSessionReference(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{SessionRefKind: "pid"})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if !errors.Is(err, ErrSessionRefUnsupported) {
		t.Fatalf("dispatch err = %v, want ErrSessionRefUnsupported", err)
	}
	if got := task.StageOf(st); got != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", got)
	}
	// A shape this build cannot read is not something a retry can fix.
	if st.Blocker == "" || !strings.Contains(st.Blocker, "pid") {
		t.Errorf("blocker = %q, want the unsupported kind named", st.Blocker)
	}
	if st.Attention != "" {
		t.Errorf("attention = %q, want nothing retryable about an unreadable shape", st.Attention)
	}
	if len(f.prompts(st.PublicPaneID)) != 0 {
		t.Error("a brief was sent although the session reference could not be read")
	}
}

func TestDispatchPollsUntilHerdrReportsTheReference(t *testing.T) {
	// The integration reports session identity asynchronously: the first reads of
	// the pane carry no reference at all, and that is not a failure.
	f := newCoordFixture(t, fixturePlan{SessionRefAfterGets: 2})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch while the reference was still absent: %v", err)
	}
	if st.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q (attention=%q)", st.Phase, task.PromptSubmitted, st.Attention)
	}
	if st.OMPSessionID == "" {
		t.Error("the reported reference was not recorded")
	}
	if f.countCalls("agent start") != 1 {
		t.Errorf("agent start calls = %d, want 1", f.countCalls("agent start"))
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts = %v, want one brief", got)
	}
}

func TestDispatchReportsAnActionableErrorWhenNoReferenceArrives(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{SuppressSession: true})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("dispatch err = %v, want ErrSessionUnavailable", err)
	}
	if got := task.StageOf(st); got != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", got)
	}
	for _, want := range []string{st.ID, "reference"} {
		if !strings.Contains(st.Attention, want) {
			t.Errorf("attention %q does not name %q", st.Attention, want)
		}
	}
	if len(f.prompts(st.PublicPaneID)) != 0 {
		t.Error("a brief was sent without a session reference")
	}
}

// ---------------------------------------------------------------------------
// the session hint bind reports
// ---------------------------------------------------------------------------

func TestBindReportsNoResumeHintForAnUnmaterializedSession(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{SessionOnDisk: false})
	project := f.mustBind()
	hint, err := f.coord.SessionHint(context.Background(), project.LaunchPaneID)
	if err != nil {
		t.Fatalf("SessionHint: %v", err)
	}
	if !strings.HasPrefix(hint.ID, "omp-session-") {
		t.Errorf("hint id = %q, want the announced session id", hint.ID)
	}
	if hint.OnDisk {
		t.Error("hint claims the session is materialized")
	}
	if hint.Resume != "" {
		t.Errorf("hint resume = %q, want none for an unmaterialized session", hint.Resume)
	}
	if !strings.Contains(hint.ResumeUnavailable, "not materialized") {
		t.Errorf("hint resumeUnavailable = %q, want the reason", hint.ResumeUnavailable)
	}
}

func TestBindReportsAResumeHintForAMaterializedSession(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{SessionOnDisk: true})
	project := f.mustBind()
	hint, err := f.coord.SessionHint(context.Background(), project.LaunchPaneID)
	if err != nil {
		t.Fatalf("SessionHint: %v", err)
	}
	if !hint.OnDisk || hint.ResumeUnavailable != "" {
		t.Fatalf("hint = %+v, want a materialized session with no caveat", hint)
	}
	if want := "omp --resume=" + hint.ID; hint.Resume != want {
		t.Errorf("hint resume = %q, want %q", hint.Resume, want)
	}
}

func TestBindAcceptsAnUndefinedSessionFile(t *testing.T) {
	// getSessionFile() may be undefined before the runtime materializes a session;
	// the id alone is then the whole identity surface.
	f := newCoordFixture(t, fixturePlan{SessionFileEmpty: true, SessionOnDisk: false})
	project := f.mustBind()
	hint, err := f.coord.SessionHint(context.Background(), project.LaunchPaneID)
	if err != nil {
		t.Fatalf("SessionHint: %v", err)
	}
	if hint.File != "" || hint.ID == "" {
		t.Errorf("hint = %+v, want an id and no file", hint)
	}
	if hint.Resume != "" {
		t.Errorf("hint resume = %q, want none", hint.Resume)
	}
	if project.OMPSessionID == "" {
		t.Error("bind recorded no session identity")
	}
}

func TestBindReportsAPathReferenceWithAFullIdentity(t *testing.T) {
	// The annotated build reports kind=path: the binding records the session file
	// Herdr reported, and the hint still knows both identities because the
	// extension announced them.
	f := newCoordFixture(t, fixturePlan{SessionRefKind: "path", SessionOnDisk: true})
	project := f.mustBind()
	hint, err := f.coord.SessionHint(context.Background(), project.LaunchPaneID)
	if err != nil {
		t.Fatalf("SessionHint: %v", err)
	}
	if !samePath(project.OMPSessionID, hint.File) {
		t.Errorf("recorded identity = %q, want the announced file %q", project.OMPSessionID, hint.File)
	}
	if !strings.HasSuffix(hint.File, ".jsonl") {
		t.Errorf("hint file = %q, want the session file", hint.File)
	}
	if want := "omp --resume=" + hint.ID; hint.Resume != want {
		t.Errorf("hint resume = %q, want %q", hint.Resume, want)
	}
	if hint.ResumeUnavailable != "" {
		t.Errorf("hint resumeUnavailable = %q, want none for a materialized session", hint.ResumeUnavailable)
	}
}

// ---------------------------------------------------------------------------
// agent names
// ---------------------------------------------------------------------------

func TestAgentNamesAreUniqueAndLegal(t *testing.T) {
	// A worker's name is its task id: already unique, stable and legal.
	for _, id := range []string{"orch_08ed0e6352e9", "orch_ffffffffffff"} {
		name := WorkerAgentName(id)
		if name != id {
			t.Errorf("WorkerAgentName(%q) = %q, want the id", id, name)
		}
		if !herdr.ValidAgentName(name) {
			t.Errorf("WorkerAgentName(%q) = %q is not a legal Herdr agent name", id, name)
		}
		if len(name) > agentNameLimit {
			t.Errorf("worker name %q is %d characters, want at most %d", name, len(name), agentNameLimit)
		}
	}
	if name := WorkerAgentName(""); name != "" {
		t.Errorf("WorkerAgentName(\"\") = %q, want no name", name)
	}
	if name := WorkerAgentName("NOT A NAME"); name != "" {
		t.Errorf("WorkerAgentName(invalid) = %q, want no name", name)
	}

	// A primary's name is stable per repository, unique across repositories, and
	// distinct from every worker name.
	first := PrimaryAgentName(`E:\work\repo-a`)
	again := PrimaryAgentName(`e:/WORK/repo-a`)
	other := PrimaryAgentName(`E:\work\repo-b`)
	if first != again {
		t.Errorf("PrimaryAgentName is not stable: %q then %q", first, again)
	}
	if first == other {
		t.Errorf("two repositories share the primary name %q", first)
	}
	for _, name := range []string{first, other} {
		if !herdr.ValidAgentName(name) {
			t.Errorf("primary name %q is not a legal Herdr agent name", name)
		}
		if len(name) > agentNameLimit {
			t.Errorf("primary name %q is %d characters, want at most %d", name, len(name), agentNameLimit)
		}
	}
	if WorkerAgentName("orch_08ed0e6352e9") == first {
		t.Error("a worker and a primary would share a name")
	}
}

func TestConcurrencySecondWorkerStartsWithItsOwnAgentName(t *testing.T) {
	// The shipped bug: every agent was named "omp", so the second worker's start
	// collided with the running primary ("agent_name_taken"). The fixture now
	// enforces Herdr's live-name uniqueness, so this test fails if two agents share
	// a name.
	f := newCoordFixture(t, fixturePlan{})
	first, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	second, err := f.dispatch("two", "brief two", []string{"go test ./... ok"}, nil, []string{"src/other.txt"}, f.base)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if first.AgentName == "" || second.AgentName == "" {
		t.Fatalf("agent names = %q / %q, want both recorded", first.AgentName, second.AgentName)
	}
	if first.AgentName == second.AgentName {
		t.Fatalf("both workers got the name %q, which Herdr rejects", first.AgentName)
	}
	if first.AgentName != first.ID || second.AgentName != second.ID {
		t.Errorf("agent names = %q / %q, want the task ids", first.AgentName, second.AgentName)
	}

	// The live names really are distinct, and the primary is a third one: every
	// started agent is recorded under the name Herdr reported.
	project, err := f.store.Project(context.Background(), f.root)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	live := map[string]string{}
	for pane, agent := range f.state().Agents {
		if agent == nil {
			continue
		}
		if other, taken := live[agent.Name]; taken {
			t.Fatalf("agents on %s and %s share the live name %q", other, pane, agent.Name)
		}
		live[agent.Name] = pane
	}
	if len(live) != 3 {
		t.Fatalf("live agents = %v, want the primary and two workers", live)
	}
	if _, ok := live[project.AgentName]; !ok {
		t.Errorf("the primary's recorded name %q is not live: %v", project.AgentName, live)
	}
	if project.AgentName == first.AgentName || project.AgentName == second.AgentName {
		t.Errorf("the primary shares a name with a worker: %q", project.AgentName)
	}
	if !herdr.ValidAgentName(project.AgentName) {
		t.Errorf("primary name %q is not a legal Herdr agent name", project.AgentName)
	}
}

func TestTakenAgentNameStaysActionable(t *testing.T) {
	// A genuine collision stays a definite failure: attention naming the pane, the
	// attempted command and Herdr's code, and no brief is sent.
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_name_taken"})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if !errors.Is(err, herdr.ErrCLI) {
		t.Fatalf("dispatch err = %v, want a Herdr CLI failure", err)
	}
	if st.Phase != task.WorktreeCreated {
		t.Fatalf("phase = %q, want %q", st.Phase, task.WorktreeCreated)
	}
	if task.StageOf(st) != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", task.StageOf(st))
	}
	for _, want := range []string{st.ID, st.LaunchPaneID, WorkerAgentName(st.ID), "agent_name_taken"} {
		if !strings.Contains(st.Attention, want) {
			t.Errorf("attention %q does not name %q", st.Attention, want)
		}
	}
	if st.Blocker != "" {
		t.Errorf("a definite start failure was recorded as terminal: %q", st.Blocker)
	}
	if len(f.prompts(st.PublicPaneID)) != 0 {
		t.Error("a brief was sent although the agent never started")
	}
}

func TestBindAdoptsTheRecordedPrimaryAgentOnARebind(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	project := f.mustBind()
	if project.AgentName == "" {
		t.Fatal("bind recorded no agent name")
	}
	before := f.countCalls("agent start")

	// Re-binding the same repository adopts the live agent that is already running
	// under the derived name instead of starting a second one.
	again, err := f.bind("", "")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if again.AgentName != project.AgentName {
		t.Errorf("rebind recorded agent name %q, want the live %q", again.AgentName, project.AgentName)
	}
	if after := f.countCalls("agent start"); after != before {
		t.Errorf("rebind called agent start %d more times, want none", after-before)
	}
	if got := f.state().Agents[project.LaunchPaneID]; got == nil || got.Name != project.AgentName {
		t.Errorf("the live agent on %s = %+v, want the recorded name %q", project.LaunchPaneID, got, project.AgentName)
	}
}

// ---------------------------------------------------------------------------
// writes, failures and their durability
// ---------------------------------------------------------------------------

func TestDispatchWritesEachStepOnTheCurrentRevision(t *testing.T) {
	// The shipped bug: another writer (the extension adopting its session through
	// the bridge) landed between the coordinator's read of the task and its write
	// of the agent name, so the write failed with a stale revision and the whole
	// dispatch stopped with nothing recorded.
	f := newCoordFixture(t, fixturePlan{AdoptDuringStart: true})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch with a concurrent writer: %v", err)
	}
	if st.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q (attention=%q blocker=%q)", st.Phase, task.PromptSubmitted, st.Attention, st.Blocker)
	}
	if st.Attention != "" || st.Blocker != "" {
		t.Errorf("a concurrent write was reported as a problem: attention=%q blocker=%q", st.Attention, st.Blocker)
	}
	if st.AgentName == "" {
		t.Error("the agent name was not recorded")
	}
	if st.OMPSessionID == "" {
		t.Error("the adopted session was not recorded")
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts = %v, want one brief", got)
	}
}

func TestDispatchFailureIsDurableAndActionable(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_name_taken"})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err == nil {
		t.Fatal("dispatch succeeded although the agent could not start")
	}
	// The durable record names the task, the phase it reached, the resources it
	// holds and the cause.
	if task.StageOf(st) != task.NeedsInput {
		t.Fatalf("stage = %q, want needs_input", task.StageOf(st))
	}
	if st.Attention == "" {
		t.Fatal("the failure was not persisted as attention")
	}
	for _, want := range []string{
		st.ID, "start the worker agent", "phase worktree_created", st.WorktreePath, st.LaunchPaneID, "agent_name_taken",
	} {
		if !strings.Contains(st.Attention, want) {
			t.Errorf("attention %q does not name %q", st.Attention, want)
		}
	}

	// The returned error is actionable: it names the step, the recorded state and
	// the way out, and it is not the bare "internal" the primary used to see.
	message := err.Error()
	for _, want := range []string{st.ID, "could not start the worker agent", "phase worktree_created", "agent_name_taken", "resumable"} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not name %q", message, want)
		}
	}
	if strings.HasPrefix(message, "internal:") {
		t.Errorf("error is still unclassified: %q", message)
	}
	// Classification is still reachable through the wrapper, and the failure is
	// not claimed to be an unavailable Herdr: it answered.
	if !errors.Is(err, herdr.ErrCLI) {
		t.Errorf("error %v does not classify as a Herdr CLI failure", err)
	}
	if errors.Is(err, ErrHerdrUnavailable) {
		t.Errorf("error %v is misclassified as an unavailable Herdr", err)
	}
	var step *StepError
	if !errors.As(err, &step) {
		t.Fatalf("error %v is not a *StepError", err)
	}
	if step.TaskID != st.ID || step.Step != "start the worker agent" || step.Phase != task.WorktreeCreated {
		t.Errorf("step error = %+v, want the recorded task, step and phase", step)
	}

	// The task is resumable: fixing the cause lets the recorded phase continue.
	f.updatePlan(func(p *fixturePlan) { p.AgentStartError = "" })
	resumed, err := f.resume(st.ID)
	if err != nil {
		t.Fatalf("resume after fixing the cause: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Errorf("resumed phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
	if f.countCalls("agent start") != 2 || f.countCalls("worktree create") != 1 {
		t.Errorf("resume did not build on the recorded phase: %v", f.calls())
	}
}

func TestFailureThatCannotBeRecordedIsReportedNotDropped(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	f.projectKey()
	// A task that does not exist cannot be updated: the attempt and its retry both
	// fail, and the caller has to learn that the durable failure is missing — while
	// still being told which step failed, what the task holds and that it is
	// resumable. Losing that context is the defect this test guards.
	ghost := task.Task{
		ID:                  "orch_000000000000",
		ProjectKey:          f.root,
		Branch:              "orch/000000000000",
		WorktreePath:        filepath.Join(f.dir, "worktrees", "orch-000000000000"),
		WorktreeWorkspaceID: "w2",
		PublicPaneID:        "w2:p1",
		AgentName:           "orch_000000000000",
		Phase:               task.WorktreeCreated,
		Revision:            7,
	}
	updated, err := f.coord.failTask(context.Background(), ghost, "start the worker agent", false,
		fmt.Errorf("%w: the agent could not start", herdr.ErrCLI))
	if err == nil {
		t.Fatal("failTask reported success although it could not write")
	}
	// Classification survives: the persistence failure and the underlying cause are
	// both reachable, and the step error is still the value a caller can inspect.
	if !errors.Is(err, ErrPersistence) {
		t.Errorf("err = %v, want ErrPersistence", err)
	}
	if !errors.Is(err, herdr.ErrCLI) {
		t.Errorf("err = %v, want the cause reachable through it", err)
	}
	var step *StepError
	if !errors.As(err, &step) {
		t.Errorf("err = %v, want the *StepError reachable through it", err)
	} else if step.Step != "start the worker agent" || step.Phase != task.WorktreeCreated {
		t.Errorf("step error = %+v, want the failed step and phase", step)
	}
	// The action context is complete: the step that failed, the phase reached, the
	// recorded resources, the cause, and what to do next.
	for _, want := range []string{
		"orch_000000000000",
		"start the worker agent",
		"phase worktree_created",
		"branch orch/000000000000",
		"worktree " + ghost.WorktreePath,
		"workspace w2",
		"pane w2:p1",
		"agent orch_000000000000",
		"the agent could not start",
		"resumable",
		"could not be recorded",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not name %q", err, want)
		}
	}
	if updated.Attention != "" || updated.Blocker != "" {
		t.Errorf("a ghost task was reported as updated: %+v", updated)
	}
}

func TestAmbiguousFailureThatCannotBeRecordedAdvisesInspectionNotResume(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	f.projectKey()
	// An ambiguous brief may already be the worker's turn. If the blocker cannot be
	// persisted either, the caller must not be told to resume: resuming would
	// resubmit a brief that may have been delivered.
	ghost := task.Task{
		ID:           "orch_000000000001",
		ProjectKey:   f.root,
		Branch:       "orch/000000000001",
		WorktreePath: filepath.Join(f.dir, "worktrees", "orch-000000000001"),
		PublicPaneID: "w2:p1",
		AgentName:    "orch_000000000001",
		Phase:        task.AgentStarted,
		Revision:     7,
	}
	cause := fmt.Errorf("%w: the brief on pane %s may already have been delivered; inspect `agent read %s` before deciding: %w",
		ErrAmbiguousStep, ghost.PublicPaneID, ghost.PublicPaneID, herdr.ErrAmbiguous)
	updated, err := f.coord.failTask(context.Background(), ghost, "submit the brief", true, cause)
	if err == nil {
		t.Fatal("failTask reported success although it could not write")
	}
	// Classification: the persistence failure, the ambiguity and the step error all
	// stay reachable.
	if !errors.Is(err, ErrPersistence) {
		t.Errorf("err = %v, want ErrPersistence", err)
	}
	if !errors.Is(err, ErrAmbiguousStep) || !errors.Is(err, herdr.ErrAmbiguous) {
		t.Errorf("err = %v, want the ambiguity still classified", err)
	}
	var step *StepError
	if !errors.As(err, &step) {
		t.Fatalf("err = %v, want the *StepError reachable through it", err)
	}
	if step.Step != "submit the brief" || step.Phase != task.AgentStarted {
		t.Errorf("step error = %+v, want the failed step and phase", step)
	}
	for _, want := range []string{
		"orch_000000000001",
		"submit the brief",
		"phase agent_started",
		"worktree " + ghost.WorktreePath,
		"pane w2:p1",
		"agent orch_000000000001",
		"could not be recorded",
		"before any retry",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not name %q", err, want)
		}
	}
	// The contradiction this test guards against: never recommend resuming (or
	// resubmitting) a brief whose delivery is unknown. A prohibition on
	// resubmitting is exactly right; a recommendation is not.
	for _, unwanted := range []string{"resume", "Resume", "resubmit it", "retry it"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("err %q recommends %q for an ambiguous, unrecorded failure", err, unwanted)
		}
	}
	if !strings.Contains(err.Error(), "do not resubmit") {
		t.Errorf("err %q does not forbid resubmitting an unidentified submission", err)
	}
	if updated.Attention != "" || updated.Blocker != "" {
		t.Errorf("a ghost task was reported as updated: %+v", updated)
	}
}

func TestDispatchToleratesTheBridgeAdoptingTheSessionFirst(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{AdoptBeforeRecord: true})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch with the bridge adopting the session first: %v", err)
	}
	if st.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q (blocker=%q)", st.Phase, task.PromptSubmitted, st.Blocker)
	}
	if st.OMPSessionID == "" {
		t.Fatal("the adopted session was not recorded on the task")
	}
	if st.Blocker != "" || st.Attention != "" {
		t.Errorf("an adoption race was reported as a problem: blocker=%q attention=%q", st.Blocker, st.Attention)
	}
	if f.countCalls("agent start") != 1 {
		t.Errorf("agent start calls = %d, want 1", f.countCalls("agent start"))
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts = %v, want one brief after readiness was confirmed", got)
	}
}

func TestRetryReconcilesAnAmbiguousWorktreeCreate(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{WorktreeCreateTimeout: true})
	st, err := f.dispatch("one", "brief one", []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch after an ambiguous create: %v", err)
	}
	if st.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q (attention=%q blocker=%q)", st.Phase, task.PromptSubmitted, st.Attention, st.Blocker)
	}
	if f.countCalls("worktree create") != 1 {
		t.Errorf("an ambiguous create was repeated: %d worktree create calls", f.countCalls("worktree create"))
	}
	wt := f.worktreeFor(t, st.Branch)
	if !samePath(st.WorktreePath, wt.Path) {
		t.Errorf("recorded worktree = %s, want the checkout Herdr lists %s", st.WorktreePath, wt.Path)
	}
	if st.LaunchPaneID != wt.PaneID {
		t.Errorf("recorded pane = %q, want the pane Herdr's snapshot reports %q", st.LaunchPaneID, wt.PaneID)
	}
	if f.countCalls("agent start") != 1 || f.countCalls("agent prompt") != 1 {
		t.Errorf("ambiguous create did not reach one agent and one brief: %v", f.calls())
	}
}

func TestDispatchRequiresARecordedProject(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	if _, err := f.coord.Dispatch(context.Background(), "E:/nowhere", "one", "brief", []string{"check"}, nil, nil, f.base); !errors.Is(err, ErrProjectUnknown) {
		t.Fatalf("dispatch without a project err = %v, want ErrProjectUnknown", err)
	}
}
