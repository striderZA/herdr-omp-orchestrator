package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/bridge"
	"herdr-omp-orchestrator/internal/state"
)

const (
	recoveryArmTimeout    = 10 * time.Second
	recoveryPollInterval  = 100 * time.Millisecond
	recoverySignalPending = "pending"
	recoverySignalDone    = "complete"
	recoverySignalAbort   = "abort"
)

type recoveryScope string

const (
	recoveryScopeOwner recoveryScope = "owner"
	recoveryScopeAll   recoveryScope = "all"
)

var errRecoveryAborted = errors.New("recovery worker was not armed")

func recoverySignalPath(markerPath, signal string) string {
	return markerPath + "." + signal
}

func writeRecoverySignal(markerPath, signal string) error {
	if signal != recoverySignalDone && signal != recoverySignalAbort {
		return fmt.Errorf("unknown recovery signal %q", signal)
	}
	path := recoverySignalPath(markerPath, signal)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func recoverySignalExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func removeRecoverySignals(markerPath string) {
	_ = os.Remove(markerPath)
	_ = os.Remove(recoverySignalPath(markerPath, recoverySignalDone))
	_ = os.Remove(recoverySignalPath(markerPath, recoverySignalAbort))
}

// runRecoverableRoute refuses to start an operation until its detached recovery
// worker has acknowledged that it is armed. Once the route runs, a signal failure
// is reported separately and never replaces an answer the route already wrote.
func runRecoverableRoute(
	launch func() (func(bool) error, error),
	route func() (int, bool),
	failed func(error) int,
	signalFailed func(error),
) int {
	signal, err := launch()
	if err != nil {
		return failed(err)
	}
	status, shouldRecover := route()
	if err := signal(shouldRecover); err != nil {
		signalFailed(fmt.Errorf("signal the recovery worker after the route: %w", err))
	}
	return status
}

func startRecoveryWorker(scope recoveryScope, ticketID string) (func(bool) error, error) {
	if scope != recoveryScopeOwner && scope != recoveryScopeAll {
		return nil, fmt.Errorf("unknown recovery scope %q", scope)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve the orchestrator executable: %w", err)
	}
	parent, err := state.CurrentProcessIdentity()
	if err != nil {
		return nil, fmt.Errorf("identify the foreground process: %w", err)
	}
	marker, err := os.CreateTemp("", "herdr-omp-orchestrator-recovery-*.signal")
	if err != nil {
		return nil, fmt.Errorf("create the recovery signal: %w", err)
	}
	markerPath := marker.Name()
	if _, err := marker.WriteString(recoverySignalPending); err != nil {
		_ = marker.Close()
		_ = os.Remove(markerPath)
		return nil, fmt.Errorf("initialize the recovery signal: %w", err)
	}
	if err := marker.Close(); err != nil {
		_ = os.Remove(markerPath)
		return nil, fmt.Errorf("close the recovery signal: %w", err)
	}
	readyPath := markerPath + ".ready"
	args := []string{
		"recover",
		string(scope),
		strconv.FormatUint(uint64(parent.PID), 10),
		strconv.FormatInt(parent.StartTime, 10),
		markerPath,
		readyPath,
		ticketID,
	}
	signal, err := launchRecoveryChild(executable, args, os.Environ(), markerPath, readyPath)
	if err != nil {
		removeRecoverySignals(markerPath)
		_ = os.Remove(readyPath)
		return nil, err
	}
	return signal, nil
}

// recoveryWorkerDir is where a detached recovery worker starts. The worker outlives
// the route that armed it and needs no directory of its own. It must not inherit the
// caller's working directory: on Windows a process keeps its working directory open,
// so a checkout or worktree the route ran from could not be removed until the worker
// exited.
func recoveryWorkerDir() string {
	return os.TempDir()
}

// launchRecoveryChild starts the hidden recovery route with detached stdio and
// waits for its readiness file before allowing the foreground route to proceed.
func launchRecoveryChild(executable string, args, environ []string, markerPath, readyPath string) (func(bool) error, error) {
	cmd := exec.Command(executable, args...)
	cmd.Env = append([]string(nil), environ...)
	cmd.Dir = recoveryWorkerDir()
	cmd.Stdin = nil
	if err := configureDetachedRecoveryChild(cmd); err != nil {
		return nil, fmt.Errorf("configure the recovery worker: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the recovery worker: %w", err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()

	deadline := time.NewTimer(recoveryArmTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(readyPath); err == nil && strings.TrimSpace(string(data)) == "ready" {
			_ = os.Remove(readyPath)
			return func(shouldRecover bool) error {
				signal := recoverySignalAbort
				if shouldRecover {
					signal = recoverySignalDone
				}
				if err := writeRecoverySignal(markerPath, signal); err != nil {
					return fmt.Errorf("write the recovery completion signal: %w", err)
				}
				return nil
			}, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("read the recovery worker readiness: %w", err)
		}
		select {
		case err := <-finished:
			if err == nil {
				return nil, fmt.Errorf("recovery worker exited before arming")
			}
			return nil, fmt.Errorf("recovery worker exited before arming: %w", err)
		case <-deadline.C:
			_ = writeRecoverySignal(markerPath, recoverySignalAbort)
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("recovery worker did not arm within %s", recoveryArmTimeout)
		case <-ticker.C:
		}
	}
}

func waitForRecoveryTrigger(
	ctx context.Context,
	markerPath string,
	parent state.ProcessIdentity,
	alive func(state.ProcessIdentity) (bool, error),
	poll time.Duration,
) error {
	if poll <= 0 {
		poll = recoveryPollInterval
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(markerPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("the recovery signal disappeared before completion")
			}
			return fmt.Errorf("read the recovery signal: %w", err)
		}
		complete, err := recoverySignalExists(recoverySignalPath(markerPath, recoverySignalDone))
		if err != nil {
			return fmt.Errorf("read the recovery completion signal: %w", err)
		}
		abort, err := recoverySignalExists(recoverySignalPath(markerPath, recoverySignalAbort))
		if err != nil {
			return fmt.Errorf("read the recovery abort signal: %w", err)
		}
		if complete && abort {
			return fmt.Errorf("both recovery completion and abort signals exist")
		}
		if abort {
			return errRecoveryAborted
		}
		if complete {
			return nil
		}
		isAlive, err := alive(parent)
		if err == nil && !isAlive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runRecovery(env map[string]string, stderr io.Writer, args []string) int {
	if len(args) != 6 {
		fmt.Fprintln(stderr, "herdr-omp-orchestrator: recover requires a scope, parent identity, signal paths, and ticket filter")
		return 2
	}
	scope := recoveryScope(args[0])
	if scope != recoveryScopeOwner && scope != recoveryScopeAll {
		fmt.Fprintln(stderr, "herdr-omp-orchestrator: recover received an invalid scope")
		return 2
	}
	pid, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil || pid == 0 {
		fmt.Fprintln(stderr, "herdr-omp-orchestrator: recover received an invalid parent PID")
		return 2
	}
	start, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || start <= 0 {
		fmt.Fprintln(stderr, "herdr-omp-orchestrator: recover received an invalid parent creation time")
		return 2
	}
	parent := state.ProcessIdentity{PID: uint32(pid), StartTime: start}
	markerPath, readyPath, ticketID := args[3], args[4], args[5]
	if strings.TrimSpace(markerPath) == "" || strings.TrimSpace(readyPath) == "" {
		fmt.Fprintln(stderr, "herdr-omp-orchestrator: recover received an empty signal path")
		return 2
	}
	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: recover: %v\n", err)
		return 2
	}
	defer closeCoordinator()
	if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: recover readiness: %v\n", err)
		return 2
	}
	defer func() { _ = os.Remove(readyPath) }()
	ctx := context.Background()
	if err := waitForRecoveryTrigger(ctx, markerPath, parent, state.ProcessAlive, recoveryPollInterval); err != nil {
		if errors.Is(err, errRecoveryAborted) {
			removeRecoverySignals(markerPath)
			return 0
		}
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: recover trigger: %v\n", err)
		return 2
	}
	defer removeRecoverySignals(markerPath)
	taskIDs, err := coordinator.CompleteRecoveryClaims(ctx, parent, ticketID)
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: recover owner claims: %v\n", err)
		return 2
	}
	if scope == recoveryScopeOwner && len(taskIDs) == 0 {
		return 0
	}
	if scope == recoveryScopeOwner {
		err = coordinator.RecoverUntilIdle(ctx, taskIDs...)
	} else {
		err = coordinator.RecoverUntilIdle(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "herdr-omp-orchestrator: recover: %v\n", err)
		return 2
	}
	return 0
}

func bridgeRecoveryScope(args []string) (recoveryScope, bool) {
	if len(args) == 0 {
		return "", false
	}
	switch strings.TrimSpace(args[0]) {
	case bridge.MethodDispatch, bridge.MethodTicketMove, bridge.MethodReview, bridge.MethodAdjudicateBrief:
		return recoveryScopeOwner, true
	default:
		return "", false
	}
}
