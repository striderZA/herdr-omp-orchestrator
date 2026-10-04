package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"

	_ "modernc.org/sqlite"
)

// This file tests the bridge protocol against real durable facts (a real
// SQLite store in a temporary plugin state directory) and a real Herdr CLI
// child process: TestMain turns this test binary into a stand-in Herdr that
// answers `pane get` from a fixture file, so the restore path really crosses an
// exec boundary. One test additionally builds and runs the real
// cmd/herdr-omp-orchestrator binary so the argv/stdin/stdout contract the OMP
// extension depends on is exercised end to end.

const (
	fixturePaneFile = "BRIDGE_FIXTURE_PANE_FILE"
	fixtureErrFile  = "BRIDGE_FIXTURE_ERROR_FILE"
	fixtureArgvFile = "BRIDGE_FIXTURE_ARGV_FILE"
	// fixtureScriptFile selects the scripted stand-in the Task 5 tests use: it
	// answers every command a report, relay, approval or cleanup makes, rather
	// than only `pane get`. fixtureCallsFile appends every argv it was invoked
	// with, so a test can assert the exact commands an operation made.
	fixtureScriptFile = "BRIDGE_FIXTURE_SCRIPT_FILE"
	fixtureCallsFile  = "BRIDGE_FIXTURE_CALLS_FILE"
	// fixtureStateDir is the plugin state directory as the stand-in Herdr child can
	// see it. A child inherits the test process's environment, not the bridge's own
	// env map, so a scripted answer that has to write the store (a concurrent move)
	// is told where that store is through the process environment.
	fixtureStateDir = "BRIDGE_FIXTURE_STATE_DIR"
)

var (
	cmdBinaryDir string
	buildOnce    sync.Once
	builtBridge  string
	buildErr     error
)

func TestMain(m *testing.M) {
	if os.Getenv(fixturePaneFile) != "" || os.Getenv(fixtureErrFile) != "" || os.Getenv(fixtureScriptFile) != "" {
		os.Exit(runHerdrFixture())
	}
	dir, err := os.MkdirTemp("", "herdr-omp-bridge-cmd-")
	if err != nil {
		os.Exit(90)
	}
	cmdBinaryDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// runHerdrFixture answers the one Herdr read the bridge performs, records the
// argv it was called with, and either prints a fixture envelope or a fixture
// error envelope on stderr with exit status 1 — the shape the real CLI uses.
func runHerdrFixture() int {
	if path := os.Getenv(fixtureArgvFile); path != "" {
		if err := os.WriteFile(path, []byte(strings.Join(os.Args[1:], " ")), 0o600); err != nil {
			return 11
		}
	}
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "unexpected argv: %q\n", os.Args)
		return 12
	}
	// One path answers a single read; the other answers whole flows, so which
	// scripted stand-in this process is decides how the argv is dispatched.
	if path := os.Getenv(fixtureScriptFile); path != "" {
		return runHerdrScript(path)
	}
	if os.Args[1] != "pane" || os.Args[2] != "get" {
		fmt.Fprintf(os.Stderr, "unexpected argv: %q\n", os.Args)
		return 12
	}
	if path := os.Getenv(fixtureErrFile); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return 13
		}
		_, _ = os.Stderr.Write(body)
		return 1
	}
	body, err := os.ReadFile(os.Getenv(fixturePaneFile))
	if err != nil {
		return 14
	}
	_, _ = os.Stdout.Write(body)
	return 0
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const (
	primaryPane    = "w3:p1"
	primarySession = "omp-primary-1"
	workerPane     = "w1:p2"
	workerSession  = "omp-worker-1"
	workerWorktree = "ws-worktree-1"
)

type bridgeFixture struct {
	t        *testing.T
	env      map[string]string
	store    *state.Store
	stateDir string
	repoRoot string
	worktree string
}

func newBridgeFixture(t *testing.T) *bridgeFixture {
	t.Helper()
	stateDir := t.TempDir()
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worktree := t.TempDir()
	t.Setenv(fixtureStateDir, stateDir)
	return &bridgeFixture{
		t:        t,
		store:    store,
		stateDir: stateDir,
		repoRoot: t.TempDir(),
		worktree: worktree,
		env: map[string]string{
			"HERDR_ENV":              "1",
			"HERDR_PANE_ID":          workerPane,
			"HERDR_PLUGIN_STATE_DIR": stateDir,
		},
	}
}

// seedWorker records a dispatched worker bound to its launch-time pane.
func (f *bridgeFixture) seedWorker(id, launchPane, publicPane, session string) task.Task {
	f.t.Helper()
	worker := task.Task{
		ID:                  id,
		ProjectKey:          f.repoRoot,
		Title:               "Implement the thing",
		Brief:               "Do the bounded thing.",
		BaseRevision:        "0123456",
		Branch:              "orch/" + id,
		Checks:              []string{"go test ./..."},
		WorktreePath:        f.worktree,
		WorktreeWorkspaceID: workerWorktree,
		CurrentWorkspaceID:  workerWorktree,
		TerminalID:          "term-" + id,
		LaunchPaneID:        launchPane,
		PublicPaneID:        publicPane,
		AgentName:           "orch-" + id,
		OMPSessionID:        session,
		Phase:               task.PromptSubmitted,
	}
	if err := f.store.CreateTask(context.Background(), worker); err != nil {
		f.t.Fatalf("create worker %s: %v", id, err)
	}
	stored, err := f.store.Task(context.Background(), id)
	if err != nil {
		f.t.Fatalf("read worker %s: %v", id, err)
	}
	return stored
}

// seedPrimary records a bound primary project.
func (f *bridgeFixture) seedPrimary() task.Project {
	f.t.Helper()
	project := task.Project{
		Key:                f.repoRoot,
		RepoWorkspaceID:    "ws-repo-1",
		CurrentWorkspaceID: "ws-repo-1",
		TerminalID:         "term-primary",
		LaunchPaneID:       primaryPane,
		PublicPaneID:       primaryPane,
		AgentName:          "primary",
		OMPSessionID:       primarySession,
	}
	if err := f.store.CreateProject(context.Background(), project); err != nil {
		f.t.Fatalf("create primary project: %v", err)
	}
	return project
}

// confirm marks a binding's handshake verified the way the coordinator does
// after comparing the announced session with `agent get`: the extension announced
// the id, and the coordinator confirmed that id.
func (f *bridgeFixture) confirm(launchPane, session string) {
	f.t.Helper()
	f.confirmRef(launchPane, task.Announcement{ID: session}, task.SessionRef{Kind: task.SessionKindID, Value: session})
}

// confirmRef records one announced identity and the reference Herdr reported for
// it, which is what a confirmed handshake is made of.
func (f *bridgeFixture) confirmRef(launchPane string, announced task.Announcement, ref task.SessionRef) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.store.RecordReady(ctx, launchPane, announced, 1); err != nil {
		f.t.Fatalf("RecordReady: %v", err)
	}
	if err := f.store.ConfirmReady(ctx, launchPane, ref); err != nil {
		f.t.Fatalf("ConfirmReady: %v", err)
	}
}

// announcedID is the announcement a bare session id describes.
func announcedID(session string) task.Announcement { return task.Announcement{ID: session} }

// refID is the reference form of a bare session id.
func refID(session string) task.SessionRef {
	return task.SessionRef{Kind: task.SessionKindID, Value: session}
}

// paneEnvelope builds the JSON envelope the real Herdr CLI prints for
// `pane get`.
func paneEnvelope(t *testing.T, paneID, terminalID, workspaceID, cwd, agent string) string {
	t.Helper()
	body := map[string]any{
		"id": "req_fixture",
		"result": map[string]any{
			"type": "pane_info",
			"pane": map[string]any{
				"pane_id":       paneID,
				"terminal_id":   terminalID,
				"workspace_id":  workspaceID,
				"tab_id":        "tab-1",
				"label":         "",
				"title":         "omp",
				"agent":         agent,
				"display_agent": agent,
				"agent_status":  "idle",
				"cwd":           cwd,
				"focused":       false,
				"revision":      7,
			},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode pane envelope: %v", err)
	}
	return string(encoded)
}

// useHerdrFixture points the bridge at this test binary as its Herdr CLI and
// makes it answer `pane get` with the given envelope.
func (f *bridgeFixture) useHerdrFixture(envelope string) string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "pane.json")
	if err := os.WriteFile(path, []byte(envelope), 0o600); err != nil {
		f.t.Fatalf("write pane fixture: %v", err)
	}
	return f.useHerdrFixtureFiles(path, "")
}

// useHerdrErrorFixture makes the stand-in Herdr answer `pane get` with an error
// envelope on stderr and exit status 1, the way the real CLI reports failures.
func (f *bridgeFixture) useHerdrErrorFixture(envelope string) string {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), "pane-error.json")
	if err := os.WriteFile(path, []byte(envelope), 0o600); err != nil {
		f.t.Fatalf("write pane error fixture: %v", err)
	}
	return f.useHerdrFixtureFiles("", path)
}

func (f *bridgeFixture) useHerdrFixtureFiles(panePath, errPath string) string {
	f.t.Helper()
	executable, err := os.Executable()
	if err != nil {
		f.t.Fatalf("os.Executable: %v", err)
	}
	f.env["HERDR_ORCH_HERDR_BIN_PATH"] = executable
	f.t.Setenv(fixturePaneFile, panePath)
	f.t.Setenv(fixtureErrFile, errPath)
	argv := filepath.Join(f.t.TempDir(), "argv.txt")
	f.t.Setenv(fixtureArgvFile, argv)
	return argv
}

// ---------------------------------------------------------------------------
// fresh-session adoption
// ---------------------------------------------------------------------------

// seedFreshWorker records a worker the coordinator has provisioned but whose OMP
// session it cannot know yet: the pane is recorded, the session is not.
func (f *bridgeFixture) seedFreshWorker(id, launchPane string) task.Task {
	f.t.Helper()
	worker := f.seedWorker(id, launchPane, launchPane, "")
	// seedWorker stores a prompt_submitted task; a fresh one is being started.
	updated, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
		next.Phase = task.AgentStarted
		return nil
	})
	if err != nil {
		f.t.Fatalf("make %s fresh: %v", id, err)
	}
	return updated
}

func TestBridgeResolveAdoptsAFreshWorkerSession(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedFreshWorker("t1", workerPane)

	resp := f.call("resolve", "omp-fresh-1", "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if !result.Bound || result.Role != string(state.RoleWorker) || result.TaskID != "t1" {
		t.Fatalf("resolve = %+v, want the fresh worker bound", result)
	}
	if !result.Rebound {
		t.Errorf("resolve = %+v, want the adoption reported as a rebind of identities", result)
	}

	stored, err := f.store.Task(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if stored.OMPSessionID != "omp-fresh-1" {
		t.Fatalf("recorded session = %q, want the announced one", stored.OMPSessionID)
	}
	// Adoption is provisional: it records the session and never the handshake.
	ready, err := f.store.Ready(context.Background(), workerPane, announcedID("omp-fresh-1"))
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Fatal("adoption marked the readiness handshake verified")
	}

	// The extension can now announce, and only the coordinator can confirm.
	readyResp := f.call("ready", "omp-fresh-1", "{}")
	var readyResult ReadyResult
	readyResp.decodeResult(t, &readyResult)
	if !readyResult.Recorded {
		t.Fatalf("ready = %+v, want the announcement recorded", readyResult)
	}
	ready, err = f.store.Ready(context.Background(), workerPane, announcedID("omp-fresh-1"))
	if err != nil || ready {
		t.Fatalf("Ready after the announcement = (%v, %v), want still unverified", ready, err)
	}

	// A mutating method is still refused while readiness is unconfirmed.
	f.env["HERDR_PANE_ID"] = workerPane
	wantError(t, f.call("report", "omp-fresh-1", `{"kind":"result"}`), CodeNotReady)

	f.confirm(workerPane, "omp-fresh-1")
	calls := 0
	ops := Ops{Report: func(context.Context, Principal, json.RawMessage) (any, error) {
		calls++
		return map[string]any{"ok": true}, nil
	}}
	if resp := f.callWith("report", "omp-fresh-1", `{"kind":"result"}`, ops); !resp.OK {
		t.Fatalf("report after confirmation failed: %+v", resp.Error)
	}
	if calls != 1 {
		t.Errorf("report ran %d times, want 1", calls)
	}
}

func TestBridgeResolveAdoptsAFreshPrimarySession(t *testing.T) {
	f := newBridgeFixture(t)
	project := f.seedPrimary()
	if err := f.store.UpdateProject(context.Background(), project.Key, func(next *task.Project) error {
		next.OMPSessionID = ""
		return nil
	}); err != nil {
		t.Fatalf("make the primary fresh: %v", err)
	}
	f.env["HERDR_PANE_ID"] = primaryPane

	resp := f.call("resolve", "omp-fresh-primary", "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if !result.Bound || result.Role != string(state.RolePrimary) || result.ProjectKey != f.repoRoot {
		t.Fatalf("resolve = %+v, want the fresh primary bound", result)
	}
	stored, err := f.store.Project(context.Background(), f.repoRoot)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if stored.OMPSessionID != "omp-fresh-primary" {
		t.Fatalf("recorded session = %q, want the announced one", stored.OMPSessionID)
	}
	ready, err := f.store.Ready(context.Background(), primaryPane, announcedID("omp-fresh-primary"))
	if err != nil || ready {
		t.Fatalf("Ready after primary adoption = (%v, %v), want unverified", ready, err)
	}
}

func TestBridgeResolveRefusesAnAlreadyRecordedDifferentSession(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)

	resp := f.call("resolve", "omp-somebody-else", "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if result.Bound {
		t.Fatalf("resolve = %+v, want a refusal", result)
	}
	if result.Reason != ReasonUnverifiedRestore || result.Attention == "" {
		t.Fatalf("resolve = %+v, want an attention-carrying refusal", result)
	}
	stored, err := f.store.Task(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if stored.OMPSessionID != workerSession {
		t.Fatalf("recorded session = %q, want the original %q untouched", stored.OMPSessionID, workerSession)
	}
	if stored.Attention == "" {
		t.Error("the mismatch was not recorded on the task")
	}
}

func TestBridgeResolveAdoptionRaceHasOneWinnerAndNoOverwrite(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedFreshWorker("t1", workerPane)

	const racers = 4
	var wg sync.WaitGroup
	results := make([]ResolveResult, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			env := map[string]string{}
			for key, value := range f.env {
				env[key] = value
			}
			out, err := HandleWith(context.Background(), env, strings.NewReader(
				fmt.Sprintf(`{"version":1,"method":"resolve","ompSessionID":"omp-race-%d","params":{}}`, i)), Ops{})
			if err != nil {
				t.Errorf("Handle(resolve %d): %v", i, err)
				return
			}
			resp := decodeResponse(t, out)
			if resp.OK {
				_ = json.Unmarshal(resp.Result, &results[i])
			}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, result := range results {
		if result.Bound {
			winners++
			if result.TaskID != "t1" {
				t.Errorf("racer %d bound %q, want t1", i, result.TaskID)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("adoption race = %d winners, want exactly 1 (%+v)", winners, results)
	}
	// The winner's session is the only one recorded, and no racer overwrote it.
	stored, err := f.store.Task(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if stored.OMPSessionID == "" || !strings.HasPrefix(stored.OMPSessionID, "omp-race-") {
		t.Fatalf("recorded session = %q, want one of the announced ones", stored.OMPSessionID)
	}
	winner := -1
	for i, result := range results {
		if result.Bound {
			winner = i
		}
	}
	if want := fmt.Sprintf("omp-race-%d", winner); stored.OMPSessionID != want {
		t.Fatalf("recorded session = %q, want the winner's %q", stored.OMPSessionID, want)
	}
}

func TestBridgeResolveKeepsAnUnknownPaneUnbound(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	// A pane no record mentions: another project's terminal or a stray shell.
	f.env["HERDR_PANE_ID"] = "w9:p9"

	resp := f.call("resolve", "omp-unknown", "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if result.Bound || result.Reason != ReasonNotBound {
		t.Fatalf("resolve = %+v, want not_bound", result)
	}
	if _, err := f.store.ByLaunchPane(context.Background(), workerPane); err != nil {
		t.Errorf("the unrelated worker binding changed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// protocol plumbing
// ---------------------------------------------------------------------------

type bridgeResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type wireRequest struct {
	Version      int             `json:"version"`
	Method       string          `json:"method"`
	OMPSessionID string          `json:"ompSessionID"`
	Params       json.RawMessage `json:"params"`
}

func (f *bridgeFixture) call(method, session, params string) bridgeResponse {
	f.t.Helper()
	return f.callWith(method, session, params, Ops{})
}

// callWith runs one bridge request in this process, exactly as the extension
// spawns the executable but without the process boundary (TestBridgeCommand
// exercises that). A non-nil ops field stands in for the coordinator a later
// task wires in.
func (f *bridgeFixture) callWith(method, session, params string, ops Ops) bridgeResponse {
	f.t.Helper()
	if params == "" {
		params = "{}"
	}
	payload, err := json.Marshal(wireRequest{Version: Protocol, Method: method, OMPSessionID: session, Params: json.RawMessage(params)})
	if err != nil {
		f.t.Fatalf("encode request: %v", err)
	}
	out, err := HandleWith(context.Background(), f.env, strings.NewReader(string(payload)), ops)
	if err != nil {
		f.t.Fatalf("Handle(%s): %v", method, err)
	}
	return decodeResponse(f.t, out)
}

func decodeResponse(t *testing.T, out []byte) bridgeResponse {
	t.Helper()
	var resp bridgeResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode bridge response %q: %v", out, err)
	}
	if resp.OK && resp.Error != nil {
		t.Fatalf("response %s is both ok and an error", out)
	}
	if !resp.OK && resp.Error == nil {
		t.Fatalf("response %s is neither ok nor an error", out)
	}
	return resp
}

func (r bridgeResponse) decodeResult(t *testing.T, into any) {
	t.Helper()
	if !r.OK {
		t.Fatalf("response is an error: %+v", r.Error)
	}
	if err := json.Unmarshal(r.Result, into); err != nil {
		t.Fatalf("decode result %s: %v", r.Result, err)
	}
}

func wantError(t *testing.T, resp bridgeResponse, code string) {
	t.Helper()
	if resp.OK {
		t.Fatalf("response succeeded (%s), want error %q", resp.Result, code)
	}
	if resp.Error.Code != code {
		t.Errorf("error code = %q (%s), want %q", resp.Error.Code, resp.Error.Message, code)
	}
	if strings.TrimSpace(resp.Error.Message) == "" {
		t.Errorf("error %q carries no message", code)
	}
}

// ---------------------------------------------------------------------------
// request validation
// ---------------------------------------------------------------------------

func TestBridgeRejectsForeignOrMalformedRequests(t *testing.T) {
	f := newBridgeFixture(t)

	t.Run("not a herdr pane", func(t *testing.T) {
		delete(f.env, "HERDR_ENV")
		defer func() { f.env["HERDR_ENV"] = "1" }()
		wantError(t, f.call("resolve", workerSession, "{}"), CodeNoHerdrContext)
	})

	t.Run("no inherited launch pane id", func(t *testing.T) {
		delete(f.env, "HERDR_PANE_ID")
		defer func() { f.env["HERDR_PANE_ID"] = workerPane }()
		wantError(t, f.call("resolve", workerSession, "{}"), CodeNoHerdrContext)
	})

	t.Run("missing state dir", func(t *testing.T) {
		delete(f.env, "HERDR_PLUGIN_STATE_DIR")
		defer func() { f.env["HERDR_PLUGIN_STATE_DIR"] = f.stateDir }()
		wantError(t, f.call("resolve", workerSession, "{}"), CodeStateDirUnavailable)
	})

	t.Run("unsupported protocol version", func(t *testing.T) {
		payload := `{"version":2,"method":"resolve","ompSessionID":"s","params":{}}`
		out, err := Handle(context.Background(), f.env, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		wantError(t, decodeResponse(t, out), CodeRequestVersion)
	})

	t.Run("malformed request", func(t *testing.T) {
		out, err := Handle(context.Background(), f.env, strings.NewReader("{not json"))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		wantError(t, decodeResponse(t, out), CodeBadRequest)
	})

	t.Run("missing method", func(t *testing.T) {
		payload := `{"version":1,"method":"","ompSessionID":"s","params":{}}`
		out, err := Handle(context.Background(), f.env, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		wantError(t, decodeResponse(t, out), CodeBadRequest)
	})

	t.Run("unknown method", func(t *testing.T) {
		wantError(t, f.call("self_destruct", workerSession, "{}"), CodeUnsupportedMethod)
	})

	t.Run("params is not an object", func(t *testing.T) {
		wantError(t, f.call("resolve", workerSession, "[]"), CodeBadParams)
	})

	t.Run("request over the size bound", func(t *testing.T) {
		payload := `{"version":1,"method":"resolve","ompSessionID":"s","params":{"pad":"` +
			strings.Repeat("x", MaxRequestBytes+1) + `"}}`
		out, err := Handle(context.Background(), f.env, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		wantError(t, decodeResponse(t, out), CodeRequestTooLarge)
	})
}

// ---------------------------------------------------------------------------
// announced identities
// ---------------------------------------------------------------------------

const workerSessionFile = `C:\work\omp\sessions\-work-orch\2026-09-25T11-59-03-837Z_omp-worker-1.jsonl`

// callIdentity runs one bridge request with the full identity surface the
// extension announces (id, session file, materialization state).
func (f *bridgeFixture) callIdentity(method, id, file string, onDisk bool, params string, ops Ops) bridgeResponse {
	f.t.Helper()
	payload, err := json.Marshal(map[string]any{
		"version":          Protocol,
		"method":           method,
		"ompSessionID":     id,
		"ompSessionFile":   file,
		"ompSessionOnDisk": onDisk,
		"params":           json.RawMessage(params),
	})
	if err != nil {
		f.t.Fatalf("encode request: %v", err)
	}
	out, err := HandleWith(context.Background(), f.env, strings.NewReader(string(payload)), ops)
	if err != nil {
		f.t.Fatalf("Handle(%s): %v", method, err)
	}
	return decodeResponse(f.t, out)
}

func TestBridgeMatchesTheIdentityHerdrReports(t *testing.T) {
	f := newBridgeFixture(t)
	// The worker's recorded identity is the session file, exactly as recorded for
	// a pane where Herdr reported kind "path".
	f.seedWorker("t1", workerPane, workerPane, workerSessionFile)
	f.confirmRef(workerPane,
		task.Announcement{ID: workerSession, File: workerSessionFile, OnDisk: true},
		task.SessionRef{Kind: task.SessionKindPath, Value: workerSessionFile})

	// The extension announces both identities; the path form is the one Herdr
	// reported, so it matches.
	resp := f.callIdentity("resolve", workerSession, workerSessionFile, true, "{}", Ops{})
	var result ResolveResult
	resp.decodeResult(t, &result)
	if !result.Bound || result.TaskID != "t1" {
		t.Fatalf("resolve = %+v, want the worker bound by its announced file", result)
	}

	// A different announced file is a different session: refused with attention,
	// and the recorded identity is never overwritten.
	other := f.callIdentity("resolve", workerSession, `C:\work\omp\sessions\other.jsonl`, true, "{}", Ops{})
	var refused ResolveResult
	other.decodeResult(t, &refused)
	if refused.Bound {
		t.Fatalf("resolve with another announced file bound: %+v", refused)
	}
	if refused.Reason != ReasonUnverifiedRestore || refused.Attention == "" {
		t.Errorf("resolve(other file) = %+v, want an attention-carrying refusal", refused)
	}
	stored, err := f.store.Task(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if stored.OMPSessionID != workerSessionFile {
		t.Errorf("recorded identity = %q, want the original file untouched", stored.OMPSessionID)
	}
}

func TestBridgeReadyRecordsTheWholeAnnouncement(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)

	// The extension announces the id, the session file and the fact that the
	// runtime has not materialized it yet.
	resp := f.callIdentity("ready", workerSession, workerSessionFile, false, "{}", Ops{})
	var result ReadyResult
	resp.decodeResult(t, &result)
	if !result.Recorded {
		t.Fatalf("ready = %+v, want the announcement recorded", result)
	}
	stored, found, err := f.store.AnnouncedSession(context.Background(), workerPane)
	if err != nil || !found {
		t.Fatalf("AnnouncedSession = (_, %v, %v), want found", found, err)
	}
	if stored.ID != workerSession || stored.File != workerSessionFile || stored.OnDisk {
		t.Errorf("stored announcement = %+v, want the id, the file and not materialized", stored)
	}
	// The confirmation that was already made is kept: the announcement still
	// describes the session it verified.
	if ready, err := f.store.Ready(context.Background(), workerPane, announcedID(workerSession)); err != nil || !ready {
		t.Errorf("announcing a file dropped the confirmed id: (%v, %v)", ready, err)
	}

	// A request with neither identity is refused before any role work.
	empty := f.callIdentity("resolve", "", "", true, "{}", Ops{})
	wantError(t, empty, CodeBadRequest)
}

func TestBridgeAcceptsAFileOnlyAnnouncement(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSessionFile)

	// getSessionId() is always present for OMP, but the bridge must not require
	// both identities: an agent whose runtime exposes only a session file still
	// binds, and the announced file adopts the fresh pane-recorded task.
	resp := f.callIdentity("resolve", "", workerSessionFile, false, "{}", Ops{})
	var result ResolveResult
	resp.decodeResult(t, &result)
	if !result.Bound || result.TaskID != "t1" {
		t.Fatalf("resolve(file only) = %+v, want the worker bound", result)
	}
	stored, err := f.store.Task(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if stored.OMPSessionID != workerSessionFile {
		t.Errorf("recorded identity = %q, want the announced file", stored.OMPSessionID)
	}
}

// ---------------------------------------------------------------------------
// the brief adjudication methods
// ---------------------------------------------------------------------------

func TestBridgeBriefAdjudicationIsPrimaryOnlyAndWired(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)
	f.confirm(primaryPane, primarySession)

	type seen struct {
		principal Principal
		params    json.RawMessage
	}
	var inspected, adjudicated seen
	ops := Ops{
		BriefInspection: func(_ context.Context, p Principal, params json.RawMessage) (any, error) {
			inspected = seen{p, params}
			return map[string]any{"taskId": "t1", "paneId": workerPane, "revision": 7}, nil
		},
		AdjudicateBrief: func(_ context.Context, p Principal, params json.RawMessage) (any, error) {
			adjudicated = seen{p, params}
			return map[string]any{"decision": "delivered"}, nil
		},
	}

	// The worker may use neither method, and neither op runs when it tries.
	f.env["HERDR_PANE_ID"] = workerPane
	wantError(t, f.callWith("brief_inspection", workerSession, `{"taskId":"t1"}`, ops), CodeForbidden)
	wantError(t, f.callWith("adjudicate_brief", workerSession,
		`{"taskId":"t1","expectedRevision":7,"decision":"delivered","paneId":"w1:p2","digest":"d"}`, ops), CodeForbidden)
	if inspected.principal.Role != "" || adjudicated.principal.Role != "" {
		t.Fatalf("an adjudication op ran for a worker: %+v / %+v", inspected, adjudicated)
	}

	// The primary may, and each op sees the verified principal and its own body.
	f.env["HERDR_PANE_ID"] = primaryPane
	resp := f.callWith("brief_inspection", primarySession, `{"taskId":"t1"}`, ops)
	if !resp.OK {
		t.Fatalf("brief_inspection failed: %+v", resp.Error)
	}
	if inspected.principal.Role != state.RolePrimary || inspected.principal.ProjectKey != f.repoRoot {
		t.Errorf("inspection principal = %+v, want the primary project", inspected.principal)
	}
	var body coord.BriefInspectionParams
	if err := json.Unmarshal(inspected.params, &body); err != nil || body.TaskID != "t1" {
		t.Errorf("inspection params = %s, want the task id", inspected.params)
	}
	decided := f.callWith("adjudicate_brief", primarySession,
		`{"taskId":"t1","expectedRevision":7,"decision":"delivered","paneId":"w1:p2","digest":"d"}`, ops)
	if !decided.OK {
		t.Fatalf("adjudicate_brief failed: %+v", decided.Error)
	}
	var report coord.AdjudicateBriefParams
	if err := json.Unmarshal(adjudicated.params, &report); err != nil {
		t.Fatalf("adjudication params = %s", adjudicated.params)
	}
	if report.TaskID != "t1" || report.ExpectedRevision != 7 || report.Decision != "delivered" ||
		report.PaneID != "w1:p2" || report.Digest != "d" {
		t.Errorf("adjudication params = %+v, want the inspected evidence verbatim", report)
	}

	// A primary whose handshake was never confirmed mutates nothing.
	f2 := newBridgeFixture(t)
	f2.seedPrimary()
	if err := f2.store.RecordReady(context.Background(), primaryPane, announcedID(primarySession), 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	f2.env["HERDR_PANE_ID"] = primaryPane
	wantError(t, f2.callWith("adjudicate_brief", primarySession,
		`{"taskId":"t1","expectedRevision":7,"decision":"delivered","paneId":"w1:p2","digest":"d"}`, ops), CodeNotReady)

	// Unwired operations answer not_implemented only after authorization.
	unauthorized := f.callWith("brief_inspection", primarySession, `{"taskId":"t1"}`, Ops{})
	wantError(t, unauthorized, CodeNotImplemented)
}

func TestBridgeAdjudicationRefusalsAreCoded(t *testing.T) {
	// The adjudication's own refusals map to stable codes, so a primary can act on
	// them without reading a message.
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"not adjudicable", coord.ErrNotAdjudicable, "not_adjudicable"},
		{"evidence changed", coord.ErrEvidenceChanged, "evidence_changed"},
		{"step in flight", coord.ErrStepInFlight, "step_in_flight"},
		{"invalid request", coord.ErrInvalidRequest, "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := coordErrorCode(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
				t.Fatalf("coordErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestBridgeAmbiguousOperationsAreCodedExplicitly pins the code an answer or an
// approved-keys operation carries when its outcome could not be identified.
//
// Herdr may already have written the input, so the caller must be told exactly
// that and inspect the worker before anything else reaches it. Reporting the
// ambiguity as `herdr_unavailable` would say the opposite — that the CLI could not
// be used at all — and invite a blind retry of input that may already be there.
func TestBridgeAmbiguousOperationsAreCodedExplicitly(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"an answer that may already have landed", fmt.Errorf("answer the worker on pane %q: %w", workerPane, herdr.ErrAmbiguous)},
		{"approved keys that may already have been written", fmt.Errorf("approve the dialog on pane %q: %w", workerPane, herdr.ErrAmbiguous)},
		{"a timeout classified as an ambiguity", &herdr.AmbiguousError{Op: "agent prompt", Reason: "the answer never arrived", Err: &herdr.CLIError{
			Op: "agent prompt", Code: herdr.CodeTimeout, Message: "the command timed out",
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coordErrorCode(tc.err)
			if got == CodeHerdrUnavailable {
				t.Errorf("coordErrorCode(%v) = %q: an ambiguous operation is not the same as an unusable CLI", tc.err, got)
			}
			if got != "ambiguous_operation" {
				t.Errorf("coordErrorCode(%v) = %q, want %q", tc.err, got, "ambiguous_operation")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// role authorization
// ---------------------------------------------------------------------------

func TestBridgeWorkerCannotDispatchAndPrimaryCan(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)
	f.confirm(primaryPane, primarySession)

	calls := 0
	var seen Principal
	ops := Ops{Dispatch: func(_ context.Context, p Principal, params json.RawMessage) (any, error) {
		calls++
		seen = p
		var decoded map[string]any
		if err := json.Unmarshal(params, &decoded); err != nil {
			return nil, err
		}
		return map[string]any{"accepted": decoded["title"]}, nil
	}}

	dispatchParams := `{"title":"Bounded task","brief":"b","checks":["go test ./..."],"baseRevision":"0123456"}`

	// The worker is a bound, ready agent, and still may not dispatch.
	f.env["HERDR_PANE_ID"] = workerPane
	wantError(t, f.callWith("dispatch", workerSession, dispatchParams, ops), CodeForbidden)
	if calls != 0 {
		t.Fatalf("dispatch ran %d times for a worker", calls)
	}

	// The primary may, and the coordinator sees the verified principal rather
	// than anything the caller supplied.
	f.env["HERDR_PANE_ID"] = primaryPane
	resp := f.callWith("dispatch", primarySession, dispatchParams, ops)
	if !resp.OK {
		t.Fatalf("primary dispatch failed: %+v", resp.Error)
	}
	if calls != 1 {
		t.Fatalf("dispatch ran %d times for the primary, want 1", calls)
	}
	if seen.Role != state.RolePrimary || seen.ProjectKey != f.repoRoot {
		t.Errorf("principal = %+v, want the primary project %q", seen, f.repoRoot)
	}
	if seen.LaunchPaneID != primaryPane || seen.PublicPaneID != primaryPane {
		t.Errorf("principal panes = %q/%q, want %q/%q", seen.LaunchPaneID, seen.PublicPaneID, primaryPane, primaryPane)
	}
	var result map[string]any
	resp.decodeResult(t, &result)
	if result["accepted"] != "Bounded task" {
		t.Errorf("dispatch result = %v, want the coordinator's value", result)
	}
}

func TestBridgePrimaryCannotReportAndWorkerCannotDispatchOtherTasks(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)
	f.confirm(primaryPane, primarySession)

	calls := 0
	ops := Ops{
		Report:   func(context.Context, Principal, json.RawMessage) (any, error) { calls++; return map[string]any{}, nil },
		Dispatch: func(context.Context, Principal, json.RawMessage) (any, error) { calls++; return map[string]any{}, nil },
	}

	f.env["HERDR_PANE_ID"] = primaryPane
	wantError(t, f.callWith("report", primarySession, `{"kind":"result"}`, ops), CodeForbidden)

	f.env["HERDR_PANE_ID"] = workerPane
	wantError(t, f.callWith("dispatch", workerSession, `{}`, ops), CodeForbidden)

	if calls != 0 {
		t.Errorf("a forbidden call reached the coordinator %d times", calls)
	}
}

func TestBridgeMutatingMethodsRequireConfirmedReadiness(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)

	calls := 0
	ops := Ops{Report: func(context.Context, Principal, json.RawMessage) (any, error) {
		calls++
		return map[string]any{"recorded": true}, nil
	}}

	// Announced but not yet compared with `agent get`: the extension may have
	// active tools, but no mutation is authorized.
	if err := f.store.RecordReady(context.Background(), workerPane, announcedID(workerSession), 1); err != nil {
		t.Fatalf("RecordReady: %v", err)
	}
	wantError(t, f.callWith("report", workerSession, `{"kind":"result"}`, ops), CodeNotReady)
	if calls != 0 {
		t.Fatalf("unverified handshake reached the coordinator")
	}

	f.confirm(workerPane, workerSession)
	if resp := f.callWith("report", workerSession, `{"kind":"result"}`, ops); !resp.OK {
		t.Fatalf("confirmed worker report failed: %+v", resp.Error)
	}
	if calls != 1 {
		t.Fatalf("report ran %d times, want 1", calls)
	}
}

func TestBridgeMismatchedSessionAndPaneFail(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.seedWorker("t2", "w5:p5", "w5:p5", "omp-worker-2")
	f.confirm("w5:p5", "omp-worker-2")

	ops := Ops{Report: func(context.Context, Principal, json.RawMessage) (any, error) {
		return map[string]any{"recorded": true}, nil
	}}

	// t2's session is genuine and ready, but this process's inherited pane is
	// not t2's launch pane: a session id that names another agent is not
	// authority over this process.
	wantError(t, f.callWith("report", "omp-worker-2", `{"kind":"result"}`, ops), CodeLaunchPaneMismatch)

	// A session nobody recorded is simply unbound.
	wantError(t, f.callWith("report", "omp-ghost", `{"kind":"result"}`, ops), CodeNotBound)

	// After the verified rebind, t2's own process may report again.
	f.env["HERDR_PANE_ID"] = "w5:p5"
	if resp := f.callWith("report", "omp-worker-2", `{"kind":"result"}`, ops); !resp.OK {
		t.Fatalf("bound worker report failed: %+v", resp.Error)
	}
}

func TestBridgeUnboundPaneIsNotBoundAndNeverMutable(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)

	// A process whose OMP session belongs to no record is simply unbound.
	f.env["HERDR_PANE_ID"] = "w9:p9"
	resp := f.call("resolve", "omp-unrecorded", "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if result.Bound {
		t.Fatalf("resolve bound an unrecorded session: %+v", result)
	}
	if result.Reason != ReasonNotBound {
		t.Errorf("reason = %q, want %q", result.Reason, ReasonNotBound)
	}
	if result.Protocol != Protocol {
		t.Errorf("protocol = %d, want %d", result.Protocol, Protocol)
	}
	if attention := result.Attention; strings.TrimSpace(attention) != "" {
		t.Errorf("an unrecorded session is not a failed restore, but carried attention %q", attention)
	}

	// Neither may it mutate, and a pane id in params is never authority:
	// naming t1's real launch pane from another process changes nothing.
	wantError(t, f.call("dispatch", "omp-unrecorded", `{"paneID":"`+workerPane+`"}`), CodeNotBound)
	wantError(t, f.callWith("report", workerSession, `{"paneID":"`+workerPane+`","paneId":"`+workerPane+`","launchPaneID":"`+workerPane+`"}`, Ops{
		Report: func(context.Context, Principal, json.RawMessage) (any, error) {
			return map[string]any{"recorded": true}, nil
		},
	}), CodeLaunchPaneMismatch)
}

// ---------------------------------------------------------------------------
// resolve
// ---------------------------------------------------------------------------

func TestBridgeResolveDirectBindingNeedsNoHerdr(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, "w2:p4", workerSession)

	resp := f.call("resolve", workerSession, "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)

	if !result.Bound || result.Role != string(state.RoleWorker) {
		t.Fatalf("resolve = %+v, want the bound worker", result)
	}
	if result.TaskID != "t1" || result.ProjectKey != f.repoRoot {
		t.Errorf("resolve identities = %+v", result)
	}
	if result.LaunchPaneID != workerPane || result.PublicPaneID != "w2:p4" {
		t.Errorf("resolve panes = %q/%q, want %q/w2:p4", result.LaunchPaneID, result.PublicPaneID, workerPane)
	}
	if result.WorktreeID != workerWorktree {
		t.Errorf("resolve worktree = %q, want %q", result.WorktreeID, workerWorktree)
	}
	if result.Rebound {
		t.Errorf("a direct binding must not report a rebind")
	}

	// The primary resolves the same way.
	f.env["HERDR_PANE_ID"] = primaryPane
	primary := f.call("resolve", primarySession, "{}")
	result = ResolveResult{}
	primary.decodeResult(t, &result)
	if !result.Bound || result.Role != string(state.RolePrimary) || result.TaskID != "" {
		t.Fatalf("primary resolve = %+v", result)
	}
}

func TestBridgeResolveClearsPrimaryAttentionAfterVerifiedRestore(t *testing.T) {
	f := newBridgeFixture(t)
	project := f.seedPrimary()
	const reason = "the recorded primary pane w3:p1 is absent from Herdr's snapshot"
	if err := f.store.UpdateProject(context.Background(), project.Key, func(next *task.Project) error {
		next.PrimaryAttention = reason
		return nil
	}); err != nil {
		t.Fatalf("record primary attention: %v", err)
	}

	const newPane = "w7:p3"
	f.useHerdrFixture(paneEnvelope(t, newPane, "term-restored", "ws-repo-1", f.repoRoot, "omp"))
	f.env["HERDR_PANE_ID"] = newPane

	resp := f.call("resolve", primarySession, "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if !result.Bound || !result.Rebound || result.Role != string(state.RolePrimary) {
		t.Fatalf("resolve after verified primary restore = %+v, want a rebound primary", result)
	}

	after, err := f.store.Project(context.Background(), project.Key)
	if err != nil {
		t.Fatalf("read primary after restore: %v", err)
	}
	if after.PrimaryAttention != "" {
		t.Errorf("PrimaryAttention = %q after verified restore, want it cleared", after.PrimaryAttention)
	}
	if after.LaunchPaneID != newPane || after.PublicPaneID != newPane || after.TerminalID != "term-restored" {
		t.Errorf("verified restore did not rebind the primary: %+v", after)
	}
}

func TestBridgeResolveRebindsVerifiedRestore(t *testing.T) {
	f := newBridgeFixture(t)
	before := f.seedWorker("t1", workerPane, workerPane, workerSession)

	// Herdr restarted the agent process in a new pane of the same worktree
	// workspace: the launch id changed, the OMP session, terminal snapshot and
	// checkout provenance still prove who this is.
	const newPane = "w7:p3"
	argv := f.useHerdrFixture(paneEnvelope(t, newPane, "term-restored", workerWorktree, f.worktree, "omp"))
	f.env["HERDR_PANE_ID"] = newPane

	resp := f.call("resolve", workerSession, "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)

	if !result.Bound || !result.Rebound {
		t.Fatalf("resolve after restore = %+v, want a verified rebind", result)
	}
	if result.LaunchPaneID != newPane || result.PublicPaneID != newPane || result.TerminalID != "term-restored" {
		t.Errorf("rebound identities = %+v", result)
	}
	if result.WorktreeID != workerWorktree {
		t.Errorf("rebind changed the worktree workspace id: %+v", result)
	}

	after, err := f.store.Task(context.Background(), "t1")
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if after.LaunchPaneID != newPane || after.PublicPaneID != newPane {
		t.Errorf("stored panes = %q/%q, want %q", after.LaunchPaneID, after.PublicPaneID, newPane)
	}
	if after.TerminalID != "term-restored" || after.CurrentWorkspaceID != workerWorktree {
		t.Errorf("stored terminal/workspace = %q/%q", after.TerminalID, after.CurrentWorkspaceID)
	}
	if after.WorktreePath != before.WorktreePath || after.WorktreeWorkspaceID != before.WorktreeWorkspaceID {
		t.Errorf("rebind changed worktree provenance: %+v", after)
	}
	if after.Revision != before.Revision+1 {
		t.Errorf("revision = %d, want %d", after.Revision, before.Revision+1)
	}

	recorded, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("read recorded herdr argv: %v", err)
	}
	if got := strings.TrimSpace(string(recorded)); got != "pane get "+newPane {
		t.Errorf("herdr argv = %q, want %q", got, "pane get "+newPane)
	}

	// A second resolve is now a direct hit and must stay idempotent.
	resp = f.call("resolve", workerSession, "{}")
	result = ResolveResult{}
	resp.decodeResult(t, &result)
	if !result.Bound || result.Rebound {
		t.Fatalf("second resolve = %+v, want a direct binding", result)
	}
	again, err := f.store.Task(context.Background(), "t1")
	if err != nil {
		t.Fatalf("re-read task: %v", err)
	}
	if again.Revision != after.Revision {
		t.Errorf("idempotent resolve rewrote the task: %d -> %d", after.Revision, again.Revision)
	}
}

// TestBridgeResolveKeepsARebindConflictRecoverable pins what happens when the
// primary's binding changes while its restore is being verified. The verified
// rebind is a compare-and-swap, so the change must reach resolve as a conflict it
// recovers from: the tools stay inactive, the concurrent write stands, and the
// primary is told — never an internal failure that hides the reason.
func TestBridgeResolveKeepsARebindConflictRecoverable(t *testing.T) {
	f := newBridgeFixture(t)
	seeded := f.seedPrimary()
	const reason = "the recorded primary cannot be verified: left by an earlier pass"
	if err := f.store.UpdateProject(context.Background(), seeded.Key, func(next *task.Project) error {
		next.PrimaryAttention = reason
		return nil
	}); err != nil {
		t.Fatalf("record primary attention: %v", err)
	}

	// Herdr restarted the primary in a new pane of the recorded workspace and
	// checkout, and while that pane is verified another bridge call records a move
	// of the same primary to a new public pane.
	const newPane = "w7:p3"
	f.useHerdrStub(stubScript{
		RepoRoot: f.repoRoot,
		Panes: map[string]stubPane{newPane: {
			PaneID:      newPane,
			TerminalID:  "term-restored",
			WorkspaceID: "ws-repo-1",
			Cwd:         f.repoRoot,
			Agent:       "omp",
		}},
		ProjectMovedTo: "w9:p9",
	})
	f.env["HERDR_PANE_ID"] = newPane

	resp := f.call("resolve", primarySession, "{}")
	if !resp.OK {
		t.Fatalf("resolve answered %+v, want the recoverable unverified-restore result", resp.Error)
	}
	var result ResolveResult
	resp.decodeResult(t, &result)
	if result.Bound {
		t.Errorf("resolve = %+v, want the orchestrator tools to stay inactive", result)
	}
	if result.Reason != ReasonUnverifiedRestore {
		t.Errorf("resolve reason = %q, want %q", result.Reason, ReasonUnverifiedRestore)
	}
	if strings.TrimSpace(result.Attention) == "" {
		t.Error("a rebind that lost its compare-and-swap recorded no attention for the human")
	}

	// The concurrent move is the fact that stands: this resolve did not overwrite it
	// with the pane it was verifying.
	project, err := f.store.Project(context.Background(), f.repoRoot)
	if err != nil {
		t.Fatalf("read the project: %v", err)
	}
	if project.PublicPaneID != "w9:p9" {
		t.Errorf("public pane = %q, want the concurrent move %q preserved", project.PublicPaneID, "w9:p9")
	}
	if project.LaunchPaneID != primaryPane {
		t.Errorf("launch pane = %q, want the recorded %q untouched", project.LaunchPaneID, primaryPane)
	}
	if project.PrimaryAttention != reason {
		t.Errorf("PrimaryAttention = %q, want %q kept: a rebind that lost its compare-and-swap verified nothing", project.PrimaryAttention, reason)
	}
	if got := len(notesOf(t, f, "attention")); got != 1 {
		t.Errorf("attention notes = %d, want the conflict reported exactly once", got)
	}
}

func TestBridgeResolveAcceptsEquivalentCheckoutPaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path semantics only")
	}
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	const newPane = "w7:p3"
	f.useHerdrFixture(paneEnvelope(t, newPane, "term-restored", workerWorktree, strings.ToUpper(f.worktree), "omp"))
	f.env["HERDR_PANE_ID"] = newPane

	resp := f.call("resolve", workerSession, "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if !result.Bound {
		t.Fatalf("resolve rejected an equivalently spelled checkout: %+v", result)
	}
}

// TestBridgeResolveAcceptsAMovedWorkerWorkspace proves the workspace check
// accepts either workspace a worker is recorded in. Moving a public pane
// records the workspace it was moved to while keeping the worktree workspace as
// provenance, so a worker whose OMP process restarts in the moved workspace is
// still verified instead of staying unbound forever.
func TestBridgeResolveAcceptsAMovedWorkerWorkspace(t *testing.T) {
	f := newBridgeFixture(t)
	ctx := context.Background()
	f.seedWorker("t1", workerPane, workerPane, workerSession)

	const movedPublicPane = "w9:p4"
	if err := f.store.MovePublicPane(ctx, workerPane, workerPane, movedPublicPane); err != nil {
		t.Fatalf("MovePublicPane: %v", err)
	}
	moved, err := f.store.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("read moved task: %v", err)
	}
	if moved.CurrentWorkspaceID == moved.WorktreeWorkspaceID {
		t.Fatalf("the move recorded no new workspace: %+v", moved)
	}
	if moved.WorktreeWorkspaceID != workerWorktree {
		t.Fatalf("the move dropped the worktree provenance: %+v", moved)
	}

	// The worker's OMP process restarted in a new pane that sits in the
	// workspace its pane was moved to.
	const restoredPane = "w7:p3"
	f.useHerdrFixture(paneEnvelope(t, restoredPane, "term-moved", moved.CurrentWorkspaceID, f.worktree, "omp"))
	f.env["HERDR_PANE_ID"] = restoredPane

	var result ResolveResult
	f.call("resolve", workerSession, "{}").decodeResult(t, &result)
	if !result.Bound || !result.Rebound {
		t.Fatalf("resolve in the moved workspace = %+v, want a verified rebind", result)
	}
	if result.Role != string(state.RoleWorker) || result.TaskID != "t1" || result.ProjectKey != f.repoRoot {
		t.Errorf("rebound role/binding = %+v", result)
	}
	if result.LaunchPaneID != restoredPane || result.PublicPaneID != restoredPane || result.TerminalID != "term-moved" {
		t.Errorf("rebound identities = %+v", result)
	}
	if result.WorktreeID != moved.WorktreeWorkspaceID {
		t.Errorf("rebind changed the worktree provenance: %+v", result)
	}

	after, err := f.store.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("re-read task: %v", err)
	}
	if after.LaunchPaneID != restoredPane || after.PublicPaneID != restoredPane {
		t.Errorf("stored panes = %q/%q, want %q", after.LaunchPaneID, after.PublicPaneID, restoredPane)
	}
	if after.CurrentWorkspaceID != moved.CurrentWorkspaceID || after.WorktreeWorkspaceID != moved.WorktreeWorkspaceID {
		t.Errorf("rebind rewrote the recorded workspaces: %+v", after)
	}
}

// TestBridgeResolveRefusesAWorkerInAThirdWorkspace proves the widened check is
// still a refusal: a pane in a workspace the task was never recorded in cannot
// promote itself, and the attention text names both the workspace found and the
// ones that would have been accepted.
func TestBridgeResolveRefusesAWorkerInAThirdWorkspace(t *testing.T) {
	f := newBridgeFixture(t)
	ctx := context.Background()
	f.seedWorker("t1", workerPane, workerPane, workerSession)

	if err := f.store.MovePublicPane(ctx, workerPane, workerPane, "w9:p4"); err != nil {
		t.Fatalf("MovePublicPane: %v", err)
	}
	moved, err := f.store.Task(ctx, "t1")
	if err != nil {
		t.Fatalf("read moved task: %v", err)
	}

	const restoredPane = "w7:p3"
	const unrelated = "ws-unrelated-9"
	if unrelated == moved.CurrentWorkspaceID || unrelated == moved.WorktreeWorkspaceID {
		t.Fatalf("the fixture reuses a recorded workspace: %+v", moved)
	}
	f.useHerdrFixture(paneEnvelope(t, restoredPane, "term-elsewhere", unrelated, f.worktree, "omp"))
	f.env["HERDR_PANE_ID"] = restoredPane

	var result ResolveResult
	f.call("resolve", workerSession, "{}").decodeResult(t, &result)
	if result.Bound {
		t.Fatalf("resolve bound a pane in an unrelated workspace: %+v", result)
	}
	if result.Reason != ReasonUnverifiedRestore {
		t.Errorf("reason = %q, want %q", result.Reason, ReasonUnverifiedRestore)
	}
	if !strings.Contains(result.Attention, unrelated) {
		t.Errorf("attention does not name the workspace the pane was found in: %q", result.Attention)
	}
	if !strings.Contains(result.Attention, moved.WorktreeWorkspaceID) || !strings.Contains(result.Attention, moved.CurrentWorkspaceID) {
		t.Errorf("attention does not name the recorded workspaces: %q", result.Attention)
	}
	f.assertAttentionRecorded(t, moved)
}

func TestBridgeResolveRefusesUnverifiedRestore(t *testing.T) {
	t.Run("different checkout", func(t *testing.T) {
		f := newBridgeFixture(t)
		before := f.seedWorker("t1", workerPane, workerPane, workerSession)
		const newPane = "w7:p3"
		f.useHerdrFixture(paneEnvelope(t, newPane, "term-elsewhere", "ws-other", t.TempDir(), "omp"))
		f.env["HERDR_PANE_ID"] = newPane

		resp := f.call("resolve", workerSession, "{}")
		var result ResolveResult
		resp.decodeResult(t, &result)
		if result.Bound {
			t.Fatalf("resolve bound a process in another checkout: %+v", result)
		}
		if result.Reason != ReasonUnverifiedRestore {
			t.Errorf("reason = %q, want %q", result.Reason, ReasonUnverifiedRestore)
		}
		if strings.TrimSpace(result.Attention) == "" {
			t.Error("unverified restore carries no attention")
		}
		f.assertAttentionRecorded(t, before)
	})

	t.Run("not an omp pane", func(t *testing.T) {
		f := newBridgeFixture(t)
		before := f.seedWorker("t1", workerPane, workerPane, workerSession)
		const newPane = "w7:p3"
		f.useHerdrFixture(paneEnvelope(t, newPane, "term-shell", workerWorktree, f.worktree, ""))
		f.env["HERDR_PANE_ID"] = newPane

		var result ResolveResult
		f.call("resolve", workerSession, "{}").decodeResult(t, &result)
		if result.Bound {
			t.Fatalf("resolve bound a non-agent pane: %+v", result)
		}
		f.assertAttentionRecorded(t, before)
	})

	t.Run("herdr cannot confirm the pane", func(t *testing.T) {
		f := newBridgeFixture(t)
		before := f.seedWorker("t1", workerPane, workerPane, workerSession)
		const newPane = "w7:p3"
		f.useHerdrErrorFixture(`{"id":"req","error":{"code":"pane_not_found","message":"no such pane"}}`)
		f.env["HERDR_PANE_ID"] = newPane

		var result ResolveResult
		f.call("resolve", workerSession, "{}").decodeResult(t, &result)
		if result.Bound {
			t.Fatalf("resolve bound a pane Herdr does not know: %+v", result)
		}
		if result.Reason != ReasonUnverifiedRestore {
			t.Errorf("reason = %q, want %q", result.Reason, ReasonUnverifiedRestore)
		}
		f.assertAttentionRecorded(t, before)
	})

	t.Run("no herdr binary available", func(t *testing.T) {
		f := newBridgeFixture(t)
		before := f.seedWorker("t1", workerPane, workerPane, workerSession)
		f.env["HERDR_PANE_ID"] = "w7:p3"

		var result ResolveResult
		f.call("resolve", workerSession, "{}").decodeResult(t, &result)
		if result.Bound || result.Reason != ReasonUnverifiedRestore {
			t.Fatalf("resolve without a Herdr binary = %+v", result)
		}
		f.assertAttentionRecorded(t, before)
	})
}

// assertAttentionRecorded proves a failed restore is durable and visible: the
// task carries attention and the primary has an inbox note, never a silent
// unbind.
func (f *bridgeFixture) assertAttentionRecorded(t *testing.T, before task.Task) {
	t.Helper()
	ctx := context.Background()
	after, err := f.store.Task(ctx, before.ID)
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	if strings.TrimSpace(after.Attention) == "" {
		t.Error("failed restore did not record attention on the task")
	}
	if after.LaunchPaneID != before.LaunchPaneID || after.PublicPaneID != before.PublicPaneID {
		t.Errorf("failed restore changed the binding: %+v", after)
	}

	notes := readInbox(t, f)
	var found bool
	for _, note := range notes {
		if note.ProjectKey == f.repoRoot && note.TaskID == before.ID && note.Kind == "attention" {
			found = true
		}
	}
	if !found {
		t.Errorf("no attention note for the primary in %d inbox rows", len(notes))
	}
}

// readInbox reads the durable inbox the way a later coordinator pass would.
// The store exposes no reader for it yet, so the test opens the same database
// read-only: the point is to prove a note was really persisted, not to exercise
// an accessor.
func readInbox(t *testing.T, f *bridgeFixture) []task.Inbox {
	t.Helper()
	path := filepath.Join(f.stateDir, "orchestrator.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatalf("open state read-only: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(), `SELECT project_key, task_id, kind, summary, delivery FROM inbox`)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	defer rows.Close()
	var out []task.Inbox
	for rows.Next() {
		var in task.Inbox
		if err := rows.Scan(&in.ProjectKey, &in.TaskID, &in.Kind, &in.Summary, &in.Delivery); err != nil {
			t.Fatalf("scan inbox: %v", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate inbox: %v", err)
	}
	return out
}

func TestBridgeResolveRefusesAmbiguousSession(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.seedWorker("t2", "w5:p5", "w5:p5", workerSession)

	resp := f.call("resolve", workerSession, "{}")
	var result ResolveResult
	resp.decodeResult(t, &result)
	if result.Bound {
		t.Fatalf("resolve picked one of two owners: %+v", result)
	}
	if result.Reason != ReasonAmbiguousSession {
		t.Errorf("reason = %q, want %q", result.Reason, ReasonAmbiguousSession)
	}
	if strings.TrimSpace(result.Attention) == "" {
		t.Error("ambiguous session carries no attention")
	}
}

// ---------------------------------------------------------------------------
// ready
// ---------------------------------------------------------------------------

func TestBridgeReadyRecordsAnUnverifiedHandshake(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.seedPrimary()

	resp := f.call("ready", workerSession, "{}")
	var result struct {
		Recorded     bool   `json:"recorded"`
		LaunchPaneID string `json:"launchPaneId"`
		PublicPaneID string `json:"publicPaneId"`
		Role         string `json:"role"`
		Protocol     int    `json:"protocol"`
	}
	resp.decodeResult(t, &result)
	if !result.Recorded || result.Role != string(state.RoleWorker) {
		t.Fatalf("ready = %+v", result)
	}
	if result.LaunchPaneID != workerPane || result.Protocol != Protocol {
		t.Errorf("ready identities = %+v", result)
	}

	ready, err := f.store.Ready(context.Background(), workerPane, announcedID(workerSession))
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Error("ready must stay unverified until the coordinator confirms it against `agent get`")
	}
	if err := f.store.ConfirmReady(context.Background(), workerPane, refID(workerSession)); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}
	if ready, err = f.store.Ready(context.Background(), workerPane, announcedID(workerSession)); err != nil || !ready {
		t.Fatalf("Ready after confirm = (%v, %v)", ready, err)
	}

	// An unrelated session records nothing.
	wantError(t, f.call("ready", "omp-ghost", "{}"), CodeNotBound)

	// A session bound to another launch pane records nothing either.
	f.env["HERDR_PANE_ID"] = "w9:p9"
	wantError(t, f.call("ready", workerSession, "{}"), CodeLaunchPaneMismatch)
	if ready, err := f.store.Ready(context.Background(), "w9:p9", announcedID(workerSession)); err != nil || ready {
		t.Fatalf("a mismatched pane recorded readiness: (%v, %v)", ready, err)
	}
}

// ---------------------------------------------------------------------------
// public identity moves
// ---------------------------------------------------------------------------

func TestBridgeReportSurvivesPublicPaneMove(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)

	// Herdr moved the worker into another workspace: only the public id
	// changes; the launch-time id is what the live process still calls home.
	if err := f.store.MovePublicPane(context.Background(), workerPane, workerPane, "w4:p8"); err != nil {
		t.Fatalf("MovePublicPane: %v", err)
	}

	var seen Principal
	ops := Ops{Report: func(_ context.Context, p Principal, _ json.RawMessage) (any, error) {
		seen = p
		return map[string]any{"recorded": true}, nil
	}}
	if resp := f.callWith("report", workerSession, `{"kind":"result","summary":"done"}`, ops); !resp.OK {
		t.Fatalf("report after a move failed: %+v", resp.Error)
	}
	if seen.LaunchPaneID != workerPane {
		t.Errorf("principal launch pane = %q, want the unchanged %q", seen.LaunchPaneID, workerPane)
	}
	if seen.PublicPaneID != "w4:p8" {
		t.Errorf("principal public pane = %q, want the moved w4:p8", seen.PublicPaneID)
	}

	// And resolve keeps reporting the public id for Herdr CLI calls.
	var result ResolveResult
	f.call("resolve", workerSession, "{}").decodeResult(t, &result)
	if !result.Bound || result.PublicPaneID != "w4:p8" || result.LaunchPaneID != workerPane {
		t.Fatalf("resolve after a move = %+v", result)
	}
}

// ---------------------------------------------------------------------------
// the real executable
// ---------------------------------------------------------------------------

func builtBridgeBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		out := filepath.Join(cmdBinaryDir, "herdr-omp-orchestrator.exe")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/herdr-omp-orchestrator")
		cmd.Dir = filepath.Join("..", "..")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if combined, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v: %s", err, combined)
			return
		}
		builtBridge = out
	})
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return builtBridge
}

func TestBridgeCommandProcess(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	binary := builtBridgeBinary(t)

	run := func(t *testing.T, args []string, payload string) (bridgeResponse, int, string) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = []string{
			"HERDR_ENV=1",
			"HERDR_PANE_ID=" + workerPane,
			"HERDR_PLUGIN_STATE_DIR=" + f.stateDir,
			"SystemRoot=" + os.Getenv("SystemRoot"),
		}
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		var exit *exec.ExitError
		if err != nil {
			if !asExitError(err, &exit) {
				t.Fatalf("run bridge: %v", err)
			}
			code = exit.ExitCode()
		}
		return decodeResponse(t, []byte(stdout.String())), code, stderr.String()
	}

	t.Run("resolve", func(t *testing.T) {
		resp, code, stderr := run(t, []string{"bridge", "resolve"},
			`{"version":1,"method":"resolve","ompSessionID":"`+workerSession+`","params":{}}`)
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %q", code, stderr)
		}
		var result ResolveResult
		resp.decodeResult(t, &result)
		if !result.Bound || result.TaskID != "t1" || result.Role != string(state.RoleWorker) {
			t.Fatalf("resolve through the executable = %+v", result)
		}
	})

	t.Run("argv method must match the request", func(t *testing.T) {
		resp, code, _ := run(t, []string{"bridge", "report"},
			`{"version":1,"method":"resolve","ompSessionID":"`+workerSession+`","params":{}}`)
		if code != 0 {
			t.Fatalf("exit code = %d", code)
		}
		wantError(t, resp, CodeMethodMismatch)
	})

	t.Run("unsupported protocol version", func(t *testing.T) {
		resp, code, _ := run(t, []string{"bridge", "resolve"},
			`{"version":9,"method":"resolve","ompSessionID":"`+workerSession+`","params":{}}`)
		if code != 0 {
			t.Fatalf("exit code = %d", code)
		}
		wantError(t, resp, CodeRequestVersion)
	})

	t.Run("oversized request", func(t *testing.T) {
		resp, code, _ := run(t, []string{"bridge", "resolve"},
			`{"version":1,"method":"resolve","ompSessionID":"s","params":{"pad":"`+strings.Repeat("x", MaxRequestBytes+1)+`"}}`)
		if code != 0 {
			t.Fatalf("exit code = %d", code)
		}
		wantError(t, resp, CodeRequestTooLarge)
	})
}

func asExitError(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*target = exit
	}
	return ok
}

// ---------------------------------------------------------------------------
// protocol constants
// ---------------------------------------------------------------------------

// TestBridgeProtocolSurface pins the wire contract the generated OMP extension
// depends on: the request field names the extension writes and the reasons the
// extension switches on.
func TestBridgeProtocolSurface(t *testing.T) {
	payload, err := json.Marshal(wireRequest{Version: Protocol, Method: "resolve", OMPSessionID: "s", Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"version":1,"method":"resolve","ompSessionID":"s","params":{}}`
	if string(payload) != want {
		t.Errorf("request wire form = %s, want %s", payload, want)
	}
	if Protocol != 1 {
		t.Errorf("Protocol = %d, want 1", Protocol)
	}
	for _, reason := range []string{ReasonNotBound, ReasonAmbiguousSession, ReasonUnverifiedRestore} {
		if strings.TrimSpace(reason) == "" {
			t.Error("empty resolve reason")
		}
	}
	if MaxRequestBytes != 1<<20 {
		t.Errorf("MaxRequestBytes = %d, want 1 MiB", MaxRequestBytes)
	}
	if MaxResponseBytes > MaxRequestBytes*4 {
		t.Errorf("MaxResponseBytes = %d is not bounded relative to the request bound", MaxResponseBytes)
	}
}

// ---------------------------------------------------------------------------
// Task 5: a scripted Herdr, worker reports and reviewed decisions
// ---------------------------------------------------------------------------
//
// The scripted stand-in below answers the commands a Task 5 operation makes —
// `agent get`, `agent read`, `agent prompt`, `agent send-keys`, `worktree list`
// and `worktree remove` — from a JSON script this process writes, and records
// every argv it was invoked with. It is a real child process, so argv
// construction, stdout and exit status all cross an exec boundary exactly as they
// do against Herdr, and the coordinator under test is the real one.

// stubPane is one pane projection the stand-in answers `pane get` with.
type stubPane struct {
	PaneID      string `json:"paneId"`
	TerminalID  string `json:"terminalId"`
	WorkspaceID string `json:"workspaceId"`
	TabID       string `json:"tabId"`
	Cwd         string `json:"cwd"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agentStatus"`
}

// stubAgent is one live agent the stand-in reports. The session pair is the
// reference Herdr publishes for it, in the form that build reports.
type stubAgent struct {
	Name         string `json:"name"`
	Agent        string `json:"agent"`
	AgentStatus  string `json:"agentStatus"`
	PaneID       string `json:"paneId"`
	TerminalID   string `json:"terminalId"`
	WorkspaceID  string `json:"workspaceId"`
	TabID        string `json:"tabId"`
	Cwd          string `json:"cwd"`
	SessionKind  string `json:"sessionKind"`
	SessionValue string `json:"sessionValue"`
}

// stubRead is one `agent read` answer, consumed in order: a test that needs the
// dialog to change between a preview and an approval scripts two of them.
type stubRead struct {
	Text      string `json:"text"`
	ErrorCode string `json:"errorCode"`
	ErrorText string `json:"errorText"`
}

// stubWorktree is one entry of `worktree list`.
type stubWorktree struct {
	Path            string `json:"path"`
	Branch          string `json:"branch"`
	OpenWorkspaceID string `json:"openWorkspaceId"`
}

// stubScript is everything the stand-in answers with, plus what it recorded.
// The stand-in rewrites the file after every call that changes what Herdr knows.
type stubScript struct {
	RepoRoot string               `json:"repoRoot"`
	Panes    map[string]stubPane  `json:"panes"`
	Agents   map[string]stubAgent `json:"agents"`
	Reads    []stubRead           `json:"reads"`
	ReadAt   int                  `json:"readAt"`
	Worktree []stubWorktree       `json:"worktree"`
	// PromptError and SendKeysError make those two calls refuse the way Herdr
	// refuses them (a blocked agent, an unknown key) without writing anything.
	PromptError   string `json:"promptError"`
	SendKeysError string `json:"sendKeysError"`
	// RemoveNotFound names workspaces Herdr no longer has, the state a move into
	// another workspace leaves behind.
	RemoveNotFound []string `json:"removeNotFound"`
	// NotLinked names workspaces Herdr refuses to remove with
	// `not_linked_worktree`: after a move, the workspace holding the pane is not
	// the checkout's Herdr-managed worktree workspace.
	NotLinked []string `json:"notLinked"`
	// OpenWorkspace is the workspace the stand-in reports when it re-opens a
	// checkout no workspace holds, standing in for the workspace Herdr creates
	// for that checkout.
	OpenWorkspace string `json:"openWorkspace"`
	// OpenContradiction makes `worktree open` contradict itself by naming another
	// checkout or repository in one field: "checkout_path", "repo_root" or
	// "worktree_path" selects which. A caller that trusted such an answer could
	// remove the wrong workspace.
	OpenContradiction string `json:"openContradiction"`
	// RemoveError makes `worktree remove` refuse; a dirty checkout is one way.
	RemoveError string `json:"removeError"`
	// RemoveEchoesForced makes a successful `worktree remove` answer claim the
	// removal was forced, which this plugin never asks for and must never accept.
	RemoveEchoesForced bool `json:"removeEchoesForced"`
	// ProjectMovedTo makes the stand-in record a move of the named repository's
	// project to that public pane before it answers `pane get`: another bridge call
	// writing the same binding while this restore is being verified.
	ProjectMovedTo string `json:"projectMovedTo"`

	Prompts  []stubPrompt   `json:"prompts"`
	SentKeys []stubSendKeys `json:"sentKeys"`
	Removed  []string       `json:"removed"`
	// Opened records the workspaces `worktree open` answered with.
	Opened []string `json:"opened"`
}

type stubPrompt struct {
	Pane string `json:"pane"`
	Text string `json:"text"`
}

type stubSendKeys struct {
	Pane string   `json:"pane"`
	Keys []string `json:"keys"`
}

func readStubScript(path string) (stubScript, error) {
	var script stubScript
	data, err := os.ReadFile(path)
	if err != nil {
		return script, err
	}
	if err := json.Unmarshal(data, &script); err != nil {
		return script, err
	}
	if script.Panes == nil {
		script.Panes = map[string]stubPane{}
	}
	if script.Agents == nil {
		script.Agents = map[string]stubAgent{}
	}
	return script, nil
}

func writeStubScript(path string, script stubScript) error {
	data, err := json.Marshal(script)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// runHerdrScript answers one command from the script, recording the argv first
// so a test sees every call even when the answer is a refusal.
func runHerdrScript(path string) int {
	if calls := os.Getenv(fixtureCallsFile); calls != "" {
		line, err := json.Marshal(os.Args[1:])
		if err != nil {
			return 31
		}
		file, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return 32
		}
		_, err = file.Write(append(line, '\n'))
		_ = file.Close()
		if err != nil {
			return 33
		}
	}
	script, err := readStubScript(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub: script: %v\n", err)
		return 34
	}
	argv := os.Args[1:]
	switch argv[0] + " " + argv[1] {
	case "pane get":
		pane, ok := script.Panes[argv[2]]
		if !ok {
			return stubError("pane_not_found", "pane "+argv[2]+" not found")
		}
		if script.ProjectMovedTo != "" {
			if code := stubMoveStoredProject(script); code != 0 {
				return code
			}
		}
		return stubResult(map[string]any{"type": "pane_info", "pane": stubPaneBody(pane)})
	case "agent get":
		agent, ok := script.agent(argv[2])
		if !ok {
			return stubError("agent_not_found", "agent target "+argv[2]+" not found")
		}
		return stubResult(map[string]any{"type": "agent_info", "agent": stubAgentBody(agent)})
	case "agent prompt":
		script.Prompts = append(script.Prompts, stubPrompt{Pane: argv[2], Text: argv[3]})
		if err := writeStubScript(path, script); err != nil {
			return 35
		}
		if script.PromptError != "" {
			return stubError(script.PromptError, "the prompt was refused before any input")
		}
		agent, ok := script.agent(argv[2])
		if !ok {
			return stubError("agent_not_found", "agent target "+argv[2]+" not found")
		}
		return stubResult(map[string]any{"type": "agent_prompted", "agent": stubAgentBody(agent)})
	case "agent read":
		if script.ReadAt >= len(script.Reads) {
			// Nothing left to show: an exhausted script is a refusal, never the
			// previous dialog again.
			return stubError("agent_not_idle", "the stand-in has no further screen scripted")
		}
		read := script.Reads[script.ReadAt]
		script.ReadAt++
		if err := writeStubScript(path, script); err != nil {
			return 36
		}
		if read.ErrorCode != "" {
			return stubError(read.ErrorCode, stubText(read.ErrorText, "the read was refused"))
		}
		if _, err := os.Stdout.WriteString(read.Text); err != nil {
			return 37
		}
		return 0
	case "agent send-keys":
		script.SentKeys = append(script.SentKeys, stubSendKeys{Pane: argv[2], Keys: argv[3:]})
		if err := writeStubScript(path, script); err != nil {
			return 38
		}
		if script.SendKeysError != "" {
			return stubError(script.SendKeysError, "the keys were refused before any were written")
		}
		return stubResult(map[string]any{"type": "ok"})
	case "worktree list":
		worktrees := make([]map[string]any, 0, len(script.Worktree))
		for _, wt := range script.Worktree {
			entry := map[string]any{
				"path":               wt.Path,
				"label":              filepath.Base(wt.Path),
				"branch":             wt.Branch,
				"open_workspace_id":  nil,
				"is_bare":            false,
				"is_detached":        false,
				"is_prunable":        false,
				"is_linked_worktree": true,
			}
			if wt.OpenWorkspaceID != "" {
				entry["open_workspace_id"] = wt.OpenWorkspaceID
			}
			worktrees = append(worktrees, entry)
		}
		return stubResult(map[string]any{
			"type": "worktree_list",
			"source": map[string]any{
				"repo_key":             strings.ToLower(filepath.ToSlash(script.RepoRoot)) + "/.git",
				"repo_name":            filepath.Base(script.RepoRoot),
				"repo_root":            script.RepoRoot,
				"source_checkout_path": script.RepoRoot,
				"source_workspace_id":  "",
			},
			"worktrees": worktrees,
		})
	case "worktree open":
		return stubWorktreeOpen(&script, path, argv)
	case "worktree remove":
		workspace := stubFlag(argv, "--workspace")
		for _, gone := range script.RemoveNotFound {
			if gone == workspace {
				return stubError("workspace_not_found", "workspace "+workspace+" no longer exists")
			}
		}
		for _, blocked := range script.NotLinked {
			if blocked == workspace {
				return stubError("not_linked_worktree", "workspace is not a Herdr-managed worktree checkout")
			}
		}
		if script.RemoveError != "" {
			return stubError(script.RemoveError, "the checkout was refused")
		}
		checkout := ""
		remaining := make([]stubWorktree, 0, len(script.Worktree))
		for _, wt := range script.Worktree {
			if wt.OpenWorkspaceID == workspace && checkout == "" {
				checkout = wt.Path
				continue
			}
			remaining = append(remaining, wt)
		}
		if checkout == "" {
			return stubError("worktree_not_found", "no worktree is open in workspace "+workspace)
		}
		script.Worktree = remaining
		script.Removed = append(script.Removed, workspace)
		if err := writeStubScript(path, script); err != nil {
			return 39
		}
		return stubResult(map[string]any{
			"type":         "worktree_removed",
			"workspace_id": workspace,
			"path":         checkout,
			"forced":       script.RemoveEchoesForced || slices.Contains(argv, "--force"),
		})
	default:
		fmt.Fprintf(os.Stderr, "stub: unsupported command: argv %q\n", argv)
		return 40
	}
}

// stubMoveStoredProject records a move of the named repository's project, the way
// another bridge call that rebound the primary would: it is the concurrent write a
// resolve's compare-and-swap must refuse to overwrite. It runs in the stand-in
// Herdr child, so the write lands between the caller's read of the binding and its
// own.
func stubMoveStoredProject(script stubScript) int {
	dir := os.Getenv(fixtureStateDir)
	if dir == "" {
		return 46
	}
	store, err := state.Open(dir)
	if err != nil {
		return 47
	}
	defer func() { _ = store.Close() }()
	err = store.UpdateProject(context.Background(), script.RepoRoot, func(next *task.Project) error {
		next.PublicPaneID = script.ProjectMovedTo
		return nil
	})
	if err != nil {
		return 48
	}
	return 0
}

// stubWorktreeOpen answers `worktree open` the way Herdr does for a checkout it
// still lists: the checkout is reported as already open, and the workspace's own
// worktree registration is restored — which is exactly what `worktree remove` was
// missing after a move closed the source workspace.
func stubWorktreeOpen(script *stubScript, scriptPath string, argv []string) int {
	checkout := stubFlag(argv, "--path")
	index := -1
	for i, wt := range script.Worktree {
		if stubSamePath(wt.Path, checkout) {
			index = i
			break
		}
	}
	if index < 0 {
		return stubError("not_git_worktree", "Herdr worktree actions require a path inside a Git work tree")
	}
	workspace := script.Worktree[index].OpenWorkspaceID
	if workspace == "" {
		workspace = stubText(script.OpenWorkspace, "w-open")
		script.Worktree[index].OpenWorkspaceID = workspace
	}
	// The open restores the workspace's worktree registration, so the refusal it
	// caused is gone.
	script.NotLinked = slices.DeleteFunc(script.NotLinked, func(id string) bool { return id == workspace })
	script.Opened = append(script.Opened, workspace)
	if err := writeStubScript(scriptPath, *script); err != nil {
		return 45
	}
	branch := script.Worktree[index].Branch
	// A real response names the paths it was asked for, in its own spelling: the
	// request carried the recorded path and Herdr answers with forward slashes for
	// the same checkout, and reports the repository it was told to open. Callers
	// must compare paths with the platform's own rules, and this stand-in keeps
	// that difference real so a regression cannot hide behind it.
	reported := filepath.ToSlash(checkout)
	workspaceCheckout, worktreePath := reported, reported
	repoRoot := stubFlag(argv, "--cwd")
	otherCheckout := filepath.ToSlash(filepath.Join(filepath.Dir(checkout), "someone-else"))
	switch script.OpenContradiction {
	case "checkout_path":
		workspaceCheckout = otherCheckout
	case "repo_root":
		repoRoot = filepath.ToSlash(filepath.Join(filepath.Dir(repoRoot), "another-repo"))
	case "worktree_path":
		worktreePath = otherCheckout
	}
	return stubResult(map[string]any{
		"type":         "worktree_opened",
		"already_open": true,
		"workspace":    stubWorkspaceBody(repoRoot, workspace, workspaceCheckout),
		"tab": map[string]any{
			"tab_id": workspace + ":t1", "workspace_id": workspace, "label": "1",
			"number": 1, "pane_count": 1,
		},
		"root_pane": stubPaneBody(stubPane{
			PaneID: workspace + ":p1", TerminalID: "term-" + workspace,
			WorkspaceID: workspace, TabID: workspace + ":t1", Cwd: checkout,
		}),
		"worktree": stubWorktreeBody(workspace, worktreePath, branch),
	})
}

// stubWorkspaceBody is one workspace projection, including the checkout
// provenance Herdr attaches to a worktree workspace.
func stubWorkspaceBody(repoRoot, workspaceID, checkout string) map[string]any {
	return map[string]any{
		"workspace_id":  workspaceID,
		"label":         filepath.Base(checkout),
		"active_tab_id": workspaceID + ":t1",
		"agent_status":  "unknown",
		"number":        1,
		"pane_count":    1,
		"tab_count":     1,
		"worktree": map[string]any{
			"checkout_path":      checkout,
			"repo_key":           strings.ToLower(filepath.ToSlash(repoRoot)) + "/.git",
			"repo_name":          filepath.Base(repoRoot),
			"repo_root":          repoRoot,
			"is_linked_worktree": true,
		},
	}
}

// stubWorktreeBody is one worktree entry as `worktree open` and `worktree list`
// report it.
func stubWorktreeBody(workspaceID, checkout, branch string) map[string]any {
	return map[string]any{
		"path":               checkout,
		"label":              filepath.Base(checkout),
		"branch":             branch,
		"open_workspace_id":  workspaceID,
		"is_bare":            false,
		"is_detached":        false,
		"is_prunable":        false,
		"is_linked_worktree": true,
	}
}

// stubSamePath compares two paths the way a Windows Herdr does: separators are
// interchangeable and case does not distinguish.
func stubSamePath(a, b string) bool {
	normalize := func(path string) string {
		return strings.ToLower(filepath.Clean(strings.ReplaceAll(path, "/", `\`)))
	}
	return normalize(a) == normalize(b)
}

// agent finds one live agent by pane id or by its live name.
func (s stubScript) agent(target string) (stubAgent, bool) {
	if agent, ok := s.Agents[target]; ok {
		return agent, true
	}
	for _, agent := range s.Agents {
		if agent.Name == target {
			return agent, true
		}
	}
	return stubAgent{}, false
}

func stubFlag(argv []string, name string) string {
	for i, arg := range argv {
		if arg == name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func stubText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func stubResult(result any) int {
	body, err := json.Marshal(map[string]any{"id": "cli:stub", "result": result})
	if err != nil {
		return 41
	}
	if _, err := os.Stdout.Write(append(body, '\n')); err != nil {
		return 42
	}
	return 0
}

func stubError(code, message string) int {
	body, err := json.Marshal(map[string]any{
		"id":    "cli:stub",
		"error": map[string]any{"code": code, "message": message},
	})
	if err != nil {
		return 43
	}
	if _, err := os.Stderr.Write(append(body, '\n')); err != nil {
		return 44
	}
	return 1
}

func stubPaneBody(p stubPane) map[string]any {
	return map[string]any{
		"pane_id":       p.PaneID,
		"terminal_id":   stubText(p.TerminalID, "term-"+p.PaneID),
		"workspace_id":  stubText(p.WorkspaceID, stubWorkspaceOf(p.PaneID)),
		"tab_id":        stubText(p.TabID, "tab-1"),
		"label":         "",
		"title":         "omp",
		"agent":         p.Agent,
		"display_agent": p.Agent,
		"agent_status":  stubText(p.AgentStatus, "idle"),
		"cwd":           p.Cwd,
		"focused":       false,
		"revision":      1,
	}
}

func stubAgentBody(a stubAgent) map[string]any {
	body := map[string]any{
		"name":                    stubText(a.Name, "orch_worker"),
		"agent":                   stubText(a.Agent, "omp"),
		"display_agent":           stubText(a.Agent, "omp"),
		"agent_status":            stubText(a.AgentStatus, "idle"),
		"pane_id":                 a.PaneID,
		"terminal_id":             stubText(a.TerminalID, "term-"+a.PaneID),
		"workspace_id":            stubText(a.WorkspaceID, stubWorkspaceOf(a.PaneID)),
		"tab_id":                  stubText(a.TabID, "tab-1"),
		"cwd":                     a.Cwd,
		"focused":                 false,
		"revision":                2,
		"state_change_seq":        1,
		"interactive_ready":       true,
		"launch_pending":          false,
		"terminal_title":          "omp",
		"terminal_title_stripped": "omp",
	}
	if strings.TrimSpace(a.SessionValue) != "" {
		body["agent_session"] = map[string]any{
			"source": "herdr:omp",
			"agent":  "omp",
			"kind":   stubText(a.SessionKind, "id"),
			"value":  a.SessionValue,
		}
	}
	return body
}

func stubWorkspaceOf(pane string) string {
	workspace, _, _ := strings.Cut(pane, ":")
	return workspace
}

// herdrStub is the scripted stand-in one bridge test talks to.
type herdrStub struct {
	t         *testing.T
	path      string
	callsFile string
}

// useHerdrStub points the coordinator under test at this test binary as its
// Herdr CLI and makes it answer from the script.
func (f *bridgeFixture) useHerdrStub(script stubScript) *herdrStub {
	f.t.Helper()
	dir := f.t.TempDir()
	path := filepath.Join(dir, "herdr-script.json")
	calls := filepath.Join(dir, "herdr-calls.jsonl")
	if err := writeStubScript(path, script); err != nil {
		f.t.Fatalf("write stub script: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		f.t.Fatalf("os.Executable: %v", err)
	}
	f.env["HERDR_ORCH_HERDR_BIN_PATH"] = executable
	f.t.Setenv(fixtureScriptFile, path)
	f.t.Setenv(fixtureCallsFile, calls)
	return &herdrStub{t: f.t, path: path, callsFile: calls}
}

func (s *herdrStub) script() stubScript {
	s.t.Helper()
	script, err := readStubScript(s.path)
	if err != nil {
		s.t.Fatalf("read stub script: %v", err)
	}
	return script
}

func (s *herdrStub) update(mutate func(*stubScript)) {
	s.t.Helper()
	script := s.script()
	mutate(&script)
	if err := writeStubScript(s.path, script); err != nil {
		s.t.Fatalf("write stub script: %v", err)
	}
}

// queueReads scripts the next `agent read` answers and rewinds the cursor, so a
// scenario that needs the screen to change scripts exactly what is shown when.
func (s *herdrStub) queueReads(reads ...stubRead) {
	s.t.Helper()
	s.update(func(script *stubScript) {
		script.Reads = reads
		script.ReadAt = 0
	})
}

// setAgent replaces what the stand-in reports for one pane.
func (s *herdrStub) setAgent(pane string, agent stubAgent) {
	s.t.Helper()
	s.update(func(script *stubScript) { script.Agents[pane] = agent })
}

func (s *herdrStub) dropAgent(pane string) {
	s.t.Helper()
	s.update(func(script *stubScript) { delete(script.Agents, pane) })
}

// calls lists every argv the stand-in was invoked with, in order.
func (s *herdrStub) calls() [][]string {
	s.t.Helper()
	data, err := os.ReadFile(s.callsFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		s.t.Fatalf("read stub calls: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			s.t.Fatalf("decode stub call %q: %v", line, err)
		}
		out = append(out, argv)
	}
	return out
}

// called reports whether one exact argv was made.
func (s *herdrStub) called(argv ...string) bool {
	s.t.Helper()
	return s.callIndex(argv...) >= 0
}

// callIndex reports where one exact argv sits in the recorded calls, or -1, so a
// test can assert the order of two operations as well as their presence.
func (s *herdrStub) callIndex(argv ...string) int {
	s.t.Helper()
	for i, call := range s.calls() {
		if slices.Equal(call, argv) {
			return i
		}
	}
	return -1
}

// lastCallIndex reports where one exact argv was made for the last time, which is
// what an operation that may be retried has to be asserted on.
func (s *herdrStub) lastCallIndex(argv ...string) int {
	s.t.Helper()
	last := -1
	for i, call := range s.calls() {
		if slices.Equal(call, argv) {
			last = i
		}
	}
	return last
}

// forced reports whether any removal asked Herdr to force it, which this plugin
// must never do.
func (s *herdrStub) forced() bool {
	s.t.Helper()
	for _, call := range s.calls() {
		if len(call) >= 2 && call[0] == "worktree" && call[1] == "remove" && slices.Contains(call, "--force") {
			return true
		}
	}
	return false
}

// notesOf lists the durable inbox rows of one kind.
func notesOf(t *testing.T, f *bridgeFixture, kind string) []task.Inbox {
	t.Helper()
	var out []task.Inbox
	for _, note := range readInbox(t, f) {
		if note.Kind == kind {
			out = append(out, note)
		}
	}
	return out
}

// coordinator wires a real coordinator over this fixture's store and the
// scripted Herdr, so a bridge test authorizes the caller and then exercises the
// coordinator's own persistence and identity checks, exactly as the executable
// does.
func (f *bridgeFixture) coordinator(t *testing.T) *coord.Coordinator {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	client, err := herdr.NewClient(executable)
	if err != nil {
		t.Fatalf("herdr.NewClient: %v", err)
	}
	coordinator, err := coord.New(f.store, client, coord.Options{
		CallTimeout:      5 * time.Second,
		ReadinessTimeout: time.Second,
		PollInterval:     10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("coord.New: %v", err)
	}
	return coordinator
}

// workerAgent is the stand-in's answer for one worker pane whose live agent
// reports the recorded session as an id, which is the form this fixture uses by
// default.
func workerAgent(pane, session, worktree string) stubAgent {
	return workerAgentOn(pane, task.SessionKindID, session, worktree)
}

// workerAgentOn is workerAgent for one explicit reference form, so a test can make
// Herdr report the session the way a build that only publishes the session file
// does.
func workerAgentOn(pane, kind, value, worktree string) stubAgent {
	return stubAgent{
		Name:         "orch_worker",
		Agent:        "omp",
		AgentStatus:  "idle",
		PaneID:       pane,
		TerminalID:   "term-worker",
		WorkspaceID:  workerWorktree,
		Cwd:          worktree,
		SessionKind:  kind,
		SessionValue: value,
	}
}

// recordSession rewrites what a seeded task records as its OMP session, which is
// the identity a build keeps when Herdr reported that form.
func (f *bridgeFixture) recordSession(t *testing.T, id, session string) {
	t.Helper()
	st := f.task(t, id)
	if _, err := f.store.UpdateTask(context.Background(), id, st.Revision, func(next *task.Task) error {
		next.OMPSessionID = session
		return nil
	}); err != nil {
		t.Fatalf("record session %q on %s: %v", session, id, err)
	}
}

// asPrimary makes this bridge process the primary's pane and records its
// confirmed handshake: the bridge keys a caller by the pane its process inherited,
// so a primary method is only addressable from the primary's own pane.
func (f *bridgeFixture) asPrimary() {
	f.t.Helper()
	f.confirm(primaryPane, primarySession)
	f.env["HERDR_PANE_ID"] = primaryPane
}

// asWorker makes this bridge process the worker's pane.
func (f *bridgeFixture) asWorker() {
	f.t.Helper()
	f.confirm(workerPane, workerSession)
	f.env["HERDR_PANE_ID"] = workerPane
}

// asWorkerWith is asWorker for a handshake the test describes in full: the whole
// announcement and the reference Herdr reported for it, which is what a build that
// publishes one of the two identities actually stores.
func (f *bridgeFixture) asWorkerWith(launchPane string, announced task.Announcement, ref task.SessionRef) {
	f.t.Helper()
	f.confirmRef(launchPane, announced, ref)
	f.env["HERDR_PANE_ID"] = launchPane
}

// movePublicPane records a cross-workspace move of a worker's public pane, which
// is the only identity a later Herdr call may target.
func (f *bridgeFixture) movePublicPane(t *testing.T, launchPane, newPublicPane string) {
	t.Helper()
	if err := f.store.MovePublicPane(context.Background(), launchPane, launchPane, newPublicPane); err != nil {
		t.Fatalf("MovePublicPane: %v", err)
	}
}

func (f *bridgeFixture) task(t *testing.T, id string) task.Task {
	t.Helper()
	st, err := f.store.Task(context.Background(), id)
	if err != nil {
		t.Fatalf("read task %s: %v", id, err)
	}
	return st
}

// reportPayload is a complete final report for one task, in the wire spelling
// the extension sends.
func (f *bridgeFixture) reportPayload(t *testing.T, id, kind string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"kind":     kind,
		"summary":  "Ported the parser and covered it with the recorded checks.",
		"branch":   "orch/" + id,
		"evidence": []string{"commit 773efbc4c9a0d7f0f6f2b3f2d5f1c0b8f0a1d2e3 internal/coord/report.go"},
		"checks":   []string{"go test ./internal/coord ok"},
		"risk":     "none: reachable only from the worker role",
	})
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	return string(payload)
}

// reviewPayload uses the durable report preview, never an assumed revision.
func (f *bridgeFixture) reviewPayload(t *testing.T, ops Ops, id, disposition string) string {
	t.Helper()
	var preview coord.ReportPreview
	f.callWith("report_preview", primarySession, fmt.Sprintf(`{"taskId":%q}`, id), ops).decodeResult(t, &preview)
	payload, err := json.Marshal(map[string]any{
		"taskId": id, "expectedRevision": preview.Revision, "outcome": "integrated", "disposition": disposition,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// TestReportMethodsAreRoleGated pins which role each new method needs: a worker
// that names its own work can never decide it, and a primary can never report as
// one.
func TestReportMethodsAreRoleGated(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)

	calls := map[string]int{}
	ops := Ops{}
	record := func(name string) func(context.Context, Principal, json.RawMessage) (any, error) {
		return func(context.Context, Principal, json.RawMessage) (any, error) {
			calls[name]++
			return map[string]any{"recorded": true}, nil
		}
	}
	for name, field := range map[string]*func(context.Context, Principal, json.RawMessage) (any, error){
		"report":          &ops.Report,
		"answer":          &ops.Answer,
		"dialog_preview":  &ops.DialogPreview,
		"approve_dialog":  &ops.ApproveDialog,
		"report_preview":  &ops.ReportPreview,
		"review":          &ops.Review,
		"cleanup_preview": &ops.CleanupPreview,
		"cleanup":         &ops.Cleanup,
	} {
		*field = record(name)
	}

	primaryMethods := []string{"answer", "dialog_preview", "approve_dialog", "report_preview", "review", "cleanup_preview", "cleanup"}
	// The worker's whole surface is one method, reachable under every kind it
	// carries: the union in the extension is a schema, not a second gate.
	workerMethods := []string{"report"}

	t.Run("a worker cannot decide", func(t *testing.T) {
		f.asWorker()
		for _, method := range primaryMethods {
			wantError(t, f.callWith(method, workerSession, `{"taskId":"t1"}`, ops), CodeForbidden)
		}
		if len(calls) != 0 {
			t.Fatalf("a worker reached %d coordinator operations", len(calls))
		}
	})
	t.Run("a primary cannot report", func(t *testing.T) {
		f.asPrimary()
		for _, method := range workerMethods {
			wantError(t, f.callWith(method, primarySession, `{"kind":"progress","summary":"x"}`, ops), CodeForbidden)
		}
		if len(calls) != 0 {
			t.Fatalf("a primary reached %d coordinator operations", len(calls))
		}
	})
	t.Run("the right role is dispatched", func(t *testing.T) {
		f.asWorker()
		if resp := f.callWith("report", workerSession, `{"kind":"progress","summary":"halfway"}`, ops); !resp.OK {
			t.Fatalf("progress failed: %+v", resp.Error)
		}
		f.asPrimary()
		if resp := f.callWith("review", primarySession, `{"taskId":"t1","outcome":"integrated","disposition":"accepted"}`, ops); !resp.OK {
			t.Fatalf("review failed: %+v", resp.Error)
		}
		if calls["report"] != 1 || calls["review"] != 1 {
			t.Fatalf("dispatch counted %v", calls)
		}
	})
}

// TestReportWorkerFactsAreDurableAndHerdrActivityNeverCompletes drives the real
// coordinator through the bridge: a question puts the task in Needs input, a
// progress note is durable and deduplicated, and Herdr reporting the agent done
// completes nothing.
func TestReportWorkerFactsAreDurableAndHerdrActivityNeverCompletes(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)
	stub := f.useHerdrStub(stubScript{
		RepoRoot: f.repoRoot,
		Panes:    map[string]stubPane{workerPane: {PaneID: workerPane, TerminalID: "term-worker", WorkspaceID: workerWorktree, Cwd: f.worktree, Agent: "omp"}},
		Agents:   map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, f.worktree)},
	})
	ops := CoordinatorOps(f.coordinator(t))

	if resp := f.callWith("report", workerSession, `{"kind":"progress","summary":"ported the lexer"}`, ops); !resp.OK {
		t.Fatalf("progress failed: %+v", resp.Error)
	}
	if resp := f.callWith("report", workerSession, `{"kind":"progress","summary":"ported the lexer"}`, ops); !resp.OK {
		t.Fatalf("repeated progress failed: %+v", resp.Error)
	}
	if resp := f.callWith("report", workerSession, `{"kind":"question","text":"Which branch should I target?"}`, ops); !resp.OK {
		t.Fatalf("question failed: %+v", resp.Error)
	}

	after := f.task(t, "t1")
	if stage := task.StageOf(after); stage != task.NeedsInput {
		t.Errorf("stage after a question = %q, want %q", stage, task.NeedsInput)
	}
	if !strings.Contains(after.Attention, "Which branch should I target?") {
		t.Errorf("attention %q does not carry the question", after.Attention)
	}
	if got := len(notesOf(t, f, "progress")); got != 1 {
		t.Errorf("progress notes = %d, want 1 deduplicated entry", got)
	}
	if got := len(notesOf(t, f, "question")); got != 1 {
		t.Errorf("question notes = %d, want exactly 1 durable entry", got)
	}
	// Reporting facts is not a terminal operation: nothing here may have read or
	// moved a pane.
	for _, call := range stub.calls() {
		if len(call) > 0 && (call[0] == "pane" || call[0] == "worktree") {
			t.Errorf("a worker report made a terminal call: %q", call)
		}
	}

	// Herdr can report the agent as done at any moment. That is terminal
	// activity, and it completes nothing.
	done := workerAgent(workerPane, workerSession, f.worktree)
	done.AgentStatus = "done"
	stub.setAgent(workerPane, done)
	if resp := f.callWith("report", workerSession, `{"kind":"progress","summary":"still working"}`, ops); !resp.OK {
		t.Fatalf("progress after Herdr reported done failed: %+v", resp.Error)
	}
	activity := f.task(t, "t1")
	if activity.Report != nil {
		t.Errorf("Herdr's done activity created a report: %+v", activity.Report)
	}
	if activity.Disposition != "" {
		t.Errorf("Herdr's done activity recorded a disposition: %q", activity.Disposition)
	}
	if stage := task.StageOf(activity); stage == task.ReadyForReview || stage == task.Done {
		t.Errorf("Herdr's done activity moved the task to %q", stage)
	}
}

// TestReportRejectsAnIncompleteReportAndReadsTheResult drives validation and the
// review handoff through the bridge: only a complete result is accepted, it makes
// the task ready for review, and only a disposition closes it.
func TestReportRejectsAnIncompleteReportAndReadsTheResult(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.confirm(workerPane, workerSession)
	f.useHerdrStub(stubScript{
		RepoRoot: f.repoRoot,
		Agents:   map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, f.worktree)},
	})
	ops := CoordinatorOps(f.coordinator(t))

	// An unknown kind is a malformed *report*, not a malformed request: the body
	// decodes, and the coordinator's result validation owns the refusal. The live
	// acceptance path asserts the same code and message for the same body.
	wantError(t, f.callWith("report", workerSession, f.reportPayload(t, "t1", "partial"), ops), "invalid_report")
	wantError(t, f.callWith("report", workerSession, `{"kind":"result","summary":"done","branch":"orch/t1","evidence":[],"checks":[],"risk":"none"}`, ops), "invalid_report")
	if after := f.task(t, "t1"); after.Report != nil || after.Revision != 1 {
		t.Fatalf("a refused report was recorded: %+v", after)
	}

	if resp := f.callWith("report", workerSession, f.reportPayload(t, "t1", "result"), ops); !resp.OK {
		t.Fatalf("report failed: %+v", resp.Error)
	}
	reviewed := f.task(t, "t1")
	if reviewed.Report == nil || reviewed.Report.Kind != "result" {
		t.Fatalf("recorded report = %+v", reviewed.Report)
	}
	if stage := task.StageOf(reviewed); stage != task.ReadyForReview {
		t.Errorf("stage after a final report = %q, want %q", stage, task.ReadyForReview)
	}
	if got := notesOf(t, f, "result"); len(got) != 1 {
		t.Errorf("result notes = %d, want exactly 1 durable entry", len(got))
	}
	// Ready for review is not done: the user has not decided anything yet.
	if reviewed.Disposition != "" {
		t.Errorf("a worker report recorded a disposition: %q", reviewed.Disposition)
	}

	// Only the primary's own pane can record the user's decision, and that
	// decision is what closes the task.
	wantError(t, f.callWith("review", workerSession, `{"taskId":"t1","outcome":"integrated","disposition":"accepted"}`, ops), CodeForbidden)
	f.asPrimary()
	previewResp := f.callWith("report_preview", primarySession, `{"taskId":"t1"}`, ops)
	var preview coord.ReportPreview
	previewResp.decodeResult(t, &preview)
	if preview.TaskID != "t1" || preview.Revision != reviewed.Revision ||
		preview.Summary != reviewed.Report.Summary || preview.Risk != reviewed.Report.Risk ||
		len(preview.Evidence) != len(reviewed.Report.Evidence) || len(preview.Checks) != len(reviewed.Report.Checks) {
		t.Fatalf("preview did not show the stored report and revision: %+v", preview)
	}
	wantError(t, f.callWith("review", primarySession, `{"taskId":"t1","outcome":"integrated","disposition":"accepted: merged by hand"}`, ops), "bad_request")
	wantError(t, f.callWith("review", primarySession, fmt.Sprintf(`{"taskId":"t1","outcome":"  ","disposition":"x","expectedRevision":%d}`, preview.Revision), ops), "bad_request")
	if _, err := f.store.UpdateTask(context.Background(), reviewed.ID, preview.Revision, func(next *task.Task) error {
		next.Activity = "Herdr activity changed after the report preview"
		return nil
	}); err != nil {
		t.Fatalf("advance task after preview: %v", err)
	}
	wantError(t, f.callWith("review", primarySession, fmt.Sprintf(`{"taskId":"t1","outcome":"integrated","disposition":"accepted","expectedRevision":%d}`, preview.Revision), ops), "report_changed")
	if st := f.task(t, "t1"); st.Disposition != "" {
		t.Fatalf("stale preview recorded a disposition: %+v", st)
	}
	f.callWith("report_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
	resp := f.callWith("review", primarySession, fmt.Sprintf(`{"taskId":"t1","outcome":"integrated","disposition":"accepted: merged by hand","expectedRevision":%d}`, preview.Revision), ops)
	var result coord.ReviewResult
	resp.decodeResult(t, &result)
	if result.Stage != task.Done {
		t.Errorf("review stage = %q, want %q", result.Stage, task.Done)
	}
	closed := f.task(t, "t1")
	if closed.Disposition != "accepted: merged by hand" {
		t.Errorf("recorded disposition = %q", closed.Disposition)
	}
	if stage := task.StageOf(closed); stage != task.Done {
		t.Errorf("stage after the disposition = %q, want %q", stage, task.Done)
	}
	f.callWith("report_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
	if retry := f.callWith("review", primarySession, fmt.Sprintf(`{"taskId":"t1","outcome":"integrated","disposition":"accepted: merged by hand","expectedRevision":%d}`, reviewed.Revision), ops); !retry.OK {
		t.Fatalf("identical decision retry could not resume queue draining: %+v", retry.Error)
	}
	wantError(t, f.callWith("review", primarySession, fmt.Sprintf(`{"taskId":"t1","outcome":"rejected","disposition":"rejected","expectedRevision":%d}`, preview.Revision), ops), "already_decided")
	if after := f.task(t, "t1"); after.Disposition != closed.Disposition || after.Revision != closed.Revision {
		t.Fatalf("a second review rewrote the decision: %+v", after)
	}
}

// TestAnswerRelaysToTheCurrentPublicPaneOnly pins the relay: an answer goes to
// the recorded current public pane, and a pane whose occupant or session changed
// is refused with nothing written.
func TestAnswerRelaysToTheCurrentPublicPaneOnly(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()
	// Herdr moved the worker into another workspace: only the public id changes.
	moved := "w4:p8"
	f.movePublicPane(t, workerPane, moved)
	stub := f.useHerdrStub(stubScript{
		RepoRoot: f.repoRoot,
		Agents:   map[string]stubAgent{moved: workerAgent(moved, workerSession, f.worktree)},
	})
	ops := CoordinatorOps(f.coordinator(t))

	resp := f.callWith("answer", primarySession, `{"taskId":"t1","text":"Keep the old API."}`, ops)
	var result coord.AnswerResult
	resp.decodeResult(t, &result)
	if result.PublicPaneID != moved {
		t.Errorf("answer went to %q, want the recorded current public pane %q", result.PublicPaneID, moved)
	}
	prompts := stub.script().Prompts
	if len(prompts) != 1 || prompts[0].Pane != moved || prompts[0].Text != "Keep the old API." {
		t.Fatalf("prompts = %+v, want one answer on %q", prompts, moved)
	}
	if !stub.called("agent", "prompt", moved, "Keep the old API.") {
		t.Errorf("the relay did not target the recorded public pane: %v", stub.calls())
	}
	// The launch-time pane is not the target.
	if stub.called("agent", "prompt", workerPane, "Keep the old API.") {
		t.Error("the relay targeted the launch-time pane")
	}

	t.Run("a changed session is refused", func(t *testing.T) {
		other := workerAgent(moved, "omp-someone-else", f.worktree)
		stub.setAgent(moved, other)
		wantError(t, f.callWith("answer", primarySession, `{"taskId":"t1","text":"Anything?"}`, ops), "agent_changed")
		if got := len(stub.script().Prompts); got != 1 {
			t.Errorf("a refused answer still prompted (%d prompts)", got)
		}
	})

	t.Run("a blocked worker is refused", func(t *testing.T) {
		blocked := workerAgent(moved, workerSession, f.worktree)
		blocked.AgentStatus = "blocked"
		stub.setAgent(moved, blocked)
		wantError(t, f.callWith("answer", primarySession, `{"taskId":"t1","text":"Anything?"}`, ops), "blocked")
		if got := len(stub.script().Prompts); got != 1 {
			t.Errorf("a blocked worker was prompted anyway (%d prompts)", got)
		}
	})

	t.Run("a vanished pane is refused", func(t *testing.T) {
		stub.dropAgent(moved)
		wantError(t, f.callWith("answer", primarySession, `{"taskId":"t1","text":"Anything?"}`, ops), "agent_changed")
	})
}

// TestAnswerAttemptIsDurableAndBlocksARepeatedSend pins the fail-closed flow for an
// ambiguous answer: Herdr may already have delivered it, so the uncertainty has to be
// durable before the call returns, a repeat must not type into the pane again, and
// only authoritative state - the worker reporting itself idle, i.e. the answer was
// not consumed - makes the same answer safe to send.
func TestAnswerAttemptIsDurableAndBlocksARepeatedSend(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()
	busy := workerAgent(workerPane, workerSession, f.worktree)
	stub := f.useHerdrStub(stubScript{
		RepoRoot:    f.repoRoot,
		Agents:      map[string]stubAgent{workerPane: busy},
		PromptError: "timeout",
	})
	ops := CoordinatorOps(f.coordinator(t))
	answer := `{"taskId":"t1","text":"Keep the old API."}`

	// The prompt may already have been written: the ambiguity is durable and names
	// the attempt without storing the answer.
	wantError(t, f.callWith("answer", primarySession, answer, ops), "ambiguous_operation")
	attempt := f.task(t, "t1").Blocker
	if !strings.Contains(attempt, "input attempt") || !strings.Contains(attempt, "answer") {
		t.Fatalf("the answer attempt was not recorded: blocker %q", attempt)
	}
	if strings.Contains(attempt, "Keep the old API") || strings.Contains(attempt, "old API") {
		t.Errorf("the durable attempt carries the answer itself: %q", attempt)
	}
	if got := len(stub.script().Prompts); got != 1 {
		t.Fatalf("prompts after the ambiguous answer = %d, want 1", got)
	}

	// Nothing about the worker's own status settles this: it could have consumed the
	// answer and be idle again by now. Every further answer is refused - even a
	// different one - until the user says what they saw.
	for _, status := range []string{"working", "idle"} {
		agent := workerAgent(workerPane, workerSession, f.worktree)
		agent.AgentStatus = status
		stub.setAgent(workerPane, agent)
		wantError(t, f.callWith("answer", primarySession, answer, ops), "input_pending")
		wantError(t, f.callWith("answer", primarySession, `{"taskId":"t1","text":"Something else entirely."}`, ops), "input_pending")
	}
	if got := len(stub.script().Prompts); got != 1 {
		t.Fatalf("a repeat sent the answer again (%d prompts)", got)
	}

	// The user inspected the worker and reports that the answer never arrived, which is
	// the only thing that settles an answer attempt.
	var settled coord.SettleInputResult
	f.callWith("settle_input", primarySession, `{"taskId":"t1","decision":"not_delivered"}`, ops).decodeResult(t, &settled)
	if settled.Channel != "answer" || settled.Decision != "not_delivered" {
		t.Fatalf("settlement = %+v, want the answer attempt resolved as not delivered", settled)
	}
	if got := f.task(t, "t1").Blocker; got != "" {
		t.Fatalf("the settled attempt is still recorded: %q", got)
	}

	// With the attempt settled the same answer is a deliberate new action and goes
	// through.
	stub.update(func(script *stubScript) {
		script.PromptError = ""
		script.Agents[workerPane] = workerAgent(workerPane, workerSession, f.worktree)
	})
	var result coord.AnswerResult
	f.callWith("answer", primarySession, answer, ops).decodeResult(t, &result)
	if !result.Delivered || result.PublicPaneID != workerPane {
		t.Fatalf("the retried answer = %+v, want it delivered to %q", result, workerPane)
	}
	if got := len(stub.script().Prompts); got != 2 {
		t.Fatalf("prompts after the safe retry = %d, want 2", got)
	}
	after := f.task(t, "t1")
	if after.Blocker != "" {
		t.Errorf("the delivered answer left an attempt behind: %q", after.Blocker)
	}
	if stage := task.StageOf(after); stage != task.Running {
		t.Errorf("stage after the delivered answer = %q, want %q", stage, task.Running)
	}
}

// TestSettleInputDeliveredClosesTheQuestionItAnswered pins the other settlement
// decision: the user confirms the worker did receive the answer, so the attempt is
// settled and the question it answered is closed - without anything being sent.
func TestSettleInputDeliveredClosesTheQuestionItAnswered(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	stub := f.useHerdrStub(stubScript{
		RepoRoot:    f.repoRoot,
		Agents:      map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, f.worktree)},
		PromptError: "timeout",
	})
	ops := CoordinatorOps(f.coordinator(t))

	// The worker asks, and the answer to it may already have landed.
	if resp := f.callWith("report", workerSession, `{"kind":"question","text":"Which branch should I target?"}`, ops); !resp.OK {
		t.Fatalf("question failed: %+v", resp.Error)
	}
	f.asPrimary()
	wantError(t, f.callWith("answer", primarySession, `{"taskId":"t1","text":"Keep the old API."}`, ops), "ambiguous_operation")
	pending := f.task(t, "t1")
	if pending.Blocker == "" || !isQuestionAttentionForTest(pending.Attention) {
		t.Fatalf("the ambiguous answer left the wrong state: blocker %q, attention %q", pending.Blocker, pending.Attention)
	}

	var settled coord.SettleInputResult
	f.callWith("settle_input", primarySession, `{"taskId":"t1","decision":"delivered"}`, ops).decodeResult(t, &settled)
	after := f.task(t, "t1")
	if after.Blocker != "" || after.Attention != "" {
		t.Fatalf("a delivered answer left the task waiting: blocker %q, attention %q", after.Blocker, after.Attention)
	}
	if stage := task.StageOf(after); stage != task.Running {
		t.Errorf("stage after the delivered settlement = %q, want %q", stage, task.Running)
	}
	if got := len(stub.script().Prompts); got != 1 {
		t.Errorf("settling an attempt sent something (%d prompts)", got)
	}
}

// isQuestionAttentionForTest mirrors the coordinator's question marker: a pending
// worker question is the only attention a delivered answer may close.
func isQuestionAttentionForTest(attention string) bool {
	return strings.HasPrefix(strings.TrimSpace(attention), "worker question: ")
}

// TestSettleInputRefusesWhenNoAttemptIsRecorded pins the settlements that are not
// settlements: a task with nothing pending, and a decision that is not one of the two
// the user can make.
func TestSettleInputRefusesWhenNoAttemptIsRecorded(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorkerWith(workerPane, task.Announcement{ID: workerSession}, task.SessionRef{Kind: task.SessionKindID, Value: workerSession})
	f.asPrimary()
	f.useHerdrStub(stubScript{RepoRoot: f.repoRoot})
	ops := CoordinatorOps(f.coordinator(t))

	wantError(t, f.callWith("settle_input", primarySession, `{"taskId":"t1","decision":"delivered"}`, ops), "bad_request")
	wantError(t, f.callWith("settle_input", primarySession, `{"taskId":"t1","decision":"maybe"}`, ops), "bad_request")
	// Only the primary decides what the user saw; the worker it concerns cannot.
	f.env["HERDR_PANE_ID"] = workerPane
	wantError(t, f.callWith("settle_input", workerSession, `{"taskId":"t1","decision":"delivered"}`, ops), "forbidden")
}

// TestDialogApprovalSettlesAnUnchangedDialogOnlyByInspection covers the case the
// dialog channel cannot settle by itself: the dialog is still blocked and unchanged,
// so the keys may or may not be in it, and only the user's own inspection resolves it.
func TestDialogApprovalSettlesAnUnchangedDialogOnlyByInspection(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	screen := "Choose an option:\r\n  1. Allow once\r\n  2. Deny\r\n"
	blocked := workerAgent(workerPane, workerSession, f.worktree)
	blocked.AgentStatus = "blocked"
	stub := f.useHerdrStub(stubScript{
		RepoRoot:      f.repoRoot,
		Agents:        map[string]stubAgent{workerPane: blocked},
		SendKeysError: "timeout",
	})
	ops := CoordinatorOps(f.coordinator(t))
	approve := func(digest, keys string) bridgeResponse {
		return f.callWith("approve_dialog", primarySession,
			fmt.Sprintf(`{"taskId":"t1","observedDigest":%q,"keys":%s}`, digest, keys), ops)
	}

	var preview coord.DialogPreview
	stub.queueReads(stubRead{Text: screen})
	f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)

	stub.queueReads(stubRead{Text: screen})
	wantError(t, approve(preview.Digest, `["1"]`), "ambiguous_operation")
	if got := len(stub.script().SentKeys); got != 1 {
		t.Fatalf("send-keys calls = %d, want the one ambiguous attempt", got)
	}

	// The same dialog is still up: the keys may already be in it, so a repeat is
	// refused rather than sent again.
	stub.queueReads(stubRead{Text: screen})
	wantError(t, approve(preview.Digest, `["1"]`), "input_pending")
	if got := len(stub.script().SentKeys); got != 1 {
		t.Fatalf("the repeat sent the keys again (%d send-keys calls)", got)
	}

	// The user inspected the dialog and reports the keys never arrived; that settles
	// the attempt, so the same approval can be made again deliberately.
	var settled coord.SettleInputResult
	f.callWith("settle_input", primarySession, `{"taskId":"t1","decision":"not_delivered"}`, ops).decodeResult(t, &settled)
	if settled.Channel != "dialog" {
		t.Fatalf("settlement = %+v, want the dialog attempt resolved", settled)
	}
	stub.update(func(script *stubScript) { script.SendKeysError = "" })
	stub.queueReads(stubRead{Text: screen})
	if resp := approve(preview.Digest, `["1"]`); !resp.OK {
		t.Fatalf("the deliberate re-approval failed: %+v", resp.Error)
	}
	if got := len(stub.script().SentKeys); got != 2 {
		t.Fatalf("send-keys calls after the re-approval = %d, want 2", got)
	}
	if got := f.task(t, "t1").Blocker; got != "" {
		t.Errorf("the delivered keys left an attempt behind: %q", got)
	}
}

// TestAttemptsWithTheSamePayloadStayDistinguishable pins the per-attempt identity: the
// same answer sent twice leaves two different records, so a decision the user makes about
// the first attempt cannot clear the second one - whose outcome nobody has looked at.
func TestAttemptsWithTheSamePayloadStayDistinguishable(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()
	stub := f.useHerdrStub(stubScript{
		RepoRoot:    f.repoRoot,
		Agents:      map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, f.worktree)},
		PromptError: "timeout",
	})
	ops := CoordinatorOps(f.coordinator(t))
	answer := `{"taskId":"t1","text":"Keep the old API."}`

	wantError(t, f.callWith("answer", primarySession, answer, ops), "ambiguous_operation")
	first := f.task(t, "t1").Blocker
	if !strings.Contains(first, "input attempt") {
		t.Fatalf("the first attempt was not recorded: blocker %q", first)
	}

	// The user says the first one never arrived, so the same answer is sent again - and it
	// is just as uncertain as the first was.
	f.callWith("settle_input", primarySession, `{"taskId":"t1","decision":"not_delivered"}`, ops)
	wantError(t, f.callWith("answer", primarySession, answer, ops), "ambiguous_operation")
	second := f.task(t, "t1").Blocker
	if !strings.Contains(second, "input attempt") {
		t.Fatalf("the second attempt was not recorded: blocker %q", second)
	}
	if first == second {
		t.Fatalf("the same answer sent twice recorded one attempt: %q", first)
	}
	if got := len(stub.script().Prompts); got != 2 {
		t.Fatalf("prompts = %d, want the two attempts", got)
	}

	// The second attempt is the pending one, so nothing else may be sent.
	wantError(t, f.callWith("answer", primarySession, answer, ops), "input_pending")
	if got := len(stub.script().Prompts); got != 2 {
		t.Fatalf("a repeat sent the answer again (%d prompts)", got)
	}
}

// TestAnswerDefiniteRefusalRecordsNoAttempt pins the other half: a refusal that
// proves Herdr wrote nothing leaves no pending attempt behind, so the answer can be
// sent again without inspection.
func TestAnswerDefiniteRefusalRecordsNoAttempt(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()
	stub := f.useHerdrStub(stubScript{
		RepoRoot:    f.repoRoot,
		Agents:      map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, f.worktree)},
		PromptError: "agent_not_found",
	})
	ops := CoordinatorOps(f.coordinator(t))

	if resp := f.callWith("answer", primarySession, `{"taskId":"t1","text":"Keep the old API."}`, ops); resp.OK {
		t.Fatal("a definite refusal was reported as delivered")
	}
	if got := f.task(t, "t1").Blocker; got != "" {
		t.Errorf("a definite refusal recorded an attempt: %q", got)
	}
	if got := len(stub.script().Prompts); got != 1 {
		t.Fatalf("prompts = %d, want the one refused attempt", got)
	}
}

// TestDialogApprovalAttemptIsTiedToTheApprovedDigest pins the same flow for approved
// keys: the durable attempt names the exact dialog it was approved for, a repeat is
// refused while that dialog is unchanged, and a dialog that has moved on settles the
// attempt so only a fresh approval can send again.
func TestDialogApprovalAttemptIsTiedToTheApprovedDigest(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	approvedScreen := "Choose an option:\r\n  1. Allow once\r\n  2. Deny\r\n"
	movedOn := "Choose an option:\r\n  1. Allow once\r\n  2. Deny\r\n  3. Always allow\r\n"
	blocked := workerAgent(workerPane, workerSession, f.worktree)
	blocked.AgentStatus = "blocked"
	stub := f.useHerdrStub(stubScript{
		RepoRoot:      f.repoRoot,
		Agents:        map[string]stubAgent{workerPane: blocked},
		SendKeysError: "timeout",
	})
	ops := CoordinatorOps(f.coordinator(t))
	approve := func(digest, keys string) bridgeResponse {
		return f.callWith("approve_dialog", primarySession,
			fmt.Sprintf(`{"taskId":"t1","observedDigest":%q,"keys":%s}`, digest, keys), ops)
	}

	var first coord.DialogPreview
	stub.queueReads(stubRead{Text: approvedScreen})
	f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &first)

	// The keys may already have been written into that dialog.
	stub.queueReads(stubRead{Text: approvedScreen})
	wantError(t, approve(first.Digest, `["1"]`), "ambiguous_operation")
	attempt := f.task(t, "t1").Blocker
	if !strings.Contains(attempt, "input attempt") || !strings.Contains(attempt, "dialog") {
		t.Fatalf("the key attempt was not recorded: blocker %q", attempt)
	}
	if !strings.Contains(attempt, first.Digest) {
		t.Errorf("the attempt does not name the dialog it was approved for: %q", attempt)
	}
	if got := len(stub.script().SentKeys); got != 1 {
		t.Fatalf("send-keys calls = %d, want the one ambiguous attempt", got)
	}

	// The same dialog is still up, so a repeat of the same approval must not send
	// those keys a second time.
	stub.queueReads(stubRead{Text: approvedScreen})
	wantError(t, approve(first.Digest, `["1"]`), "input_pending")
	if got := len(stub.script().SentKeys); got != 1 {
		t.Fatalf("the repeat sent the keys again (%d send-keys calls)", got)
	}

	// The screen has moved on, but the caller still approves the *original* dialog: the
	// read that would authorise a send does not match what was approved, so nothing is
	// sent - and the pending attempt is left for the user rather than cleared by a
	// refusal that proves nothing about it.
	stub.queueReads(stubRead{Text: movedOn})
	wantError(t, approve(first.Digest, `["1"]`), "dialog_changed")
	if got := len(stub.script().SentKeys); got != 1 {
		t.Fatalf("an approval for a dialog that is not on screen sent keys (%d send-keys calls)", got)
	}
	if got := f.task(t, "t1").Blocker; got == "" {
		t.Error("a refusal that proves nothing about the attempt cleared it")
	}

	// The dialog moved on, which is what the keys did: the attempt is settled, and a
	// fresh approval for the dialog that is on screen now goes through.
	var next coord.DialogPreview
	stub.queueReads(stubRead{Text: movedOn})
	f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &next)
	if next.Digest == first.Digest {
		t.Fatal("the two dialogs hash the same, so this test cannot tell them apart")
	}
	// The stand-in stops refusing, so the fresh approval is the one that lands.
	stub.update(func(script *stubScript) { script.SendKeysError = "" })
	stub.queueReads(stubRead{Text: movedOn})
	if resp := approve(next.Digest, `["2"]`); !resp.OK {
		t.Fatalf("the fresh approval failed: %+v", resp.Error)
	}
	if got := len(stub.script().SentKeys); got != 2 {
		t.Fatalf("send-keys calls after the fresh approval = %d, want 2", got)
	}
	if got := f.task(t, "t1").Blocker; got != "" {
		t.Errorf("the settled attempt is still recorded: %q", got)
	}

	// The stale approval is now refused as a changed dialog, and records nothing.
	stub.queueReads(stubRead{Text: movedOn})
	wantError(t, approve(first.Digest, `["1"]`), "dialog_changed")
	if got := len(stub.script().SentKeys); got != 2 {
		t.Fatalf("a stale approval sent keys (%d send-keys calls)", got)
	}
	if got := f.task(t, "t1").Blocker; got != "" {
		t.Errorf("a changed dialog recorded an attempt: %q", got)
	}
}

// TestDialogApprovalSettlesAnAttemptWhenTheWorkerIsUnblocked pins the one piece of
// evidence a key attempt has of its own: once the worker is no longer blocked, the dialog
// those keys were approved for is gone, which is what a consumed approval looks like. The
// attempt is settled there, so it cannot block every later approval for a worker that has
// moved on - while the call itself still refuses, because there is no dialog to approve.
func TestDialogApprovalSettlesAnAttemptWhenTheWorkerIsUnblocked(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	screen := "Choose an option:\r\n  1. Allow once\r\n  2. Deny\r\n"
	blocked := workerAgent(workerPane, workerSession, f.worktree)
	blocked.AgentStatus = "blocked"
	stub := f.useHerdrStub(stubScript{
		RepoRoot:      f.repoRoot,
		Agents:        map[string]stubAgent{workerPane: blocked},
		SendKeysError: "timeout",
	})
	ops := CoordinatorOps(f.coordinator(t))
	approve := func(digest, keys string) bridgeResponse {
		return f.callWith("approve_dialog", primarySession,
			fmt.Sprintf(`{"taskId":"t1","observedDigest":%q,"keys":%s}`, digest, keys), ops)
	}

	var preview coord.DialogPreview
	stub.queueReads(stubRead{Text: screen})
	f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
	stub.queueReads(stubRead{Text: screen})
	wantError(t, approve(preview.Digest, `["1"]`), "ambiguous_operation")
	if got := f.task(t, "t1").Blocker; got == "" {
		t.Fatal("the ambiguous approval left no attempt recorded")
	}

	// The worker leaves the dialog: the keys landed (or the dialog was dismissed), which
	// settles the attempt, and the approval itself is still refused.
	stub.setAgent(workerPane, workerAgent(workerPane, workerSession, f.worktree))
	wantError(t, approve(preview.Digest, `["1"]`), "not_blocked")
	if got := f.task(t, "t1").Blocker; got != "" {
		t.Errorf("the attempt survived a worker that is no longer blocked: %q", got)
	}
	if got := len(stub.script().SentKeys); got != 1 {
		t.Errorf("a refused approval sent keys (%d send-keys calls)", got)
	}
}

// TestDialogApprovalRevalidatesTheApprovedScreen pins the approval path: the
// preview reads the visible screen passively, and the approval is refused unless
// the pane, the session and the dialog digest are all still the approved ones.
func TestDialogApprovalRevalidatesTheApprovedScreen(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	approved := "Choose an option:\r\n  1. Allow once\r\n  2. Deny\r\n"
	changed := "Choose an option:\r\n  1. Allow once\r\n  2. Deny\r\n  3. Always allow\r\n"
	blocked := workerAgent(workerPane, workerSession, f.worktree)
	blocked.AgentStatus = "blocked"
	stub := f.useHerdrStub(stubScript{
		RepoRoot: f.repoRoot,
		Agents:   map[string]stubAgent{workerPane: blocked},
		Reads:    []stubRead{{Text: approved}},
	})
	ops := CoordinatorOps(f.coordinator(t))

	var preview coord.DialogPreview
	f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
	if !preview.Blocked || preview.Digest == "" || !strings.Contains(preview.Dialog, "Allow once") {
		t.Fatalf("preview = %+v, want the on-screen dialog and its digest", preview)
	}
	// The preview is passive: it reads the visible screen, which is the source
	// that works while a full-screen OMP dialog is up.
	if !stub.called("agent", "read", workerPane, "--source", "visible") {
		t.Errorf("the preview did not read the visible source: %v", stub.calls())
	}
	for _, call := range stub.calls() {
		if len(call) > 1 && call[0] == "agent" && call[1] == "read" && slices.Contains(call, "recent-unwrapped") {
			t.Errorf("the preview used an intrusive read source: %q", call)
		}
	}

	approve := func(digest string, keys string) bridgeResponse {
		return f.callWith("approve_dialog", primarySession,
			fmt.Sprintf(`{"taskId":"t1","observedDigest":%q,"keys":%s}`, digest, keys), ops)
	}

	t.Run("the approved keys are sent exactly", func(t *testing.T) {
		stub.queueReads(stubRead{Text: approved})
		if resp := approve(preview.Digest, `["1"]`); !resp.OK {
			t.Fatalf("approve failed: %+v", resp.Error)
		}
		sent := stub.script().SentKeys
		if len(sent) != 1 || sent[0].Pane != workerPane || !slices.Equal(sent[0].Keys, []string{"1"}) {
			t.Fatalf("sent keys = %+v, want exactly [1] on %q", sent, workerPane)
		}
		if !stub.called("agent", "send-keys", workerPane, "1") {
			t.Errorf("the approval did not send the approved key: %v", stub.calls())
		}
	})

	t.Run("a stale digest sends nothing", func(t *testing.T) {
		stub.queueReads(stubRead{Text: approved})
		before := len(stub.script().SentKeys)
		wantError(t, approve("deadbeefdeadbeef", `["1"]`), "dialog_changed")
		if got := len(stub.script().SentKeys); got != before {
			t.Errorf("a stale approval still sent keys (%d > %d)", got, before)
		}
	})

	t.Run("a changed dialog sends nothing", func(t *testing.T) {
		stub.queueReads(stubRead{Text: changed})
		before := len(stub.script().SentKeys)
		wantError(t, approve(preview.Digest, `["1"]`), "dialog_changed")
		if got := len(stub.script().SentKeys); got != before {
			t.Errorf("a changed dialog still sent keys (%d > %d)", got, before)
		}
	})

	t.Run("an unreadable dialog sends nothing", func(t *testing.T) {
		stub.queueReads(stubRead{ErrorCode: "agent_not_idle", ErrorText: "the agent is not idle"})
		before := len(stub.script().SentKeys)
		wantError(t, approve(preview.Digest, `["1"]`), "dialog_changed")
		if got := len(stub.script().SentKeys); got != before {
			t.Errorf("an unreadable dialog still sent keys (%d > %d)", got, before)
		}
	})

	t.Run("a changed session sends nothing", func(t *testing.T) {
		stub.queueReads(stubRead{Text: approved})
		stub.setAgent(workerPane, workerAgent(workerPane, "omp-someone-else", f.worktree))
		defer stub.setAgent(workerPane, blocked)
		before := len(stub.script().SentKeys)
		wantError(t, approve(preview.Digest, `["1"]`), "agent_changed")
		if got := len(stub.script().SentKeys); got != before {
			t.Errorf("a changed session still sent keys (%d > %d)", got, before)
		}
	})

	t.Run("a worker that is not blocked is refused", func(t *testing.T) {
		stub.queueReads(stubRead{Text: approved})
		stub.setAgent(workerPane, workerAgent(workerPane, workerSession, f.worktree))
		defer stub.setAgent(workerPane, blocked)
		before := len(stub.script().SentKeys)
		wantError(t, approve(preview.Digest, `["1"]`), "not_blocked")
		if got := len(stub.script().SentKeys); got != before {
			t.Errorf("a worker that was not blocked still received keys (%d > %d)", got, before)
		}
	})

	t.Run("an approval without a digest is refused", func(t *testing.T) {
		wantError(t, approve("", `["1"]`), "bad_request")
		wantError(t, approve(preview.Digest, `[]`), "bad_request")
	})
}
func TestDialogApprovalIgnoresVolatileScreenOutsideActiveDialog(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	activeDialog := func(question string) string {
		return strings.Join([]string{
			"╭─ Ask",
			"│ " + question,
			"├─ choices",
			"│ keep the current contract",
			"│ change the contract",
			"│ Enter select · n note · ↑/↓ move · Esc cancel",
			"╰─",
		}, "\n")
	}
	screen := func(status, question string) string {
		panelLines := strings.Split(activeDialog(question), "\n")
		for i := range panelLines {
			panelLines[i] = "│ " + panelLines[i] + " │"
		}
		return strings.Join([]string{
			"previous output changed at " + status,
			strings.Join(panelLines, "\n"),
			"terminal status " + status,
		}, "\n")
	}
	blocked := workerAgent(workerPane, workerSession, f.worktree)
	blocked.AgentStatus = "blocked"
	stub := f.useHerdrStub(stubScript{
		RepoRoot: f.repoRoot,
		Agents:   map[string]stubAgent{workerPane: blocked},
	})
	ops := CoordinatorOps(f.coordinator(t))
	approve := func(text string, digest string) bridgeResponse {
		stub.queueReads(stubRead{Text: text})
		return f.callWith("approve_dialog", primarySession,
			fmt.Sprintf(`{"taskId":"t1","observedDigest":%q,"keys":["enter"]}`, digest), ops)
	}

	stub.queueReads(stubRead{Text: screen("00:00:01", "ACCEPT-Q-DIALOG: which contract should b.js use?")})
	var preview coord.DialogPreview
	f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
	if strings.Contains(preview.Dialog, "previous output") || strings.Contains(preview.Dialog, "terminal status") {
		t.Fatalf("preview included volatile content outside the active dialog: %q", preview.Dialog)
	}
	if resp := approve(screen("00:00:02", "ACCEPT-Q-DIALOG: which contract should b.js use?"), preview.Digest); !resp.OK {
		t.Fatalf("unchanged active dialog was refused after surrounding output changed: %+v", resp.Error)
	}
	if got := stub.script().SentKeys; len(got) != 1 || got[0].Pane != workerPane || !slices.Equal(got[0].Keys, []string{"enter"}) {
		t.Fatalf("sent keys = %+v, want exactly [enter] on %q", got, workerPane)
	}
	wantError(t, approve(screen("00:00:03", "ACCEPT-Q-DIALOG: which namespace should b.js use?"), preview.Digest), "dialog_changed")
	if got := len(stub.script().SentKeys); got != 1 {
		t.Fatalf("changed active question sent keys again (%d send-keys calls)", got)
	}
}

// TestDialogAndCleanupAcceptTheSessionFormsHerdrReallyReports reproduces the Run-2
// failure: a build reports the session file while the record kept the announced
// id (or the other way round). The two values name one running OMP session, and
// only the announcement of the pane's handshake knows both, so relay and cleanup
// must decide identity through it instead of comparing the two raw strings.
func TestDialogAndCleanupAcceptTheSessionFormsHerdrReallyReports(t *testing.T) {
	t.Run("the recorded id and Herdr's path are one session", func(t *testing.T) {
		f := newBridgeFixture(t)
		f.seedPrimary()
		f.seedWorker("t1", workerPane, workerPane, workerSession)
		// The extension announced both forms; the coordinator confirmed the path.
		f.asWorkerWith(workerPane, task.Announcement{ID: workerSession, File: workerSessionFile},
			task.SessionRef{Kind: task.SessionKindPath, Value: workerSessionFile})
		f.asPrimary()
		stub := f.useHerdrStub(stubScript{
			RepoRoot: f.repoRoot,
			Agents:   map[string]stubAgent{workerPane: workerAgentOn(workerPane, task.SessionKindPath, workerSessionFile, f.worktree)},
		})
		stub.queueReads(stubRead{Text: "  1. Allow once\n  2. Deny\n"})
		ops := CoordinatorOps(f.coordinator(t))

		var preview coord.DialogPreview
		f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
		if preview.TaskID != "t1" || preview.PublicPaneID != workerPane {
			t.Fatalf("preview = %+v, want the worker the record names", preview)
		}
	})

	t.Run("the recorded path and Herdr's id are one session", func(t *testing.T) {
		f := newBridgeFixture(t)
		f.seedPrimary()
		f.seedWorker("t1", workerPane, workerPane, workerSession)
		f.recordSession(t, "t1", workerSessionFile)
		f.asWorkerWith(workerPane, task.Announcement{ID: workerSession, File: workerSessionFile},
			task.SessionRef{Kind: task.SessionKindID, Value: workerSession})
		f.asPrimary()
		stub := f.useHerdrStub(stubScript{
			RepoRoot: f.repoRoot,
			Agents:   map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, f.worktree)},
		})
		stub.queueReads(stubRead{Text: "  1. Allow once\n  2. Deny\n"})
		ops := CoordinatorOps(f.coordinator(t))

		var preview coord.DialogPreview
		f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
		if preview.TaskID != "t1" {
			t.Fatalf("preview = %+v, want the worker the record names", preview)
		}
	})

	t.Run("liveness is decided across the forms too", func(t *testing.T) {
		f := newBridgeFixture(t)
		f.seedPrimary()
		worker := f.seedWorker("t1", workerPane, workerPane, workerSession)
		f.asWorker()
		f.asPrimary()
		root, checkout := bridgeRepoWithWorktree(t, worker.Branch)
		if _, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
			next.WorktreePath = checkout
			return nil
		}); err != nil {
			t.Fatalf("record the checkout: %v", err)
		}
		f.useHerdrStub(stubScript{
			RepoRoot: root,
			Agents:   map[string]stubAgent{workerPane: workerAgentOn(workerPane, task.SessionKindPath, workerSessionFile, checkout)},
			Worktree: []stubWorktree{{Path: checkout, Branch: worker.Branch, OpenWorkspaceID: workerWorktree}},
		})
		ops := CoordinatorOps(f.coordinator(t))
		f.approve(t, ops)

		// The build that publishes the session file, recorded after the review so
		// nothing rewrites the handshake: the task keeps the announced id, Herdr
		// reports the path, and the two are one session.
		f.confirmRef(workerPane, task.Announcement{ID: workerSession, File: workerSessionFile},
			task.SessionRef{Kind: task.SessionKindPath, Value: workerSessionFile})

		// The worker is identified, so its liveness is the answer — not "cannot
		// verify which session this is".
		wantError(t, f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops), "worker_live")
	})

	t.Run("an unverified handshake cannot be trusted", func(t *testing.T) {
		f := newBridgeFixture(t)
		f.seedPrimary()
		f.seedWorker("t1", workerPane, workerPane, workerSession)
		if err := f.store.RecordReady(context.Background(), workerPane,
			task.Announcement{ID: workerSession, File: workerSessionFile}, 1); err != nil {
			t.Fatalf("RecordReady: %v", err)
		}
		f.confirm(primaryPane, primarySession)
		f.env["HERDR_PANE_ID"] = primaryPane
		f.useHerdrStub(stubScript{
			RepoRoot: f.repoRoot,
			Agents:   map[string]stubAgent{workerPane: workerAgentOn(workerPane, task.SessionKindPath, workerSessionFile, f.worktree)},
		})
		ops := CoordinatorOps(f.coordinator(t))

		// Only the coordinator's comparison with `agent get` may confirm a
		// handshake, and until it has, the announcement proves nothing.
		resp := f.callWith("dialog_preview", primarySession, `{"taskId":"t1"}`, ops)
		wantError(t, resp, "agent_changed")
		if !strings.Contains(resp.Error.Message, "verified") {
			t.Errorf("the refusal does not say the handshake is unverified: %q", resp.Error.Message)
		}
	})
}

// TestCleanupRequiresReviewedDoneAndACleanProvenanceCheckout drives the whole
// cleanup gate: nothing is refused more weakly than the acceptance requires, and
// a removal is never forced, never touches the branch and never happens twice.
func TestCleanupRequiresReviewedDoneAndACleanProvenanceCheckout(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	worker := f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	// A real checkout, so clean and dirty are decided by git itself.
	root, checkout := bridgeRepoWithWorktree(t, worker.Branch)
	if _, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
		next.WorktreePath = checkout
		return nil
	}); err != nil {
		t.Fatalf("point the task at a real checkout: %v", err)
	}
	stub := f.useHerdrStub(stubScript{
		RepoRoot: root,
		Agents:   map[string]stubAgent{workerPane: workerAgent(workerPane, workerSession, checkout)},
		Worktree: []stubWorktree{{Path: checkout, Branch: worker.Branch, OpenWorkspaceID: workerWorktree}},
	})
	ops := CoordinatorOps(f.coordinator(t))

	var preview coord.CleanupPreview
	f.callWith("cleanup_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
	if preview.Reviewed || preview.Removable {
		t.Fatalf("preview of an unreviewed task = %+v, want it refused", preview)
	}
	if preview.Path != checkout || preview.Branch != worker.Branch {
		t.Errorf("preview = %+v, want the recorded path and branch", preview)
	}
	wantError(t, f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops), "not_reviewed")
	if got := stub.script().Removed; len(got) != 0 {
		t.Fatalf("cleanup removed a checkout the user never reviewed: %v", got)
	}

	// Review it the way the user does: a final result, then a disposition. The
	// report is the worker's own call, so the process is its pane for that one.
	f.asWorker()
	if resp := f.callWith("report", workerSession, f.reportPayload(t, "t1", "result"), ops); !resp.OK {
		t.Fatalf("report failed: %+v", resp.Error)
	}
	f.asPrimary()
	if resp := f.callWith("review", primarySession, f.reviewPayload(t, ops, "t1", "accepted"), ops); !resp.OK {
		t.Fatalf("review failed: %+v", resp.Error)
	}

	t.Run("a live worker refuses cleanup", func(t *testing.T) {
		f.callWith("cleanup_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
		if !preview.LiveWorker || preview.Removable {
			t.Fatalf("preview with a live worker = %+v", preview)
		}
		wantError(t, f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops), "worker_live")
		if got := stub.script().Removed; len(got) != 0 {
			t.Fatalf("cleanup removed a checkout under a live worker: %v", got)
		}
		assertCheckoutNotOpened(t, stub, f.repoRoot, checkout)
	})

	// The worker exits; its checkout is still not clean.
	stub.dropAgent(workerPane)
	if err := os.WriteFile(filepath.Join(checkout, "notes.txt"), []byte("still mine\n"), 0o644); err != nil {
		t.Fatalf("write into the checkout: %v", err)
	}
	t.Run("a dirty checkout refuses cleanup", func(t *testing.T) {
		f.callWith("cleanup_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
		if preview.Clean || preview.Removable {
			t.Fatalf("preview of a dirty checkout = %+v", preview)
		}
		wantError(t, f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops), "checkout_dirty")
		if got := stub.script().Removed; len(got) != 0 {
			t.Fatalf("cleanup removed a dirty checkout: %v", got)
		}
		assertCheckoutNotOpened(t, stub, f.repoRoot, checkout)
	})

	if err := os.Remove(filepath.Join(checkout, "notes.txt")); err != nil {
		t.Fatalf("clean the checkout: %v", err)
	}

	t.Run("a reviewed clean checkout is removed without force", func(t *testing.T) {
		f.callWith("cleanup_preview", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &preview)
		if !preview.Removable || preview.WorkspaceID != workerWorktree {
			t.Fatalf("preview of a removable checkout = %+v", preview)
		}
		reviewed := f.task(t, "t1")

		var result coord.CleanupResult
		f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &result)
		if !result.Removed || result.WorkspaceID != workerWorktree || result.Path != checkout {
			t.Fatalf("cleanup = %+v", result)
		}
		if stub.forced() {
			t.Error("cleanup asked Herdr to force the removal")
		}
		if !stub.called("worktree", "remove", "--workspace", workerWorktree) {
			t.Errorf("cleanup did not remove the verified workspace: %v", stub.calls())
		}
		if got := stub.script().Removed; len(got) != 1 {
			t.Fatalf("removals = %v, want exactly one", got)
		}

		// The branch is preserved: cleanup never deletes it.
		if branches := bridgeGit(t, root, "branch", "--list", worker.Branch); !strings.Contains(branches, worker.Branch) {
			t.Errorf("cleanup deleted the branch: %q", branches)
		}
		// Herdr no longer holds the checkout it removed, and nothing ever asked
		// git to touch the branch in any way.
		if listed := stub.script().Worktree; len(listed) != 0 {
			t.Errorf("the removed checkout is still listed: %+v", listed)
		}
		for _, call := range stub.calls() {
			if slices.Contains(call, "branch") || slices.Contains(call, "-D") || slices.Contains(call, "-d") {
				t.Errorf("cleanup touched a branch: %q", call)
			}
		}
		if result.Forced {
			t.Error("Forced = true, want the removal the plugin asked for (never --force)")
		}

		// Every live routing and runtime identity is gone: the checkout no longer
		// exists, so nothing may still point at a pane, workspace or agent.
		after := f.task(t, "t1")
		for _, live := range []struct{ name, value string }{
			{"launch pane", after.LaunchPaneID},
			{"public pane", after.PublicPaneID},
			{"terminal", after.TerminalID},
			{"worktree workspace", after.WorktreeWorkspaceID},
			{"current workspace", after.CurrentWorkspaceID},
			{"agent name", after.AgentName},
			{"OMP session", after.OMPSessionID},
		} {
			if live.value != "" {
				t.Errorf("the task still records its %s: %q", live.name, live.value)
			}
		}

		// The reviewed report, the user's decision and the provenance are history:
		// cleanup may not rewrite any of them.
		if after.Branch != reviewed.Branch || after.WorktreePath != reviewed.WorktreePath {
			t.Errorf("cleanup changed the provenance: branch %q -> %q, path %q -> %q",
				reviewed.Branch, after.Branch, reviewed.WorktreePath, after.WorktreePath)
		}
		if after.Disposition != reviewed.Disposition || after.Disposition == "" {
			t.Errorf("cleanup changed the user's disposition: %q -> %q", reviewed.Disposition, after.Disposition)
		}
		if !sameReportText(after.Report, reviewed.Report) || after.Report == nil {
			t.Errorf("cleanup changed the reviewed report: %+v -> %+v", reviewed.Report, after.Report)
		}
		if task.StageOf(after) != task.Done {
			t.Errorf("stage after cleanup = %q, want %q", task.StageOf(after), task.Done)
		}
		if got := notesOf(t, f, "cleanup"); len(got) != 1 {
			t.Errorf("cleanup notes = %d, want 1 durable entry", len(got))
		}
	})

	t.Run("a second cleanup removes nothing more", func(t *testing.T) {
		// The task no longer names a worker or a workspace, so a repeat is refused
		// before it can look for anything to remove.
		resp := f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops)
		wantError(t, resp, "agent_changed")
		if !strings.Contains(resp.Error.Message, "no launch pane") {
			t.Errorf("the refusal does not say the task names no worker: %q", resp.Error.Message)
		}
		if got := stub.script().Removed; len(got) != 1 {
			t.Fatalf("a repeated cleanup removed again: %v", got)
		}
	})
}

// TestCleanupRefusesACheckoutHeldInAnUnknownWorkspace keeps the provenance gate
// honest: a checkout Herdr holds in a workspace this task never recorded is
// somebody else's, and cleanup refuses instead of guessing by label.
func TestCleanupRefusesACheckoutHeldInAnUnknownWorkspace(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	worker := f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	root, checkout := bridgeRepoWithWorktree(t, worker.Branch)
	if _, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
		next.WorktreePath = checkout
		return nil
	}); err != nil {
		t.Fatalf("record the checkout: %v", err)
	}
	stub := f.useHerdrStub(stubScript{
		RepoRoot: root,
		Worktree: []stubWorktree{{Path: checkout, Branch: worker.Branch, OpenWorkspaceID: "w4"}},
	})
	ops := CoordinatorOps(f.coordinator(t))
	f.approve(t, ops)

	wantError(t, f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops), "provenance_unconfirmed")
	if got := stub.script().Removed; len(got) != 0 {
		t.Fatalf("cleanup removed a checkout Herdr holds elsewhere: %v", got)
	}
	if after := f.task(t, "t1"); after.WorktreePath != checkout || after.WorktreeWorkspaceID == "" {
		t.Errorf("a refused cleanup changed the task: %+v", after)
	}
}

// TestCleanupRemovesNothingWhenTheOpenResponseContradictsItself pins the other
// half of the recovery: the answer that authorises a removal has to describe the
// checkout that was opened. A response that names the requested checkout in one
// field and a different one in another refuses cleanup, and no removal is
// attempted at all.
func TestCleanupRemovesNothingWhenTheOpenResponseContradictsItself(t *testing.T) {
	for _, contradiction := range []string{"checkout_path", "repo_root", "worktree_path"} {
		t.Run(contradiction, func(t *testing.T) {
			f := newBridgeFixture(t)
			f.seedPrimary()
			worker := f.seedWorker("t1", workerPane, workerPane, workerSession)
			f.asWorker()
			f.asPrimary()

			root, checkout := bridgeRepoWithWorktree(t, worker.Branch)
			if _, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
				next.WorktreePath = checkout
				next.CurrentWorkspaceID = movedWorkspace
				return nil
			}); err != nil {
				t.Fatalf("record the move: %v", err)
			}
			stub := f.useHerdrStub(stubScript{
				RepoRoot:          root,
				Worktree:          []stubWorktree{{Path: filepath.ToSlash(checkout), Branch: worker.Branch, OpenWorkspaceID: movedWorkspace}},
				NotLinked:         []string{movedWorkspace},
				OpenContradiction: contradiction,
			})
			ops := CoordinatorOps(f.coordinator(t))
			f.approve(t, ops)

			wantError(t, f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops), "provenance_unconfirmed")
			if got := stub.script().Removed; len(got) != 0 {
				t.Fatalf("a contradictory open response still removed a checkout: %v", got)
			}
			open := stub.callIndex("worktree", "open", "--cwd", f.repoRoot, "--path", checkout, "--no-focus")
			if open < 0 {
				t.Fatalf("cleanup never opened the recorded checkout: %v", stub.calls())
			}
			if stub.lastCallIndex("worktree", "remove", "--workspace", movedWorkspace) > open {
				t.Errorf("a removal was attempted after a contradictory open response: %v", stub.calls())
			}
			// Nothing about the failed recovery may be recorded as removed.
			after := f.task(t, "t1")
			if after.WorktreePath != checkout || after.WorktreeWorkspaceID == "" || after.LaunchPaneID == "" {
				t.Errorf("a refused cleanup changed the task: %+v", after)
			}
		})
	}
}

// sameReportText reports whether two recorded reports say exactly the same thing,
// field by field, which is how "cleanup did not rewrite the review" is asserted
// without depending on pointer identity.
func sameReportText(a, b *task.Report) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind && a.Summary == b.Summary && a.Branch == b.Branch && a.Risk == b.Risk &&
		slices.Equal(a.Evidence, b.Evidence) && slices.Equal(a.Checks, b.Checks)
}

// TestCleanupRefusesARemovalHerdrClaimsItForced pins the other half of the
// Forced contract: this plugin never asks for a forced removal, so an answer
// claiming one is refused rather than reported as a successful cleanup. Herdr
// says it removed the checkout by force, so the task must keep its identities and
// nothing may be recorded as removed.
func TestCleanupRefusesARemovalHerdrClaimsItForced(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	worker := f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	root, checkout := bridgeRepoWithWorktree(t, worker.Branch)
	if _, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
		next.WorktreePath = checkout
		return nil
	}); err != nil {
		t.Fatalf("record the checkout: %v", err)
	}
	stub := f.useHerdrStub(stubScript{
		RepoRoot:           root,
		Worktree:           []stubWorktree{{Path: checkout, Branch: worker.Branch, OpenWorkspaceID: workerWorktree}},
		RemoveEchoesForced: true,
	})
	ops := CoordinatorOps(f.coordinator(t))
	f.approve(t, ops)
	before := f.task(t, "t1")

	wantError(t, f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops), "provenance_unconfirmed")
	if stub.forced() {
		t.Error("cleanup asked Herdr to force the removal")
	}
	after := f.task(t, "t1")
	if after.WorktreeWorkspaceID != before.WorktreeWorkspaceID || after.LaunchPaneID != before.LaunchPaneID || after.AgentName != before.AgentName {
		t.Errorf("a refused forced removal changed the task: %+v", after)
	}
	if got := notesOf(t, f, "cleanup"); len(got) != 0 {
		t.Errorf("a refused forced removal left %d cleanup notes: %+v", len(got), got)
	}
	if task.StageOf(after) != task.Done {
		t.Errorf("stage after the refused removal = %q, want the disposition to still close the task", task.StageOf(after))
	}
}

// assertCheckoutNotOpened pins the ordering the cleanup gate requires: the
// live-worker and dirty-checkout refusals happen before anything is opened, so a
// refused cleanup has no side effect to undo.
func assertCheckoutNotOpened(t *testing.T, stub *herdrStub, root, checkout string) {
	t.Helper()
	if stub.called("worktree", "open", "--cwd", root, "--path", checkout, "--no-focus") {
		t.Errorf("a refused cleanup still opened the checkout: %v", stub.calls())
	}
}

// approve records the worker's final report and the primary's disposition for it,
// which is the state cleanup requires before it may touch a checkout.
func (f *bridgeFixture) approve(t *testing.T, ops Ops) {
	t.Helper()
	f.asWorker()
	if resp := f.callWith("report", workerSession, f.reportPayload(t, "t1", "result"), ops); !resp.OK {
		t.Fatalf("report failed: %+v", resp.Error)
	}
	f.asPrimary()
	if resp := f.callWith("review", primarySession, f.reviewPayload(t, ops, "t1", "accepted"), ops); !resp.OK {
		t.Fatalf("review failed: %+v", resp.Error)
	}
}

// movedWorkspace is the ordinary workspace a pane move puts a worker in, which is
// not the checkout's Herdr-managed worktree workspace.
const movedWorkspace = "wS"

// TestCleanupRecoversAMovedWorkerCheckout reproduces the production failure: the
// worker's pane was moved into an ordinary workspace, that move closed the
// worktree's own workspace, Herdr still lists the exact checkout but refuses to
// remove the workspace the pane now lives in (`not_linked_worktree`).
//
// Cleanup must recover by opening the *recorded* checkout — its project root and
// its exact path, never a label — and then remove the workspace Herdr reports for
// it, without forcing anything and without touching the branch.
func TestCleanupRecoversAMovedWorkerCheckout(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	worker := f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asWorker()
	f.asPrimary()

	root, checkout := bridgeRepoWithWorktree(t, worker.Branch)
	// The record a move leaves behind: the pane lives in the workspace Herdr moved
	// it to, while the worktree workspace it was created in is closed.
	if _, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
		next.WorktreePath = checkout
		next.CurrentWorkspaceID = movedWorkspace
		return nil
	}); err != nil {
		t.Fatalf("record the move: %v", err)
	}
	stub := f.useHerdrStub(stubScript{
		RepoRoot:  root,
		Worktree:  []stubWorktree{{Path: checkout, Branch: worker.Branch, OpenWorkspaceID: movedWorkspace}},
		NotLinked: []string{movedWorkspace},
	})
	ops := CoordinatorOps(f.coordinator(t))
	f.approve(t, ops)

	var result coord.CleanupResult
	f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &result)
	if !result.Removed || result.Path != checkout {
		t.Fatalf("cleanup = %+v, want the recorded checkout %s removed", result, checkout)
	}
	if result.Forced {
		t.Error("Forced = true, want the removal the plugin asked for (never --force)")
	}

	// The open names the *recorded* checkout, not the listing's spelling of it.
	open := []string{"worktree", "open", "--cwd", f.repoRoot, "--path", checkout, "--no-focus"}
	if !stub.called(open...) {
		t.Fatalf("cleanup did not open the recorded checkout: %v", stub.calls())
	}
	// The refused attempt came first, then the open, then the removal that
	// succeeded: the retry is what the recovery is for.
	remove := []string{"worktree", "remove", "--workspace", movedWorkspace}
	openAt := stub.callIndex(open...)
	if stub.callIndex(remove...) > openAt {
		t.Errorf("the refusal was never attempted before the open: %v", stub.calls())
	}
	if stub.lastCallIndex(remove...) < openAt {
		t.Errorf("the removal was not retried after the checkout was re-opened: %v", stub.calls())
	}
	if stub.forced() {
		t.Error("cleanup asked Herdr to force the removal")
	}
	if branches := bridgeGit(t, root, "branch", "--list", worker.Branch); !strings.Contains(branches, worker.Branch) {
		t.Errorf("cleanup deleted the branch: %q", branches)
	}
}

// TestCleanupOpensACheckoutNoWorkspaceHolds covers the other half of the same
// state: Herdr lists the recorded checkout but names no workspace holding it, so
// the removal has to begin by opening that exact checkout.
func TestCleanupOpensACheckoutNoWorkspaceHolds(t *testing.T) {
	f := newBridgeFixture(t)
	f.seedPrimary()
	worker := f.seedWorker("t1", workerPane, workerPane, workerSession)
	f.asPrimary()

	root, checkout := bridgeRepoWithWorktree(t, worker.Branch)
	if _, err := f.store.UpdateTask(context.Background(), worker.ID, worker.Revision, func(next *task.Task) error {
		next.WorktreePath = checkout
		return nil
	}); err != nil {
		t.Fatalf("record the checkout: %v", err)
	}
	stub := f.useHerdrStub(stubScript{
		RepoRoot: root,
		// No open_workspace_id: the workspace that held this checkout is gone.
		Worktree:      []stubWorktree{{Path: checkout, Branch: worker.Branch}},
		OpenWorkspace: "wT",
	})
	ops := CoordinatorOps(f.coordinator(t))
	f.approve(t, ops)

	var result coord.CleanupResult
	f.callWith("cleanup", primarySession, `{"taskId":"t1"}`, ops).decodeResult(t, &result)
	if !result.Removed || result.Path != checkout || result.WorkspaceID != "wT" {
		t.Fatalf("cleanup = %+v, want the recorded checkout removed from the re-opened workspace", result)
	}
	if result.Forced {
		t.Error("Forced = true, want the removal the plugin asked for (never --force)")
	}

	open := []string{"worktree", "open", "--cwd", f.repoRoot, "--path", checkout, "--no-focus"}
	if !stub.called(open...) {
		t.Fatalf("cleanup did not open the recorded checkout: %v", stub.calls())
	}
	remove := []string{"worktree", "remove", "--workspace", "wT"}
	if stub.callIndex(remove...) < stub.callIndex(open...) {
		t.Errorf("the removal was attempted before the checkout was re-opened: %v", stub.calls())
	}
	if stub.forced() {
		t.Error("cleanup asked Herdr to force the removal")
	}
	if branches := bridgeGit(t, root, "branch", "--list", worker.Branch); !strings.Contains(branches, worker.Branch) {
		t.Errorf("cleanup deleted the branch: %q", branches)
	}
}

// bridgeRepoWithWorktree creates a real repository with a real linked checkout,
// so a checkout's clean and dirty states are decided by git itself.
func bridgeRepoWithWorktree(t *testing.T, branch string) (root, checkout string) {
	t.Helper()
	root = t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "orchestrator@example.invalid"},
		{"config", "user.name", "Orchestrator Test"},
		{"config", "core.autocrlf", "false"},
	} {
		bridgeGit(t, root, args...)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("first\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	bridgeGit(t, root, "add", "-A")
	bridgeGit(t, root, "commit", "-m", "first commit")
	checkout = filepath.Join(t.TempDir(), "checkout")
	bridgeGit(t, root, "worktree", "add", "-b", branch, checkout, "HEAD")
	return root, checkout
}

func bridgeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v: %s", dir, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestEveryMethodTheExtensionCallsIsDispatched pins the extension-to-bridge seam
// from the bridge's side: a tool that calls a method this build does not dispatch
// would answer `unsupported_method` in the user's session, and nothing else would
// catch it, because the extension is not part of this binary.
func TestEveryMethodTheExtensionCallsIsDispatched(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "omp", "herdr-orchestrator.ts"))
	if err != nil {
		t.Fatalf("read the extension source: %v", err)
	}
	called := map[string]int{}
	for _, match := range callBridgePattern.FindAllStringSubmatch(string(source), -1) {
		called[match[1]]++
	}
	if len(called) < 10 {
		t.Fatalf("the extension calls %d bridge methods (%v), want every tool's method", len(called), called)
	}

	f := newBridgeFixture(t)
	for method := range called {
		// No binding exists for this session, so every method stops at
		// authorization — which is exactly how an unknown method would *not*
		// stop, and the only thing this test needs to distinguish.
		resp := f.call(method, "omp-unbound", `{}`)
		if !resp.OK && resp.Error.Code == CodeUnsupportedMethod {
			t.Errorf("the extension calls %q, which this bridge does not dispatch", method)
		}
	}
	for _, method := range []string{
		MethodDispatch, MethodReport,
		MethodAnswer, MethodDialogPreview, MethodApproveDialog, MethodReportPreview,
		MethodReview, MethodCleanupPreview, MethodCleanup,
	} {
		if called[method] == 0 {
			t.Errorf("no extension tool calls %q, so that capability is unreachable", method)
		}
	}
}

var callBridgePattern = regexp.MustCompile(`callBridge\(\s*"([a-z_]+)"`)
