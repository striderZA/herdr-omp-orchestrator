package main

// Task 6: the hook routes are the only surface Herdr and the OMP extension call
// directly, so their refusals are exercised without a coordinator, a state
// directory or a Herdr binary: every one of these paths returns before anything
// is opened.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/setup"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

// routeFixtureEnv turns this test binary into a stand-in Herdr CLI for the
// board routes, so the action's own argv is asserted on a real child process
// rather than on a mock.
const (
	routeFixtureEnv   = "ORCH_HERDR_ROUTE_FIXTURE"
	routeFixtureCalls = "ORCH_HERDR_ROUTE_FIXTURE_CALLS"
	// routeFixtureStatus is the text `herdr integration status` prints.
	routeFixtureStatus = "ORCH_HERDR_ROUTE_FIXTURE_STATUS"
)

func TestMain(m *testing.M) {
	if os.Getenv(routeFixtureEnv) == "1" {
		os.Exit(routeFixtureMain())
	}
	os.Exit(m.Run())
}

func routeFixtureMain() int {
	calls := os.Getenv(routeFixtureCalls)
	if calls == "" {
		return 30
	}
	line, err := json.Marshal(map[string]any{"argv": os.Args[1:]})
	if err != nil {
		return 31
	}
	file, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 32
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return 33
	}
	if err := file.Close(); err != nil {
		return 34
	}
	if len(os.Args) >= 3 && os.Args[1] == "integration" && os.Args[2] == "status" {
		// A text report, not the JSON envelope every other command answers with.
		if _, err := os.Stdout.WriteString(os.Getenv(routeFixtureStatus) + "\n"); err != nil {
			return 36
		}
		return 0
	}
	answer := `{"id":"cli:fixture","result":{"type":"plugin_pane_opened","pane":{"pane_id":"w1:p9"}}}` + "\n"
	if _, err := os.Stdout.WriteString(answer); err != nil {
		return 35
	}
	return 0
}

// eventAnswer is one JSON envelope an event route wrote.
type eventAnswer struct {
	OK    bool `json:"ok"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func runEventRoute(t *testing.T, env map[string]string) (int, eventAnswer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := runEvent(env, &stdout, &stderr)
	var answer eventAnswer
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
			t.Fatalf("decode the answer %q: %v", stdout.String(), err)
		}
	}
	return status, answer
}

// TestEventRouteChecksThePayloadBoundBeforeTrimming pins that the bound applies
// to the bytes Herdr delivered: a payload made mostly of legal JSON whitespace
// with a tiny valid object inside is refused whole, while a whitespace-only
// payload is still refused as missing and one exactly at the bound passes the
// gate.
func TestEventRouteChecksThePayloadBoundBeforeTrimming(t *testing.T) {
	inner := `{"type":"pane_agent_status_changed","pane_id":"w1:p1","workspace_id":"w1","agent_status":"idle"}`

	t.Run("an oversized payload is refused whole", func(t *testing.T) {
		status, answer := runEventRoute(t, map[string]string{
			envPluginEvent:     coord.EventAgentStatus,
			envPluginEventJSON: inner + strings.Repeat(" ", coord.MaxEventPayloadBytes),
		})
		if status == 0 {
			t.Fatalf("status = 0, want nonzero")
		}
		if answer.Error == nil || answer.Error.Code != "event_payload_too_large" {
			t.Fatalf("answer = %+v, want event_payload_too_large", answer.Error)
		}
		if !strings.Contains(answer.Error.Message, "larger than") {
			t.Fatalf("message = %q, want the size refusal", answer.Error.Message)
		}
	})

	t.Run("a whitespace-only payload is still missing", func(t *testing.T) {
		status, answer := runEventRoute(t, map[string]string{
			envPluginEvent:     coord.EventAgentStatus,
			envPluginEventJSON: strings.Repeat(" \t\r\n", 16),
		})
		if status == 0 {
			t.Fatalf("status = 0, want nonzero")
		}
		if answer.Error == nil || answer.Error.Code != "event_payload_unavailable" {
			t.Fatalf("answer = %+v, want event_payload_unavailable", answer.Error)
		}
	})

	t.Run("a payload at the bound passes the size gate", func(t *testing.T) {
		atBound := inner + strings.Repeat(" ", coord.MaxEventPayloadBytes-len(inner))
		status, answer := runEventRoute(t, map[string]string{
			envPluginEvent:     coord.EventAgentStatus,
			envPluginEventJSON: atBound,
		})
		if status == 0 {
			t.Fatalf("status = 0, want the missing state directory to fail the route")
		}
		if answer.Error == nil || answer.Error.Code != "bad_request" {
			t.Fatalf("answer = %+v, want the state-dir refusal (the size gate must not fire)", answer.Error)
		}
		if !strings.Contains(answer.Error.Message, envStateDir) {
			t.Fatalf("message = %q, want it to name the missing state directory", answer.Error.Message)
		}
	})

	t.Run("a missing event name is refused first", func(t *testing.T) {
		status, answer := runEventRoute(t, map[string]string{envPluginEventJSON: inner})
		if status == 0 {
			t.Fatalf("status = 0, want nonzero")
		}
		if answer.Error == nil || answer.Error.Code != "event_unavailable" {
			t.Fatalf("answer = %+v, want event_unavailable", answer.Error)
		}
	})
}

// ---------------------------------------------------------------------------
// board routes
// ---------------------------------------------------------------------------

// TestOpenBoardRouteOpensTheDeclaredPane pins the action route: it resolves the
// bound project from Herdr's injected workspace and opens the declared overlay
// in the active pane rather than passing the workspace as a pane target.
func TestOpenBoardRouteOpensTheDeclaredPane(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := boardRouteEnv(t, `C:\work\demo-repo`, "w1")
	env[envHerdrBinary] = exe
	// The child process inherits the real environment, so the stand-in mode has
	// to be set on it, not only on the map the route reads.
	t.Setenv(routeFixtureEnv, "1")
	t.Setenv(routeFixtureCalls, calls)

	var stdout, stderr bytes.Buffer
	if status := runOpenBoard(env, &stdout, &stderr); status != 0 {
		t.Fatalf("runOpenBoard = %d: %s%s", status, stdout.String(), stderr.String())
	}
	var answer struct {
		OK        bool   `json:"ok"`
		Project   string `json:"project"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if !answer.OK || answer.Workspace != "w1" {
		t.Errorf("answer = %+v, want the board opened in w1", answer)
	}

	recorded := readRouteCalls(t, calls)
	if len(recorded) != 1 {
		t.Fatalf("Herdr calls = %v, want exactly one", recorded)
	}
	want := []string{"plugin", "pane", "open", "--plugin", "user.omp-orchestrator", "--entrypoint", "board"}
	if strings.Join(recorded[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", recorded[0], want)
	}
}

// TestOpenBoardRouteRefusesAnUnboundContext pins that the action never opens a
// board for a guess: an unbound workspace is answered with a code and no Herdr
// call.
func TestOpenBoardRouteRefusesAnUnboundContext(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := boardRouteEnv(t, `C:\work\demo-repo`, "w1")
	env[envHerdrBinary] = exe
	env[routeFixtureEnv] = "1"
	env[routeFixtureCalls] = calls
	env[envWorkspaceID] = "w-nothing"
	t.Setenv(routeFixtureEnv, "1")
	t.Setenv(routeFixtureCalls, calls)

	var stdout, stderr bytes.Buffer
	if status := runOpenBoard(env, &stdout, &stderr); status == 0 {
		t.Fatalf("runOpenBoard succeeded on an unbound workspace: %s", stdout.String())
	}
	if answer := decodeAnswer(t, stdout.Bytes()); answer.Error == nil || answer.Error.Code != "project_unknown" {
		t.Fatalf("answer = %+v, want project_unknown", answer.Error)
	}
	if recorded := readRouteCalls(t, calls); len(recorded) != 0 {
		t.Errorf("an unbound context called Herdr: %v", recorded)
	}
}

// TestBoardRoutesRefuseBeforeTheyOpenAnything keeps both routes fail-closed when
// the environment they need is missing, and actionable about what is missing.
func TestBoardRoutesRefuseBeforeTheyOpenAnything(t *testing.T) {
	cases := []struct {
		name       string
		run        func(env map[string]string, stdout, stderr *bytes.Buffer) int
		env        map[string]string
		noStateDir bool
		code       string
		want       string
	}{
		{
			name:       "the pane route without a state directory",
			run:        func(env map[string]string, stdout, stderr *bytes.Buffer) int { return runBoard(env, stdout, stderr) },
			env:        map[string]string{},
			noStateDir: true,
			code:       "state_dir_unavailable",
			want:       envStateDir,
		},
		{
			name: "the pane route without a resolvable project",
			run:  func(env map[string]string, stdout, stderr *bytes.Buffer) int { return runBoard(env, stdout, stderr) },
			env:  map[string]string{envHerdrBinary: "C:\\imaginary\\herdr.exe"},
			code: "project_unknown",
			want: "workspace",
		},
		{
			name: "the action without a Herdr binary",
			run: func(env map[string]string, stdout, stderr *bytes.Buffer) int {
				return runOpenBoard(env, stdout, stderr)
			},
			env:  map[string]string{envWorkspaceID: "w1"},
			code: "herdr_unavailable",
			want: envHerdrBinary,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			if !tc.noStateDir {
				env[envStateDir] = t.TempDir()
			}
			for key, value := range tc.env {
				env[key] = value
			}
			var stdout, stderr bytes.Buffer
			if status := tc.run(env, &stdout, &stderr); status == 0 {
				t.Fatalf("the route succeeded: %s", stdout.String())
			}
			answer := decodeAnswer(t, stdout.Bytes())
			if answer.Error == nil {
				t.Fatalf("answer = %s, want a failure envelope", stdout.String())
			}
			if answer.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", answer.Error.Code, tc.code)
			}
			if !strings.Contains(answer.Error.Message, tc.want) {
				t.Errorf("message = %q, want it to name %q", answer.Error.Message, tc.want)
			}
		})
	}
}

// boardRouteEnv is one state directory holding one bound project, plus the
// workspace Herdr would have injected.
func boardRouteEnv(t *testing.T, key, workspace string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	err = store.CreateProject(context.Background(), task.Project{
		Key:                key,
		RepoWorkspaceID:    workspace,
		CurrentWorkspaceID: workspace,
		LaunchPaneID:       workspace + ":p1",
		PublicPaneID:       workspace + ":p1",
		AgentName:          "orch_p_fixture",
	})
	if err != nil {
		_ = store.Close()
		t.Fatalf("create project: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the state: %v", err)
	}
	return map[string]string{envStateDir: dir, envWorkspaceID: workspace}
}

func decodeAnswer(t *testing.T, body []byte) eventAnswer {
	t.Helper()
	var answer eventAnswer
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &answer); err != nil {
			t.Fatalf("decode the answer %q: %v", body, err)
		}
	}
	return answer
}

func readRouteCalls(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read the call log: %v", err)
	}
	calls := [][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var call struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		calls = append(calls, call.Argv)
	}
	return calls
}
func TestCoordCodePreservesAmbiguousOperation(t *testing.T) {
	if got := coordCode(herdr.ErrAmbiguous); got != "ambiguous_operation" {
		t.Fatalf("coordCode(herdr.ErrAmbiguous) = %q, want ambiguous_operation", got)
	}
}

// ---------------------------------------------------------------------------
// bind route
// ---------------------------------------------------------------------------

const (
	routeStatusInstalled    = "omp: current (v10) (C:\\Users\\fixture\\.omp\\agent\\extensions\\herdr-omp-agent-state.ts)"
	routeStatusNotInstalled = "omp: not installed (C:\\Users\\fixture\\.omp\\agent\\extensions\\herdr-omp-agent-state.ts)"
)

// bindRouteFixture is everything the bind action reads: a real Git repository
// to bind, a state directory, an isolated OMP profile and the stand-in Herdr
// CLI that logs every call it receives.
type bindRouteFixture struct {
	env        map[string]string
	calls      string
	stateDir   string
	extensions string
}

func newBindRouteFixture(t *testing.T, status string) *bindRouteFixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	invocation, err := json.Marshal(map[string]string{
		"focused_pane_cwd": repo,
		"workspace_id":     "w1",
		"focused_pane_id":  "w1:p1",
	})
	if err != nil {
		t.Fatalf("encode the invocation context: %v", err)
	}
	home := t.TempDir()
	extensions := filepath.Join(home, ".omp", "agent", "extensions")
	if err := os.MkdirAll(extensions, 0o755); err != nil {
		t.Fatalf("create the extensions directory: %v", err)
	}
	f := &bindRouteFixture{
		calls:      filepath.Join(t.TempDir(), "calls.jsonl"),
		stateDir:   t.TempDir(),
		extensions: extensions,
	}
	f.env = map[string]string{
		envStateDir:        f.stateDir,
		envContextJSON:     string(invocation),
		envHerdrBinary:     exe,
		"HOME":             home,
		routeFixtureEnv:    "1",
		routeFixtureCalls:  f.calls,
		routeFixtureStatus: status,
	}
	// The coordinator's Herdr client inherits the process environment, so the
	// stand-in mode has to be set on it as well as on the map the route reads.
	t.Setenv(routeFixtureEnv, "1")
	t.Setenv(routeFixtureCalls, f.calls)
	t.Setenv(routeFixtureStatus, status)
	return f
}

func (f *bindRouteFixture) installIntegration(t *testing.T) {
	t.Helper()
	path := filepath.Join(f.extensions, "herdr-omp-agent-state.ts")
	if err := os.WriteFile(path, []byte("// Herdr's own integration.\n"), 0o644); err != nil {
		t.Fatalf("write the integration file: %v", err)
	}
}

func (f *bindRouteFixture) installExtension(t *testing.T) {
	t.Helper()
	path := filepath.Join(f.extensions, "herdr-orchestrator.ts")
	if err := os.WriteFile(path, []byte(setup.ManagedMarker+"\nexport default {};\n"), 0o644); err != nil {
		t.Fatalf("write the extension: %v", err)
	}
}

func (f *bindRouteFixture) run(t *testing.T) (int, eventAnswer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := runBind(f.env, &stdout, &stderr)
	return status, decodeAnswer(t, stdout.Bytes())
}

func (f *bindRouteFixture) projects(t *testing.T) []task.Project {
	t.Helper()
	store, err := state.Open(f.stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	projects, err := store.Projects(context.Background())
	if err != nil {
		t.Fatalf("read the projects: %v", err)
	}
	return projects
}

// TestBindRouteRefusesBeforeRecordingOrStartingAnything pins the founding spec's
// first bind step through the real route: with Herdr's OMP integration or the
// plugin's extension missing, the action answers with the setup action's own
// code and the remedy, Herdr sees only the read-only status call, and no project
// row exists afterwards.
func TestBindRouteRefusesBeforeRecordingOrStartingAnything(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		setup  func(t *testing.T, f *bindRouteFixture)
		code   string
		remedy string
	}{
		{
			name:   "Herdr reports the OMP integration is not installed",
			status: routeStatusNotInstalled,
			setup: func(t *testing.T, f *bindRouteFixture) {
				f.installIntegration(t)
				f.installExtension(t)
			},
			code:   "integration_missing",
			remedy: "herdr integration install omp",
		},
		{
			name:   "the plugin's extension is absent from the effective profile",
			status: routeStatusInstalled,
			setup:  func(t *testing.T, f *bindRouteFixture) { f.installIntegration(t) },
			code:   "extension_missing",
			remedy: "setup action",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBindRouteFixture(t, tc.status)
			tc.setup(t, f)

			status, answer := f.run(t)
			if status == 0 {
				t.Fatal("runBind succeeded without its prerequisites")
			}
			if answer.Error == nil || answer.Error.Code != tc.code {
				t.Fatalf("answer = %+v, want code %q", answer.Error, tc.code)
			}
			if !strings.Contains(answer.Error.Message, tc.remedy) {
				t.Errorf("message = %q, want it to name the remedy %q", answer.Error.Message, tc.remedy)
			}
			if recorded := readRouteCalls(t, f.calls); len(recorded) != 1 || strings.Join(recorded[0], " ") != "integration status" {
				t.Errorf("Herdr calls = %v, want only `integration status`", recorded)
			}
			if projects := f.projects(t); len(projects) != 0 {
				t.Errorf("a refused bind recorded projects: %+v", projects)
			}
			if _, err := os.Stat(filepath.Join(f.extensions, "herdr-orchestrator.ts")); tc.code == "extension_missing" && !os.IsNotExist(err) {
				t.Errorf("the check installed the extension: stat err = %v", err)
			}
		})
	}
}

// TestBindRouteContinuesPastASatisfiedGate pins the other side: with both
// prerequisites present the route does not refuse; the first Herdr call after
// the status check is Bind's own pane lookup.
func TestBindRouteContinuesPastASatisfiedGate(t *testing.T) {
	f := newBindRouteFixture(t, routeStatusInstalled)
	f.installIntegration(t)
	f.installExtension(t)

	// The stand-in answers every command with the same pane-opened envelope, so
	// the bind itself fails further in; what matters is where it got to.
	_, answer := f.run(t)
	if answer.Error != nil && (answer.Error.Code == "integration_missing" || answer.Error.Code == "extension_missing") {
		t.Fatalf("a satisfied gate refused the bind: %+v", answer.Error)
	}
	recorded := readRouteCalls(t, f.calls)
	if len(recorded) < 2 {
		t.Fatalf("Herdr calls = %v, want the status check followed by Bind's own calls", recorded)
	}
	if got := strings.Join(recorded[0], " "); got != "integration status" {
		t.Errorf("the first Herdr call = %q, want `integration status`", got)
	}
	if len(recorded[1]) < 2 || recorded[1][0]+" "+recorded[1][1] != "pane get" {
		t.Errorf("the second Herdr call = %v, want Bind's `pane get`", recorded[1])
	}
}

// TestCoordCodeNamesAnUnmetBindPrerequisite pins the code mapping directly: the
// setup check's own code when there is one, and a generic code otherwise.
func TestCoordCodeNamesAnUnmetBindPrerequisite(t *testing.T) {
	if got := coordCode(fmt.Errorf("%w: %w", coord.ErrNotReady, setup.ErrExtensionMissing)); got != "extension_missing" {
		t.Errorf("coordCode(not ready: extension missing) = %q, want extension_missing", got)
	}
	if got := coordCode(fmt.Errorf("%w: %w", coord.ErrNotReady, setup.ErrIntegrationMissing)); got != "integration_missing" {
		t.Errorf("coordCode(not ready: integration missing) = %q, want integration_missing", got)
	}
	if got := coordCode(fmt.Errorf("%w: boom", coord.ErrNotReady)); got != "not_ready" {
		t.Errorf("coordCode(not ready) = %q, want not_ready", got)
	}
}
