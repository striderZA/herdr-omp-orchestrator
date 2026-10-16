package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"herdr-omp-orchestrator/internal/board"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/setup"
	"herdr-omp-orchestrator/internal/state"
)

const statusEntrypoint = "status"

// runOpenStatus opens the read-only status overlay with the invoking pane active.
func runOpenStatus(env map[string]string, stdout, stderr io.Writer) int {
	binary := herdrBinary(env)
	if strings.TrimSpace(binary) == "" {
		return emit(stdout, stderr, failureBody("herdr_unavailable", envHerdrBinary+" is not set"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), openBoardTimeout)
	defer cancel()
	client, err := herdr.NewClient(binary)
	if err != nil {
		return emit(stdout, stderr, failureBody("herdr_unavailable", err.Error()))
	}
	opened, err := client.Call(ctx, "plugin", "pane", "open",
		"--plugin", pluginID, "--entrypoint", statusEntrypoint)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	return emit(stdout, stderr, map[string]any{"ok": true, "pane": opened})
}

// runStatus displays read-only checks for this pane and waits for Enter.
func runStatus(env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	in := bufio.NewReader(stdin)
	body := map[string]any{"ok": true}
	binary := herdrBinary(env)
	verifyErr := setup.Verify(context.Background(), env, binary)
	lines := setupStatusLines(verifyErr)
	stateDir := strings.TrimSpace(env[envStateDir])
	if stateDir == "" {
		lines = append(lines, "Project: none resolved", "Board: cannot open ("+envStateDir+" is not set)")
		body = failureBody("state_dir_unavailable", envStateDir+" is not set")
		return finishStatus(stdout, stderr, in, lines, body)
	}
	store, err := state.OpenReadOnly(stateDir)
	if err != nil {
		if errors.Is(err, state.ErrStateAbsent) {
			lines = append(lines, "Project: none resolved (no state recorded yet)", "Board: cannot open (no state recorded yet)")
			return finishStatus(stdout, stderr, in, lines, body)
		}
		lines = append(lines, "Project: none resolved", "Board: cannot open (state store unavailable)")
		body = failureBody("state_read_failed", fmt.Sprintf("read the orchestrator state: %v", err))
		return finishStatus(stdout, stderr, in, lines, body)
	}
	defer func() { _ = store.Close() }()
	invocation := invocationContext(env)
	workspace := firstNonEmpty(invocation.WorkspaceID, env[envWorkspaceID])
	directory := invocationDir(invocation)
	project, projectErr := board.ProjectFor(context.Background(), store, workspace, directory)
	if projectErr != nil {
		lines = append(lines, "Project: none resolved")
		lines = append(lines, statusBoardLine(binary, projectErr))
		return finishStatus(stdout, stderr, in, lines, body)
	}
	lines = append(lines, "Project: "+project.Key)
	if strings.TrimSpace(project.PrimaryAttention) == "" {
		lines = append(lines, "Primary: verified")
	} else {
		lines = append(lines, "Primary: not verified; reattach is needed")
	}
	lines = append(lines, statusBoardLine(binary, nil))
	return finishStatus(stdout, stderr, in, lines, body)
}

func setupStatusLines(verifyErr error) []string {
	if verifyErr == nil {
		return []string{"OMP integration: verified", "Plugin extension: verified"}
	}
	if errors.Is(verifyErr, setup.ErrExtensionMissing) {
		return []string{"OMP integration: verified", "Plugin extension: not verified (" + verifyErr.Error() + ")"}
	}
	if errors.Is(verifyErr, setup.ErrIntegrationMissing) {
		return []string{"OMP integration: not verified (" + verifyErr.Error() + ")", "Plugin extension: not checked"}
	}
	return []string{"OMP integration: not checked (" + verifyErr.Error() + ")", "Plugin extension: not checked"}
}

func finishStatus(stdout, stderr io.Writer, in *bufio.Reader, lines []string, body map[string]any) int {
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprint(stdout, "\nPress Enter to close.")
	_, _ = in.ReadString('\n')
	fmt.Fprintln(stdout)
	return emit(stderr, stderr, body)
}
func statusBoardLine(binary string, projectErr error) string {
	if projectErr != nil {
		return "Board: cannot open (" + projectErr.Error() + ")"
	}
	if strings.TrimSpace(binary) == "" {
		return "Board: cannot open (" + envHerdrBinary + " is not set)"
	}
	if _, err := herdr.NewClient(binary); err != nil {
		return "Board: cannot open (" + err.Error() + ")"
	}
	return "Board: can open"
}
