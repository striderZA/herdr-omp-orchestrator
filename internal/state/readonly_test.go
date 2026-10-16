package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"herdr-omp-orchestrator/internal/state"
	"herdr-omp-orchestrator/internal/task"
)

func TestOpenReadOnlyReadsClosedWALStoreWithoutWritingFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	project := task.Project{Key: "repo-a", RepoWorkspaceID: "workspace-a"}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "orchestrator.sqlite")
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("after clean close, %s stat error = %v; expected absent WAL sidecar", suffix, err)
		}
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := state.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly(%q): %v", dir, err)
	}
	got, err := readOnly.Project(ctx, project.Key)
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	if got.Key != project.Key || got.RepoWorkspaceID != project.RepoWorkspaceID {
		t.Fatalf("Project = %+v, want %+v", got, project)
	}
	if err := readOnly.CreateProject(ctx, task.Project{Key: "repo-b"}); err == nil {
		t.Fatal("CreateProject on a read-only store succeeded")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("OpenReadOnly changed directory entries: before=%v after=%v", dirEntryNames(before), dirEntryNames(after))
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatalf("OpenReadOnly changed directory entries: before=%v after=%v", dirEntryNames(before), dirEntryNames(after))
		}
	}
}

func TestOpenReadOnlyReportsAbsentWithoutCreatingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "state")
	store, err := state.OpenReadOnly(dir)
	if store != nil {
		t.Fatalf("OpenReadOnly(%q) returned a store for an absent database", dir)
	}
	if !errors.Is(err, state.ErrStateAbsent) {
		t.Fatalf("OpenReadOnly(%q) error = %v, want ErrStateAbsent", dir, err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("OpenReadOnly created state directory: stat error = %v", statErr)
	}
}

func dirEntryNames(entries []os.DirEntry) []string {
	result := make([]string, len(entries))
	for i, entry := range entries {
		result[i] = entry.Name()
	}
	return result
}
