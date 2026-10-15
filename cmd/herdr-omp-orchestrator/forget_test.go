package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

type fakeForgetter struct {
	plan      coord.ForgetPlan
	planErr   error
	forgetErr error
	forgotten []coord.ForgetPlan
	keys      []string
}

func (f *fakeForgetter) PlanForget(_ context.Context, key string) (coord.ForgetPlan, error) {
	if f.planErr != nil {
		return coord.ForgetPlan{}, f.planErr
	}
	if key != f.plan.ProjectKey {
		return coord.ForgetPlan{}, fmt.Errorf("planned for %q, want %q", f.plan.ProjectKey, key)
	}
	return f.plan, nil
}
func (f *fakeForgetter) Forget(_ context.Context, key string, confirmed coord.ForgetPlan) (coord.ForgetResult, error) {
	f.keys = append(f.keys, key)
	f.forgotten = append(f.forgotten, confirmed)
	if f.forgetErr != nil {
		return coord.ForgetResult{}, f.forgetErr
	}
	return coord.ForgetResult{}, nil
}
func sameForgetPlan(a, b coord.ForgetPlan) bool {
	return a.ProjectKey == b.ProjectKey && a.FolderName == b.FolderName && a.Tasks == b.Tasks && a.Decided == b.Decided && a.Tickets == b.Tickets && a.NotesDelivered == b.NotesDelivered && a.NotesPending == b.NotesPending && a.NotesBlocking == b.NotesBlocking && strings.Join(a.Blockers, "\x00") == strings.Join(b.Blockers, "\x00") && strings.Join(a.Stays, "\x00") == strings.Join(b.Stays, "\x00") && a.Fingerprint == b.Fingerprint
}
func newFakeForgetter() *fakeForgetter {
	return &fakeForgetter{plan: coord.ForgetPlan{ProjectKey: `C:\work\demo-repo`, FolderName: "herdr-omp-orchestrator", Tasks: 3, Decided: 2, Tickets: 4, NotesDelivered: 5, NotesPending: 1, NotesBlocking: 1, Stays: []string{"repository: C:/work/demo-repo", "branch: main", "worktree: C:/work/demo-repo-wt"}}}
}
func runForgetSession(t *testing.T, f *fakeForgetter, input string) (map[string]any, string) {
	t.Helper()
	var out bytes.Buffer
	body := forgetSession(context.Background(), f, f.plan.ProjectKey, bufio.NewReader(strings.NewReader(input)), &out)
	return body, out.String()
}
func TestRunForgetWithoutProjectContextDoesNotOpenCoordinator(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	env := map[string]string{
		envForgetProject: "", envStateDir: stateDir, envHerdrBinary: os.Args[0],
		routeFixtureEnv: "1", routeFixtureCalls: calls,
	}
	t.Setenv(routeFixtureEnv, "1")
	t.Setenv(routeFixtureCalls, calls)
	t.Setenv(envForgetProject, "")
	var stdout, stderr bytes.Buffer

	code := runForget(env, strings.NewReader("\n"), &stdout, &stderr)

	if code != 1 {
		t.Fatalf("runForget exit code = %d, want 1", code)
	}
	answer := decodeAnswer(t, stderr.Bytes())
	if answer.Error == nil || answer.Error.Code != "context_unavailable" {
		t.Fatalf("stderr answer = %+v, want context_unavailable", answer.Error)
	}
	if got := readRouteCalls(t, calls); len(got) != 0 {
		t.Fatalf("Herdr called without invocation context: %v", got)
	}
}
func TestForgetPaneDeletesOnlyOnTheTypedFolderName(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		delete      bool
	}{{"empty", "", false}, {"newline", "\n", false}, {"EOF", "", false}, {"yes", "yes\n", false}, {"exact", "Herdr-OMP-Orchestrator\n", true}, {"trimmed case insensitive", "  herdr-omp-orchestrator  \r\n", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForgetter()
			var input *strings.Reader = strings.NewReader(tc.input)
			if tc.name == "EOF" {
				input = strings.NewReader("")
			}
			var out bytes.Buffer
			body := forgetSession(context.Background(), f, f.plan.ProjectKey, bufio.NewReader(input), &out)
			if body["forgotten"] != tc.delete {
				t.Fatalf("body = %v, want forgotten=%v", body, tc.delete)
			}
			if got := len(f.forgotten); got != map[bool]int{true: 1, false: 0}[tc.delete] {
				t.Fatalf("Forget calls = %d", got)
			}
			if tc.delete && (!sameForgetPlan(f.forgotten[0], f.plan) || f.keys[0] != f.plan.ProjectKey) {
				t.Fatalf("Forget args = %+v %v", f.forgotten, f.keys)
			}
		})
	}
}
func TestForgetPaneRefusesAndShowsTheReasons(t *testing.T) {
	f := newFakeForgetter()
	f.plan.Blockers = []string{"a task is active", "a note blocks deletion"}
	body, shown := runForgetSession(t, f, "herdr-omp-orchestrator\n")
	failure, _ := body["error"].(map[string]any)
	if failure["code"] != "forget_refused" {
		t.Fatalf("body = %v", body)
	}
	for _, reason := range f.plan.Blockers {
		if !strings.Contains(shown, reason) {
			t.Errorf("output misses %q: %s", reason, shown)
		}
	}
	if strings.Contains(shown, "Type ") {
		t.Errorf("refusal prompted: %s", shown)
	}
	if len(f.forgotten) != 0 {
		t.Fatalf("Forget called: %v", f.forgotten)
	}
}
func TestForgetPaneShowsWhatStays(t *testing.T) {
	f := newFakeForgetter()
	body, shown := runForgetSession(t, f, "\n")
	for _, line := range f.plan.Stays {
		if !strings.Contains(shown, line) {
			t.Errorf("output misses %q: %s", line, shown)
		}
	}
	for _, required := range []string{"read-only restriction", "restart the session", "close its pane"} {
		if !strings.Contains(strings.ToLower(shown), required) {
			t.Errorf("output misses restart note phrase %q: %s", required, shown)
		}
	}
	want := map[string]any{"ok": true, "forgotten": false}
	if len(body) != len(want) || body["ok"] != true || body["forgotten"] != false {
		t.Errorf("declined body = %v, want exactly %v", body, want)
	}
}
func TestForgetPaneArgsCarryTheKeyVerbatim(t *testing.T) {
	key := "C:/Work/My Repo ü"
	got := forgetPaneArgs(key)
	want := []string{"--env", envForgetProject + "=" + key}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("forgetPaneArgs = %q, want %q", got, want)
	}
}
func TestOpenForgetRefusesAnUnknownOrAmbiguousProject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		projects  []task.Project
		workspace string
	}{{"unknown", nil, "w1"}, {"ambiguous", []task.Project{{Key: "C:/one", RepoWorkspaceID: "w1"}, {Key: "C:/two", RepoWorkspaceID: "w1"}}, "w1"}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range tc.projects {
				if err := store.CreateProject(context.Background(), p); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(t.TempDir(), "calls.jsonl")
			env := map[string]string{envStateDir: dir, envHerdrBinary: os.Args[0], envWorkspaceID: tc.workspace, routeFixtureEnv: "1", routeFixtureCalls: calls}
			t.Setenv(routeFixtureEnv, "1")
			t.Setenv(routeFixtureCalls, calls)
			var stdout, stderr bytes.Buffer
			if status := runOpenForget(env, &stdout, &stderr); status == 0 {
				t.Fatalf("succeeded: %s", stdout.String())
			}
			if answer := decodeAnswer(t, stdout.Bytes()); answer.Error == nil || answer.Error.Code != "project_unknown" {
				t.Fatalf("answer = %+v", answer.Error)
			}
			if got := readRouteCalls(t, calls); len(got) != 0 {
				t.Fatalf("Herdr called: %v", got)
			}
		})
	}
}
func TestCoordCodeMapsTheForgetErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{coord.ErrForgetRefused, "forget_refused"}, {coord.ErrForgetPlanChanged, "forget_plan_changed"}, {coord.ErrForgetNotConfirmed, "forget_not_confirmed"}, {errors.New("unrelated"), "internal"}} {
		if got := coordCode(tc.err); got != tc.want {
			t.Errorf("coordCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
func TestManifestDeclaresTheForgetActionAndItsPane(t *testing.T) {
	action := manifestEntry(t, "actions", "forget")
	if got, want := action.fields["command"], `["./herdr-omp-orchestrator.exe", "open-forget"]`; got != want {
		t.Errorf("action command = %s, want %s", got, want)
	}
	if got, want := action.fields["contexts"], `["pane", "workspace"]`; got != want {
		t.Errorf("action contexts = %s, want %s", got, want)
	}
	pane := manifestEntry(t, "panes", "forget")
	if got, want := pane.fields["command"], `["./herdr-omp-orchestrator.exe", "forget"]`; got != want {
		t.Errorf("pane command = %s, want %s", got, want)
	}
	if got, want := pane.fields["placement"], `"overlay"`; got != want {
		t.Errorf("pane placement = %s, want %s", got, want)
	}
}

func TestReadmeNamesEveryForgetCode(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	for _, code := range []string{"forget_refused", "forget_plan_changed", "forget_not_confirmed"} {
		if !strings.Contains(string(readme), code) {
			t.Errorf("README does not contain %q", code)
		}
	}
}
