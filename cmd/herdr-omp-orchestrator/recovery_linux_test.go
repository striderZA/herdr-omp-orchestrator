//go:build linux

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestDetachedRecoveryWorkerSurvivesAStderrWriteAfterTheForegroundExits pins the
// Linux worker's stdio: it is detached onto /dev/null, so a write to stderr made
// after the foreground process has exited still completes.
func TestDetachedRecoveryWorkerSurvivesAStderrWriteAfterTheForegroundExits(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "route.signal")
	ready := filepath.Join(dir, "worker.ready")
	recovered := filepath.Join(dir, "worker.recovered")
	late := filepath.Join(dir, "worker.late")
	if err := os.WriteFile(marker, []byte("pending"), 0o600); err != nil {
		t.Fatalf("create the pending marker: %v", err)
	}
	env := os.Environ()
	env = replaceTestEnv(env, recoveryProcessTestEnv, "parent")
	env = replaceTestEnv(env, recoveryProcessMarkerEnv, marker)
	env = replaceTestEnv(env, recoveryProcessReadyEnv, ready)
	env = replaceTestEnv(env, recoveryProcessRecoveredEnv, recovered)
	env = replaceTestEnv(env, recoveryProcessLateEnv, "1")
	env = replaceTestEnv(env, recoveryProcessLateFileEnv, late)
	parent := exec.Command(os.Args[0], "-test.run=^TestDetachedRecoveryWorkerSurvivesAStderrWriteAfterTheForegroundExits$")
	parent.Env = env
	parent.Stdout = io.Discard
	parent.Stderr = io.Discard
	if err := parent.Run(); err != nil {
		t.Fatalf("foreground test process: %v", err)
	}
	exitedAt := time.Now()
	if !waitForPath(recovered, exitedAt.Add(5*time.Second)) {
		t.Fatal("recovery worker did not recover after the foreground process exited")
	}
	if !waitForPath(late, exitedAt.Add(5*time.Second)) {
		t.Fatal("recovery worker's stderr write failed after the foreground process exited")
	}
}

func waitForPath(path string, deadline time.Time) bool {
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
