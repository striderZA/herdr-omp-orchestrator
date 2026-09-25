package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This file tests the Herdr client against two kinds of real evidence:
//
//   - Response bytes captured from the installed Herdr 0.9.1-preview CLI
//     (`herdr api schema --json`, `herdr workspace create`, `herdr worktree
//     create/remove`, `herdr pane get/move`, `herdr agent get`, `herdr worktree
//     list`, and the failing `herdr pane get` / `herdr worktree list` forms).
//     Files under testdata/ are sanitized copies of those captures; inline
//     constants below are marked with their capture provenance.
//   - A real child process: TestMain turns this test binary into a stand-in
//     Herdr CLI chosen by HERDR_FIXTURE, so argv construction, output bounding
//     and timeout handling are exercised through exec, never through a mock.

// ---------------------------------------------------------------------------
// fixture child process
// ---------------------------------------------------------------------------

const (
	fixtureEnvScenario   = "HERDR_FIXTURE"
	fixtureEnvStdoutFile = "HERDR_FIXTURE_STDOUT_FILE"
	fixtureEnvStderrFile = "HERDR_FIXTURE_STDERR_FILE"
	fixtureEnvExit       = "HERDR_FIXTURE_EXIT"
	fixtureEnvArgvFile   = "HERDR_FIXTURE_ARGV_FILE"
	fixtureEnvParentFile = "HERDR_FIXTURE_PARENT_FILE"
	fixtureEnvBytes      = "HERDR_FIXTURE_BYTES"
	fixtureEnvStream     = "HERDR_FIXTURE_STREAM"
	fixtureEnvSleepMS    = "HERDR_FIXTURE_SLEEP_MS"
)

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fixtureEnvScenario); scenario != "" {
		os.Exit(runFixture(scenario))
	}
	os.Exit(m.Run())
}

// runFixture behaves like the Herdr CLI would for one invocation: record argv
// and the parent pid for the test to assert on, then emit the configured
// streams and exit code.
func runFixture(scenario string) int {
	argv := append([]string(nil), os.Args[1:]...)
	if path := os.Getenv(fixtureEnvArgvFile); path != "" {
		encoded, err := json.Marshal(argv)
		if err != nil {
			return 11
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return 12
		}
	}
	if path := os.Getenv(fixtureEnvParentFile); path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
			return 13
		}
	}
	switch scenario {
	case "respond":
		for _, s := range []struct {
			env  string
			file *os.File
		}{{fixtureEnvStdoutFile, os.Stdout}, {fixtureEnvStderrFile, os.Stderr}} {
			path := os.Getenv(s.env)
			if path == "" {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return 14
			}
			if _, err := s.file.Write(data); err != nil {
				return 15
			}
		}
		return fixtureExitCode()
	case "big":
		total, err := strconv.Atoi(os.Getenv(fixtureEnvBytes))
		if err != nil || total < 0 {
			return 16
		}
		out := os.Stdout
		if os.Getenv(fixtureEnvStream) == "stderr" {
			out = os.Stderr
		}
		chunk := bytes.Repeat([]byte("x"), 64*1024)
		for written := 0; written < total; {
			n := len(chunk)
			if total-written < n {
				n = total - written
			}
			if _, err := out.Write(chunk[:n]); err != nil {
				return 17
			}
			written += n
		}
		return fixtureExitCode()
	case "hang":
		ms, err := strconv.Atoi(os.Getenv(fixtureEnvSleepMS))
		if err != nil {
			return 18
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return fixtureExitCode()
	default:
		fmt.Fprintf(os.Stderr, "unknown fixture scenario %q\n", scenario)
		return 19
	}
}

func fixtureExitCode() int {
	if raw := os.Getenv(fixtureEnvExit); raw != "" {
		code, err := strconv.Atoi(raw)
		if err == nil {
			return code
		}
	}
	return 0
}

// fixture drives the test binary as a stand-in Herdr CLI.
type fixture struct {
	t          *testing.T
	dir        string
	client     *Client
	argvPath   string
	parentPath string
	stdoutPath string
	stderrPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	client, err := NewClient(exe)
	if err != nil {
		t.Fatalf("NewClient(%q): %v", exe, err)
	}
	f := &fixture{t: t, dir: t.TempDir(), client: client}
	f.argvPath = filepath.Join(f.dir, "argv.json")
	f.parentPath = filepath.Join(f.dir, "parent.txt")
	f.stdoutPath = filepath.Join(f.dir, "stdout.json")
	f.stderrPath = filepath.Join(f.dir, "stderr.json")
	t.Setenv(fixtureEnvArgvFile, f.argvPath)
	t.Setenv(fixtureEnvParentFile, f.parentPath)
	t.Setenv(fixtureEnvScenario, "respond")
	t.Setenv(fixtureEnvExit, "0")
	return f
}

// stdout scripts the child to print payload verbatim on stdout and exit 0.
func (f *fixture) stdout(payload string) *fixture {
	f.t.Helper()
	return f.streams(payload, "", 0)
}

// streams scripts the child's stdout, stderr and exit code.
func (f *fixture) streams(stdout, stderr string, exit int) *fixture {
	f.t.Helper()
	f.t.Setenv(fixtureEnvScenario, "respond")
	f.t.Setenv(fixtureEnvExit, strconv.Itoa(exit))
	f.t.Setenv(fixtureEnvStdoutFile, f.write("stdout", stdout))
	f.t.Setenv(fixtureEnvStderrFile, f.write("stderr", stderr))
	return f
}

func (f *fixture) write(name, content string) string {
	f.t.Helper()
	if content == "" {
		return ""
	}
	path := filepath.Join(f.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// captured responds with the bytes of a sanitized capture from testdata/.
func (f *fixture) captured(name string) *fixture {
	f.t.Helper()
	return f.streams(readFixture(f.t, name), "", 0)
}

// capturedError responds the way the installed CLI does for a failing command:
// the error envelope on stderr, exit status 1.
func (f *fixture) capturedError(name string) *fixture {
	f.t.Helper()
	return f.streams("", readFixture(f.t, name), 1)
}

// hang scripts the child to sleep far longer than any test context.
func (f *fixture) hang() *fixture {
	f.t.Helper()
	f.t.Setenv(fixtureEnvScenario, "hang")
	f.t.Setenv(fixtureEnvSleepMS, "30000")
	f.t.Setenv(fixtureEnvExit, "0")
	return f
}

func (f *fixture) big(stream string, total int) *fixture {
	f.t.Helper()
	f.t.Setenv(fixtureEnvScenario, "big")
	f.t.Setenv(fixtureEnvBytes, strconv.Itoa(total))
	f.t.Setenv(fixtureEnvStream, stream)
	f.t.Setenv(fixtureEnvExit, "0")
	return f
}

// argv returns the argv the child process actually received.
func (f *fixture) argv() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.argvPath)
	if err != nil {
		f.t.Fatalf("child did not record argv (%v); it was probably never started", err)
	}
	var argv []string
	if err := json.Unmarshal(data, &argv); err != nil {
		f.t.Fatalf("argv file %q: %v", data, err)
	}
	return argv
}

// assertDirectChild fails unless this test process was the child's parent, which
// is only true when no shell (cmd.exe, sh -c, ...) sat in between.
func (f *fixture) assertDirectChild() {
	f.t.Helper()
	data, err := os.ReadFile(f.parentPath)
	if err != nil {
		f.t.Fatalf("child did not record its parent pid: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		f.t.Fatalf("parent pid %q: %v", data, err)
	}
	if want := os.Getpid(); got != want {
		f.t.Fatalf("child parent pid = %d, want this test process %d; a shell was involved", got, want)
	}
}

func (f *fixture) assertNotStarted() {
	f.t.Helper()
	if _, err := os.Stat(f.argvPath); err == nil {
		f.t.Fatalf("child started with argv %q; arguments should have been rejected before exec", f.argv())
	} else if !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("stat argv file: %v", err)
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

func shortCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// inline captures (sanitized; provenance in comments)
// ---------------------------------------------------------------------------

const (
	// Sanitized `herdr workspace create --cwd <dir> --label demo --no-focus`.
	rawWorkspaceCreate = `{"id":"cli:workspace:create","result":{"root_pane":{"agent_status":"unknown","cwd":"C:\\work\\demo-repo\\","focused":false,"pane_id":"w2:p1","revision":0,"scroll":{"max_offset_from_bottom":0,"offset_from_bottom":0,"viewport_rows":69},"tab_id":"w2:t1","terminal_id":"term_0000000000000001","workspace_id":"w2"},"tab":{"agent_status":"unknown","focused":false,"label":"1","number":1,"pane_count":1,"tab_id":"w2:t1","workspace_id":"w2"},"type":"workspace_created","workspace":{"active_tab_id":"w2:t1","agent_status":"unknown","focused":false,"label":"demo","number":2,"pane_count":1,"tab_count":1,"workspace_id":"w2"}}}`

	// Sanitized `herdr pane get <alias>` after a cross-workspace move: the
	// launch-time id w2:p1 resolves to the current public id w3:p1 with the same
	// terminal_id. PaneGet must therefore not require the ids to match.
	rawPaneGetAlias = `{"id":"cli:pane:get","result":{"pane":{"agent_status":"unknown","cwd":"C:\\herdr\\worktrees\\demo-repo\\demo-branch\\","focused":false,"pane_id":"w3:p1","revision":0,"scroll":{"max_offset_from_bottom":0,"offset_from_bottom":0,"viewport_rows":69},"tab_id":"w3:t1","terminal_id":"term_0000000000000001","workspace_id":"w3"},"type":"pane_info"}}`

	// Sanitized `herdr pane get wZ:p9` for a pane that does not exist.
	rawPaneNotFound = `{"error":{"code":"pane_not_found","message":"pane wZ:p9 not found"},"id":"cli:pane:get"}`

	// Sanitized `herdr agent get <pane>`.
	rawAgentGet = `{"id":"cli:agent:get","result":{"agent":{"agent":"omp","agent_status":"idle","cwd":"C:\\work\\demo-repo","focused":false,"pane_id":"w1:p2","revision":54042,"state_change_seq":8,"tab_id":"w1:t1","terminal_id":"term_0000000000000002","terminal_title":"omp","terminal_title_stripped":"omp","workspace_id":"w1"},"type":"agent_info"}}`

	// Sanitized and reduced `herdr api snapshot` (one workspace, tab, pane and
	// agent kept so the fixture stays small).
	rawSnapshot = `{"id":"cli:api:snapshot","result":{"snapshot":{"agents":[{"agent":"omp","agent_status":"idle","cwd":"C:\\work\\demo-repo","focused":true,"pane_id":"w1:p2","revision":132751,"state_change_seq":1,"tab_id":"w1:t1","terminal_id":"term_0000000000000002","terminal_title":"omp","terminal_title_stripped":"omp","workspace_id":"w1"}],"focused_pane_id":"w1:p2","focused_tab_id":"w1:t1","focused_workspace_id":"w1","panes":[{"agent":"omp","agent_status":"idle","cwd":"C:\\work\\demo-repo","focused":true,"pane_id":"w1:p2","revision":132751,"tab_id":"w1:t1","terminal_id":"term_0000000000000002","workspace_id":"w1"}],"protocol":22,"tabs":[{"agent_status":"idle","focused":true,"label":"1","number":1,"pane_count":1,"tab_id":"w1:t1","workspace_id":"w1"}],"version":"0.9.1-preview","workspaces":[{"active_tab_id":"w1:t1","agent_status":"idle","focused":true,"label":"demo-repo","number":1,"pane_count":1,"tab_count":1,"workspace_id":"w1"}]},"type":"session_snapshot"}}`

	// Sanitized `herdr worktree list --cwd <repo>`.
	rawWorktreeList = `{"id":"cli:worktree:list","result":{"source":{"repo_key":"\\\\?\\C:\\work\\demo-repo\\.git","repo_name":"demo-repo","repo_root":"C:\\work\\demo-repo","source_checkout_path":"C:\\work\\demo-repo","source_workspace_id":"w1"},"type":"worktree_list","worktrees":[{"branch":"master","is_bare":false,"is_detached":false,"is_linked_worktree":false,"is_prunable":false,"label":"demo-repo","open_workspace_id":"w1","path":"C:/work/demo-repo"},{"branch":"demo-branch","is_bare":false,"is_detached":false,"is_linked_worktree":true,"is_prunable":false,"label":"demo-repo","open_workspace_id":"w2","path":"C:/herdr/worktrees/demo-repo/demo-branch"}]}}`

	// Sanitized `herdr worktree remove --workspace w2 --force`.
	rawWorktreeRemove = `{"id":"cli:worktree:remove","result":{"forced":true,"path":"C:\\herdr\\worktrees\\demo-repo\\demo-branch","type":"worktree_removed","workspace_id":"w2"}}`

	// Documented-not-captured: `agent start`/`agent prompt` were never run
	// against a live agent (that would spend the user's tokens). The shape is
	// the `agent_started` / `agent_prompted` variant of the API schema
	// (`herdr api schema --json`) carrying a real sanitized AgentInfo.
	rawAgentStarted  = `{"id":"cli:agent:start","result":{"agent":{"agent":"omp","agent_status":"idle","cwd":"C:\\work\\demo-repo","focused":false,"name":"orchestrator","pane_id":"w2:p1","revision":1,"state_change_seq":1,"tab_id":"w2:t1","terminal_id":"term_0000000000000001","workspace_id":"w2"},"argv":["omp"],"type":"agent_started"}}`
	rawAgentPrompted = `{"id":"cli:agent:prompt","result":{"agent":{"agent":"omp","agent_status":"working","cwd":"C:\\work\\demo-repo","focused":false,"name":"orchestrator","pane_id":"w2:p1","revision":2,"state_change_seq":2,"tab_id":"w2:t1","terminal_id":"term_0000000000000001","workspace_id":"w2"},"type":"agent_prompted"}}`

	// Sanitized `herdr pane get` usage failure: no JSON anywhere, exit status 2.
	rawUsageFailure = "usage: herdr pane get <pane_id>\n"

	// Documented codes (CLI reference + `herdr agent prompt --help`).
	rawHerdrTimeout  = `{"error":{"code":"timeout","message":"operation timed out"},"id":"cli:agent:prompt"}`
	rawPromptStalled = `{"error":{"code":"agent_prompt_stalled","message":"The agent did not report working or blocked after submission"},"id":"cli:agent:prompt"}`
)

// ---------------------------------------------------------------------------
// Step 1: decoding real responses
// ---------------------------------------------------------------------------

// TestMoveExposesPreviousAndCurrentPublicPaneID feeds the captured pane-move
// response: the move changed the workspace-qualified public id from w2:p1 to
// w3:p1 while the terminal identity stayed the same.
func TestMoveExposesPreviousAndCurrentPublicPaneID(t *testing.T) {
	f := newFixture(t).captured("pane-move.json")
	moved, err := f.client.PaneMove(shortCtx(t, 5*time.Second), "w2:p1", PaneMoveOptions{NewWorkspace: true, Label: "demo-moved"})
	if err != nil {
		t.Fatalf("PaneMove: %v", err)
	}
	if moved.PreviousPaneID != "w2:p1" {
		t.Errorf("PreviousPaneID = %q, want w2:p1 (from .result.move_result.previous_pane_id)", moved.PreviousPaneID)
	}
	if moved.PaneID != "w3:p1" {
		t.Errorf("PaneID = %q, want w3:p1 (from .result.move_result.pane.pane_id)", moved.PaneID)
	}
	if moved.PreviousPaneID == moved.PaneID {
		t.Error("the move must expose two distinct identities, or the caller cannot re-bind only the public one")
	}
	if moved.PreviousWorkspaceID != "w2" || moved.WorkspaceID != "w3" {
		t.Errorf("workspace ids = %q -> %q, want w2 -> w3", moved.PreviousWorkspaceID, moved.WorkspaceID)
	}
	if moved.TerminalID != "term_0000000000000001" {
		t.Errorf("TerminalID = %q, want the unchanged terminal identity", moved.TerminalID)
	}
	// The captured move closed the source workspace (its last pane left), so the
	// client must not drop the closed layout it was told about.
	if moved.ClosedWorkspaceID != "w2" || moved.ClosedTabID != "w2:t1" {
		t.Errorf("closed = %q/%q, want w2/w2:t1 (from .result.move_result.closed_workspace_id/closed_tab_id)",
			moved.ClosedWorkspaceID, moved.ClosedTabID)
	}
	if !moved.Changed {
		t.Error("Changed = false, want true")
	}
}

func TestDecodeWorktreeCreateResponse(t *testing.T) {
	f := newFixture(t).captured("worktree-create.json")
	created, err := f.client.WorktreeCreate(shortCtx(t, 5*time.Second), `C:\work\demo-repo`, "demo-branch", "abc1234")
	if err != nil {
		t.Fatalf("WorktreeCreate: %v", err)
	}
	if created.WorkspaceID != "w2" {
		t.Errorf("WorkspaceID = %q, want w2 (from .result.workspace.workspace_id)", created.WorkspaceID)
	}
	if created.RootPaneID != "w2:p1" {
		t.Errorf("RootPaneID = %q, want w2:p1 (from .result.root_pane.pane_id)", created.RootPaneID)
	}
	if created.TabID != "w2:t1" {
		t.Errorf("TabID = %q, want w2:t1", created.TabID)
	}
	if created.WorktreePath != `C:\herdr\worktrees\demo-repo\demo-branch` {
		t.Errorf("WorktreePath = %q", created.WorktreePath)
	}
	if created.RepoRoot != `C:\work\demo-repo` {
		t.Errorf("RepoRoot = %q", created.RepoRoot)
	}
	if created.Branch != "demo-branch" {
		t.Errorf("Branch = %q, want demo-branch", created.Branch)
	}
	if !created.IsLinkedWorktree {
		t.Error("IsLinkedWorktree = false, want true for a created worktree checkout")
	}
	if raw := f.argv(); len(raw) != 9 || raw[0] != "worktree" || raw[1] != "create" {
		t.Errorf("argv = %q, want worktree create ...", raw)
	}
	f.assertDirectChild()
}

func TestDecodeWorkspaceCreateResponse(t *testing.T) {
	f := newFixture(t).stdout(rawWorkspaceCreate)
	created, err := f.client.WorkspaceCreate(shortCtx(t, 5*time.Second), `C:\work\demo-repo`, "demo")
	if err != nil {
		t.Fatalf("WorkspaceCreate: %v", err)
	}
	if created.WorkspaceID != "w2" {
		t.Errorf("WorkspaceID = %q, want w2 (from .result.workspace.workspace_id)", created.WorkspaceID)
	}
	if created.RootPaneID != "w2:p1" {
		t.Errorf("RootPaneID = %q, want w2:p1 (from .result.root_pane.pane_id)", created.RootPaneID)
	}
}

func TestDecodePaneGetResolvesAliasToCurrentPublicID(t *testing.T) {
	f := newFixture(t).stdout(rawPaneGetAlias)
	pane, err := f.client.PaneGet(shortCtx(t, 5*time.Second), "w2:p1")
	if err != nil {
		t.Fatalf("PaneGet: %v", err)
	}
	if pane.PaneID != "w3:p1" {
		t.Errorf("PaneID = %q, want the current public id w3:p1", pane.PaneID)
	}
	if pane.TerminalID != "term_0000000000000001" {
		t.Errorf("TerminalID = %q, want the unchanged terminal identity", pane.TerminalID)
	}
}

func TestDecodeAgentGetAndSnapshot(t *testing.T) {
	f := newFixture(t).stdout(rawAgentGet)
	agent, err := f.client.AgentGet(shortCtx(t, 5*time.Second), "w1:p2")
	if err != nil {
		t.Fatalf("AgentGet: %v", err)
	}
	if agent.PaneID != "w1:p2" || agent.AgentStatus != "idle" {
		t.Errorf("agent = %+v, want pane w1:p2 idle", agent)
	}

	f.stdout(rawSnapshot)
	snap, err := f.client.Snapshot(shortCtx(t, 5*time.Second))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Version != "0.9.1-preview" || snap.Protocol != 22 {
		t.Errorf("snapshot version/protocol = %q/%d", snap.Version, snap.Protocol)
	}
	if len(snap.Agents) != 1 || snap.Agents[0].PaneID != "w1:p2" {
		t.Errorf("snapshot agents = %+v", snap.Agents)
	}
	if snap.FocusedPaneID != "w1:p2" {
		t.Errorf("FocusedPaneID = %q", snap.FocusedPaneID)
	}
}

func TestDecodeWorktreeListAndRemove(t *testing.T) {
	f := newFixture(t).stdout(rawWorktreeList)
	list, err := f.client.WorktreeList(shortCtx(t, 5*time.Second), `C:\work\demo-repo`)
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}
	if list.Source.RepoRoot != `C:\work\demo-repo` || list.Source.SourceWorkspaceID != "w1" {
		t.Errorf("source = %+v", list.Source)
	}
	if len(list.Worktrees) != 2 {
		t.Fatalf("worktrees = %+v, want 2", list.Worktrees)
	}
	if list.Worktrees[1].Path != "C:/herdr/worktrees/demo-repo/demo-branch" || !list.Worktrees[1].IsLinkedWorktree {
		t.Errorf("worktrees[1] = %+v", list.Worktrees[1])
	}
	if list.Worktrees[0].Branch == nil || *list.Worktrees[0].Branch != "master" {
		t.Errorf("worktrees[0].Branch = %v", list.Worktrees[0].Branch)
	}

	f.stdout(rawWorktreeRemove)
	removed, err := f.client.WorktreeRemove(shortCtx(t, 5*time.Second), "w2", true)
	if err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}
	if removed.WorkspaceID != "w2" || !removed.Forced || removed.Path != `C:\herdr\worktrees\demo-repo\demo-branch` {
		t.Errorf("removed = %+v", removed)
	}
}

func TestDecodeAgentStartAndPrompt(t *testing.T) {
	f := newFixture(t).stdout(rawAgentStarted)
	started, err := f.client.AgentStart(shortCtx(t, 5*time.Second), "orchestrator", "w2:p1")
	if err != nil {
		t.Fatalf("AgentStart: %v", err)
	}
	if started.Agent.PaneID != "w2:p1" || started.Agent.Name != "orchestrator" {
		t.Errorf("started = %+v", started)
	}

	f.stdout(rawAgentPrompted)
	prompted, err := f.client.AgentPrompt(shortCtx(t, 5*time.Second), "w2:p1", "do the thing")
	if err != nil {
		t.Fatalf("AgentPrompt: %v", err)
	}
	if prompted.PaneID != "w2:p1" || prompted.AgentStatus != "working" {
		t.Errorf("prompted = %+v", prompted)
	}
}

func TestDecodeAgentReadReturnsTextVerbatim(t *testing.T) {
	body := "no json here {\n\tand \x1b[31mansi\x1b[0m stays\n"
	f := newFixture(t).streams(body, "", 0)
	got, err := f.client.AgentRead(shortCtx(t, 5*time.Second), "w1:p2", AgentReadOptions{})
	if err != nil {
		t.Fatalf("AgentRead: %v", err)
	}
	if got != body {
		t.Errorf("AgentRead = %q, want the raw terminal text %q", got, body)
	}
}

func TestDecodeAgentSendKeysRejectsUnreadableResult(t *testing.T) {
	f := newFixture(t).stdout(`{"id":"cli:agent:send-keys","result":{"type":"something_new"}}`)
	err := f.client.AgentSendKeys(shortCtx(t, 5*time.Second), "w2:p1", "enter")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("AgentSendKeys err = %v, want ErrMalformedResponse for an unrecognized result type", err)
	}
}

func TestDecodeCallReturnsRawResultMember(t *testing.T) {
	f := newFixture(t).stdout(`{"id":"cli:api:snapshot","result":{"a":1}}`)
	raw, err := f.client.Call(shortCtx(t, 5*time.Second), "api", "snapshot")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if string(raw) != `{"a":1}` {
		t.Errorf("Call = %s, want the result member verbatim", raw)
	}
	if raw := f.argv(); len(raw) != 2 || raw[0] != "api" || raw[1] != "snapshot" {
		t.Errorf("argv = %q, want api snapshot", raw)
	}
}

func TestDecodeUnknownEnvelopeFieldFailsClosed(t *testing.T) {
	f := newFixture(t).stdout(`{"id":"cli:x","result":{"type":"ok"},"unexpected":true}`)
	_, err := f.client.Call(shortCtx(t, 5*time.Second), "x")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("Call err = %v, want ErrMalformedResponse for an unknown envelope field", err)
	}
}

func TestDecodeUnknownResultTypeFailsClosed(t *testing.T) {
	f := newFixture(t).stdout(`{"id":"cli:pane:get","result":{"type":"pane_list","panes":[]}}`)
	_, err := f.client.PaneGet(shortCtx(t, 5*time.Second), "w1:p2")
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("PaneGet err = %v, want ErrMalformedResponse for result type pane_list", err)
	}
}

func TestDecodeAbsentRequiredIDFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		call    func(context.Context, *Client) error
	}{
		{
			name:    "workspace id absent",
			payload: `{"id":"cli:workspace:create","result":{"root_pane":{"pane_id":"w2:p1","terminal_id":"t","workspace_id":"w2","tab_id":"w2:t1"},"tab":{"tab_id":"w2:t1","workspace_id":"w2"},"type":"workspace_created","workspace":{"active_tab_id":"w2:t1","workspace_id":""}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, `C:\work\demo-repo`, "demo")
				return err
			},
		},
		{
			name:    "root pane id absent",
			payload: `{"id":"cli:workspace:create","result":{"root_pane":{"pane_id":"","terminal_id":"t","workspace_id":"w2","tab_id":"w2:t1"},"tab":{"tab_id":"w2:t1","workspace_id":"w2"},"type":"workspace_created","workspace":{"active_tab_id":"w2:t1","workspace_id":"w2"}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, `C:\work\demo-repo`, "demo")
				return err
			},
		},
		{
			name:    "worktree path absent",
			payload: `{"id":"cli:worktree:create","result":{"root_pane":{"pane_id":"w2:p1","terminal_id":"t","workspace_id":"w2","tab_id":"w2:t1"},"tab":{"tab_id":"w2:t1","workspace_id":"w2"},"type":"worktree_created","workspace":{"active_tab_id":"w2:t1","workspace_id":"w2","worktree":{"checkout_path":"C:\\herdr\\wt","repo_root":"C:\\work\\demo-repo"}},"worktree":{"path":"","is_linked_worktree":true}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeCreate(ctx, `C:\work\demo-repo`, "demo-branch", "abc1234")
				return err
			},
		},
		{
			name:    "moved pane id absent",
			payload: `{"id":"cli:pane:move","result":{"move_result":{"changed":true,"pane":{"pane_id":"","terminal_id":"t","workspace_id":"w3","tab_id":"w3:t1"},"previous_pane_id":"w2:p1","previous_tab_id":"w2:t1","previous_workspace_id":"w2"},"type":"pane_move"}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneMove(ctx, "w2:p1", PaneMoveOptions{NewWorkspace: true})
				return err
			},
		},
		{
			name:    "moved pane previous id absent",
			payload: `{"id":"cli:pane:move","result":{"move_result":{"changed":true,"pane":{"pane_id":"w3:p1","terminal_id":"t","workspace_id":"w3","tab_id":"w3:t1"},"previous_pane_id":"","previous_tab_id":"w2:t1","previous_workspace_id":"w2"},"type":"pane_move"}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneMove(ctx, "w2:p1", PaneMoveOptions{NewWorkspace: true})
				return err
			},
		},
		{
			name:    "agent pane id absent",
			payload: `{"id":"cli:agent:get","result":{"agent":{"agent":"omp","agent_status":"idle","pane_id":"","terminal_id":"t","workspace_id":"w1","tab_id":"w1:t1"},"type":"agent_info"}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentGet(ctx, "w1:p2")
				return err
			},
		},
		{
			name:    "snapshot agent pane id absent",
			payload: `{"id":"cli:api:snapshot","result":{"snapshot":{"agents":[{"agent_status":"idle","pane_id":"","terminal_id":"t","workspace_id":"w1","tab_id":"w1:t1"}],"panes":[],"protocol":22,"tabs":[],"version":"0.9.1-preview","workspaces":[]},"type":"session_snapshot"}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Snapshot(ctx)
				return err
			},
		},
		{
			name:    "worktree remove workspace id absent",
			payload: `{"id":"cli:worktree:remove","result":{"forced":false,"path":"C:\\herdr\\wt","type":"worktree_removed","workspace_id":""}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeRemove(ctx, "w2", false)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t).stdout(tc.payload)
			err := tc.call(shortCtx(t, 5*time.Second), f.client)
			if !errors.Is(err, ErrIdentity) {
				t.Fatalf("err = %v, want ErrIdentity", err)
			}
			var identity *IdentityError
			if !errors.As(err, &identity) {
				t.Fatalf("err = %v, want *IdentityError", err)
			}
			if identity.Field == "" {
				t.Errorf("IdentityError.Field is empty: %+v", identity)
			}
		})
	}
}

func TestDecodeIdentityMismatchFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		call    func(context.Context, *Client) error
	}{
		{
			name:    "root pane in another workspace",
			payload: `{"id":"cli:workspace:create","result":{"root_pane":{"pane_id":"w9:p1","terminal_id":"t","workspace_id":"w9","tab_id":"w9:t1"},"tab":{"tab_id":"w9:t1","workspace_id":"w9"},"type":"workspace_created","workspace":{"active_tab_id":"w9:t1","workspace_id":"w2"}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, `C:\work\demo-repo`, "demo")
				return err
			},
		},
		{
			name:    "worktree opened in an unrelated workspace",
			payload: `{"id":"cli:worktree:create","result":{"root_pane":{"pane_id":"w2:p1","terminal_id":"t","workspace_id":"w2","tab_id":"w2:t1"},"tab":{"tab_id":"w2:t1","workspace_id":"w2"},"type":"worktree_created","workspace":{"active_tab_id":"w2:t1","workspace_id":"w2","worktree":{"checkout_path":"C:\\herdr\\wt","repo_root":"C:\\work\\demo-repo"}},"worktree":{"path":"C:\\herdr\\wt","is_linked_worktree":true,"open_workspace_id":"w9"}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeCreate(ctx, `C:\work\demo-repo`, "demo-branch", "abc1234")
				return err
			},
		},
		{
			name:    "agent started on an unexpected pane",
			payload: `{"id":"cli:agent:start","result":{"agent":{"agent":"omp","agent_status":"idle","name":"orchestrator","pane_id":"w9:p1","terminal_id":"t","workspace_id":"w9","tab_id":"w9:t1"},"argv":["omp"],"type":"agent_started"}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentStart(ctx, "orchestrator", "w2:p1")
				return err
			},
		},
		{
			name:    "worktree remove answered for another workspace",
			payload: `{"id":"cli:worktree:remove","result":{"forced":false,"path":"C:\\herdr\\wt","type":"worktree_removed","workspace_id":"w9"}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeRemove(ctx, "w2", false)
				return err
			},
		},
		{
			name:    "worktree create answered with the primary checkout",
			payload: `{"id":"cli:worktree:create","result":{"root_pane":{"pane_id":"w2:p1","terminal_id":"t","workspace_id":"w2","tab_id":"w2:t1"},"tab":{"tab_id":"w2:t1","workspace_id":"w2"},"type":"worktree_created","workspace":{"active_tab_id":"w2:t1","workspace_id":"w2","worktree":{"checkout_path":"C:\\work\\demo-repo","repo_root":"C:\\work\\demo-repo"}},"worktree":{"path":"C:\\work\\demo-repo","is_linked_worktree":false,"open_workspace_id":"w2"}}}`,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeCreate(ctx, `C:\work\demo-repo`, "demo-branch", "abc1234")
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t).stdout(tc.payload)
			err := tc.call(shortCtx(t, 5*time.Second), f.client)
			var identity *IdentityError
			if !errors.As(err, &identity) {
				t.Fatalf("err = %v, want *IdentityError", err)
			}
		})
	}
}

func TestDecodeContradictoryEnvelopeFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		out  string
		exit int
		err  string
	}{
		{"result and error together", `{"id":"x","result":{"type":"ok"},"error":{"code":"boom","message":"bad"}}`, 0, "both"},
		{"neither result nor error", `{"id":"x"}`, 0, "neither"},
		{"null result", `{"id":"x","result":null}`, 0, "neither"},
		{"error without message", `{"id":"x","error":{"code":"boom"}}`, 1, "missing code or message"},
		{"error without code", `{"id":"x","error":{"message":"bad"}}`, 1, "missing code or message"},
		{"result is not an object", `{"id":"x","result":[1,2]}`, 0, "not a JSON object"},
		{"malformed json", `{"id":"x","result":`, 0, ""},
		{"two envelopes", `{"id":"x","result":{"type":"ok"}}{"id":"y","result":{"type":"ok"}}`, 0, "trailing"},
		{"garbage after envelope", `{"id":"x","result":{"type":"ok"}}PASS`, 0, "trailing"},
		{"empty stdout", ``, 0, "empty"},
		{"whitespace stdout", "  \n\t ", 0, "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t).streams(tc.out, "", tc.exit)
			_, err := f.client.Call(shortCtx(t, 5*time.Second), "x")
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("Call err = %v, want ErrMalformedResponse", err)
			}
			if tc.err != "" && !strings.Contains(err.Error(), tc.err) {
				t.Errorf("err = %v, want it to mention %q", err, tc.err)
			}
		})
	}
}

func TestDecodeBoundedOutputFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		total  int
	}{
		{"stdout just over the limit", "stdout", MaxOutputBytes + 1},
		{"stdout far over the limit", "stdout", 4 * MaxOutputBytes},
		{"stderr over the limit", "stderr", MaxOutputBytes + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t).big(tc.stream, tc.total)
			_, err := f.client.Call(context.Background(), "api", "snapshot")
			if !errors.Is(err, ErrOutputTooLarge) {
				t.Fatalf("Call err = %v, want ErrOutputTooLarge", err)
			}
			var limit *OutputLimitError
			if !errors.As(err, &limit) {
				t.Fatalf("err = %v, want *OutputLimitError", err)
			}
			if limit.Limit != MaxOutputBytes || limit.Stream != tc.stream {
				t.Errorf("limit = %+v, want stream %q limit %d", limit, tc.stream, MaxOutputBytes)
			}
		})
	}
}

func TestDecodeOutputAtTheLimitIsAccepted(t *testing.T) {
	// The bound is a comparison against MaxOutputBytes, so the boundary case is
	// equality: build a valid envelope whose stdout is exactly MaxOutputBytes
	// long (a shorter payload would assert nothing about the comparison).
	const prefix = `{"id":"x","result":{"type":"ok","pad":"`
	const suffix = `"}}`
	padLen := MaxOutputBytes - len(prefix) - len(suffix)
	if padLen <= 0 {
		t.Fatalf("MaxOutputBytes (%d) is too small to build an at-the-limit envelope", MaxOutputBytes)
	}
	payload := prefix + strings.Repeat("p", padLen) + suffix
	if len(payload) != MaxOutputBytes {
		t.Fatalf("fixture is %d bytes, want exactly MaxOutputBytes (%d)", len(payload), MaxOutputBytes)
	}
	f := newFixture(t).streams(payload, "", 0)
	raw, err := f.client.Call(shortCtx(t, 5*time.Second), "x")
	if err != nil {
		t.Fatalf("Call with exactly MaxOutputBytes on stdout: %v", err)
	}
	var res struct {
		Type string `json:"type"`
		Pad  string `json:"pad"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode result at exactly MaxOutputBytes: %v", err)
	}
	if res.Type != "ok" || len(res.Pad) != padLen {
		t.Errorf("result = type %q, pad %d bytes; want type \"ok\" and %d bytes", res.Type, len(res.Pad), padLen)
	}
}

// ---------------------------------------------------------------------------
// Step 1: error classification
// ---------------------------------------------------------------------------

func TestCLIErrorAgentBlockedIsDefiniteAndStaysACLIError(t *testing.T) {
	// Provenance: this envelope is documented, not captured. `agent prompt` was
	// never run against a live agent (that would spend the user's tokens), so
	// testdata/cli-error-agent-blocked.json is the CLI reference's literal
	// agent_blocked envelope. The capturedError helper only models where the
	// installed CLI writes an error envelope (stderr, exit status 1) — it does
	// not vouch for the provenance of the bytes. The file header's "captured"
	// claim applies to the other fixtures in testdata/, not to this one.
	f := newFixture(t).capturedError("cli-error-agent-blocked.json")
	_, err := f.client.AgentPrompt(shortCtx(t, 5*time.Second), "w2:p1", "do the thing")
	if err == nil {
		t.Fatal("AgentPrompt succeeded, want agent_blocked")
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v (%T), want *CLIError", err, err)
	}
	if cliErr.Code != "agent_blocked" {
		t.Errorf("Code = %q, want agent_blocked", cliErr.Code)
	}
	if cliErr.Message != "The agent requires an approval choice" {
		t.Errorf("Message = %q", cliErr.Message)
	}
	if cliErr.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", cliErr.ExitCode)
	}
	if !errors.Is(err, ErrCLI) {
		t.Error("errors.Is(err, ErrCLI) = false")
	}
	if !cliErr.IsCode("agent_blocked") {
		t.Error("IsCode(agent_blocked) = false")
	}
	if cliErr.Stderr == "" {
		t.Error("CLIError.Stderr is empty; the raw stream must be preserved for diagnosis")
	}
	// No input is sent to a blocked agent, so this is a definite outcome.
	var ambiguous *AmbiguousError
	if errors.As(err, &ambiguous) {
		t.Fatalf("agent_blocked classified as ambiguous: %v", err)
	}
}

func TestCLIErrorObservedEnvelopeDecodes(t *testing.T) {
	f := newFixture(t).capturedError("cli-error.json")
	_, err := f.client.WorktreeList(shortCtx(t, 5*time.Second), `C:\work\demo-repo`)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v, want *CLIError", err)
	}
	if cliErr.Code != "not_git_worktree" {
		t.Errorf("Code = %q, want not_git_worktree", cliErr.Code)
	}
	if cliErr.Op == "" || len(cliErr.Args) == 0 {
		t.Errorf("op/args not preserved: %+v", cliErr)
	}
}

func TestCLIErrorEnvelopeOnStdoutIsDecoded(t *testing.T) {
	f := newFixture(t).streams(rawPaneNotFound, "", 1)
	_, err := f.client.PaneGet(shortCtx(t, 5*time.Second), "wZ:p9")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v, want *CLIError", err)
	}
	if cliErr.Code != "pane_not_found" {
		t.Errorf("Code = %q, want pane_not_found", cliErr.Code)
	}
}

func TestCLIErrorWithoutEnvelopePreservesExitCodeAndStderr(t *testing.T) {
	f := newFixture(t).streams("", rawUsageFailure, 2)
	_, err := f.client.PaneGet(shortCtx(t, 5*time.Second), "wZ:p9")
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v, want *CLIError", err)
	}
	if cliErr.Code != "" {
		t.Errorf("Code = %q, want empty when the CLI printed no envelope", cliErr.Code)
	}
	if cliErr.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", cliErr.ExitCode)
	}
	if !strings.Contains(cliErr.Stderr, "usage: herdr pane get") {
		t.Errorf("Stderr = %q, want the raw CLI usage text", cliErr.Stderr)
	}
	if !errors.Is(err, ErrCLI) {
		t.Error("errors.Is(err, ErrCLI) = false")
	}
}

func TestCLITimeoutAfterSubmissionIsAmbiguous(t *testing.T) {
	f := newFixture(t).hang()
	_, err := f.client.AgentPrompt(shortCtx(t, 300*time.Millisecond), "w2:p1", "long brief")
	if err == nil {
		t.Fatal("AgentPrompt succeeded, want a timeout")
	}
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v (%T), want *AmbiguousError", err, err)
	}
	if !errors.Is(err, ErrAmbiguous) {
		t.Error("errors.Is(err, ErrAmbiguous) = false")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if ambiguous.PaneID != "w2:p1" {
		t.Errorf("PaneID = %q, want w2:p1", ambiguous.PaneID)
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) {
		t.Errorf("a killed process is not a Herdr CLI error: %v", err)
	}
}

func TestCLITimeoutCodeFromHerdrIsAmbiguous(t *testing.T) {
	f := newFixture(t).streams("", rawHerdrTimeout, 1)
	_, err := f.client.AgentPrompt(shortCtx(t, 5*time.Second), "w2:p1", "do the thing")
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v (%T), want *AmbiguousError", err, err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v, want the underlying *CLIError to stay reachable", err)
	}
	if cliErr.Code != CodeTimeout {
		t.Errorf("Code = %q, want %q", cliErr.Code, CodeTimeout)
	}
	if ambiguous.Code() != CodeTimeout {
		t.Errorf("ambiguous.Code() = %q, want %q", ambiguous.Code(), CodeTimeout)
	}
	if !strings.Contains(ambiguous.Error(), "inspect") {
		t.Errorf("message %q should tell the caller to inspect state instead of resubmitting", ambiguous.Error())
	}
}

func TestCLIPromptStalledIsAmbiguousAndKeepsItsCode(t *testing.T) {
	f := newFixture(t).streams("", rawPromptStalled, 1)
	_, err := f.client.AgentPrompt(shortCtx(t, 5*time.Second), "w2:p1", "do the thing")
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v (%T), want *AmbiguousError", err, err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("err = %v, want the underlying *CLIError", err)
	}
	if cliErr.Code != CodeAgentPromptStalled {
		t.Errorf("Code = %q, want %q", cliErr.Code, CodeAgentPromptStalled)
	}
}

func TestCLITimeoutOutsideSubmissionIsNotAmbiguous(t *testing.T) {
	f := newFixture(t).hang()
	_, err := f.client.AgentGet(shortCtx(t, 300*time.Millisecond), "w2:p1")
	if err == nil {
		t.Fatal("AgentGet succeeded, want a timeout")
	}
	var ambiguous *AmbiguousError
	if errors.As(err, &ambiguous) {
		t.Fatalf("err = %v, want a plain timeout for a read-only command", err)
	}
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrTimeout wrapping context.DeadlineExceeded", err)
	}
	var timeout *TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want *TimeoutError", err)
	}
	if len(timeout.Args) == 0 {
		t.Error("TimeoutError.Args is empty; argv must be preserved for diagnosis")
	}
}

func TestCLIWorktreeCreateTimeoutIsAmbiguous(t *testing.T) {
	f := newFixture(t).hang()
	_, err := f.client.WorktreeCreate(shortCtx(t, 300*time.Millisecond), `C:\work\demo-repo`, "demo-branch", "abc1234")
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %v (%T), want *AmbiguousError: the checkout may exist", err, err)
	}
}

// TestCLIUninterpretableResponseOnMutatingCommandIsAmbiguous pins the invariant
// that failing to interpret a response is not proof that nothing happened. For
// a command whose side effect may already have been applied — the brief was
// written into the pane, the checkout was created — the failure must be
// ambiguous, because a coordinator that retries definite failures would
// otherwise resubmit a delivered brief or create a second checkout. The
// underlying decoding failure stays reachable through errors.As.
func TestCLIUninterpretableResponseOnMutatingCommandIsAmbiguous(t *testing.T) {
	cases := []struct {
		name     string
		payload  string // child answers with this on stdout (when non-empty)
		oversize bool   // child writes more than MaxOutputBytes on stdout instead
		want     error  // sentinel the cause must still satisfy errors.Is for
		wantType func(error) bool
		call     func(context.Context, *Client) error
	}{
		{
			name:    "agent prompt with an unrecognized result type",
			payload: `{"id":"cli:agent:prompt","result":{"type":"agent_prompted_v2","agent":{}}}`,
			want:    ErrMalformedResponse,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentPrompt(ctx, "w2:p1", "do the thing")
				return err
			},
		},
		{
			name:    "agent prompt with a contradictory envelope",
			payload: `{"id":"cli:agent:prompt","result":null}`,
			want:    ErrMalformedResponse,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentPrompt(ctx, "w2:p1", "do the thing")
				return err
			},
		},
		{
			name:     "agent prompt with an oversized stream",
			oversize: true,
			want:     ErrOutputTooLarge,
			wantType: func(err error) bool {
				var limit *OutputLimitError
				return errors.As(err, &limit)
			},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentPrompt(ctx, "w2:p1", "do the thing")
				return err
			},
		},
		{
			name:    "agent send-keys with an unrecognized result type",
			payload: `{"id":"cli:agent:send-keys","result":{"type":"something_new"}}`,
			want:    ErrMalformedResponse,
			call: func(ctx context.Context, c *Client) error {
				return c.AgentSendKeys(ctx, "w2:p1", "enter")
			},
		},
		{
			name:     "agent send-keys with an oversized stream",
			oversize: true,
			want:     ErrOutputTooLarge,
			wantType: func(err error) bool {
				var limit *OutputLimitError
				return errors.As(err, &limit)
			},
			call: func(ctx context.Context, c *Client) error {
				return c.AgentSendKeys(ctx, "w2:p1", "enter")
			},
		},
		{
			name:    "worktree create with an absent required identity",
			payload: `{"id":"cli:worktree:create","result":{"type":"worktree_created"}}`,
			want:    ErrIdentity,
			wantType: func(err error) bool {
				var identity *IdentityError
				return errors.As(err, &identity)
			},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeCreate(ctx, `C:\work\demo-repo`, "demo-branch", "abc1234")
				return err
			},
		},
		{
			name:    "workspace create with an absent required identity",
			payload: `{"id":"cli:workspace:create","result":{"type":"workspace_created"}}`,
			want:    ErrIdentity,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, `C:\work\demo-repo`, "demo")
				return err
			},
		},
		{
			name:    "worktree remove with an absent required identity",
			payload: `{"id":"cli:worktree:remove","result":{"forced":false,"path":"C:\\herdr\\wt","type":"worktree_removed","workspace_id":""}}`,
			want:    ErrIdentity,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeRemove(ctx, "w2", false)
				return err
			},
		},
		{
			name:    "agent start with an unrecognized result type",
			payload: `{"id":"cli:agent:start","result":{"type":"agent_started_v2","agent":{}}}`,
			want:    ErrMalformedResponse,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentStart(ctx, "orchestrator", "w2:p1")
				return err
			},
		},
		{
			name:    "pane move with an absent required identity",
			payload: `{"id":"cli:pane:move","result":{"type":"pane_move","move_result":{"changed":false}}}`,
			want:    ErrIdentity,
			wantType: func(err error) bool {
				var identity *IdentityError
				return errors.As(err, &identity)
			},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneMove(ctx, "w2:p1", PaneMoveOptions{NewWorkspace: true})
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.oversize {
				f.big("stdout", MaxOutputBytes+1)
			} else {
				f.stdout(tc.payload)
			}
			err := tc.call(shortCtx(t, 30*time.Second), f.client)
			var ambiguous *AmbiguousError
			if !errors.As(err, &ambiguous) {
				t.Fatalf("err = %v (%T), want *AmbiguousError: the effect may already have happened", err, err)
			}
			if !errors.Is(err, ErrAmbiguous) {
				t.Errorf("errors.Is(err, ErrAmbiguous) = false for %v", err)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want the cause to stay reachable through errors.Is(err, %v)", err, tc.want)
			}
			if tc.wantType != nil && !tc.wantType(err) {
				t.Errorf("err = %v, want the concrete cause to stay reachable through errors.As", err)
			}
			if !strings.Contains(ambiguous.Error(), "inspect") {
				t.Errorf("message %q should tell the caller to inspect state instead of retrying", ambiguous.Error())
			}
			if strings.Contains(ambiguous.Error(), "timeout") {
				t.Errorf("message %q describes a timeout that never happened; the response could not be interpreted", ambiguous.Error())
			}
		})
	}
}

// TestCLIUninterpretableResponseOnReadOnlyCommandStaysDefinite is the other half
// of the same invariant: a command that changes nothing reports an
// uninterpretable response as the definite decoding failure it is, so callers
// do not inspect state over a read.
func TestCLIUninterpretableResponseOnReadOnlyCommandStaysDefinite(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		oversize bool
		want     error
		call     func(context.Context, *Client) error
	}{
		{
			name:    "pane get with an unrecognized result type",
			payload: `{"id":"cli:pane:get","result":{"type":"pane_list","panes":[]}}`,
			want:    ErrMalformedResponse,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneGet(ctx, "w1:p2")
				return err
			},
		},
		{
			name:    "pane get with a contradictory envelope",
			payload: `{"id":"cli:pane:get","result":null}`,
			want:    ErrMalformedResponse,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneGet(ctx, "w1:p2")
				return err
			},
		},
		{
			name:    "agent get with an absent required identity",
			payload: `{"id":"cli:agent:get","result":{"agent":{"agent_status":"idle","pane_id":"","terminal_id":"t","workspace_id":"w1","tab_id":"w1:t1"},"type":"agent_info"}}`,
			want:    ErrIdentity,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentGet(ctx, "w1:p2")
				return err
			},
		},
		{
			name:    "snapshot with an unrecognized result type",
			payload: `{"id":"cli:api:snapshot","result":{"snapshot":{},"type":"session_snapshot_v2"}}`,
			want:    ErrMalformedResponse,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Snapshot(ctx)
				return err
			},
		},
		{
			name:    "worktree list with an absent source root",
			payload: `{"id":"cli:worktree:list","result":{"source":{},"type":"worktree_list","worktrees":[]}}`,
			want:    ErrIdentity,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeList(ctx, `C:\work\demo-repo`)
				return err
			},
		},
		{
			name:     "worktree list with an oversized stream",
			oversize: true,
			want:     ErrOutputTooLarge,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeList(ctx, `C:\work\demo-repo`)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.oversize {
				f.big("stdout", MaxOutputBytes+1)
			} else {
				f.stdout(tc.payload)
			}
			err := tc.call(shortCtx(t, 30*time.Second), f.client)
			if err == nil {
				t.Fatal("call succeeded, want a definite decoding failure")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var ambiguous *AmbiguousError
			if errors.As(err, &ambiguous) {
				t.Fatalf("err = %v, want a definite failure: a read-only command cannot have changed anything", err)
			}
		})
	}
}

func TestCLIArgumentValidationHappensBeforeExec(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{"empty agent name", func(ctx context.Context, c *Client) error {
			_, err := c.AgentStart(ctx, "", "w2:p1")
			return err
		}},
		{"illegal agent name", func(ctx context.Context, c *Client) error {
			_, err := c.AgentStart(ctx, "Orchestrator 1", "w2:p1")
			return err
		}},
		{"agent name too long", func(ctx context.Context, c *Client) error {
			_, err := c.AgentStart(ctx, "a"+strings.Repeat("b", 32), "w2:p1")
			return err
		}},
		{"empty pane id", func(ctx context.Context, c *Client) error {
			_, err := c.AgentPrompt(ctx, "  ", "brief")
			return err
		}},
		{"empty brief", func(ctx context.Context, c *Client) error {
			_, err := c.AgentPrompt(ctx, "w2:p1", "")
			return err
		}},
		{"no keys", func(ctx context.Context, c *Client) error {
			return c.AgentSendKeys(ctx, "w2:p1")
		}},
		{"empty cwd", func(ctx context.Context, c *Client) error {
			_, err := c.WorkspaceCreate(ctx, "", "demo")
			return err
		}},
		{"missing base revision", func(ctx context.Context, c *Client) error {
			_, err := c.WorktreeCreate(ctx, `C:\work\demo-repo`, "demo-branch", "")
			return err
		}},
		{"conflicting pane move targets", func(ctx context.Context, c *Client) error {
			_, err := c.PaneMove(ctx, "w2:p1", PaneMoveOptions{NewWorkspace: true, NewTab: true})
			return err
		}},
		{"unknown read source", func(ctx context.Context, c *Client) error {
			_, err := c.AgentRead(ctx, "w2:p1", AgentReadOptions{Source: "everything"})
			return err
		}},
		{"negative read lines", func(ctx context.Context, c *Client) error {
			_, err := c.AgentRead(ctx, "w2:p1", AgentReadOptions{Lines: -1})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(shortCtx(t, 5*time.Second), f.client)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
			f.assertNotStarted()
		})
	}
}

func TestCLIRejectsEmptyBinary(t *testing.T) {
	if _, err := NewClient("   "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("NewClient(\"   \") err = %v, want ErrInvalidArgument", err)
	}
}

// ---------------------------------------------------------------------------
// Step 1: argv stays discrete
// ---------------------------------------------------------------------------

func TestArgvWindowsPathsWithSpacesStayDiscrete(t *testing.T) {
	root := `C:\work\my repo & tools\100% done`
	branch := `feature/quoted "branch"`
	base := "abc1234def5678"
	cases := []struct {
		name    string
		diff    string
		payload string
		call    func(context.Context, *Client) error
		want    []string
	}{
		{
			name:    "worktree create",
			payload: readFixture(t, "worktree-create.json"),
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeCreate(ctx, root, branch, base)
				return err
			},
			want: []string{"worktree", "create", "--cwd", root, "--branch", branch, "--base", base, "--no-focus"},
		},
		{
			name:    "workspace create",
			payload: rawWorkspaceCreate,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorkspaceCreate(ctx, root, `demo "primary" & co`)
				return err
			},
			want: []string{"workspace", "create", "--cwd", root, "--label", `demo "primary" & co`, "--no-focus"},
		},
		{
			name:    "worktree list",
			payload: rawWorktreeList,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeList(ctx, root)
				return err
			},
			want: []string{"worktree", "list", "--cwd", root},
		},
		{
			name:    "worktree remove",
			payload: rawWorktreeRemove,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.WorktreeRemove(ctx, "w2", true)
				return err
			},
			want: []string{"worktree", "remove", "--workspace", "w2", "--force"},
		},
		{
			name:    "pane move to a new workspace",
			payload: readFixture(t, "pane-move.json"),
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneMove(ctx, "w2:p1", PaneMoveOptions{NewWorkspace: true, Label: `demo "moved" & more`})
				return err
			},
			want: []string{"pane", "move", "w2:p1", "--new-workspace", "--label", `demo "moved" & more`, "--no-focus"},
		},
		{
			name:    "pane move into a tab",
			payload: readFixture(t, "pane-move.json"),
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneMove(ctx, "w2:p1", PaneMoveOptions{TabID: "w2:t1", Split: "right", TargetPaneID: "w2:p2"})
				return err
			},
			want: []string{"pane", "move", "w2:p1", "--tab", "w2:t1", "--split", "right", "--target-pane", "w2:p2", "--no-focus"},
		},
		{
			name:    "pane get",
			payload: rawPaneGetAlias,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.PaneGet(ctx, root)
				return err
			},
			want: []string{"pane", "get", root},
		},
		{
			name:    "agent start",
			payload: rawAgentStarted,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentStart(ctx, "orchestrator", "w2:p1")
				return err
			},
			want: []string{"agent", "start", "orchestrator", "--kind", "omp", "--pane", "w2:p1"},
		},
		{
			name:    "agent get",
			payload: rawAgentGet,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentGet(ctx, "orchestrator")
				return err
			},
			want: []string{"agent", "get", "orchestrator"},
		},
		{
			name:    "agent read",
			payload: "some terminal text\n",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.AgentRead(ctx, "w2:p1", AgentReadOptions{Source: "recent-unwrapped", Lines: 120, ANSI: true})
				return err
			},
			want: []string{"agent", "read", "w2:p1", "--source", "recent-unwrapped", "--lines", "120", "--ansi"},
		},
		{
			name:    "agent send-keys",
			payload: `{"id":"cli:agent:send-keys","result":{"type":"ok"}}`,
			call: func(ctx context.Context, c *Client) error {
				return c.AgentSendKeys(ctx, "w2:p1", "ctrl+c", "enter")
			},
			want: []string{"agent", "send-keys", "w2:p1", "ctrl+c", "enter"},
		},
		{
			name:    "snapshot",
			payload: rawSnapshot,
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Snapshot(ctx)
				return err
			},
			want: []string{"api", "snapshot"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t).streams(tc.payload, "", 0)
			if err := tc.call(shortCtx(t, 5*time.Second), f.client); err != nil {
				t.Fatalf("call: %v", err)
			}
			got := f.argv()
			if len(got) != len(tc.want) {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
			for j := range got {
				if got[j] != tc.want[j] {
					t.Fatalf("argv[%d] = %q, want %q (full argv %q)", j, got[j], tc.want[j], got)
				}
			}
			f.assertDirectChild()
		})
	}
}

func TestArgvPromptWithQuotesAndNewlinesStaysASingleArgument(t *testing.T) {
	brief := "Plan:\n\tsay \"hello\" & echo %PATH%\n\tthen `run` $(this)\n\ttrailing backslash \\\n"
	f := newFixture(t).stdout(rawAgentPrompted)
	prompted, err := f.client.AgentPrompt(shortCtx(t, 5*time.Second), `w2:p1`, brief)
	if err != nil {
		t.Fatalf("AgentPrompt: %v", err)
	}
	if prompted.PaneID != "w2:p1" {
		t.Errorf("prompted pane = %q", prompted.PaneID)
	}
	got := f.argv()
	want := []string{"agent", "prompt", "w2:p1", brief}
	if len(got) != len(want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	for j := range got {
		if got[j] != want[j] {
			t.Fatalf("argv[%d] = %q, want %q", j, got[j], want[j])
		}
	}
	f.assertDirectChild()
}

func TestArgvNeverInvokesAShell(t *testing.T) {
	// A path that a shell would mangle (metacharacters, %-expansion, quotes,
	// embedded newline) must reach the process byte for byte.
	cwd := "C:\\work\\a & b\\%TEMP%\\\"quoted\"\nnewline"
	f := newFixture(t).stdout(rawWorkspaceCreate)
	if _, err := f.client.WorkspaceCreate(shortCtx(t, 5*time.Second), cwd, "demo"); err != nil {
		t.Fatalf("WorkspaceCreate: %v", err)
	}
	want := []string{"workspace", "create", "--cwd", cwd, "--label", "demo", "--no-focus"}
	got := f.argv()
	if !equalStrings(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	f.assertDirectChild()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
