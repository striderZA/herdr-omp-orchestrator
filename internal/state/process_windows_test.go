//go:build windows

package state_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"herdr-omp-orchestrator/internal/state"
)

const processExitCodeHelperEnv = "ORCH_PROCESS_EXIT_CODE_HELPER"

// stillActiveCode is what GetExitCodeProcess reports for a running process, and the
// code the helper exits with.
const stillActiveCode = 259

// TestProcessExitCodeHelper is the child process. It reports its identity, waits for
// the parent to release it, and exits with stillActiveCode. It runs only when
// re-executed by the test below.
func TestProcessExitCodeHelper(t *testing.T) {
	if os.Getenv(processExitCodeHelperEnv) != "1" {
		return
	}
	identity, err := state.CurrentProcessIdentity()
	if err != nil {
		fmt.Fprintf(os.Stderr, "CurrentProcessIdentity: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%d %d\n", identity.PID, identity.StartTime)
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		fmt.Fprintf(os.Stderr, "wait for the parent: %v\n", err)
		os.Exit(1)
	}
	os.Exit(stillActiveCode)
}

// TestProcessAliveIsFalseAfterAProcessExitsWithTheStillActiveCode pins a liveness
// check that read exit code 259 as "still running". Windows keeps an exited
// process's object, and so its exit code, only while a handle to it is open. A route
// waiting on its child holds such a handle, and that is the state in which
// GetExitCodeProcess reports 259 for a process that has finished.
func TestProcessAliveIsFalseAfterAProcessExitsWithTheStillActiveCode(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessExitCodeHelper$")
	cmd.Env = append(os.Environ(), processExitCodeHelperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read helper identity: %v", err)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("helper identity = %q, want PID and start time", line)
	}
	pid, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		t.Fatalf("helper PID %q: %v", fields[0], err)
	}
	start, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("helper start time %q: %v", fields[1], err)
	}
	identity := state.ProcessIdentity{PID: uint32(pid), StartTime: start}

	// Hold a handle on the child while it runs, as the waiting route does, so the
	// process object outlives the child's exit.
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, identity.PID)
	if err != nil {
		t.Fatalf("open helper process: %v", err)
	}
	defer windows.CloseHandle(handle)

	if _, err := stdin.Write([]byte("exit\n")); err != nil {
		t.Fatalf("release helper: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close helper stdin: %v", err)
	}
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != stillActiveCode {
		t.Fatalf("helper exit = %v, want exit code %d", err, stillActiveCode)
	}
	alive, err := state.ProcessAlive(identity)
	if err != nil {
		t.Fatalf("ProcessAlive: %v", err)
	}
	if alive {
		t.Fatal("ProcessAlive = true for a process that has exited; want false")
	}
}
