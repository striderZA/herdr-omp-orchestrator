package main

// Issue #18: the reattach action is explicit and confirmed. These tests pin the
// manifest entries Herdr dispatches, the action that opens the confirmation pane
// for the pane the user invoked it from, and the pane's own contract: nothing is
// reattached without an explicit yes, and what is reattached is exactly the plan
// that was shown.

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
	"herdr-omp-orchestrator/internal/task"
)

// manifestSection is one `[[actions]]` or `[[panes]]` entry of the manifest.
type manifestSection struct {
	kind   string
	fields map[string]string
}

// readManifestSections is a small reader for this repository's own manifest: it
// keeps each array-of-tables section's `key = value` lines verbatim.
func readManifestSections(t *testing.T) []manifestSection {
	t.Helper()
	return readManifestDoc(t, filepath.Join("..", "..", "herdr-plugin.toml")).sections
}

func manifestEntry(t *testing.T, kind, id string) manifestSection {
	t.Helper()
	var found []manifestSection
	for _, section := range readManifestSections(t) {
		if section.kind == kind && section.fields["id"] == fmt.Sprintf("%q", id) {
			found = append(found, section)
		}
	}
	if len(found) != 1 {
		t.Fatalf("the manifest declares %d [[%s]] entries with id %q, want exactly 1", len(found), kind, id)
	}
	return found[0]
}

func TestManifestDeclaresTheReattachActionAndItsConfirmationPane(t *testing.T) {
	action := manifestEntry(t, "actions", "reattach")
	if got, want := action.fields["command"], `["./herdr-omp-orchestrator.exe", "open-reattach"]`; got != want {
		t.Errorf("action command = %s, want %s", got, want)
	}
	// The replacement is the pane the action is invoked from.
	if got, want := action.fields["contexts"], `["pane"]`; got != want {
		t.Errorf("action contexts = %s, want %s", got, want)
	}
	pane := manifestEntry(t, "panes", reattachEntrypoint)
	if got, want := pane.fields["command"], `["./herdr-omp-orchestrator.exe", "reattach"]`; got != want {
		t.Errorf("pane command = %s, want %s", got, want)
	}
	if got, want := pane.fields["placement"], `"overlay"`; got != want {
		t.Errorf("pane placement = %s, want %s", got, want)
	}
	// Bind keeps its own action: reattach never replaces it.
	manifestEntry(t, "actions", "bind")
}

func openReattachEnv(t *testing.T, calls, contextJSON string) map[string]string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(routeFixtureEnv, "1")
	t.Setenv(routeFixtureCalls, calls)
	env := map[string]string{envStateDir: t.TempDir(), envHerdrBinary: exe}
	if contextJSON != "" {
		env[envContextJSON] = contextJSON
	}
	return env
}

func TestOpenReattachRouteOpensTheDeclaredPaneForTheInvokedPane(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	env := openReattachEnv(t, calls, `{"focused_pane_id":"w3:p1","focused_pane_cwd":"C:\\work\\demo-repo"}`)
	// The overlay is a pane of its own: the environment's pane is not the target.
	env[envFocusedPane] = "w9:p9"

	var stdout, stderr bytes.Buffer
	if status := runOpenReattach(env, &stdout, &stderr); status != 0 {
		t.Fatalf("runOpenReattach = %d: %s%s", status, stdout.String(), stderr.String())
	}
	recorded := readRouteCalls(t, calls)
	if len(recorded) != 1 {
		t.Fatalf("Herdr calls = %v, want exactly one", recorded)
	}
	want := []string{"plugin", "pane", "open", "--plugin", "user.omp-orchestrator", "--entrypoint", "reattach",
		"--env", "HERDR_ORCH_REATTACH_PANE=w3:p1"}
	if strings.Join(recorded[0], "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", recorded[0], want)
	}
	if answer := decodeAnswer(t, stdout.Bytes()); !answer.OK {
		t.Errorf("answer = %s, want ok", stdout.String())
	}
}

func TestOpenReattachRouteFallsBackToTheInjectedPane(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls.jsonl")
	env := openReattachEnv(t, calls, "")
	env[envFocusedPane] = "w4:p2"

	var stdout, stderr bytes.Buffer
	if status := runOpenReattach(env, &stdout, &stderr); status != 0 {
		t.Fatalf("runOpenReattach = %d: %s%s", status, stdout.String(), stderr.String())
	}
	recorded := readRouteCalls(t, calls)
	if len(recorded) != 1 || recorded[0][len(recorded[0])-1] != "HERDR_ORCH_REATTACH_PANE=w4:p2" {
		t.Fatalf("Herdr calls = %v, want the injected pane named", recorded)
	}
}

func TestOpenReattachRouteRefusesBeforeOpeningAnything(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(env map[string]string)
		code     string
		mentions string
	}{
		{"no state directory", func(env map[string]string) { delete(env, envStateDir) }, "state_dir_unavailable", envStateDir},
		{"no Herdr binary", func(env map[string]string) { delete(env, envHerdrBinary) }, "herdr_unavailable", envHerdrBinary},
		{"no pane to reattach", func(env map[string]string) { delete(env, envContextJSON); delete(env, envFocusedPane) }, "context_unavailable", "pane"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := filepath.Join(t.TempDir(), "calls.jsonl")
			env := openReattachEnv(t, calls, `{"focused_pane_id":"w3:p1"}`)
			tc.mutate(env)

			var stdout, stderr bytes.Buffer
			if status := runOpenReattach(env, &stdout, &stderr); status == 0 {
				t.Fatalf("the route succeeded: %s", stdout.String())
			}
			answer := decodeAnswer(t, stdout.Bytes())
			if answer.Error == nil || answer.Error.Code != tc.code || !strings.Contains(answer.Error.Message, tc.mentions) {
				t.Fatalf("answer = %s, want %s naming %q", stdout.String(), tc.code, tc.mentions)
			}
			if recorded := readRouteCalls(t, calls); len(recorded) != 0 {
				t.Errorf("a refused action called Herdr: %v", recorded)
			}
		})
	}
}

// fakeReattacher records what the pane route asks of the coordinator.
type fakeReattacher struct {
	project      string
	plan         coord.ReattachPlan
	projectErr   error
	planErr      error
	reattachErr  error
	reattached   []coord.ReattachPlan
	reattachedAt []string
}

func (f *fakeReattacher) PaneProject(_ context.Context, _ string) (string, error) {
	return f.project, f.projectErr
}

func (f *fakeReattacher) PlanReattach(_ context.Context, key, pane string) (coord.ReattachPlan, error) {
	if f.planErr != nil {
		return coord.ReattachPlan{}, f.planErr
	}
	if key != f.project {
		return coord.ReattachPlan{}, fmt.Errorf("planned for %q, want the project the pane names, %q", key, f.project)
	}
	return f.plan, nil
}

func (f *fakeReattacher) Reattach(_ context.Context, key, pane string, confirmed coord.ReattachPlan) (task.Project, error) {
	f.reattached = append(f.reattached, confirmed)
	f.reattachedAt = append(f.reattachedAt, key+"|"+pane)
	if f.reattachErr != nil {
		return task.Project{}, f.reattachErr
	}
	return task.Project{Key: key, LaunchPaneID: pane, OMPSessionID: confirmed.Session.Value}, nil
}

func newFakeReattacher() *fakeReattacher {
	return &fakeReattacher{
		project: `C:\work\demo-repo`,
		plan: coord.ReattachPlan{
			ProjectKey: `C:\work\demo-repo`, PaneID: "w3:p1", WorkspaceID: "w3", TerminalID: "term-3",
			Session:  task.SessionRef{Kind: task.SessionKindID, Value: "omp-new"},
			Replaces: task.ReplacedPrimary{LaunchPaneID: "w1:p1", PublicPaneID: "w1:p1", OMPSessionID: "omp-old"},
			Observed: "the recorded primary cannot be verified: pane w1:p1 holds no agent",
		},
	}
}

func runSession(t *testing.T, f *fakeReattacher, input string) (map[string]any, string) {
	t.Helper()
	var out bytes.Buffer
	body := reattachSession(context.Background(), f, "w3:p1", bufio.NewReader(strings.NewReader(input)), &out)
	return body, out.String()
}

func TestReattachPaneReattachesTheConfirmedPlanOnAnExplicitYes(t *testing.T) {
	for _, reply := range []string{"yes\n", "y\n", "  YES  \r\n", "yes"} {
		t.Run(fmt.Sprintf("%q", reply), func(t *testing.T) {
			f := newFakeReattacher()
			body, shown := runSession(t, f, reply)
			if ok, _ := body["ok"].(bool); !ok || body["reattached"] != true {
				t.Fatalf("body = %v, want a reattach", body)
			}
			if len(f.reattached) != 1 || f.reattached[0] != f.plan || f.reattachedAt[0] != `C:\work\demo-repo|w3:p1` {
				t.Fatalf("Reattach calls = %+v at %v, want the plan that was shown, for the project and pane it names", f.reattached, f.reattachedAt)
			}
			for _, want := range []string{`C:\work\demo-repo`, "w3:p1", "omp-new", "w1:p1", "omp-old", "holds no agent", "yes"} {
				if !strings.Contains(shown, want) {
					t.Errorf("the user was never shown %q before confirming:\n%s", want, shown)
				}
			}
			if !strings.Contains(shown, "Reattached and confirmed") {
				t.Errorf("the outcome was not reported:\n%s", shown)
			}
		})
	}
}

func TestReattachPaneChangesNothingWithoutAnExplicitYes(t *testing.T) {
	for _, reply := range []string{"", "\n", "no\n", "n\n", "yes please\n", "ye\n", "sure\n"} {
		t.Run(fmt.Sprintf("%q", reply), func(t *testing.T) {
			f := newFakeReattacher()
			body, shown := runSession(t, f, reply)
			if ok, _ := body["ok"].(bool); !ok || body["reattached"] != false {
				t.Fatalf("body = %v, want a cancelled reattach", body)
			}
			if len(f.reattached) != 0 {
				t.Fatalf("Reattach was called without a yes: %+v", f.reattached)
			}
			if !strings.Contains(shown, "Nothing was changed") {
				t.Errorf("the cancellation was not reported:\n%s", shown)
			}
		})
	}
}

func TestReattachPaneRefusesBeforeAskingAnythingWhenThePlanIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fakeReattacher)
		code  string
	}{
		{"the pane names no project", func(f *fakeReattacher) { f.projectErr = fmt.Errorf("%w: nope", coord.ErrNotGitRepository) }, "not_git_repository"},
		{"the project is unbound", func(f *fakeReattacher) { f.planErr = fmt.Errorf("%w: bind first", coord.ErrProjectUnknown) }, "project_unknown"},
		{"the recorded primary is live", func(f *fakeReattacher) { f.planErr = fmt.Errorf("%w: w1:p1", coord.ErrPrimaryLive) }, "primary_live"},
		{"another repository", func(f *fakeReattacher) { f.planErr = fmt.Errorf("%w: elsewhere", coord.ErrDifferentRepository) }, "different_repository"},
		{"another agent kind", func(f *fakeReattacher) { f.planErr = fmt.Errorf("%w: claude", coord.ErrAgentChanged) }, "agent_changed"},
		{"a pane another record owns", func(f *fakeReattacher) { f.planErr = fmt.Errorf("%w: task", coord.ErrAlreadyBound) }, "already_bound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeReattacher()
			tc.setup(f)
			// A yes waiting on stdin must not be consumed by a refused plan.
			body, shown := runSession(t, f, "yes\n")
			failure, _ := body["error"].(map[string]any)
			if body["ok"] != false || failure["code"] != tc.code {
				t.Fatalf("body = %v, want %s", body, tc.code)
			}
			if len(f.reattached) != 0 {
				t.Fatalf("a refused plan was reattached: %+v", f.reattached)
			}
			if strings.Contains(shown, "Type yes") {
				t.Errorf("a refused plan still asked for confirmation:\n%s", shown)
			}
		})
	}
}

func TestReattachPaneReportsAnUnconfirmedReattachAsAFailure(t *testing.T) {
	f := newFakeReattacher()
	f.reattachErr = fmt.Errorf("%w: pane w3:p1 is recorded but not confirmed (%w)", coord.ErrReattachPending, coord.ErrReadinessTimeout)
	body, shown := runSession(t, f, "yes\n")
	failure, _ := body["error"].(map[string]any)
	if body["ok"] != false || failure["code"] != "reattach_pending" {
		t.Fatalf("body = %v, want reattach_pending, not the readiness timeout that caused it", body)
	}
	if strings.Contains(shown, "Reattached and confirmed") {
		t.Errorf("a provisional write was reported as success:\n%s", shown)
	}
}

func TestReattachRouteFailsClosedWithoutAPaneAndNeverBlocks(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status := runReattach(map[string]string{envStateDir: t.TempDir()}, strings.NewReader(""), &stdout, &stderr)
	if status == 0 {
		t.Fatalf("the route succeeded without a pane: %s", stdout.String())
	}
	answer := decodeAnswer(t, stderr.Bytes())
	if answer.Error == nil || answer.Error.Code != "bad_request" || !strings.Contains(answer.Error.Message, envReattachPane) {
		t.Fatalf("envelope = %s, want bad_request naming %s", stderr.String(), envReattachPane)
	}
	if !strings.Contains(stdout.String(), "Not reattached") {
		t.Errorf("the user was shown nothing:\n%s", stdout.String())
	}
}

func TestCoordCodeNamesTheReattachRefusals(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{coord.ErrPrimaryLive, "primary_live"},
		{coord.ErrAlreadyBound, "already_bound"},
		{coord.ErrReplacementBusy, "replacement_busy"},
		{coord.ErrEvidenceChanged, "evidence_changed"},
		{coord.ErrReattachPending, "reattach_pending"},
		// The cause that left a reattach pending does not hide that it is pending.
		{fmt.Errorf("%w (%w)", coord.ErrReattachPending, coord.ErrReadinessTimeout), "reattach_pending"},
		{fmt.Errorf("%w (%w)", coord.ErrReattachPending, coord.ErrAgentChanged), "reattach_pending"},
		{errors.New("unrelated"), "internal"},
	}
	for _, tc := range cases {
		if got := coordCode(tc.err); got != tc.want {
			t.Errorf("coordCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
