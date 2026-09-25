package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

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
)

var (
	cmdBinaryDir string
	buildOnce    sync.Once
	builtBridge  string
	buildErr     error
)

func TestMain(m *testing.M) {
	if os.Getenv(fixturePaneFile) != "" || os.Getenv(fixtureErrFile) != "" {
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
	if len(os.Args) < 3 || os.Args[1] != "pane" || os.Args[2] != "get" {
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
// after comparing the announced session with `agent get`.
func (f *bridgeFixture) confirm(launchPane, session string) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.store.RecordReady(ctx, launchPane, session, 1); err != nil {
		f.t.Fatalf("RecordReady: %v", err)
	}
	if err := f.store.ConfirmReady(ctx, launchPane, session); err != nil {
		f.t.Fatalf("ConfirmReady: %v", err)
	}
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
	ready, err := f.store.Ready(context.Background(), workerPane, "omp-fresh-1")
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
	ready, err = f.store.Ready(context.Background(), workerPane, "omp-fresh-1")
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
	ready, err := f.store.Ready(context.Background(), primaryPane, "omp-fresh-primary")
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
	if err := f.store.RecordReady(context.Background(), workerPane, workerSession, 1); err != nil {
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

	ready, err := f.store.Ready(context.Background(), workerPane, workerSession)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Error("ready must stay unverified until the coordinator confirms it against `agent get`")
	}
	if err := f.store.ConfirmReady(context.Background(), workerPane, workerSession); err != nil {
		t.Fatalf("ConfirmReady: %v", err)
	}
	if ready, err = f.store.Ready(context.Background(), workerPane, workerSession); err != nil || !ready {
		t.Fatalf("Ready after confirm = (%v, %v)", ready, err)
	}

	// An unrelated session records nothing.
	wantError(t, f.call("ready", "omp-ghost", "{}"), CodeNotBound)

	// A session bound to another launch pane records nothing either.
	f.env["HERDR_PANE_ID"] = "w9:p9"
	wantError(t, f.call("ready", workerSession, "{}"), CodeLaunchPaneMismatch)
	if ready, err := f.store.Ready(context.Background(), "w9:p9", workerSession); err != nil || ready {
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
