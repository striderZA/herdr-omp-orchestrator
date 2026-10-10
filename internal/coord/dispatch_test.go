package coord

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
	_ "modernc.org/sqlite"
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
	// fixtureCreateStartedName is the marker the stand-in writes while a delayed
	// `worktree create` is still answering, so a test can act in that window.
	fixtureCreateStartedName = "worktree_create_started"
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
	// IntegrationStatus is the text `herdr integration status` prints; empty
	// prints the line an installed, up-to-date OMP integration produces.
	IntegrationStatus string `json:"integration_status"`
	// SuppressReady makes the extension stand-in never record the handshake.
	SuppressReady bool `json:"suppress_ready"`
	// AnnounceOnPrompt makes the extension stand-in announce only in the turn an
	// `agent prompt` starts instead of as soon as the store binds the pane: an
	// OMP that is already running resolves its binding at its next turn.
	AnnounceOnPrompt bool `json:"announce_on_prompt"`
	// SuppressSession makes `agent get` report no native session reference.
	SuppressSession bool `json:"suppress_session"`
	// SuppressSessionFor makes `agent get` report no native session reference for
	// one pane only, which is how a test asks what happens when one agent's
	// identity cannot be proven while everything else stays healthy.
	SuppressSessionFor string `json:"suppress_session_for"`
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
	// AgentReadText is what `agent read` prints for a pane: the terminal text the
	// primary would show a user. An empty value prints a stable placeholder.
	AgentReadText string `json:"agent_read_text"`
	// AdoptDuringStart makes the extension (through the bridge) adopt its
	// announced session while `agent start` is still answering, which is what
	// bumps the task's revision between the coordinator's read and its write.
	AdoptDuringStart bool `json:"adopt_during_start"`
	// SnapshotProtocol overrides the socket protocol `api snapshot` reports;
	// zero reports the supported protocol.
	SnapshotProtocol int `json:"snapshot_protocol"`
	// PromptAmbiguous makes `agent prompt` report Herdr's own timeout code after
	// recording the submission, the way a real CLI timeout does.
	PromptAmbiguous bool `json:"prompt_ambiguous"`
	// PromptError makes `agent prompt` refuse with that Herdr code before any
	// input is written, the way a blocked or vanished agent is refused.
	PromptError string `json:"prompt_error"`
	// RecoverInboxDuringPrompt simulates a concurrent startup recovery after a
	// flush claimed its batch but before AgentPrompt returns.
	RecoverInboxDuringPrompt bool `json:"recover_inbox_during_prompt"`
	// PromptDelayMillis delays agent prompts after the subprocess starts, so
	// tests can exercise adjudication while an external write is still live.
	PromptDelayMillis int `json:"prompt_delay_millis"`
	// BlankCwdFor makes `agent get` report an empty working directory for one
	// pane, which is how a test asks what happens when the pane's checkout
	// cannot be read back.
	BlankCwdFor string `json:"blank_cwd_for"`
	// WorktreeCreateError makes `worktree create` fail with that Herdr code
	// before any checkout exists.
	WorktreeCreateError string `json:"worktree_create_error"`
	// WorktreeCreateDelayMillis delays that refusal: the stand-in records the call
	// and then waits, so a test can act while the attempt that owns the step is
	// still inside `worktree create`.
	WorktreeCreateDelayMillis int `json:"worktree_create_delay_millis"`
	// WorktreeCreateTimeout makes `worktree create` create the checkout and
	// then answer with Herdr's own timeout code, the way a real CLI timeout
	// arrives after the side effect.
	WorktreeCreateTimeout bool `json:"worktree_create_timeout"`
	// WorkspaceCreateTimeout creates the workspace, then reports a timeout as Herdr
	// does when the side effect completes but its response is lost.
	WorkspaceCreateTimeout bool `json:"workspace_create_timeout"`
	// AgentGetAliasFrom and AgentGetAliasTo model Herdr resolving a pane's
	// launch-time id to its current public id after a move.
	AgentGetAliasFrom string `json:"agent_get_alias_from"`
	AgentGetAliasTo   string `json:"agent_get_alias_to"`
	// AgentStartError makes `agent start` fail with that Herdr code.
	AgentStartError string `json:"agent_start_error"`
	// AgentStartTimeout makes `agent start` start the agent and then answer with
	// Herdr's own timeout code, the way a CLI timeout arrives after the effect.
	AgentStartTimeout bool `json:"agent_start_timeout"`
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
	// Kind is the agent Herdr reports for the pane; empty means omp.
	Kind string `json:"kind"`
}

// agentKindOf is the agent kind a stand-in agent reports.
func agentKindOf(agent *fixtureAgent) string {
	if agent != nil && agent.Kind != "" {
		return agent.Kind
	}
	return "omp"
}

// fixtureWorktree is one checkout the stand-in CLI created.
type fixtureWorktree struct {
	Path        string `json:"path"`
	Branch      string `json:"branch"`
	Base        string `json:"base"`
	WorkspaceID string `json:"workspace_id"`
	PaneID      string `json:"pane_id"`
	// Detached lists the checkout with a detached HEAD: no branch.
	Detached bool `json:"detached"`
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
		if plan.WorkspaceCreateTimeout {
			return fixtureError("timeout", "the command timed out after the workspace was created")
		}
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
	case "agent read":
		text := plan.AgentReadText
		if text == "" {
			text = "worker terminal\n> waiting for instructions"
		}
		if _, err := os.Stdout.WriteString(text); err != nil {
			return 30
		}
		return 0
	case "integration status":
		// Herdr prints a text report here, not the JSON envelope every other
		// command answers with.
		report := plan.IntegrationStatus
		if report == "" {
			report = "omp: current (v10) (C:\\Users\\fixture\\.omp\\agent\\extensions\\herdr-omp-agent-state.ts)"
		}
		if _, err := os.Stdout.WriteString(report + "\n"); err != nil {
			return 30
		}
		return 0
	default:
		return fixtureUnexpected(argv, "unsupported command")
	}
}

func fixtureWorktreeCreate(dir string, plan fixturePlan, state *fixtureState, argv []string) int {
	if plan.WorktreeCreateError != "" {
		if plan.WorktreeCreateDelayMillis > 0 {
			// The refusal is definite but slow to arrive: a test that wants to act
			// while this attempt still owns its step waits for the marker.
			if err := os.WriteFile(filepath.Join(dir, fixtureCreateStartedName), []byte("waiting"), 0o600); err != nil {
				return 25
			}
			time.Sleep(time.Duration(plan.WorktreeCreateDelayMillis) * time.Millisecond)
		}
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
		if out, err := runGitWorktreeAdd(root, branch, path, base); err != nil {
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
			agent = agentKindOf(state.Agents[pane])
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
	resolvedPane := pane
	if pane == plan.AgentGetAliasFrom && plan.AgentGetAliasTo != "" {
		resolvedPane = plan.AgentGetAliasTo
	}
	agent := state.Agents[resolvedPane]
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
	if plan.SuppressSessionFor != "" && plan.SuppressSessionFor == pane {
		// This one pane's agent has not published its identity, the way a pane
		// whose integration has not reported yet looks.
		reported = nil
	}
	if plan.RestartBeforeReady && fixtureBound(stateDirOf(), pane, announced) {
		// The extension announced this session, and Herdr now reports a different
		// one for the same pane: the process was restarted.
		recordFixtureReady(pane, announced)
		restarted := announced
		restarted.ID += "-restarted"
		reported = &task.SessionRef{Kind: task.SessionKindID, Value: restarted.ID}
	} else if !plan.SuppressReady && !plan.AnnounceOnPrompt && fixtureBound(stateDirOf(), pane, announced) {
		// The OMP extension can only announce readiness for a session the bridge
		// already resolves to its own launch pane.
		recordFixtureReady(pane, announced)
	}
	state.save(dir)
	return fixtureResult(map[string]any{
		"type":  "agent_info",
		"agent": agentInfo(*state, resolvedPane, fixtureCwd(plan, *state, resolvedPane), agent, reported),
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
	if plan.BlankCwdFor != "" && plan.BlankCwdFor == pane {
		// This pane's agent reports no working directory, the way a pane whose
		// process has not settled into a checkout looks.
		return ""
	}
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
	if plan.AgentStartTimeout {
		// The answer never arrived and nothing was recorded: the outcome is unknown
		// in both directions, so the coordinator cannot reconcile it.
		return fixtureError("timeout", "the command timed out before the agent was seen")
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
	if plan.RecoverInboxDuringPrompt {
		if err := recoverFixtureInbox(os.Getenv(fixtureEnvStore)); err != nil {
			return 31
		}
	}
	if plan.PromptError != "" {
		// Refused before anything was written: Herdr rejects a prompt to a
		// blocked or vanished agent without sending input.
		return fixtureError(plan.PromptError, "the prompt was refused before any input was written")
	}
	if plan.PromptDelayMillis > 0 {
		if err := os.WriteFile(filepath.Join(dir, "prompt_started"), []byte("waiting"), 0o600); err != nil {
			return 31
		}
		time.Sleep(time.Duration(plan.PromptDelayMillis) * time.Millisecond)
	}
	// The submission is recorded before the answer: a timeout here means the
	// brief may already have been delivered.
	state.Prompts[pane] = append(state.Prompts[pane], argv[3])
	agent.Prompted++
	agent.Status = "working"
	state.save(dir)
	if plan.AnnounceOnPrompt && !plan.SuppressReady {
		// The extension resolves and announces in the turn this prompt starts
		// (before_agent_start), once the store binds the pane's session.
		if announced := fixtureAnnouncement(plan, agent.Session); fixtureBound(stateDirOf(), pane, announced) {
			recordFixtureReady(pane, announced)
		}
	}
	if plan.PromptAmbiguous {
		return fixtureError("timeout", "the prompt may already have been submitted")
	}
	return fixtureResult(map[string]any{
		"type":  "agent_prompted",
		"agent": agentInfo(*state, pane, fixtureCwd(plan, *state, pane), agent, fixtureReference(plan, fixtureAnnouncement(plan, agent.Session))),
	})
}

func recoverFixtureInbox(storeDir string) error {
	store, err := state.Open(storeDir)
	if err != nil {
		return err
	}
	_, recoverErr := store.RecoverInbox(context.Background())
	return errors.Join(recoverErr, store.Close())
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
// the task or project already keyed to this pane: the extension resolves before
// the coordinator could have recorded the session it reads from `agent get`.
//
// Both roles are adopted the way the real bridge does it: a primary's announced
// native id is recorded even though `agent get` will report the session file, so
// a fixture that reports kind "path" exercises the cross-form handshake.
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
	if err == nil {
		if strings.TrimSpace(taskOnPane.OMPSessionID) != "" {
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
		return
	}
	primary, err := store.ProjectByLaunchPane(ctx, pane)
	if err != nil || strings.TrimSpace(primary.OMPSessionID) != "" {
		return
	}
	if err := store.UpdateProject(ctx, primary.Key, func(next *task.Project) error {
		if strings.TrimSpace(next.OMPSessionID) == "" {
			next.OMPSessionID = session
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fixture: adopt primary: %v\n", err)
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
		var branch any = wt.Branch
		if wt.Detached {
			branch = nil
		}
		worktrees = append(worktrees, map[string]any{
			"path":               wt.Path,
			"label":              filepath.Base(wt.Path),
			"branch":             branch,
			"open_workspace_id":  open,
			"is_bare":            false,
			"is_detached":        wt.Detached,
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
			"protocol":             snapshotProtocol(plan),
			"focused_pane_id":      "",
			"focused_tab_id":       "",
			"focused_workspace_id": "",
			"agents":               agents,
			"panes":                panes,
		},
	}
}

func snapshotProtocol(plan fixturePlan) int {
	if plan.SnapshotProtocol != 0 {
		return plan.SnapshotProtocol
	}
	return herdr.SupportedProtocol
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
		"agent":                   agentKindOf(agent),
		"display_agent":           agentKindOf(agent),
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

// runGitWorktreeAdd runs the one git command Herdr's `worktree create` runs, and
// retries briefly while another invocation holds git's lock.
//
// Herdr serializes worktree creation inside one session; the stand-in CLI is
// invoked as one process per call, so two concurrent dispatches can reach `git
// worktree add` at the same moment. Retrying on git's own lock keeps that
// fixture artefact from looking like a coordinator failure: what the tests
// measure is the durable dispatch gate and the recorded facts, not git's locking.
func runGitWorktreeAdd(root, branch, path, base string) (string, error) {
	var lastOut string
	var lastErr error
	for attempt := range 40 {
		if _, err := os.Stat(path); err == nil {
			// Another invocation already created it: the branch and path are what
			// this call asked for, so report success.
			return "", nil
		}
		out, err := runGit(root, "worktree", "add", "-b", branch, path, base)
		if err == nil {
			return out, nil
		}
		lastOut, lastErr = out, err
		if !strings.Contains(out, "index.lock") && !strings.Contains(out, "Unable to create") &&
			!strings.Contains(out, "already checked out") && !strings.Contains(out, "locked") {
			return out, err
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return lastOut, lastErr
}

func workspaceOfPane(pane string) string {
	ws, _, _ := strings.Cut(pane, ":")
	return ws
}

// ---------------------------------------------------------------------------
// test fixture
// ---------------------------------------------------------------------------

type coordFixture struct {
	t        *testing.T
	root     string
	base     string
	dir      string
	plan     fixturePlan
	store    *state.Store
	coord    *Coordinator
	client   *herdr.Client
	project  string
	stateDir string
}

// sibling opens a second store and coordinator over the same plugin state
// directory, standing in for a second bridge process dispatching for the same
// project: separate connections, one shared database.
func (f *coordFixture) sibling() *Coordinator {
	f.t.Helper()
	store, err := state.Open(f.stateDir)
	if err != nil {
		f.t.Fatalf("sibling state.Open: %v", err)
	}
	f.t.Cleanup(func() { _ = store.Close() })
	coordinator, err := New(store, f.client, Options{
		Concurrency:      3,
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
	})
	if err != nil {
		f.t.Fatalf("sibling coord.New: %v", err)
	}
	return coordinator
}

// spec describes one dispatch request for the concurrency tests.
type spec struct {
	title    string
	checks   []string
	deps     []string
	reserved []string
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
	f.stateDir = stateDir

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
		// These tests are about what happens after a satisfied gate; the gate
		// itself is exercised against the setup check in bind_preflight_test.go.
		Preflight: satisfiedPreflight,
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

// slotClaimToken reads the durable slot claim token of one task: the value the
// attempt that acquired the claim holds. It is read straight from the store so a
// test can present the claim an earlier attempt acquired, exactly as that attempt
// would when it gives the slot back.
func (f *coordFixture) slotClaimToken(id string) int64 {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		f.t.Fatalf("open state database: %v", err)
	}
	defer func() { _ = db.Close() }()
	var token int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT slot_token FROM tasks WHERE id = ?`, id).Scan(&token); err != nil {
		f.t.Fatalf("read the slot claim of %s: %v", id, err)
	}
	return token
}

func (f *coordFixture) prompts(pane string) []string {
	f.t.Helper()
	return f.state().Prompts[pane]
}

// satisfyReview marks a task reviewed the way a worker's final report does. A
// reviewed task is not closed yet: a dependent task stays queued until the primary
// records the disposition that closes it (recordDisposition below), and the
// worker's report alone is never the gate.
func (f *coordFixture) satisfyReview(st task.Task) task.Task {
	f.t.Helper()
	updated, err := f.store.UpdateTask(context.Background(), st.ID, st.Revision, func(next *task.Task) error {
		next.Report = &task.Report{
			Kind:     "result",
			Summary:  "done",
			Branch:   next.Branch,
			Evidence: []string{"commit " + f.base},
			Checks:   []task.Check{{Name: "go test ./...", Outcome: task.CheckPassed}},
			Risk:     "none",
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("mark %s reviewed: %v", st.ID, err)
	}
	return updated
}

// recordDisposition records the primary's decision for a task, the way Review
// does. The disposition is the gate a dependent task waits for, and it is the
// only thing that frees the reviewed task's slot.
func (f *coordFixture) recordDisposition(st task.Task, disposition string) task.Task {
	f.t.Helper()
	return f.block(st, func(next *task.Task) {
		next.Disposition = disposition
	})
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
	calls := f.calls()
	snapshotIndex, workspaceCreateIndex := -1, -1
	for i, call := range calls {
		if len(call.Argv) < 2 {
			continue
		}
		switch call.Argv[0] + " " + call.Argv[1] {
		case "api snapshot":
			if snapshotIndex < 0 {
				snapshotIndex = i
			}
		case "workspace create":
			workspaceCreateIndex = i
		}
	}
	if snapshotIndex < 0 || workspaceCreateIndex <= snapshotIndex {
		t.Fatalf("Herdr calls = %v, want the identity snapshot before workspace create", calls)
	}
	argv := calls[workspaceCreateIndex].Argv
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

func TestDispatchWaitsForDependenciesUntilTheDispositionIsRecorded(t *testing.T) {
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
		t.Fatalf("dependent task started a worker before its dependency was closed: %v", f.calls())
	}
	if resumed, resumeErr := f.resume(second.ID); resumed.Phase != task.Intent || resumed.Attention != "" {
		t.Fatalf("resume before the disposition = (%q, attention %q, err %v), want still queued", resumed.Phase, resumed.Attention, resumeErr)
	}

	// A final report is not a decision. The dependency is ready for review, which
	// says its worker stopped — not that the user accepted the work — so the task
	// waiting on it must stay queued until the primary records a disposition.
	first = f.satisfyReview(first)
	if got := task.StageOf(first); got != task.ReadyForReview {
		t.Fatalf("dependency stage = %q, want ready_for_review", got)
	}
	if resumed, resumeErr := f.resume(second.ID); resumed.Phase != task.Intent || resumed.Attention != "" {
		t.Fatalf("resume after the report = (%q, attention %q, err %v), want still queued until the disposition", resumed.Phase, resumed.Attention, resumeErr)
	}
	if created := f.countCalls("worktree create"); created != 1 {
		t.Fatalf("a final report unlocked its dependent: %d worktree create calls", created)
	}

	// The disposition is what opens the gate.
	first = f.recordDisposition(first, "accepted")
	if got := task.StageOf(first); got != task.Done {
		t.Fatalf("dependency stage = %q, want done", got)
	}
	resumed, err := f.resume(second.ID)
	if err != nil {
		t.Fatalf("resume after the disposition: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Errorf("resume after the disposition phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
	if f.countCalls("worktree create") != 2 {
		t.Errorf("worktree create calls = %d, want 2", f.countCalls("worktree create"))
	}
}

func TestDependentsReleaseOnlyOnAcceptingOutcomes(t *testing.T) {
	cases := []struct {
		outcome, disposition string
		released             bool
	}{
		{task.OutcomeIntegrated, "integrated", true},
		{task.OutcomeKept, "kept", true},
		{task.OutcomeRejected, "rejected", false},
		{task.OutcomeSuperseded, "superseded", false},
		{task.OutcomeAbandoned, "abandoned", false},
		{"", "legacy free text", true}, // rows recorded before the contract
	}
	for _, tc := range cases {
		dep := task.Task{ID: "dep", Disposition: tc.disposition, Outcome: tc.outcome}
		waiting := task.Task{ID: "waiting", Dependencies: []string{"dep"}}
		reason := unsatisfiedDependency([]task.Task{dep, waiting}, waiting)
		if released := reason == ""; released != tc.released {
			t.Errorf("outcome %q: released=%v (reason %q), want %v", tc.outcome, released, reason, tc.released)
		}
		if task.StageOf(dep) != task.Done {
			t.Errorf("outcome %q must still close the task", tc.outcome)
		}
	}
}

func TestDecisionOfIsTheSingleValidationPoint(t *testing.T) {
	if _, _, err := decisionOf("", "accepted"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("empty outcome err = %v, want ErrInvalidRequest", err)
	}
	if _, _, err := decisionOf("merged", "x"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("unknown outcome err = %v, want ErrInvalidRequest", err)
	}
	kind, note, err := decisionOf(" Rejected ", "")
	if err != nil || kind != task.OutcomeRejected || note != "rejected" {
		t.Errorf("decisionOf = (%q, %q, %v), want rejected with the outcome as the default note", kind, note, err)
	}
}

func TestDispatchRechecksDependencyAfterCreatingTheIntent(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	prerequisite := f.mustDispatch("prerequisite", "Prerequisite result.")
	prerequisite = f.satisfyReview(prerequisite)
	if task.StageOf(prerequisite) != task.ReadyForReview {
		t.Fatalf("prerequisite stage = %q, want ready_for_review before the concurrent disposition", task.StageOf(prerequisite))
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	trigger := fmt.Sprintf(`CREATE TRIGGER finish_prerequisite_after_intent
		AFTER INSERT ON tasks
		WHEN json_extract(NEW.data, '$.Title') = 'dependent'
		BEGIN
			UPDATE tasks
			SET revision = revision + 1,
			    data = json_set(data, '$.Revision', revision + 1, '$.Disposition', 'accepted')
			WHERE id = '%s';
		END`, prerequisite.ID)
	if _, err := db.ExecContext(context.Background(), trigger); err != nil {
		t.Fatalf("install concurrent-disposition trigger: %v", err)
	}

	dependent, err := f.dispatch("dependent", "Dependent brief.", []string{"go test ./... ok"},
		[]string{prerequisite.ID}, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch after concurrent prerequisite disposition: %v", err)
	}
	if dependent.Phase != task.PromptSubmitted {
		t.Fatalf("dependent phase = %q, want prompt_submitted after the disposition committed during task creation",
			dependent.Phase)
	}
	if got := f.countCalls("worktree create"); got != 2 {
		t.Fatalf("worktree create calls = %d, want prerequisite plus dependent", got)
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
		// A worker's report is not the gate: the disposition is.
		gate = f.satisfyReview(gate)
		if resumed, resumeErr := f.resume(queued.ID); resumed.Phase != task.Intent || resumed.Attention != "" {
			t.Fatalf("resume after the report = (%q, attention %q, err %v), want still queued", resumed.Phase, resumed.Attention, resumeErr)
		}
		gate = f.recordDisposition(gate, "accepted")
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
	if slots, err := f.store.ProvisioningSlots(context.Background(), f.project); err != nil || slots != 0 {
		t.Errorf("provisioning slots after definite worktree refusal = (%d, %v), want the released slot to be free", slots, err)
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

func TestWorktreeFailureSurfacesProvisioningSlotReleaseError(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{WorktreeCreateError: "not_git_worktree"})
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER refuse_slot_release
		BEFORE UPDATE OF slot_claimed_at ON tasks
		WHEN NEW.slot_claimed_at = 0
		BEGIN SELECT RAISE(ABORT, 'slot release is not writable'); END`); err != nil {
		t.Fatalf("install slot-release trigger: %v", err)
	}

	st, dispatchErr := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
	if dispatchErr == nil {
		t.Fatal("dispatch returned nil, want the injected slot-release failure")
	}
	if !errors.Is(dispatchErr, herdr.ErrCLI) {
		t.Errorf("dispatch error %v lost the original Herdr worktree failure", dispatchErr)
	}
	for _, want := range []string{"worktree create", "slot release is not writable"} {
		if !strings.Contains(dispatchErr.Error(), want) {
			t.Errorf("dispatch error %q does not name %q", dispatchErr, want)
		}
	}
	if st.Phase != task.Intent || st.Attention == "" {
		t.Fatalf("failed task = phase %q attention %q, want an actionable Intent task", st.Phase, st.Attention)
	}
	if slots, err := f.store.ProvisioningSlots(context.Background(), f.project); err != nil || slots != 1 {
		t.Errorf("provisioning slots = (%d, %v), want the unreleased slot to remain visible", slots, err)
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
	if !IsAmbiguousBriefTask(st) {
		t.Errorf("ambiguous brief blocker %q is not adjudicable", st.Blocker)
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

func TestUnknownPromptFailureRemainsAdjudicable(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{PromptError: "unclassified_prompt_error"})
	st, err := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
	if err == nil {
		t.Fatal("dispatch returned nil, want the unclassified prompt error")
	}
	if errors.Is(err, herdr.ErrAmbiguous) {
		t.Fatalf("dispatch err %v is already classified as ambiguous; want the generic uncertain-failure path", err)
	}
	if !IsAmbiguousBriefTask(st) {
		t.Fatalf("unclassified prompt failure blocker %q is not adjudicable", st.Blocker)
	}
	if _, _, err := f.coord.BriefInspection(context.Background(), st.ID); err != nil {
		t.Fatalf("BriefInspection after unclassified prompt failure: %v", err)
	}
}

func TestPendingBriefCannotBeAdjudicatedWhileStepClaimIsLive(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.ambiguousBrief("one")
	marker, err := f.coord.newBriefSubmissionMarker(st.Brief)
	if err != nil {
		t.Fatalf("newBriefSubmissionMarker: %v", err)
	}
	st = f.block(st, func(next *task.Task) { next.Blocker = marker })
	before := f.countCalls("agent prompt")

	evidence, _, err := f.coord.BriefInspection(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("BriefInspection with a live step claim: %v", err)
	}
	if _, err := f.coord.AdjudicateBrief(context.Background(), st.ID, st.Revision, BriefNotDelivered, evidence); !errors.Is(err, ErrStepInFlight) {
		t.Fatalf("AdjudicateBrief while the submission claim is live = %v, want ErrStepInFlight", err)
	}
	current, err := f.store.Task(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("read task after refused adjudication: %v", err)
	}
	if current.Phase != task.AgentStarted || current.Blocker != marker {
		t.Fatalf("task after refused adjudication = phase %q blocker %q, want the pending marker unchanged",
			current.Phase, current.Blocker)
	}
	if got := f.countCalls("agent prompt"); got != before {
		t.Fatalf("agent prompt calls after refused adjudication = %d, want unchanged at %d", got, before)
	}
}

func TestPendingBriefClaimOutlivesInFlightPrompt(t *testing.T) {
	const (
		lease        = 50 * time.Millisecond
		promptDelay  = 3 * time.Second
		callTimeout  = 5 * time.Second
		readyTimeout = 2 * time.Second
	)
	f := newCoordFixture(t, fixturePlan{PromptDelayMillis: int(promptDelay / time.Millisecond)})
	projectKey := f.projectKey()
	coordinator, err := New(f.store, f.client, Options{
		Concurrency:      3,
		CallTimeout:      callTimeout,
		ReadinessTimeout: readyTimeout,
		PollInterval:     10 * time.Millisecond,
		StepLease:        lease,
	})
	if err != nil {
		t.Fatalf("New short-lease coordinator: %v", err)
	}
	f.coord = coordinator

	type dispatchResult struct {
		task task.Task
		err  error
	}
	done := make(chan dispatchResult, 1)
	go func() {
		st, err := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
		done <- dispatchResult{task: st, err: err}
	}()
	waitForFixtureFile(t, filepath.Join(f.dir, "prompt_started"), 10*time.Second)

	tasks, err := f.store.Tasks(context.Background(), projectKey)
	if err != nil {
		t.Fatalf("read tasks while prompt is in flight: %v", err)
	}
	var st task.Task
	for _, candidate := range tasks {
		if isBriefSubmissionPending(candidate.Blocker) {
			st = candidate
			break
		}
	}
	if st.ID == "" {
		t.Fatal("no pending brief marker while Herdr's prompt call is in flight")
	}
	time.Sleep(lease + 20*time.Millisecond)
	evidence, _, err := f.coord.BriefInspection(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("BriefInspection during prompt call: %v", err)
	}
	if _, err := f.coord.AdjudicateBrief(context.Background(), st.ID, st.Revision, BriefNotDelivered, evidence); !errors.Is(err, ErrStepInFlight) {
		t.Fatalf("AdjudicateBrief after the configured lease elapsed = %v, want ErrStepInFlight", err)
	}
	result := <-done
	if result.err != nil {
		t.Errorf("dispatch after the prompt completed: %v", result.err)
	}
	if result.task.Phase != task.PromptSubmitted {
		t.Errorf("dispatch phase = %q, want %q", result.task.Phase, task.PromptSubmitted)
	}
	if got := f.countCalls("agent prompt"); got != 1 {
		t.Errorf("agent prompt calls = %d, want one while adjudication was refused", got)
	}
}

func waitForFixtureFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat fixture signal %q: %v", path, err)
		}
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for fixture signal %q", path)
		case <-ticker.C:
		}
	}
}

func TestDefiniteBriefRefusalClearsAttemptMarkerAndCanRetry(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{PromptError: "agent_blocked"})
	st, err := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
	if err == nil {
		t.Fatal("dispatch returned nil, want the definite prompt refusal")
	}
	st, err = f.store.Task(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("read task after refusal: %v", err)
	}
	if st.Attention == "" || st.Blocker != "" {
		t.Fatalf("task after definite refusal = attention %q blocker %q, want retryable attention without pending marker",
			st.Attention, st.Blocker)
	}
	if got := f.countCalls("agent prompt"); got != 1 {
		t.Fatalf("agent prompt calls after refusal = %d, want the one refused call", got)
	}

	f.updatePlan(func(plan *fixturePlan) { plan.PromptError = "" })
	retried, err := f.resume(st.ID)
	if err != nil {
		t.Fatalf("ResumeDispatch after definite refusal: %v", err)
	}
	if retried.Phase != task.PromptSubmitted || retried.Blocker != "" {
		t.Fatalf("retried task = phase %q blocker %q, want submitted without blocker",
			retried.Phase, retried.Blocker)
	}
	if got := f.countCalls("agent prompt"); got != 2 {
		t.Fatalf("agent prompt calls after retry = %d, want one refused attempt and one successful retry", got)
	}
}

func TestPromptRecordFailureLeavesBriefAdjudicableWithoutResending(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	const lease = 30 * time.Millisecond
	coordinator, err := New(f.store, f.client, Options{
		Concurrency:      3,
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
		StepLease:        lease,
		Preflight:        satisfiedPreflight,
	})
	if err != nil {
		t.Fatalf("New short-lease coordinator: %v", err)
	}
	f.coord = coordinator

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER refuse_prompt_phase
		BEFORE UPDATE OF data ON tasks
		WHEN json_extract(NEW.data, '$.Phase') = 'prompt_submitted'
		BEGIN SELECT RAISE(ABORT, 'prompt phase is not writable'); END`); err != nil {
		t.Fatalf("install prompt-phase trigger: %v", err)
	}

	st, dispatchErr := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
	if dispatchErr == nil {
		t.Fatal("dispatch returned nil, want the injected phase-write failure")
	}
	if !strings.Contains(dispatchErr.Error(), "orch_adjudicate_dispatch") || !strings.Contains(dispatchErr.Error(), st.ID) {
		t.Fatalf("dispatch error %q must name the task and orch_adjudicate_dispatch", dispatchErr)
	}
	st, err = f.store.Task(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("read task after phase-write failure: %v", err)
	}
	if st.Phase != task.AgentStarted || !IsAmbiguousBriefTask(st) {
		t.Fatalf("task after accepted prompt and failed phase write = phase %q, blocker %q; want an adjudicable pending brief",
			st.Phase, st.Blocker)
	}
	if got := f.countCalls("agent prompt"); got != 1 {
		t.Fatalf("agent prompt calls = %d, want exactly one", got)
	}

	time.Sleep(lease + 20*time.Millisecond)
	retry, err := New(f.store, f.client, Options{
		Concurrency:      3,
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: 2 * time.Second,
		PollInterval:     10 * time.Millisecond,
		StepLease:        lease,
	})
	if err != nil {
		t.Fatalf("New retry coordinator: %v", err)
	}
	if _, err := retry.ResumeDispatch(context.Background(), st.ID); !errors.Is(err, ErrBlocked) {
		t.Fatalf("ResumeDispatch after the claim expired = %v, want ErrBlocked", err)
	}
	if got := f.countCalls("agent prompt"); got != 1 {
		t.Fatalf("agent prompt calls after resume = %d, want no resubmission", got)
	}

	if _, err := db.ExecContext(context.Background(), `DROP TRIGGER refuse_prompt_phase`); err != nil {
		t.Fatalf("remove prompt-phase trigger: %v", err)
	}
	evidence, _, err := f.coord.BriefInspection(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("BriefInspection(pending successful prompt): %v", err)
	}
	decided, err := f.coord.AdjudicateBrief(context.Background(), st.ID, st.Revision, BriefDelivered, evidence)
	if err != nil {
		t.Fatalf("AdjudicateBrief(delivered): %v", err)
	}
	if decided.Phase != task.PromptSubmitted || decided.Blocker != "" {
		t.Fatalf("adjudicated task = phase %q blocker %q, want prompt_submitted without blocker",
			decided.Phase, decided.Blocker)
	}
	if got := f.countCalls("agent prompt"); got != 1 {
		t.Fatalf("agent prompt calls after adjudication = %d, want no resubmission", got)
	}
}

func TestBriefAttemptWriteFailurePreventsPrompt(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(f.stateDir, "orchestrator.sqlite")))
	if err != nil {
		t.Fatalf("open state database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(context.Background(), `CREATE TRIGGER refuse_brief_attempt
		BEFORE UPDATE OF data ON tasks
		WHEN json_extract(NEW.data, '$.Blocker') LIKE 'brief submission pending:%'
		BEGIN SELECT RAISE(ABORT, 'brief attempt is not writable'); END`); err != nil {
		t.Fatalf("install brief-attempt trigger: %v", err)
	}

	st, dispatchErr := f.dispatch("one", "brief one", []string{"check"}, nil, nil, f.base)
	if dispatchErr == nil {
		t.Fatal("dispatch returned nil, want the injected attempt-write failure")
	}
	if got := f.countCalls("agent prompt"); got != 0 {
		t.Fatalf("agent prompt calls = %d, want none until the attempt is durable", got)
	}
	st, err = f.store.Task(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("read task after attempt-write failure: %v", err)
	}
	if st.Phase != task.AgentStarted || st.Blocker != "" {
		t.Fatalf("task after attempt-write failure = phase %q blocker %q, want unchanged AgentStarted state",
			st.Phase, st.Blocker)
	}

	retry := f.sibling()
	if _, err := retry.ResumeDispatch(context.Background(), st.ID); err == nil {
		t.Fatal("retry unexpectedly succeeded while the attempt-write trigger remains")
	} else if errors.Is(err, ErrStepInFlight) {
		t.Fatalf("retry was blocked by a stale step claim after no prompt was sent: %v", err)
	}
	if got := f.countCalls("agent prompt"); got != 0 {
		t.Fatalf("agent prompt calls after safe retry failure = %d, want none", got)
	}
}

func TestResumeDoesNotPromptAReplacedWorkerSession(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.mustDispatch("one", "brief one")

	started, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
		next.Phase = task.AgentStarted
		return nil
	})
	if err != nil {
		t.Fatalf("move task back to the pre-submit phase: %v", err)
	}
	f.coord.releaseStep(context.Background(), started)
	f.rewriteState(func(current *fixtureState) {
		current.Agents[worker.PublicPaneID].Session = "someone-elses-session"
	})
	beforeCalls := f.countCalls("agent prompt")
	beforePrompts := len(f.prompts(worker.PublicPaneID))

	resumed, err := f.resume(started.ID)
	if !errors.Is(err, ErrAgentChanged) {
		t.Fatalf("ResumeDispatch err = %v, want ErrAgentChanged", err)
	}
	if resumed.Phase != task.AgentStarted || resumed.Blocker == "" || task.StageOf(resumed) != task.NeedsInput {
		t.Fatalf("replaced session left task at phase %q, blocker %q, stage %q; want a blocked pre-submit task",
			resumed.Phase, resumed.Blocker, task.StageOf(resumed))
	}
	if got := f.countCalls("agent prompt"); got != beforeCalls {
		t.Fatalf("agent prompt calls = %d, want unchanged at %d", got, beforeCalls)
	}
	if got := len(f.prompts(worker.PublicPaneID)); got != beforePrompts {
		t.Fatalf("prompts on the replaced worker pane = %d, want unchanged at %d", got, beforePrompts)
	}
}

func TestResumeAgentStartedAfterMoveUsesLaunchPaneHandshake(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	worker := f.mustDispatch("one", "brief one")
	ctx := context.Background()
	started, err := f.store.UpdateTask(ctx, worker.ID, worker.Revision, func(next *task.Task) error {
		next.Phase = task.AgentStarted
		return nil
	})
	if err != nil {
		t.Fatalf("move task back to the pre-submit phase: %v", err)
	}
	f.coord.releaseStep(ctx, started)
	f.rewriteState(func(current *fixtureState) {
		current.Agents[worker.PublicPaneID].Prompted = 0
		current.Prompts[worker.PublicPaneID] = nil
	})

	move := f.moveResult(t, worker.PublicPaneID)
	newPane, _ := move["pane"].(map[string]any)["pane_id"].(string)
	if newPane == "" {
		t.Fatalf("move result names no public pane: %v", move)
	}
	f.mustHandle(t, EventPaneMoved, eventFromMoveResult(t, move))
	moved := f.taskByID(t, worker.ID)
	if moved.PublicPaneID != newPane || moved.LaunchPaneID != worker.LaunchPaneID {
		t.Fatalf("moved task panes = launch %q/public %q, want launch %q/public %q",
			moved.LaunchPaneID, moved.PublicPaneID, worker.LaunchPaneID, newPane)
	}

	// Readiness remains keyed by the launch pane inherited by the worker. Do not
	// let the fixture create a second, misleading row for the moved public pane.
	f.updatePlan(func(plan *fixturePlan) { plan.SuppressReady = true })
	beforeCalls := f.countCalls("agent prompt")
	resumed, err := f.resume(moved.ID)
	if err != nil {
		t.Fatalf("resume moved AgentStarted worker: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Fatalf("resumed phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
	if got := f.countCalls("agent prompt"); got != beforeCalls+1 {
		t.Fatalf("agent prompt calls = %d, want one new brief after readiness confirmation", got)
	}
	if prompts := f.prompts(newPane); len(prompts) != 1 {
		t.Fatalf("prompts to moved pane = %v, want exactly one brief", prompts)
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
	st, err := f.dispatch("quoted", brief, []string{"go test ./... ok"}, nil, nil, f.base)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if st.Phase != task.PromptSubmitted || st.Blocker != "" {
		t.Fatalf("successful dispatch = phase %q blocker %q, want submitted without pending marker",
			st.Phase, st.Blocker)
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

// ---------------------------------------------------------------------------
// the durable dispatch gate
// ---------------------------------------------------------------------------

// dispatchSpec runs one dispatch request through one coordinator.
func (f *coordFixture) dispatchSpec(c *Coordinator, s spec) (task.Task, error) {
	f.t.Helper()
	return c.Dispatch(context.Background(), f.projectKey(), s.title, "brief "+s.title, s.checks, s.deps, s.reserved, f.base)
}

func (f *coordFixture) stages() map[task.Stage]int {
	f.t.Helper()
	tasks, err := f.store.Tasks(context.Background(), f.root)
	if err != nil {
		f.t.Fatalf("Tasks: %v", err)
	}
	counts := map[task.Stage]int{}
	for _, st := range tasks {
		counts[task.StageOf(st)]++
	}
	return counts
}

func TestConcurrentDispatchesCannotExceedTheBudget(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	sibling := f.sibling()
	checks := []string{"go test ./... ok"}

	// Two workers are running, so one slot is left for two requests that are
	// decided at the same time.
	for _, s := range []spec{
		{title: "one", checks: checks, reserved: []string{"a.txt"}},
		{title: "two", checks: checks, reserved: []string{"b.txt"}},
	} {
		if _, err := f.dispatchSpec(f.coord, s); err != nil {
			t.Fatalf("dispatch %s: %v", s.title, err)
		}
	}
	before := f.countCalls("worktree create")

	type outcome struct {
		st  task.Task
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	go func() {
		<-start
		st, err := f.dispatchSpec(f.coord, spec{title: "three", checks: checks, reserved: []string{"c.txt"}})
		results <- outcome{st, err}
	}()
	go func() {
		<-start
		st, err := f.dispatchSpec(sibling, spec{title: "four", checks: checks, reserved: []string{"d.txt"}})
		results <- outcome{st, err}
	}()
	close(start)
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent dispatch failed: %v", got.err)
		}
	}

	// The budget is a property of the project, not of one connection: exactly one
	// of the two requests could take the last slot.
	if created := f.countCalls("worktree create"); created != before+1 {
		t.Errorf("worktree create calls = %d, want %d: two processes provisioned past the budget", created, before+1)
	}
	stages := f.stages()
	if stages[task.Running] != 3 {
		t.Errorf("running tasks = %d, want 3 (%v)", stages[task.Running], stages)
	}
	if stages[task.Queued] != 1 {
		t.Errorf("queued tasks = %d, want 1 (%v)", stages[task.Queued], stages)
	}
}

func TestConcurrentDispatchesCannotShareReservedPaths(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	sibling := f.sibling()
	checks := []string{"go test ./... ok"}
	owner, err := f.dispatchSpec(f.coord, spec{title: "owner", checks: checks, reserved: []string{"notes.md"}})
	if err != nil {
		t.Fatalf("dispatch owner: %v", err)
	}

	// A second process, choosing the same reservation at the same time as a third
	// request, must not both take it: the gate decides on the database's contents.
	type outcome struct {
		title string
		err   error
		st    task.Task
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for i, c := range []*Coordinator{f.coord, sibling} {
		go func(i int, c *Coordinator) {
			<-start
			st, err := f.dispatchSpec(c, spec{title: fmt.Sprintf("thief-%d", i), checks: checks, reserved: []string{"notes.md"}})
			results <- outcome{fmt.Sprintf("thief-%d", i), err, st}
		}(i, c)
	}
	close(start)
	refused := 0
	for range 2 {
		got := <-results
		if got.err != nil {
			if !errors.Is(got.err, ErrReservedConflict) {
				t.Fatalf("dispatch %s err = %v, want ErrReservedConflict", got.title, got.err)
			}
			if !strings.Contains(got.err.Error(), owner.ID) {
				t.Errorf("refusal %q does not name the owner %q", got.err, owner.ID)
			}
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("refusals = %d, want 2: the reservation is held by %s", refused, owner.ID)
	}
	if created := f.countCalls("worktree create"); created != 1 {
		t.Errorf("worktree create calls = %d, want 1", created)
	}
	// Only the owner holds the path, and no thief task was recorded.
	tasks, err := f.store.Tasks(context.Background(), f.root)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	for _, st := range tasks {
		if st.ID == owner.ID {
			continue
		}
		for _, path := range st.ReservedPaths {
			if path == "notes.md" {
				t.Errorf("task %s also reserved notes.md", st.ID)
			}
		}
	}
}

func TestQueuedTasksDrainWhenASlotFrees(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	var workers []task.Task
	for i, title := range []string{"one", "two", "three"} {
		st, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}})
		if err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
		workers = append(workers, st)
	}
	queued, err := f.dispatchSpec(f.coord, spec{title: "four", checks: checks})
	if err != nil {
		t.Fatalf("dispatch four: %v", err)
	}
	if queued.Phase != task.Intent {
		t.Fatalf("fourth task phase = %q, want %q", queued.Phase, task.Intent)
	}

	// The primary reviews a worker (the write Task 5's review makes), which frees
	// its slot, and dispatches one more request: the queue has been waiting longer,
	// so it goes first and the new request is queued instead.
	f.block(workers[0], func(next *task.Task) {
		next.Disposition = "integrated"
	})
	fresh, err := f.dispatchSpec(f.coord, spec{title: "five", checks: checks})
	if err != nil {
		t.Fatalf("dispatch five: %v", err)
	}
	drained, err := f.store.Task(context.Background(), queued.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if drained.Phase != task.PromptSubmitted {
		t.Fatalf("the queued task is still %q, want %q (attention=%q)", drained.Phase, task.PromptSubmitted, drained.Attention)
	}
	if got := f.prompts(drained.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts for the drained task = %v, want one brief", got)
	}
	if fresh.Phase != task.Intent {
		t.Errorf("the newest request phase = %q, want %q: it overtook the queue", fresh.Phase, task.Intent)
	}
	if created := f.countCalls("worktree create"); created != 4 {
		t.Errorf("worktree create calls = %d, want 4 (three workers plus the drained task)", created)
	}
}

func TestDependentTasksDrainWhenThePrerequisiteIsClosed(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	prereq, err := f.dispatchSpec(f.coord, spec{title: "prereq", checks: checks})
	if err != nil {
		t.Fatalf("dispatch prereq: %v", err)
	}
	dependent, err := f.dispatchSpec(f.coord, spec{title: "dependent", checks: checks, deps: []string{prereq.ID}})
	if err != nil {
		t.Fatalf("dispatch dependent: %v", err)
	}
	if dependent.Phase != task.Intent {
		t.Fatalf("dependent phase = %q, want %q", dependent.Phase, task.Intent)
	}

	// The prerequisite's final report is not a decision: it leaves the dependency
	// ready for review, and the dependent task stays queued even though an ordinary
	// dispatch carries the drain.
	f.satisfyReview(prereq)
	if _, err := f.dispatchSpec(f.coord, spec{title: "unrelated", checks: checks}); err != nil {
		t.Fatalf("dispatch after the report: %v", err)
	}
	still, err := f.store.Task(context.Background(), dependent.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if still.Phase != task.Intent || still.Attention != "" {
		t.Fatalf("a final report opened the gate: the dependent task is %q (attention=%q)", still.Phase, still.Attention)
	}

	// The primary's disposition closes the prerequisite, and a perfectly ordinary
	// dispatch is what carries the drain.
	closed := f.recordDisposition(f.taskByID(t, prereq.ID), "integrated")
	if got := task.StageOf(closed); got != task.Done {
		t.Fatalf("prerequisite stage = %q, want done", got)
	}
	if _, err := f.dispatchSpec(f.coord, spec{title: "another", checks: checks}); err != nil {
		t.Fatalf("dispatch after the disposition: %v", err)
	}
	provisioned, err := f.store.Task(context.Background(), dependent.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if provisioned.Phase != task.PromptSubmitted {
		t.Fatalf("the dependent task is still %q, want %q (attention=%q)",
			provisioned.Phase, task.PromptSubmitted, provisioned.Attention)
	}
	if got := f.prompts(provisioned.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts for the dependent task = %v, want one brief", got)
	}
}

// TestAFinalReportAloneDoesNotUnlockADependent pins the user's decision: a
// dependency is gated on the primary's disposition, never on the worker's final
// report. A report leaves the dependency ready for review, and ready for review
// stays queued.
func TestAFinalReportAloneDoesNotUnlockADependent(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()
	checks := []string{"go test ./... ok"}

	prereq, err := f.dispatchSpec(f.coord, spec{title: "prereq", checks: checks})
	if err != nil {
		t.Fatalf("dispatch prereq: %v", err)
	}
	dependent, err := f.dispatchSpec(f.coord, spec{title: "dependent", checks: checks, deps: []string{prereq.ID}})
	if err != nil {
		t.Fatalf("dispatch dependent: %v", err)
	}
	prereq = f.satisfyReview(prereq)
	if got := task.StageOf(prereq); got != task.ReadyForReview {
		t.Fatalf("prerequisite stage = %q, want %q", got, task.ReadyForReview)
	}
	created := f.countCalls("worktree create") // only the prerequisite has a checkout

	// Every surface that can admit queued work is asked, and none of them unlocks
	// the dependent: a drain and a resume.
	if _, err := f.coord.DrainQueued(ctx, f.projectKey()); err != nil {
		t.Fatalf("DrainQueued after the report: %v", err)
	}
	if resumed, resumeErr := f.resume(dependent.ID); resumed.Phase != task.Intent || resumed.Attention != "" {
		t.Fatalf("resume after the report = (%q, attention %q, err %v), want still queued", resumed.Phase, resumed.Attention, resumeErr)
	}
	if got := f.countCalls("worktree create"); got != created {
		t.Errorf("a report alone provisioned a worker: worktree create calls = %d, want %d", got, created)
	}

	// An ordinary dispatch is the third surface: it carries the drain, and it is
	// the one that must not let the gated task overtake it.
	if _, err := f.dispatchSpec(f.coord, spec{title: "unrelated", checks: checks}); err != nil {
		t.Fatalf("dispatch after the report: %v", err)
	}
	queued, err := f.store.Task(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("read the dependent task: %v", err)
	}
	if queued.Phase != task.Intent || task.StageOf(queued) != task.Queued {
		t.Fatalf("the dependent task left the queue on a report alone: %q/%q", queued.Phase, task.StageOf(queued))
	}
	if queued.Attention != "" || queued.Blocker != "" {
		t.Errorf("the gated task recorded a reason it never ran: attention=%q blocker=%q", queued.Attention, queued.Blocker)
	}
	if got := f.prompts(queued.PublicPaneID); len(got) != 0 {
		t.Errorf("a brief was submitted for a task that is still gated: %v", got)
	}
	if got := f.countCalls("worktree create"); got != created+1 {
		t.Errorf("worktree create calls = %d, want %d: the gated task was provisioned instead of the new request",
			got, created+1)
	}

	// The disposition is the gate-opening event: Review records it and then drains,
	// so the task that was waiting starts without another dispatch or a restart.
	project, err := f.store.Project(ctx, f.projectKey())
	if err != nil {
		t.Fatalf("read the bound project: %v", err)
	}
	result, err := f.coord.Review(ctx, project.LaunchPaneID, prereq.ID, "integrated", "accepted: the user kept the branch", "")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Stage != task.Done {
		t.Fatalf("review stage = %q, want %q", result.Stage, task.Done)
	}
	provisioned, err := f.store.Task(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("read the dependent task: %v", err)
	}
	if provisioned.Phase != task.PromptSubmitted {
		t.Fatalf("the disposition did not admit the dependent task: %q (attention=%q)", provisioned.Phase, provisioned.Attention)
	}
	if got := f.prompts(provisioned.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts for the dependent task = %v, want one brief", got)
	}
}

func TestDrainStopsAtTheBudgetAndKeepsQueuedWorkQueued(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	for i, title := range []string{"one", "two", "three"} {
		if _, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}}); err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
	}
	queued, err := f.dispatchSpec(f.coord, spec{title: "four", checks: checks})
	if err != nil {
		t.Fatalf("dispatch four: %v", err)
	}
	// Nothing has freed a slot, so a drain changes nothing at all.
	if _, err := f.dispatchSpec(f.coord, spec{title: "five", checks: checks}); err != nil {
		t.Fatalf("dispatch five: %v", err)
	}
	still, err := f.store.Task(context.Background(), queued.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if still.Phase != task.Intent || still.Attention != "" || still.Blocker != "" {
		t.Errorf("the queued task changed without a free slot: phase=%q attention=%q", still.Phase, still.Attention)
	}
	if created := f.countCalls("worktree create"); created != 3 {
		t.Errorf("worktree create calls = %d, want 3", created)
	}
}

func TestDrainQueuedSurfaceProgressesAQueuedTaskAfterReview(t *testing.T) {
	// Task 5's Review records the disposition and then calls this surface: the
	// queued task must progress without waiting for another dispatch.
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	var workers []task.Task
	for i, title := range []string{"one", "two", "three"} {
		st, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}})
		if err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
		workers = append(workers, st)
	}
	queued, err := f.dispatchSpec(f.coord, spec{title: "four", checks: checks})
	if err != nil {
		t.Fatalf("dispatch four: %v", err)
	}

	// Before the slot is freed the drain changes nothing.
	if drained, err := f.coord.DrainQueued(context.Background(), f.root); err != nil || len(drained) != 0 {
		t.Fatalf("DrainQueued with a full budget = (%v, %v), want no progress", drained, err)
	}
	still, err := f.store.Task(context.Background(), queued.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if still.Phase != task.Intent {
		t.Fatalf("phase = %q, want %q", still.Phase, task.Intent)
	}

	// The review frees the slot, and the drain is what carries it.
	f.block(workers[1], func(next *task.Task) {
		next.Disposition = "integrated"
	})
	drained, err := f.coord.DrainQueued(context.Background(), f.root)
	if err != nil {
		t.Fatalf("DrainQueued: %v", err)
	}
	if len(drained) != 1 || drained[0].ID != queued.ID {
		t.Fatalf("DrainQueued = %+v, want the queued task %s", drained, queued.ID)
	}
	if drained[0].Phase != task.PromptSubmitted {
		t.Errorf("drained phase = %q, want %q", drained[0].Phase, task.PromptSubmitted)
	}
	if got := f.prompts(drained[0].PublicPaneID); len(got) != 1 {
		t.Errorf("prompts = %v, want one brief", got)
	}
	// The drain is idempotent: again changes nothing, and no step is repeated.
	again, err := f.coord.DrainQueued(context.Background(), f.root)
	if err != nil || len(again) != 0 {
		t.Errorf("second DrainQueued = (%v, %v), want no progress", again, err)
	}
	if created := f.countCalls("worktree create"); created != 4 {
		t.Errorf("worktree create calls = %d, want 4", created)
	}
	// A project key is required: an empty one is a caller error, not a silent
	// no-op that would hide a missing project.
	if _, err := f.coord.DrainQueued(context.Background(), "  "); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("DrainQueued(\"\") err = %v, want ErrInvalidRequest", err)
	}
}

func TestGatedIntentTasksHoldNoSlot(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	slots := func() int {
		f.t.Helper()
		count, err := f.store.ProvisioningSlots(context.Background(), f.root)
		if err != nil {
			f.t.Fatalf("ProvisioningSlots: %v", err)
		}
		return count
	}

	// A dependency-gated request is recorded without a slot: the admitted task
	// would otherwise hold capacity it cannot use.
	prereq, err := f.dispatchSpec(f.coord, spec{title: "prereq", checks: checks})
	if err != nil {
		t.Fatalf("dispatch prereq: %v", err)
	}
	dependent, err := f.dispatchSpec(f.coord, spec{title: "dependent", checks: checks, deps: []string{prereq.ID}})
	if err != nil {
		t.Fatalf("dispatch dependent: %v", err)
	}
	if dependent.Phase != task.Intent {
		t.Fatalf("dependent phase = %q, want %q", dependent.Phase, task.Intent)
	}
	if got := slots(); got != 1 {
		t.Fatalf("slots held = %d, want only the running prerequisite", got)
	}

	// Fill the budget: the next request is recorded queued and holds nothing.
	for i, title := range []string{"two", "three"} {
		if _, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}}); err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
	}
	if got := slots(); got != 3 {
		t.Fatalf("slots held = %d, want the full budget of 3", got)
	}
	refused, err := f.dispatchSpec(f.coord, spec{title: "four", checks: checks})
	if err != nil {
		t.Fatalf("dispatch four: %v", err)
	}
	if refused.Phase != task.Intent {
		t.Fatalf("fourth phase = %q, want %q", refused.Phase, task.Intent)
	}
	if got := slots(); got != 3 {
		t.Errorf("a budget-refused task holds a slot: %d held, want 3", got)
	}
	// A definite failure before any resource goes back too.
	failed, err := f.coord.Dispatch(context.Background(), f.root, "five", "brief five",
		[]string{"go test ./... ok"}, nil, []string{"other.txt"}, strings.Repeat("b", 40))
	if !errors.Is(err, ErrBaseRevision) {
		t.Fatalf("dispatch with an unknown base err = %v, want ErrBaseRevision", err)
	}
	_ = failed
	if got := slots(); got != 3 {
		t.Errorf("a refused request changed the slots: %d held, want 3", got)
	}
}

func TestResumeReusesTheClaimACrashLeft(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}
	st, err := f.dispatchSpec(f.coord, spec{title: "one", checks: checks})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Rewind the task to just after it was admitted: the claim is durable, the
	// process died before any Herdr call.
	admitted := f.block(st, func(next *task.Task) {
		next.Phase = task.Intent
		next.WorktreePath, next.WorktreeWorkspaceID, next.CurrentWorkspaceID = "", "", ""
		next.LaunchPaneID, next.PublicPaneID = "", ""
		next.AgentName, next.OMPSessionID, next.TerminalID = "", "", ""
	})
	// The rewind leaves the claim the dead dispatch took: that is exactly the state
	// a crash between admitting a task and recording a resource leaves behind, and
	// the retry must reconcile it rather than take a second slot.
	slots, err := f.store.ProvisioningSlots(context.Background(), f.root)
	if err != nil || slots != 1 {
		t.Fatalf("ProvisioningSlots = (%d, %v), want the one claim", slots, err)
	}

	// The retry reconciles that same claim: it does not take a second slot, it
	// adopts the checkout the dead attempt left, and it starts no second agent.
	beforeCreate := f.countCalls("worktree create")
	beforeStart := f.countCalls("agent start")
	resumed, err := f.resume(admitted.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Fatalf("resumed phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
	if slots, err = f.store.ProvisioningSlots(context.Background(), f.root); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots after the retry = (%d, %v), want 1: the claim was double counted", slots, err)
	}
	if created := f.countCalls("worktree create"); created != beforeCreate {
		t.Errorf("worktree create calls = %d, want %d: the retry created a second checkout", created, beforeCreate)
	}
	if started := f.countCalls("agent start"); started != beforeStart {
		t.Errorf("agent start calls = %d, want %d: the retry started a second agent", started, beforeStart)
	}
	list := gitOutput(t, f.root, "worktree", "list", "--porcelain")
	if strings.Count(list, "orch/"+strings.TrimPrefix(admitted.Branch, "orch/")) != 1 {
		t.Errorf("the repository has more than one checkout for %s:\n%s", admitted.Branch, list)
	}
}

func TestDrainRetriesARetryableAttentionAndNeverABlocker(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{WorktreeCreateError: "not_git_worktree"})
	checks := []string{"go test ./... ok"}
	failed, err := f.dispatchSpec(f.coord, spec{title: "one", checks: checks})
	if !errors.Is(err, herdr.ErrCLI) {
		t.Fatalf("dispatch err = %v, want a Herdr CLI failure", err)
	}
	if failed.Attention == "" || failed.Blocker != "" || failed.Phase != task.Intent {
		t.Fatalf("failed task = %+v, want a retryable attention at intent", failed)
	}
	// A failed step that recorded nothing gives its slot back, so the queue can
	// still make progress while this task waits for its cause to be fixed.
	if slots, err := f.store.ProvisioningSlots(context.Background(), f.root); err != nil || slots != 0 {
		t.Fatalf("ProvisioningSlots = (%d, %v), want the slot released", slots, err)
	}

	// The cause is fixed and nothing resumes the task explicitly: the drain does,
	// and the retryable reason must not outlive the retry that mended it.
	f.updatePlan(func(p *fixturePlan) { p.WorktreeCreateError = "" })
	drained, err := f.coord.DrainQueued(context.Background(), f.root)
	if err != nil {
		t.Fatalf("DrainQueued: %v", err)
	}
	if len(drained) != 1 || drained[0].ID != failed.ID {
		t.Fatalf("DrainQueued = %+v, want the failed task %s", drained, failed.ID)
	}
	if drained[0].Attention != "" {
		t.Errorf("attention survived the successful retry: %q", drained[0].Attention)
	}
	if got := task.StageOf(drained[0]); got != task.Running {
		t.Errorf("stage = %q, want running", got)
	}

	// A blocker is never retried by a drain: it is a decision only a human makes.
	blocked := f.mustDispatch("two", "brief two")
	blocked = f.block(blocked, func(next *task.Task) {
		next.Phase = task.Intent
		next.Blocker = "ambiguous, a human decides"
	})
	if drained, err := f.coord.DrainQueued(context.Background(), f.root); err != nil || len(drained) != 0 {
		t.Fatalf("DrainQueued with a blocked task = (%v, %v), want no progress", drained, err)
	}
	after, err := f.store.Task(context.Background(), blocked.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Blocker != "ambiguous, a human decides" || after.Phase != task.Intent {
		t.Errorf("a blocker was touched by the drain: %+v", after)
	}
}

func TestDrainNeverProvisionsATaskAnotherCallerOwns(t *testing.T) {
	// The reviewed interleaving: process A admitted a task and is inside its
	// provisioning (claim held, phase still intent, no resource recorded yet), and
	// process B's drain sees it as an eligible queued candidate. B must not
	// provision it — that would race the same branch, pane and brief, and B's
	// failure would then release a claim A is still using.
	f := newCoordFixture(t, fixturePlan{})
	sibling := f.sibling()
	checks := []string{"go test ./... ok"}
	request, err := newDispatchRequest("one", "brief one", checks, nil, nil)
	if err != nil {
		t.Fatalf("newDispatchRequest: %v", err)
	}
	base, err := f.coord.committedSHA(context.Background(), f.root, f.base)
	if err != nil {
		t.Fatalf("committedSHA: %v", err)
	}

	// Dispatch records the zero-slot intent before the queue drain, then claims
	// capacity after older work has had its turn. There is no queue here, so make
	// that claim directly and stop before any provisioning side effect.
	admitted, duplicate, err := f.coord.recordTaskIntent(context.Background(), f.root, request, base)
	if err != nil || duplicate {
		t.Fatalf("record the intent = (%+v, %v, %v), want a new task", admitted, duplicate, err)
	}
	claimed, admittedToken, err := f.store.ClaimProvisioningSlot(context.Background(), admitted.ID, f.coord.opts.Concurrency)
	if err != nil || !claimed {
		t.Fatalf("claim the admitted task's slot = (%v, %v), want a claim", claimed, err)
	}

	// B drains the project: the claimed task is the queue's only candidate.
	drained, err := sibling.DrainQueued(context.Background(), f.root)
	if err != nil {
		t.Fatalf("sibling DrainQueued: %v", err)
	}
	if len(drained) != 0 {
		t.Fatalf("the sibling drain provisioned %+v, want nothing: the task belongs to another caller", drained)
	}
	if created := f.countCalls("worktree create"); created != 0 {
		t.Errorf("worktree create calls = %d, want 0: the drain raced another caller's provisioning", created)
	}
	after, err := f.store.Task(context.Background(), admitted.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Phase != task.Intent || after.Attention != "" || after.Blocker != "" {
		t.Errorf("the drain changed a task another caller owns: %+v", after)
	}
	if slots, err := f.store.ProvisioningSlots(context.Background(), f.root); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots = (%d, %v), want the claim A holds", slots, err)
	}

	// A finishes its own provisioning, and the claim it held is what counted. It
	// presents that claim's token, which is the one it acquired at admission.
	finished, err := f.coord.provision(context.Background(), admitted, slotClaim{token: admittedToken})
	if err != nil {
		t.Fatalf("the owner's provisioning failed: %v", err)
	}
	if finished.Phase != task.PromptSubmitted {
		t.Errorf("phase = %q, want %q", finished.Phase, task.PromptSubmitted)
	}
	if slots, err := f.store.ProvisioningSlots(context.Background(), f.root); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots after the owner finished = (%d, %v), want 1", slots, err)
	}
	if created := f.countCalls("worktree create"); created != 1 {
		t.Errorf("worktree create calls = %d, want exactly 1", created)
	}
}

// TestDefiniteWorktreeFailureImmediatelyDrainsEligibleQueuedWork pins what a
// released slot is for: a definite worktree-create failure provably created
// nothing, so the capacity it held goes back — and the task that was waiting for
// that capacity starts at that moment, without another dispatch, a resume or a
// restart.
func TestDefiniteWorktreeFailureImmediatelyDrainsEligibleQueuedWork(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}

	// The budget is full and one task is waiting for a slot.
	workers := make([]task.Task, 0, 3)
	for i, title := range []string{"one", "two", "three"} {
		st, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}})
		if err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
		workers = append(workers, st)
	}
	queued, err := f.dispatchSpec(f.coord, spec{title: "queued", checks: checks, reserved: []string{"src/queued.txt"}})
	if err != nil {
		t.Fatalf("dispatch queued: %v", err)
	}
	if queued.Phase != task.Intent {
		t.Fatalf("queued phase = %q, want a queued intent", queued.Phase)
	}

	// The waiting task's own checkout is prepared, so its provisioning succeeds
	// while the failing attempt's `worktree create` is the one that is refused:
	// what this test observes is the drain, not a second failure.
	path := filepath.Join(f.dir, "worktrees", "queued")
	if out, err := runGit(f.root, "worktree", "add", "-b", queued.Branch, path, f.base); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	state := f.state()
	state.ensure()
	state.NextWorkspace++
	pane := fmt.Sprintf("w%d:p1", state.NextWorkspace)
	state.Workspaces[fmt.Sprintf("w%d", state.NextWorkspace)] = path
	state.Worktrees = append(state.Worktrees, fixtureWorktree{
		Path: path, Branch: queued.Branch, Base: f.base, WorkspaceID: fmt.Sprintf("w%d", state.NextWorkspace), PaneID: pane,
	})
	if err := writeJSONFile(filepath.Join(f.dir, fixtureStateName), state); err != nil {
		t.Fatalf("write fixture state: %v", err)
	}
	queued = f.block(queued, func(next *task.Task) {
		next.LaunchPaneID = pane
		next.PublicPaneID = pane
	})

	// One worker is reworked onto a branch that has no checkout, and its create is
	// refused: a definite failure, so its slot belongs to the queue.
	f.updatePlan(func(p *fixturePlan) { p.WorktreeCreateError = "not_git_worktree" })
	failed := f.block(workers[2], func(next *task.Task) {
		next.Branch = "orch/refused-without-checkout"
		next.Phase = task.Intent
		next.WorktreePath, next.WorktreeWorkspaceID, next.CurrentWorkspaceID = "", "", ""
		next.LaunchPaneID, next.PublicPaneID = "", ""
		next.AgentName, next.OMPSessionID, next.TerminalID = "", "", ""
	})
	if _, err := f.resume(failed.ID); err == nil {
		t.Fatal("the refused worktree create was reported as success")
	}
	if got := f.taskByID(t, failed.ID); got.Phase != task.Intent || got.Attention == "" {
		t.Fatalf("the failed attempt = phase %q attention %q, want an actionable intent task", got.Phase, got.Attention)
	}

	drained := f.taskByID(t, queued.ID)
	if drained.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q: the released slot did not drain the queue (attention=%q)",
			drained.Phase, task.PromptSubmitted, drained.Attention)
	}
	if got := f.prompts(drained.PublicPaneID); len(got) != 1 {
		t.Errorf("prompts for the drained task = %v, want one brief", got)
	}
	if got := f.countCalls("worktree create"); got < 4 {
		t.Errorf("worktree create calls = %d, want at least 4 (three workers plus the refused attempt): the drain adopted an existing checkout", got)
	}
}

// TestWorktreeCreateFailureNeverClearsARetrysActiveSlot pins the atomicity of the
// slot release a definite provisioning failure performs.
//
// The release only ever gives back the capacity of the attempt that failed. The
// window this test pins is the one between that attempt recording its definite
// failure and giving its slot back: another coordinator process may already have
// resumed the same task, and from that moment the capacity claim is the retry's.
// The retry here is a real ResumeDispatch — in a sibling coordinator, blocked
// inside its own `worktree create` — so an old release that clears the claim lets
// the project admit work past its budget while the retry is still provisioning.
func TestWorktreeCreateFailureNeverClearsARetrysActiveSlot(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()
	checks := []string{"go test ./... ok"}

	// The budget is full: two running workers and a task rewound to the state an
	// interrupted attempt leaves — admitted, its claim still held, and the definite
	// failure already recorded, with its slot release still outstanding.
	for i, title := range []string{"one", "two"} {
		if _, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}}); err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
	}
	stale, err := f.dispatchSpec(f.coord, spec{title: "three", checks: checks, reserved: []string{"src/three.txt"}})
	if err != nil {
		t.Fatalf("dispatch three: %v", err)
	}
	stale = f.block(stale, func(next *task.Task) {
		// The task's checkout is gone, so its retry really calls `worktree create`.
		next.Branch = "orch/retry-without-checkout"
		next.Phase = task.Intent
		next.WorktreePath, next.WorktreeWorkspaceID, next.CurrentWorkspaceID = "", "", ""
		next.LaunchPaneID, next.PublicPaneID = "", ""
		next.AgentName, next.OMPSessionID, next.TerminalID = "", "", ""
		next.Attention = "task " + next.ID + " could not create the worktree: the checkout was refused"
	})
	if slots, err := f.store.ProvisioningSlots(ctx, f.projectKey()); err != nil || slots != 3 {
		t.Fatalf("ProvisioningSlots before the retry = (%d, %v), want the full budget of 3", slots, err)
	}
	// The claim this attempt acquired. The retry renews the claim it reuses, so this
	// token stops being the task's the moment the retry takes over.
	staleToken := f.slotClaimToken(stale.ID)

	// Another coordinator process resumes the task: it retires the recorded
	// attention, takes the task's step, and stays inside `worktree create` — whose
	// refusal this fixture delays — so the test can act in exactly that window.
	f.updatePlan(func(p *fixturePlan) {
		p.WorktreeCreateDelayMillis = 1500
		p.WorktreeCreateError = "not_git_worktree"
	})
	sibling := f.sibling()
	retried := make(chan error, 1)
	go func() {
		_, err := sibling.ResumeDispatch(ctx, stale.ID)
		retried <- err
	}()
	marker := filepath.Join(f.dir, fixtureCreateStartedName)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the retry never reached its worktree create")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resumed := f.taskByID(t, stale.ID); resumed.Phase != task.Intent || resumed.Attention != "" {
		t.Fatalf("the retry did not take the task over: phase %q attention %q", resumed.Phase, resumed.Attention)
	}

	// The failed attempt's release runs here, presenting the claim it acquired. The
	// retry renewed that claim when it took the task over, so the release must be
	// refused: the capacity belongs to the retry, and clearing it would let the
	// project admit work past its budget while the retry is still provisioning.
	if err := f.store.ReleaseProvisioningSlot(ctx, stale.ID, staleToken); !errors.Is(err, state.ErrConflict) {
		t.Errorf("the failed attempt's late release = %v, want ErrConflict: the retry's claim was not renewed", err)
	}
	if slots, err := f.store.ProvisioningSlots(ctx, f.projectKey()); err != nil || slots != 3 {
		t.Errorf("ProvisioningSlots after the old release = (%d, %v), want 3: the retry's active capacity claim was cleared", slots, err)
	}

	// The retry runs its own recorded phase to its own conclusion, so the window
	// closed on a task that was still owned by it.
	if err := <-retried; err == nil {
		t.Error("the retry's definite worktree failure was reported as success")
	}
	after := f.taskByID(t, stale.ID)
	if after.Phase != task.Intent || after.Attention == "" {
		t.Errorf("the retry ended at phase %q with attention %q, want its own definite failure recorded at intent", after.Phase, after.Attention)
	}
	if slots, err := f.store.ProvisioningSlots(ctx, f.projectKey()); err != nil || slots != 2 {
		t.Errorf("ProvisioningSlots after the retry's own failure = (%d, %v), want the released slot counted once (2)", slots, err)
	}
}

// TestResumeStepInFlightTouchesNothing pins the order a resume works in when it
// loses the step: the durable step claim comes first, and only a caller that got it
// takes capacity or retires the retryable reason.
//
// The interleaving it covers is the one where an older attempt still owns the
// worktree step and is between recording its definite failure and giving its slot
// back. A resume that renewed the claim (or cleared the attention) before learning
// that the step is not its own would take the capacity past the failed attempt —
// whose release then no longer matches — and erase the retry reason of a step it
// never ran, leaving a slot charged and nobody provisioning.
func TestResumeStepInFlightTouchesNothing(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()

	// A task admitted and then rewound to the state an interrupted attempt leaves:
	// it holds its claim, carries the retryable reason, and has no resources.
	admitted := f.mustDispatch("one", "brief one")
	stale := f.block(admitted, func(next *task.Task) {
		next.Branch = "orch/interrupted-without-checkout"
		next.Phase = task.Intent
		next.WorktreePath, next.WorktreeWorkspaceID, next.CurrentWorkspaceID = "", "", ""
		next.LaunchPaneID, next.PublicPaneID = "", ""
		next.AgentName, next.OMPSessionID, next.TerminalID = "", "", ""
		next.Attention = "task " + next.ID + " could not create the worktree: the checkout was refused"
	})
	token := f.slotClaimToken(stale.ID)

	// The older attempt owns the worktree step right now.
	if took, err := f.store.ClaimStep(ctx, stale.ID, task.Intent, "attempt-a", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep(attempt-a) = (%v, %v), want the live step claim", took, err)
	}
	before := f.countCalls("worktree create")

	// The resume cannot take the step, so it must have changed nothing at all.
	sibling := f.sibling()
	if _, err := sibling.ResumeDispatch(ctx, stale.ID); !errors.Is(err, ErrStepInFlight) {
		t.Fatalf("ResumeDispatch under a live step = %v, want ErrStepInFlight", err)
	}
	if got := f.slotClaimToken(stale.ID); got != token {
		t.Errorf("the resume renewed the claim under a step it does not own: token %d -> %d", token, got)
	}
	if after := f.taskByID(t, stale.ID); after.Attention != stale.Attention {
		t.Errorf("the resume retired the retryable reason of a step it never ran: %q", after.Attention)
	}
	if got := f.countCalls("worktree create"); got != before {
		t.Errorf("the resume issued %d worktree creates although it never owned the step", got-before)
	}
	if slots, err := f.store.ProvisioningSlots(ctx, f.projectKey()); err != nil || slots != 1 {
		t.Errorf("ProvisioningSlots after the refused resume = (%d, %v), want the claim the older attempt still holds", slots, err)
	}

	// The older attempt records its definite failure and gives its own slot back:
	// because no renewal got in the way, that release matches and the capacity is
	// free again.
	if _, _, err := f.store.RecordStepFailure(ctx, stale.ID, f.taskByID(t, stale.ID).Revision, func(next *task.Task) error {
		next.Attention = "task " + next.ID + " could not create the worktree: the checkout was refused"
		return nil
	}, "attempt-a", task.Intent, true); err != nil {
		t.Fatalf("RecordStepFailure: %v", err)
	}
	if err := f.store.ReleaseProvisioningSlot(ctx, stale.ID, token); err != nil {
		t.Fatalf("the older attempt's release = %v, want the capacity back", err)
	}
	if slots, err := f.store.ProvisioningSlots(ctx, f.projectKey()); err != nil || slots != 0 {
		t.Errorf("ProvisioningSlots after the older attempt released = (%d, %v), want 0", slots, err)
	}
}

// TestResumeWithoutCapacityReleasesTheStepItTook pins the other side of that order:
// a resume takes the step claim before it looks for capacity, so when the budget is
// spent it must give the step back and leave the task queued — no external call, and
// no claim left for a later retry to wait out.
func TestResumeWithoutCapacityReleasesTheStepItTook(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()
	checks := []string{"go test ./... ok"}

	// Three workers hold the whole budget; the fourth task is queued and holds no
	// claim at all, which is the state a resume finds after its attempt gave the slot
	// back.
	for i, title := range []string{"one", "two", "three"} {
		if _, err := f.dispatchSpec(f.coord, spec{title: title, checks: checks, reserved: []string{fmt.Sprintf("src/%d.txt", i)}}); err != nil {
			t.Fatalf("dispatch %s: %v", title, err)
		}
	}
	queued, err := f.dispatchSpec(f.coord, spec{title: "queued", checks: checks})
	if err != nil {
		t.Fatalf("dispatch queued: %v", err)
	}
	if queued.Phase != task.Intent || f.slotClaimToken(queued.ID) != 0 {
		t.Fatalf("the queued task = phase %q token %d, want an unclaimed intent", queued.Phase, f.slotClaimToken(queued.ID))
	}
	before := f.countCalls("worktree create")

	resumed, err := f.coord.provision(ctx, queued, slotClaim{renew: true})
	if err != nil {
		t.Fatalf("provision(renew without capacity) = %v, want the task left queued", err)
	}
	if resumed.Phase != task.Intent {
		t.Errorf("phase = %q, want %q: nothing may be provisioned without a slot", resumed.Phase, task.Intent)
	}
	if got := f.countCalls("worktree create"); got != before {
		t.Errorf("worktree create calls = %d, want %d: a task without capacity issued a side effect", got, before)
	}
	if slots, err := f.store.ProvisioningSlots(ctx, f.projectKey()); err != nil || slots != 3 {
		t.Errorf("ProvisioningSlots = (%d, %v), want the three workers' claims and nothing else", slots, err)
	}
	// The step it took was given back, so the next retry is not stuck behind a lease.
	if took, err := f.store.ClaimStep(ctx, queued.ID, task.Intent, "probe-owner", time.Minute); err != nil || !took {
		t.Errorf("ClaimStep after the refused resume = (%v, %v), want the step free again", took, err)
	}
}

// ---------------------------------------------------------------------------
// adjudicating an ambiguous brief submission
// ---------------------------------------------------------------------------

// ambiguousBrief produces the stuck state a real ambiguous submission leaves: an
// AgentStarted task whose blocker is that submission, with a live worker.
func (f *coordFixture) ambiguousBrief(title string) task.Task {
	f.t.Helper()
	f.updatePlan(func(p *fixturePlan) { p.PromptAmbiguous = true })
	st, err := f.dispatchSpec(f.coord, spec{title: title, checks: []string{"go test ./... ok"}})
	if !errors.Is(err, herdr.ErrAmbiguous) {
		f.t.Fatalf("dispatch err = %v, want an ambiguous submission", err)
	}
	if !f.isAdjudicable(st) {
		f.t.Fatalf("task %s is not in the ambiguous state: phase=%q blocker=%q", st.ID, st.Phase, st.Blocker)
	}
	f.updatePlan(func(p *fixturePlan) { p.PromptAmbiguous = false })
	return f.block(st, func(next *task.Task) {
		next.Attention = "task 5's question marker stays untouched"
	})
}

// isAdjudicable asks the coordinator whether the task carries the ambiguous brief
// blocker, through the same predicate the adjudication uses.
func (f *coordFixture) isAdjudicable(st task.Task) bool {
	f.t.Helper()
	return IsAmbiguousBriefTask(st)
}

func TestAdjudicateBriefDeliveredRecordsTheSubmissionWithoutPrompting(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.ambiguousBrief("one")
	before := f.countCalls("agent prompt")

	evidence, text, err := f.coord.BriefInspection(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("BriefInspection: %v", err)
	}
	if text == "" || evidence.PaneID != st.PublicPaneID || evidence.Digest == "" {
		t.Fatalf("inspection = (%+v, %q), want the pane and the text", evidence, text)
	}

	decided, err := f.coord.AdjudicateBrief(context.Background(), st.ID, st.Revision, BriefDelivered, evidence)
	if err != nil {
		t.Fatalf("AdjudicateBrief(delivered): %v", err)
	}
	// The submission is recorded as delivered and nothing was sent again.
	if decided.Phase != task.PromptSubmitted {
		t.Errorf("phase = %q, want %q", decided.Phase, task.PromptSubmitted)
	}
	if decided.Blocker != "" {
		t.Errorf("the ambiguity blocker survived the adjudication: %q", decided.Blocker)
	}
	if decided.Attention != "task 5's question marker stays untouched" {
		t.Errorf("attention = %q, want the worker's own question untouched", decided.Attention)
	}
	if got := f.countCalls("agent prompt"); got != before {
		t.Errorf("agent prompt calls = %d, want %d: a delivered brief was submitted again", got, before)
	}
	// The worker's question still awaits the user, so the stage stays Needs input
	// until Task 5's answer machinery resolves it; the adjudication only recorded
	// the submission, it did not answer anything.
	if got := task.StageOf(decided); got != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input while the worker's question stands", got)
	}
	answered := f.block(decided, func(next *task.Task) {
		next.Attention = ""
	})
	if got := task.StageOf(answered); got != task.Running {
		t.Errorf("stage after the question is answered = %q, want running", got)
	}
	// The decision is durable and auditable.
	notes, err := f.store.PendingInbox(context.Background(), f.root)
	if err != nil {
		t.Fatalf("PendingInbox: %v", err)
	}
	found := false
	for _, note := range notes {
		if note.TaskID == st.ID && strings.Contains(note.Summary, "delivered") {
			found = true
		}
	}
	if !found {
		t.Errorf("no durable note records the adjudication: %+v", notes)
	}
	// A second decision has nothing to adjudicate.
	if _, err := f.coord.AdjudicateBrief(context.Background(), st.ID, decided.Revision, BriefDelivered, evidence); !errors.Is(err, ErrNotAdjudicable) {
		t.Errorf("second adjudication err = %v, want ErrNotAdjudicable", err)
	}
}

func TestAdjudicateBriefNotDeliveredResumesExactlyOnce(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.ambiguousBrief("one")
	before := f.countCalls("agent prompt")

	evidence, _, err := f.coord.BriefInspection(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("BriefInspection: %v", err)
	}
	decided, err := f.coord.AdjudicateBrief(context.Background(), st.ID, st.Revision, BriefNotDelivered, evidence)
	if err != nil {
		t.Fatalf("AdjudicateBrief(not_delivered): %v", err)
	}
	if decided.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q", decided.Phase, task.PromptSubmitted)
	}
	if decided.Blocker != "" {
		t.Errorf("the ambiguity blocker survived the adjudication: %q", decided.Blocker)
	}
	if decided.Attention == "" {
		t.Error("the adjudication cleared an attention it does not own")
	}
	if got := f.countCalls("agent prompt"); got != before+1 {
		t.Errorf("agent prompt calls = %d, want %d: the resume submitted the brief once", got, before+1)
	}
	// Once: another decision finds nothing to adjudicate and sends nothing.
	if _, err := f.coord.AdjudicateBrief(context.Background(), st.ID, decided.Revision, BriefNotDelivered, evidence); !errors.Is(err, ErrNotAdjudicable) {
		t.Fatalf("second adjudication err = %v, want ErrNotAdjudicable", err)
	}
	if got := f.countCalls("agent prompt"); got != before+1 {
		t.Errorf("agent prompt calls = %d, want no further submission", got)
	}
}

func TestAdjudicateBriefRefusesChangedOrForeignEvidence(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.ambiguousBrief("one")
	ctx := context.Background()
	evidence, inspected, err := f.coord.BriefInspection(ctx, st.ID)
	if err != nil {
		t.Fatalf("BriefInspection: %v", err)
	}

	// Another pane: the decision must be about the worker the user inspected.
	foreign := evidence
	foreign.PaneID = "w9:p9"
	if _, err := f.coord.AdjudicateBrief(ctx, st.ID, st.Revision, BriefDelivered, foreign); !errors.Is(err, ErrEvidenceChanged) {
		t.Errorf("foreign pane err = %v, want ErrEvidenceChanged", err)
	}
	// A stale revision: something changed the task between the inspection and the
	// decision, so the decision may no longer describe it.
	f.block(st, func(next *task.Task) { next.Activity = "moved on" })
	if _, err := f.coord.AdjudicateBrief(ctx, st.ID, st.Revision, BriefDelivered, evidence); !errors.Is(err, state.ErrConflict) {
		t.Errorf("stale revision err = %v, want state.ErrConflict", err)
	}
	current, err := f.store.Task(ctx, st.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	// The terminal moved on since the inspection: re-reading yields a different
	// digest, so the decision is refused.
	f.updatePlan(func(p *fixturePlan) { p.AgentReadText = "the worker is now asking a question" })
	if _, err := f.coord.AdjudicateBrief(ctx, st.ID, current.Revision, BriefDelivered, evidence); !errors.Is(err, ErrEvidenceChanged) {
		t.Errorf("moved-on terminal err = %v, want ErrEvidenceChanged", err)
	}
	// A decision that names no decision is a caller error.
	if _, err := f.coord.AdjudicateBrief(ctx, st.ID, current.Revision, "maybe", evidence); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("unknown decision err = %v, want ErrInvalidRequest", err)
	}

	// The pane now holds a different session: the terminal text is restored to what
	// was inspected, so the digest matches and only the worker's identity can tell
	// that the inspected worker is gone. The decision must be refused.
	f.updatePlan(func(p *fixturePlan) { p.AgentReadText = inspected })
	f.updatePlan(func(p *fixturePlan) { p.SessionRefValue = "omp-session-replaced" })
	if _, err := f.coord.AdjudicateBrief(ctx, st.ID, current.Revision, BriefDelivered, evidence); !errors.Is(err, ErrEvidenceChanged) {
		t.Errorf("replaced session err = %v, want ErrEvidenceChanged", err)
	}
	f.updatePlan(func(p *fixturePlan) { p.SessionRefValue = "" })

	// The pane holds a different worker by name: the session still matches, but the
	// recorded worker is not the one on the pane, so the decision is refused.
	wrongWorker := f.block(current, func(next *task.Task) { next.AgentName = "orch_someone_else" })
	if _, err := f.coord.AdjudicateBrief(ctx, st.ID, wrongWorker.Revision, BriefDelivered, evidence); !errors.Is(err, ErrEvidenceChanged) {
		t.Errorf("wrong worker err = %v, want ErrEvidenceChanged", err)
	}

	// Nothing was applied by any of those refusals.
	after, err := f.store.Task(ctx, st.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Phase != task.AgentStarted || after.Blocker != st.Blocker {
		t.Errorf("a refused adjudication changed the task: %+v", after)
	}
}

func TestAdjudicationRetiresTheStepClaimItDecidedAbout(t *testing.T) {
	// The claim can outlive its lease while the user thinks, so a concurrent resume
	// may hold the step again by the time the decision lands. The decision retires
	// the claim in the same transaction that clears the blocker, so the brief goes
	// out once and no claim survives to be raced.
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()
	st := f.ambiguousBrief("one")
	before := f.countCalls("agent prompt")

	// Another caller holds the step by the time the decision lands (the lease can
	// outlive the caller that wrote the blocker while the user thinks).
	f.coord.releaseStep(ctx, st)
	if took, err := f.store.ClaimStep(ctx, st.ID, task.AgentStarted, "another-caller", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep as another caller = (%v, %v), want the claim", took, err)
	}
	current, err := f.store.Task(ctx, st.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	evidence, _, err := f.coord.BriefInspection(ctx, st.ID)
	if err != nil {
		t.Fatalf("BriefInspection: %v", err)
	}
	decided, err := f.coord.AdjudicateBrief(ctx, st.ID, current.Revision, BriefNotDelivered, evidence)
	if err != nil {
		t.Fatalf("AdjudicateBrief(not_delivered): %v", err)
	}
	if decided.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q", decided.Phase, task.PromptSubmitted)
	}
	if got := f.countCalls("agent prompt"); got != before+1 {
		t.Errorf("agent prompt calls = %d, want %d: the decision's own resume submitted the brief once", got, before+1)
	}
	// The decided step's claim is gone, and the phase has moved on, so nothing can
	// re-run it: the next step is the one that is now claimable.
	if took, err := f.store.ClaimStep(ctx, st.ID, task.AgentStarted, "probe", time.Minute); err != nil || took {
		t.Errorf("ClaimStep for the decided step = (%v, %v), want the phase to refuse it", took, err)
	}
}

func TestAdjudicateBriefRefusesEveryOtherBlockerAndPhase(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()
	checks := []string{"go test ./... ok"}

	// A running task with no blocker is not adjudicable.
	running, err := f.dispatchSpec(f.coord, spec{title: "running", checks: checks})
	if err != nil {
		t.Fatalf("dispatch running: %v", err)
	}
	if _, err := f.coord.AdjudicateBrief(ctx, running.ID, running.Revision, BriefDelivered, BriefEvidence{PaneID: running.PublicPaneID, Digest: "x"}); !errors.Is(err, ErrNotAdjudicable) {
		t.Errorf("running task err = %v, want ErrNotAdjudicable", err)
	}
	// Neither is Task 5's input-attempt marker: a different feature owns it.
	asked := f.block(running, func(next *task.Task) {
		next.Blocker = "input attempt: answer abc123 - 1790366337785"
		next.Phase = task.AgentStarted
	})
	evidence := BriefEvidence{PaneID: asked.PublicPaneID, Digest: "x"}
	if _, err := f.coord.AdjudicateBrief(ctx, asked.ID, asked.Revision, BriefDelivered, evidence); !errors.Is(err, ErrNotAdjudicable) {
		t.Errorf("input-attempt marker err = %v, want ErrNotAdjudicable", err)
	}
	// And an ambiguity from another step is not the brief's: the predicate is
	// exact, so only this task's own submission failure is accepted.
	other := f.block(asked, func(next *task.Task) {
		next.Blocker = "task " + next.ID + " could not create the worktree: coord: the step's outcome could not be identified"
	})
	evidence = BriefEvidence{PaneID: other.PublicPaneID, Digest: "x"}
	if _, err := f.coord.AdjudicateBrief(ctx, other.ID, other.Revision, BriefDelivered, evidence); !errors.Is(err, ErrNotAdjudicable) {
		t.Errorf("other step's ambiguity err = %v, want ErrNotAdjudicable", err)
	}
	// The inspection itself refuses outside the ambiguous state.
	if _, _, err := f.coord.BriefInspection(ctx, running.ID); !errors.Is(err, ErrNotAdjudicable) {
		t.Errorf("BriefInspection(running) err = %v, want ErrNotAdjudicable", err)
	}
}

// ---------------------------------------------------------------------------
// the durable step claim
// ---------------------------------------------------------------------------

func TestConcurrentResumesIssueOneCommandPerStep(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	checks := []string{"go test ./... ok"}

	// The brief step is the one Herdr cannot protect: two callers resuming the same
	// AgentStarted task would each submit it, and nothing on the other side would
	// notice. Only the durable step claim stops that.
	submitted, err := f.dispatchSpec(f.coord, spec{title: "one", checks: checks})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	atBrief := f.block(submitted, func(next *task.Task) {
		next.Phase = task.AgentStarted
	})
	f.coord.releaseStep(context.Background(), atBrief)
	before := f.countCalls("agent prompt")
	sibling := f.sibling()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, c := range []*Coordinator{f.coord, sibling} {
		wg.Add(1)
		go func(c *Coordinator) {
			defer wg.Done()
			<-start
			_, err := c.ResumeDispatch(context.Background(), atBrief.ID)
			errs <- err
		}(c)
	}
	close(start)
	wg.Wait()
	close(errs)

	inFlight := 0
	for err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ErrStepInFlight):
			inFlight++
		default:
			t.Errorf("concurrent resume err = %v", err)
		}
	}
	if inFlight != 1 {
		t.Errorf("callers told the step was in flight = %d, want exactly 1", inFlight)
	}
	if got := f.countCalls("agent prompt"); got != before+1 {
		t.Fatalf("agent prompt calls = %d, want %d: the brief was submitted twice", got, before+1)
	}
	final, err := f.store.Task(context.Background(), atBrief.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if final.Phase != task.PromptSubmitted || final.Attention != "" || final.Blocker != "" {
		t.Errorf("the resumed task = phase %q attention %q, want a clean submitted task", final.Phase, final.Attention)
	}
	if got := f.prompts(final.PublicPaneID); len(got) != before+1 {
		t.Errorf("prompts = %v, want %d: one brief from the dispatch and one from the winner", got, before+1)
	}

	// The same for the worker-start step: without the claim both callers would run
	// it, and the loser would record a failure on a task the winner is finishing.
	atStart := f.block(final, func(next *task.Task) {
		next.Phase = task.WorktreeCreated
		next.LaunchPaneID, next.PublicPaneID = "", ""
		next.AgentName, next.OMPSessionID, next.TerminalID = "", "", ""
	})
	f.coord.releaseStep(context.Background(), atStart)
	beforeStart := f.countCalls("agent start")
	beforePrompts := f.countCalls("agent prompt")
	other := f.sibling()
	errs = make(chan error, 2)
	start = make(chan struct{})
	for _, c := range []*Coordinator{f.coord, other} {
		wg.Add(1)
		go func(c *Coordinator) {
			defer wg.Done()
			<-start
			_, err := c.ResumeDispatch(context.Background(), atStart.ID)
			errs <- err
		}(c)
	}
	close(start)
	wg.Wait()
	close(errs)
	inFlight = 0
	for err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ErrStepInFlight):
			inFlight++
		default:
			t.Errorf("concurrent start err = %v", err)
		}
	}
	if inFlight != 1 {
		t.Errorf("callers told the step was in flight = %d, want exactly 1", inFlight)
	}
	// One caller ran the step: it adopted the agent that was already live on the
	// pane, so no start was issued at all, and the brief went out once.
	if got := f.countCalls("agent start"); got != beforeStart {
		t.Errorf("agent start calls = %d, want %d: two callers ran the start step", got, beforeStart)
	}
	if got := f.countCalls("agent prompt"); got != beforePrompts+1 {
		t.Errorf("agent prompt calls = %d, want %d", got, beforePrompts+1)
	}
	started, err := f.store.Task(context.Background(), atStart.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if started.Attention != "" || started.Blocker != "" {
		t.Errorf("a losing caller recorded a failure on a running task: attention=%q blocker=%q",
			started.Attention, started.Blocker)
	}
	if started.Phase != task.PromptSubmitted {
		t.Errorf("phase = %q, want %q", started.Phase, task.PromptSubmitted)
	}
}

func TestDefiniteAgentStartRefusalReleasesTheClaimForAnImmediateResume(t *testing.T) {
	// A definite refusal provably wrote nothing, so the claim goes back at once and
	// a resume may run immediately instead of waiting out the two-minute lease.
	f := newCoordFixture(t, fixturePlan{AgentStartError: "agent_start_failed"})
	ctx := context.Background()
	failed, err := f.dispatchSpec(f.coord, spec{title: "one", checks: []string{"go test ./... ok"}})
	if !errors.Is(err, herdr.ErrCLI) {
		t.Fatalf("dispatch err = %v, want a Herdr CLI failure", err)
	}
	if failed.Phase != task.WorktreeCreated || failed.Attention == "" {
		t.Fatalf("failed task = phase %q attention %q, want a retryable failure at worktree_created",
			failed.Phase, failed.Attention)
	}
	// No lease wait: the claim the failed step held is already gone.
	if took, err := f.store.ClaimStep(ctx, failed.ID, task.WorktreeCreated, "probe", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep after a definite refusal = (%v, %v), want the claim released", took, err)
	}
	if err := f.store.ReleaseStep(ctx, failed.ID, "probe"); err != nil {
		t.Fatalf("ReleaseStep: %v", err)
	}

	f.updatePlan(func(p *fixturePlan) { p.AgentStartError = "" })
	beforeStart := f.countCalls("agent start")
	resumed, err := f.resume(failed.ID)
	if err != nil {
		t.Fatalf("immediate resume after a definite refusal: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Fatalf("phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
	if got := f.countCalls("agent start"); got != beforeStart+1 {
		t.Errorf("agent start calls = %d, want %d", got, beforeStart+1)
	}
	if resumed.Attention != "" {
		t.Errorf("attention survived the successful retry: %q", resumed.Attention)
	}
}

func TestAmbiguousAgentStartKeepsItsBlockerAndClaimUntilInspected(t *testing.T) {
	// An ambiguous start may have taken effect, so nothing is released and nothing
	// retries automatically: the task is blocked for a human, and the claim stays
	// until its lease expires or a user decides.
	f := newCoordFixture(t, fixturePlan{AgentStartTimeout: true})
	ctx := context.Background()
	st, err := f.dispatchSpec(f.coord, spec{title: "one", checks: []string{"go test ./... ok"}})
	if !errors.Is(err, herdr.ErrAmbiguous) {
		t.Fatalf("dispatch err = %v, want an ambiguous outcome", err)
	}
	if st.Blocker == "" || st.Attention != "" {
		t.Fatalf("task = blocker %q attention %q, want a blocker only", st.Blocker, st.Attention)
	}
	if got := task.StageOf(st); got != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", got)
	}
	// The claim is still held: another owner cannot take that step.
	phase := st.Phase
	if took, err := f.store.ClaimStep(ctx, st.ID, phase, "probe", time.Minute); err != nil || took {
		t.Fatalf("ClaimStep after an ambiguous start = (%v, %v), want the claim kept", took, err)
	}
	// And nothing resumes it automatically, even with the cause "fixed".
	f.updatePlan(func(p *fixturePlan) { p.AgentStartTimeout = false })
	if _, err := f.resume(st.ID); !errors.Is(err, ErrBlocked) {
		t.Fatalf("resume of a blocked task err = %v, want ErrBlocked", err)
	}
	// Nothing was recorded on the pane either: the outcome is genuinely unknown, so
	// no caller may proceed on its own.
	if live := f.state().Agents[st.LaunchPaneID]; live != nil {
		t.Errorf("pane %q reports an agent, so this was not an unknown outcome", st.LaunchPaneID)
	}
	if got := f.prompts(st.PublicPaneID); len(got) != 0 {
		t.Errorf("prompts = %v, want none before a human decides", got)
	}
}

func TestStaleFailureIsNotRecordedOntoThePhaseThatAdvanced(t *testing.T) {
	// A definite failure releases its step claim so a retry can run at once, which
	// means another coordinator can advance the task before the failure that released
	// the claim is written. That writer must then refuse: recording its failure onto
	// the newer phase would undo the progress and, worse, hide it behind a failure
	// that describes a step nobody is running any more.
	f := newCoordFixture(t, fixturePlan{WorktreeCreateError: "worktree_create_failed"})
	ctx := context.Background()
	stale, err := f.dispatchSpec(f.coord, spec{title: "one", checks: []string{"go test ./... ok"}})
	if !errors.Is(err, herdr.ErrCLI) {
		t.Fatalf("dispatch err = %v, want a definite Herdr failure", err)
	}
	// The create step runs *from* the intent phase, so a definite create failure
	// leaves the task recorded exactly there.
	if stale.Phase != task.Intent {
		t.Fatalf("phase = %q, want %q", stale.Phase, task.Intent)
	}

	// The retry another coordinator runs takes the step and advances the task past it.
	f.updatePlan(func(p *fixturePlan) { p.WorktreeCreateError = "" })
	sibling := f.sibling()
	advanced, err := f.resume(stale.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if advanced.Phase != task.PromptSubmitted {
		t.Fatalf("phase after the retry = %q, want %q", advanced.Phase, task.PromptSubmitted)
	}
	if advanced.Attention != "" {
		t.Fatalf("attention after the retry = %q, want it cleared", advanced.Attention)
	}
	// Another caller holds the step the retry left the task on. The stale failure
	// belongs to the phase it failed, so it must not release this claim: a release
	// that matched nothing would have to be caught here, not assumed.
	if took, err := f.store.ClaimStep(ctx, advanced.ID, advanced.Phase, "probe", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep for the retry's phase = (%v, %v), want the claim", took, err)
	}

	// Now the stale writer records its failure. It must fail closed, leave the task
	// and its progress alone, and say that recording failed.
	_, err = f.coord.failTask(ctx, stale, "create the worktree", false,
		fmt.Errorf("worktree create for %s failed", stale.ProjectKey))
	var persistence *PersistenceError
	if !errors.As(err, &persistence) {
		t.Fatalf("stale failure err = %v, want a PersistenceError", err)
	}
	if !errors.Is(err, state.ErrConflict) {
		t.Errorf("stale failure err = %v, want it to report the conflict", err)
	}
	after, err := f.store.Task(ctx, stale.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if after.Phase != task.PromptSubmitted || after.Attention != "" || after.Revision != advanced.Revision {
		t.Fatalf("task after the stale failure = %+v, want the retry's progress untouched", after)
	}
	// The failed create never reached the worker, so the retry's brief is the only one.
	if got := f.countCalls("agent prompt"); got != 1 {
		t.Errorf("agent prompt calls = %d, want 1 (the retry's brief)", got)
	}
	// The claim that was not this writer's is still where it was.
	if took, err := f.store.ClaimStep(ctx, advanced.ID, advanced.Phase, "probe-2", time.Minute); err != nil || took {
		t.Errorf("ClaimStep after the stale failure = (%v, %v), want the other caller's claim kept", took, err)
	}
	_ = sibling
}

func TestStaleFailuresAreRefusedWhenAnotherOwnerHoldsTheStep(t *testing.T) {
	// The other half of the stale-failure race: the owner of the step changed before
	// the failure was recorded (the lease expired and another caller took the step).
	// The late writer must write nothing at all — neither an attention nor a blocker —
	// because the outcome it carries describes a step another caller now owns.
	for _, tc := range []struct {
		name     string
		cause    error
		terminal bool
	}{
		{name: "definite", cause: herdr.ErrCLI, terminal: false},
		{name: "ambiguous", cause: herdr.ErrAmbiguous, terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCoordFixture(t, fixturePlan{})
			ctx := context.Background()
			st, err := f.dispatchSpec(f.coord, spec{title: "one", checks: []string{"go test ./... ok"}})
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			before := f.countCalls("agent prompt")
			// The task's current step belongs to another caller now.
			if took, err := f.store.ClaimStep(ctx, st.ID, st.Phase, "another-caller", time.Minute); err != nil || !took {
				t.Fatalf("ClaimStep as another caller = (%v, %v), want the claim", took, err)
			}

			_, err = f.coord.failTask(ctx, st, "submit the brief", tc.terminal,
				fmt.Errorf("submit the brief for task %s failed: %w", st.ID, tc.cause))
			var persistence *PersistenceError
			if !errors.As(err, &persistence) {
				t.Fatalf("late %s failure err = %v, want a PersistenceError", tc.name, err)
			}
			if !errors.Is(err, state.ErrStepNotHeld) {
				t.Errorf("late %s failure err = %v, want it to report the lost step", tc.name, err)
			}
			after, err := f.store.Task(ctx, st.ID)
			if err != nil {
				t.Fatalf("Task: %v", err)
			}
			if after.Attention != "" || after.Blocker != "" {
				t.Errorf("late %s failure wrote %q/%q onto the task, want nothing", tc.name, after.Attention, after.Blocker)
			}
			if after.Phase != st.Phase || after.Revision != st.Revision {
				t.Errorf("late %s failure changed the task: %+v, want it untouched", tc.name, after)
			}
			// The other caller's claim is still exactly theirs, and no brief went out.
			if took, err := f.store.ClaimStep(ctx, st.ID, st.Phase, "probe", time.Minute); err != nil || took {
				t.Errorf("ClaimStep after the late %s failure = (%v, %v), want the other caller's claim kept", tc.name, took, err)
			}
			if got := f.countCalls("agent prompt"); got != before {
				t.Errorf("agent prompt calls = %d, want %d: a refused failure sent a brief", got, before)
			}
		})
	}
}

func TestAnUndecidedFailureWithoutALiveOwnerStillBlocksTheRetry(t *testing.T) {
	// The brief's fate is unknown and the lease that carried the step has expired
	// without anyone taking it over. The failure must still be recorded as a blocker:
	// the alternative is a task that looks resumable and would resubmit a brief whose
	// fate nobody established.
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()
	st := f.ambiguousBrief("one")
	// Back to the state before the ambiguity was recorded: the worker's pane is live
	// and the task stands at the step that submits the brief, with nothing recorded.
	clear := f.block(st, func(next *task.Task) {
		next.Blocker = ""
		next.Attention = ""
	})
	before := f.countCalls("agent prompt")
	// The recorded claim is this coordinator's, and its lease has run out.
	f.coord.releaseStep(ctx, clear)
	if took, err := f.store.ClaimStep(ctx, clear.ID, clear.Phase, f.coord.owner, 20*time.Millisecond); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the claim", took, err)
	}
	time.Sleep(40 * time.Millisecond)

	_, err := f.coord.failTask(ctx, clear, "submit the brief", true,
		fmt.Errorf("submit the brief for task %s: %w", clear.ID, herdr.ErrAmbiguous))
	if !errors.Is(err, herdr.ErrAmbiguous) {
		t.Fatalf("failure err = %v, want the ambiguous cause reported", err)
	}
	blocked, err := f.store.Task(ctx, clear.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if blocked.Blocker == "" || blocked.Attention != "" {
		t.Fatalf("task = blocker %q attention %q, want a blocker for the undecided outcome", blocked.Blocker, blocked.Attention)
	}
	if blocked.Phase != task.AgentStarted {
		t.Fatalf("phase = %q, want %q", blocked.Phase, task.AgentStarted)
	}
	if got := task.StageOf(blocked); got != task.NeedsInput {
		t.Errorf("stage = %q, want needs_input", got)
	}
	// And the retry is refused: this task needs an inspection, not a resubmission.
	if _, err := f.resume(clear.ID); !errors.Is(err, ErrBlocked) {
		t.Fatalf("resume of the blocked task err = %v, want ErrBlocked", err)
	}
	if got := f.countCalls("agent prompt"); got != before {
		t.Errorf("agent prompt calls = %d, want %d: the brief was blindly resubmitted", got, before)
	}
}

func TestStepClaimLeaseKeepsCrashRecovery(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	ctx := context.Background()
	st, err := f.dispatchSpec(f.coord, spec{title: "one", checks: []string{"go test ./... ok"}})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	atBrief := f.block(st, func(next *task.Task) {
		next.Phase = task.AgentStarted
	})
	f.coord.releaseStep(ctx, atBrief)
	before := f.countCalls("agent prompt")

	// A live claim for this very step stops another caller: that is the
	// no-duplicate half.
	if took, err := f.store.ClaimStep(ctx, atBrief.ID, task.AgentStarted, "another-caller", time.Minute); err != nil || !took {
		t.Fatalf("ClaimStep = (%v, %v), want the claim", took, err)
	}
	if _, err := f.resume(atBrief.ID); !errors.Is(err, ErrStepInFlight) {
		t.Fatalf("resume with a live claim err = %v, want ErrStepInFlight", err)
	}
	if got := f.countCalls("agent prompt"); got != before {
		t.Fatalf("agent prompt calls = %d, want %d", got, before)
	}
	// The claim is released by the caller that failed before any side effect, so a
	// retry does not have to wait for the lease.
	if err := f.coord.store.ReleaseStep(ctx, atBrief.ID, "another-caller"); err != nil {
		t.Fatalf("ReleaseStep: %v", err)
	}
	// A crashed caller's claim expires, and the next resume continues from the
	// recorded phase: crash recovery is not weakened by the exclusivity.
	if took, err := f.store.ClaimStep(ctx, atBrief.ID, task.AgentStarted, "dead-caller", 20*time.Millisecond); err != nil || !took {
		t.Fatalf("ClaimStep(dead) = (%v, %v), want the claim", took, err)
	}
	time.Sleep(40 * time.Millisecond)
	identity, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	f.store.MarkTaskComplete(identity, atBrief.ID)
	resumed, err := f.resume(atBrief.ID)
	if err != nil {
		t.Fatalf("resume after the lease expired and owner completed: %v", err)
	}
	if resumed.Phase != task.PromptSubmitted {
		t.Errorf("phase = %q, want %q", resumed.Phase, task.PromptSubmitted)
	}
	if got := f.countCalls("agent prompt"); got != before+1 {
		t.Errorf("agent prompt calls = %d, want %d", got, before+1)
	}
}

func TestBriefInspectionForStaysInsideThePrimariesProject(t *testing.T) {
	f := newCoordFixture(t, fixturePlan{})
	st := f.ambiguousBrief("one")
	ctx := context.Background()

	// The primary's own project may inspect and decide.
	result, err := f.coord.BriefInspectionFor(ctx, f.root, st.ID)
	if err != nil {
		t.Fatalf("BriefInspectionFor: %v", err)
	}
	if result.TaskID != st.ID || result.Revision != st.Revision || result.PaneID != st.PublicPaneID {
		t.Fatalf("inspection = %+v, want the task, its revision and its pane", result)
	}
	if len(result.Decisions) != 2 || result.Terminal == "" {
		t.Errorf("inspection = %+v, want both decisions and the terminal text", result)
	}
	// Another project may not, even with the right task id.
	if _, err := f.coord.BriefInspectionFor(ctx, "E:/somewhere/else", st.ID); !errors.Is(err, ErrNotAdjudicable) {
		t.Errorf("foreign project inspection err = %v, want ErrNotAdjudicable", err)
	}
	if _, err := f.coord.AdjudicateBriefFor(ctx, "E:/somewhere/else", st.ID, st.Revision, BriefDelivered,
		BriefEvidence{PaneID: result.PaneID, Digest: result.Digest}); !errors.Is(err, ErrNotAdjudicable) {
		t.Errorf("foreign project adjudication err = %v, want ErrNotAdjudicable", err)
	}
	// The primary's own decision is recorded and answers with the outcome and the
	// task's new facts.
	decided, err := f.coord.AdjudicateBriefFor(ctx, f.root, st.ID, result.Revision, BriefDelivered,
		BriefEvidence{PaneID: result.PaneID, Digest: result.Digest})
	if err != nil {
		t.Fatalf("AdjudicateBriefFor: %v", err)
	}
	if decided.Decision != string(BriefDelivered) || decided.Task.TaskID != st.ID || decided.Task.Phase != string(task.PromptSubmitted) {
		t.Errorf("adjudication = %+v, want the delivered outcome and the submitted task", decided)
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
