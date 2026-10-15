package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"herdr-omp-orchestrator/internal/board"
	"herdr-omp-orchestrator/internal/coord"
	"herdr-omp-orchestrator/internal/herdr"
	"herdr-omp-orchestrator/internal/state"
)

const forgetEntrypoint = "forget"
const envForgetProject = "HERDR_ORCH_FORGET_PROJECT"

const forgetPlanTimeout = 30 * time.Second
const forgetTimeout = 30 * time.Second

type forgetter interface {
	PlanForget(ctx context.Context, projectKey string) (coord.ForgetPlan, error)
	Forget(ctx context.Context, projectKey string, confirmed coord.ForgetPlan) (coord.ForgetResult, error)
}

// forgetPaneArgs carries the resolved project identity from the action to its overlay.
func forgetPaneArgs(projectKey string) []string {
	return []string{"--env", envForgetProject + "=" + projectKey}
}

// runOpenForget resolves the invoked project before asking Herdr to open anything.
func runOpenForget(env map[string]string, stdout, stderr io.Writer) int {
	stateDir := strings.TrimSpace(env[envStateDir])
	if stateDir == "" {
		return emit(stdout, stderr, failureBody("state_dir_unavailable", envStateDir+" is not set"))
	}
	binary := herdrBinary(env)
	if binary == "" {
		return emit(stdout, stderr, failureBody("herdr_unavailable", envHerdrBinary+" is not set"))
	}
	store, err := state.Open(stateDir)
	if err != nil {
		return emit(stdout, stderr, failureBody("state_write_failed", fmt.Sprintf("open the orchestrator state: %v", err)))
	}
	defer func() { _ = store.Close() }()
	invocation := invocationContext(env)
	project, err := board.ProjectFor(context.Background(), store,
		firstNonEmpty(invocation.WorkspaceID, env[envWorkspaceID]), invocationDir(invocation))
	if err != nil {
		return emit(stdout, stderr, failureBody("project_unknown", err.Error()))
	}
	client, err := herdr.NewClient(binary)
	if err != nil {
		return emit(stdout, stderr, failureBody("herdr_unavailable", err.Error()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), openBoardTimeout)
	defer cancel()
	argv := append([]string{"plugin", "pane", "open", "--plugin", pluginID, "--entrypoint", forgetEntrypoint}, forgetPaneArgs(project.Key)...)
	opened, err := client.Call(ctx, argv...)
	if err != nil {
		return emit(stdout, stderr, failureBody(coordCode(err), err.Error()))
	}
	return emit(stdout, stderr, map[string]any{"ok": true, "project": project.Key, "pane": opened})
}

// runForget is the interactive forget confirmation pane.
func runForget(env map[string]string, stdin io.Reader, stdout, stderr io.Writer) int {
	in := bufio.NewReader(stdin)
	key := env[envForgetProject]
	if strings.TrimSpace(key) == "" {
		return finishForget(stdout, stderr, in, failureBody("context_unavailable", envForgetProject+" is not set"))
	}
	coordinator, closeCoordinator, err := openCoordinator(env)
	if err != nil {
		return finishForget(stdout, stderr, in, failureBody(coordCode(err), err.Error()))
	}
	defer closeCoordinator()
	return finishForget(stdout, stderr, in, forgetSession(context.Background(), coordinator, key, in, stdout))
}

func forgetSession(ctx context.Context, c forgetter, key string, in *bufio.Reader, stdout io.Writer) map[string]any {
	planCtx, cancel := context.WithTimeout(ctx, forgetPlanTimeout)
	plan, err := c.PlanForget(planCtx, key)
	cancel()
	if err != nil {
		code := coordCode(err)
		if code == "forget_refused" {
			fmt.Fprintf(stdout, "Cannot forget %s:\n", key)
		}
		return failureBody(code, err.Error())
	}
	if len(plan.Blockers) != 0 {
		fmt.Fprintf(stdout, "Cannot forget %s:\n", key)
		for _, reason := range plan.Blockers { fmt.Fprintf(stdout, "- %s\n", reason) }
		return failureBody("forget_refused", strings.Join(plan.Blockers, "; "))
	}
	fmt.Fprintf(stdout, "Forgetting %s: %d tasks (%d decided), %d tickets, %d delivered notes, %d pending notes.\n", key, plan.Tasks, plan.Decided, plan.Tickets, plan.NotesDelivered, plan.NotesPending)
	for _, line := range plan.Stays { fmt.Fprintf(stdout, "Stays: %s\n", line) }
	fmt.Fprintf(stdout, "Type %s to forget %s, or press Enter to cancel: ", plan.FolderName, key)
	line, _ := in.ReadString('\n')
	if !strings.EqualFold(strings.TrimSpace(line), plan.FolderName) {
		fmt.Fprintln(stdout, "\nCancelled. Nothing was changed.")
		return map[string]any{"ok": true, "forgotten": false, "project": key}
	}
	actCtx, cancel := context.WithTimeout(ctx, forgetTimeout)
	defer cancel()
	result, err := c.Forget(actCtx, key, plan)
	if err != nil { return failureBody(coordCode(err), err.Error()) }
	for _, stays := range result.Stays { fmt.Fprintf(stdout, "Stays: %s\n", stays) }
	counts := result.Deleted
	fmt.Fprintf(stdout, "Forgot %s: %d tasks, %d tickets, %d notes removed.\n", key, counts.Tasks, counts.Tickets, counts.Notes)
	return map[string]any{"ok": true, "forgotten": true, "project": key, "tasks": counts.Tasks, "tickets": counts.Tickets, "notes": counts.Notes}
}

func finishForget(stdout, stderr io.Writer, in *bufio.Reader, body map[string]any) int {
	if ok, _ := body["ok"].(bool); !ok {
		code, message := "internal", ""
		if failure, found := body["error"].(map[string]any); found { code, _ = failure["code"].(string); message, _ = failure["message"].(string) }
		fmt.Fprintf(stdout, "\nNot forgotten (%s): %s\n", code, message)
	}
	fmt.Fprint(stdout, "\nPress Enter to close.")
	_, _ = in.ReadString('\n')
	fmt.Fprintln(stdout)
	return emit(stderr, stderr, body)
}
