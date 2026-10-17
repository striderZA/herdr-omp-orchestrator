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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"herdr-omp-orchestrator/internal/bridge"
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
	switch os.Getenv(recoveryProcessTestEnv) {
	case "parent":
		os.Exit(recoveryProcessParentMain())
	case "worker":
		os.Exit(recoveryProcessWorkerMain())
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

func TestBindNextStepNamesActualPrimaryLocation(t *testing.T) {
	project := task.Project{Key: "project-123"}
	for _, tc := range []struct {
		name       string
		launchPane string
		invoking   string
		want       string
	}{
		{name: "reuses invoking pane", launchPane: "w1:p1", invoking: "w1:p1", want: "primary runs in this pane"},
		{name: "dedicated workspace", launchPane: "w2:p1", invoking: "w1:p1", want: "primary runs in a new workspace on pane w2:p1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := bindNextStep(project, tc.launchPane, tc.invoking)
			for _, want := range []string{project.Key, tc.want, "Open the task board"} {
				if !strings.Contains(line, want) {
					t.Errorf("bindNextStep() = %q; want it to contain %q", line, want)
				}
			}
			if strings.ContainsAny(line, "\r\n") {
				t.Fatalf("bindNextStep() = %q; want one line", line)
			}
		})
	}
}

func TestFinishBindWritesNextStepAndAnswer(t *testing.T) {
	project := task.Project{Key: "project-123", LaunchPaneID: "w2:p1"}
	answer := map[string]any{"ok": true, "project": project}
	var gotStdout, stderr, wantStdout bytes.Buffer
	emit(&wantStdout, io.Discard, answer)

	if code := finishBind(project, "w1:p1", answer, &gotStdout, &stderr); code != 0 {
		t.Fatalf("finishBind() = %d, want 0", code)
	}
	if gotStdout.String() != wantStdout.String() {
		t.Errorf("stdout = %q, want emit output %q", gotStdout.String(), wantStdout.String())
	}
	if !strings.Contains(stderr.String(), "the primary runs in a new workspace on pane w2:p1") {
		t.Errorf("stderr = %q, want next-step line naming the launched pane", stderr.String())
	}
}

func TestBindRefusalPrintsNoNextStep(t *testing.T) {
	f := newBindRouteFixture(t, routeStatusNotInstalled)
	f.installIntegration(t)
	f.installExtension(t)
	var stdout, stderr bytes.Buffer
	if code := runBind(f.env, &stdout, &stderr); code == 0 {
		t.Fatal("runBind succeeded when Herdr reports integration not installed")
	}
	answer := decodeAnswer(t, stdout.Bytes())
	if answer.OK || answer.Error == nil || answer.Error.Code != "integration_missing" {
		t.Fatalf("answer = %+v, want integration_missing refusal", answer)
	}
	if strings.Contains(stderr.String(), "Open the task board") {
		t.Fatalf("refusal printed a next step: %q", stderr.String())
	}
}

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

func TestRecoverableRouteArmsBeforeRunningAndSignalsCompletionAfterward(t *testing.T) {
	var order []string
	status := runRecoverableRoute(
		func() (func(bool) error, error) {
			order = append(order, "armed")
			return func(recover bool) error {
				if recover {
					order = append(order, "complete")
				} else {
					order = append(order, "abort")
				}
				return nil
			}, nil
		},
		func() (int, bool) {
			order = append(order, "route")
			return 7, true
		},
		func(err error) int {
			t.Fatalf("runRecoverableRoute failure: %v", err)
			return 2
		},
		func(err error) { t.Fatalf("signal failure: %v", err) },
	)
	if status != 7 {
		t.Fatalf("status = %d, want the route's status 7", status)
	}
	if got := strings.Join(order, ","); got != "armed,route,complete" {
		t.Fatalf("operation order = %q, want armed before the route and completion after it", got)
	}
}

func TestRecoverableRouteRefusesBeforeRunningWhenWorkerCannotArm(t *testing.T) {
	routeCalls := 0
	status := runRecoverableRoute(
		func() (func(bool) error, error) {
			return nil, fmt.Errorf("recovery worker could not start")
		},
		func() (int, bool) {
			routeCalls++
			return 0, true
		},
		func(err error) int {
			if !strings.Contains(err.Error(), "recovery worker could not start") {
				t.Fatalf("failure = %v, want the arming error", err)
			}
			return 2
		},
		func(err error) { t.Fatalf("unexpected post-route signal failure: %v", err) },
	)
	if status != 2 || routeCalls != 0 {
		t.Fatalf("status = %d routeCalls = %d, want refusal before route execution", status, routeCalls)
	}
}

func TestRecoverableRouteAbortsWorkerForAnIdempotentNoOp(t *testing.T) {
	var recover bool
	status := runRecoverableRoute(
		func() (func(bool) error, error) {
			return func(shouldRecover bool) error {
				recover = shouldRecover
				return nil
			}, nil
		},
		func() (int, bool) { return 0, false },
		func(err error) int {
			t.Fatalf("runRecoverableRoute failure: %v", err)
			return 2
		},
		func(err error) { t.Fatalf("signal failure: %v", err) },
	)
	if status != 0 || recover {
		t.Fatalf("status = %d recover = %v, want a no-op to abort the worker", status, recover)
	}
}

func TestRecoverableRouteKeepsRouteResultWhenCompletionSignalFails(t *testing.T) {
	var signalFailure string
	status := runRecoverableRoute(
		func() (func(bool) error, error) {
			return func(bool) error { return fmt.Errorf("marker write failed") }, nil
		},
		func() (int, bool) { return 7, true },
		func(err error) int {
			t.Fatalf("route unexpectedly failed before running: %v", err)
			return 2
		},
		func(err error) { signalFailure = err.Error() },
	)
	if status != 7 || !strings.Contains(signalFailure, "marker write failed") {
		t.Fatalf("status = %d, signal failure = %q; want route status 7 and a reported signal failure",
			status, signalFailure)
	}
}

func TestBridgeResponseNeedsRecoveryDistinguishesMutationsFromNoOps(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		payload string
		want    bool
	}{
		{"dispatch created", bridge.MethodDispatch, `{"ok":true,"result":{"duplicate":false}}`, true},
		{"dispatch duplicate", bridge.MethodDispatch, `{"ok":true,"result":{"duplicate":true}}`, false},
		{"dispatch refusal", bridge.MethodDispatch, `{"ok":false,"error":{"code":"invalid_request"}}`, true},
		{"ticket moved", bridge.MethodTicketMove, `{"ok":true,"result":{}}`, true},
		{"ticket move refusal", bridge.MethodTicketMove, `{"ok":false,"error":{"code":"ticket_changed"}}`, false},
		{"review result", bridge.MethodReview, `{"ok":true,"result":{}}`, true},
		{"review error", bridge.MethodReview, `{"ok":false,"error":{"code":"report_changed"}}`, true},
		{"brief adjudication resumes", bridge.MethodAdjudicateBrief, `{"ok":true,"result":{"decision":"not_delivered"}}`, true},
		{"brief adjudication delivered", bridge.MethodAdjudicateBrief, `{"ok":true,"result":{"decision":"delivered"}}`, false},
		{"brief adjudication error", bridge.MethodAdjudicateBrief, `{"ok":false,"error":{"code":"step_failed"}}`, true},
		{"read only", bridge.MethodTaskList, `{"ok":true,"result":{}}`, false},
		{"malformed answer", bridge.MethodDispatch, `{`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := bridgeResponseNeedsRecovery(test.method, []byte(test.payload)); got != test.want {
				t.Fatalf("bridgeResponseNeedsRecovery(%q, %s) = %v, want %v", test.method, test.payload, got, test.want)
			}
		})
	}
}

func TestBridgeRecoveryScopeCoversOnlyTaskProgressOperations(t *testing.T) {
	tests := []struct {
		method string
		scope  recoveryScope
		want   bool
	}{
		{method: bridge.MethodDispatch, scope: recoveryScopeOwner, want: true},
		{method: bridge.MethodTicketMove, scope: recoveryScopeOwner, want: true},
		{method: bridge.MethodReview, scope: recoveryScopeOwner, want: true},
		{method: bridge.MethodAdjudicateBrief, scope: recoveryScopeOwner, want: true},
		{method: bridge.MethodTaskList, want: false},
		{want: false},
	}
	for _, test := range tests {
		scope, got := bridgeRecoveryScope([]string{test.method})
		if got != test.want || scope != test.scope {
			t.Errorf("bridgeRecoveryScope(%q) = (%q, %v), want (%q, %v)",
				test.method, scope, got, test.scope, test.want)
		}
	}
}

func TestRecoveryWaitsForRouteCompletionOrParentExit(t *testing.T) {
	parent := state.ProcessIdentity{PID: 42, StartTime: 99}
	t.Run("route is still active", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "route.signal")
		if err := os.WriteFile(marker, []byte("pending"), 0o600); err != nil {
			t.Fatalf("create the pending marker: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- waitForRecoveryTrigger(ctx, marker, parent,
				func(got state.ProcessIdentity) (bool, error) { return got == parent, nil },
				5*time.Millisecond)
		}()
		select {
		case err := <-done:
			t.Fatalf("worker armed recovery before the route completed: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if err := writeRecoverySignal(marker, recoverySignalDone); err != nil {
			t.Fatalf("complete the route marker: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("waitForRecoveryTrigger after completion: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("worker did not observe the completed route")
		}
	})
	t.Run("parent exited before completion", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "route.signal")
		if err := os.WriteFile(marker, []byte("pending"), 0o600); err != nil {
			t.Fatalf("create the pending marker: %v", err)
		}
		err := waitForRecoveryTrigger(context.Background(), marker, parent,
			func(state.ProcessIdentity) (bool, error) { return false, nil },
			time.Millisecond)
		if err != nil {
			t.Fatalf("waitForRecoveryTrigger after owner exit: %v", err)
		}
	})
}

func TestRecoveryWorkerRemovesMarkerWhenRouteAborts(t *testing.T) {
	parent, err := state.CurrentProcessIdentity()
	if err != nil {
		t.Fatalf("CurrentProcessIdentity: %v", err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "route.signal")
	ready := filepath.Join(dir, "worker.ready")
	if err := os.WriteFile(marker, []byte(recoverySignalPending), 0o600); err != nil {
		t.Fatalf("create the pending marker: %v", err)
	}
	stderr := &bytes.Buffer{}
	done := make(chan int, 1)
	env := map[string]string{envStateDir: dir, envHerdrBinary: "herdr"}
	args := []string{
		string(recoveryScopeOwner),
		fmt.Sprint(parent.PID),
		fmt.Sprint(parent.StartTime),
		marker,
		ready,
		"",
	}
	go func() { done <- runRecovery(env, stderr, args) }()
	readyDeadline := time.NewTimer(recoveryArmTimeout)
	defer readyDeadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(ready); err == nil && string(data) == "ready" {
			break
		}
		select {
		case status := <-done:
			t.Fatalf("runRecovery exited before readiness: status %d, stderr %q", status, stderr.String())
		case <-readyDeadline.C:
			if err := writeRecoverySignal(marker, recoverySignalAbort); err != nil {
				t.Fatalf("abort the recovery worker after readiness timeout: %v", err)
			}
			select {
			case status := <-done:
				t.Fatalf("recovery worker did not become ready before deadline: status %d, stderr %q", status, stderr.String())
			case <-time.After(recoveryArmTimeout):
				t.Fatal("recovery worker did not exit after aborting during startup")
			}
		case <-ticker.C:
		}
	}
	if err := writeRecoverySignal(marker, recoverySignalAbort); err != nil {
		t.Fatalf("abort the recovery worker: %v", err)
	}
	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("runRecovery status = %d, want successful abort (stderr %q)", status, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("recovery worker did not exit after the route abort")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("abort marker remains: %v", err)
	}
}

const (
	recoveryProcessTestEnv      = "ORCH_RECOVERY_PROCESS_TEST"
	recoveryProcessMarkerEnv    = "ORCH_RECOVERY_PROCESS_MARKER"
	recoveryProcessReadyEnv     = "ORCH_RECOVERY_PROCESS_READY"
	recoveryProcessRecoveredEnv = "ORCH_RECOVERY_PROCESS_RECOVERED"
	recoveryProcessLateEnv      = "ORCH_RECOVERY_PROCESS_LATE"
	recoveryProcessLateFileEnv  = "ORCH_RECOVERY_PROCESS_LATE_FILE"
)

func TestDetachedRecoveryChildSurvivesTheForegroundRoute(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the recovery child uses Windows breakaway process flags")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "route.signal")
	ready := filepath.Join(dir, "worker.ready")
	recovered := filepath.Join(dir, "worker.recovered")
	if err := os.WriteFile(marker, []byte("pending"), 0o600); err != nil {
		t.Fatalf("create the pending marker: %v", err)
	}
	env := os.Environ()
	env = replaceTestEnv(env, recoveryProcessTestEnv, "parent")
	env = replaceTestEnv(env, recoveryProcessMarkerEnv, marker)
	env = replaceTestEnv(env, recoveryProcessReadyEnv, ready)
	env = replaceTestEnv(env, recoveryProcessRecoveredEnv, recovered)
	parent := exec.Command(os.Args[0], "-test.run=^TestDetachedRecoveryChildSurvivesTheForegroundRoute$")
	parent.Env = env
	parent.Stdout = io.Discard
	parent.Stderr = io.Discard
	if err := parent.Run(); err != nil {
		t.Fatalf("foreground test process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(recovered); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("recovery child did not survive the foreground process exit")
}

func TestDetachedRecoveryWorkerDoesNotPinTheCallersDirectory(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the working-directory pin matters only on Windows, where a process holds its directory open; its Linux behavior is not tested, by design")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve the test binary: %v", err)
	}
	dir := t.TempDir()
	caller := filepath.Join(dir, "caller")
	if err := os.Mkdir(caller, 0o700); err != nil {
		t.Fatalf("create the caller directory: %v", err)
	}
	marker := filepath.Join(dir, "route.signal")
	ready := filepath.Join(dir, "worker.ready")
	recovered := filepath.Join(dir, "worker.recovered")
	if err := os.WriteFile(marker, []byte("pending"), 0o600); err != nil {
		t.Fatalf("create the pending marker: %v", err)
	}
	childEnv := replaceTestEnv(os.Environ(), recoveryProcessTestEnv, "worker")
	childEnv = replaceTestEnv(childEnv, recoveryProcessMarkerEnv, marker)
	childEnv = replaceTestEnv(childEnv, recoveryProcessReadyEnv, ready)
	childEnv = replaceTestEnv(childEnv, recoveryProcessRecoveredEnv, recovered)

	// os.Chdir, not t.Chdir: testing keeps a handle on the directory it leaves, which would
	// pin the caller's checkout inside this process and hide what the worker does.
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("read the working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore the working directory: %v", err)
		}
	})
	// The route arms its worker while it still runs from the caller's checkout, then returns.
	if err := os.Chdir(caller); err != nil {
		t.Fatalf("enter the caller's checkout: %v", err)
	}
	completed, err := launchRecoveryChild(exe, []string{"-test.run=^TestDetachedRecoveryWorkerDoesNotPinTheCallersDirectory$"},
		childEnv, marker, ready)
	if chdirErr := os.Chdir(previous); chdirErr != nil {
		t.Fatalf("leave the caller's checkout: %v", chdirErr)
	}
	if err != nil {
		t.Fatalf("arm the recovery worker: %v", err)
	}
	released := false
	release := func() error {
		if released {
			return nil
		}
		released = true
		if err := completed(true); err != nil {
			return err
		}
		return waitForWorkerExit(recovered, 10*time.Second)
	}
	// However the test ends, the worker has exited before the temp directory is removed:
	// a live worker keeps its working directory, and this test binary, in use on Windows.
	t.Cleanup(func() {
		if err := release(); err != nil {
			t.Errorf("release the recovery worker: %v", err)
		}
	})

	// Once the route has returned, the checkout must be removable while the armed worker
	// still waits for its completion signal.
	if err := os.RemoveAll(caller); err != nil {
		t.Fatalf("the armed recovery worker keeps the caller's directory open: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release the recovery worker: %v", err)
	}
	if _, err := os.Stat(recovered); err != nil {
		t.Fatalf("the released recovery worker did not finish: %v", err)
	}
}

// waitForWorkerExit waits for the recovery worker that records its process id in
// pidFile to exit. The worker writes that file last, just before it returns.
func waitForWorkerExit(pidFile string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(data) > 0 {
			pid, convErr := strconv.Atoi(string(data))
			if convErr != nil {
				return fmt.Errorf("the worker recorded %q, not a process id", data)
			}
			return waitForProcessExit(pid, timeout)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the worker did not record its process id within %s", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		// Windows cannot open a process once it has been fully reaped, so it has exited.
		return nil
	}
	defer proc.Release()
	exited := make(chan error, 1)
	go func() {
		_, err := proc.Wait()
		exited <- err
	}()
	select {
	case err := <-exited:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("process %d did not exit within %s", pid, timeout)
	}
}

func recoveryProcessParentMain() int {
	marker := os.Getenv(recoveryProcessMarkerEnv)
	ready := os.Getenv(recoveryProcessReadyEnv)
	childEnv := replaceTestEnv(os.Environ(), recoveryProcessTestEnv, "worker")
	completed, err := launchRecoveryChild(os.Args[0], []string{"-test.run=^TestDetachedRecoveryChildSurvivesTheForegroundRoute$"},
		childEnv, marker, ready)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 31
	}
	if err := completed(true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 32
	}
	return 0
}

func recoveryProcessWorkerMain() int {
	ready := os.Getenv(recoveryProcessReadyEnv)
	marker := os.Getenv(recoveryProcessMarkerEnv)
	recovered := os.Getenv(recoveryProcessRecoveredEnv)
	late := os.Getenv(recoveryProcessLateEnv) == "1"
	lateFile := os.Getenv(recoveryProcessLateFileEnv)
	if ready == "" || marker == "" || recovered == "" || (late && lateFile == "") {
		return 41
	}
	foregroundPID := os.Getppid()
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		return 42
	}
	for {
		complete, err := recoverySignalExists(recoverySignalPath(marker, recoverySignalDone))
		if err == nil && complete {
			if err := os.WriteFile(recovered, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
				return 43
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !late {
		return 0
	}
	// The foreground process exiting reparents this worker, which is the moment
	// the stderr it inherited would have been closed on a real Herdr pane.
	reparented := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if os.Getppid() != foregroundPID {
			reparented = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !reparented {
		return 44
	}
	if _, err := os.Stderr.WriteString("late stderr write\n"); err != nil {
		return 46
	}
	if err := os.WriteFile(lateFile, []byte("late"), 0o600); err != nil {
		return 45
	}
	return 0
}

func replaceTestEnv(env []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(strings.ToUpper(entry), prefix) {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}
