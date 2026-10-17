package coord

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCanonicalRootDeadlineIsNotNotGitRepository(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}

	coordinator := &Coordinator{opts: Options{CallTimeout: time.Nanosecond, Git: "git"}}
	_, err := coordinator.canonicalRoot(context.Background(), root)
	if errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("canonicalRoot error = %v, must not be ErrNotGitRepository", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canonicalRoot error = %v, want context.DeadlineExceeded", err)
	}
}

func TestCanonicalRootGitStartFailureIsNotNotGitRepository(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}

	coordinator := &Coordinator{opts: Options{CallTimeout: time.Second, Git: filepath.Join(root, "missing-git-executable")}}
	_, err := coordinator.canonicalRoot(context.Background(), root)
	if errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("canonicalRoot error = %v, must not be ErrNotGitRepository", err)
	}
	var pathErr *exec.Error
	if !errors.As(err, &pathErr) {
		t.Fatalf("canonicalRoot error = %v, want wrapped executable start failure (*exec.Error)", err)
	}
}

func TestCanonicalRootOutsideWorkTreeIsNotGitRepository(t *testing.T) {
	root := t.TempDir()
	// t.TempDir is outside the fixture repository; this test does not use
	// newCoordFixture, which creates its own repository under its temp root.
	coordinator := &Coordinator{opts: Options{CallTimeout: time.Second, Git: "git"}}
	_, err := coordinator.canonicalRoot(context.Background(), root)
	if !errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("canonicalRoot error = %v, want ErrNotGitRepository", err)
	}
}
