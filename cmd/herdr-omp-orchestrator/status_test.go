package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestStatusPaneShowsEveryResolvedProjectCheck(t *testing.T) {
	fixture := newSetupRouteFixture(t)
	if status, answer := runSetupRoute(t, fixture.env); status != 0 || !answer.OK {
		t.Fatalf("install fixture extension: status=%d answer=%+v", status, answer)
	}
	stateDir := t.TempDir()
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	project := task.Project{Key: `C:\work\demo`, RepoWorkspaceID: "workspace-1"}
	if err := store.CreateProject(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	env := fixture.env
	env[envStateDir] = stateDir
	env[envWorkspaceID] = "workspace-1"
	var stdout, stderr bytes.Buffer
	if code := runStatus(env, strings.NewReader("\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("runStatus = %d: %s", code, stderr.String())
	}
	for _, want := range []string{"OMP integration: verified", "Plugin extension: verified", `Project: C:\work\demo`, "Primary: verified", "Board: can open"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status missing %q:\n%s", want, stdout.String())
		}
	}
}
func TestStatusPaneDoesNotWriteStoreOrSetupFiles(t *testing.T) {
	fixture := newSetupRouteFixture(t)
	if status, answer := runSetupRoute(t, fixture.env); status != 0 || !answer.OK {
		t.Fatalf("install fixture extension: status=%d answer=%+v", status, answer)
	}
	stateDir := t.TempDir()
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(t.TempDir(), "demo")
	if err := store.CreateProject(context.Background(), task.Project{Key: projectDir, RepoWorkspaceID: "workspace-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(stateDir, "orchestrator.sqlite")
	before, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	beforeHash := sha256.Sum256(before)
	beforeConfig := fixture.configTree(t)
	env := fixture.env
	env[envStateDir] = stateDir
	env[envWorkspaceID] = "workspace-1"
	var stdout, stderr bytes.Buffer
	if code := runStatus(env, strings.NewReader("\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("runStatus = %d: %s", code, stderr.String())
	}
	after, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256.Sum256(after); got != beforeHash {
		t.Fatal("status modified the store")
	}
	if got := fixture.configTree(t); !maps.Equal(got, beforeConfig) {
		t.Fatal("status modified setup profile files")
	}
	for _, want := range []string{"OMP integration: verified", "Plugin extension: verified", "Project: " + projectDir, "Primary: verified", "Board: can open"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestStatusPaneReportsNoProjectWithoutCreatingState(t *testing.T) {
	stateDir := t.TempDir()
	before, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	env := map[string]string{envStateDir: stateDir, envWorkspaceID: "unbound", envHerdrBinary: "herdr"}
	if code := runStatus(env, strings.NewReader("\n"), &stdout, &stderr); code != 0 {
		t.Fatalf("runStatus = %d: %s", code, stderr.String())
	}
	after, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("status changed empty state directory: before=%d entries after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatalf("status changed empty state directory: before=%q after=%q", before[i].Name(), after[i].Name())
		}
	}
	for _, want := range []string{"Project: none resolved (no state recorded yet)", "Board: cannot open (no state recorded yet)"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status missing %q:\n%s", want, stdout.String())
		}
	}
}
func TestManifestDeclaresTheStatusActionAndPane(t *testing.T) {
	action := manifestEntry(t, "actions", "status")
	if got, want := action.fields["command"], `["./herdr-omp-orchestrator.exe", "open-status"]`; got != want {
		t.Errorf("action command = %s, want %s", got, want)
	}
	pane := manifestEntry(t, "panes", "status")
	if got, want := pane.fields["command"], `["./herdr-omp-orchestrator.exe", "status"]`; got != want {
		t.Errorf("pane command = %s, want %s", got, want)
	}
	if got, want := pane.fields["placement"], `"overlay"`; got != want {
		t.Errorf("pane placement = %s, want %s", got, want)
	}
}

func TestUsageIncludesStatusRoutes(t *testing.T) {
	for _, route := range []string{"open-status", "status"} {
		if !strings.Contains(usage, route) {
			t.Errorf("usage does not include %q: %s", route, usage)
		}
	}
}

func TestStatusEnvelopeIsSentToStderrAfterEnter(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := finishStatus(&stdout, &stderr, bufio.NewReader(strings.NewReader("\n")), []string{"Project: none resolved"}, map[string]any{"ok": true})
	if code != 0 || !strings.Contains(stdout.String(), "Press Enter to close.") {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), `"ok":true`) {
		t.Fatalf("stderr=%q", stderr.String())
	}
}
